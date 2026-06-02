package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	rowNF    = `{"type":"row","schema":"perfmatrix/v1","backend":"mock","axis":"nf","scenario":"S","mode":"PortForward","cold_p50_ns":1191700000,"warm_p50_ns":192600000,"warm_p95_ns":193500000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2155600000}`
	rowFN    = `{"type":"row","schema":"perfmatrix/v1","backend":"mock","axis":"fn","scenario":"S","mode":"PortForward","cold_p50_ns":1104900000,"warm_p50_ns":192600000,"warm_p95_ns":193800000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2069300000}`
	rowAzure = `{"type":"row","schema":"perfmatrix/v1","backend":"azure","axis":"sas","scenario":"S","mode":"PortForward","cold_p50_ns":1450000000,"warm_p50_ns":210000000,"warm_p95_ns":215000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2500000000}`
)

func TestLoad_SkipsBlankAndNonRow(t *testing.T) {
	p := writeTemp(t, rowNF, "", `{"type":"meta","schema":"perfmatrix/v1"}`, rowFN)
	recs, _, err := load([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d rows, want 2 (blank + meta skipped)", len(recs))
	}
}

func TestLoad_DuplicateIsError(t *testing.T) {
	p := writeTemp(t, rowNF, rowNF)
	if _, _, err := load([]string{p}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestLoad_MalformedIsError(t *testing.T) {
	p := writeTemp(t, "{not json}")
	if _, _, err := load([]string{p}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want malformed error, got %v", err)
	}
}

func TestRenderGrid_Est(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowNF, rowFN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderGrid(&b, recs, "est"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// nf est = 1191.7-192.6 = 999.1; fn est = 1104.9-192.6 = 912.3.
	for _, want := range []string{"999.1", "912.3", "metric=est", "mode=PortForward"} {
		if !strings.Contains(out, want) {
			t.Errorf("grid output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderGrid_AmbiguousModeIsError(t *testing.T) {
	socks := strings.Replace(rowNF, "PortForward", "SOCKS5", 1)
	recs, _, err := load([]string{writeTemp(t, rowNF, socks)})
	if err != nil {
		t.Fatal(err)
	}
	err = renderGrid(&strings.Builder{}, recs, "est")
	if err == nil || !strings.Contains(err.Error(), "exactly one mode") {
		t.Fatalf("want ambiguous-mode error, got %v", err)
	}
}

func TestRenderGrid_NonPlacementAxisIsError(t *testing.T) {
	row := strings.Replace(rowNF, `"axis":"nf"`, `"axis":"entra/far"`, 1)
	recs, _, err := load([]string{writeTemp(t, row)})
	if err != nil {
		t.Fatal(err)
	}
	err = renderGrid(&strings.Builder{}, recs, "est")
	if err == nil || !strings.Contains(err.Error(), "placement grid cell") {
		t.Fatalf("want placement-parse error, got %v", err)
	}
}

func TestRenderGrid_SkipsNonPlacementRowsInMergedHistory(t *testing.T) {
	// A non-placement run (e.g. a plain perf-mock row) can share the
	// scenario and mode in the unified history; the grid builds from the
	// placement rows and skips the stray one instead of erroring.
	stray := strings.Replace(rowNF, `"axis":"nf"`, `"axis":"entra"`, 1)
	recs, _, err := load([]string{writeTemp(t, rowNF, rowFN, stray)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderGrid(&b, recs, "est"); err != nil {
		t.Fatalf("grid should tolerate a non-placement row: %v", err)
	}
	out := b.String()
	for _, want := range []string{"999.1", "912.3"} { // nf and fn cells still render
		if !strings.Contains(out, want) {
			t.Errorf("grid missing placement cell %q:\n%s", want, out)
		}
	}
}

func TestRenderGrid_AmbiguousAxisStaysFatal(t *testing.T) {
	// Skipping non-placement rows must NOT swallow a genuinely ambiguous
	// axis (two placement-shaped segments), even alongside a valid row.
	ambig := strings.Replace(rowNF, `"axis":"nf"`, `"axis":"nf/ff"`, 1)
	recs, _, err := load([]string{writeTemp(t, rowFN, ambig)})
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGrid(&strings.Builder{}, recs, "est"); err == nil || !strings.Contains(err.Error(), "multiple placement-shaped") {
		t.Fatalf("ambiguous axis should stay fatal, got %v", err)
	}
}

func TestRenderGrid_DuplicatePlacementCellIsError(t *testing.T) {
	// A perf-placement row (axis "nf") and an unpinned perf-axes-mock row
	// (axis "sas/nf") both map to cell n,f but differ on the auth axis;
	// the grid must reject the ambiguity rather than silently pick one.
	axesRow := strings.Replace(rowNF, `"axis":"nf"`, `"axis":"sas/nf"`, 1)
	recs, _, err := load([]string{writeTemp(t, rowNF, axesRow)})
	if err != nil {
		t.Fatal(err)
	}
	if err := renderGrid(&strings.Builder{}, recs, "est"); err == nil || !strings.Contains(err.Error(), "placement cell") {
		t.Fatalf("want duplicate-cell error, got %v", err)
	}
}

func TestRenderGrid_SkipsBackendlessNonPlacementRow(t *testing.T) {
	// A plain perf-mock row carries no backend tag and a non-placement
	// axis; it must be skipped before the single-backend check, not abort
	// it (the grid still renders from the real placement rows).
	stray := strings.Replace(rowNF, `"backend":"mock","axis":"nf"`, `"backend":"","axis":""`, 1)
	recs, _, err := load([]string{writeTemp(t, rowNF, rowFN, stray)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderGrid(&b, recs, "est"); err != nil {
		t.Fatalf("backend-less non-placement row should be skipped, got %v", err)
	}
	for _, want := range []string{"999.1", "912.3"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("grid missing placement cell %q:\n%s", want, b.String())
		}
	}
}

func TestParsePlacement(t *testing.T) {
	cases := map[string]struct {
		s, l string
		ok   bool
	}{
		"nf":      {"n", "f", true},
		"fn":      {"f", "n", true},
		"sas/ff":  {"f", "f", true},
		"sas/nf":  {"n", "f", true},
		"nf/sas":  {"n", "f", true},
		"nn":      {"n", "n", true},
		"xy":      {"", "", false},
		"n":       {"", "", false},
		"near":    {"", "", false},
		"entra/x": {"", "", false},
		"nf/ff":   {"", "", false}, // two placement-shaped segments → ambiguous
	}
	for axis, want := range cases {
		s, l, err := parsePlacement(axis)
		if want.ok && (err != nil || s != want.s || l != want.l) {
			t.Errorf("parsePlacement(%q) = (%q,%q,%v), want (%q,%q,nil)", axis, s, l, err, want.s, want.l)
		}
		if !want.ok && err == nil {
			t.Errorf("parsePlacement(%q) = nil err, want error", axis)
		}
	}
}

func TestRenderGrid_CompositeAll(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowNF, rowFN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderGrid(&b, recs, "all"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// nf cell = cold/warm/est = 1191.7/192.6/999.1 in one cell.
	for _, want := range []string{"1191.7/192.6/999.1", "1104.9/192.6/912.3", "cell=cold/warm/est", "metric=all"} {
		if !strings.Contains(out, want) {
			t.Errorf("composite grid missing %q:\n%s", want, out)
		}
	}
}

func TestMetricFn_AllNilColdIsDashEst(t *testing.T) {
	fn, ok := metricFn("all")
	if !ok {
		t.Fatal("all metric not found")
	}
	warm := int64(192600000)
	// cold absent: cold and est show "-", warm still renders.
	if got := fn(record{WarmP50Ns: &warm}); got != "-/192.6/-" {
		t.Errorf("all with nil cold = %q, want -/192.6/-", got)
	}
}

func TestLoad_BadSchemaIsError(t *testing.T) {
	row := strings.Replace(rowNF, "perfmatrix/v1", "perfmatrix/v2", 1)
	p := writeTemp(t, row)
	if _, _, err := load([]string{p}); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("want schema error, got %v", err)
	}
}

func TestLoad_MergesBackendFiles(t *testing.T) {
	mock := writeTemp(t, rowNF, rowFN)
	azure := writeTemp(t, rowAzure)
	recs, _, err := load([]string{mock, azure})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("merged %d rows, want 3 (2 mock + 1 azure)", len(recs))
	}
	var b strings.Builder
	if err := renderTable(&b, recs); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"backend", "mock", "azure"} {
		if !strings.Contains(out, want) {
			t.Errorf("merged table missing %q:\n%s", want, out)
		}
	}
}

func TestRenderGrid_MultiBackendIsError(t *testing.T) {
	// Two placement rows on different backends must trip the single-backend
	// check (a non-placement azure auth row would instead be skipped).
	azureNF := strings.Replace(rowNF, `"backend":"mock"`, `"backend":"azure"`, 1)
	recs, _, err := load([]string{writeTemp(t, rowNF, azureNF)})
	if err != nil {
		t.Fatal(err)
	}
	err = renderGrid(&strings.Builder{}, recs, "est")
	if err == nil || !strings.Contains(err.Error(), "exactly one backend") {
		t.Fatalf("want ambiguous-backend error, got %v", err)
	}
}

func TestRenderGrid_EmptyRecordsIsErrorNotPanic(t *testing.T) {
	if err := renderGrid(&strings.Builder{}, nil, "est"); err == nil {
		t.Fatal("want error for empty record set, got nil")
	}
}

func TestSingleCell(t *testing.T) {
	one := []record{
		{Scenario: "A", Mode: "PortForward", Axis: "nn"},
		{Scenario: "A", Mode: "PortForward", Axis: "ff"},
	}
	if !singleCell(one) {
		t.Error("singleCell = false for one scenario+mode, want true (grid-able)")
	}
	twoScenarios := append(append([]record{}, one...), record{Scenario: "B", Mode: "PortForward", Axis: "nn"})
	if singleCell(twoScenarios) {
		t.Error("singleCell = true for two scenarios, want false (table fallback)")
	}
	twoModes := append(append([]record{}, one...), record{Scenario: "A", Mode: "SOCKS5", Axis: "nn"})
	if singleCell(twoModes) {
		t.Error("singleCell = true for two modes, want false (table fallback)")
	}
}

func TestMetricFn_EstNilIsDash(t *testing.T) {
	fn, ok := metricFn("est")
	if !ok {
		t.Fatal("est metric not found")
	}
	warm := int64(5)
	// cold absent (e.g. a prewarmed-only row): est must be "-", never a bogus value.
	if got := fn(record{WarmP50Ns: &warm}); got != "-" {
		t.Errorf("est with nil cold = %q, want -", got)
	}
}

const (
	runMock  = `{"type":"run","schema":"perfmatrix/v1","backend":"mock","generated_at":"2026-06-01T10:00:00Z","git_sha":"abc1234"}`
	runAzure = `{"type":"run","schema":"perfmatrix/v1","backend":"azure","generated_at":"2026-06-01T10:05:00Z","git_sha":"abc1234"}`
	runStale = `{"type":"run","schema":"perfmatrix/v1","backend":"azure","generated_at":"2026-05-20T08:00:00Z","git_sha":"deadbee"}`
)

func TestLoad_CollectsRunRecords(t *testing.T) {
	_, runs, err := load([]string{writeTemp(t, runMock, rowNF, rowFN)})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Backend != "mock" || runs[0].GitSHA != "abc1234" {
		t.Fatalf("got runs %+v, want one mock@abc1234", runs)
	}
}

func TestRenderProvenance_SingleBuildNoWarning(t *testing.T) {
	mock := writeTemp(t, runMock, rowNF, rowFN)
	azure := writeTemp(t, runAzure, rowAzure)
	recs, runs, err := load([]string{mock, azure})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	renderProvenance(&b, relevantRuns(runs, recs))
	out := b.String()
	for _, want := range []string{"sources:", "mock@abc1234", "azure@abc1234"} {
		if !strings.Contains(out, want) {
			t.Errorf("provenance missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning") {
		t.Errorf("same-sha sources should not warn:\n%s", out)
	}
}

func TestRenderProvenance_MultiBuildWarns(t *testing.T) {
	recs, runs, err := load([]string{writeTemp(t, runMock, rowNF, rowFN), writeTemp(t, runStale, rowAzure)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	renderProvenance(&b, relevantRuns(runs, recs))
	if out := b.String(); !strings.Contains(out, "warning") {
		t.Errorf("diverging shas should warn:\n%s", out)
	}
}

func TestRelevantRuns_FiltersByPresentBackend(t *testing.T) {
	recs, runs, err := load([]string{writeTemp(t, runMock, rowNF), writeTemp(t, runAzure, rowAzure)})
	if err != nil {
		t.Fatal(err)
	}
	// Filter rows to mock only; provenance should drop the azure source.
	rel := relevantRuns(runs, applyFilters(recs, map[string]string{"backend": "mock"}))
	if len(rel) != 1 || rel[0].Backend != "mock" {
		t.Fatalf("relevantRuns = %+v, want only mock", rel)
	}
}

const (
	rowAuthSasNN   = `{"type":"row","schema":"perfmatrix/v1","backend":"mock","axis":"sas/nn","axes":{"auth":"sas","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":255000000,"warm_p50_ns":22000000,"warm_p95_ns":22500000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":366000000}`
	rowAuthEntraNN = `{"type":"row","schema":"perfmatrix/v1","backend":"mock","axis":"entra/nn","axes":{"auth":"entra","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":256000000,"warm_p50_ns":22100000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":367000000}`
	rowAuthSasFF   = `{"type":"row","schema":"perfmatrix/v1","backend":"mock","axis":"sas/ff","axes":{"auth":"sas","delay":"ff"},"scenario":"S","mode":"PortForward","cold_p50_ns":1950000000,"warm_p50_ns":362000000,"warm_p95_ns":363000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":3770000000}`
)

func TestRenderTable_NamedAxisColumns(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowAuthSasNN, rowAuthEntraNN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderTable(&b, recs); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// auth and delay must be their own header columns, not a flat "axis".
	hdr := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "backend") {
			hdr = line
			break
		}
	}
	for _, want := range []string{"auth", "delay"} {
		if !strings.Contains(hdr, want) {
			t.Errorf("header %q missing column %q", hdr, want)
		}
	}
	if strings.Contains(hdr, "axis ") || strings.HasSuffix(strings.TrimSpace(hdr), "axis") {
		t.Errorf("named-axis table should not have a flat 'axis' column: %q", hdr)
	}
}

func TestApplyFilters_AxisKey(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowAuthSasNN, rowAuthEntraNN, rowAuthSasFF)})
	if err != nil {
		t.Fatal(err)
	}
	got := applyFilters(recs, map[string]string{"delay": "nn"})
	if len(got) != 2 {
		t.Fatalf("delay=nn filter got %d rows, want 2", len(got))
	}
	got = applyFilters(recs, map[string]string{"auth": "sas", "delay": "ff"})
	if len(got) != 1 || got[0].Axes["delay"] != "ff" {
		t.Fatalf("auth=sas,delay=ff filter got %#v, want one ff row", got)
	}
	got = applyFilters(recs, map[string]string{"backend": "mock"})
	if len(got) != 3 {
		t.Fatalf("backend=mock got %d, want 3 (virtual key)", len(got))
	}
}

func TestLoad_DedupUsesNamedAxes(t *testing.T) {
	// Same scenario+mode+backend but different named axes must NOT collide.
	if _, _, err := load([]string{writeTemp(t, rowAuthSasNN, rowAuthEntraNN)}); err != nil {
		t.Fatalf("distinct named axes treated as duplicate: %v", err)
	}
	// Identical rows DO collide.
	if _, _, err := load([]string{writeTemp(t, rowAuthSasNN, rowAuthSasNN)}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error for identical named-axis rows, got %v", err)
	}
}

func TestWarnMixedAxisKeys(t *testing.T) {
	// rowNF/rowFN carry no named axes; mixing them with named-axis rows
	// must surface a warning so a pivot doesn't silently drop them.
	recs, _, err := load([]string{writeTemp(t, rowAuthSasNN, rowNF)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderTable(&b, recs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "differing axis dimensions") {
		t.Errorf("expected mixed-axes warning, got:\n%s", b.String())
	}
}

// --- multi-run / comparison ---------------------------------------------

const (
	rowRunOld = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T100000.000Z-aaaa","backend":"mock","axes":{"auth":"sas","delay":"ff"},"scenario":"S","mode":"PortForward","cold_p50_ns":2000000000,"warm_p50_ns":400000000,"warm_p95_ns":410000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2100000000}`
	rowRunNew = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T120000.000Z-bbbb","backend":"mock","axes":{"auth":"sas","delay":"ff"},"scenario":"S","mode":"PortForward","cold_p50_ns":1600000000,"warm_p50_ns":380000000,"warm_p95_ns":390000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":1700000000}`
)

func TestLoad_SameCellDistinctRunsCoexist(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowRunOld, rowRunNew)})
	if err != nil {
		t.Fatalf("two runs of one cell should not collide: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d rows, want 2", len(recs))
	}
}

func TestLoad_SameRunSameCellStillDuplicate(t *testing.T) {
	p := writeTemp(t, rowRunOld, rowRunOld)
	if _, _, err := load([]string{p}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error within one run, got %v", err)
	}
}

func TestLatestPerCell_KeepsNewestRun(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowRunOld, rowRunNew)})
	if err != nil {
		t.Fatal(err)
	}
	got := latestPerCell(recs)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 collapsed", len(got))
	}
	if got[0].Run != "20260601T120000.000Z-bbbb" {
		t.Errorf("kept run %q, want the newer bbbb", got[0].Run)
	}
}

func TestResolveRun(t *testing.T) {
	runs := []string{"20260601T120000.000Z-bbbb", "20260601T100000.000Z-aaaa"} // newest first
	cases := []struct {
		sel     string
		want    string
		wantErr bool
	}{
		{"latest", "20260601T120000.000Z-bbbb", false},
		{"previous", "20260601T100000.000Z-aaaa", false},
		{"20260601T100000.000Z-aaaa", "20260601T100000.000Z-aaaa", false}, // exact
		{"20260601T12", "20260601T120000.000Z-bbbb", false},               // unique prefix
		{"20260601T1", "", true},                                          // ambiguous prefix
		{"nope", "", true},                                                // no match
	}
	for _, tc := range cases {
		got, err := resolveRun(tc.sel, runs)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %q", tc.sel, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got (%q,%v), want (%q,nil)", tc.sel, got, err, tc.want)
		}
	}
}

func TestRenderCompare_Deltas(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowRunOld, rowRunNew)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := renderCompare(&b, recs, "run", "previous", "latest"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// warm: (380-400)/400 = -5.0%; cold: (1600-2000)/2000 = -20.0%;
	// est: base 1600, cand 1220 -> (1220-1600)/1600 = -23.8%.
	for _, want := range []string{"-5.0%", "-20.0%", "-23.8%", "baseline=20260601T100000.000Z-aaaa"} {
		if !strings.Contains(out, want) {
			t.Errorf("compare output missing %q:\n%s", want, out)
		}
	}
}

// gateCandRegress is newer than rowRunOld and SLOWER: warm 400ms->500ms
// (+25%, +100ms), cold 2000ms->2200ms (+10%, +200ms).
const gateCandRegress = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T140000.000Z-cccc","backend":"mock","axes":{"auth":"sas","delay":"ff"},"scenario":"S","mode":"PortForward","cold_p50_ns":2200000000,"warm_p50_ns":500000000,"warm_p95_ns":510000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2700000000}`

// gateTinyBase/gateTinyCand share a cell whose warm doubles (1ms->2ms):
// +100% but only +1ms absolute, to exercise the --fail-min-abs floor.
const gateTinyBase = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T100000.000Z-aaaa","backend":"mock","axes":{"auth":"sas","delay":"ff"},"scenario":"S","mode":"PortForward","cold_p50_ns":1000000,"warm_p50_ns":1000000,"warm_p95_ns":1100000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2000000}`
const gateTinyCand = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T140000.000Z-cccc","backend":"mock","axes":{"auth":"sas","delay":"ff"},"scenario":"S","mode":"PortForward","cold_p50_ns":1000000,"warm_p50_ns":2000000,"warm_p95_ns":2100000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2000000}`

func gateStatsFor(t *testing.T, rows ...string) gateStats {
	t.Helper()
	recs, _, err := load([]string{writeTemp(t, rows...)})
	if err != nil {
		t.Fatal(err)
	}
	gs, err := renderCompare(&strings.Builder{}, recs, "run", "previous", "latest")
	if err != nil {
		t.Fatalf("renderCompare: %v", err)
	}
	return gs
}

func TestGate_CollectsWarmAndColdRegressions(t *testing.T) {
	gs := gateStatsFor(t, rowRunOld, gateCandRegress)
	if gs.paired != 1 {
		t.Fatalf("paired = %d, want 1", gs.paired)
	}
	if len(gs.regressions) != 2 {
		t.Fatalf("regressions = %d, want 2 (warm+cold):\n%+v", len(gs.regressions), gs.regressions)
	}
	var warm, cold *regression
	for i := range gs.regressions {
		switch {
		case strings.HasSuffix(gs.regressions[i].label, "warm_p50"):
			warm = &gs.regressions[i]
		case strings.HasSuffix(gs.regressions[i].label, "cold_p50"):
			cold = &gs.regressions[i]
		}
	}
	if warm == nil || cold == nil {
		t.Fatalf("missing warm/cold regression: %+v", gs.regressions)
	}
	if got := warm.pct; got < 24.9 || got > 25.1 {
		t.Errorf("warm pct = %.2f, want ~25", got)
	}
	if warm.absNs != 100000000 {
		t.Errorf("warm absNs = %d, want 100000000", warm.absNs)
	}
}

func TestGateVerdict_TripsAboveThreshold(t *testing.T) {
	gs := gateStatsFor(t, rowRunOld, gateCandRegress)
	// warm +25% > 20% and +100ms > 50ms floor; cold +10% does not.
	worst, err := gateVerdict(gs, 20, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if worst == nil {
		t.Fatal("want a tripping regression, got nil")
	}
	if !strings.HasSuffix(worst.label, "warm_p50") {
		t.Errorf("worst = %q, want the warm_p50 cell", worst.label)
	}
}

func TestGateVerdict_PassesWithinThreshold(t *testing.T) {
	gs := gateStatsFor(t, rowRunOld, gateCandRegress)
	// Both warm (+25%) and cold (+10%) are under a 30% gate.
	worst, err := gateVerdict(gs, 30, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if worst != nil {
		t.Errorf("want pass, got tripping cell %q (%.1f%%)", worst.label, worst.pct)
	}
}

func TestGateVerdict_AbsFloorSuppressesTinyBaseline(t *testing.T) {
	gs := gateStatsFor(t, gateTinyBase, gateTinyCand)
	// warm doubled (+100%) but only +1ms; a 50ms floor must suppress it.
	worst, err := gateVerdict(gs, 20, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if worst != nil {
		t.Errorf("want floor to suppress, got tripping cell %q (+%dns)", worst.label, worst.absNs)
	}
}

func TestGateVerdict_IgnoresImprovement(t *testing.T) {
	gs := gateStatsFor(t, rowRunOld, rowRunNew) // candidate is faster
	if len(gs.regressions) != 0 {
		t.Errorf("an improvement produced regressions: %+v", gs.regressions)
	}
	worst, err := gateVerdict(gs, 1, time.Nanosecond)
	if err != nil || worst != nil {
		t.Errorf("want clean pass, got worst=%v err=%v", worst, err)
	}
}

func TestGateVerdict_NoPairedCellsIsError(t *testing.T) {
	if _, err := gateVerdict(gateStats{}, 20, 50*time.Millisecond); err == nil {
		t.Fatal("want error when nothing paired, got nil")
	}
}

func TestGate_DroppedCandidateCellIsTrackedNotPaired(t *testing.T) {
	candOther := strings.Replace(gateCandRegress, `"scenario":"S"`, `"scenario":"OtherScenario"`, 1)
	gs := gateStatsFor(t, rowRunOld, candOther)
	if gs.paired != 0 {
		t.Errorf("paired = %d, want 0 (scenario differs)", gs.paired)
	}
	if len(gs.missingInCand) != 1 {
		t.Errorf("missingInCand = %v, want one entry", gs.missingInCand)
	}
	if _, err := gateVerdict(gs, 20, 50*time.Millisecond); err == nil {
		t.Error("want gateVerdict error when nothing paired")
	}
}

func TestGate_SingleRunIsNotEnoughRuns(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowRunOld)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = renderCompare(&strings.Builder{}, recs, "run", "previous", "latest")
	if !errors.Is(err, errNotEnoughRuns) {
		t.Fatalf("want errNotEnoughRuns, got %v", err)
	}
}

func TestRenderCompare_MissingCellIsDash(t *testing.T) {
	// candidate run lacks the cell present in baseline.
	candOther := strings.Replace(rowRunNew, `"S"`, `"OtherScenario"`, 1)
	recs, _, err := load([]string{writeTemp(t, rowRunOld, candOther)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := renderCompare(&b, recs, "run", "previous", "latest"); err != nil {
		t.Fatal(err)
	}
	// Each cell appears on exactly one side, so every delta is a dash.
	if !strings.Contains(b.String(), "-\t") && !strings.Contains(b.String(), " - ") {
		t.Errorf("expected dashes for one-sided cells:\n%s", b.String())
	}
}

func TestRenderTable_RunColumnWhenMultipleRuns(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowRunOld, rowRunNew)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderTable(&b, recs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "run") || !strings.Contains(b.String(), "aaaa") {
		t.Errorf("expected a run column listing both runs:\n%s", b.String())
	}
}

func TestApplyFilters_RunKey(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowRunOld, rowRunNew)})
	if err != nil {
		t.Fatal(err)
	}
	got := applyFilters(recs, map[string]string{"run": "20260601T120000.000Z-bbbb"})
	if len(got) != 1 || got[0].Run != "20260601T120000.000Z-bbbb" {
		t.Errorf("run filter kept %d rows: %+v", len(got), got)
	}
}

// Fixtures for compare-by-dimension: two runs, each carrying both auth
// values at delay=nn, plus a legacy flat-axis row with no named axes.
const (
	rowR1SasNN   = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T100000.000Z-aaaa","backend":"mock","axes":{"auth":"sas","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":250000000,"warm_p50_ns":22000000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":360000000}`
	rowR1EntraNN = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T100000.000Z-aaaa","backend":"mock","axes":{"auth":"entra","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":340000000,"warm_p50_ns":22000000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":450000000}`
	rowR2SasNN   = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T110000.000Z-bbbb","backend":"mock","axes":{"auth":"sas","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":255000000,"warm_p50_ns":22000000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":366000000}`
	rowR2EntraNN = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T110000.000Z-bbbb","backend":"mock","axes":{"auth":"entra","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":360000000,"warm_p50_ns":22000000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":470000000}`
	rowLegacyNF  = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T100000.000Z-aaaa","backend":"mock","axis":"nf","scenario":"S","mode":"PortForward","cold_p50_ns":300000000,"warm_p50_ns":22000000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":360000000}`
	rowLegacyFN  = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T100000.000Z-aaaa","backend":"mock","axis":"fn","scenario":"S","mode":"PortForward","cold_p50_ns":330000000,"warm_p50_ns":22000000,"warm_p95_ns":23000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":360000000}`
)

func TestRenderCompare_ByAxis_DeltaAndColumns(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowR1SasNN, rowR1EntraNN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := renderCompare(&b, recs, "auth", "sas", "entra"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// cold: (340-250)/250 = +36.0%.
	if !strings.Contains(out, "+36.0%") {
		t.Errorf("missing cold delta +36.0%%:\n%s", out)
	}
	if !strings.Contains(out, "auth: baseline=sas  candidate=entra") {
		t.Errorf("header should name the compared dimension:\n%s", out)
	}
	hdr := headerLine(out)
	// The compared axis is dropped; the other axis remains a column.
	if !strings.Contains(hdr, "delay") {
		t.Errorf("expected residual 'delay' column:\n%s", hdr)
	}
	if strings.Contains(hdr, "auth") {
		t.Errorf("compared axis 'auth' must not be an identity column:\n%s", hdr)
	}
}

func TestRenderCompare_ByAxis_MultiRunShowsRunColumn(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowR1SasNN, rowR1EntraNN, rowR2SasNN, rowR2EntraNN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := renderCompare(&b, recs, "auth", "sas", "entra"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	hdr := headerLine(out)
	if !strings.Contains(hdr, "run") {
		t.Errorf("multi-run axis compare should add a run column:\n%s", hdr)
	}
	// One comparison row per run; both run ids present.
	for _, want := range []string{"aaaa", "bbbb"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected run %q in output:\n%s", want, out)
		}
	}
}

func TestRenderCompare_ByLegacyAxis(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowLegacyNF, rowLegacyFN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := renderCompare(&b, recs, "axis", "nf", "fn"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// cold: (330-300)/300 = +10.0%.
	if !strings.Contains(out, "+10.0%") {
		t.Errorf("missing cold delta +10.0%%:\n%s", out)
	}
	if !strings.Contains(out, "axis: baseline=nf  candidate=fn") {
		t.Errorf("header should name the legacy axis dimension:\n%s", out)
	}
}

func TestRenderCompare_NoOverlapNonRunErrors(t *testing.T) {
	// mock and azure live in separate runs, so a backend compare has no
	// residual that exists on both sides.
	recs, _, err := load([]string{writeTemp(t, rowR1SasNN, rowAzureRun)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	_, err = renderCompare(&b, recs, "backend", "mock", "azure")
	if err == nil || !strings.Contains(err.Error(), "no cells matched on both sides") {
		t.Errorf("expected no-overlap error, got %v", err)
	}
}

func TestRenderCompare_UnknownValueListsAvailable(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowR1SasNN, rowR1EntraNN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	_, err = renderCompare(&b, recs, "auth", "sas", "ntlm")
	if err == nil || !strings.Contains(err.Error(), "available: entra, sas") {
		t.Errorf("expected available-values error, got %v", err)
	}
}

func TestRenderCompare_ExcludesRowsLackingDim(t *testing.T) {
	// A named-axis row carries no flat axis, so an --compare-by axis run
	// should skip it and warn rather than match it as empty.
	recs, _, err := load([]string{writeTemp(t, rowLegacyNF, rowLegacyFN, rowR1SasNN)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if _, err := renderCompare(&b, recs, "axis", "nf", "fn"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `lack dimension "axis"`) {
		t.Errorf("expected an excluded-rows warning:\n%s", b.String())
	}
}

func TestResolveDimValue(t *testing.T) {
	recs, _, err := load([]string{writeTemp(t, rowR1SasNN, rowR1EntraNN)})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := resolveDimValue("auth", "entra", recs); err != nil || v != "entra" {
		t.Errorf("exact match: got (%q,%v)", v, err)
	}
	if _, err := resolveDimValue("auth", "nope", recs); err == nil {
		t.Error("expected error for missing value")
	}
	if _, err := resolveDimValue("frob", "x", recs); err == nil {
		t.Error("expected error for dimension no row carries")
	}
}

// headerLine returns the first line beginning with "backend" (the table
// header) from a rendered comparison.
func headerLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "backend") {
			return line
		}
	}
	return ""
}

const rowAzureRun = `{"type":"row","schema":"perfmatrix/v1","run":"20260601T120000.000Z-cccc","backend":"azure","axes":{"auth":"sas","delay":"nn"},"scenario":"S","mode":"PortForward","cold_p50_ns":1450000000,"warm_p50_ns":210000000,"warm_p95_ns":215000000,"cold_n":5,"warm_n":25,"success_n":5,"attempt_n":5,"wall_ns":2500000000}`
