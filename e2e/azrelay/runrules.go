package azrelay

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"
)

// RunRuleNamePattern matches per-`go test` namespace authorization rules
// created by AcquireRunRules. The janitor uses this exact pattern
// (anchored) to identify orphaned run rules that should be cleaned up.
// Listener and sender rules share the same 12-char suffix so a janitor
// pass can trivially correlate the two halves of a run.
var RunRuleNamePattern = regexp.MustCompile(`^e2e-run-[0-9a-f]{12}-(listener|sender)$`)

// runRulePrefix is the common prefix of every run-scoped namespace
// authorization rule. Listener and sender rules append the role
// (and share the 12-char suffix) so the janitor can correlate the
// two halves of a run.
const runRulePrefix = "e2e-run-"

// dataPlaneSettleAfterKeys gives Relay's data plane a brief grace
// period to propagate a newly-created SAS auth rule after the control
// plane (ListKeys) has acknowledged it. Without this, the first
// handshake using the fresh key can observe a 401 before the rule has
// converged across the data-plane nodes. Empirically a sub-second wait
// suffices; 2s adds a comfortable margin. Paid once per RunRules
// acquisition, not once per pair.
const dataPlaneSettleAfterKeys = 2 * time.Second

// RunRules holds the two namespace-scoped authorization rules a single
// `go test` invocation provisions: one Listen-only rule whose key the
// relay-listener uses, and one Send-only rule whose key the
// relay-sender uses. Both rules live at namespace scope, so the same
// key authorizes the corresponding action against every hybrid
// connection the run creates.
//
// Two rules (not one combined Listen+Send rule) is deliberate: the
// suite asserts that a listener key cannot be used to send and vice
// versa (TestWrongSASClaim). A single rule with both rights would
// silently pass that test.
//
// Acquire one with AcquireRunRules, propagate via Config.RunRules
// into NewProvider, and defer Teardown from TestMain.
type RunRules struct {
	ListenerName string
	ListenerKey  string
	SenderName   string
	SenderKey    string

	cfg    Config
	ns     *armrelay.NamespacesClient
	suffix string

	teardownOnce sync.Once
	teardownErr  error
}

// AcquireRunRules creates two namespace-scoped authorization rules in
// cfg.Namespace — one Listen-only, one Send-only, both named
// "e2e-run-<hex>-<role>" with a shared random 12-char suffix — reads
// their primary keys, lets the data plane settle, and returns the
// populated *RunRules.
//
// On any failure after a rule has been created, AcquireRunRules issues
// a best-effort delete before returning the error, so the caller does
// not need to handle partial state.
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
	suffix, err := newSuffix()
	if err != nil {
		return nil, fmt.Errorf("generate suffix: %w", err)
	}

	r := &RunRules{
		cfg:          cfg,
		ns:           ns,
		suffix:       suffix,
		ListenerName: runRulePrefix + suffix + "-listener",
		SenderName:   runRulePrefix + suffix + "-sender",
	}

	if err := r.createRule(ctx, r.ListenerName, armrelay.AccessRightsListen); err != nil {
		r.bestEffortDelete(ctx, r.ListenerName)
		return nil, fmt.Errorf("create %s: %w", r.ListenerName, err)
	}
	if err := r.createRule(ctx, r.SenderName, armrelay.AccessRightsSend); err != nil {
		r.bestEffortDelete(ctx, r.ListenerName)
		r.bestEffortDelete(ctx, r.SenderName)
		return nil, fmt.Errorf("create %s: %w", r.SenderName, err)
	}

	listenerKey, err := r.readKey(ctx, r.ListenerName)
	if err != nil {
		r.bestEffortDelete(ctx, r.ListenerName)
		r.bestEffortDelete(ctx, r.SenderName)
		return nil, fmt.Errorf("read %s key: %w", r.ListenerName, err)
	}
	senderKey, err := r.readKey(ctx, r.SenderName)
	if err != nil {
		r.bestEffortDelete(ctx, r.ListenerName)
		r.bestEffortDelete(ctx, r.SenderName)
		return nil, fmt.Errorf("read %s key: %w", r.SenderName, err)
	}
	r.ListenerKey = listenerKey
	r.SenderKey = senderKey

	select {
	case <-ctx.Done():
		r.bestEffortDelete(ctx, r.ListenerName)
		r.bestEffortDelete(ctx, r.SenderName)
		return nil, ctx.Err()
	case <-time.After(dataPlaneSettleAfterKeys):
	}

	return r, nil
}

