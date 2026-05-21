// Package scenarios defines a backend abstraction for running the
// same end-to-end test scenarios against multiple relay implementations.
//
// Two implementations exist:
//
//   - github.com/philsphicas/aztunnel/e2e/backends/mock.MockBackend
//     runs the mock relay server (mockrelay/server) and the aztunnel
//     listener and sender in-process. Fast, deterministic, no external
//     dependencies. Lives in the root module's e2e tree but only
//     imports the mockrelay module from test packages, so the
//     in-process server does not enter aztunnel's binary dependency
//     graph.
//
//   - github.com/philsphicas/aztunnel/e2e/backends/azure (azureBackend,
//     build tag e2e) drives subprocess aztunnel listeners and senders
//     against a real Azure Relay namespace. Runs in the e2e CI job and
//     requires Azure credentials.
//
// Scenarios in this package describe observable behavior of the
// tunnel and are written once. They run against both backends so any
// divergence is surfaced as a test failure (or documented contract
// gap).
package scenarios
