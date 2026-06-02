package scenarios

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// perfMatrixRow is one rendered line in the human-readable performance
// matrix: a single scenario's representative client-side timings,
// distilled from the same data that produces its `workload-summary`
// log line. The matrix exists so a human can scan placement / shape /
// mode trade-offs at a glance instead of parsing key=val log soup.
type perfMatrixRow struct {
	axis     string            // axis-cell path, e.g. "sas" or "entra/far" (flat; for the human table)
	axes     map[string]string // named axis dimensions, e.g. {"auth":"entra","delay":"far"}
	scenario string            // leaf scenario label, e.g. "Parallel_ConnReusedEcho"
	mode     string            // PortForward | SOCKS5 | Connect | -
	family   string            // metric family: "" (== rtt) or "stream"
	coldP50  time.Duration
	warmP50  time.Duration
	warmP95  time.Duration
	coldN    int
	warmN    int
	successN int
	attemptN int
	wall     time.Duration

	// Streaming family (family == "stream"). Zero on rtt rows.
	ttfbP50            time.Duration
	ttfbP95            time.Duration
	gapP95             time.Duration
	maxStreamGapP95    time.Duration
	maxGap             time.Duration
	finalChunkSpread   time.Duration
	completionSpread   time.Duration
	goodputBytesPerSec int64
}

// perfMatrix collects rows across all scenarios in a run and renders
// them once at the end. Guarded by a mutex because scenarios (and the
// conns within them) can record concurrently.
type perfMatrix struct {
	mu        sync.Mutex
	rows      []perfMatrixRow
	axisNames []string // ordered axis names for this run (from Backend.Axes()), used to label path segments
}

var perfMatrixSink perfMatrix

func (m *perfMatrix) add(row perfMatrixRow) {
	m.mu.Lock()
	m.rows = append(m.rows, row)
	m.mu.Unlock()
}

// setAxisNames records the ordered axis names the run is fanning over so
// recorded rows can label their path segments (segment i is axisNames[i]).
// Called once before any cell runs; rows read it concurrently afterward.
func (m *perfMatrix) setAxisNames(names []string) {
	m.mu.Lock()
	m.axisNames = append([]string(nil), names...)
	m.mu.Unlock()
}

func (m *perfMatrix) snapshotAxisNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.axisNames...)
}

// drain returns the recorded rows in a stable sort order and clears the
// sink so a subsequent run in the same process starts fresh. The single
// snapshot under the mutex is shared by every consumer (table render and
// JSONL emission) so they never disagree about what ran.
func (m *perfMatrix) drain() []perfMatrixRow {
	m.mu.Lock()
	rows := m.rows
	m.rows = nil
	m.mu.Unlock()

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].axis != rows[j].axis {
			return rows[i].axis < rows[j].axis
		}
		if rows[i].scenario != rows[j].scenario {
			return rows[i].scenario < rows[j].scenario
		}
		return rows[i].mode < rows[j].mode
	})
	return rows
}

// renderTable formats the rtt-family rows as the aligned human-readable
// matrix. Streaming rows are rendered separately by renderStreamTable.
// Returns "" if there are no rtt rows.
func renderTable(rows []perfMatrixRow) string {
	var rtt []perfMatrixRow
	for _, r := range rows {
		if r.family != streamFamily {
			rtt = append(rtt, r)
		}
	}
	if len(rtt) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nPERF MATRIX (client-side RTT; est = cold_p50 − warm_p50 ≈ establishment cost)\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "axis\tscenario\tmode\tcold_p50\twarm_p50\twarm_p95\test\tcold_n\twarm_n\tsuccess\twall")
	for _, r := range rtt {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d/%d\t%s\n",
			dash(r.axis), r.scenario, dash(r.mode),
			durOrDash(r.coldP50, r.coldN),
			durOrDash(r.warmP50, r.warmN),
			durOrDash(r.warmP95, r.warmN),
			estCol(r),
			r.coldN, r.warmN, r.successN, r.attemptN, round1(r.wall),
		)
	}
	_ = tw.Flush()
	b.WriteString("END PERF MATRIX\n")
	return b.String()
}

