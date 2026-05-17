// Package muxconfig holds shared smux configuration applied at both ends
// of an aztunnel mux session. Keeping these settings in one place ensures
// the sender and listener agree on buffer sizing and keepalive cadence;
// asymmetric settings can cause subtle window-update stalls.
package muxconfig

import (
	"time"

	"github.com/xtaci/smux"
)

const (
	// MaxStreamBuffer caps the per-stream receive window. Sized so a
	// typical Azure Relay path's bandwidth-delay product (RTT in the
	// tens of ms, single-stream throughput in the low MiB/s) fits
	// inside one window cycle; smux's 64 KiB default is far below this
	// BDP and produces SteadyThroughput regressions for any transfer
	// larger than one window. See docs/mux.md for the math.
	MaxStreamBuffer = 1 << 20 // 1 MiB

	// MaxReceiveBuffer caps total in-flight bytes per session across
	// all streams. Sized to absorb a modest fan-out burst (e.g. several
	// large responses landing simultaneously) without unbounded memory
	// growth on the receiver. Worst-case per-session memory ceiling on
	// each side.
	MaxReceiveBuffer = 16 << 20 // 16 MiB

	// KeepAliveInterval is how often smux sends a NOP frame on an idle
	// session. Matches the WS-level bridgePingInterval so an idle mux
	// session and an idle v1 bridge present the same liveness shape to
	// Azure Relay's ~120 s idle-tear-down timer.
	KeepAliveInterval = 30 * time.Second

	// KeepAliveTimeout is the smux read-deadline for any incoming
	// frame; on expiry smux closes the session. Three times the
	// interval gives two missed pings of headroom before tear-down.
	KeepAliveTimeout = 90 * time.Second
)

// SmuxConfig returns a *smux.Config preset with aztunnel's shared
// settings. The protocol version, keepalive cadence, and buffer sizes
// must match on both ends; callers should not mutate those fields after
// calling this. Caller-specific tuning (none today) can be applied to
// the returned config before handing it to smux.Client / smux.Server.
func SmuxConfig() *smux.Config {
	c := smux.DefaultConfig()
	c.Version = 2
	c.KeepAliveDisabled = false
	c.KeepAliveInterval = KeepAliveInterval
	c.KeepAliveTimeout = KeepAliveTimeout
	c.MaxStreamBuffer = MaxStreamBuffer
	c.MaxReceiveBuffer = MaxReceiveBuffer
	return c
}
