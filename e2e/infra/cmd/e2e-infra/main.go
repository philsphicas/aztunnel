// Command e2e-infra is the maintainer-facing setup tool for the aztunnel
// E2E test infrastructure. It replaces the historic shell scripts in
// this directory with a single Go binary that uses the Azure SDK + GitHub
// API directly — no `az`, `gh`, or `jq` shell-outs.
//
// Subcommands:
//
//	setup      — Create namespace + permanent SAS rules + grant self Azure Relay Owner
//	ci         — setup + create Entra app + federated credential + GitHub secrets
//	clean      — Delete the resource group (and everything in it)
//	grant      — Grant Azure Relay Owner to a principal
//	env        — Print the env vars required to run e2e tests locally
//	janitor    — Delete orphan e2e-{entra,sas}-<hex> hycos older than --max-age
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/philsphicas/aztunnel/e2e/infra/azure"
	"github.com/philsphicas/aztunnel/e2e/infra/entra"
	"github.com/philsphicas/aztunnel/e2e/infra/githubcfg"
	"github.com/philsphicas/aztunnel/e2e/infra/janitor"
)

// CLI is the top-level kong-parsed command structure.
var CLI struct {
	ResourceGroup string `name:"resource-group" env:"RESOURCE_GROUP" default:"aztunnel-e2e" help:"Resource group containing the relay namespace."`
	RelayName     string `name:"relay-name" env:"RELAY_NAME" help:"Relay namespace name (auto-generated from subscription + RG if empty)."`

	Setup   SetupCmd   `cmd:"" help:"Create namespace + permanent SAS rules + grant self Azure Relay Owner."`
	CI      CICmd      `cmd:"" help:"Full CI setup: namespace + permanent SAS rules + Entra app + GitHub secrets."`
	Clean   CleanCmd   `cmd:"" help:"Delete the resource group."`
	Grant   GrantCmd   `cmd:"" help:"Grant Azure Relay Owner to a principal."`
	Env     EnvCmd     `cmd:"" help:"Print E2E_* env vars for local test runs."`
	Janitor JanitorCmd `cmd:"" help:"Delete orphan e2e-{entra,sas}-<hex> hycos."`
}

// SetupCmd creates the resource group + relay namespace and grants the
// signed-in user Azure Relay Owner at namespace scope.
type SetupCmd struct {
	Location string `name:"location" env:"LOCATION" default:"westus2" help:"Azure region for created resources."`
	SkipRBAC bool   `name:"skip-rbac" help:"Do not grant RBAC roles (useful when running without role-assignment permissions)."`
}

func (c *SetupCmd) Run(ctx context.Context) error {
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		Location:      c.Location,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}
	if _, err := prov.EnsureNamespace(ctx); err != nil {
		return fmt.Errorf("ensure namespace: %w", err)
	}
	if err := prov.EnsureRunRules(ctx); err != nil {
		return fmt.Errorf("ensure run rules: %w", err)
	}
	if c.SkipRBAC {
		fmt.Fprintln(os.Stderr, "    (skipping RBAC per --skip-rbac)")
		return nil
	}
	if err := prov.GrantOwnerSelf(ctx); err != nil {
		return fmt.Errorf("grant self: %w", err)
	}
	return nil
}

// CICmd performs the full maintainer setup: namespace + Entra app +
// federated credential + GitHub secrets/variables.
type CICmd struct {
	Location   string `name:"location" env:"LOCATION" default:"westus2" help:"Azure region for created resources."`
	EntraApp   string `name:"entra-app" env:"ENTRA_APP" default:"aztunnel-e2e-ci" help:"Entra app registration display name."`
	GitHubRepo string `name:"github-repo" env:"GITHUB_REPO" help:"GitHub owner/repo (auto-detected from git remote if empty)."`
	GitHubEnv  string `name:"github-env" env:"GITHUB_ENV" default:"e2e-azure" help:"GitHub environment name."`
	Dependabot bool   `name:"dependabot" help:"Also configure Dependabot secrets."`
	DryRun     bool   `name:"dry-run" help:"Print what would be done instead of doing it."`
}

