package arc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/coder/websocket"
)

// fakeCredential implements azcore.TokenCredential for testing.
type fakeCredential struct{}

func (fakeCredential) GetToken(_ context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "fake-token"}, nil
}

// newTestClient creates a Client backed by the given test server.
// It uses cloud.Configuration to point the ARM endpoint at the test server,
// so the client's public methods construct correct URLs without any
// transport-level rewrites.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	opts := &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Cloud: cloud.Configuration{
				Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
					cloud.ResourceManager: {
						Endpoint: srv.URL,
						Audience: srv.URL,
					},
				},
			},
			Transport: srv.Client(),
		},
	}
	c, err := NewClientWithCredential(fakeCredential{}, slog.Default(), opts)
	if err != nil {
		t.Fatalf("newTestClient: %v", err)
	}
	return c
}

func TestRelayInfoEndpoint(t *testing.T) {
	r := &RelayInfo{
		NamespaceName:       "my-relay",
		NamespaceNameSuffix: "servicebus.windows.net",
	}
	want := "my-relay.servicebus.windows.net"
	if got := r.Endpoint(); got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
}

func TestEnsureHybridConnectivity(t *testing.T) {
	const resourceID = "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.HybridCompute/machines/vm1"

	type requestRecord struct {
		method string
		path   string
		query  string
	}

	t.Run("creates endpoint and service config", func(t *testing.T) {
		var requests []requestRecord
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests = append(requests, requestRecord{
				method: r.Method,
				path:   r.URL.Path,
				query:  r.URL.RawQuery,
			})
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		if err := c.EnsureHybridConnectivity(context.Background(), resourceID, "SSH", 2222); err != nil {
			t.Fatalf("EnsureHybridConnectivity: %v", err)
		}

		if len(requests) != 2 {
			t.Fatalf("expected 2 requests, got %d", len(requests))
		}

		// First PUT: endpoint creation
		if requests[0].method != http.MethodPut {
			t.Errorf("request 0: expected PUT, got %s", requests[0].method)
		}
		wantPath := resourceID + "/providers/Microsoft.HybridConnectivity/endpoints/default"
		if requests[0].path != wantPath {
			t.Errorf("request 0: path = %q, want %q", requests[0].path, wantPath)
		}
		if !strings.Contains(requests[0].query, "api-version=2023-03-15") {
			t.Errorf("request 0: missing api-version in query: %s", requests[0].query)
		}

		// Second PUT: service configuration
		if requests[1].method != http.MethodPut {
			t.Errorf("request 1: expected PUT, got %s", requests[1].method)
		}
		wantPath = resourceID + "/providers/Microsoft.HybridConnectivity/endpoints/default/serviceConfigurations/SSH"
		if requests[1].path != wantPath {
			t.Errorf("request 1: path = %q, want %q", requests[1].path, wantPath)
		}
	})

	t.Run("defaults for empty service and zero port", func(t *testing.T) {
		var requests []requestRecord
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests = append(requests, requestRecord{
				method: r.Method,
				path:   r.URL.Path,
			})
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		if err := c.EnsureHybridConnectivity(context.Background(), resourceID, "", 0); err != nil {
			t.Fatalf("EnsureHybridConnectivity: %v", err)
		}

		if len(requests) != 2 {
			t.Fatalf("expected 2 requests, got %d", len(requests))
		}

		// Service config URL should use default "SSH"
		wantPath := resourceID + "/providers/Microsoft.HybridConnectivity/endpoints/default/serviceConfigurations/SSH"
		if requests[1].path != wantPath {
			t.Errorf("request 1: path = %q, want %q", requests[1].path, wantPath)
		}
	})

	t.Run("endpoint PUT failure", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error": {"code": "AuthorizationFailed"}}`))
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		err := c.EnsureHybridConnectivity(context.Background(), resourceID, "SSH", 22)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "create HybridConnectivity endpoint") {
			t.Errorf("error should mention endpoint creation: %v", err)
		}
	})
}

