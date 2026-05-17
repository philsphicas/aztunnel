// Package janitor deletes orphan per-invocation hybrid connections from
// the aztunnel E2E relay namespace. Names are matched against
// azrelay.HycoNamePattern so static bootstrap hycos and unrelated hycos
// in the namespace are not touched.
package janitor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay"

	"github.com/philsphicas/aztunnel/e2e/azrelay"
)

// Config configures Run.
type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Namespace      string
	// MaxAge is the minimum age before a matching hyco is considered
	// orphaned. Hycos younger than MaxAge are skipped (they may belong
	// to a currently-running test).
	MaxAge time.Duration
	// DryRun, when true, prints what would be deleted without deleting.
	DryRun bool
	// Cred is the Azure credential to use.
	Cred azcore.TokenCredential
}

// Run lists hycos in the namespace, filters by name pattern and age, and
// deletes the matching ones. Per-delete errors are logged but do not
// abort the run; the function returns the first list-time error or nil.
func Run(ctx context.Context, cfg Config) error {
	if cfg.SubscriptionID == "" || cfg.ResourceGroup == "" || cfg.Namespace == "" {
		return fmt.Errorf("janitor: SubscriptionID, ResourceGroup, Namespace are required")
	}
	if cfg.MaxAge <= 0 {
		return fmt.Errorf("janitor: MaxAge must be positive")
	}
	if cfg.Cred == nil {
		return fmt.Errorf("janitor: Cred is required")
	}
	client, err := armrelay.NewHybridConnectionsClient(cfg.SubscriptionID, cfg.Cred, nil)
	if err != nil {
		return fmt.Errorf("new hybrid connections client: %w", err)
	}
	now := time.Now().UTC()
	threshold := now.Add(-cfg.MaxAge)

	fmt.Fprintf(os.Stderr, "==> janitor: namespace=%s max-age=%s (cutoff=%s) dry-run=%t\n",
		cfg.Namespace, cfg.MaxAge, threshold.Format(time.RFC3339), cfg.DryRun)

	pager := client.NewListByNamespacePager(cfg.ResourceGroup, cfg.Namespace, nil)
	var matched, deleted, failed, missingCreatedAt int
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list hycos: %w", err)
		}
		for _, hc := range page.Value {
			if hc == nil || hc.Name == nil {
				continue
			}
			name := *hc.Name
			if !azrelay.HycoNamePattern.MatchString(name) {
				continue
			}
			matched++
			created := hcCreatedAt(hc)
			if created.IsZero() {
				// Some ARM list responses strip timestamps; fall back
				// to a per-item GET before deciding.
				full, getErr := client.Get(ctx, cfg.ResourceGroup, cfg.Namespace, name, nil)
				if getErr != nil {
					fmt.Fprintf(os.Stderr, "    ? %s: list missing createdAt; GET failed: %v; skipping\n", name, getErr)
					missingCreatedAt++
					continue
				}
				created = hcCreatedAt(&full.HybridConnection)
				if created.IsZero() {
					fmt.Fprintf(os.Stderr, "    ? %s: createdAt unavailable after GET; skipping\n", name)
					missingCreatedAt++
					continue
				}
			}
			age := now.Sub(created)
			if created.After(threshold) {
				fmt.Fprintf(os.Stderr, "    · %s: age=%s < max-age; skipping\n", name, age.Round(time.Second))
				continue
			}
			if cfg.DryRun {
				fmt.Fprintf(os.Stderr, "    - %s: age=%s; would delete (dry-run)\n", name, age.Round(time.Second))
				continue
			}
			if _, err := client.Delete(ctx, cfg.ResourceGroup, cfg.Namespace, name, nil); err != nil {
				fmt.Fprintf(os.Stderr, "    ! %s: delete failed: %v\n", name, err)
				failed++
				continue
			}
			fmt.Fprintf(os.Stderr, "    ✓ %s: age=%s; deleted\n", name, age.Round(time.Second))
			deleted++
		}
	}
	fmt.Fprintf(os.Stderr, "==> janitor: matched=%d deleted=%d failed=%d missing-createdAt=%d\n",
		matched, deleted, failed, missingCreatedAt)
	// Any matched hyco that we couldn't determine the age of is silently
	// skipped above (the janitor cannot decide whether it's an orphan).
	// Surface even a partial outage loudly so operators don't lose track
	// of slowly-accreting orphans when only some entries lack createdAt.
	if missingCreatedAt > 0 {
		fmt.Fprintf(os.Stderr, "==> janitor: WARNING: %d of %d matched hyco(s) were missing createdAt and could not be aged — orphan-reaping is degraded\n",
			missingCreatedAt, matched)
	}
	if failed > 0 {
		return fmt.Errorf("%d delete(s) failed", failed)
	}
	return nil
}

func hcCreatedAt(hc *armrelay.HybridConnection) time.Time {
	if hc == nil || hc.Properties == nil || hc.Properties.CreatedAt == nil {
		return time.Time{}
	}
	return hc.Properties.CreatedAt.UTC()
}
