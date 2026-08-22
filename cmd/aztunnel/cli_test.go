package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestVersionFormsIgnoreWriteErrors(t *testing.T) {
	exitCode := -1
	parser := kong.Must(
		&struct{}{},
		kong.Writers(errorWriter{}, errorWriter{}),
		kong.Exit(func(code int) { exitCode = code }),
	)

	if err := (VersionFlag(true)).BeforeApply(parser); err != nil {
		t.Errorf("VersionFlag.BeforeApply() error = %v, want nil", err)
	}
	if exitCode != 0 {
		t.Errorf("VersionFlag.BeforeApply() exit code = %d, want 0", exitCode)
	}
	if err := (&VersionCmd{}).Run(parser); err != nil {
		t.Errorf("VersionCmd.Run() error = %v, want nil", err)
	}
}

func TestCLICommandBindDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		bind func(*CLIConfig) string
		want string
	}{
		{
			name: "relay port-forward remains ephemeral",
			args: []string{"relay-sender", "port-forward", "--hyco", "tunnel", "target:22"},
			bind: func(cli *CLIConfig) string { return cli.RelaySender.PortForward.Bind },
			want: "127.0.0.1:0",
		},
		{
			name: "socks5 uses conventional port",
			args: []string{"relay-sender", "socks5-proxy", "--hyco", "tunnel"},
			bind: func(cli *CLIConfig) string { return cli.RelaySender.Socks5Proxy.Bind },
			want: "127.0.0.1:1080",
		},
		{
			name: "arc port-forward remains ephemeral",
			args: []string{"arc", "port-forward"},
			bind: func(cli *CLIConfig) string { return cli.Arc.PortForward.Bind },
			want: "127.0.0.1:0",
		},
		{
			name: "socks5 explicit ephemeral override",
			args: []string{"relay-sender", "socks5-proxy", "--hyco", "tunnel", "-b", "127.0.0.1:0"},
			bind: func(cli *CLIConfig) string { return cli.RelaySender.Socks5Proxy.Bind },
			want: "127.0.0.1:0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cli CLIConfig
			parser := kong.Must(&cli)
			if _, err := parser.Parse(tt.args); err != nil {
				t.Fatalf("parse %q: %v", tt.args, err)
			}
			if got := tt.bind(&cli); got != tt.want {
				t.Errorf("bind = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCLIHycoRemainsFlagOnly(t *testing.T) {
	var cli CLIConfig
	parser := kong.Must(&cli)
	if _, err := parser.Parse([]string{"relay-sender", "socks5-proxy", "legacy-positional-hyco"}); err == nil {
		t.Fatal("expected positional hybrid connection name to be rejected")
	}
	if cli.RelaySender.Socks5Proxy.Hyco != "" {
		t.Errorf("hyco = %q, want empty without --hyco", cli.RelaySender.Socks5Proxy.Hyco)
	}
}

func TestTopLevelHelpBindDefaults(t *testing.T) {
	portForwardHelp := helpSection(t, topLevelHelp, "Relay Sender - Port Forward:", "Relay Sender - Connect:")
	if !strings.Contains(portForwardHelp, `default "127.0.0.1:0"`) {
		t.Errorf("port-forward help does not retain the ephemeral bind default:\n%s", portForwardHelp)
	}

	socksHelp := helpSection(t, topLevelHelp, "Relay Sender - SOCKS5 Proxy:", "Arc Connect:")
	if !strings.Contains(socksHelp, `default "127.0.0.1:1080"`) {
		t.Errorf("SOCKS5 help does not show the conventional bind default:\n%s", socksHelp)
	}
}

func helpSection(t *testing.T, help, startMarker, endMarker string) string {
	t.Helper()

	start := strings.Index(help, startMarker)
	if start == -1 {
		t.Fatalf("help is missing start marker %q", startMarker)
	}
	sectionStart := start + len(startMarker)
	relativeEnd := strings.Index(help[sectionStart:], endMarker)
	if relativeEnd == -1 {
		t.Fatalf("help is missing end marker %q", endMarker)
	}
	end := sectionStart + relativeEnd
	return help[start:end]
}

func TestResolveBindAddress(t *testing.T) {
	tests := []struct {
		name    string
		bind    string
		gateway bool
		want    string
		wantErr bool
	}{
		{name: "unchanged without gateway", bind: "127.0.0.1:1080", want: "127.0.0.1:1080"},
		{name: "gateway preserves socks5 port", bind: "127.0.0.1:1080", gateway: true, want: "0.0.0.0:1080"},
		{name: "gateway preserves ephemeral port", bind: "127.0.0.1:0", gateway: true, want: "0.0.0.0:0"},
		{name: "gateway defaults empty port to ephemeral", bind: "127.0.0.1:", gateway: true, want: "0.0.0.0:0"},
		{name: "invalid bind", bind: "1080", gateway: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBindAddress(tt.bind, tt.gateway)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBindAddress: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveBindAddress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCLIVersionFormsAndHelpDefaults(t *testing.T) {
	binary := buildAztunnelForTest(t)

	run := func(t *testing.T, args ...string) (string, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // test-controlled binary path and args
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("aztunnel %s: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
		}
		return stdout.String(), stderr.String()
	}

	flagOutput, flagStderr := run(t, "--version")
	commandOutput, commandStderr := run(t, "version")
	if flagOutput != commandOutput {
		t.Errorf("--version output %q differs from version command output %q", flagOutput, commandOutput)
	}
	if flagOutput != version+"\n" {
		t.Errorf("version output = %q, want %q", flagOutput, version+"\n")
	}
	if flagStderr != "" || commandStderr != "" {
		t.Errorf("version forms wrote stderr: --version=%q version=%q", flagStderr, commandStderr)
	}

	helpTests := []struct {
		name string
		args []string
		want string
	}{
		{name: "relay port-forward", args: []string{"relay-sender", "port-forward", "--help"}, want: `--bind="127.0.0.1:0"`},
		{name: "SOCKS5 proxy", args: []string{"relay-sender", "socks5-proxy", "--help"}, want: `--bind="127.0.0.1:1080"`},
		{name: "Arc port-forward", args: []string{"arc", "port-forward", "--help"}, want: `--bind="127.0.0.1:0"`},
	}
	for _, tt := range helpTests {
		t.Run(tt.name+" help", func(t *testing.T) {
			output, _ := run(t, tt.args...)
			if !strings.Contains(output, tt.want) {
				t.Errorf("help does not contain %q:\n%s", tt.want, output)
			}
		})
	}
}
