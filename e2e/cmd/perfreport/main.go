// Command perfreport renders the aztunnel performance matrix from a
// JSON Lines artifact produced by the e2e harness (PERF_MATRIX_JSONL).
//
// Decoupling reporting from execution lets the same artifact drive
// several views and lets sharded runs be concatenated then rendered
// once. The artifact — not a shared Go type — is the contract, so this
// command owns its own copy of the record schema rather than importing
// the harness internals.
//
// Usage:
//
//	perfreport [flags] [artifact.jsonl]   # or read JSONL from stdin
//
// Flags:
//
//	--format table|grid|auto
//	                        table (default) reproduces the flat matrix
//	                        (with a backend column); grid pivots the
//	                        placement axis into a 3x3; auto renders the
//	                        grid when the (filtered) records hold a single
//	                        backend+scenario+mode, else the table.
//	--metric all|est|warm|cold
//	                        grid cell metric (default all → composite
//	                        cold/warm/est; est = cold_p50 − warm_p50).
//	--scenario <name>       restrict to one scenario (grid needs exactly one).
//	--mode <name>           restrict to one mode (grid needs exactly one).
//	--backend <name>        restrict to one backend, e.g. mock|azure (grid
//	                        needs exactly one; pass several artifacts to
//	                        compare backends side by side in the table).
//
// Multiple artifact files are merged (the table then shows one row per
// backend×axis×scenario×mode), so a mock run and an azure run render as a
// single comparison table: perfreport mock.jsonl azure.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// wantSchema is the only artifact schema this reporter understands; a
// row tagged with anything else is rejected so an incompatible producer
// fails loudly instead of rendering misaligned data.
const wantSchema = "perfmatrix/v1"

// record mirrors the harness's perfMatrixRecord (perfmatrix/v1). It is
// duplicated on purpose: the JSONL artifact is the boundary, so the
// reporter does not import the e2e scenarios package.
type record struct {
	Type     string            `json:"type"`
	Schema   string            `json:"schema"`
	Run      string            `json:"run,omitempty"`
	Backend  string            `json:"backend"`
	Axis     string            `json:"axis"`
	Axes     map[string]string `json:"axes,omitempty"`
	Scenario string            `json:"scenario"`
	Mode     string            `json:"mode"`
	// MetricFamily is "" (legacy RTT family) or "stream". Streaming rows
	// leave the RTT fields null and carry the streaming fields below.
	MetricFamily string `json:"metric_family,omitempty"`
	ColdP50Ns    *int64 `json:"cold_p50_ns"`
	WarmP50Ns    *int64 `json:"warm_p50_ns"`
	WarmP95Ns    *int64 `json:"warm_p95_ns"`
	ColdN        int    `json:"cold_n"`
	WarmN        int    `json:"warm_n"`
	SuccessN     int    `json:"success_n"`
	AttemptN     int    `json:"attempt_n"`
	WallNs       int64  `json:"wall_ns"`

	// Streaming family (metric_family == "stream").
	FirstRespP50Ns     *int64 `json:"first_resp_p50_ns,omitempty"`
	FirstRespP95Ns     *int64 `json:"first_resp_p95_ns,omitempty"`
	GapP95Ns           *int64 `json:"gap_p95_ns,omitempty"`
	MaxStreamGapP95Ns  *int64 `json:"max_stream_gap_p95_ns,omitempty"`
	MaxGapNs           *int64 `json:"max_gap_ns,omitempty"`
	FinalChunkSpreadNs *int64 `json:"final_chunk_spread_ns,omitempty"`
	CompletionSpreadNs *int64 `json:"completion_spread_ns,omitempty"`
	GoodputBytesPerSec *int64 `json:"goodput_bytes_per_sec,omitempty"`
	StreamN            int    `json:"stream_n,omitempty"`

	// Duplex family (metric_family == "duplex"). Per-leg p50/p95 from
	// the steady-state sample population pooled across flows, plus
	// throughput and per-flow ack fairness. BytesPerSecPerDir is the
	// per-direction application payload throughput (DuplexShape uses
	// symmetric BodySize so both legs carry the same byte count).
	RTTP50Ns          *int64 `json:"rtt_p50_ns,omitempty"`
	RTTP95Ns          *int64 `json:"rtt_p95_ns,omitempty"`
	ReqLegP50Ns       *int64 `json:"req_leg_p50_ns,omitempty"`
	ReqLegP95Ns       *int64 `json:"req_leg_p95_ns,omitempty"`
	RespLegP50Ns      *int64 `json:"resp_leg_p50_ns,omitempty"`
	RespLegP95Ns      *int64 `json:"resp_leg_p95_ns,omitempty"`
	ThinkP50Ns        *int64 `json:"think_p50_ns,omitempty"`
	ThinkP95Ns        *int64 `json:"think_p95_ns,omitempty"`
	AcksPerSec        *int64 `json:"acks_per_sec,omitempty"`
	BytesPerSecPerDir *int64 `json:"bytes_per_sec_per_dir,omitempty"`
	AckSpread         *int64 `json:"ack_spread,omitempty"`
	SampleN           int    `json:"sample_n,omitempty"`
	FlowN             int    `json:"flow_n,omitempty"`
}

// streamFamily is the metric_family value for streaming rows; the rtt
// family is the empty string. recFamily normalizes the two so identity
// keys and gating never confuse the families (an empty value from a
// legacy v1 artifact is the rtt family).
const streamFamily = "stream"
const duplexFamily = "duplex"

func recFamily(r record) string {
	if r.MetricFamily == "" {
		return "rtt"
	}
	return r.MetricFamily
}

// runMeta is the "run" header record (one per source file). The reporter
// retains these so a merged table can show its provenance — which backend
// / build / time each set of rows came from — and warn when the merged
// sources span different builds (a stale-artifact footgun, since the
// per-backend producers only overwrite their own file).
type runMeta struct {
	Type        string `json:"type"`
	Run         string `json:"run"`
	Backend     string `json:"backend"`
	GeneratedAt string `json:"generated_at"`
	GitSHA      string `json:"git_sha"`
	E2EAuth     string `json:"e2e_auth"`
	E2EDelay    string `json:"e2e_delay"`
}

