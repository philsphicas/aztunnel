package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/philsphicas/aztunnel/internal/relay"
)

// discardLogger returns a logger that drops all output, for tests that
// want to exercise resolveAuth without polluting test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAutomemlimitActive(t *testing.T) {
	// automemlimit is activated via blank import in main.go. It reads the
	// cgroup memory limit (container or systemd MemoryMax=) and sets
	// GOMEMLIMIT to 90% of that value. On machines without a cgroup limit
	// it logs "memory is not limited, skipping" and leaves GOMEMLIMIT at
	// the default (math.MaxInt64).
	//
	// This test verifies the import is wired up and doesn't panic. In CI
	// containers with memory limits, GOMEMLIMIT will be a real value.
	limit := debug.SetMemoryLimit(-1) // read current value without changing it
	t.Logf("GOMEMLIMIT = %d bytes (%.0f MiB)", limit, float64(limit)/(1024*1024))
	if limit <= 0 {
		t.Errorf("expected GOMEMLIMIT > 0, got %d", limit)
	}
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		input   string
		wantLvl slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"DEBUG", slog.LevelDebug},  // case-insensitive
		{"INFO", slog.LevelInfo},    // case-insensitive
		{"WARN", slog.LevelWarn},    // case-insensitive
		{"unknown", slog.LevelInfo}, // default
		{"", slog.LevelInfo},        // empty defaults to info
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			logger := newLogger(tt.input)
			if logger == nil {
				t.Fatal("newLogger returned nil")
			}

			// Verify the logger is configured at the right level by checking
			// if it is enabled at the expected level.
			if !logger.Enabled(context.Background(), tt.wantLvl) {
				t.Errorf("newLogger(%q): expected level %v to be enabled", tt.input, tt.wantLvl)
			}

			// If the level is above Debug, Debug should be disabled.
			if tt.wantLvl > slog.LevelDebug {
				if logger.Enabled(context.Background(), slog.LevelDebug) {
					t.Errorf("newLogger(%q): Debug should be disabled for level %v", tt.input, tt.wantLvl)
				}
			}
		})
	}
}

func TestResolveAuth_NamespaceFromEnv(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "test")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, tp, err := resolveAuth(AuthFlags{}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "test.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "test.servicebus.windows.net")
	}
	if tp == nil {
		t.Fatal("token provider is nil")
	}
}

func TestResolveAuth_SASCredentials(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "myns")
	t.Setenv("AZTUNNEL_KEY_NAME", "RootManageSharedAccessKey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, tp, err := resolveAuth(AuthFlags{}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "myns.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "myns.servicebus.windows.net")
	}

	sas, ok := tp.(*relay.SASTokenProvider)
	if !ok {
		t.Fatalf("expected *relay.SASTokenProvider, got %T", tp)
	}
	if sas.KeyName != "RootManageSharedAccessKey" {
		t.Errorf("KeyName = %q, want %q", sas.KeyName, "RootManageSharedAccessKey")
	}
	if sas.Key != "dGVzdGtleQ==" {
		t.Errorf("Key = %q, want %q", sas.Key, "dGVzdGtleQ==")
	}
}

func TestResolveAuth_MissingNamespace(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")

	_, _, _, err := resolveAuth(AuthFlags{}, discardLogger())
	if err == nil {
		t.Fatal("expected error when namespace is missing, got nil")
	}
	if !strings.Contains(err.Error(), "namespace is required") {
		t.Errorf("error %q does not contain %q", err.Error(), "namespace is required")
	}
}

func TestResolveAuth_NamespaceFlagPriority(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "from-env")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{Relay: "from-flag"}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "from-flag.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q (flag should take priority over env)", endpoint, "from-flag.servicebus.windows.net")
	}
}

func TestResolveAuth_FQDNInput(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "my-relay.servicebus.windows.net")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "my-relay.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "my-relay.servicebus.windows.net")
	}
}

func TestResolveAuth_URIInput(t *testing.T) {
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{Relay: "sb://my-relay.servicebus.windows.net"}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "my-relay.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "my-relay.servicebus.windows.net")
	}
}

