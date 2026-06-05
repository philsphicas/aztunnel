// Mock relay server module — see ./README.md.
//
// This module is intentionally separate from the parent aztunnel module
// so that consumers of aztunnel-the-client don't pull in the relay code
// or its dependencies, and so the relay can evolve independently. The
// SAS-token helper and aztunnel-relay CLI tests import the parent
// aztunnel module for end-to-end verification (`server/auth_test.go`
// imports `internal/relay`; `cmd/aztunnel-relay/helpers_test.go`
// builds `cmd/aztunnel` as a subprocess).
module github.com/philsphicas/aztunnel/mockrelay

go 1.26.4

require (
	github.com/KimMachineGun/automemlimit v0.7.5
	github.com/alecthomas/kong v1.15.0
	github.com/coder/websocket v1.8.14
	github.com/philsphicas/aztunnel v0.0.0
	golang.org/x/sync v0.20.0
)

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.22.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.6.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/pbnjay/memory v0.0.0-20210728143218-7b4eea64cf58 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/philsphicas/aztunnel => ../
