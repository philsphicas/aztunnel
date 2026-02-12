package relay

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGenerateSASToken(t *testing.T) {
	token, err := GenerateSASToken("https://test.servicebus.windows.net/myhc", "mypolicy", "test-secret-key", time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(token, "SharedAccessSignature ") {
		t.Errorf("token should start with SharedAccessSignature, got: %s", token)
	}
	for _, field := range []string{"sr=", "sig=", "se=", "skn=mypolicy"} {
		if !strings.Contains(token, field) {
			t.Errorf("token missing %q: %s", field, token)
		}
	}
}

func TestEndpointConversion(t *testing.T) {
	ep := "sb://test.servicebus.windows.net"
	if got := EndpointToWSS(ep); got != "wss://test.servicebus.windows.net" {
		t.Errorf("WSS = %q", got)
	}
	if got := EndpointToHTTPS(ep); got != "https://test.servicebus.windows.net" {
		t.Errorf("HTTPS = %q", got)
	}
}

func TestResourceURI(t *testing.T) {
	got := ResourceURI("sb://test.servicebus.windows.net", "myhc")
	want := "https://test.servicebus.windows.net/myhc"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSASTokenProvider_GetToken(t *testing.T) {
	tp := &SASTokenProvider{KeyName: "mypolicy", Key: "test-secret-key"}
	token, err := tp.GetToken(context.Background(), "https://test.servicebus.windows.net/myhc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(token, "SharedAccessSignature ") {
		t.Errorf("token should start with SharedAccessSignature, got: %s", token)
	}
	if !strings.Contains(token, "skn=mypolicy") {
		t.Errorf("token missing policy name: %s", token)
	}
}

func TestEntraTokenProvider_GetToken(t *testing.T) {
	// Use a mock credential to test the EntraTokenProvider without
	// requiring real Azure credentials.
	mock := &mockTokenCredential{token: "mock-entra-token"}
	tp := NewEntraTokenProviderWithCredential(mock)

	token, err := tp.GetToken(context.Background(), "https://test.servicebus.windows.net/myhc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "mock-entra-token" {
		t.Errorf("got %q, want %q", token, "mock-entra-token")
	}
	if mock.lastScope != "https://relay.azure.net/.default" {
		t.Errorf("scope = %q, want %q", mock.lastScope, "https://relay.azure.net/.default")
	}
}
