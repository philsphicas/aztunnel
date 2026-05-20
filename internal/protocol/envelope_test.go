package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	env := ConnectEnvelope{
		Version:  CurrentVersion,
		Target:   "10.0.0.5:22",
		Metadata: map[string]string{"trace": "abc123"},
		BridgeID: "ABCDEFGHIJKLMNOP",
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got ConnectEnvelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != env.Version {
		t.Errorf("version = %d, want %d", got.Version, env.Version)
	}
	if got.Target != env.Target {
		t.Errorf("target = %q, want %q", got.Target, env.Target)
	}
	if got.Metadata["trace"] != "abc123" {
		t.Errorf("metadata[trace] = %q, want %q", got.Metadata["trace"], "abc123")
	}
	if got.BridgeID != env.BridgeID {
		t.Errorf("bridge_id = %q, want %q", got.BridgeID, env.BridgeID)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		resp ConnectResponse
	}{
		{"success", ConnectResponse{Version: CurrentVersion, OK: true}},
		{"failure", ConnectResponse{Version: CurrentVersion, OK: false, Error: "connection failed"}},
		{"success-with-listener-id", ConnectResponse{Version: CurrentVersion, OK: true, ListenerID: "abc123def4567890"}},
		{"failure-with-listener-id", ConnectResponse{Version: CurrentVersion, OK: false, Error: "connection failed", Code: CodeConnectionRefused, ListenerID: "0123456789abcdef"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got ConnectResponse
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.OK != tt.resp.OK {
				t.Errorf("ok = %v, want %v", got.OK, tt.resp.OK)
			}
			if got.Error != tt.resp.Error {
				t.Errorf("error = %q, want %q", got.Error, tt.resp.Error)
			}
			if got.Code != tt.resp.Code {
				t.Errorf("code = %q, want %q", got.Code, tt.resp.Code)
			}
			if got.ListenerID != tt.resp.ListenerID {
				t.Errorf("listener_id = %q, want %q", got.ListenerID, tt.resp.ListenerID)
			}
		})
	}
}

func TestEnvelopeOmitEmptyMetadata(t *testing.T) {
	env := ConnectEnvelope{Version: CurrentVersion, Target: "host:22"}
	data, _ := json.Marshal(env)
	s := string(data)
	if strings.Contains(s, "metadata") {
		t.Errorf("expected metadata to be omitted, got: %s", s)
	}
}

func TestEnvelopeOmitEmptyBridgeID(t *testing.T) {
	env := ConnectEnvelope{Version: CurrentVersion, Target: "host:22"}
	data, _ := json.Marshal(env)
	s := string(data)
	if strings.Contains(s, "bridge_id") {
		t.Errorf("expected bridge_id to be omitted, got: %s", s)
	}
}

func TestResponseOmitEmptyError(t *testing.T) {
	resp := ConnectResponse{Version: CurrentVersion, OK: true}
	data, _ := json.Marshal(resp)
	s := string(data)
	if strings.Contains(s, "error") {
		t.Errorf("expected error to be omitted, got: %s", s)
	}
}

// TestResponseOmitEmptyListenerID guards the backward-compatibility
// contract: a listener that hasn't been upgraded to the listener_id
// build emits responses without the field, and a parsing peer must
// see "" rather than seeing the key with an empty string value (the
// latter would still leak the field name onto the wire, which is the
// canary the omitempty tag exists to prevent).
func TestResponseOmitEmptyListenerID(t *testing.T) {
	resp := ConnectResponse{Version: CurrentVersion, OK: true}
	data, _ := json.Marshal(resp)
	s := string(data)
	if strings.Contains(s, "listener_id") {
		t.Errorf("expected listener_id to be omitted, got: %s", s)
	}
}
