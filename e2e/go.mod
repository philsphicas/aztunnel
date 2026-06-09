// Package e2e/... is the test surface for aztunnel: scenarios that run
// against multiple backends, the Azure-Relay and in-process mock backend
// adapters, and the per-test hyco provisioner library. Kept as a separate
// Go module so the test-only dependencies — most notably the mockrelay
// websocket server — do not enter the main aztunnel module's import
// graph. That keeps `go install github.com/philsphicas/aztunnel/cmd/aztunnel`
// free of `replace` directives and free of any reference to the in-repo
// mockrelay sibling module.
//
// The Azure-specific maintainer CLI keeps its own nested module under
// e2e/infra/ to isolate Microsoft Graph and GitHub-API deps even further.
module github.com/philsphicas/aztunnel/e2e

go 1.26.4

replace github.com/philsphicas/aztunnel => ../

replace github.com/philsphicas/aztunnel/mockrelay => ../mockrelay

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.22.0
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay v1.2.0
	github.com/coder/websocket v1.8.14
	github.com/philsphicas/aztunnel v0.0.0
	github.com/philsphicas/aztunnel/mockrelay v0.0.0-00010101000000-000000000000
	github.com/prometheus/client_model v0.6.2
	golang.org/x/crypto v0.53.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources v1.2.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.6.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)
