package scenarios

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRunID(t *testing.T) {
	t.Setenv("PERF_MATRIX_RUN_ID", "pinned-run-7")
	if got := newRunID(); got != "pinned-run-7" {
		t.Fatalf("override: got %q, want pinned-run-7", got)
	}

	t.Setenv("PERF_MATRIX_RUN_ID", "")
	a, b := newRunID(), newRunID()
	if a == b {
		t.Fatalf("two generated ids collided: %q", a)
	}
	for _, id := range []string{a, b} {
		ts, suffix, ok := strings.Cut(id, "-")
		if !ok || len(suffix) != 8 {
			t.Fatalf("id %q is not <ts>-<8hex>", id)
		}
		if !strings.HasSuffix(ts, "Z") || !strings.Contains(ts, "T") {
			t.Fatalf("id %q lacks a sortable UTC timestamp prefix", id)
		}
	}
}

func TestWriteJSONL_StampsRunOnHeaderAndRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "out.jsonl") // also exercises MkdirAll
	rows := []perfMatrixRow{
		{axis: "nn", scenario: "S", mode: "PortForward", coldP50: 1, coldN: 1, successN: 1, attemptN: 1},
	}
	if err := writeJSONL(path, "run-XYZ", rows); err != nil {
		t.Fatalf("writeJSONL: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	var header struct {
		Type string `json:"type"`
		Run  string `json:"run"`
	}
	var rowRun string
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan(); i++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if i == 0 {
			if err := json.Unmarshal([]byte(line), &header); err != nil {
				t.Fatalf("header unmarshal: %v", err)
			}
			continue
		}
		var row struct {
			Type string `json:"type"`
			Run  string `json:"run"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row unmarshal: %v", err)
		}
		rowRun = row.Run
	}
	if header.Type != "run" || header.Run != "run-XYZ" {
		t.Errorf("header = %+v, want type=run run=run-XYZ", header)
	}
	if rowRun != "run-XYZ" {
		t.Errorf("row run = %q, want run-XYZ", rowRun)
	}
}

func TestHistoryDir_HonorsOverride(t *testing.T) {
	t.Setenv("PERF_MATRIX_HISTORY_DIR", "/tmp/custom-hist")
	if got := historyDir(); got != "/tmp/custom-hist" {
		t.Fatalf("got %q, want /tmp/custom-hist", got)
	}
}

func TestHistoryPath_BackendAndRunInName(t *testing.T) {
	t.Setenv("PERF_MATRIX_HISTORY_DIR", "/tmp/h")
	t.Setenv("PERF_MATRIX_BACKEND", "mock")
	if got, want := historyPath("run-9"), filepath.Join("/tmp/h", "mock-run-9.jsonl"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	t.Setenv("PERF_MATRIX_BACKEND", "")
	if got, want := historyPath("run-9"), filepath.Join("/tmp/h", "e2e-run-9.jsonl"); got != want {
		t.Fatalf("unset backend: got %q, want %q", got, want)
	}
}

// recLogf records how finishPerfMatrix reported, so we can pin the
// best-effort-history vs fatal-explicit contract.
type recLogf struct{ logfN, errorfN int }

func (r *recLogf) Logf(string, ...any)   { r.logfN++ }
func (r *recLogf) Errorf(string, ...any) { r.errorfN++ }

// unwritablePath returns a path whose parent is a regular file, so any
// MkdirAll/Open beneath it fails regardless of uid (root bypasses mode
// bits, so 0o500 dirs are not reliable).
func unwritablePath(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f, "sub", "out.jsonl")
}

func TestFinishPerfMatrix_HistoryWriteIsBestEffort(t *testing.T) {
	perfMatrixSink.drain()
	t.Cleanup(func() { perfMatrixSink.drain() })
	t.Setenv("PERF_MATRIX_JSONL", "")
	t.Setenv("PERF_MATRIX_HISTORY_DIR", filepath.Dir(unwritablePath(t))) // unwritable

	perfMatrixSink.add(perfMatrixRow{scenario: "S", mode: "PortForward", coldP50: 1, coldN: 1, successN: 1, attemptN: 1})
	var rec recLogf
	finishPerfMatrix(&rec)
	if rec.errorfN != 0 {
		t.Fatalf("history write failure must not Errorf (would flake functional `make test`); got %d", rec.errorfN)
	}
}

func TestFinishPerfMatrix_ExplicitWriteIsFatal(t *testing.T) {
	perfMatrixSink.drain()
	t.Cleanup(func() { perfMatrixSink.drain() })
	t.Setenv("PERF_MATRIX_JSONL", unwritablePath(t)) // unwritable -> fatal
	t.Setenv("PERF_MATRIX_HISTORY_DIR", t.TempDir()) // writable -> best-effort succeeds

	perfMatrixSink.add(perfMatrixRow{scenario: "S", mode: "PortForward", coldP50: 1, coldN: 1, successN: 1, attemptN: 1})
	var rec recLogf
	finishPerfMatrix(&rec)
	if rec.errorfN != 1 {
		t.Fatalf("explicit PERF_MATRIX_JSONL write failure must Errorf exactly once, got %d", rec.errorfN)
	}
}

// TestPerfMatrixBackend_PrefersEnvOverSink verifies the env var wins
// over a sink-registered backend name when both are set.
func TestPerfMatrixBackend_PrefersEnvOverSink(t *testing.T) {
	perfMatrixSink.setBackendName("mock")
	t.Cleanup(func() { perfMatrixSink.setBackendName("") })
	t.Setenv("PERF_MATRIX_BACKEND", "ci-override")
	if got := perfMatrixBackend(); got != "ci-override" {
		t.Errorf("perfMatrixBackend()=%q with env set, want ci-override", got)
	}
}

// TestPerfMatrixBackend_FallsBackToSink verifies the sink-registered
// backend name is used when the env var is empty — the "just works"
// path for a normal `make e2e-mock` invocation.
func TestPerfMatrixBackend_FallsBackToSink(t *testing.T) {
	perfMatrixSink.setBackendName("mock")
	t.Cleanup(func() { perfMatrixSink.setBackendName("") })
	t.Setenv("PERF_MATRIX_BACKEND", "")
	if got := perfMatrixBackend(); got != "mock" {
		t.Errorf("perfMatrixBackend()=%q with sink-set name, want mock", got)
	}
}

// TestPerfMatrixBackend_EmptyWhenNeitherSet documents the legacy
// behavior — if a caller bypasses RunAllScenarios and doesn't set the
// env, the label stays empty (it can't be inferred from nothing).
func TestPerfMatrixBackend_EmptyWhenNeitherSet(t *testing.T) {
	perfMatrixSink.setBackendName("")
	t.Setenv("PERF_MATRIX_BACKEND", "")
	if got := perfMatrixBackend(); got != "" {
		t.Errorf("perfMatrixBackend()=%q with nothing set, want empty", got)
	}
}

// TestMatrixIdentity_PinsMergedIntoAxes verifies that a row recorded
// from a single-cell run (no axis sub-layers) still carries the pinned
// dimensional identity from setPins. This is the property that makes
// cross-run comparison along a pinned dimension work.
func TestMatrixIdentity_PinsMergedIntoAxes(t *testing.T) {
	perfMatrixSink.setAxisNames(nil)
	perfMatrixSink.setPins(map[string]string{"auth": "sas", "delay": "zero"})
	t.Cleanup(func() {
		perfMatrixSink.setAxisNames(nil)
		perfMatrixSink.setPins(nil)
	})

	axis, axes, scenario, mode := matrixIdentity("TestE2E_Mock/Duplex_Probe_SOCKS5_FanOut")
	if axis != "" {
		t.Errorf("axis=%q with no axis layers, want empty", axis)
	}
	if scenario != "Duplex_Probe_FanOut" || mode != "SOCKS5" {
		t.Errorf("scenario/mode = %q/%q, want Duplex_Probe_FanOut/SOCKS5", scenario, mode)
	}
	if got, want := axes["auth"], "sas"; got != want {
		t.Errorf("axes[auth]=%q, want %q", got, want)
	}
	if got, want := axes["delay"], "zero"; got != want {
		t.Errorf("axes[delay]=%q, want %q", got, want)
	}
}

// TestMatrixIdentity_AxisValuesWinOverPins verifies that when a
// dimension is both pinned (shouldn't happen, but guard) and varied as
// an axis, the per-cell axis value wins so the cell isn't silently
// mislabelled.
func TestMatrixIdentity_AxisValuesWinOverPins(t *testing.T) {
	perfMatrixSink.setAxisNames([]string{"delay"})
	perfMatrixSink.setPins(map[string]string{"delay": "zero", "auth": "sas"})
	t.Cleanup(func() {
		perfMatrixSink.setAxisNames(nil)
		perfMatrixSink.setPins(nil)
	})

	_, axes, _, _ := matrixIdentity("TestE2E_Mock/default/Duplex_Probe_SOCKS5_FanOut")
	if got, want := axes["delay"], "default"; got != want {
		t.Errorf("axes[delay]=%q, want %q (axis value should win over pin)", got, want)
	}
	if got, want := axes["auth"], "sas"; got != want {
		t.Errorf("axes[auth]=%q, want %q (pin should still appear)", got, want)
	}
}