func main() {
	format := flag.String("format", "table", "output format: table|grid|auto")
	metric := flag.String("metric", "all", "grid cell metric: all|est|warm|cold")
	scenario := flag.String("scenario", "", "restrict to one scenario")
	mode := flag.String("mode", "", "restrict to one mode")
	backend := flag.String("backend", "", "restrict to one backend (mock|azure)")
	var filters filterFlags
	flag.Var(&filters, "filter", "restrict to rows matching key=value (repeatable; key may be any axis name or backend/scenario/mode/run)")
	compare := flag.String("compare", "", "compare two values: baseline..candidate (run ids/prefixes or latest/previous for runs; exact values for other dimensions)")
	compareBy := flag.String("compare-by", "run", "dimension to compare across: run|backend|scenario|mode|axis|<named axis key>")
	allRuns := flag.Bool("all-runs", false, "show every run per cell instead of collapsing to the latest")
	failOver := flag.Float64("fail-over", 0, "with --compare: exit non-zero if any warm/cold p50 cell regresses by more than this percent (0 disables gating)")
	failMinAbs := flag.String("fail-min-abs", "20ms", "with --fail-over: a regression must also exceed this absolute p50 delta (a Go duration) before the gate trips, so tiny baselines can't flake it")
	flag.Parse()

	recs, runs, err := load(flag.Args())
	if err != nil {
		fail(err)
	}
	fm, err := filters.toMap()
	if err != nil {
		fail(err)
	}
	// The dedicated --scenario/--mode/--backend flags are sugar for the
	// equivalent --filter entries, so everything narrows through one path.
	addFilter(fm, "scenario", *scenario)
	addFilter(fm, "mode", *mode)
	addFilter(fm, "backend", *backend)
	recs = applyFilters(recs, fm)
	renderProvenance(os.Stdout, relevantRuns(runs, recs))

	if *compare != "" {
		base, cand, ok := strings.Cut(*compare, "..")
		if !ok || base == "" || cand == "" {
			fail(fmt.Errorf("--compare wants baseline..candidate, got %q", *compare))
		}
		// Filtering on the compared dimension would remove one side of the
		// comparison before it runs, so reject the contradiction up front.
		if _, ok := fm[*compareBy]; ok {
			fail(fmt.Errorf("cannot filter on the compared dimension %q while --compare-by %q is set", *compareBy, *compareBy))
		}
		// Compare each metric family independently: filtering, a stream-only
		// artifact, or a mid-migration history can leave one family with too
		// few runs while the other has a real comparison to gate. Attempt
		// each before deciding to skip, so a regression in any family is
		// never silently dropped because another family happened to be sparse.
		gs, rttErr := renderCompare(os.Stdout, recs, *compareBy, base, cand)
		sgs, streamErr := renderStreamCompare(os.Stdout, recs, *compareBy, base, cand)
		dgs, duplexErr := renderDuplexCompare(os.Stdout, recs, *compareBy, base, cand)

		// errNotEnoughRuns / errNoRTTRows / errNoStreamRows / errNoDuplexRows
		// mean "nothing to compare yet" for that family — skippable. Any
		// other error is fatal.
		rttSkippable := rttErr != nil && (errors.Is(rttErr, errNotEnoughRuns) || errors.Is(rttErr, errNoRTTRows))
		streamSkippable := streamErr != nil && (errors.Is(streamErr, errNoStreamRows) || errors.Is(streamErr, errNotEnoughRuns))
		duplexSkippable := duplexErr != nil && (errors.Is(duplexErr, errNoDuplexRows) || errors.Is(duplexErr, errNotEnoughRuns))
		if rttErr != nil && !rttSkippable {
			fail(rttErr)
		}
		if streamErr != nil && !streamSkippable {
			fail(streamErr)
		}
		if duplexErr != nil && !duplexSkippable {
			fail(duplexErr)
		}

		// Merge per-family accounting into the shared gateStats only when
		// that family's comparison actually ran.
		if streamErr == nil {
			gs.streamPaired = sgs.streamPaired
			gs.streamComparable = sgs.streamComparable
			gs.streamRegressions = sgs.streamRegressions
			gs.missingInCand = append(gs.missingInCand, sgs.missingInCand...)
		}
		if duplexErr == nil {
			gs.duplexPaired = dgs.duplexPaired
			gs.duplexComparable = dgs.duplexComparable
			gs.duplexRegressions = dgs.duplexRegressions
			gs.missingInCand = append(gs.missingInCand, dgs.missingInCand...)
		}

		// No family produced a comparison: a bootstrap gate run skips
		// (exit 0); a plain compare surfaces the error.
		if rttErr != nil && streamErr != nil && duplexErr != nil {
			if *failOver > 0 {
				fmt.Fprintf(os.Stderr, "perf-gate: nothing to compare yet (skipping) — rtt: %v; stream: %v; duplex: %v\n", rttErr, streamErr, duplexErr)
				return
			}
			fail(rttErr)
		}

		if *failOver > 0 {
			applyGate(gs, *failOver, *failMinAbs)
		}
		return
	}
	if *failOver > 0 {
		fail(errors.New("--fail-over requires --compare (the gate compares a baseline to a candidate)"))
	}

	// By default collapse to the newest run per cell so a merged history
	// shows one current number per cell; --all-runs keeps every run (the
	// table then grows a run column). Either way, note when history exists.
	nRuns := len(distinctRuns(recs))
	if !*allRuns {
		recs = latestPerCell(recs)
	}
	if nRuns > 1 {
		if *allRuns {
			fmt.Fprintf(os.Stderr, "note: %d runs present; showing all (use --compare a..b for deltas)\n", nRuns)
		} else {
			fmt.Fprintf(os.Stderr, "note: %d runs present; showing latest per cell (use --all-runs or --compare a..b)\n", nRuns)
		}
	}

	switch *format {
	case "table":
		if err := renderTable(os.Stdout, recs); err != nil {
			fail(err)
		}
	case "grid":
		if err := renderGrid(os.Stdout, recs, *metric); err != nil {
			fail(err)
		}
	case "auto":
		// Grid needs exactly one scenario and one mode; when the artifact
		// holds more than that (e.g. a full-scenario sweep), fall back to
		// the table, which renders any number of rows.
		if singleCell(recs) {
			if err := renderGrid(os.Stdout, recs, *metric); err != nil {
				fail(err)
			}
		} else if err := renderTable(os.Stdout, recs); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown --format %q (want table|grid|auto)", *format))
	}
}

// singleCell reports whether the records collapse to one backend, one
// scenario, and one mode — the precondition for a grid render.
func singleCell(recs []record) bool {
	backends, scenarios, modes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		backends[r.Backend] = true
		scenarios[r.Scenario] = true
		modes[r.Mode] = true
	}
	return len(backends) == 1 && len(scenarios) == 1 && len(modes) == 1
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "perfreport:", err)
	os.Exit(1)
}

// load reads JSONL from the named files (or stdin when none are given),
// skipping blank lines, collecting "run" header records, and rejecting
// duplicate (backend, axis, scenario, mode, run) keys so a stale or
// double-appended artifact fails loudly instead of silently picking one
// row. The run id is part of the dedup key, so distinct runs of the same
// cell coexist (that is how multi-run history and before/after comparison
// work); only a repeated cell *within the same run* is an error. It
// returns the data rows and the run-header records (the latter drive the
// provenance line).
func load(paths []string) ([]record, []runMeta, error) {
	var readers []io.Reader
	var closers []io.Closer
	if len(paths) == 0 {
		readers = append(readers, os.Stdin)
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, nil, err
		}
		readers = append(readers, f)
		closers = append(closers, f)
	}
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()

	var recs []record
	var runs []runMeta
	seen := map[string]bool{}
	seenRun := map[string]bool{}
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var rec record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return nil, nil, fmt.Errorf("malformed JSONL line: %v", err)
			}
			// Retain the run header for provenance; skip any other
			// non-row meta type. Everything else is a data row and must
			// carry the v1 schema so a malformed or pre-contract line
			// fails loudly rather than being rendered as if it were valid.
			if rec.Type == "run" {
				var rm runMeta
				if err := json.Unmarshal([]byte(line), &rm); err != nil {
					return nil, nil, fmt.Errorf("malformed run record: %v", err)
				}
				// A single run sharded across processes writes one run
				// header per shard (same backend + run id). Keep only the
				// first so provenance lists each run once, not once per
				// shard, when artifacts are concatenated. Only dedup on a
				// real run id; an empty id (legacy/hand-written artifact)
				// can't be proven a shard, so keep every such header.
				if rm.Run != "" {
					rk := rm.Backend + "\x00" + rm.Run
					if seenRun[rk] {
						continue
					}
					seenRun[rk] = true
				}
				runs = append(runs, rm)
				continue
			}
			if rec.Type != "" && rec.Type != "row" {
				continue
			}
			if rec.Schema != wantSchema {
				return nil, nil, fmt.Errorf("row record has schema %q, want %q "+
					"(missing or produced by an incompatible harness version)", rec.Schema, wantSchema)
			}
			key := cellKey(rec) + "\x00" + rec.Run
			if seen[key] {
				return nil, nil, fmt.Errorf("duplicate row for backend=%q axis=%q scenario=%q mode=%q run=%q "+
					"(stale or double-appended artifact?)", rec.Backend, displayAxes(rec), rec.Scenario, rec.Mode, rec.Run)
			}
			seen[key] = true
			recs = append(recs, rec)
		}
		if err := sc.Err(); err != nil {
			return nil, nil, err
		}
	}
	if len(recs) == 0 {
		return nil, nil, errors.New("no rows in artifact")
	}
	return recs, runs, nil
}

// relevantRuns returns the run headers whose backend actually appears in
// recs, so a --backend-filtered render shows only the matching source(s).
func relevantRuns(runs []runMeta, recs []record) []runMeta {
	present := map[string]bool{}
	for _, r := range recs {
		present[r.Backend] = true
	}
	var out []runMeta
	for _, rm := range runs {
		if present[rm.Backend] {
			out = append(out, rm)
		}
	}
	return out
}

// renderProvenance prints a one-line summary of the source runs (backend,
// build, time) and a non-fatal warning when the merged sources span more
// than one build — making the stale-artifact footgun visible without
// changing the merge-by-default UX. No output when there are no run
// headers (e.g. a pre-provenance artifact).
func renderProvenance(w io.Writer, runs []runMeta) {
	if len(runs) == 0 {
		return
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Backend < runs[j].Backend })
	shas := map[string]bool{}
	var parts []string
	for _, r := range runs {
		sha := r.GitSHA
		if sha == "" {
			sha = "?"
		}
		shas[r.GitSHA] = true
		label := dash(r.Backend) + "@" + sha
		if r.GeneratedAt != "" {
			label += " " + r.GeneratedAt
		}
		parts = append(parts, label)
	}
	_, _ = fmt.Fprintln(w, "sources: "+strings.Join(parts, "  |  "))
	if len(shas) > 1 {
		_, _ = fmt.Fprintln(w, "warning: merged artifacts span multiple builds (git shas differ) — comparison may mix versions")
	}
	_, _ = fmt.Fprintln(w)
}

// filterFlags collects repeatable --filter key=value pairs.
type filterFlags []string

func (f *filterFlags) String() string { return strings.Join(*f, ",") }
func (f *filterFlags) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func (f filterFlags) toMap() (map[string]string, error) {
	m := map[string]string{}
	for _, kv := range f {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad --filter %q (want key=value)", kv)
		}
		m[k] = v
	}
	return m, nil
}

func addFilter(m map[string]string, key, val string) {
	if val != "" {
		m[key] = val
	}
}

// applyFilters keeps rows matching every key=value constraint. backend,
// scenario, and mode are virtual keys; any other key is matched against
// the row's named axes (a row missing that axis never matches).
func applyFilters(recs []record, filters map[string]string) []record {
	if len(filters) == 0 {
		return recs
	}
	var out []record
	for _, r := range recs {
		if matchesFilters(r, filters) {
			out = append(out, r)
		}
	}
	return out
}

