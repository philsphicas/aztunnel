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
// values ["v1","v2"] as the INNERMOST axis (appended to inner.Axes()).
// Sub-test paths read as `.../<auth>/<delay>/<mux>/<scenario>`, so the
// PERF MATRIX history file groups v1/v2 rows together within each cell.
//
// On every Cell call the wrapper pins opts.SenderMaxProtocolVersion to
// the cell's mux value (1 for v1, 2 for v2) before delegating Setup /
// SetupExpectingFailure to the inner backend. A scenario that has
// already populated opts.SenderMaxProtocolVersion before calling
// b.Setup(t, opts) wins — the explicit pin overrides the cell. That
// matters for scenarios whose contract depends on a specific protocol
// version regardless of the surrounding cell (e.g. v1-only topology
// distribution scenarios that exercise per-rendezvous semantics mux
// dissolves).
//
// The wrapper delegates Name(), latency thresholds, and policy to the
// inner Backend so the axis adds nothing to the backend's identity
// beyond the extra sub-test layer.
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

func (b *muxAxisBackend) WarmRequestBudget() time.Duration {
	return b.inner.WarmRequestBudget()
}

func (b *muxAxisBackend) ConnectLatencyPolicy() ConnectLatencyPolicy {
	return b.inner.ConnectLatencyPolicy()
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
	// A scenario that pre-populates SenderMaxProtocolVersion wins.
	// This protects v1-only and v2-only scenarios from being silently
	// flipped by the surrounding mux-axis cell.
	if opts.SenderMaxProtocolVersion != 0 {
		return opts
	}
	switch b.mode {
	case MuxAxisValueV1:
		opts.SenderMaxProtocolVersion = 1
	case MuxAxisValueV2:
		opts.SenderMaxProtocolVersion = 2
	}
	return opts
}

// muxAxis is the Axis implementation for the mux-mode dimension.
// Kept unexported because callers construct the axis via the
// WithMuxAxis decorator rather than instantiating the axis directly.
type muxAxis struct{}

func (muxAxis) Name() string     { return MuxAxisName }
func (muxAxis) Values() []string { return []string{MuxAxisValueV1, MuxAxisValueV2} }