// renderStreamTable formats the streaming-family rows as their own
// aligned matrix (start latency, jitter, fairness, goodput). Returns ""
// if there are no streaming rows.
func renderStreamTable(rows []perfMatrixRow) string {
	var stream []perfMatrixRow
	for _, r := range rows {
		if r.family == streamFamily {
			stream = append(stream, r)
		}
	}
	if len(stream) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nPERF MATRIX (streaming; ttfb = first-chunk latency, spread = max−min across streams)\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "axis\tscenario\tmode\tttfb_p50\tttfb_p95\tgap_p95\tmax_gap\tfinal_spread\tgoodput_KiB/s\tsuccess\twall")
	for _, r := range stream {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n",
			dash(r.axis), r.scenario, dash(r.mode),
			durOrDash(r.ttfbP50, r.successN),
			durOrDash(r.ttfbP95, r.successN),
			durOrDash(r.gapP95, r.successN),
			durOrDash(r.maxGap, r.successN),
			durOrDash(r.finalChunkSpread, r.successN),
			goodputCol(r),
			r.successN, r.attemptN, round1(r.wall),
		)
	}
	_ = tw.Flush()
	b.WriteString("END PERF MATRIX\n")
	return b.String()
}

// goodputCol renders a streaming row's goodput in KiB/s, or "-" when no
// stream in the round succeeded.
func goodputCol(r perfMatrixRow) string {
	if r.successN == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(r.goodputBytesPerSec)/1024)
}

func estCol(r perfMatrixRow) string {
	if r.coldN == 0 || r.warmN == 0 {
		return "-"
	}
	return round1(r.coldP50 - r.warmP50).String()
}

