// Command e2e-infra is the implementation behind the `make e2e-*` Make
// targets that provision and operate the aztunnel E2E test
// infrastructure. Use the Make targets directly — they are the stable
// user surface. `make help` lists them with one-line summaries.
//
// Subcommands:
//
//	setup      — Create per-developer RG + namespace + permanent SAS rules + grant self. Writes e2e/.local.json.
//	attach     — Record an existing RG + namespace in e2e/.local.json (no create / grant).
//	status     — Print resolved config and probe Azure-side health.
//	ci         — Maintainer-only: setup + Entra app + GitHub secrets.
//	clean      — Delete the resource group (owner-tag-protected; --force overrides).
//	grant      — Grant Azure Relay Owner to a principal.
//	janitor    — Delete orphan e2e-{entra,sas}-<hex> hycos older than --max-age.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/alecthomas/kong"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
	"github.com/philsphicas/aztunnel/e2e/infra/azure"
	"github.com/philsphicas/aztunnel/e2e/infra/entra"
	"github.com/philsphicas/aztunnel/e2e/infra/githubcfg"
	"github.com/philsphicas/aztunnel/e2e/infra/internal/msgraph"
	"github.com/philsphicas/aztunnel/e2e/infra/janitor"
)

// CLI is the top-level kong-parsed command structure.
//
// ResourceGroup intentionally has no default — each subcommand calls
// azure.ResolveResourceGroup with the appropriate ciFallback flag so
// the per-developer derivation happens consistently. `make e2e-ci`
// is the only target that defaults to azure.CIResourceGroup.
var CLI struct {
	ResourceGroup string `name:"resource-group" env:"RESOURCE_GROUP" help:"Resource group (default: aztunnel-e2e-<alias-from-signed-in-user>; CI: aztunnel-e2e)."`
	RelayName     string `name:"relay-name" env:"RELAY_NAME" help:"Relay namespace name (auto-generated from subscription + RG if empty)."`
	Alias         string `name:"alias" env:"ALIAS" help:"Slug used to form aztunnel-e2e-<alias>; default derives from the signed-in user (Microsoft Graph /me)."`

	Setup   SetupCmd   `cmd:"" help:"Provision per-developer RG + namespace + permanent SAS rules + grant self. Writes e2e/.local.json."`
	Attach  AttachCmd  `cmd:"" help:"Record an existing RG + namespace in e2e/.local.json without provisioning."`
	Status  StatusCmd  `cmd:"" help:"Print resolved config and probe Azure-side health."`
	CI      CICmd      `cmd:"" help:"Maintainer-only: setup + Entra app + GitHub secrets."`
	Clean   CleanCmd   `cmd:"" help:"Delete this developer's RG (owner-tag-protected)."`
	Grant   GrantCmd   `cmd:"" help:"Grant Azure Relay Owner to a principal."`
	Janitor JanitorCmd `cmd:"" help:"Delete orphan e2e-{entra,sas}-<hex> hycos."`
}

// bootstrap is the common pre-amble: construct a credential (via a
// throwaway placeholder Provisioner), resolve the RG name, then
// construct the real Provisioner against that RG.
//
// ciFallback=true uses azure.CIResourceGroup when no RG / alias is
// specified; false derives one from the signed-in user.
func bootstrap(ctx context.Context, location string, ciFallback bool) (*azure.Provisioner, string, error) {
	cred, err := bootstrapCredential(ctx)
	if err != nil {
		return nil, "", err
	}
	rg, err := azure.ResolveResourceGroup(ctx, cred, CLI.ResourceGroup, CLI.Alias, ciFallback)
	if err != nil {
		return nil, "", err
	}
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: rg,
		Location:      location,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return nil, rg, err
	}
	return prov, rg, nil
}

// bootstrapCredential returns a TokenCredential without needing an
// already-resolved RG. We construct a placeholder Provisioner whose
// only job is to materialise the same DefaultAzureCredential each
// subcommand would otherwise build itself.
func bootstrapCredential(ctx context.Context) (azcore.TokenCredential, error) {
	prov, err := azure.NewProvisioner(ctx, azure.Config{ResourceGroup: "_bootstrap"})
	if err != nil {
		return nil, err
	}
	return prov.Credential(), nil
}

// SetupCmd creates the RG + relay namespace + permanent SAS rules,
// tags the RG with the signed-in user's OID, grants Azure Relay Owner
// on the namespace, and writes e2e/.local.json. On Contributor-only
// callers the grant gracefully falls back to SAS-only mode.
type SetupCmd struct {
	Location string `name:"location" env:"LOCATION" default:"westus2" help:"Azure region for created resources."`
	SkipRBAC bool   `name:"skip-rbac" help:"Do not attempt the RBAC grant (forces SAS-only)."`
}

const rbacPropagationMaxWait = 30 * time.Second

