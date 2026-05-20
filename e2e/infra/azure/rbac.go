package azure

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"github.com/google/uuid"

	"github.com/philsphicas/aztunnel/e2e/infra/internal/msgraph"
)

// azureRelayOwnerRoleDefID is the built-in "Azure Relay Owner" role
// definition ID (verified at plan time:
// `az role definition list --query "[?roleName=='Azure Relay Owner']"`).
// The role's actions and dataActions are both Microsoft.Relay/*, covering
// hyco create/delete, auth-rule create, ListKeys, Listen, and Send.
const azureRelayOwnerRoleDefID = "2787bf04-f1f5-4bfe-8383-c8a24483ee38"

// ErrAuthorizationFailed is returned by GrantOwnerSelf (and the
// underlying grantOwner) when the caller lacks
// Microsoft.Authorization/roleAssignments/write at the scope. SetupCmd
// downgrades this to the documented SAS-only fallback path; other
// callers (`make e2e-grant ASSIGNEE=…`, the CI subcommand) propagate
// it as a hard failure.
var ErrAuthorizationFailed = errors.New("caller lacks role-assignment write permission")

// GrantOwnerSelf grants Azure Relay Owner to the signed-in user at
// namespace scope.
func (p *Provisioner) GrantOwnerSelf(ctx context.Context) error {
	gc, err := msgraph.New(p.cred)
	if err != nil {
		return err
	}
	oid, upn, err := gc.SignedInUserObjectID(ctx)
	if err != nil {
		return fmt.Errorf("resolve signed-in user: %w", err)
	}
	fmt.Fprintf(os.Stderr, "==> granting Azure Relay Owner to %s\n", upn)
	return p.grantOwner(ctx, oid, armauthorization.PrincipalTypeUser)
}

// GrantOwnerUser grants Azure Relay Owner to a user identified by UPN.
func (p *Provisioner) GrantOwnerUser(ctx context.Context, upn string) error {
	gc, err := msgraph.New(p.cred)
	if err != nil {
		return err
	}
	oid, err := gc.UserObjectIDByUPN(ctx, upn)
	if err != nil {
		return fmt.Errorf("resolve user %s: %w", upn, err)
	}
	fmt.Fprintf(os.Stderr, "==> granting Azure Relay Owner to %s\n", upn)
	return p.grantOwner(ctx, oid, armauthorization.PrincipalTypeUser)
}

// GrantOwnerSPByAppName resolves an app registration by display name to
// its service-principal object ID, then grants Owner.
func (p *Provisioner) GrantOwnerSPByAppName(ctx context.Context, appName string) error {
	gc, err := msgraph.New(p.cred)
	if err != nil {
		return err
	}
	app, err := gc.AppByDisplayName(ctx, appName)
	if err != nil {
		return fmt.Errorf("resolve app %s: %w", appName, err)
	}
	sp, err := gc.ServicePrincipalByAppID(ctx, app.AppID)
	if err != nil {
		return fmt.Errorf("resolve SP for %s: %w", appName, err)
	}
	fmt.Fprintf(os.Stderr, "==> granting Azure Relay Owner to %s (SP %s)\n", appName, sp.ID)
	return p.grantOwner(ctx, sp.ID, armauthorization.PrincipalTypeServicePrincipal)
}

// GrantOwnerSP grants Owner to a service principal identified by its
// object ID. Used when the caller already has the OID in hand (e.g. fresh
// from EnsureApp).
func (p *Provisioner) GrantOwnerSP(ctx context.Context, spObjectID string) error {
	if spObjectID == "" {
		return errors.New("empty service-principal object ID")
	}
	fmt.Fprintf(os.Stderr, "==> granting Azure Relay Owner to SP %s\n", spObjectID)
	return p.grantOwner(ctx, spObjectID, armauthorization.PrincipalTypeServicePrincipal)
}

func (p *Provisioner) grantOwner(ctx context.Context, oid string, pt armauthorization.PrincipalType) error {
	relay, err := p.DiscoverNamespace(ctx)
	if err != nil {
		return err
	}
	scope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Relay/namespaces/%s",
		p.cfg.SubscriptionID, p.cfg.ResourceGroup, relay)
	roleDef := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		p.cfg.SubscriptionID, azureRelayOwnerRoleDefID)

	ra, err := armauthorization.NewRoleAssignmentsClient(p.cfg.SubscriptionID, p.cred, nil)
	if err != nil {
		return fmt.Errorf("new role assignments client: %w", err)
	}
	name := uuid.NewString()
	_, err = ra.Create(ctx, scope, name, armauthorization.RoleAssignmentCreateParameters{
		Properties: &armauthorization.RoleAssignmentProperties{
			PrincipalID:      &oid,
			RoleDefinitionID: &roleDef,
			PrincipalType:    &pt,
		},
	}, nil)
	if err != nil {
		if isRoleAlreadyAssigned(err) {
			fmt.Fprintln(os.Stderr, "    ✓ already assigned")
			return nil
		}
		if isAuthorizationFailed(err) {
			return fmt.Errorf("%w: %v", ErrAuthorizationFailed, err)
		}
		return fmt.Errorf("create role assignment: %w", err)
	}
	fmt.Fprintln(os.Stderr, "    ✓ granted")
	return nil
}

// isAuthorizationFailed reports whether err is the well-known
// "caller is missing Microsoft.Authorization/roleAssignments/write"
// ARM response. Returned to allow `make e2e-setup` to fall back to
// SAS-only mode without failing.
func isAuthorizationFailed(err error) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	return respErr.ErrorCode == "AuthorizationFailed"
}

// isRoleAlreadyAssigned returns true if the error from a role-assignment
// Create call is the well-known "principal already has this role at this
// scope" response. Azure returns RoleAssignmentExists in most regions but
// some API versions return RoleAssignmentAlreadyExists for the same
// condition, so we accept both.
func isRoleAlreadyAssigned(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.ErrorCode {
		case "RoleAssignmentExists", "RoleAssignmentAlreadyExists":
			return true
		}
	}
	return false
}
