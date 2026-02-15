package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func arcCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arc",
		Short: "Connect through Azure Arc managed relays",
		Long: `The arc command group connects to Azure Arc-enrolled machines through
the Azure Relay hybrid connection that Azure provisions automatically
when the OpenSSH extension is installed.

Unlike relay-sender (which requires you to provision your own Azure Relay
and run an aztunnel listener), arc mode works with any Arc-connected
machine that has the HybridConnectivity endpoint enabled.`,
	}

	cmd.PersistentFlags().String("resource-id", "", "ARM resource ID of the Arc-connected machine")
	cmd.PersistentFlags().Int("port", 22, "remote port the service listens on")
	cmd.PersistentFlags().String("service", "SSH", "service name (SSH or WAC)")

	cmd.AddCommand(arcConnectCmd())
	cmd.AddCommand(arcPortForwardCmd())

	return cmd
}

// resolveResourceID returns the resource ID from --resource-id flag or
// AZTUNNEL_ARC_RESOURCE_ID env var.
func resolveResourceID(cmd *cobra.Command) (string, error) {
	rid, _ := cmd.Flags().GetString("resource-id")
	if rid != "" {
		return rid, nil
	}
	if rid := os.Getenv("AZTUNNEL_ARC_RESOURCE_ID"); rid != "" {
		return rid, nil
	}
	return "", fmt.Errorf("resource ID is required: use --resource-id or set AZTUNNEL_ARC_RESOURCE_ID")
}

// arcStdioConn adapts stdin/stdout to net.Conn for use with relay.Bridge.
type arcStdioConn struct {
	in  io.ReadCloser
	out io.WriteCloser
}

func (c *arcStdioConn) Read(b []byte) (int, error)       { return c.in.Read(b) }
func (c *arcStdioConn) Write(b []byte) (int, error)      { return c.out.Write(b) }
func (c *arcStdioConn) Close() error                     { return errors.Join(c.in.Close(), c.out.Close()) }
func (c *arcStdioConn) LocalAddr() net.Addr              { return arcStubAddr{} }
func (c *arcStdioConn) RemoteAddr() net.Addr             { return arcStubAddr{} }
func (c *arcStdioConn) SetDeadline(time.Time) error      { return nil }
func (c *arcStdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *arcStdioConn) SetWriteDeadline(time.Time) error { return nil }

type arcStubAddr struct{}

func (arcStubAddr) Network() string { return "stdio" }
func (arcStubAddr) String() string  { return "stdio" }
