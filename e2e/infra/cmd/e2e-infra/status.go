package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
	"github.com/philsphicas/aztunnel/e2e/infra/azure"
	"github.com/philsphicas/aztunnel/e2e/infra/internal/msgraph"
)

// StatusCmd prints the resolved e2e config and probes Azure-side
// health. Exits 0 when everything looks good; non-zero otherwise so
// `make e2e-status && make e2e` is composable.
type StatusCmd struct{}

func (c *StatusCmd) Run(ctx context.Context) error {
	local, path, err := readLocalConfig()
	if errors.Is(err, os.ErrNotExist) {
		msg := fmt.Sprintf("no %s found\n"+
			"    Run `make e2e-setup` to provision a per-developer Azure Relay namespace,\n"+
			"    or `make e2e-attach RESOURCE_GROUP=<rg>` to record a pre-existing one",
			path)
		return errors.New(msg)
	}
	if err != nil {
		return err
	}

	// Header / one-line summary first. Subsequent checks may append
	// WARN / ERROR lines.
	fmt.Printf("config source     : e2e/%s\n", localConfigFilename)
	fmt.Printf("subscription      : %s\n", local.Subscription)
	fmt.Printf("tenant            : %s\n", local.Tenant)
	fmt.Printf("resource group    : %s\n", local.ResourceGroup)
	fmt.Printf("relay namespace   : %s\n", local.RelayName)
	fmt.Printf("sas-only mode     : %t\n", local.SASOnly)

	var problems []string

	// Subscription / tenant drift check against the Azure CLI default.
	// Skipped silently when the CLI profile is absent — managed-identity
	// and OIDC environments don't have ~/.azure/azureProfile.json.
	azureProfile, err := azure.ReadAzureCLIProfileDefault()
	switch {
	case errors.Is(err, os.ErrNotExist):
		// nothing to compare against; skip
	case err != nil:
		problems = append(problems, fmt.Sprintf("could not read ~/.azure/azureProfile.json: %v", err))
	case azureProfile.Subscription != "" && !strings.EqualFold(azureProfile.Subscription, local.Subscription):
		problems = append(problems, fmt.Sprintf("subscription mismatch: Azure CLI default is %s but persisted is %s — run `az account set --subscription %s` or re-run `make e2e-setup`",
			azureProfile.Subscription, local.Subscription, local.Subscription))
	case azureProfile.Tenant != "" && local.Tenant != "" && !strings.EqualFold(azureProfile.Tenant, local.Tenant):
		problems = append(problems, fmt.Sprintf("tenant mismatch: Azure CLI default tenant is %s but persisted is %s — `az login --tenant %s`",
			azureProfile.Tenant, local.Tenant, local.Tenant))
	}

	prov, err := azure.NewProvisioner(ctx, azure.Config{
		SubscriptionID: local.Subscription,
		ResourceGroup:  local.ResourceGroup,
		RelayName:      local.RelayName,
	})
	if err != nil {
		problems = append(problems, fmt.Sprintf("construct provisioner: %v", err))
	} else {
		if _, err := prov.GetResourceGroupTags(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("resource group GET failed: %v", err))
		}
		if _, err := azrelay.AcquireRunRules(ctx, azrelay.Config{
			SubscriptionID: local.Subscription,
			ResourceGroup:  local.ResourceGroup,
			Namespace:      local.RelayName,
			Cred:           prov.Credential(),
		}); err != nil {
			problems = append(problems, fmt.Sprintf("permanent SAS rules ListKeys failed: %v", err))
		} else {
			fmt.Printf("perm SAS rules    : readable\n")
		}

		gc, err := msgraph.New(prov.Credential())
		if err != nil {
			problems = append(problems, fmt.Sprintf("graph client: %v", err))
		} else {
			oid, _, err := gc.SignedInUserObjectID(ctx)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("resolve current user: %v", err))
			case local.UserObjectID != "" && !strings.EqualFold(oid, local.UserObjectID):
				problems = append(problems, fmt.Sprintf("identity drift: persisted user %s, current %s — `az login` or re-run `make e2e-setup`",
					local.UserObjectID, oid))
			}
		}
	}

	if len(problems) == 0 {
		fmt.Printf("status            : OK\n")
		return nil
	}
	fmt.Printf("status            : ATTENTION\n")
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "    !! %s\n", p)
	}
	return fmt.Errorf("%d issue(s) need attention; see above", len(problems))
}