func TestGetRelayCredentials(t *testing.T) {
	const resourceID = "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.HybridCompute/machines/vm1"

	validResp := listCredentialsResponse{
		Relay: RelayInfo{
			NamespaceName:             "azgnrelay-eastus-l1",
			NamespaceNameSuffix:       "servicebus.windows.net",
			HybridConnectionName:      "microsoft.hybridcompute/machines/vm1/abc123",
			AccessKey:                 "SharedAccessSignature sr=test&sig=test",
			ExpiresOn:                 9999999999,
			ServiceConfigurationToken: "eyJ.test.jwt",
		},
	}

	t.Run("success", func(t *testing.T) {
		var capturedPath, capturedQuery, capturedService string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			capturedQuery = r.URL.RawQuery
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			capturedService = body["serviceName"]
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(validResp)
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		info, err := c.GetRelayCredentials(context.Background(), resourceID, "SSH")
		if err != nil {
			t.Fatalf("GetRelayCredentials: %v", err)
		}

		wantPath := resourceID + "/providers/Microsoft.HybridConnectivity/endpoints/default/listCredentials"
		if capturedPath != wantPath {
			t.Errorf("path = %q, want %q", capturedPath, wantPath)
		}
		if !strings.Contains(capturedQuery, "expiresin=10800") {
			t.Errorf("missing expiresin in query: %s", capturedQuery)
		}
		if !strings.Contains(capturedQuery, "api-version=2023-03-15") {
			t.Errorf("missing api-version in query: %s", capturedQuery)
		}
		if capturedService != "SSH" {
			t.Errorf("serviceName = %q, want SSH", capturedService)
		}
		if info.NamespaceName != "azgnrelay-eastus-l1" {
			t.Errorf("namespace = %q, want azgnrelay-eastus-l1", info.NamespaceName)
		}
		if info.HybridConnectionName != "microsoft.hybridcompute/machines/vm1/abc123" {
			t.Errorf("hybridConnection = %q", info.HybridConnectionName)
		}
	})

	t.Run("default service name", func(t *testing.T) {
		var capturedService string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			capturedService = body["serviceName"]
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(validResp)
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		_, err := c.GetRelayCredentials(context.Background(), resourceID, "")
		if err != nil {
			t.Fatalf("GetRelayCredentials: %v", err)
		}
		if capturedService != "SSH" {
			t.Errorf("default serviceName = %q, want SSH", capturedService)
		}
	})

	t.Run("incomplete response missing namespace", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(listCredentialsResponse{
				Relay: RelayInfo{
					HybridConnectionName: "some/name",
				},
			})
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		_, err := c.GetRelayCredentials(context.Background(), resourceID, "SSH")
		if err == nil {
			t.Fatal("expected error for incomplete response")
		}
		if !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("error should mention incomplete: %v", err)
		}
	})

	t.Run("incomplete response missing hybridConnection", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(listCredentialsResponse{
				Relay: RelayInfo{
					NamespaceName: "ns",
				},
			})
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		_, err := c.GetRelayCredentials(context.Background(), resourceID, "SSH")
		if err == nil {
			t.Fatal("expected error for incomplete response")
		}
		if !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("error should mention incomplete: %v", err)
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error": {"code": "EndpointNotFound"}}`))
		}))
		defer srv.Close()

		c := newTestClient(t, srv)
		_, err := c.GetRelayCredentials(context.Background(), resourceID, "SSH")
		if err == nil {
			t.Fatal("expected error for 404")
		}
		if !strings.Contains(err.Error(), "list credentials") {
			t.Errorf("error should mention list credentials: %v", err)
		}
	})
}

func TestARMErrorHandling(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": {"code": "ResourceNotFound", "message": "not found"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	err := c.armPUT(context.Background(), srv.URL+"/notfound", `{}`)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should contain status code: %v", err)
	}
}

