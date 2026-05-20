package azure

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
)

// EnsureRunRules idempotently provisions the two permanent
// namespace-scoped SAS authorization rules every e2e test invocation
// reads at startup: PermanentListenerRuleName (Listen) and
// PermanentSenderRuleName (Send). Both rules live at namespace scope
// so the same key authorizes the corresponding action against every
// hybrid connection the test suite creates.
//
// Two rules (not one combined Listen+Send rule): TestWrongSASClaim
// asserts that a listener key cannot send and a sender key cannot
// listen; a single rule with both rights would silently pass that
// test.
//
// Permanent rules (not per-test or per-run rules): the Azure Relay
// namespace caps at 12 SAS authorization rules, and a shared CI
// namespace serving many parallel test invocations must keep its
// rule count constant.
//
// EnsureRunRules is safe to call repeatedly. Existing rules with the
// expected name and rights are left alone; rules with the expected
// name but wrong rights are corrected via CreateOrUpdate (idempotent
// upsert); missing rules are created. The namespace must already
// exist — EnsureNamespace / DiscoverNamespace must be called first.
func (p *Provisioner) EnsureRunRules(ctx context.Context) error {
	relay, err := p.DiscoverNamespace(ctx)
	if err != nil {
		return fmt.Errorf("discover namespace: %w", err)
	}
	if err := p.ensureOneRunRule(ctx, relay, azrelay.PermanentListenerRuleName, armrelay.AccessRightsListen); err != nil {
		return err
	}
	if err := p.ensureOneRunRule(ctx, relay, azrelay.PermanentSenderRuleName, armrelay.AccessRightsSend); err != nil {
		return err
	}
	return nil
}

// ensureOneRunRule provisions a single permanent namespace SAS rule
// with exactly one access right. It first GETs the rule to surface
// whether it already exists with the expected rights (the common
// no-op path on rerun); otherwise it CreateOrUpdate-upserts. Any
// not-found from the GET is treated as "create"; any other error is
// returned. The upsert path is wrapped in RetryOnAuthRuleConflict
// to absorb Azure Relay's per-scope 40901 throttle, which can fire
// when two parallel `make e2e-setup` invocations race against each
// other or when listener and sender upserts land back-to-back on a
// fresh namespace.
func (p *Provisioner) ensureOneRunRule(ctx context.Context, namespace, name string, right armrelay.AccessRights) error {
	got, err := p.nsClient.GetAuthorizationRule(ctx, p.cfg.ResourceGroup, namespace, name, nil)
	switch {
	case err == nil:
		if rightsMatch(got.AuthorizationRule, right) {
			fmt.Fprintf(os.Stderr, "    · rule %s: already present with rights=%s\n", name, right)
			return nil
		}
		fmt.Fprintf(os.Stderr, "    ! rule %s: rights drifted (%v); restoring to [%s]\n",
			name, currentRights(got.AuthorizationRule), right)
	case azrelay.IsNotFound(err):
		fmt.Fprintf(os.Stderr, "    + rule %s: creating with rights=[%s]\n", name, right)
	default:
		return fmt.Errorf("get rule %s: %w", name, err)
	}
	rights := []*armrelay.AccessRights{&right}
	if err := azrelay.RetryOnAuthRuleConflict(ctx, func() error {
		_, err := p.nsClient.CreateOrUpdateAuthorizationRule(ctx, p.cfg.ResourceGroup, namespace, name, armrelay.AuthorizationRule{
			Properties: &armrelay.AuthorizationRuleProperties{Rights: rights},
		}, nil)
		return err
	}); err != nil {
		return fmt.Errorf("ensure rule %s: %w", name, err)
	}
	fmt.Fprintf(os.Stderr, "    ✓ rule %s: ready (rights=[%s])\n", name, right)
	return nil
}

// rightsMatch reports whether the rule's Rights slice contains exactly
// `want` and nothing else, order-insensitive and duplicate-insensitive.
// Duplicate-insensitive is on `currentRights`, which dedupes; ARM
// theoretically returns each right once but we're defensive.
func rightsMatch(rule armrelay.AuthorizationRule, want armrelay.AccessRights) bool {
	got := currentRights(rule)
	if len(got) != 1 {
		return false
	}
	return got[0] == string(want)
}

// currentRights returns the rule's Rights as a sorted, deduplicated
// slice of strings for diagnostic printing and comparison. Nil-safe.
func currentRights(rule armrelay.AuthorizationRule) []string {
	if rule.Properties == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(rule.Properties.Rights))
	for _, r := range rule.Properties.Rights {
		if r == nil {
			continue
		}
		seen[string(*r)] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
