//go:build e2e

package e2e

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/testharness/relayparity"
)

// TestParity_Azure runs the shared core parity suite against a real
// Azure Relay namespace, once per configured auth method (Entra,
// SAS). The same scenarios run against the in-process MockBackend in
// mockrelay/testharness/parity — any divergence between this test and that one is
// a behavioural gap to fix in the mock.
func TestParity_Azure(t *testing.T) {
	env := requireRelayEnv(t)
	for _, auth := range availableAuths(t, env) {
		auth := auth
		t.Run(auth.name, func(t *testing.T) {
			b := &azureBackend{env: env, auth: auth}
			relayparity.RunCoreSuite(t, b)
			relayparity.RunTopologySuite(t, b)
		})
	}
}
