package scenarios

import (
	"reflect"
	"testing"
	"time"
)

func TestSplitScenarioPath_NamedAxisCount(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		nAxes    int
		wantVals []string
		wantLeaf string
	}{
		{
			name:     "two axes",
			path:     "TestE2E_Mock/sas/nn/Parallel_ConnReusedEcho_PortForward",
			nAxes:    2,
			wantVals: []string{"sas", "nn"},
			wantLeaf: "Parallel_ConnReusedEcho_PortForward",
		},
		{
			name:     "one axis (pinned auth) collapses",
			path:     "TestE2E_Mock/nn/Parallel_ConnReusedEcho_PortForward",
			nAxes:    1,
			wantVals: []string{"nn"},
			wantLeaf: "Parallel_ConnReusedEcho_PortForward",
		},
		{
			name:     "no axes (fully pinned)",
			path:     "TestE2E_Mock/Parallel_ConnReusedEcho_PortForward",
			nAxes:    0,
			wantVals: []string{},
			wantLeaf: "Parallel_ConnReusedEcho_PortForward",
		},
		{
			// A scenario that nests its own sub-test must not have that
			// sub-path mistaken for an axis: only nAxes segments are axes.
			name:     "inner subtest stays in the leaf",
			path:     "TestE2E_Mock/sas/nn/Parallel_ConnReusedEcho/inner",
			nAxes:    2,
			wantVals: []string{"sas", "nn"},
			wantLeaf: "Parallel_ConnReusedEcho/inner",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals, leaf := splitScenarioPath(tc.path, tc.nAxes)
			if !reflect.DeepEqual(vals, tc.wantVals) {
				t.Errorf("axisVals = %#v, want %#v", vals, tc.wantVals)
			}
			if leaf != tc.wantLeaf {
				t.Errorf("leaf = %q, want %q", leaf, tc.wantLeaf)
			}
		})
	}
}

func TestRecordPerfMatrixRow_NamedAxes(t *testing.T) {
	perfMatrixSink.drain() // clear any prior rows
	perfMatrixSink.setAxisNames([]string{"auth", "delay"})
	t.Cleanup(func() {
		perfMatrixSink.drain()
		perfMatrixSink.setAxisNames(nil)
	})

	recordPerfMatrixRow(
		"TestE2E_Mock/entra/ff/Parallel_ConnReusedEcho_SOCKS5",
		[]time.Duration{time.Second}, []time.Duration{time.Millisecond},
		3, 3, 2*time.Second,
	)
	rows := perfMatrixSink.drain()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	wantAxes := map[string]string{"auth": "entra", "delay": "ff"}
	if !reflect.DeepEqual(r.axes, wantAxes) {
		t.Errorf("axes = %#v, want %#v", r.axes, wantAxes)
	}
	if r.axis != "entra/ff" {
		t.Errorf("flat axis = %q, want entra/ff", r.axis)
	}
	if r.scenario != "Parallel_ConnReusedEcho" || r.mode != "SOCKS5" {
		t.Errorf("scenario/mode = %q/%q, want Parallel_ConnReusedEcho/SOCKS5", r.scenario, r.mode)
	}
	rec := r.record()
	if rec.Axes["auth"] != "entra" || rec.Axes["delay"] != "ff" {
		t.Errorf("record Axes = %#v, want auth=entra delay=ff", rec.Axes)
	}
}