func matchesFilters(r record, filters map[string]string) bool {
	for k, v := range filters {
		switch k {
		case "backend":
			if r.Backend != v {
				return false
			}
		case "scenario":
			if r.Scenario != v {
				return false
			}
		case "mode":
			if r.Mode != v {
				return false
			}
		case "run":
			if r.Run != v {
				return false
			}
		default:
			if av, ok := r.Axes[k]; !ok || av != v {
				return false
			}
		}
	}
	return true
}

// axisColumns is the sorted union of named-axis keys across recs, or nil
// when no row carries named axes (a legacy artifact renders the single
// flat "axis" column instead).
func axisColumns(recs []record) []string {
	set := map[string]bool{}
	for _, r := range recs {
		for k := range r.Axes {
			set[k] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	cols := make([]string, 0, len(set))
	for k := range set {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	return cols
}

// canonicalAxes is a deterministic identity for a row's axis cell, used
// for sort and dedup. It serializes the named axes when present, else
// falls back to the flat axis string so legacy artifacts keep distinct
// keys.
func canonicalAxes(r record) string { return canonicalAxesExcept(r, "") }

// displayAxes is the human-facing axis label for messages (gate failures,
// duplicate-row errors). It prefers the flat Axis path the harness always
// emits (e.g. "entra/far") so the named-axis separator used by
// canonicalAxes (the non-printable \x1f) never leaks into user-visible
// text; it falls back to canonicalAxes only for a record with no flat Axis.
func displayAxes(r record) string {
	if r.Axis != "" {
		return r.Axis
	}
	return canonicalAxes(r)
}

// canonicalAxesExcept is canonicalAxes with one dimension dropped, used by
// the compare-by-dimension path to build a residual identity that excludes
// the dimension under comparison. skip=="" reproduces canonicalAxes
// exactly. For a legacy row (no named axes) the flat Axis stands in for the
// whole axis dimension, so skip=="axis" drops it.
func canonicalAxesExcept(r record, skip string) string {
	if len(r.Axes) == 0 {
		if skip == "axis" {
			return ""
		}
		return r.Axis
	}
	keys := make([]string, 0, len(r.Axes))
	for k := range r.Axes {
		if k == skip {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\x1f')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(r.Axes[k])
	}
	return b.String()
}

// cellKey identifies a matrix cell independent of which run produced it:
// (backend, axes, scenario, mode). Two runs of the same cell share a
// cellKey, which is how before/after comparison and latest-per-cell
// collapsing match rows across runs.
func cellKey(r record) string {
	return recFamily(r) + "\x00" + r.Backend + "\x00" + canonicalAxes(r) + "\x00" + r.Scenario + "\x00" + r.Mode
}

// distinctRuns returns the sorted-descending set of run ids present, so
// the newest run sorts first (run ids are lexically sortable timestamps).
func distinctRuns(recs []record) []string {
	set := map[string]bool{}
	for _, r := range recs {
		set[r.Run] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// latestPerCell keeps, for each cell, only the row from the newest run, so
// the default table shows one current number per cell even when several
// runs are merged. It returns the collapsed rows.
func latestPerCell(recs []record) []record {
	best := map[string]record{}
	for _, r := range recs {
		k := cellKey(r)
		if cur, ok := best[k]; !ok || r.Run > cur.Run {
			best[k] = r
		}
	}
	out := make([]record, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	return out
}

// dimValue extracts the value of the comparison dimension from a row. The
// bool is false when the row does not carry the dimension at all (an axis
// key it lacks, or a legacy-only "axis" on a named-axis row), so such rows
// are excluded from the comparison rather than matched as empty.
func dimValue(r record, dim string) (string, bool) {
	switch dim {
	case "run":
		return r.Run, true
	case "backend":
		return r.Backend, true
	case "scenario":
		return r.Scenario, true
	case "mode":
		return r.Mode, true
	case "axis":
		if len(r.Axes) == 0 {
			return r.Axis, r.Axis != ""
		}
		return "", false
	default:
		v, ok := r.Axes[dim]
		return v, ok
	}
}

// isAxisDim reports whether dim names a key in the named Axes map rather
// than one of the fixed identity fields or the legacy flat axis.
func isAxisDim(dim string) bool {
	switch dim {
	case "run", "backend", "scenario", "mode", "axis":
		return false
	default:
		return true
	}
}

// residualKey identifies a row by every identity dimension EXCEPT the one
// under comparison, so two rows differing only in that dimension (e.g.
// auth=sas vs auth=entra of the same run/scenario/mode) collapse to one
// comparison cell. When crossRun is true the run id is dropped from the
// identity too, so two rows from different runs that agree on every
// other dimension pair up. renderCompare/renderStreamCompare/
// renderDuplexCompare use the crossRun=true mode as a fallback when
// within-run pairing produces zero matches, which is the typical
// situation when a user has done two separate single-value runs (e.g.
// E2E_DELAY=zero in one and E2E_DELAY=default in another) and wants to
// compare them.
func residualKey(r record, dim string, crossRun bool) string {
	backend, scenario, mode, run := r.Backend, r.Scenario, r.Mode, r.Run
	switch dim {
	case "backend":
		backend = ""
	case "scenario":
		scenario = ""
	case "mode":
		mode = ""
	case "run":
		run = ""
	}
	if crossRun {
		run = ""
	}
	return recFamily(r) + "\x00" + backend + "\x00" + canonicalAxesExcept(r, dim) + "\x00" + scenario + "\x00" + mode + "\x00" + run
}

// putNewestByCell stores r at key k in m, preferring the newer of r and
// any existing entry. "Newer" is determined by lexicographic Run id
// comparison (run ids are UTC-timestamp-prefixed by newRunID, so
// lexicographic ascending matches chronological order).
//
// This matters in cross-run pairing: when residualKey(crossRun=true)
// drops the run id, multiple runs with the same residual can map to the
// same key. A plain `m[k] = r` would pick by input order — non-
// deterministic on multi-run artifacts and prone to comparing against a
// stale run. Within-run pairing carries the run id in the key, so
// collisions only happen on legitimate duplicate data points and the
// newest-wins rule is harmless.
func putNewestByCell(m map[string]record, k string, r record) {
	if existing, ok := m[k]; ok && existing.Run >= r.Run {
		return
	}
	m[k] = r
}

// distinctDimValues returns the sorted-ascending set of non-empty values a
// dimension takes across recs (empty values are not selectable).
func distinctDimValues(recs []record, dim string) []string {
	set := map[string]bool{}
	for _, r := range recs {
		if v, ok := dimValue(r, dim); ok && v != "" {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// resolveDimValue maps a selector to a concrete value of the dimension. The
// run dimension keeps its latest/previous/prefix semantics; every other
// dimension requires an exact match against the values actually present.
func resolveDimValue(dim, sel string, recs []record) (string, error) {
	if dim == "run" {
		return resolveRun(sel, distinctRuns(recs))
	}
	values := distinctDimValues(recs, dim)
	if len(values) == 0 {
		return "", fmt.Errorf("no rows carry dimension %q", dim)
	}
	for _, v := range values {
		if v == sel {
			return v, nil
		}
	}
	return "", fmt.Errorf("no %s value matches %q (available: %s)", dim, sel, strings.Join(values, ", "))
}

// errNotEnoughRuns marks the "fewer than two runs to compare" condition so
// a gate (--fail-over) can treat it as a skip rather than a failure.
var errNotEnoughRuns = errors.New("not enough runs to compare")

// errNoRTTRows signals that a comparison input carried no rtt rows at
// all (the run only produced stream / duplex output). Callers skip the
// rtt comparison rather than failing.
var errNoRTTRows = errors.New("no rtt rows present")

// resolveRun maps a run selector to a concrete run id. "latest" and
// "previous" pick the 1st and 2nd newest runs; otherwise the value must
// equal a run id or be an unambiguous prefix of exactly one.
func resolveRun(sel string, runs []string) (string, error) {
	switch sel {
	case "latest":
		if len(runs) == 0 {
			return "", fmt.Errorf("%w: no runs present", errNotEnoughRuns)
		}
		return runs[0], nil
	case "previous":
		if len(runs) < 2 {
			return "", fmt.Errorf("%w: need at least 2 runs for %q (have %d)", errNotEnoughRuns, sel, len(runs))
		}
		return runs[1], nil
	}
	var match string
	var n int
	for _, id := range runs {
		if id == sel {
			return id, nil
		}
		if strings.HasPrefix(id, sel) {
			match = id
			n++
		}
	}
	switch n {
	case 1:
		return match, nil
	case 0:
		return "", fmt.Errorf("no run matches %q", sel)
	default:
		return "", fmt.Errorf("run selector %q is ambiguous (%d matches)", sel, n)
	}
}

func sortRecs(recs []record) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Backend != recs[j].Backend {
			return recs[i].Backend < recs[j].Backend
		}
		if a, b := canonicalAxes(recs[i]), canonicalAxes(recs[j]); a != b {
			return a < b
		}
		if recs[i].Scenario != recs[j].Scenario {
			return recs[i].Scenario < recs[j].Scenario
		}
		if recs[i].Mode != recs[j].Mode {
			return recs[i].Mode < recs[j].Mode
		}
		// Newest run first so an --all-runs table reads top-down in time.
		return recs[i].Run > recs[j].Run
	})
}

func renderTable(w io.Writer, recs []record) error {
	var rtt, stream, duplex []record
	for _, r := range recs {
		switch recFamily(r) {
		case streamFamily:
			stream = append(stream, r)
		case duplexFamily:
			duplex = append(duplex, r)
		default:
			rtt = append(rtt, r)
		}
	}
	if len(rtt) > 0 {
		if err := renderRTTTable(w, rtt); err != nil {
			return err
		}
	}
	if len(stream) > 0 {
		if len(rtt) > 0 {
			_, _ = fmt.Fprintln(w)
		}
		if err := renderStreamReportTable(w, stream); err != nil {
			return err
		}
	}
	if len(duplex) > 0 {
		if len(rtt) > 0 || len(stream) > 0 {
			_, _ = fmt.Fprintln(w)
		}
		if err := renderDuplexReportTable(w, duplex); err != nil {
			return err
		}
	}
	return nil
}

func renderRTTTable(w io.Writer, recs []record) error {
	sortRecs(recs)
	cols := axisColumns(recs)
	warnMixedAxisKeys(w, recs, cols)
	showRun := len(distinctRuns(recs)) >= 2
	_, _ = fmt.Fprintln(w, "PERF MATRIX (client-side RTT; est = cold_p50 − warm_p50 ≈ establishment cost)")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	header := []string{"backend"}
	if len(cols) == 0 {
		header = append(header, "axis")
	} else {
		header = append(header, cols...)
	}
	if showRun {
		header = append(header, "run")
	}
	header = append(header, "scenario", "mode", "cold_p50", "warm_p50", "warm_p95", "est", "cold_n", "warm_n", "success", "wall")
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))

	for _, r := range recs {
		cells := []string{dash(r.Backend)}
		if len(cols) == 0 {
			cells = append(cells, dash(r.Axis))
		} else {
			for _, k := range cols {
				cells = append(cells, dash(r.Axes[k]))
			}
		}
		if showRun {
			cells = append(cells, dash(r.Run))
		}
		cells = append(cells,
			r.Scenario, dash(r.Mode),
			durOrDash(r.ColdP50Ns), durOrDash(r.WarmP50Ns), durOrDash(r.WarmP95Ns),
			estCell(r),
			fmt.Sprintf("%d", r.ColdN), fmt.Sprintf("%d", r.WarmN),
			fmt.Sprintf("%d/%d", r.SuccessN, r.AttemptN), dur(r.WallNs),
		)
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// renderStreamReportTable renders the streaming-family rows: start
// latency, inter-chunk jitter, fairness spread, and goodput.
func renderStreamReportTable(w io.Writer, recs []record) error {
	sortRecs(recs)
	cols := axisColumns(recs)
	warnMixedAxisKeys(w, recs, cols)
	showRun := len(distinctRuns(recs)) >= 2
	_, _ = fmt.Fprintln(w, "PERF MATRIX (streaming; first_resp = client-side time to first server output from release, spread = max−min across streams)")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	header := []string{"backend"}
	if len(cols) == 0 {
		header = append(header, "axis")
	} else {
		header = append(header, cols...)
	}
	if showRun {
		header = append(header, "run")
	}
	header = append(header, "scenario", "mode", "first_resp_p50", "first_resp_p95", "gap_p95", "maxgap_p95", "max_gap", "final_spread", "completion_spread", "goodput_KiB/s", "success", "wall")
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))

	for _, r := range recs {
		cells := []string{dash(r.Backend)}
		if len(cols) == 0 {
			cells = append(cells, dash(r.Axis))
		} else {
			for _, k := range cols {
				cells = append(cells, dash(r.Axes[k]))
			}
		}
		if showRun {
			cells = append(cells, dash(r.Run))
		}
		cells = append(cells,
			r.Scenario, dash(r.Mode),
			durOrDash(r.FirstRespP50Ns), durOrDash(r.FirstRespP95Ns),
			durOrDash(r.GapP95Ns), durOrDash(r.MaxStreamGapP95Ns),
			durOrDash(r.MaxGapNs), durOrDash(r.FinalChunkSpreadNs), durOrDash(r.CompletionSpreadNs),
			goodputCell(r.GoodputBytesPerSec),
			fmt.Sprintf("%d/%d", r.SuccessN, r.AttemptN), dur(r.WallNs),
		)
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// goodputCell renders bytes/sec as KiB/s, or "-" when unmeasured.
func goodputCell(bps *int64) string {
	if bps == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(*bps)/1024)
}

// renderDuplexReportTable renders the duplex-family rows: per-leg p50/p95,
// pooled exchange throughput, and per-flow ack fairness.
func renderDuplexReportTable(w io.Writer, recs []record) error {
	sortRecs(recs)
	cols := axisColumns(recs)
	warnMixedAxisKeys(w, recs, cols)
	showRun := len(distinctRuns(recs)) >= 2
	_, _ = fmt.Fprintln(w, "PERF MATRIX (duplex; per-leg latency under sustained bidirectional load, bytes/s is per direction, ack_spread = max−min acks across successful flows)")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	header := []string{"backend"}
	if len(cols) == 0 {
		header = append(header, "axis")
	} else {
		header = append(header, cols...)
	}
	if showRun {
		header = append(header, "run")
	}
	header = append(header, "scenario", "mode", "rtt_p50", "rtt_p95", "req_leg_p50", "req_leg_p95", "resp_leg_p50", "resp_leg_p95", "think_p50", "think_p95", "acks/s", "bytes/s", "ack_spread", "sample_n", "success", "wall")
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))

	for _, r := range recs {
		cells := []string{dash(r.Backend)}
		if len(cols) == 0 {
			cells = append(cells, dash(r.Axis))
		} else {
			for _, k := range cols {
				cells = append(cells, dash(r.Axes[k]))
			}
		}
		if showRun {
			cells = append(cells, dash(r.Run))
		}
		cells = append(cells,
			r.Scenario, dash(r.Mode),
			durOrDash(r.RTTP50Ns), durOrDash(r.RTTP95Ns),
			durOrDash(r.ReqLegP50Ns), durOrDash(r.ReqLegP95Ns),
			durOrDash(r.RespLegP50Ns), durOrDash(r.RespLegP95Ns),
			durOrDash(r.ThinkP50Ns), durOrDash(r.ThinkP95Ns),
			i64Cell(r.AcksPerSec), i64Cell(r.BytesPerSecPerDir), i64Cell(r.AckSpread),
			fmt.Sprintf("%d", r.SampleN),
			fmt.Sprintf("%d/%d", r.SuccessN, r.AttemptN), dur(r.WallNs),
		)
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// i64Cell renders a nullable int64 metric, "-" when unmeasured.
func i64Cell(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *v)
}

// regression is one positive warm/cold p50 delta for a single cell, used
// by the --fail-over gate. pct is the percent slowdown; absNs the absolute
// p50 increase in nanoseconds.
type regression struct {
	label string
	pct   float64
	absNs int64
}

// gateStats summarises a comparison for the --fail-over gate: how many
// cells paired on both sides, which baseline cells the candidate dropped,
// and every warm/cold p50 regression observed. The stream* fields carry
// the streaming family's parallel accounting so the gate can fail
// per-family (e.g. a stream comparison that paired cells but produced no
// comparable streaming metric must not silently pass).
type gateStats struct {
	paired        int
	missingInCand []string
	regressions   []regression

	streamPaired      int
	streamComparable  int
	streamRegressions []streamRegression

	duplexPaired      int
	duplexComparable  int
	duplexRegressions []regression // percent-based, same shape as rtt
}

// streamRegression is one positive absolute increase of a gated streaming
// metric (max_stream_gap_p95 or final_chunk_spread) for a single cell.
// Streaming metrics are gated on absolute deltas, not percentages, because
// an injected trickle interval inflates the percentage denominator and would
// hide an absolute fairness/jitter regression.
type streamRegression struct {
	label string
	absNs int64
}

// filterFamily returns the records whose normalized metric family equals
// want ("rtt" or "stream").
func filterFamily(recs []record, want string) []record {
	out := make([]record, 0, len(recs))
	for _, r := range recs {
		if recFamily(r) == want {
			out = append(out, r)
		}
	}
	return out
}

// regressionPct returns the percent and absolute slowdown of cand over
// base, and ok=true only when both are present, base is positive, and cand
// is actually slower (an improvement or unchanged value is not a
// regression and never gates).
func regressionPct(base, cand *int64) (pct float64, absNs int64, ok bool) {
	if base == nil || cand == nil || *base <= 0 {
		return 0, 0, false
	}
	d := *cand - *base
	if d <= 0 {
		return 0, 0, false
	}
	return float64(d) / float64(*base) * 100, d, true
}

// cellLabel is a short human key for a comparison cell, used in gate
// messages so a failure names the offending scenario/mode/axis.
func cellLabel(r record) string {
	ax := displayAxes(r)
	if ax == "" {
		ax = "-"
	}
	return fmt.Sprintf("%s/%s/%s/%s", dash(r.Backend), ax, r.Scenario, dash(r.Mode))
}

// renderCompare matches cells across two values of a dimension (runs by
// default; or a named axis like auth/delay, or backend/scenario/mode) and
// prints baseline, candidate, and the delta for the three headline
// metrics. The compared dimension is dropped from the identity columns.
// Cells present on only one side are listed (with dashes) so missing
// coverage is visible. For non-run dimensions the run id stays part of a
// cell's identity, so a multi-run history yields one comparison row per
// run (surfaced via a run column). It returns gateStats for the
// --fail-over gate; the stats are meaningful only when no error is
// returned.
func renderCompare(w io.Writer, recs []record, dim, baseSel, candSel string) (gateStats, error) {
	var gs gateStats
	// The rtt compare table only handles the RTT metric family; streaming
	// rows are compared separately by renderStreamCompare with their own
	// columns and gate.
	recs = filterFamily(recs, "rtt")
	if len(recs) == 0 {
		return gs, errNoRTTRows
	}
	base, err := resolveDimValue(dim, baseSel, recs)
	if err != nil {
		return gs, fmt.Errorf("baseline %q: %w", baseSel, err)
	}
	cand, err := resolveDimValue(dim, candSel, recs)
	if err != nil {
		return gs, fmt.Errorf("candidate %q: %w", candSel, err)
	}
	if base == cand {
		return gs, fmt.Errorf("baseline and candidate resolve to the same %s %q", dim, base)
	}

	baseByCell := map[string]record{}
	candByCell := map[string]record{}
	cellOrder := []record{}
	seen := map[string]bool{}
	excluded := 0
	crossRun := false
	buildMaps := func(crossRun bool) {
		baseByCell = map[string]record{}
		candByCell = map[string]record{}
		cellOrder = []record{}
		seen = map[string]bool{}
		excluded = 0
		for _, r := range recs {
			v, ok := dimValue(r, dim)
			if !ok {
				excluded++
				continue
			}
			if v != base && v != cand {
				continue
			}
			k := residualKey(r, dim, crossRun)
			if v == base {
				putNewestByCell(baseByCell, k, r)
			} else {
				putNewestByCell(candByCell, k, r)
			}
			if !seen[k] {
				seen[k] = true
				cellOrder = append(cellOrder, r)
			}
		}
	}
	buildMaps(false)

	for k, br := range baseByCell {
		if _, ok := candByCell[k]; ok {
			gs.paired++
		} else {
			gs.missingInCand = append(gs.missingInCand, cellLabel(br))
		}
	}
	// Auto-fallback: if within-run pairing produced nothing (typical when
	// a user has two separate runs each pinning one of the compared
	// values), retry with run dropped from the residual key. This pairs
	// rows across runs that agree on every other dimension. Announce the
	// fallback so the user knows the comparison spans runs.
	if gs.paired == 0 && dim != "run" {
		gs.missingInCand = nil
		buildMaps(true)
		for k, br := range baseByCell {
			if _, ok := candByCell[k]; ok {
				gs.paired++
			} else {
				gs.missingInCand = append(gs.missingInCand, cellLabel(br))
			}
		}
		if gs.paired > 0 {
			crossRun = true
			_, _ = fmt.Fprintf(w, "note: pairing rows across runs — no within-run %s pairs found\n", dim)
		}
	}
	if gs.paired == 0 && dim != "run" {
		return gs, fmt.Errorf("no cells matched on both sides of %s %q..%q: tried within-run pairing (rows must share backend/scenario/mode/axes/run-id) and cross-run pairing (drops run id), neither produced a pair — narrow with filters or compare runs instead", dim, base, cand)
	}
	if excluded > 0 {
		_, _ = fmt.Fprintf(w, "warning: %d rows lack dimension %q and were excluded from the comparison\n", excluded, dim)
	}

	sortRecs(cellOrder)
	cols := axisColumns(cellOrder)
	namedAxes := len(cols) > 0
	if isAxisDim(dim) {
		cols = removeStr(cols, dim)
	}
	showBackend := dim != "backend"
	showScenario := dim != "scenario"
	showMode := dim != "mode"
	showRun := dim != "run" && len(distinctRuns(cellOrder)) > 1

	_, _ = fmt.Fprintf(w, "PERF COMPARE  %s: baseline=%s  candidate=%s  (Δ%% = (candidate−baseline)/baseline; negative is faster)\n", dim, base, cand)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var header []string
	if showBackend {
		header = append(header, "backend")
	}
	if namedAxes {
		header = append(header, cols...)
	} else if dim != "axis" {
		header = append(header, "axis")
	}
	if showRun {
		header = append(header, "run")
	}
	if showScenario {
		header = append(header, "scenario")
	}
	if showMode {
		header = append(header, "mode")
	}
	header = append(header,
		"warm_base", "warm_cand", "warm_Δ%",
		"cold_base", "cold_cand", "cold_Δ%",
		"est_base", "est_cand", "est_Δ%")
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))

	for _, c := range cellOrder {
		k := residualKey(c, dim, crossRun)
		b, bok := baseByCell[k]
		n, nok := candByCell[k]
		var cells []string
		if showBackend {
			cells = append(cells, dash(c.Backend))
		}
		if namedAxes {
			for _, key := range cols {
				cells = append(cells, dash(c.Axes[key]))
			}
		} else if dim != "axis" {
			cells = append(cells, dash(c.Axis))
		}
		if showRun {
			cells = append(cells, dash(c.Run))
		}
		if showScenario {
			cells = append(cells, c.Scenario)
		}
		if showMode {
			cells = append(cells, dash(c.Mode))
		}
		cells = append(cells, compareTriplet(ptrIf(bok, b.WarmP50Ns), ptrIf(nok, n.WarmP50Ns))...)
		cells = append(cells, compareTriplet(ptrIf(bok, b.ColdP50Ns), ptrIf(nok, n.ColdP50Ns))...)
		cells = append(cells, compareTriplet(estNs(b, bok), estNs(n, nok))...)
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))

		// Collect warm/cold p50 regressions for the gate. est is derived
		// (cold − warm) and amplifies noise, so it is reported but never
		// gated. Only paired cells contribute.
		if bok && nok {
			if pct, abs, ok := regressionPct(b.WarmP50Ns, n.WarmP50Ns); ok {
				gs.regressions = append(gs.regressions, regression{cellLabel(c) + " warm_p50", pct, abs})
			}
			if pct, abs, ok := regressionPct(b.ColdP50Ns, n.ColdP50Ns); ok {
				gs.regressions = append(gs.regressions, regression{cellLabel(c) + " cold_p50", pct, abs})
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return gs, err
	}
	return gs, nil
}

// errNoStreamRows signals that a comparison input carried no streaming
// rows at all, so the streaming comparison is a clean no-op (not a gate
// failure). The caller skips streaming when it sees this.
var errNoStreamRows = errors.New("no streaming rows present")

// renderStreamCompare matches streaming-family cells across two values of
// a dimension and prints the streaming metrics side by side. It populates
// the stream* fields of gateStats: streamPaired (cells matched on both
// sides), streamComparable (paired cells with at least one gated metric
// present on both sides), and streamRegressions (absolute increases of the
// gated metrics max_stream_gap_p95 and final_chunk_spread). Streaming metrics
// are compared on absolute deltas, so an injected trickle interval can't
// dilute a regression the way a percentage denominator would. first_resp_p95
// is shown for context but not gated: it tracks a warm round-trip plus the
// server's injected think time, neither of which is a tunnel regression.
func renderStreamCompare(w io.Writer, recs []record, dim, baseSel, candSel string) (gateStats, error) {
	var gs gateStats
	recs = filterFamily(recs, streamFamily)
	if len(recs) == 0 {
		return gs, errNoStreamRows
	}
	base, err := resolveDimValue(dim, baseSel, recs)
	if err != nil {
		return gs, fmt.Errorf("baseline %q: %w", baseSel, err)
	}
	cand, err := resolveDimValue(dim, candSel, recs)
	if err != nil {
		return gs, fmt.Errorf("candidate %q: %w", candSel, err)
	}
	if base == cand {
		return gs, fmt.Errorf("baseline and candidate resolve to the same %s %q", dim, base)
	}

	baseByCell := map[string]record{}
	candByCell := map[string]record{}
	cellOrder := []record{}
	seen := map[string]bool{}
	crossRun := false
	buildMaps := func(crossRun bool) {
		baseByCell = map[string]record{}
		candByCell = map[string]record{}
		cellOrder = []record{}
		seen = map[string]bool{}
		for _, r := range recs {
			v, ok := dimValue(r, dim)
			if !ok || (v != base && v != cand) {
				continue
			}
			k := residualKey(r, dim, crossRun)
			if v == base {
				putNewestByCell(baseByCell, k, r)
			} else {
				putNewestByCell(candByCell, k, r)
			}
			if !seen[k] {
				seen[k] = true
				cellOrder = append(cellOrder, r)
			}
		}
	}
	buildMaps(false)
	for k, br := range baseByCell {
		if _, ok := candByCell[k]; ok {
			gs.streamPaired++
		} else {
			gs.missingInCand = append(gs.missingInCand, cellLabel(br))
		}
	}
	// Auto-fallback to cross-run pairing when within-run produced no
	// matches, mirroring renderCompare. Same shape: stream comparison
	// across two single-value runs is just as legitimate as rtt.
	if gs.streamPaired == 0 && dim != "run" {
		gs.missingInCand = nil
		buildMaps(true)
		for k, br := range baseByCell {
			if _, ok := candByCell[k]; ok {
				gs.streamPaired++
			} else {
				gs.missingInCand = append(gs.missingInCand, cellLabel(br))
			}
		}
		if gs.streamPaired > 0 {
			crossRun = true
			_, _ = fmt.Fprintf(w, "note: pairing streaming rows across runs — no within-run %s pairs found\n", dim)
		}
	}

	sortRecs(cellOrder)
	cols := axisColumns(cellOrder)
	namedAxes := len(cols) > 0
	if isAxisDim(dim) {
		cols = removeStr(cols, dim)
	}
	showBackend := dim != "backend"
	showScenario := dim != "scenario"
	showMode := dim != "mode"
	showRun := dim != "run" && len(distinctRuns(cellOrder)) > 1

	_, _ = fmt.Fprintf(w, "PERF COMPARE (streaming)  %s: baseline=%s  candidate=%s  (Δ = candidate−baseline; negative is faster/tighter)\n", dim, base, cand)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var header []string
	if showBackend {
		header = append(header, "backend")
	}
	if namedAxes {
		header = append(header, cols...)
	} else if dim != "axis" {
		header = append(header, "axis")
	}
	if showRun {
		header = append(header, "run")
	}
	if showScenario {
		header = append(header, "scenario")
	}
	if showMode {
		header = append(header, "mode")
	}
	header = append(header,
		"first_resp_p95_base", "first_resp_p95_cand", "first_resp_p95_Δ",
		"maxgap_p95_base", "maxgap_p95_cand", "maxgap_p95_Δ",
		"finalspread_base", "finalspread_cand", "finalspread_Δ",
		"goodput_base", "goodput_cand")
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))

	for _, c := range cellOrder {
		k := residualKey(c, dim, crossRun)
		b, bok := baseByCell[k]
		n, nok := candByCell[k]
		var cells []string
		if showBackend {
			cells = append(cells, dash(c.Backend))
		}
		if namedAxes {
			for _, key := range cols {
				cells = append(cells, dash(c.Axes[key]))
			}
		} else if dim != "axis" {
			cells = append(cells, dash(c.Axis))
		}
		if showRun {
			cells = append(cells, dash(c.Run))
		}
		if showScenario {
			cells = append(cells, c.Scenario)
		}
		if showMode {
			cells = append(cells, dash(c.Mode))
		}
		bFirstResp, nFirstResp := ptrIf(bok, b.FirstRespP95Ns), ptrIf(nok, n.FirstRespP95Ns)
		bMaxGap, nMaxGap := ptrIf(bok, b.MaxStreamGapP95Ns), ptrIf(nok, n.MaxStreamGapP95Ns)
		bSpread, nSpread := ptrIf(bok, b.FinalChunkSpreadNs), ptrIf(nok, n.FinalChunkSpreadNs)
		cells = append(cells, absCompareTriplet(bFirstResp, nFirstResp)...)
		cells = append(cells, absCompareTriplet(bMaxGap, nMaxGap)...)
		cells = append(cells, absCompareTriplet(bSpread, nSpread)...)
		cells = append(cells, goodputCell(ptrIf(bok, b.GoodputBytesPerSec)), goodputCell(ptrIf(nok, n.GoodputBytesPerSec)))
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))

		if bok && nok {
			comparable := false
			// first_resp is informational only (warm RTT + injected
			// think time); the gate watches jitter and fairness.
			if abs, ok := absRegression(bMaxGap, nMaxGap); ok {
				gs.streamRegressions = append(gs.streamRegressions, streamRegression{cellLabel(c) + " max_stream_gap_p95", abs})
			}
			if bMaxGap != nil && nMaxGap != nil {
				comparable = true
			}
			if abs, ok := absRegression(bSpread, nSpread); ok {
				gs.streamRegressions = append(gs.streamRegressions, streamRegression{cellLabel(c) + " final_chunk_spread", abs})
			}
			if bSpread != nil && nSpread != nil {
				comparable = true
			}
			if comparable {
				gs.streamComparable++
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return gs, err
	}
	return gs, nil
}

// errNoDuplexRows signals that a comparison input carried no duplex
// rows at all, so the duplex comparison is a clean no-op (not a gate
// failure). The caller skips duplex when it sees this.
var errNoDuplexRows = errors.New("no duplex rows present")

// renderDuplexCompare matches duplex-family cells across two values of a
// dimension and prints the duplex metrics side by side. It populates the
// duplex* fields of gateStats: duplexPaired (cells matched on both
// sides), duplexComparable (paired cells with at least one gated metric
// present on both sides), and duplexRegressions (percent increases of
// the gated metrics rtt_p50 and rtt_p95).
//
// Duplex metrics are compared on percent deltas (same as rtt), because
// the duplex shape produces real measured timings that scale with
// backend latency rather than with an injected interval.
func renderDuplexCompare(w io.Writer, recs []record, dim, baseSel, candSel string) (gateStats, error) {
	var gs gateStats
	recs = filterFamily(recs, duplexFamily)
	if len(recs) == 0 {
		return gs, errNoDuplexRows
	}
	base, err := resolveDimValue(dim, baseSel, recs)
	if err != nil {
		return gs, fmt.Errorf("baseline %q: %w", baseSel, err)
	}
	cand, err := resolveDimValue(dim, candSel, recs)
	if err != nil {
		return gs, fmt.Errorf("candidate %q: %w", candSel, err)
	}
	if base == cand {
		return gs, fmt.Errorf("baseline and candidate resolve to the same %s %q", dim, base)
	}

	baseByCell := map[string]record{}
	candByCell := map[string]record{}
	cellOrder := []record{}
	seen := map[string]bool{}
	crossRun := false
	buildMaps := func(crossRun bool) {
		baseByCell = map[string]record{}
		candByCell = map[string]record{}
		cellOrder = []record{}
		seen = map[string]bool{}
		for _, r := range recs {
			v, ok := dimValue(r, dim)
			if !ok || (v != base && v != cand) {
				continue
			}
			k := residualKey(r, dim, crossRun)
			if v == base {
				putNewestByCell(baseByCell, k, r)
			} else {
				putNewestByCell(candByCell, k, r)
			}
			if !seen[k] {
				seen[k] = true
				cellOrder = append(cellOrder, r)
			}
		}
	}
	buildMaps(false)
	for k, br := range baseByCell {
		if _, ok := candByCell[k]; ok {
			gs.duplexPaired++
		} else {
			gs.missingInCand = append(gs.missingInCand, cellLabel(br))
		}
	}
	if gs.duplexPaired == 0 && dim != "run" {
		gs.missingInCand = nil
		buildMaps(true)
		for k, br := range baseByCell {
			if _, ok := candByCell[k]; ok {
				gs.duplexPaired++
			} else {
				gs.missingInCand = append(gs.missingInCand, cellLabel(br))
			}
		}
		if gs.duplexPaired > 0 {
			crossRun = true
			_, _ = fmt.Fprintf(w, "note: pairing duplex rows across runs — no within-run %s pairs found\n", dim)
		}
	}

	sortRecs(cellOrder)
	cols := axisColumns(cellOrder)
	namedAxes := len(cols) > 0
	if isAxisDim(dim) {
		cols = removeStr(cols, dim)
	}
	showBackend := dim != "backend"
	showScenario := dim != "scenario"
	showMode := dim != "mode"
	showRun := dim != "run" && len(distinctRuns(cellOrder)) > 1

	_, _ = fmt.Fprintf(w, "PERF COMPARE (duplex)  %s: baseline=%s  candidate=%s  (Δ%% = (candidate−baseline)/baseline; negative is faster)\n", dim, base, cand)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var header []string
	if showBackend {
		header = append(header, "backend")
	}
	if namedAxes {
		header = append(header, cols...)
	} else if dim != "axis" {
		header = append(header, "axis")
	}
	if showRun {
		header = append(header, "run")
	}
	if showScenario {
		header = append(header, "scenario")
	}
	if showMode {
		header = append(header, "mode")
	}
	header = append(header,
		"rtt_p50_base", "rtt_p50_cand", "rtt_p50_Δ%",
		"rtt_p95_base", "rtt_p95_cand", "rtt_p95_Δ%",
		"req_leg_p95_base", "req_leg_p95_cand", "req_leg_p95_Δ%",
		"resp_leg_p95_base", "resp_leg_p95_cand", "resp_leg_p95_Δ%")
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))

	for _, c := range cellOrder {
		k := residualKey(c, dim, crossRun)
		b, bok := baseByCell[k]
		n, nok := candByCell[k]
		var cells []string
		if showBackend {
			cells = append(cells, dash(c.Backend))
		}
		if namedAxes {
			for _, key := range cols {
				cells = append(cells, dash(c.Axes[key]))
			}
		} else if dim != "axis" {
			cells = append(cells, dash(c.Axis))
		}
		if showRun {
			cells = append(cells, dash(c.Run))
		}
		if showScenario {
			cells = append(cells, c.Scenario)
		}
		if showMode {
			cells = append(cells, dash(c.Mode))
		}
		cells = append(cells, compareTriplet(ptrIf(bok, b.RTTP50Ns), ptrIf(nok, n.RTTP50Ns))...)
		cells = append(cells, compareTriplet(ptrIf(bok, b.RTTP95Ns), ptrIf(nok, n.RTTP95Ns))...)
		cells = append(cells, compareTriplet(ptrIf(bok, b.ReqLegP95Ns), ptrIf(nok, n.ReqLegP95Ns))...)
		cells = append(cells, compareTriplet(ptrIf(bok, b.RespLegP95Ns), ptrIf(nok, n.RespLegP95Ns))...)
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))

		if bok && nok {
			comparable := false
			if b.RTTP50Ns != nil && n.RTTP50Ns != nil {
				comparable = true
				if pct, absNs, ok := regressionPct(b.RTTP50Ns, n.RTTP50Ns); ok {
					gs.duplexRegressions = append(gs.duplexRegressions, regression{cellLabel(c) + " rtt_p50", pct, absNs})
				}
			}
			if b.RTTP95Ns != nil && n.RTTP95Ns != nil {
				comparable = true
				if pct, absNs, ok := regressionPct(b.RTTP95Ns, n.RTTP95Ns); ok {
					gs.duplexRegressions = append(gs.duplexRegressions, regression{cellLabel(c) + " rtt_p95", pct, absNs})
				}
			}
			if comparable {
				gs.duplexComparable++
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return gs, err
	}
	return gs, nil
}

