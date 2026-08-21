package scenarios

import (
	"strings"
	"testing"
)

func TestJoinProxyCommandWindows(t *testing.T) {
	got := joinProxyCommand([]string{
		`C:\Program Files\aztunnel.exe`,
		"relay-sender",
		"connect",
		"%h:%p",
	})

	if strings.Contains(got, "'") {
		t.Fatalf("Windows ProxyCommand must not use POSIX single quotes: %q", got)
	}
	if !strings.HasPrefix(got, `"C:\Program Files\aztunnel.exe" `) {
		t.Fatalf("executable path was not Windows-escaped: %q", got)
	}
}

func TestNewShellCommandWindows(t *testing.T) {
	out, err := newShellCommand("echo tunnel-works").CombinedOutput()
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if !strings.Contains(string(out), "tunnel-works") {
		t.Fatalf("unexpected command output: %q", out)
	}
}