func durOrDash(d time.Duration, n int) string {
	if n == 0 {
		return "-"
	}
	return round1(d).String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// round1 trims sub-millisecond noise so the table reads cleanly.
func round1(d time.Duration) time.Duration {
	if d >= time.Millisecond {
		return d.Round(100 * time.Microsecond)
	}
	return d.Round(time.Microsecond)
}

// matrixIdentity derives the axis path, named axes, scenario, and mode
// columns from a sub-test path (t.Name()), shared by the rtt and stream
// recorders so both families label cells identically.
func matrixIdentity(scenarioPath string) (axis string, axes map[string]string, scenario, mode string) {
	names := perfMatrixSink.snapshotAxisNames()
	axisVals, leaf := splitScenarioPath(scenarioPath, len(names))
	scenario, mode = splitMode(leaf)
	axis = strings.Join(axisVals, "/")
	if len(axisVals) > 0 {
		axes = make(map[string]string, len(axisVals))
		for i, v := range axisVals {
			key := fmt.Sprintf("axis%d", i)
			if i < len(names) && names[i] != "" {
				key = names[i]
			}
			axes[key] = v
		}
	}
	return axis, axes, scenario, mode
}

// recordPerfMatrixRow distils a finished round into one matrix row.
// scenarioPath is t.Name(); coldRTT/warmAll are the same samples the
// workload-summary line aggregates; successN/attemptN are the connection
// success counts (rendered "n/m").
func recordPerfMatrixRow(scenarioPath string, coldRTT, warmAll []time.Duration, successN, attemptN int, wall time.Duration) {
	axis, axes, scenario, mode := matrixIdentity(scenarioPath)
	perfMatrixSink.add(perfMatrixRow{
		axis:     axis,
		axes:     axes,
		scenario: scenario,
		mode:     mode,
		coldP50:  repr(coldRTT, 0.50),
		warmP50:  repr(warmAll, 0.50),
		warmP95:  repr(warmAll, 0.95),
		coldN:    len(coldRTT),
		warmN:    len(warmAll),
		successN: successN,
		attemptN: attemptN,
		wall:     wall,
	})
}

// recordStreamMatrixRow distils a finished stream round into one
// streaming-family matrix row. The streaming metrics live in their own
// columns; the rtt cold/warm fields stay zero (and serialize null).
func recordStreamMatrixRow(scenarioPath string, m streamMetrics) {
	axis, axes, scenario, mode := matrixIdentity(scenarioPath)
	perfMatrixSink.add(perfMatrixRow{
		axis:               axis,
		axes:               axes,
		scenario:           scenario,
		mode:               mode,
		family:             streamFamily,
		successN:           m.successN,
		attemptN:           m.streamN,
		wall:               m.wall,
		ttfbP50:            m.ttfbP50,
		ttfbP95:            m.ttfbP95,
		gapP95:             m.gapP95,
		maxStreamGapP95:    m.maxStreamGapP95,
		maxGap:             m.maxGap,
		finalChunkSpread:   m.finalChunkSpread,
		completionSpread:   m.completionSpread,
		goodputBytesPerSec: m.goodputBytesPerSec,
	})
}

// perfMatrixSchema is the artifact schema tag carried by every emitted
// record. Bump it on any breaking field change so consumers can branch.
const perfMatrixSchema = "perfmatrix/v1"

// perfMatrixRecord is the JSON Lines shape of one matrix row — the
// decoupling boundary between test execution (which emits) and reporting
// (which renders). It carries only raw measurements; derived columns
// such as est are recomputed by the reporter, never stored. Duration
// fields are integer nanoseconds and are pointers so an unmeasured
// metric (cold_n==0 or warm_n==0) serializes as null rather than a
// misleading zero. Keep this in sync with the reader-side struct in
// cmd/perfreport, which owns its own copy on purpose: the artifact, not
// a shared Go type, is the contract.
type perfMatrixRecord struct {
	Type     string            `json:"type"` // always "row"; a "run" meta record (see perfMatrixRunRecord) precedes the rows
	Schema   string            `json:"schema"`
	Run      string            `json:"run,omitempty"`     // run id: groups all rows emitted by one test invocation (see newRunID)
	Backend  string            `json:"backend,omitempty"` // mock | azure — set via PERF_MATRIX_BACKEND so a merged file is per-backend distinguishable
	Axis     string            `json:"axis"`
	Axes     map[string]string `json:"axes,omitempty"` // named axis dimensions, e.g. {"auth":"sas","delay":"nn"}
	Scenario string            `json:"scenario"`
	Mode     string            `json:"mode,omitempty"`
	// MetricFamily discriminates the metric family this row carries.
	// Omitted (empty) means the legacy RTT family, so v1 artifacts emitted
	// before streaming existed parse identically; "stream" rows carry the
	// streaming columns below and leave the RTT columns null.
	MetricFamily string `json:"metric_family,omitempty"`
	ColdP50Ns    *int64 `json:"cold_p50_ns"`
	WarmP50Ns    *int64 `json:"warm_p50_ns"`
	WarmP95Ns    *int64 `json:"warm_p95_ns"`
	ColdN        int    `json:"cold_n"`
	WarmN        int    `json:"warm_n"`
	SuccessN     int    `json:"success_n"`
	AttemptN     int    `json:"attempt_n"`
	WallNs       int64  `json:"wall_ns"`

	// Streaming family (metric_family == "stream"). Duration fields are
	// nullable pointers (null on rtt rows and when no stream succeeded);
	// GoodputBytesPerSec is application payload bytes per second.
	TTFBP50Ns          *int64 `json:"ttfb_p50_ns,omitempty"`
	TTFBP95Ns          *int64 `json:"ttfb_p95_ns,omitempty"`
	GapP95Ns           *int64 `json:"gap_p95_ns,omitempty"`
	MaxStreamGapP95Ns  *int64 `json:"max_stream_gap_p95_ns,omitempty"`
	MaxGapNs           *int64 `json:"max_gap_ns,omitempty"`
	FinalChunkSpreadNs *int64 `json:"final_chunk_spread_ns,omitempty"`
	CompletionSpreadNs *int64 `json:"completion_spread_ns,omitempty"`
	GoodputBytesPerSec *int64 `json:"goodput_bytes_per_sec,omitempty"`
	StreamN            int    `json:"stream_n,omitempty"`
}

// perfMatrixRunRecord is the leading "run" meta record that makes an
// archived artifact self-describing: when a CI-stored file is read in
// isolation, these fields answer "which build, which backend config,
// when?" without the surrounding test log. Consumers that only care
// about measurements skip every record whose type != "row". git_sha is
// supplied by the orchestrator via PERF_MATRIX_GIT_SHA (the test process
// does not shell out to git); auth/delay come from the same env the
// harness already reads.
type perfMatrixRunRecord struct {
	Type        string `json:"type"` // always "run"
	Schema      string `json:"schema"`
	Run         string `json:"run"` // run id stamped on the header and every row of this invocation
	Backend     string `json:"backend,omitempty"`
	GeneratedAt string `json:"generated_at"`
	GitSHA      string `json:"git_sha,omitempty"`
	E2EAuth     string `json:"e2e_auth,omitempty"`
	E2EDelay    string `json:"e2e_delay,omitempty"`
}

// perfMatrixBackend names the backend that produced these rows (mock or
// azure), taken from PERF_MATRIX_BACKEND. The harness itself is
// backend-agnostic — the entrypoint's Makefile target sets this — so a
// merged artifact can distinguish, and the reporter can group by, rows
// from different backends.
func perfMatrixBackend() string { return os.Getenv("PERF_MATRIX_BACKEND") }

func newRunRecord(runID string) perfMatrixRunRecord {
	return perfMatrixRunRecord{
		Type:        "run",
		Schema:      perfMatrixSchema,
		Run:         runID,
		Backend:     perfMatrixBackend(),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		GitSHA:      os.Getenv("PERF_MATRIX_GIT_SHA"),
		E2EAuth:     os.Getenv("E2E_AUTH"),
		E2EDelay:    os.Getenv("E2E_DELAY"),
	}
}

// newRunID returns a collision-resistant, lexically-sortable run id:
// PERF_MATRIX_RUN_ID overrides (so a sharded run can share one id and
// merge its rows), else a UTC millisecond timestamp plus an 8-hex random
// suffix. The fixed-width timestamp prefix makes plain string compare a
// valid newest-first ordering; the random suffix prevents same-ms
// collisions across processes. Falls back to the bare timestamp if the
// system RNG is unavailable.
func newRunID() string {
	if v := os.Getenv("PERF_MATRIX_RUN_ID"); v != "" {
		return v
	}
	ts := time.Now().UTC().Format("20060102T150405.000Z")
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ts
	}
	return ts + "-" + hex.EncodeToString(b[:])
}

