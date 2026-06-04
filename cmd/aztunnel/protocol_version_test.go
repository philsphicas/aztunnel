package main

import (
	"strconv"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/philsphicas/aztunnel/internal/listener"
	"github.com/philsphicas/aztunnel/internal/sender"
)

// TestKongDefault_MaxProtocolVersion_TracksConstants asserts that the
// kong-parsed default for --max-protocol-version on both sender and
// listener subcommands matches the in-tree DefaultSenderMaxProtocolVersion
// / DefaultListenerMaxProtocolVersion constants.
//
// This is the pin that makes the 0.5.0 sender-default flip a single-line
// change: bump DefaultSenderMaxProtocolVersion and every code path
// (sender library normalization, the CLI default surfaced by kong's
// ${defaultSenderMaxProtocolVersion} substitution, this test) updates
// in lockstep. Without this pin, kong's hardcoded `default:"1"` tag
// would silently lag the constant.
func TestKongDefault_MaxProtocolVersion_TracksConstants(t *testing.T) {
	// CLI is a package-level var, so each parse builds its own struct.
	var c struct {
		Globals
		RelayListener RelayListenerCmd `cmd:"" name:"relay-listener"`
		RelaySender   RelaySenderCmd   `cmd:"" name:"relay-sender"`
	}

	vars := kong.Vars{
		"defaultSenderMaxProtocolVersion":   strconv.Itoa(sender.DefaultSenderMaxProtocolVersion),
		"defaultListenerMaxProtocolVersion": strconv.Itoa(listener.DefaultListenerMaxProtocolVersion),
	}

	parser, err := kong.New(&c, vars)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	// Drive the sender port-forward path with the minimum args kong
	// needs to satisfy required positional/flag fields;
	// --max-protocol-version is omitted so kong fills it from the
	// substituted default. The test only inspects the parsed value,
	// so we never call Run() and no auth is needed.
	if _, err := parser.Parse([]string{
		"relay-sender", "port-forward", "10.0.0.1:22",
		"--relay", "test-namespace",
		"--hyco", "test-hyco",
	}); err != nil {
		t.Fatalf("kong.Parse (sender): %v", err)
	}
	if got, want := c.RelaySender.PortForward.MaxProtocolVersion, sender.DefaultSenderMaxProtocolVersion; got != want {
		t.Errorf("kong-parsed sender --max-protocol-version default = %d, want %d (sender.DefaultSenderMaxProtocolVersion). Bumping the constant must propagate via kong.Vars; see cmd/aztunnel/main.go.",
			got, want)
	}

	// Same drill for the listener subcommand. Build a fresh struct +
	// parser; kong stores parsed values on the receiver.
	c.RelayListener = RelayListenerCmd{}
	parser, err = kong.New(&c, vars)
	if err != nil {
		t.Fatalf("kong.New (listener): %v", err)
	}
	if _, err := parser.Parse([]string{
		"relay-listener",
		"--relay", "test-namespace",
		"--hyco", "test-hyco",
	}); err != nil {
		t.Fatalf("kong.Parse (listener): %v", err)
	}
	if got, want := c.RelayListener.MaxProtocolVersion, listener.DefaultListenerMaxProtocolVersion; got != want {
		t.Errorf("kong-parsed listener --max-protocol-version default = %d, want %d (listener.DefaultListenerMaxProtocolVersion). Bumping the constant must propagate via kong.Vars; see cmd/aztunnel/main.go.",
			got, want)
	}
}
