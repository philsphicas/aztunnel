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
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
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
