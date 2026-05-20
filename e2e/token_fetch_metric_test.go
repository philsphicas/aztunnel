//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestTokenFetchMetric_Azure verifies that aztunnel_token_fetch_seconds
// (histogram) and aztunnel_token_fetch_total (counter) appear on a real
// Azure-bound subprocess's metrics surface after a successful dial,
// with the `provider` label matching the auth method in use.
//
// Both auth methods are exercised — Entra and SAS — so the same
// invariants are checked for both production code paths
// (resolveAuth → relay.WithMetrics wrapping). The sender path is
// asserted, not the listener, because the sender's dial is the
// observable trigger for the wrapper from a single round-trip: the
// listener's control-channel attach also fetches a token but completes
// before the test holds an addressable metrics endpoint to scrape.
// Asserting the sender side is sufficient for confirming the
// production wiring is intact end-to-end.
func TestTokenFetchMetric_Azure(t *testing.T) {
	t.Parallel()
	env := requireDedicatedHyco(t)

	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			echo := startEchoServer(t)
			listener := startListener(t, env, auth,
				"--allow", echo.Addr(),
			)
			sender := startPortForwardSender(t, env, auth, echo.Addr(),
				"--metrics-addr", "127.0.0.1:0",
			)

			waitForLog(t, listener, "control channel connected", 30*time.Second)
			senderAddr := waitForLogAddr(t, sender, "port-forward listening", 15*time.Second)
			senderMetrics := sender.MetricsAddr(t, 15*time.Second)

			// One successful round-trip ensures the sender's dial
			// actually ran (and therefore exercised the wrapped
			// TokenProvider) before we scrape metrics.
			conn, err := net.DialTimeout("tcp", senderAddr, 10*time.Second)
			if err != nil {
				t.Fatalf("dial sender: %v", err)
			}
			defer conn.Close() //nolint:errcheck // best-effort cleanup
			payload := []byte("token-fetch-metric\n")
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				t.Fatalf("read: %v", err)
			}

			// Expected provider label: auth.name is "entra" or "sas",
			// which matches relay.ProviderEntra / relay.ProviderSAS by
			// construction.
			counterLine := fmt.Sprintf(`aztunnel_token_fetch_total{provider="%s",result="ok"}`, auth.name)
			waitForMetricsContains(t, senderMetrics, counterLine, 15*time.Second)

			// Both the counter and the histogram must have at least
			// one sample under the (provider=auth.name, result=ok)
			// label pair. The histogram's _count line confirms
			// observations landed, not just the counter.
			histCountLine := fmt.Sprintf(`aztunnel_token_fetch_seconds_count{provider="%s",result="ok"}`, auth.name)
			body := waitForMetricsContains(t, senderMetrics, histCountLine, 5*time.Second)

			// Sanity-check the counter actually has a positive value
			// (the substring match would also pass for a 0 value).
			counterValue := metricLineValue(t, body, counterLine)
			if counterValue < 1 {
				t.Errorf("%s = %v, want >= 1", counterLine, counterValue)
			}
			histCountValue := metricLineValue(t, body, histCountLine)
			if histCountValue < 1 {
				t.Errorf("%s = %v, want >= 1", histCountLine, histCountValue)
			}

			// The histogram count must equal the counter value:
			// ObserveTokenFetch increments both per call. They are
			// distinct Prometheus collectors so the two writes are
			// not atomic, but the test holds the wrapper still — no
			// new GetToken calls happen between the dial completing
			// and the scrape, so a single Gather observes consistent
			// values.
			if histCountValue != counterValue {
				t.Errorf("histogram count %v != counter %v (wrapper must observe both per call)",
					histCountValue, counterValue)
			}

			// No other provider label may appear on these metric
			// families — the wiring should only register one
			// provider per process.
			assertNoOtherProvider(t, body, "aztunnel_token_fetch_total", auth.name)
		})
	}
}

// metricLineValue parses the value off the end of the first Prometheus
// text line in body that starts with linePrefix. Returns 0 on miss.
// Used by TestTokenFetchMetric_Azure to read a single labelled sample
// without bringing in a full prom-text parser.
func metricLineValue(t *testing.T, body, linePrefix string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, linePrefix) {
			continue
		}
		// Line format: name{labels} value [timestamp]
		idx := strings.LastIndex(line, " ")
		if idx == -1 || idx == len(line)-1 {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(line[idx+1:], "%f", &v); err != nil {
			t.Logf("could not parse value from line %q: %v", line, err)
			continue
		}
		return v
	}
	return 0
}

// assertNoOtherProvider fails the test if family appears in body with
// a `provider="..."` label that is not wantProvider. Catches
// accidental wiring of multiple TokenProvider implementations under
// distinct provider labels in one process, which is not a supported
// production configuration.
func assertNoOtherProvider(t *testing.T, body, family, wantProvider string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, family+"{") {
			continue
		}
		const marker = `provider="`
		i := strings.Index(line, marker)
		if i == -1 {
			continue
		}
		rest := line[i+len(marker):]
		end := strings.IndexByte(rest, '"')
		if end == -1 {
			continue
		}
		got := rest[:end]
		if got != wantProvider {
			t.Errorf("%s has unexpected provider label %q (want only %q): %s",
				family, got, wantProvider, line)
		}
	}
}
