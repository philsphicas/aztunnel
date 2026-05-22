// Package e2e is the umbrella for aztunnel's end-to-end test surface.
// The package itself has no Go source beyond this file; tests live in
// the sub-packages e2e/scenarios (backend-agnostic), e2e/backends/{azure,mock}
// (backend adapters + per-backend tests), and e2e/azrelay (per-test
// hyco provisioner for the Azure backend). The setup CLI lives in the
// separate e2e/infra Go module.
package e2e