func (c *SetupCmd) Run(ctx context.Context) error {
	prov, rg, err := bootstrap(ctx, c.Location, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> using resource group %s\n", rg)

	gc, err := msgraph.New(prov.Credential())
	if err != nil {
		return err
	}
	oid, upn, err := gc.SignedInUserObjectID(ctx)
	if err != nil {
		return fmt.Errorf("resolve signed-in user (run `az login`): %w", err)
	}
	tenant, err := gc.TenantID(ctx)
	if err != nil {
		return fmt.Errorf("resolve tenant id: %w", err)
	}

	if _, err := prov.EnsureNamespace(ctx); err != nil {
		return fmt.Errorf("ensure namespace: %w", err)
	}
	if err := prov.EnsureRunRules(ctx); err != nil {
		return fmt.Errorf("ensure run rules: %w", err)
	}

	existingTags, err := prov.GetResourceGroupTags(ctx)
	if err != nil {
		return fmt.Errorf("read resource group tags: %w", err)
	}
	switch {
	case azure.AssertOwnedForDelete(existingTags, oid) == nil:
		if err := prov.TagResourceGroup(ctx, oid); err != nil {
			fmt.Fprintf(os.Stderr, "    WARNING: could not refresh ownership tags: %v\n", err)
		}
	case existingTags[azure.TagKeyTool] == nil:
		if err := prov.TagResourceGroup(ctx, oid); err != nil {
			fmt.Fprintf(os.Stderr, "    WARNING: could not tag resource group: %v\n", err)
			fmt.Fprintln(os.Stderr, "    WARNING: `make e2e-clean` will refuse this RG until the tag is applied")
		}
	default:
		fmt.Fprintln(os.Stderr, "==> resource group is tagged by a different owner; leaving ownership tags unchanged")
	}

	sasOnly := c.SkipRBAC
	if c.SkipRBAC {
		fmt.Fprintln(os.Stderr, "==> skipping RBAC grant per --skip-rbac; SAS-only mode")
	} else {
		err := prov.GrantOwnerSelf(ctx)
		switch {
		case err == nil:
			waitForRBACPropagation(ctx, prov, rg)
		case errors.Is(err, azure.ErrAuthorizationFailed):
			printAuthorizationFallbackNote(prov.SubscriptionID(), rg, prov.ResolvedNamespace())
			sasOnly = true
		default:
			return fmt.Errorf("grant self: %w", err)
		}
	}

	cfg := LocalConfig{
		Subscription:  prov.SubscriptionID(),
		Tenant:        tenant,
		UserObjectID:  oid,
		ResourceGroup: rg,
		RelayName:     prov.ResolvedNamespace(),
		SASOnly:       sasOnly,
		CreatedAt:     nowRFC3339(),
	}
	path, err := writeLocalConfig(cfg)
	if err != nil {
		return err
	}
	printLocalConfigWritten(path)
	fmt.Fprintf(os.Stderr, "==> setup complete for %s\n", upn)
	fmt.Fprintln(os.Stderr, "    next: `make e2e` to run the test suite")
	return nil
}

// waitForRBACPropagation gives newly created role assignments a short
// window to propagate before the first Entra-backed test run.
func waitForRBACPropagation(ctx context.Context, prov *azure.Provisioner, rg string) {
	cfg := azrelay.Config{
		SubscriptionID: prov.SubscriptionID(),
		ResourceGroup:  rg,
		Namespace:      prov.ResolvedNamespace(),
		Cred:           prov.Credential(),
	}
	deadline := time.Now().Add(rbacPropagationMaxWait)
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := azrelay.AcquireRunRules(probeCtx, cfg)
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "    WARNING: Azure Relay RBAC may still be propagating; first Entra test run may fail briefly: %v\n", lastErr)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// CICmd performs the full maintainer setup: namespace + Entra app +
// federated credential + GitHub secrets/variables. Targets the
// CI-fixed RG name (azure.CIResourceGroup) by default; explicit
// --resource-group / RESOURCE_GROUP / --alias still wins.
type CICmd struct {
	Location   string `name:"location" env:"LOCATION" default:"westus2" help:"Azure region for created resources."`
	EntraApp   string `name:"entra-app" env:"ENTRA_APP" default:"aztunnel-e2e-ci" help:"Entra app registration display name."`
	GitHubRepo string `name:"github-repo" env:"GITHUB_REPO" help:"GitHub owner/repo (auto-detected from git remote if empty)."`
	GitHubEnv  string `name:"github-env" env:"GITHUB_ENV" default:"e2e-azure" help:"GitHub environment name."`
	Dependabot bool   `name:"dependabot" help:"Also configure Dependabot secrets."`
	DryRun     bool   `name:"dry-run" help:"Print what would be done instead of doing it."`
}

func (c *CICmd) Run(ctx context.Context) error {
	prov, rg, err := bootstrap(ctx, c.Location, true)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> using resource group %s\n", rg)

	// CI's RG is shared across maintainers and predates per-developer
	// tagging; do not stamp owner tags so `make e2e-clean` continues
	// to require --force against it.
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
		ResourceGroup:  rg,
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

// CleanCmd deletes the resource group. Refuses unless the RG carries
// the `aztunnel-e2e-tool=e2e-infra` owner tag (or --force is set).
type CleanCmd struct {
	Confirm bool `name:"yes" help:"Skip the y/N confirmation prompt."`
	Force   bool `name:"force" help:"Bypass the owner-tag check (use with care)."`
}

func (c *CleanCmd) Run(ctx context.Context) error {
	prov, rg, err := bootstrap(ctx, "", false)
	if err != nil {
		return err
	}
	if !c.Force {
		gc, err := msgraph.New(prov.Credential())
		if err != nil {
			return fmt.Errorf("resolve current user: %w", err)
		}
		callerOID, _, err := gc.SignedInUserObjectID(ctx)
		if err != nil {
			return fmt.Errorf("resolve current user: %w", err)
		}
		tags, err := prov.GetResourceGroupTags(ctx)
		if err != nil {
			return fmt.Errorf("verify RG ownership: %w", err)
		}
		if err := azure.AssertOwnedForDelete(tags, callerOID); err != nil {
			msg := fmt.Sprintf("refusing to delete resource group %s: %v\n"+
				"    `make e2e-clean` only deletes RGs created by `make e2e-setup` (verified\n"+
				"    via the `%s` and `%s` tags). To delete anyway, pass --force, or use\n"+
				"    `az group delete --name %s` for a manually-managed RG",
				rg, err, azure.TagKeyTool, azure.TagKeyOwner, rg)
			return errors.New(msg)
		}
	}
	if !c.Confirm {
		fmt.Fprintf(os.Stderr, "About to delete resource group %q. Pass --yes to confirm.\n", rg)
		return errors.New("not confirmed")
	}
	if err := prov.DeleteResourceGroup(ctx); err != nil {
		return err
	}
	// Best-effort: drop e2e/.local.json if it pointed at the RG we
	// just deleted. Non-fatal — the deletion already happened.
	if local, path, lerr := readLocalConfig(); lerr == nil && strings.EqualFold(local.ResourceGroup, rg) {
		if rerr := os.Remove(path); rerr != nil {
			fmt.Fprintf(os.Stderr, "    note: could not remove %s: %v\n", path, rerr)
		} else {
			fmt.Fprintf(os.Stderr, "==> removed e2e/%s (it pointed at the deleted RG)\n", localConfigFilename)
		}
	}
	return nil
}

// GrantCmd grants Azure Relay Owner to a principal at namespace scope.
// --assignee is the Make-target-friendly entry point; --self / --user /
// --sp remain for explicit cases.
type GrantCmd struct {
	Self     bool   `name:"self" xor:"target" help:"Grant to the signed-in user."`
	User     string `name:"user" xor:"target" help:"Grant to a user by UPN/email."`
	SP       string `name:"sp" xor:"target" help:"Grant to a service principal by app display name."`
	Assignee string `name:"assignee" xor:"target" help:"Grant to UPN (contains @) or app display name (auto-detected)."`
}

func (c *GrantCmd) Run(ctx context.Context) error {
	if !c.Self && c.User == "" && c.SP == "" && c.Assignee == "" {
		return errors.New("exactly one of --self, --user, --sp, --assignee is required")
	}
	prov, _, err := bootstrap(ctx, "", false)
	if err != nil {
		return err
	}
	switch {
	case c.Self:
		return prov.GrantOwnerSelf(ctx)
	case c.User != "":
		return prov.GrantOwnerUser(ctx, c.User)
	case c.SP != "":
		return prov.GrantOwnerSPByAppName(ctx, c.SP)
	default:
		if strings.Contains(c.Assignee, "@") {
			return prov.GrantOwnerUser(ctx, c.Assignee)
		}
		return prov.GrantOwnerSPByAppName(ctx, c.Assignee)
	}
}

// JanitorCmd deletes orphan per-invocation hycos older than --max-age.
// Falls back to the CI RG (azure.CIResourceGroup) when no explicit
// override is set; matches the cron job's expectation.
type JanitorCmd struct {
	MaxAge time.Duration `name:"max-age" default:"4h" help:"Delete e2e-{entra,sas}-<hex> hycos older than this."`
	DryRun bool          `name:"dry-run" help:"Print what would be deleted without deleting."`
}

func (c *JanitorCmd) Run(ctx context.Context) error {
	prov, rg, err := bootstrap(ctx, "", true)
	if err != nil {
		return err
	}
	relay, err := prov.DiscoverNamespace(ctx)
	if err != nil {
		return err
	}
	return janitor.Run(ctx, janitor.Config{
		SubscriptionID: prov.SubscriptionID(),
		ResourceGroup:  rg,
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
		kong.Description("Implementation behind the `make e2e-*` Make targets — use those directly."),
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
