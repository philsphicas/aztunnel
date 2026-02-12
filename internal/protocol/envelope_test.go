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
}

func TestResponseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		resp ConnectResponse
	}{
		{"success", ConnectResponse{Version: CurrentVersion, OK: true}},
		{"failure", ConnectResponse{Version: CurrentVersion, OK: false, Error: "connection failed"}},
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

func TestResponseOmitEmptyError(t *testing.T) {
	resp := ConnectResponse{Version: CurrentVersion, OK: true}
	data, _ := json.Marshal(resp)
	s := string(data)
	if strings.Contains(s, "error") {
		t.Errorf("expected error to be omitted, got: %s", s)
	}
}