func TestSanitizeErr(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"query param token", "dial wss://host/$hc/path?sb-hc-action=connect&sb-hc-token=SECRET"},
		{"header with space", "failed: Servicebusauthorization: SharedAccessSignature sr=test&sig=SECRET&se=123"},
		{"header no space", "failed: Servicebusauthorization:SharedAccessSignature SECRET"},
		{"config token", "error: Service-Configuration-Token: eyJhbGciOi.SECRET.payload rest"},
		{"lowercase header", "failed: servicebusauthorization: SECRET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sanitizeErr(errString(tt.input))
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("token not redacted: %v", err)
			}
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("expected REDACTED in error: %v", err)
			}
		})
	}

	t.Run("no matching patterns", func(t *testing.T) {
		err := sanitizeErr(errString("connection refused"))
		if err.Error() != "connection refused" {
			t.Errorf("expected unchanged error, got %q", err.Error())
		}
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func TestDialWithLoggerRetry(t *testing.T) {
	t.Run("retries on 404 then succeeds", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))
		defer srv.Close()

		origTransport := http.DefaultTransport
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		}
		defer func() { http.DefaultTransport = origTransport }()

		// Point info at test server. The URL is built as
		// wss://{NamespaceName}.{NamespaceNameSuffix}/$hc/...
		// We need the combined host to resolve to the test server, so
		// we use the raw host:port as-is with no dot separator by
		// calling Dial directly. Instead, we override the wss host
		// construction. Since DialWithLogger calls Endpoint() internally,
		// we set the suffix so that "name.suffix" equals the test host.
		srvHost := strings.TrimPrefix(srv.URL, "https://")
		// Split into name and suffix at an artificial boundary.
		// e.g. "127" + "0.0.1:34407"
		dotIdx := strings.Index(srvHost, ".")
		testInfo := &RelayInfo{
			NamespaceName:             srvHost[:dotIdx],
			NamespaceNameSuffix:       srvHost[dotIdx+1:],
			HybridConnectionName:      "test-hyco",
			AccessKey:                 "test-key",
			ServiceConfigurationToken: "test-token",
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, err := DialWithLogger(ctx, testInfo, 22, logger)
		if err != nil {
			t.Fatalf("DialWithLogger: %v", err)
		}
		defer ws.CloseNow()

		mu.Lock()
		got := attempts
		mu.Unlock()
		if got < 2 {
			t.Errorf("expected at least 2 attempts, got %d", got)
		}
	})

	t.Run("401 fails immediately without retry", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		origTransport := http.DefaultTransport
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		}
		defer func() { http.DefaultTransport = origTransport }()

		host := strings.TrimPrefix(srv.URL, "https://")
		dotIdx := strings.Index(host, ".")
		testInfo := &RelayInfo{
			NamespaceName:             host[:dotIdx],
			NamespaceNameSuffix:       host[dotIdx+1:],
			HybridConnectionName:      "test-hyco",
			AccessKey:                 "test-key",
			ServiceConfigurationToken: "test-token",
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := DialWithLogger(ctx, testInfo, 22, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		mu.Lock()
		got := attempts
		mu.Unlock()
		if got != 1 {
			t.Errorf("expected exactly 1 attempt (no retry), got %d", got)
		}
	})

	t.Run("retries on 503 then succeeds", func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0

		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()

			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			<-r.Context().Done()
		}))
		defer srv.Close()

		origTransport := http.DefaultTransport
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		}
		defer func() { http.DefaultTransport = origTransport }()

		host := strings.TrimPrefix(srv.URL, "https://")
		dotIdx := strings.Index(host, ".")
		testInfo := &RelayInfo{
			NamespaceName:             host[:dotIdx],
			NamespaceNameSuffix:       host[dotIdx+1:],
			HybridConnectionName:      "test-hyco",
			AccessKey:                 "test-key",
			ServiceConfigurationToken: "test-token",
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		ws, err := DialWithLogger(ctx, testInfo, 22, logger)
		if err != nil {
			t.Fatalf("DialWithLogger: %v", err)
		}
		defer ws.CloseNow()

		mu.Lock()
		got := attempts
		mu.Unlock()
		if got != 2 {
			t.Errorf("expected 2 attempts, got %d", got)
		}
	})

	t.Run("context cancelled during retry returns error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		origTransport := http.DefaultTransport
		http.DefaultTransport = &http.Transport{
			TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
		}
		defer func() { http.DefaultTransport = origTransport }()

		host := strings.TrimPrefix(srv.URL, "https://")
		dotIdx := strings.Index(host, ".")
		testInfo := &RelayInfo{
			NamespaceName:             host[:dotIdx],
			NamespaceNameSuffix:       host[dotIdx+1:],
			HybridConnectionName:      "test-hyco",
			AccessKey:                 "test-key",
			ServiceConfigurationToken: "test-token",
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		_, err := DialWithLogger(ctx, testInfo, 22, logger)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "dial arc relay") {
			t.Errorf("error %q should contain 'dial arc relay'", err.Error())
		}
	})
}