// historyDir is where always-on per-run history files accumulate:
// PERF_MATRIX_HISTORY_DIR overrides, else <go.work root>/e2e/perf-artifacts/history
// (so every module's tests land in one shared place), else a cwd-relative
// fallback when the workspace root can't be located.
func historyDir() string {
	if v := os.Getenv("PERF_MATRIX_HISTORY_DIR"); v != "" {
		return v
	}
	if root := workspaceRoot(); root != "" {
		return filepath.Join(root, "e2e", "perf-artifacts", "history")
	}
	return filepath.Join("perf-artifacts", "history")
}

// workspaceRoot walks up from the cwd to the directory containing go.work,
// returning "" if none is found.
func workspaceRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// historyPath names a run's history file <backend-or-"e2e">-<runID>.jsonl
// under historyDir, so a directory listing is grouped by backend and
// ordered by run id.
func historyPath(runID string) string {
	backend := perfMatrixBackend()
	if backend == "" {
		backend = "e2e"
	}
	return filepath.Join(historyDir(), backend+"-"+runID+".jsonl")
}

// nsIf returns a pointer to d's nanoseconds when n>0, else nil — so an
// unmeasured metric serializes as JSON null instead of a bogus zero.
func nsIf(d time.Duration, n int) *int64 {
	if n == 0 {
		return nil
	}
	v := d.Nanoseconds()
	return &v
}

// streamFamily is the metric_family value carried by streaming rows; the
// rtt family is the empty string (so legacy v1 artifacts are unchanged).
const streamFamily = "stream"

// i64If returns a pointer to v when n>0, else nil — the non-duration
// counterpart of nsIf, used for the goodput counter so an all-failed
// stream round serializes goodput as null rather than a bogus zero.
func i64If(v int64, n int) *int64 {
	if n == 0 {
		return nil
	}
	return &v
}

func (r perfMatrixRow) record() perfMatrixRecord {
	rec := perfMatrixRecord{
		Type:      "row",
		Schema:    perfMatrixSchema,
		Backend:   perfMatrixBackend(),
		Axis:      r.axis,
		Axes:      r.axes,
		Scenario:  r.scenario,
		Mode:      r.mode,
		ColdP50Ns: nsIf(r.coldP50, r.coldN),
		WarmP50Ns: nsIf(r.warmP50, r.warmN),
		WarmP95Ns: nsIf(r.warmP95, r.warmN),
		ColdN:     r.coldN,
		WarmN:     r.warmN,
		SuccessN:  r.successN,
		AttemptN:  r.attemptN,
		WallNs:    r.wall.Nanoseconds(),
	}
	if r.family == streamFamily {
		rec.MetricFamily = streamFamily
		rec.StreamN = r.attemptN
		rec.TTFBP50Ns = nsIf(r.ttfbP50, r.successN)
		rec.TTFBP95Ns = nsIf(r.ttfbP95, r.successN)
		rec.GapP95Ns = nsIf(r.gapP95, r.successN)
		rec.MaxStreamGapP95Ns = nsIf(r.maxStreamGapP95, r.successN)
		rec.MaxGapNs = nsIf(r.maxGap, r.successN)
		rec.FinalChunkSpreadNs = nsIf(r.finalChunkSpread, r.successN)
		rec.CompletionSpreadNs = nsIf(r.completionSpread, r.successN)
		rec.GoodputBytesPerSec = i64If(r.goodputBytesPerSec, r.successN)
	}
	return rec
}

