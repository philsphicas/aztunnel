package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// Tag keys stamped on RGs that `make e2e-setup` creates. `make e2e-clean`
// uses TagKeyTool to refuse deleting RGs it doesn't own (CI's RG,
// a colleague's RG, anything pre-existing the developer attached to).
const (
	TagKeyOwner = "aztunnel-e2e-owner"
	TagKeyTool  = "aztunnel-e2e-tool"
	TagValTool  = "e2e-infra"
)

// ErrNotOwned is returned by CleanCmd's tag check when the RG either
// has no `aztunnel-e2e-tool` tag or carries a different value. Callers
// surface a directive error pointing at `az group delete` for the
// "really wipe it" escape hatch and `--force` for the in-tool override.
var ErrNotOwned = errors.New("resource group is not tagged as owned by e2e-infra")

// ownerTags returns the tag map stamped onto an RG at setup time.
// Values are pointers because armresources.ResourceGroup.Tags is
// map[string]*string.
func ownerTags(ownerOID string) map[string]*string {
	owner := ownerOID
	tool := TagValTool
	return map[string]*string{
		TagKeyOwner: &owner,
		TagKeyTool:  &tool,
	}
}

// AssertOwnedForDelete checks whether the RG's existing tags include
// the e2e-infra ownership marker and match the current caller's
// object ID. Returns nil when the RG is safe to delete via
// `make e2e-clean`; ErrNotOwned when not.
func AssertOwnedForDelete(tags map[string]*string, callerOID string) error {
	tool, ok := tags[TagKeyTool]
	if !ok || tool == nil || *tool != TagValTool {
		return fmt.Errorf("%w: tag %s=%q is required", ErrNotOwned, TagKeyTool, TagValTool)
	}
	owner, ok := tags[TagKeyOwner]
	if !ok || owner == nil || !strings.EqualFold(*owner, callerOID) {
		return fmt.Errorf("%w: tag %s must match the current user object id", ErrNotOwned, TagKeyOwner)
	}
	return nil
}

// GetResourceGroupTags fetches the current tag map of the RG. Used by
// CleanCmd before deletion. Returns the tags as-is from ARM (may be
// nil when the RG carries no tags).
func (p *Provisioner) GetResourceGroupTags(ctx context.Context) (map[string]*string, error) {
	resp, err := p.rgClient.Get(ctx, p.cfg.ResourceGroup, nil)
	if err != nil {
		return nil, fmt.Errorf("get resource group: %w", err)
	}
	if resp.Tags == nil {
		return nil, nil
	}
	return resp.Tags, nil
}

// TagResourceGroup writes (or overwrites) the ownership tags on the
// RG. Idempotent: the patch preserves other tags. Setup only calls
// this when the RG is untagged or already owned by the current user;
// the caller is responsible for not overwriting another owner's tags.
func (p *Provisioner) TagResourceGroup(ctx context.Context, ownerOID string) error {
	tags := ownerTags(ownerOID)
	_, err := p.rgClient.Update(ctx, p.cfg.ResourceGroup, armresources.ResourceGroupPatchable{
		Tags: tags,
	}, nil)
	if err != nil {
		return fmt.Errorf("tag resource group: %w", err)
	}
	return nil
}
