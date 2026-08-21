package scenarios

import (
	"errors"
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

func TestSSHCommandResultRequiresSuccessMarker(t *testing.T) {
	t.Run("missing marker after successful exit", func(t *testing.T) {
		output := newMarkerBuffer([]byte("success"))
		_, _ = output.Write([]byte("other output"))

		got, err := sshCommandResult(output, nil)
		if err == nil {
			t.Fatal("sshCommandResult returned nil error without success marker")
		}
		if string(got) != "other output" {
			t.Fatalf("output = %q, want %q", got, "other output")
		}
	})

	t.Run("missing marker preserves process error", func(t *testing.T) {
		output := newMarkerBuffer([]byte("success"))
		processErr := errors.New("process failed")

		_, err := sshCommandResult(output, processErr)
		if !errors.Is(err, processErr) {
			t.Fatalf("error = %v, want wrapped process error", err)
		}
	})

	t.Run("marker overrides forced process termination", func(t *testing.T) {
		output := newMarkerBuffer([]byte("success"))
		_, _ = output.Write([]byte("success"))

		got, err := sshCommandResult(output, errors.New("process killed"))
		if err != nil {
			t.Fatalf("sshCommandResult returned error after marker: %v", err)
		}
		if string(got) != "success" {
			t.Fatalf("output = %q, want %q", got, "success")
		}
	})
}
