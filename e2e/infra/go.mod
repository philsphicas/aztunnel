// Package e2e/infra hosts the maintainer-facing setup CLI for the aztunnel
// E2E test infrastructure. Kept as a separate module so that its heavier
// dependencies (go-github, Microsoft Graph helpers, armresources,
// armauthorization) do not enter the main aztunnel module's dependency
// graph.
//
// The CLI mirrors the historic shell-script Makefile (`setup`, `ci`,
// `clean`, `grant`, `env`) and adds a `janitor` subcommand used by the
// daily cleanup workflow.
//
// Per-invocation hyco provisioning lives in github.com/philsphicas/aztunnel/e2e/azrelay
// in the sibling e2e module. The `replace` directive below keeps the
// module buildable outside workspace mode (GOWORK=off); go.work is
// used for the workspace-development experience.
module github.com/philsphicas/aztunnel/e2e/infra

go 1.26.4

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.22.0
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2 v2.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay v1.2.0
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources v1.2.0
	github.com/alecthomas/kong v1.15.0
	github.com/google/go-github/v86 v86.0.0
	github.com/google/uuid v1.6.0
	github.com/philsphicas/aztunnel/e2e v0.0.0
	golang.org/x/crypto v0.52.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.6.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/philsphicas/aztunnel/e2e => ..
