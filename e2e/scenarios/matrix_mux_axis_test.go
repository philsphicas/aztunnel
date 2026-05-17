package scenarios

import (
	"reflect"
	"testing"
	"time"
)

// TestPerfMatrix_RecordsMuxAxisCellValue exercises the integration
// between WithMuxAxis-tagged test paths and the perfMatrix recorder:
// when a perf row's t.Name() contains a `/v2/` segment in the
// canonical sub-test position, matrixIdentity should label it with
// axes["mux"]="v2", and v1 vs v2 rows should be treated as distinct
// cells by drain ordering and reporter pivoting.
func TestPerfMatrix_RecordsMuxAxisCellValue(t *testing.T) {
	perfMatrixSink.setAxisNames([]string{"auth", "delay", "mux"})
	t.Cleanup(func() {
		perfMatrixSink.drain()
		perfMatrixSink.setAxisNames(nil)
	})

	// Record one row per (mux=v1, mux=v2) cell within the same outer
	// auth/delay cell, simulating what RunPerformanceScenarios emits
	// when wrapped by WithMuxAxis.
	for _, mux := range []string{"v1", "v2"} {
		recordPerfMatrixRow(
			"TestE2E_Mock/sas/default/"+mux+"/Parallel_ConnReusedEcho_SOCKS5",
			[]time.Duration{time.Second},
			[]time.Duration{time.Millisecond},
			3, 3, 2*time.Second,
		)
	}

	rows := perfMatrixSink.drain()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one per mux cell)", len(rows))
	}

	// Drain orders rows by (axis, scenario, mode). v1/v2 share scenario
	// and mode but differ on axis, so v1 sorts before v2.
	wantV1 := map[string]string{"auth": "sas", "delay": "default", "mux": "v1"}
	wantV2 := map[string]string{"auth": "sas", "delay": "default", "mux": "v2"}
	if !reflect.DeepEqual(rows[0].axes, wantV1) {
		t.Errorf("rows[0].axes = %#v, want %#v", rows[0].axes, wantV1)
	}
	if !reflect.DeepEqual(rows[1].axes, wantV2) {
		t.Errorf("rows[1].axes = %#v, want %#v", rows[1].axes, wantV2)
	}
	if rows[0].axis == rows[1].axis {
		t.Errorf("v1 and v2 rows must NOT share the flat axis path (got %q for both); the reporter would treat them as duplicate cells",
			rows[0].axis)
	}
}

// TestPerfMatrix_NonPerfPathSkipsMuxAxis exercises the case where the
// global axisNames includes "mux" (because the perf suite uses it) but
// a non-perf path doesn't have a mux segment in t.Name(). The recorder
// must not invent a mux axis value out of thin air — splitScenarioPath's
// length cap is the line of defence.
//
// In practice non-perf scenarios don't call recordPerfMatrixRow today,
// but this test pins the contract so a future scenario that does (e.g.
// a streaming-family observability check) won't silently mislabel rows.
func TestPerfMatrix_NonPerfPathSkipsMuxAxis(t *testing.T) {
	perfMatrixSink.setAxisNames([]string{"auth", "delay", "mux"})
	t.Cleanup(func() {
		perfMatrixSink.drain()
		perfMatrixSink.setAxisNames(nil)
	})

	// Path has only auth/delay segments, no mux segment — looks like a
	// row a topology scenario would record if it ever did.
	recordPerfMatrixRow(
		"TestE2E_Mock/sas/default/Distribution_PerListener_PortForward",
		[]time.Duration{time.Second},
		[]time.Duration{time.Millisecond},
		1, 1, time.Second,
	)
	rows := perfMatrixSink.drain()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	want := map[string]string{"auth": "sas", "delay": "default"}
	if !reflect.DeepEqual(rows[0].axes, want) {
		t.Errorf("axes = %#v, want %#v (no synthesised mux key)", rows[0].axes, want)
	}
}