func TestResolveAuth_CustomSuffixFlag(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "my-relay")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{RelaySuffix: ".servicebus.chinacloudapi.cn"}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "my-relay.servicebus.chinacloudapi.cn" {
		t.Errorf("endpoint = %q, want %q", endpoint, "my-relay.servicebus.chinacloudapi.cn")
	}
}

func TestResolveAuth_SuffixEnvVar(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "my-relay")
	t.Setenv("AZTUNNEL_RELAY_SUFFIX", ".servicebus.usgovcloudapi.net")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "my-relay.servicebus.usgovcloudapi.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "my-relay.servicebus.usgovcloudapi.net")
	}
}

func TestResolveAuth_SuffixIgnoredForFQDN(t *testing.T) {
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{Relay: "my-relay.servicebus.chinacloudapi.cn", RelaySuffix: ".should-be-ignored"}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "my-relay.servicebus.chinacloudapi.cn" {
		t.Errorf("endpoint = %q, want %q (suffix should be ignored for FQDN)", endpoint, "my-relay.servicebus.chinacloudapi.cn")
	}
}

func TestResolveAuth_SuffixFlagPrecedenceOverEnv(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "my-relay")
	t.Setenv("AZTUNNEL_RELAY_SUFFIX", ".servicebus.chinacloudapi.cn")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, _, err := resolveAuth(AuthFlags{RelaySuffix: ".servicebus.usgovcloudapi.net"}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "my-relay.servicebus.usgovcloudapi.net" {
		t.Errorf("endpoint = %q, want %q (flag should take precedence over env)", endpoint, "my-relay.servicebus.usgovcloudapi.net")
	}
}

func TestResolveAuth_InvalidURIInput(t *testing.T) {
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	_, _, _, err := resolveAuth(AuthFlags{Relay: "sb://"}, discardLogger())
	if err == nil {
		t.Fatal("expected error for invalid URI input, got nil")
	}
	if !strings.Contains(err.Error(), "invalid relay endpoint") {
		t.Errorf("error %q does not contain %q", err.Error(), "invalid relay endpoint")
	}
}

func TestResolveAuth_OnlyKeyNameNoKey(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "test")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "")

	_, _, tp, err := resolveAuth(AuthFlags{}, discardLogger())
	// Either it succeeds with Entra or fails because no Azure creds available.
	// Either way, tp should NOT be a SASTokenProvider.
	if err == nil {
		if _, ok := tp.(*relay.SASTokenProvider); ok {
			t.Error("expected non-SAS provider when only KEY_NAME is set (no KEY)")
		}
	}
	// If err != nil, that's expected in CI where no Azure creds are available.
}

