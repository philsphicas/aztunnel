package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/spf13/cobra"
)

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

// makeAuthCmd creates a cobra.Command with auth flags for testing resolveAuth.
func makeAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "test",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	addAuthFlags(cmd)
	return cmd
}

func TestResolveAuth_NamespaceFromEnv(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "test")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	cmd := makeAuthCmd()
	cmd.SetArgs([]string{})
	_ = cmd.Execute()

	endpoint, tp, err := resolveAuth(cmd)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "sb://test.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "sb://test.servicebus.windows.net")
	}
	if tp == nil {
		t.Fatal("token provider is nil")
	}
}

func TestResolveAuth_SASCredentials(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "myns")
	t.Setenv("AZTUNNEL_KEY_NAME", "RootManageSharedAccessKey")
	t.Setenv("AZTUNNEL_KEY", "dGVzdGtleQ==")

	cmd := makeAuthCmd()
	cmd.SetArgs([]string{})
	_ = cmd.Execute()

	endpoint, tp, err := resolveAuth(cmd)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "sb://myns.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q", endpoint, "sb://myns.servicebus.windows.net")
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

	cmd := makeAuthCmd()
	cmd.SetArgs([]string{})
	_ = cmd.Execute()

	_, _, err := resolveAuth(cmd)
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

	cmd := makeAuthCmd()
	cmd.SetArgs([]string{"--relay", "from-flag"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	endpoint, _, err := resolveAuth(cmd)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if endpoint != "sb://from-flag.servicebus.windows.net" {
		t.Errorf("endpoint = %q, want %q (flag should take priority over env)", endpoint, "sb://from-flag.servicebus.windows.net")
	}
}

func TestResolveAuth_OnlyKeyNameNoKey(t *testing.T) {
	t.Setenv("AZTUNNEL_RELAY_NAME", "test")
	t.Setenv("AZTUNNEL_KEY_NAME", "mykey")
	t.Setenv("AZTUNNEL_KEY", "")

	cmd := makeAuthCmd()
	cmd.SetArgs([]string{})
	_ = cmd.Execute()

	_, tp, err := resolveAuth(cmd)
	// Either it succeeds with Entra or fails because no Azure creds available.
	// Either way, tp should NOT be a SASTokenProvider.
	if err == nil {
		if _, ok := tp.(*relay.SASTokenProvider); ok {
			t.Error("expected non-SAS provider when only KEY_NAME is set (no KEY)")
		}
	}
	// If err != nil, that's expected in CI where no Azure creds are available.
}

func TestVersion(t *testing.T) {
	// Verify the version variable is set (compile-time default is "dev").
	if version == "" {
		t.Error("version should not be empty")
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
