// Package azure provisions and manages the Azure-side of the aztunnel
// E2E test infrastructure: resource group, Relay namespace, RBAC role
// assignments. It is consumed by the cmd/e2e-infra binary and is not
// imported by the test suite itself (the suite uses
// github.com/philsphicas/aztunnel/e2e/azrelay for per-invocation hycos).
package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// Config configures a Provisioner. SubscriptionID is read from the
// AZURE_SUBSCRIPTION_ID environment variable if not set explicitly.
type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	RelayName      string // optional; if empty, EnsureNamespace generates one
}

// Provisioner manages a single Resource Group + Relay namespace.
type Provisioner struct {
	cfg           Config
	cred          azcore.TokenCredential
	rgClient      *armresources.ResourceGroupsClient
	nsClient      *armrelay.NamespacesClient
	resolvedRelay string
	resolvedSubID string
}

// NewProvisioner constructs a Provisioner using DefaultAzureCredential.
// It does not call Azure other than to resolve the subscription ID, which
// is read from AZURE_SUBSCRIPTION_ID env var (set by `azure/login` in CI)
// and otherwise falls back to ~/.azure/azureProfile.json (`az account
// show`'s default subscription) so a contributor only needs `az login`.
func NewProvisioner(ctx context.Context, cfg Config) (*Provisioner, error) {
	if cfg.ResourceGroup == "" {
		return nil, errors.New("ResourceGroup is required")
	}
	if cfg.SubscriptionID == "" {
		cfg.SubscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}
	if cfg.SubscriptionID == "" {
		if id, err := defaultSubscriptionFromAzureCLIProfile(); err == nil {
			cfg.SubscriptionID = id
		}
	}
	if cfg.SubscriptionID == "" {
		return nil, errors.New("AZURE_SUBSCRIPTION_ID must be set (run `az account set --subscription <id>` and `az login`, or pass Config.SubscriptionID explicitly)")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("default azure credential: %w", err)
	}
	rgClient, err := armresources.NewResourceGroupsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("new resource groups client: %w", err)
	}
	nsClient, err := armrelay.NewNamespacesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("new namespaces client: %w", err)
	}
	return &Provisioner{
		cfg:           cfg,
		cred:          cred,
		rgClient:      rgClient,
		nsClient:      nsClient,
		resolvedRelay: cfg.RelayName,
		resolvedSubID: cfg.SubscriptionID,
	}, nil
}

// SubscriptionID returns the resolved subscription ID.
func (p *Provisioner) SubscriptionID() string { return p.resolvedSubID }

// Credential returns the underlying token credential.
func (p *Provisioner) Credential() azcore.TokenCredential { return p.cred }

// ResolvedNamespace returns the namespace name after EnsureNamespace /
// DiscoverNamespace have run. Returns "" if neither has been called.
func (p *Provisioner) ResolvedNamespace() string { return p.resolvedRelay }

// EnsureNamespace creates the resource group + relay namespace if either
// is missing. The namespace name is resolved in the following order:
//
//  1. Config.RelayName, if non-empty.
//  2. Any single existing namespace already present in the resource
//     group (so attaching to an existing setup does not silently end
//     up with a second namespace alongside the one another tool
//     created).
//  3. A deterministic hash derived from subscription + RG (see
//     generateRelayName).
//
// If two or more namespaces already exist in the RG and Config.RelayName
// is empty, an error is returned — pick one explicitly with --relay-name.
// If Config.RelayName is set but does not match an existing namespace
// while other namespaces exist, a warning is printed (we do not refuse
// the operation, because a second namespace may be intentional, but the
// state will break DiscoverNamespace for later subcommands).
func (p *Provisioner) EnsureNamespace(ctx context.Context) (string, error) {
	if p.cfg.Location == "" {
		p.cfg.Location = "westus2"
	}
	fmt.Fprintf(os.Stderr, "==> ensuring resource group %s in %s\n", p.cfg.ResourceGroup, p.cfg.Location)
	if _, err := p.rgClient.CreateOrUpdate(ctx, p.cfg.ResourceGroup, armresources.ResourceGroup{
		Location: &p.cfg.Location,
	}, nil); err != nil {
		return "", fmt.Errorf("create resource group: %w", err)
	}

	relay := p.resolvedRelay
	existing, err := p.listExistingNamespaces(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case relay != "":
		// Explicit name. Warn (but do not refuse) when this would leave
		// the RG with multiple namespaces — DiscoverNamespace will then
		// require --relay-name for every subsequent subcommand.
		others := stringsWithout(existing, relay)
		if len(others) > 0 {
			fmt.Fprintf(os.Stderr, "    WARNING: namespace %s does not match existing namespace(s) in %s (%v)\n", relay, p.cfg.ResourceGroup, others)
			fmt.Fprintln(os.Stderr, "    later subcommands (env, janitor, …) will require --relay-name until the RG has a single namespace")
		}
	case len(existing) == 0:
		relay = generateRelayName(p.cfg.SubscriptionID, p.cfg.ResourceGroup)
	case len(existing) == 1:
		relay = existing[0]
		fmt.Fprintf(os.Stderr, "    (reusing existing namespace %s)\n", relay)
	default:
		return "", fmt.Errorf("multiple relay namespaces in %s — set --relay-name explicitly", p.cfg.ResourceGroup)
	}
	fmt.Fprintf(os.Stderr, "==> ensuring relay namespace %s\n", relay)
	poller, err := p.nsClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, relay, armrelay.Namespace{
		Location: &p.cfg.Location,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("begin create namespace: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return "", fmt.Errorf("create namespace: %w", err)
	}
	p.resolvedRelay = relay
	return relay, nil
}

func stringsWithout(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func (p *Provisioner) listExistingNamespaces(ctx context.Context) ([]string, error) {
	pager := p.nsClient.NewListByResourceGroupPager(p.cfg.ResourceGroup, nil)
	var names []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		for _, ns := range page.Value {
			if ns != nil && ns.Name != nil {
				names = append(names, *ns.Name)
			}
		}
	}
	return names, nil
}

// DiscoverNamespace returns the relay namespace name. If Config.RelayName
// is set, returns it. Otherwise lists namespaces in the resource group and
// returns the single match, erroring if there are zero or more than one.
func (p *Provisioner) DiscoverNamespace(ctx context.Context) (string, error) {
	if p.resolvedRelay != "" {
		return p.resolvedRelay, nil
	}
	names, err := p.listExistingNamespaces(ctx)
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no relay namespace found in %s — run `make e2e-setup` first", p.cfg.ResourceGroup)
	case 1:
		p.resolvedRelay = names[0]
		return names[0], nil
	default:
		return "", fmt.Errorf("multiple relay namespaces in %s — set --relay-name explicitly", p.cfg.ResourceGroup)
	}
}