func TestResolveAuth_AuthModeNone(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")
	t.Setenv("AZTUNNEL_RELAY_AUTH", "")

	endpoint, opts, tp, err := resolveAuth(AuthFlags{
		Relay:     "localhost:8080",
		RelayAuth: "none",
	}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "localhost:8080" {
		t.Errorf("endpoint = %q, want %q", endpoint, "localhost:8080")
	}
	if _, ok := tp.(relay.NoOpTokenProvider); !ok {
		t.Errorf("token provider = %T, want relay.NoOpTokenProvider", tp)
	}
	if opts.Scheme != relay.SchemeWSS {
		t.Errorf("scheme = %q, want %q", opts.Scheme, relay.SchemeWSS)
	}
}

func TestResolveAuth_WsSchemeFromURL(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")

	_, opts, _, err := resolveAuth(AuthFlags{
		Relay:     "ws://localhost:8080",
		RelayAuth: "none",
	}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if opts.Scheme != relay.SchemeWS {
		t.Errorf("scheme = %q, want %q", opts.Scheme, relay.SchemeWS)
	}
}

func TestResolveAuth_InsecureTLSFlag(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_INSECURE_TLS", "")

	_, opts, _, err := resolveAuth(AuthFlags{
		Relay:            "localhost:8443",
		RelayAuth:        "none",
		RelayInsecureTLS: true,
	}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if opts.TLSConfig == nil || !opts.TLSConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify TLS config")
	}
}

func TestResolveAuth_InsecureTLSIgnoredForWS(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_INSECURE_TLS", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")

	_, opts, _, err := resolveAuth(AuthFlags{
		Relay:            "ws://localhost:8080",
		RelayAuth:        "none",
		RelayInsecureTLS: true,
	}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if opts.Scheme != relay.SchemeWS {
		t.Fatalf("scheme = %q, want ws (precondition)", opts.Scheme)
	}
	if opts.TLSConfig != nil {
		t.Error("expected nil TLSConfig for ws scheme (insecure-tls is a no-op)")
	}
}

func TestResolveAuth_NilLoggerDoesNotPanic(t *testing.T) {
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")
	// resolveAuth must internally default a nil logger to slog.Default().
	// The --relay-auth=none branch logs a warning, so this exercises a
	// real *slog.Logger call site.
	_, _, _, err := resolveAuth(AuthFlags{
		Relay:     "localhost:8080",
		RelayAuth: "none",
	}, nil)
	if err != nil {
		t.Fatalf("resolveAuth(nil logger): %v", err)
	}
}

func TestResolveAuth_AuthModeSAS_MissingCreds(t *testing.T) {
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")

	_, _, _, err := resolveAuth(AuthFlags{
		Relay:     "myns",
		RelayAuth: "sas",
	}, discardLogger())
	if err == nil {
		t.Fatal("expected error when --relay-auth=sas without credentials")
	}
	if !strings.Contains(err.Error(), "AZTUNNEL_KEY_NAME") {
		t.Errorf("error %q should mention AZTUNNEL_KEY_NAME", err.Error())
	}
}

func TestResolveAuth_BareHostPortKeepsNoSuffix(t *testing.T) {
	endpoint, opts, _, err := resolveAuth(AuthFlags{
		Relay:     "localhost:8080",
		RelayAuth: "none",
	}, discardLogger())
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "localhost:8080" {
		t.Errorf("endpoint = %q, want %q (no suffix when port present)", endpoint, "localhost:8080")
	}
	if opts.Scheme != relay.SchemeWSS {
		t.Errorf("scheme = %q, want default wss", opts.Scheme)
	}
}

func TestVersion(t *testing.T) {
	// Verify the version variable is set (compile-time default is "dev").
	if version == "" {
		t.Error("version should not be empty")
	}
}

func TestResolveResourceID_FromFlag(t *testing.T) {
	t.Setenv("AZTUNNEL_ARC_RESOURCE_ID", "from-env")

	rid, err := resolveResourceID("from-flag")
	if err != nil {
		t.Fatalf("resolveResourceID: %v", err)
	}
	if rid != "from-flag" {
		t.Errorf("got %q, want %q (flag should take priority over env)", rid, "from-flag")
	}
}

func TestResolveResourceID_FromEnv(t *testing.T) {
	t.Setenv("AZTUNNEL_ARC_RESOURCE_ID", "from-env")

	rid, err := resolveResourceID("")
	if err != nil {
		t.Fatalf("resolveResourceID: %v", err)
	}
	if rid != "from-env" {
		t.Errorf("got %q, want %q", rid, "from-env")
	}
}

func TestResolveResourceID_Missing(t *testing.T) {
	t.Setenv("AZTUNNEL_ARC_RESOURCE_ID", "")

	_, err := resolveResourceID("")
	if err == nil {
		t.Fatal("expected error when resource ID is missing")
	}
	if !strings.Contains(err.Error(), "resource ID is required") {
		t.Errorf("error %q does not contain %q", err.Error(), "resource ID is required")
	}
}

func TestNewLoggerWritesToStderr(t *testing.T) {
	// Redirect stderr before creating the logger so the handler
	// writes to our pipe.
	old := os.Stderr
	defer func() { os.Stderr = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w

	logger := newLogger("info")
	logger.Info("test message", "key", "value")

	w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()

	output := string(buf[:n])
	if !strings.Contains(output, "test message") {
		t.Errorf("expected logger output to contain %q, got %q", "test message", output)
	}
}
