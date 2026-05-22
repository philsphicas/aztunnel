package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/philsphicas/aztunnel/internal/relay"
)

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

	endpoint, _, tp, providerName, err := resolveAuth(AuthFlags{})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "test.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "test.servicebus.windows.net")
	}
	if tp == nil {
		t.Fatal("token provider is nil")
	}
	if providerName != relay.ProviderSAS {
		t.Errorf("providerName = %q, want %q", providerName, relay.ProviderSAS)
	}
}

func TestResolveAuth_SASCredentials(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "myns")
	t.Setenv("AZTUNNEL_KEY_NAME", "RootManageSharedAccessKey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	endpoint, _, tp, providerName, err := resolveAuth(AuthFlags{})
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
	if providerName != relay.ProviderSAS {
		t.Errorf("providerName = %q, want %q", providerName, relay.ProviderSAS)
	}
}

func TestResolveAuth_MissingNamespace(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "")
	t.Setenv("AZTUNNEL_KEY", "")

	_, _, _, _, err := resolveAuth(AuthFlags{})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{Relay: "from-flag"})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{Relay: "sb://my-relay.servicebus.windows.net"})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{RelaySuffix: ".servicebus.chinacloudapi.cn"})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{Relay: "my-relay.servicebus.chinacloudapi.cn", RelaySuffix: ".should-be-ignored"})
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

	endpoint, _, _, _, err := resolveAuth(AuthFlags{RelaySuffix: ".servicebus.usgovcloudapi.net"})
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

	_, _, _, _, err := resolveAuth(AuthFlags{Relay: "sb://"})
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

	_, _, tp, providerName, err := resolveAuth(AuthFlags{})
	// Either it succeeds with Entra or fails because no Azure creds available.
	// Either way, tp should NOT be a SASTokenProvider.
	if err == nil {
		if _, ok := tp.(*relay.SASTokenProvider); ok {
			t.Error("expected non-SAS provider when only KEY_NAME is set (no KEY)")
		}
		if providerName != relay.ProviderEntra {
			t.Errorf("providerName = %q, want %q", providerName, relay.ProviderEntra)
		}
	}
	// If err != nil, that's expected in CI where no Azure creds are available.
}

func TestResolveAuth_InsecureTLSFlag(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_INSECURE_TLS", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "k")
	t.Setenv("AZTUNNEL_KEY", "v")

	_, opts, _, _, err := resolveAuth(AuthFlags{
		Relay:            "wss://localhost:8443",
		RelayInsecureTLS: true,
	})
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if opts.TLSConfig == nil || !opts.TLSConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify TLS config")
	}
}

func TestResolveAuth_RejectsPlainSchemes(t *testing.T) {
	// aztunnel only dials TLS-protected relays, so any --relay value
	// using ws://, http://, or another non-TLS scheme is rejected at
	// parse time (not silently downgraded to wss).
	t.Setenv("AZTUNNEL_RELAY_NAME", "")
	t.Setenv("AZTUNNEL_KEY_NAME", "k")
	t.Setenv("AZTUNNEL_KEY", "v")

	for _, tc := range []struct {
		name  string
		relay string
	}{
		{"ws scheme", "ws://localhost:8080"},
		{"http scheme", "http://localhost:8080"},
		{"ftp scheme", "ftp://relay.example.com:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, err := resolveAuth(AuthFlags{Relay: tc.relay})
			if err == nil {
				t.Fatalf("expected error for relay %q, got nil", tc.relay)
			}
			if !strings.Contains(err.Error(), "invalid relay endpoint") {
				t.Errorf("error %q should mention 'invalid relay endpoint'", err.Error())
			}
		})
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

// TestCLI_MissingRequiredArgs verifies the aztunnel CLI emits a
// clean error (not a usage dump) when a required argument is
// missing. Runs the freshly-built binary as a subprocess and
// captures stderr; the assertion is on the error shape, not on the
// exact message wording. Pure CLI parsing path — no network.
func TestCLI_MissingRequiredArgs(t *testing.T) {
	binary := buildAztunnelForTest(t)

	// Strip AZTUNNEL_* from env so resolveAuth doesn't pick up
	// values from a developer shell.
	var cleanEnv []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AZTUNNEL_") {
			cleanEnv = append(cleanEnv, e)
		}
	}

	runClean := func(t *testing.T, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // test-controlled binary path
		cmd.Env = cleanEnv
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdout = io.Discard
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected non-zero exit")
		}
		return stderr.String()
	}

	t.Run("listener_no_relay", func(t *testing.T) {
		output := runClean(t, "relay-listener", "--hyco", "some-hyco")
		if !strings.Contains(output, "relay") {
			t.Errorf("expected error mentioning 'relay', got: %s", output)
		}
	})

	t.Run("sender_no_relay", func(t *testing.T) {
		output := runClean(t, "relay-sender", "port-forward", "127.0.0.1:22")
		if !strings.Contains(output, "relay") && !strings.Contains(output, "hyco") {
			t.Errorf("expected error mentioning 'relay' or 'hyco', got: %s", output)
		}
	})

	t.Run("sender_no_target", func(t *testing.T) {
		output := runClean(t, "relay-sender", "connect")
		if !strings.Contains(output, "arg") && !strings.Contains(output, "expected") {
			t.Errorf("expected error about missing argument, got: %s", output)
		}
	})
}

// TestCLI_BadRelayName verifies the aztunnel CLI exits cleanly with
// a recognisable error when given a relay name that doesn't
// resolve. Uses a TLD reserved by RFC 2606 (.invalid) so DNS lookup
// fails NXDOMAIN — no real network round-trip, no Azure setup.
func TestCLI_BadRelayName(t *testing.T) {
	binary := buildAztunnelForTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, //nolint:gosec // test-controlled binary path
		"relay-listener",
		"--relay", "nonexistent.relay.invalid",
		"--hyco", "some-hyco",
		"--log-level", "debug",
	)
	cmd.Env = append(os.Environ(),
		"AZTUNNEL_KEY_NAME=test-key-name",
		// Valid base64 placeholder so SAS code path doesn't reject
		// at parse time; the relay-name DNS failure is the
		// observable error.
		"AZTUNNEL_KEY=dGVzdGtleQ==",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected aztunnel to exit non-zero on bad relay name")
	}
	logs := stderr.String()
	// The listener subprocess logs "control channel disconnected"
	// after the dial fails; that's the operator-visible signal
	// for a bad-relay-name failure. We also tolerate "control
	// channel" error log lines that contain a DNS resolution
	// failure (lookup *.invalid: no such host).
	if !strings.Contains(logs, "control channel") && !strings.Contains(logs, "no such host") &&
		!strings.Contains(logs, "lookup") {
		t.Errorf("expected control-channel or DNS error in stderr; got:\n%s", logs)
	}
	// Redaction contract: no raw sb-hc-token in stderr.
	stripped := strings.ReplaceAll(logs, "sb-hc-token=REDACTED", "")
	if strings.Contains(stripped, "sb-hc-token=") {
		t.Error("stderr contains non-redacted sb-hc-token")
	}
}

// buildAztunnelForTest builds cmd/aztunnel into a temp directory
// and returns the binary path. Per-test build keeps the
// no-e2e-tag main_test.go independent of the e2e helpers' shared
// build cache.
func buildAztunnelForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "aztunnel"
	if runtime.GOOS == "windows" {
		name = "aztunnel.exe"
	}
	binary := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", binary, ".") //nolint:gosec // test-controlled args
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build aztunnel: %v\n%s", err, out)
	}
	return binary
}