// DeleteResourceGroup deletes the resource group and all resources in it.
// Long-running; uses BeginDelete and waits for completion.
func (p *Provisioner) DeleteResourceGroup(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "==> deleting resource group %s (this may take several minutes)\n", p.cfg.ResourceGroup)
	poller, err := p.rgClient.BeginDelete(ctx, p.cfg.ResourceGroup, nil)
	if err != nil {
		return fmt.Errorf("begin delete: %w", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("delete resource group: %w", err)
	}
	return nil
}

// generateRelayName produces a deterministic, globally-unique-by-convention
// namespace name from the subscription ID + resource group name. Repeated
// runs against the same RG produce the same namespace name.
//
// Note: this is NOT bit-compatible with Bicep's uniqueString() or the
// historic uniquestring.sh helper. It is a SHA-256 hash truncated to 12
// hex characters (48 bits). Users migrating from those scripts should
// either pass --relay-name explicitly to preserve their existing name,
// or let DiscoverNamespace pick up the lone existing namespace in the RG.
func generateRelayName(subID, rg string) string {
	h := sha256.Sum256([]byte(strings.ToLower(subID + rg)))
	return "aztunnel-" + hex.EncodeToString(h[:6])
}

// defaultSubscriptionFromAzureCLIProfile reads ~/.azure/azureProfile.json
// (the file maintained by `az login` / `az account set`) and returns the
// id of the subscription flagged as default. Used as a fallback for
// contributors who have authenticated via `az login` but have not
// exported AZURE_SUBSCRIPTION_ID into their shell environment.
func defaultSubscriptionFromAzureCLIProfile() (string, error) {
	p, err := ReadAzureCLIProfileDefault()
	if err != nil {
		return "", err
	}
	return p.Subscription, nil
}

// AzureCLIDefaults is the resolved default subscription + tenant from
// the Azure CLI's local profile. Either field may be empty when the
// profile is absent or unparseable.
type AzureCLIDefaults struct {
	Subscription string
	Tenant       string
}

// ReadAzureCLIProfileDefault parses ~/.azure/azureProfile.json and
// returns the subscription id + tenant id flagged as the current
// default. Used by `make e2e-status` to detect identity-context drift
// without making any network calls.
func ReadAzureCLIProfileDefault() (AzureCLIDefaults, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AzureCLIDefaults{}, err
	}
	path := filepath.Join(home, ".azure", "azureProfile.json")
	data, err := os.ReadFile(path) //nolint:gosec // path is under user's home dir
	if err != nil {
		return AzureCLIDefaults{}, err
	}
	// Azure CLI on Windows writes a UTF-8 BOM.
	trimmed := strings.TrimPrefix(string(data), "\ufeff")
	var profile struct {
		Subscriptions []struct {
			ID        string `json:"id"`
			TenantID  string `json:"tenantId"`
			IsDefault bool   `json:"isDefault"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal([]byte(trimmed), &profile); err != nil {
		return AzureCLIDefaults{}, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, s := range profile.Subscriptions {
		if s.IsDefault {
			return AzureCLIDefaults{Subscription: s.ID, Tenant: s.TenantID}, nil
		}
	}
	return AzureCLIDefaults{}, errors.New("no default subscription in azureProfile.json")
}