// Teardown deletes both namespace authorization rules. Safe to call
// multiple times: only the first call performs the deletes; subsequent
// calls return the same error.
//
// Teardown strips cancellation from ctx (via context.WithoutCancel)
// so cleanup completes even when ctx has been cancelled (e.g. on
// TestMain timeout), but preserves any deadline the caller set. If
// ctx has no deadline, Teardown applies a defensive 60s ceiling so a
// stuck control plane cannot hang the run indefinitely.
//
// Individual delete failures are joined and returned. The janitor
// (RunRuleNamePattern sweep) will reap anything we can't clean up
// here.
func (r *RunRules) Teardown(ctx context.Context) error {
	r.teardownOnce.Do(func() {
		ctx, cancel := detachAndBoundContext(ctx, 60*time.Second)
		defer cancel()
		var errs []error
		if err := r.deleteRule(ctx, r.ListenerName); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", r.ListenerName, err))
		}
		if err := r.deleteRule(ctx, r.SenderName); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", r.SenderName, err))
		}
		if len(errs) > 0 {
			r.teardownErr = errors.Join(errs...)
		}
	})
	return r.teardownErr
}

func (r *RunRules) createRule(ctx context.Context, name string, right armrelay.AccessRights) error {
	return retryOnAuthRuleConflict(ctx, defaultAuthRuleRetry(), func() error {
		_, err := r.ns.CreateOrUpdateAuthorizationRule(ctx, r.cfg.ResourceGroup, r.cfg.Namespace, name, armrelay.AuthorizationRule{
			Properties: &armrelay.AuthorizationRuleProperties{
				Rights: []*armrelay.AccessRights{&right},
			},
		}, nil)
		return err
	})
}

// readKey fetches the primary key for the given namespace-scoped rule,
// retrying on transient 404s that occasionally follow rule creation
// before ARM has fully propagated the new entity. Bounded by
// readinessMaxWait.
func (r *RunRules) readKey(ctx context.Context, name string) (string, error) {
	deadline := time.Now().Add(readinessMaxWait)
	var lastErr error
	for {
		resp, err := r.ns.ListKeys(ctx, r.cfg.ResourceGroup, r.cfg.Namespace, name, nil)
		if err == nil {
			if resp.PrimaryKey == nil {
				return "", errors.New("ListKeys returned nil PrimaryKey")
			}
			return *resp.PrimaryKey, nil
		}
		lastErr = err
		if !isTransient(err) {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("ListKeys timed out after %s: %w", readinessMaxWait, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (r *RunRules) deleteRule(ctx context.Context, name string) error {
	// Same per-scope serialization gate as createRule — back-to-back
	// deletes on the namespace authorizationRules scope (listener
	// immediately followed by sender) can race the gate and surface
	// as 40901. The retry envelope is bounded; a stuck control plane
	// still surfaces through retryOnAuthRuleConflict's "after N
	// attempts" wrap.
	return retryOnAuthRuleConflict(ctx, defaultAuthRuleRetry(), func() error {
		_, err := r.ns.DeleteAuthorizationRule(ctx, r.cfg.ResourceGroup, r.cfg.Namespace, name, nil)
		if err != nil && isNotFound(err) {
			return nil
		}
		return err
	})
}

// bestEffortDelete swallows errors. Used on the AcquireRunRules
// failure path; the janitor will reap anything left behind. We also
// retry 40901 here for the same reason createRule does — a not-yet-
// committed create can race the cleanup delete.
func (r *RunRules) bestEffortDelete(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = retryOnAuthRuleConflict(ctx, defaultAuthRuleRetry(), func() error {
		_, err := r.ns.DeleteAuthorizationRule(ctx, r.cfg.ResourceGroup, r.cfg.Namespace, name, nil)
		if err != nil && isNotFound(err) {
			return nil
		}
		return err
	})
}

// validateForRunRules is the same as validate today (Subscription, RG,
// Namespace required) but exists as a separate entry point so a future
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
