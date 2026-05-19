package azrelay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"
)

// PermanentListenerRuleName is the well-known name of the Listen-only
// namespace SAS authorization rule that every e2e test invocation reads
// (never creates) at startup. Provisioned once per namespace by
// e2e/infra/azure.Provisioner.EnsureRunRules (via `e2e-infra setup`).
const PermanentListenerRuleName = "e2e-listener"

// PermanentSenderRuleName is the well-known name of the Send-only
// namespace SAS authorization rule that every e2e test invocation reads
// at startup. See PermanentListenerRuleName.
const PermanentSenderRuleName = "e2e-sender"

// readKeyMaxWait bounds how long readKey will retry ListKeys against a
// transient ARM error. The permanent rules are long-lived, so the only
// errors we expect to absorb here are regional control-plane blips.
const readKeyMaxWait = 30 * time.Second

// RunRules holds the two namespace-scoped SAS authorization rules every
// e2e `go test` invocation shares: a Listen-only rule whose key the
// relay-listener uses, and a Send-only rule whose key the relay-sender
// uses. Both rules live at namespace scope, so the same key authorizes
// the corresponding action against every hybrid connection the run
// creates.
//
// The rules are permanent fixtures of the namespace (provisioned by
// `e2e-infra setup` once, never deleted by tests). AcquireRunRules
// reads their primary keys via ListKeys; it does NOT create or delete
// rules. This keeps the e2e-owned authorization-rule count constant
// at two regardless of how many parallel CI jobs target the
// namespace, well within the Azure Relay namespace 12-rule cap.
//
// Two rules (not one combined Listen+Send rule) is deliberate: the
// suite asserts that a listener key cannot be used to send and vice
// versa (TestWrongSASClaim). A single rule with both rights would
// silently pass that test.
//
// Acquire one with AcquireRunRules and propagate via Config.RunRules
// into NewProvider.
type RunRules struct {
	ListenerName string
	ListenerKey  string
	SenderName   string
	SenderKey    string
}

// AcquireRunRules reads the primary keys of the two permanent
// namespace-scoped SAS authorization rules (PermanentListenerRuleName,
// PermanentSenderRuleName) and returns a populated *RunRules.
//
// The rules must already exist in cfg.Namespace — provision them once
// with `e2e-infra setup`. If either rule is missing, AcquireRunRules
// returns an error whose message names the missing rule and points to
// `e2e-infra setup`.
//
// cfg.RunRules is ignored by this function (the returned *RunRules is
// what the caller would set on cfg.RunRules before constructing a
// Provider).
func AcquireRunRules(ctx context.Context, cfg Config) (*RunRules, error) {
	if err := cfg.validateForRunRules(); err != nil {
		return nil, err
	}
	cred := cfg.Cred
	if cred == nil {
		c, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("default azure credential: %w", err)
		}
		cred = c
	}
	opts := cfg.ClientOptions
	if opts == nil {
		opts = DefaultClientOptions()
	}
	ns, err := armrelay.NewNamespacesClient(cfg.SubscriptionID, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("new namespaces client: %w", err)
	}

	listenerKey, err := readPermanentKey(ctx, ns, cfg, PermanentListenerRuleName)
	if err != nil {
		return nil, err
	}
	senderKey, err := readPermanentKey(ctx, ns, cfg, PermanentSenderRuleName)
	if err != nil {
		return nil, err
	}
	return &RunRules{
		ListenerName: PermanentListenerRuleName,
		ListenerKey:  listenerKey,
		SenderName:   PermanentSenderRuleName,
		SenderKey:    senderKey,
	}, nil
}

// Teardown is a no-op: RunRules is read-only and the permanent rules
// are owned by `e2e-infra setup`, surviving past every test invocation.
// The method exists so callers can `defer rr.Teardown()` symmetrically
// with other resource handles.
func (r *RunRules) Teardown(ctx context.Context) error { return nil }

// readPermanentKey fetches the primary key for the named permanent
// rule, retrying briefly on transient ARM errors. A 404 is surfaced
// with a hint pointing at `e2e-infra setup` — the most common cause is
// running tests against a namespace that has not been provisioned yet.
func readPermanentKey(ctx context.Context, ns *armrelay.NamespacesClient, cfg Config, name string) (string, error) {
	deadline := time.Now().Add(readKeyMaxWait)
	var lastErr error
	for {
		resp, err := ns.ListKeys(ctx, cfg.ResourceGroup, cfg.Namespace, name, nil)
		if err == nil {
			if resp.PrimaryKey == nil {
				return "", fmt.Errorf("ListKeys(%s) returned nil PrimaryKey", name)
			}
			return *resp.PrimaryKey, nil
		}
		lastErr = err
		if isNotFound(err) {
			return "", fmt.Errorf("permanent SAS rule %q (or its parent namespace %s/%s) not found — run `e2e-infra setup` to provision (or `e2e-infra ci` for CI setup): %w",
				name, cfg.ResourceGroup, cfg.Namespace, err)
		}
		if !isTransient(err) {
			return "", fmt.Errorf("ListKeys(%s): %w", name, err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("ListKeys(%s) timed out after %s: %w", name, readKeyMaxWait, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// validateForRunRules is the same as validate today (Subscription, RG,
// Namespace required). Kept as a separate entry point so a future
// addition of a "RunRules required" field on Config doesn't accidentally
// short-circuit AcquireRunRules — which is what populates that field.
func (c Config) validateForRunRules() error { return c.validate() }

// IsNotFound reports whether err is an ARM HTTP 404 NotFound response.
// Useful for callers that treat absent resources as success during
// cleanup (e.g. teardown / janitor sweeps that race other deletes).
// Inspects azcore.ResponseError.StatusCode only; 404s from any parent
// scope (missing namespace, missing resource group) are returned as
// true on the same basis, which is the intended behaviour for cleanup
// callers — the cleanup post-condition holds regardless.
func IsNotFound(err error) bool { return isNotFound(err) }

func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	return respErr.StatusCode == 404
}
