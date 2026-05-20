package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
	"github.com/philsphicas/aztunnel/e2e/infra/azure"
	"github.com/philsphicas/aztunnel/e2e/infra/internal/msgraph"
)

// AttachCmd records an attachment to a pre-existing RG + namespace in
// e2e/.local.json. It performs verification only — no create, no grant,
// no delete. Use case: joining a colleague's or CI's setup without
// re-provisioning.
type AttachCmd struct{}

func (c *AttachCmd) Run(ctx context.Context) error {
	if CLI.ResourceGroup == "" {
		return errors.New("attach requires --resource-group (or RESOURCE_GROUP env)")
	}
	prov, err := azure.NewProvisioner(ctx, azure.Config{
		ResourceGroup: CLI.ResourceGroup,
		RelayName:     CLI.RelayName,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "==> attaching to resource group %s\n", CLI.ResourceGroup)
	tags, err := prov.GetResourceGroupTags(ctx)
	if err != nil {
		return fmt.Errorf("verify resource group access: %w", err)
	}
	_ = tags // tags are informational only on attach; clean's owner-check is what matters.

	relay, err := prov.DiscoverNamespace(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> discovered relay namespace %s\n", relay)

	// Confirm the permanent SAS rules exist + we can ListKeys at
	// least one. ListKeys is the data-plane gate that decides whether
	// tests can run at all.
	if _, err := azrelay.AcquireRunRules(ctx, azrelay.Config{
		SubscriptionID: prov.SubscriptionID(),
		ResourceGroup:  CLI.ResourceGroup,
		Namespace:      relay,
		Cred:           prov.Credential(),
	}); err != nil {
		return fmt.Errorf("verify SAS rules readable: %w\n"+
			"    the namespace exists but `make e2e-attach` cannot read the permanent\n"+
			"    SAS rules; ask whoever ran `make e2e-setup` on it to grant you the\n"+
			"    `Azure Relay Owner` role at namespace scope (or run\n"+
			"    `make e2e-grant RESOURCE_GROUP=%s RELAY_NAME=%s ASSIGNEE=<your-upn>`)",
			err, CLI.ResourceGroup, relay)
	}
	fmt.Fprintln(os.Stderr, "    ✓ permanent SAS rules readable")

	gc, err := msgraph.New(prov.Credential())
	if err != nil {
		return err
	}
	oid, _, err := gc.SignedInUserObjectID(ctx)
	if err != nil {
		return fmt.Errorf("resolve signed-in user: %w", err)
	}
	tenant, err := gc.TenantID(ctx)
	if err != nil {
		return fmt.Errorf("resolve tenant id: %w", err)
	}

	cfg := LocalConfig{
		Subscription:  prov.SubscriptionID(),
		Tenant:        tenant,
		UserObjectID:  oid,
		ResourceGroup: CLI.ResourceGroup,
		RelayName:     relay,
		// Attach leaves sasOnly false; `make e2e-status` re-probes
		// data-plane access on demand. If the caller cannot Listen /
		// Send the entra subtests will fail, which is the same
		// outcome as running tests against an under-permissioned
		// namespace today — just with a clearer error.
		SASOnly:   false,
		CreatedAt: nowRFC3339(),
	}
	path, err := writeLocalConfig(cfg)
	if err != nil {
		return err
	}
	printLocalConfigWritten(path)
	fmt.Fprintln(os.Stderr, "==> ready to run `make e2e`")
	return nil
}
