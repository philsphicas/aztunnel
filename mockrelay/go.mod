// Mock relay server module — see ./README.md.
//
// This module is intentionally separate from the parent aztunnel module
// so that consumers of aztunnel-the-client don't pull in the relay code
// or its dependencies, and so the relay can evolve independently. The
// integration test in server/integration_test.go imports the parent
// aztunnel module for end-to-end verification (one-way dependency).
module github.com/philsphicas/aztunnel/mockrelay

go 1.26.0

require (
	github.com/KimMachineGun/automemlimit v0.7.5
	github.com/alecthomas/kong v1.15.0
	github.com/coder/websocket v1.8.14
	github.com/philsphicas/aztunnel v0.0.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.21.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.6.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pbnjay/memory v0.0.0-20210728143218-7b4eea64cf58 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/prometheus/client_golang v1.23.2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)

replace github.com/philsphicas/aztunnel => ../