// writeJSONL appends a leading "run" meta record followed by one JSON
// object per row to path, stamping runID on the header and every row. It
// MkdirAll's the parent dir, then opens O_APPEND so a single run sharded
// across processes (disjoint cells, shared PERF_MATRIX_RUN_ID) merges by
// concatenation. Distinct runs carry distinct run ids, so the reporter
// keeps them apart (and still rejects a doubled single run, which collides
// on run id + cell). Each record is marshaled then written with its
// trailing newline in a single Write so a record is never split across a
// partial append.
func writeJSONL(path, runID string, rows []perfMatrixRow) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // path is an operator-controlled perf-artifact location (PERF_MATRIX_HISTORY_DIR/PERF_MATRIX_JSONL), not external input
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is an operator-controlled perf-artifact location (PERF_MATRIX_HISTORY_DIR/PERF_MATRIX_JSONL), not external input
	if err != nil {
		return err
	}
	writeRecord := func(v any) error {
		line, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return writeFull(f, append(line, '\n'))
	}
	if err := writeRecord(newRunRecord(runID)); err != nil {
		_ = f.Close()
		return err
	}
	for _, r := range rows {
		rec := r.record()
		rec.Run = runID
		if err := writeRecord(rec); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// finishPerfMatrix drains the sink once and, from that single snapshot,
// logs the human table and emits the JSONL artifact(s). Emission is
// always-on: every run appends a timestamped per-run file to the history
// dir (best-effort — a failure is logged, never fatal, so a functional
// `make test` is never broken by an unwritable artifact dir). When
// PERF_MATRIX_JSONL is additionally set, that explicit path is also
// written and a failure there IS fatal, since the caller asked for it by
// name.
func finishPerfMatrix(t logf) {
	rows := perfMatrixSink.drain()
	if table := renderTable(rows); table != "" {
		t.Logf("%s", table)
	}
	if table := renderStreamTable(rows); table != "" {
		t.Logf("%s", table)
	}
	if len(rows) == 0 {
		return
	}
	runID := newRunID()
	if path := os.Getenv("PERF_MATRIX_JSONL"); path != "" {
		if err := writeJSONL(path, runID, rows); err != nil {
			t.Errorf("perf matrix: writing JSONL artifact to %q: %v", path, err)
		}
	}
	hpath := historyPath(runID)
	if err := writeJSONL(hpath, runID, rows); err != nil {
		t.Logf("perf matrix: best-effort history write to %q failed: %v", hpath, err)
	}
}

// logf is the slice of *testing.T finishPerfMatrix needs, kept narrow so
// the function is unit-testable without a real T.
type logf interface {
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
}

// repr reduces a sample to its representative latency at percentile p
// (0.50 → median, 0.95 → p95). A percentile is well-defined for any
// non-empty sample, so even a small run reports a true percentile that
// matches the *_p50 / *_p95 column (and the regression gate) names
// rather than silently switching to the mean; the *_n columns convey
// how many samples back it. Zero for an empty sample.
func repr(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return pct(sorted, p)
}

// splitScenarioPath separates the axis-cell values from the leaf scenario
// in a sub-test path. The first segment is always the entry-point test
// (e.g. "TestE2E_Mock") and is dropped. The next nAxes segments are the
// axis values (in Backend.Axes() order); everything after that joins into
// the leaf, so a scenario that nests its own t.Run sub-paths can't be
// mistaken for an axis. nAxes comes authoritatively from Backend.Axes(), so
// nAxes==0 means a fully-pinned backend with no axes: zero middle segments
// are axis values and the whole remainder is the leaf.
func splitScenarioPath(name string, nAxes int) (axisVals []string, leaf string) {
	segs := strings.Split(name, "/")
	if len(segs) <= 1 {
		return nil, name
	}
	segs = segs[1:] // drop the entry-point test name
	if nAxes < 0 {
		nAxes = 0
	}
	if nAxes > len(segs)-1 {
		nAxes = len(segs) - 1
	}
	return segs[:nAxes], strings.Join(segs[nAxes:], "/")
}

// splitMode peels a _PortForward / _SOCKS5 / _Connect token out of a
// scenario leaf so the mode becomes its own column, tolerating the
// token appearing mid-name (e.g. "..._SOCKS5_MultiTarget").
func splitMode(leaf string) (scenario, mode string) {
	for _, m := range []string{"PortForward", "SOCKS5", "Connect"} {
		tok := "_" + m
		if i := strings.Index(leaf, tok); i >= 0 {
			return leaf[:i] + leaf[i+len(tok):], m
		}
	}
	return leaf, ""
}
