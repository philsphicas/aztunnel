module github.com/philsphicas/aztunnel

go 1.26.0

require (
	github.com/Azure/azure-sdk-for-go/sdk/azcore v1.21.1
	github.com/Azure/azure-sdk-for-go/sdk/azidentity v1.13.1
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/relay/armrelay v1.2.0
	github.com/KimMachineGun/automemlimit v0.7.5
	github.com/alecthomas/kong v1.15.0
	github.com/coder/websocket v1.8.14
	github.com/philsphicas/aztunnel/mockrelay v0.0.0-00010101000000-000000000000
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/willabides/kongplete v0.4.0
	golang.org/x/crypto v0.52.0
)

// mockrelay is a sibling module in this repo. The workspace
// (go.work) resolves the import for in-workspace builds; this
// replace directive covers GOWORK=off / GOWORK=auto invocations
// inside this repo. Downstream consumers of this module never
// need to resolve mockrelay — it's imported only from test
// packages (e2e/backends/mock/*_test.go) and the aztunnel main
// binary doesn't depend on it. (Replace directives in non-main
// modules are ignored by Go anyway, so consumers couldn't act on
// this even if they tried.)
replace github.com/philsphicas/aztunnel/mockrelay => ./mockrelay

require (
	github.com/Azure/azure-sdk-for-go/sdk/internal v1.12.0 // indirect
	github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources v1.2.0 // indirect
	github.com/AzureAD/microsoft-authentication-library-for-go v1.6.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pbnjay/memory v0.0.0-20210728143218-7b4eea64cf58 // indirect
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c // indirect
	github.com/posener/complete v1.2.3 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/riywo/loginshell v0.0.0-20200815045211-7d26008be1ab // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)
