package scenarios

import "testing"

// senderLogsWithRetry mirrors the empirical log shape captured in
// CI run 26620057291 attempt 2: the first SOCKS5 dial to the echo
// target lost its relay rendezvous (no "relay connected" line for
// the abandoned bridge_id), dialSOCKS5WithRetry retried, and the
// second attempt completed normally. Both attempts emit
// "connection requested" with target="127.0.0.1:46299"; only the
// second emits "relay connected". The refused target has a single
// attempt — DialSOCKS5 (not dialSOCKS5WithRetry) is used in the
// failure branch of ScenarioBridgeID_Correlation.
const senderLogsWithRetry = `time=2026-05-29T06:21:19.321Z level=INFO msg="connection requested" bridge_id=7CUBB3OWDGJFEQG6 target=127.0.0.1:46299
time=2026-05-29T06:21:19.321Z level=DEBUG msg="dialing relay" bridge_id=7CUBB3OWDGJFEQG6 entityPath=e2e-sas-example
time=2026-05-29T06:21:21.373Z level=INFO msg="connection requested" bridge_id=QJ5JL2KH2SLLHFRE target=127.0.0.1:46299
time=2026-05-29T06:21:21.373Z level=DEBUG msg="dialing relay" bridge_id=QJ5JL2KH2SLLHFRE entityPath=e2e-sas-example
time=2026-05-29T06:21:21.806Z level=DEBUG msg="relay connected" bridge_id=QJ5JL2KH2SLLHFRE entityPath=e2e-sas-example
time=2026-05-29T06:21:22.089Z level=INFO msg="connection requested" bridge_id=VH4TWWUZH3WJ7EYA target=127.0.0.1:41415
time=2026-05-29T06:21:22.090Z level=DEBUG msg="dialing relay" bridge_id=VH4TWWUZH3WJ7EYA entityPath=e2e-sas-example
time=2026-05-29T06:21:22.521Z level=DEBUG msg="relay connected" bridge_id=VH4TWWUZH3WJ7EYA entityPath=e2e-sas-example
`

func TestBridgeIDForTarget_PicksSuccessfulRetry(t *testing.T) {
	got := bridgeIDForTarget(senderLogsWithRetry, "127.0.0.1:46299")
	const want = "QJ5JL2KH2SLLHFRE"
	if got != want {
		t.Errorf("bridgeIDForTarget(echo) = %q, want %q (must skip abandoned 7CUBB3O bridge that never connected)", got, want)
	}
}

func TestBridgeIDForTarget_SingleAttemptStillWorks(t *testing.T) {
	got := bridgeIDForTarget(senderLogsWithRetry, "127.0.0.1:41415")
	const want = "VH4TWWUZH3WJ7EYA"
	if got != want {
		t.Errorf("bridgeIDForTarget(refused) = %q, want %q", got, want)
	}
}

func TestBridgeIDForTarget_NoMatchReturnsEmpty(t *testing.T) {
	if got := bridgeIDForTarget(senderLogsWithRetry, "127.0.0.1:9999"); got != "" {
		t.Errorf("bridgeIDForTarget(missing target) = %q, want empty", got)
	}
}

func TestBridgeIDForTarget_NoConnectedBridgeReturnsEmpty(t *testing.T) {
	// All bridges abandoned: only "connection requested" + "dialing
	// relay", never "relay connected". bridgeIDForTarget must
	// return "" rather than handing back an abandoned bridge_id.
	const logs = `time=2026-05-29T06:21:19.321Z level=INFO msg="connection requested" bridge_id=ABCDEFGHIJKLMNOP target=127.0.0.1:46299
time=2026-05-29T06:21:19.321Z level=DEBUG msg="dialing relay" bridge_id=ABCDEFGHIJKLMNOP entityPath=e2e-sas
`
	if got := bridgeIDForTarget(logs, "127.0.0.1:46299"); got != "" {
		t.Errorf("bridgeIDForTarget on all-abandoned bridges = %q, want empty", got)
	}
}

func TestBridgeIDForTarget_PrefixPortDoesNotMatch(t *testing.T) {
	// Guard against substring matching: a target of
	// "127.0.0.1:4141" must not match a sender log carrying
	// target=127.0.0.1:41415 (port 41415 starts with the digits
	// "4141"). bridgeIDForTarget must perform whole-field matching.
	if got := bridgeIDForTarget(senderLogsWithRetry, "127.0.0.1:4141"); got != "" {
		t.Errorf("bridgeIDForTarget(prefix port) = %q, want empty (must not substring-match 127.0.0.1:41415)", got)
	}
}

func TestConnectedBridgeIDs_BuildsSet(t *testing.T) {
	set := connectedBridgeIDs(senderLogsWithRetry)
	if _, ok := set["QJ5JL2KH2SLLHFRE"]; !ok {
		t.Errorf("connectedBridgeIDs missing QJ5J")
	}
	if _, ok := set["VH4TWWUZH3WJ7EYA"]; !ok {
		t.Errorf("connectedBridgeIDs missing VH4T")
	}
	if _, ok := set["7CUBB3OWDGJFEQG6"]; ok {
		t.Errorf("connectedBridgeIDs incorrectly includes abandoned 7CUBB3O")
	}
	if got, want := len(set), 2; got != want {
		t.Errorf("connectedBridgeIDs size = %d, want %d", got, want)
	}
}
