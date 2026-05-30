package scenarios

import (
	"fmt"
	"testing"
	"time"
)

// MuxAxisName is the Axis.Name() used by WithMuxAxis. Exported so
// scenarios that need to inspect or skip cells by mux mode can do so
// without string-duplicating the literal.
const MuxAxisName = "mux"

// MuxAxisValueV1 selects the v1 sender path (mux disabled — every
// connection uses the legacy one-stream-per-rendezvous protocol).
const MuxAxisValueV1 = "v1"

// MuxAxisValueV2 selects the v2 sender path (mux pool enabled —
// streams multiplexed over a small number of long-lived sessions).
// This matches the sender's production default.
const MuxAxisValueV2 = "v2"

// WithMuxAxis returns a Backend wrapper that adds a "mux" axis with
// values ["v1","v2"] as the INNERMOST axis (appended to
// inner.Axes()). For an Azure cell this means the rendered sub-test
// path stays grouped by auth first, mux second, e.g.
// `BenchmarkE2E_Azure/sas/v2/ConnectLatency_Serial_PortForward`,
// which keeps benchstat output paired naturally on benchmarks that
// only care about mux.
//
// On cells where the mux value is "v1", the wrapper sets
// opts.NoMux = true before delegating to the inner backend's Setup
// / SetupExpectingFailure. On "v2" cells the wrapper leaves
// opts.NoMux untouched (so a scenario that intentionally pins NoMux
// stays v1 regardless of which cell it runs in — a no-op when the
// scenario is v2-only).
//
// The wrapper delegates Name() and ConnectLatencyThreshold() to the
// inner Backend so axis values are the only thing added to test
// paths; nothing else about the inner backend's identity changes.
func WithMuxAxis(inner Backend) Backend {
	if inner == nil {
		panic("scenarios.WithMuxAxis: inner Backend must not be nil")
	}
	return &muxAxisBackend{inner: inner}
}

type muxAxisBackend struct {
	inner Backend
	// mode is empty until Cell() is called. The harness contract
	// (Backend.Cell, backend.go) requires Cell() to return a fresh
	// backend pinned to the cell; we honour that by returning a new
	// *muxAxisBackend with mode set, never mutating the receiver.
	mode string
}

func (b *muxAxisBackend) Name() string { return b.inner.Name() }

func (b *muxAxisBackend) ConnectLatencyThreshold() time.Duration {
	return b.inner.ConnectLatencyThreshold()
}

func (b *muxAxisBackend) ColdStartLatencyThreshold() time.Duration {
	return b.inner.ColdStartLatencyThreshold()
}

func (b *muxAxisBackend) Axes() []Axis {
	inner := b.inner.Axes()
	axes := make([]Axis, 0, len(inner)+1)
	axes = append(axes, inner...)
	axes = append(axes, muxAxis{})
	return axes
}

func (b *muxAxisBackend) Cell(values map[string]string) Backend {
	mode, ok := values[MuxAxisName]
	if !ok {
		panic(fmt.Sprintf("scenarios.muxAxisBackend.Cell: missing %q in cell values %v", MuxAxisName, values))
	}
	switch mode {
	case MuxAxisValueV1, MuxAxisValueV2:
	default:
		panic(fmt.Sprintf("scenarios.muxAxisBackend.Cell: unknown %q value %q", MuxAxisName, mode))
	}
	// Strip our axis before delegating; the inner backend's Cell
	// expects only its own axes' keys.
	rest := make(map[string]string, len(values)-1)
	for k, v := range values {
		if k == MuxAxisName {
			continue
		}
		rest[k] = v
	}
	return &muxAxisBackend{inner: b.inner.Cell(rest), mode: mode}
}

func (b *muxAxisBackend) Setup(t testing.TB, opts SetupOptions) *Tunnel {
	t.Helper()
	return b.inner.Setup(t, b.applyMux(opts))
}

func (b *muxAxisBackend) SetupExpectingFailure(t testing.TB, opts SetupOptions) FailureHandle {
	t.Helper()
	return b.inner.SetupExpectingFailure(t, b.applyMux(opts))
}

func (b *muxAxisBackend) applyMux(opts SetupOptions) SetupOptions {
	if b.mode == MuxAxisValueV1 {
		// v1 cell pins the sender into legacy mode. v2 cell leaves
		// opts.NoMux at whatever the scenario set (caller wins) so
		// scenarios that intentionally test v1-only behaviour
		// remain v1 even when WithMuxAxis would otherwise paint
		// them as v2.
		opts.NoMux = true
	}
	return opts
}

// muxAxis is the Axis implementation for the mux-mode dimension.
// Kept unexported because callers construct the axis via the
// WithMuxAxis decorator rather than instantiating the axis directly.
type muxAxis struct{}

func (muxAxis) Name() string     { return MuxAxisName }
func (muxAxis) Values() []string { return []string{MuxAxisValueV1, MuxAxisValueV2} }
