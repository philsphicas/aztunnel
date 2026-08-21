//go:build !windows

package scenarios

import (
	"context"
	"os/exec"
	"strings"
)

func joinProxyCommand(argv []string) string {
	var sb strings.Builder
	for i, arg := range argv {
		if i > 0 {
			sb.WriteByte(' ')
		}
		if strings.ContainsAny(arg, " \t\"\\$`") || strings.Contains(arg, "'") {
			sb.WriteByte('\'')
			sb.WriteString(strings.ReplaceAll(arg, "'", `'\''`))
			sb.WriteByte('\'')
		} else {
			sb.WriteString(arg)
		}
	}
	return sb.String()
}

func newShellCommand(command string) *exec.Cmd {
	return exec.Command("sh", "-c", command) //nolint:gosec // test-controlled SSH command
}

func runSSHCommand(ctx context.Context, args, env []string, _ []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // test-controlled args
	cmd.Env = env
	return cmd.CombinedOutput()
}
