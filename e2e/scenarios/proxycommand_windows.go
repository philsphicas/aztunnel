package scenarios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

func joinProxyCommand(argv []string) string {
	escaped := make([]string, len(argv))
	for i, arg := range argv {
		escaped[i] = syscall.EscapeArg(arg)
	}
	return strings.Join(escaped, " ")
}

func newShellCommand(command string) *exec.Cmd {
	return exec.Command("cmd.exe", "/d", "/s", "/c", command) //nolint:gosec // test-controlled SSH command
}

func runSSHCommand(ctx context.Context, args, env []string, successMarker []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // test-controlled args
	cmd.Env = env

	output := newMarkerBuffer(successMarker)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return output.Bytes(), err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return sshCommandResult(output, err)
	case <-output.found:
		_ = cmd.Process.Kill()
		<-done
		return output.Bytes(), nil
	case <-ctx.Done():
		err := <-done
		return sshCommandResult(output, errors.Join(ctx.Err(), err))
	}
}

func sshCommandResult(output *markerBuffer, err error) ([]byte, error) {
	data := output.Bytes()
	if output.Found() {
		return data, nil
	}
	if err != nil {
		return data, fmt.Errorf("ssh exited before success marker %q: %w", output.marker, err)
	}
	return data, fmt.Errorf("ssh exited before success marker %q", output.marker)
}

type markerBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker []byte
	found  chan struct{}
	once   sync.Once
}

func newMarkerBuffer(marker []byte) *markerBuffer {
	return &markerBuffer{
		marker: marker,
		found:  make(chan struct{}),
	}
}

func (b *markerBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if bytes.Contains(b.buf.Bytes(), b.marker) {
		b.once.Do(func() { close(b.found) })
	}
	return n, err
}

func (b *markerBuffer) Found() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Contains(b.buf.Bytes(), b.marker)
}

func (b *markerBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}