// absRegression returns the absolute positive increase of cand over base,
// with ok=true only when both are present and cand is actually larger (a
// tighter/faster candidate is not a regression).
func absRegression(base, cand *int64) (absNs int64, ok bool) {
	if base == nil || cand == nil {
		return 0, false
	}
	d := *cand - *base
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// absCompareTriplet renders base, candidate, and the signed absolute delta
// (as a duration) for one streaming metric.
func absCompareTriplet(base, cand *int64) []string {
	bc, cc := durOrDash(base), durOrDash(cand)
	if base == nil || cand == nil {
		return []string{bc, cc, "-"}
	}
	d := *cand - *base
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return []string{bc, cc, sign + dur(d)}
}

// gateVerdict is the pure decision behind applyGate: it returns the worst
// warm/cold p50 regression that breaches BOTH the percent threshold and
// the absolute floor (nil when none do), or an error when the comparison
// paired no cells (the gate must never silently pass on nothing).
func gateVerdict(gs gateStats, failOverPct float64, floor time.Duration) (*regression, error) {
	if gs.paired == 0 && gs.streamPaired == 0 && gs.duplexPaired == 0 {
		return nil, errors.New("no cells paired across baseline and candidate — nothing was compared")
	}
	var worst *regression
	for i := range gs.regressions {
		r := &gs.regressions[i]
		if r.pct > failOverPct && time.Duration(r.absNs) > floor {
			if worst == nil || r.pct > worst.pct {
				worst = r
			}
		}
	}
	return worst, nil
}

// streamGateFloorMin is the minimum absolute floor for the streaming gate.
// Streaming fairness/latency metrics are noisier than RTT p50s, so even
// when --fail-min-abs is set lower the streaming gate never trips below
// this, to keep CI from flaking on sub-perceptible jitter.
const streamGateFloorMin = 50 * time.Millisecond

// streamGateVerdict is the streaming-family analogue of gateVerdict. It
// returns the worst gated streaming regression (max_stream_gap_p95 or
// final_chunk_spread) whose absolute increase exceeds the floor, or an
// error when streaming cells paired but none yielded a comparable gated
// metric (a paired-but-uncomparable comparison must not silently pass).
// When no streaming cells paired at all it returns (nil, nil): the
// combined emptiness check lives in gateVerdict.
func streamGateVerdict(gs gateStats, floor time.Duration) (*streamRegression, error) {
	if gs.streamPaired == 0 {
		return nil, nil
	}
	if gs.streamComparable == 0 {
		return nil, errors.New("streaming cells paired but none carried a comparable gated metric (max_stream_gap_p95 / final_chunk_spread) on both sides — nothing was gated")
	}
	if floor < streamGateFloorMin {
		floor = streamGateFloorMin
	}
	var worst *streamRegression
	for i := range gs.streamRegressions {
		r := &gs.streamRegressions[i]
		if time.Duration(r.absNs) > floor {
			if worst == nil || r.absNs > worst.absNs {
				worst = r
			}
		}
	}
	return worst, nil
}

// duplexGateVerdict is the duplex-family analogue of gateVerdict. Duplex
// metrics are percent-gated (same shape as rtt) because they're real
// measured timings that scale with backend latency rather than an
// injected interval. Returns the worst gated duplex regression that
// breaches BOTH failOverPct and the floor; (nil, nil) when no duplex
// cells paired; an error when paired-but-uncomparable.
func duplexGateVerdict(gs gateStats, failOverPct float64, floor time.Duration) (*regression, error) {
	if gs.duplexPaired == 0 {
		return nil, nil
	}
	if gs.duplexComparable == 0 {
		return nil, errors.New("duplex cells paired but none carried a comparable gated metric (rtt_p50 / rtt_p95) on both sides — nothing was gated")
	}
	var worst *regression
	for i := range gs.duplexRegressions {
		r := &gs.duplexRegressions[i]
		if r.pct > failOverPct && time.Duration(r.absNs) > floor {
			if worst == nil || r.pct > worst.pct {
				worst = r
			}
		}
	}
	return worst, nil
}

// It exits non-zero when any paired warm/cold p50 cell regressed by more
// than failOverPct AND by more than the failMinAbs duration floor (so a
// large percent on a tiny baseline can't flake the gate). A comparison
// that paired no cells is a hard failure. Cells the candidate dropped are
// surfaced as a warning rather than a failure, because the measured
// scenario set legitimately evolves over time on main.
func applyGate(gs gateStats, failOverPct float64, failMinAbs string) {
	floor, err := time.ParseDuration(failMinAbs)
	if err != nil {
		fail(fmt.Errorf("--fail-min-abs %q: %w", failMinAbs, err))
	}
	if len(gs.missingInCand) > 0 {
		fmt.Fprintf(os.Stderr, "perf-gate: warning: %d baseline cell(s) absent from the candidate (scenario set drift?): %s\n",
			len(gs.missingInCand), strings.Join(gs.missingInCand, ", "))
	}
	worst, err := gateVerdict(gs, failOverPct, floor)
	if err != nil {
		fail(fmt.Errorf("perf-gate: %w", err))
	}
	streamWorst, err := streamGateVerdict(gs, floor)
	if err != nil {
		fail(fmt.Errorf("perf-gate: %w", err))
	}
	duplexWorst, err := duplexGateVerdict(gs, failOverPct, floor)
	if err != nil {
		fail(fmt.Errorf("perf-gate: %w", err))
	}
	failed := false
	if worst != nil {
		fmt.Fprintf(os.Stderr, "perf-gate: FAIL — %s regressed %+.1f%% (+%s), exceeding --fail-over %.1f%% / --fail-min-abs %s\n",
			worst.label, worst.pct, time.Duration(worst.absNs).Round(time.Millisecond), failOverPct, floor)
		failed = true
	}
	if streamWorst != nil {
		streamFloor := floor
		if streamFloor < streamGateFloorMin {
			streamFloor = streamGateFloorMin
		}
		fmt.Fprintf(os.Stderr, "perf-gate: FAIL — %s regressed +%s (absolute), exceeding streaming floor %s\n",
			streamWorst.label, time.Duration(streamWorst.absNs).Round(time.Millisecond), streamFloor)
		failed = true
	}
	if duplexWorst != nil {
		fmt.Fprintf(os.Stderr, "perf-gate: FAIL — %s regressed %+.1f%% (+%s), exceeding --fail-over %.1f%% / --fail-min-abs %s\n",
			duplexWorst.label, duplexWorst.pct, time.Duration(duplexWorst.absNs).Round(time.Millisecond), failOverPct, floor)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	if gs.paired > 0 {
		fmt.Fprintf(os.Stderr, "perf-gate: OK — no warm/cold p50 regression beyond %.1f%% / %s across %d paired cell(s)\n",
			failOverPct, floor, gs.paired)
	}
	if gs.streamPaired > 0 {
		streamFloor := floor
		if streamFloor < streamGateFloorMin {
			streamFloor = streamGateFloorMin
		}
		fmt.Fprintf(os.Stderr, "perf-gate: OK — no streaming max_stream_gap_p95/final_chunk_spread regression beyond %s across %d paired stream cell(s)\n",
			streamFloor, gs.streamPaired)
	}
}

// removeStr returns ss without the first occurrence of v.
func removeStr(ss []string, v string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == v {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ptrIf returns p only when present is true, so a cell missing on one side
// of a comparison renders as a dash rather than borrowing the other side.
func ptrIf(present bool, p *int64) *int64 {
	if !present {
		return nil
	}
	return p
}

// estNs is the establishment estimate (cold_p50 − warm_p50) in ns, or nil
// when present is false or either component is unmeasured.
func estNs(r record, present bool) *int64 {
	if !present || r.ColdP50Ns == nil || r.WarmP50Ns == nil {
		return nil
	}
	v := *r.ColdP50Ns - *r.WarmP50Ns
	return &v
}

// compareTriplet renders base, candidate, and percent delta for one metric.
func compareTriplet(base, cand *int64) []string {
	bc, cc := durOrDash(base), durOrDash(cand)
	if base == nil || cand == nil || *base == 0 {
		return []string{bc, cc, "-"}
	}
	pct := float64(*cand-*base) / float64(*base) * 100
	return []string{bc, cc, fmt.Sprintf("%+.1f%%", pct)}
}

// warnMixedAxisKeys prints one warning when the named-axis key sets differ
// across rows (including legacy rows that carry no named axes), because a
// pivot/filter on a key that some rows lack will silently exclude them.
func warnMixedAxisKeys(w io.Writer, recs []record, cols []string) {
	if len(cols) == 0 {
		return
	}
	for _, r := range recs {
		for _, k := range cols {
			if _, ok := r.Axes[k]; !ok {
				_, _ = fmt.Fprintf(w, "warning: rows carry differing axis dimensions (some lack %q) — filters/pivots on it will skip them\n", k)
				return
			}
		}
	}
}

// placementCodes maps the single-letter distance codes used in placement
// axis names to display labels, in near→far order.
var placementCodes = []struct {
	code, label string
}{
	{"n", "near"},
	{"m", "mid"},
	{"f", "far"},
}

// renderGrid pivots the placement axis into a 3x3 of the chosen metric.
// It requires the (filtered) records to share exactly one backend, one
// scenario, and one mode — otherwise the grid would conflate distinct
// measurements — and every axis to be a two-letter placement code
// <sender><listener>.
func renderGrid(w io.Writer, recs []record, metric string) error {
	value, ok := metricFn(metric)
	if !ok {
		return fmt.Errorf("unknown --metric %q (want all|est|warm|cold)", metric)
	}

	// The grid pivots the placement axis, so first keep only rows that name
	// a placement cell. A merged history can hold non-placement runs (a
	// plain perf-mock row with no backend tag, an azure auth cell) that
	// share this scenario/mode; those are skipped, not fatal. A genuinely
	// ambiguous axis (two placement-shaped segments) is still fatal. The
	// single-backend/scenario/mode check runs on the surviving placement
	// rows so a stray non-placement row cannot abort the grid before it.
	var placement []record
	skipped := 0
	for _, r := range recs {
		if _, _, err := parsePlacement(r.Axis); err != nil {
			if errors.Is(err, errNotPlacement) {
				skipped++
				continue
			}
			return err
		}
		placement = append(placement, r)
	}
	if len(placement) == 0 {
		if len(recs) == 0 {
			return errors.New("no rows selected for the grid (check --scenario/--mode/--filter)")
		}
		return fmt.Errorf("no placement grid cells among the selected rows "+
			"(scenario=%s mode=%s); none of %d row(s) carry a two-letter "+
			"<sender><listener> code — run 'make perf-placement' or pick a swept scenario",
			dash(recs[0].Scenario), dash(recs[0].Mode), len(recs))
	}
	if err := requireSingle(placement, func(r record) string { return r.Backend }, "backend"); err != nil {
		return err
	}
	if err := requireSingle(placement, func(r record) string { return r.Scenario }, "scenario"); err != nil {
		return err
	}
	if err := requireSingle(placement, func(r record) string { return r.Mode }, "mode"); err != nil {
		return err
	}

	cell := map[string]string{} // "sender,listener" -> formatted value
	for _, r := range placement {
		s, l, _ := parsePlacement(r.Axis) // already validated above
		coord := s + "," + l
		// Two rows landing on the same coordinate means placement is not
		// the only varying dimension (e.g. a perf-placement "nf" row and an
		// unpinned perf-axes-mock "sas/nf" row both map to n,f). Refuse
		// rather than silently render whichever was iterated last.
		if _, dup := cell[coord]; dup {
			return fmt.Errorf("multiple rows map to placement cell %s%s — "+
				"placement is not the only varying dimension; narrow with "+
				"--filter <axis>=<value> or compare a single run", s, l)
		}
		cell[coord] = value(r)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "note: skipped %d non-placement row(s) outside the grid\n", skipped)
	}

	cellLegend := "ms"
	if metric == "all" {
		cellLegend = "cell=cold/warm/est ms"
	}
	_, _ = fmt.Fprintf(w, "PERF MATRIX GRID  backend=%s  metric=%s  scenario=%s  mode=%s  (rows=sender, cols=listener; %s)\n",
		dash(placement[0].Backend), metric, placement[0].Scenario, dash(placement[0].Mode), cellLegend)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "sender\\listener"
	for _, c := range placementCodes {
		header += "\t" + c.label
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, s := range placementCodes {
		row := s.label
		for _, l := range placementCodes {
			v := cell[s.code+","+l.code]
			if v == "" {
				v = "-"
			}
			row += "\t" + v
		}
		_, _ = fmt.Fprintln(tw, row)
	}
	return tw.Flush()
}

// metricFn returns a formatter for the chosen grid metric. est is the
// derived cold−warm establishment cost, computed here (never stored);
// warm/cold are the corresponding p50s. A "-" results when the needed
// measurement is absent.
func metricFn(metric string) (func(record) string, bool) {
	switch metric {
	case "all":
		return func(r record) string {
			cold := msPtr(r.ColdP50Ns)
			warm := msPtr(r.WarmP50Ns)
			est := "-"
			if r.ColdP50Ns != nil && r.WarmP50Ns != nil {
				est = ms(*r.ColdP50Ns - *r.WarmP50Ns)
			}
			return cold + "/" + warm + "/" + est
		}, true
	case "est":
		return func(r record) string {
			if r.ColdP50Ns == nil || r.WarmP50Ns == nil {
				return "-"
			}
			return ms(*r.ColdP50Ns - *r.WarmP50Ns)
		}, true
	case "warm":
		return func(r record) string { return msPtr(r.WarmP50Ns) }, true
	case "cold":
		return func(r record) string { return msPtr(r.ColdP50Ns) }, true
	default:
		return nil, false
	}
}

func requireSingle(recs []record, key func(record) string, name string) error {
	set := map[string]bool{}
	for _, r := range recs {
		set[key(r)] = true
	}
	if len(set) == 1 {
		return nil
	}
	vals := make([]string, 0, len(set))
	for v := range set {
		if v == "" {
			v = "(none)"
		}
		vals = append(vals, v)
	}
	sort.Strings(vals)
	return fmt.Errorf("grid needs exactly one %s but artifact has %d (%s); "+
		"narrow with --%s", name, len(set), strings.Join(vals, ", "), name)
}

// parsePlacement extracts the sender/listener distance codes from a
// placement axis. The placement code is the unique `/`-separated segment
// shaped as two letters drawn from n/m/f — found by scanning ALL segments
// rather than assuming a fixed position, so it stays correct if a backend
// reorders or inserts axis dimensions around the placement one. Zero
// matches means the axis is not a placement grid; more than one match is
// ambiguous — both fail loudly rather than pivoting on the wrong segment.
// errNotPlacement marks an axis that carries no placement-shaped segment,
// so callers (e.g. the grid over a merged history) can skip such a row
// instead of treating it as a hard error.
var errNotPlacement = errors.New("not a placement grid cell")

func parsePlacement(axis string) (sender, listener string, err error) {
	var matches []string
	for _, seg := range strings.Split(axis, "/") {
		if len(seg) == 2 && isCode(seg[0]) && isCode(seg[1]) {
			matches = append(matches, seg)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0][0:1], matches[0][1:2], nil
	case 0:
		return "", "", fmt.Errorf("%w: axis %q "+
			"(want a two-letter <sender><listener> code from n/m/f)", errNotPlacement, axis)
	default:
		return "", "", fmt.Errorf("axis %q has multiple placement-shaped segments %v; "+
			"cannot tell which is the placement dimension", axis, matches)
	}
}

func isCode(b byte) bool { return b == 'n' || b == 'm' || b == 'f' }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func durOrDash(ns *int64) string {
	if ns == nil {
		return "-"
	}
	return dur(*ns)
}

func estCell(r record) string {
	if r.ColdP50Ns == nil || r.WarmP50Ns == nil {
		return "-"
	}
	return dur(*r.ColdP50Ns - *r.WarmP50Ns)
}

// dur renders a nanosecond count as a duration with the same 100µs
// rounding the harness table uses, so reporter and inline outputs match.
func dur(ns int64) string {
	d := time.Duration(ns)
	if d >= time.Millisecond {
		return d.Round(100 * time.Microsecond).String()
	}
	return d.Round(time.Microsecond).String()
}

// ms renders a nanosecond count as a millisecond float with one decimal,
// the natural unit for the placement grid.
func ms(ns int64) string {
	return fmt.Sprintf("%.1f", float64(ns)/float64(time.Millisecond))
}

func msPtr(ns *int64) string {
	if ns == nil {
		return "-"
	}
	return ms(*ns)
}