func (c *CICmd) Run(ctx context.Context) error {
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		Location:      c.Location,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}
	if _, err := prov.EnsureNamespace(ctx); err != nil {
		return fmt.Errorf("ensure namespace: %w", err)
	}
	if err := prov.EnsureRunRules(ctx); err != nil {
		return fmt.Errorf("ensure run rules: %w", err)
	}
	if err := prov.GrantOwnerSelf(ctx); err != nil {
		return fmt.Errorf("grant self: %w", err)
	}
	repo, err := resolveGitHubRepo(c.GitHubRepo)
	if err != nil {
		return err
	}
	app, err := entra.EnsureApp(ctx, entra.Config{
		DisplayName: c.EntraApp,
		Repo:        repo,
		Environment: c.GitHubEnv,
	})
	if err != nil {
		return fmt.Errorf("ensure entra app: %w", err)
	}
	if err := prov.GrantOwnerSP(ctx, app.ServicePrincipalObjectID); err != nil {
		return fmt.Errorf("grant SP: %w", err)
	}
	if err := githubcfg.Configure(ctx, githubcfg.Config{
		Repo:           repo,
		Environment:    c.GitHubEnv,
		RelayName:      prov.ResolvedNamespace(),
		ResourceGroup:  CLI.ResourceGroup,
		ClientID:       app.ApplicationID,
		TenantID:       app.TenantID,
		SubscriptionID: prov.SubscriptionID(),
		Dependabot:     c.Dependabot,
		DryRun:         c.DryRun,
	}); err != nil {
		return fmt.Errorf("configure github: %w", err)
	}
	return nil
}

// CleanCmd deletes the resource group (and all resources in it).
type CleanCmd struct {
	Confirm bool `name:"yes" help:"Skip the y/N confirmation prompt."`
}

func (c *CleanCmd) Run(ctx context.Context) error {
	if !c.Confirm {
		fmt.Fprintf(os.Stderr, "About to delete resource group %q. Pass --yes to confirm.\n", CLI.ResourceGroup)
		return fmt.Errorf("not confirmed")
	}
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}
	return prov.DeleteResourceGroup(ctx)
}

// GrantCmd grants Azure Relay Owner to a principal at namespace scope.
type GrantCmd struct {
	Self bool   `name:"self" xor:"target" help:"Grant to the signed-in user."`
	User string `name:"user" xor:"target" help:"Grant to a user by UPN/email."`
	SP   string `name:"sp" xor:"target" help:"Grant to a service principal by app display name."`
}

func (c *GrantCmd) Run(ctx context.Context) error {
	if !c.Self && c.User == "" && c.SP == "" {
		return fmt.Errorf("exactly one of --self, --user, --sp is required")
	}
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}
	switch {
	case c.Self:
		return prov.GrantOwnerSelf(ctx)
	case c.User != "":
		return prov.GrantOwnerUser(ctx, c.User)
	default:
		return prov.GrantOwnerSPByAppName(ctx, c.SP)
	}
}

// EnvCmd prints the env vars needed for local `make e2e` runs (replacement
// for the historic env.sh).
type EnvCmd struct{}

func (c *EnvCmd) Run(ctx context.Context) error {
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}
	relay, err := prov.DiscoverNamespace(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("export E2E_RELAY_NAME=%s\n", relay)
	fmt.Printf("export E2E_RESOURCE_GROUP=%s\n", CLI.ResourceGroup)
	fmt.Printf("export AZURE_SUBSCRIPTION_ID=%s\n", prov.SubscriptionID())
	return nil
}

// JanitorCmd deletes orphan per-invocation hycos older than --max-age.
type JanitorCmd struct {
	MaxAge time.Duration `name:"max-age" default:"4h" help:"Delete e2e-{entra,sas}-<hex> hycos older than this."`
	DryRun bool          `name:"dry-run" help:"Print what would be deleted without deleting."`
}

func (c *JanitorCmd) Run(ctx context.Context) error {
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}
	relay, err := prov.DiscoverNamespace(ctx)
	if err != nil {
		return err
	}
	return janitor.Run(ctx, janitor.Config{
		SubscriptionID: prov.SubscriptionID(),
		ResourceGroup:  CLI.ResourceGroup,
		Namespace:      relay,
		MaxAge:         c.MaxAge,
		DryRun:         c.DryRun,
		Cred:           prov.Credential(),
	})
}

func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cliCtx := kong.Parse(&CLI,
		kong.Name("e2e-infra"),
		kong.Description("Maintainer-facing setup CLI for aztunnel E2E test infrastructure."),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true}),
		// BindTo (not Bind/Run-arg) is required for interface types: kong's
		// runtime binding uses reflect.TypeOf, which on an interface value
		// returns the concrete type, so binding via `cliCtx.Run(ctx)` would
		// register the concrete *signalCtx rather than context.Context.
		kong.BindTo(ctx, (*context.Context)(nil)),
	)

	if err := cliCtx.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
