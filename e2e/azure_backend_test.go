//go:build e2e

package e2e

import (
	"strconv"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/relayparity"
)

// azureBackend implements relayparity.Backend against a real Azure
// Relay namespace. It is the source-of-truth side of the parity
// matrix: any scenario divergence between this backend and the
// in-process MockBackend (mockrelay/parity) is a behavioural gap in
// the mock that we have to either fix or document.
//
// Each instance is bound to a single authConfig (Entra or SAS) so the
// caller can run the same scenario suite once per available auth.
// All listeners and the sender are real aztunnel subprocesses driven
// by the existing helpers (startListener, startPortForwardSender,
// startSOCKS5Sender), so they exercise the same code paths that
// production users hit.
type azureBackend struct {
	env  *relayEnv
	auth authConfig
}

// Name returns the backend identifier used in test sub-paths, e.g.
// "azure-entra" or "azure-sas".
func (b *azureBackend) Name() string { return "azure-" + b.auth.name }

// Setup brings up the requested topology and blocks until every
// listener has logged "control channel connected" and the sender has
// logged its bind address. All subprocesses are torn down via the
// existing t.Cleanup wiring inside startAztunnelWithSAS.
func (b *azureBackend) Setup(t *testing.T, opts relayparity.SetupOptions) *relayparity.Tunnel {
	t.Helper()
	if opts.NumListeners < 1 {
		t.Fatalf("NumListeners must be >= 1, got %d", opts.NumListeners)
	}

	listenerArgs := []string{}
	for _, target := range opts.AllowedTargets {
		listenerArgs = append(listenerArgs, "--allow", target)
	}
	if opts.MaxConnections > 0 {
		listenerArgs = append(listenerArgs, "--max-connections",
			strconv.Itoa(opts.MaxConnections))
	}

	for i := 0; i < opts.NumListeners; i++ {
		lst := startListener(t, b.env, b.auth, listenerArgs...)
		waitForLog(t, lst, "control channel connected", 30*time.Second)
	}

	var senderAddr string
	switch opts.SenderMode {
	case relayparity.ModePortForward:
		s := startPortForwardSender(t, b.env, b.auth, opts.Target)
		senderAddr = waitForLogAddr(t, s, "port-forward listening", 15*time.Second)
	case relayparity.ModeSOCKS5:
		s := startSOCKS5Sender(t, b.env, b.auth)
		senderAddr = waitForLogAddr(t, s, "socks5-proxy listening", 15*time.Second)
	default:
		t.Fatalf("unknown SenderMode %v", opts.SenderMode)
	}

	return &relayparity.Tunnel{SenderAddr: senderAddr}
}
