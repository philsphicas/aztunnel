package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
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

func TestMuxHandshakeRoundTrip(t *testing.T) {
	hs := MuxHandshake{
		Version: MuxVersion,
		Mode:    MuxMode,
		Capabilities: &MuxCapabilities{
			ClientVersion:    "aztunnel/test",
			KeepAliveSeconds: 30,
			MaxStreams:       256,
		},
	}
	data, err := json.Marshal(hs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got MuxHandshake
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != MuxVersion || got.Mode != MuxMode {
		t.Errorf("got version=%d mode=%q, want %d/%q", got.Version, got.Mode, MuxVersion, MuxMode)
	}
	if got.Capabilities == nil || got.Capabilities.MaxStreams != 256 {
		t.Errorf("capabilities lost in round-trip: %+v", got.Capabilities)
	}
}

func TestFirstMessageIsMux(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		isMux   bool
	}{
		{"v1 envelope", `{"version":1,"target":"10.0.0.5:22"}`, false},
		{"v2 mux handshake", `{"version":2,"mode":"mux"}`, true},
		{"v2 no mode", `{"version":2}`, false},
		{"v1 with mode (defensive)", `{"version":1,"mode":"mux"}`, false},
		{"unknown version", `{"version":99,"mode":"mux"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fm FirstMessage
			if err := json.Unmarshal([]byte(tt.payload), &fm); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := fm.IsMux(); got != tt.isMux {
				t.Errorf("IsMux() = %v, want %v (payload=%s)", got, tt.isMux, tt.payload)
			}
		})
	}
}

func TestStreamEnvelopeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := ConnectEnvelope{Version: CurrentVersion, Target: "10.0.0.5:22"}
	if err := WriteStreamEnvelope(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadStreamEnvelope(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Target != want.Target || got.Version != want.Version {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if buf.Len() != 0 {
		t.Errorf("expected reader fully consumed, %d bytes left", buf.Len())
	}
}

func TestStreamResponseRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := ConnectResponse{Version: CurrentVersion, OK: true}
	if err := WriteStreamResponse(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadStreamResponse(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.OK != want.OK {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestStreamResponseRoundTrip_ListenerID guards the integration: the
// length-prefixed framing must preserve every field on ConnectResponse,
// including the post-rebase ListenerID, so mux-mode senders can
// correlate listener_id on accept the same way v1 senders do.
func TestStreamResponseRoundTrip_ListenerID(t *testing.T) {
	var buf bytes.Buffer
	want := ConnectResponse{Version: CurrentVersion, OK: true, ListenerID: "ABCDEFGHIJKLMNOP"}
	if err := WriteStreamResponse(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadStreamResponse(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ListenerID != want.ListenerID {
		t.Errorf("listener_id = %q, want %q", got.ListenerID, want.ListenerID)
	}
}

// TestStreamEnvelopeRoundTrip_BridgeID mirrors the response test: every
// stream-mode envelope the sender writes must carry BridgeID through
// the length-prefixed framing so the listener can bind bridge_id on
// its per-stream logger.
func TestStreamEnvelopeRoundTrip_BridgeID(t *testing.T) {
	var buf bytes.Buffer
	want := ConnectEnvelope{Version: CurrentVersion, Target: "10.0.0.5:22", BridgeID: "ABCDEFGHIJKLMNOP"}
	if err := WriteStreamEnvelope(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadStreamEnvelope(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.BridgeID != want.BridgeID {
		t.Errorf("bridge_id = %q, want %q", got.BridgeID, want.BridgeID)
	}
}

// TestReadStreamResponse_NoOverreadOfBanner is the critical regression test
// for the bug the rubber-duck review caught in the prototype: json.Decoder
// buffers and would consume target-banner bytes (SSH "SSH-2.0...", SMTP
// "220 ...", etc.) emitted by a server-first target immediately after the
// listener's response. Length-prefixed framing must stop exactly at the
// JSON boundary so any following bytes remain on the wire for the bridge.
func TestReadStreamResponse_NoOverreadOfBanner(t *testing.T) {
	banner := []byte("SSH-2.0-OpenSSH_9.6p1\r\n")
	// Build: [2-byte length][JSON response][banner]
	resp := ConnectResponse{Version: CurrentVersion, OK: true}
	respJSON, _ := json.Marshal(resp)
	var buf bytes.Buffer
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(respJSON)))
	buf.Write(hdr[:])
	buf.Write(respJSON)
	buf.Write(banner)

	got, err := ReadStreamResponse(&buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !got.OK {
		t.Fatalf("got %+v, want ok=true", got)
	}
	// Critical: the banner must still be readable from the same stream.
	remaining, _ := io.ReadAll(&buf)
	if !bytes.Equal(remaining, banner) {
		t.Fatalf("banner lost or corrupted: got %q, want %q", remaining, banner)
	}
}

// TestReadStreamEnvelope_NoOverreadOfPayload is the listener-side mirror:
// after reading the envelope, subsequent stream bytes (e.g. an HTTP request
// the sender wrote immediately) must still be available.
func TestReadStreamEnvelope_NoOverreadOfPayload(t *testing.T) {
	payload := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	env := ConnectEnvelope{Version: CurrentVersion, Target: "example.com:80"}
	envJSON, _ := json.Marshal(env)
	var buf bytes.Buffer
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(envJSON)))
	buf.Write(hdr[:])
	buf.Write(envJSON)
	buf.Write(payload)

	got, err := ReadStreamEnvelope(&buf)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	if got.Target != "example.com:80" {
		t.Errorf("got target %q, want example.com:80", got.Target)
	}
	remaining, _ := io.ReadAll(&buf)
	if !bytes.Equal(remaining, payload) {
		t.Errorf("payload lost or corrupted: got %q, want %q", remaining, payload)
	}
}

func TestReadStreamResponse_FrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	var hdr [2]byte
	// maxStreamFrameSize is 8 KiB; ask for something bigger.
	binary.BigEndian.PutUint16(hdr[:], maxStreamFrameSize+1)
	buf.Write(hdr[:])
	_, err := ReadStreamResponse(&buf)
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected size error, got: %v", err)
	}
}

func TestReadStreamResponse_EmptyFrame(t *testing.T) {
	var buf bytes.Buffer
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], 0)
	buf.Write(hdr[:])
	_, err := ReadStreamResponse(&buf)
	if err == nil {
		t.Fatal("expected error for empty frame")
	}
}

func TestReadStreamResponse_TruncatedHeader(t *testing.T) {
	_, err := ReadStreamResponse(bytes.NewReader([]byte{0x01}))
	if err == nil {
		t.Fatal("expected error for truncated header")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) && !strings.Contains(err.Error(), "read length") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWriteStreamEnvelope_FrameTooLarge(t *testing.T) {
	big := strings.Repeat("x", maxStreamFrameSize+1)
	env := ConnectEnvelope{Version: CurrentVersion, Target: big}
	err := WriteStreamEnvelope(io.Discard, env)
	if err == nil {
		t.Fatal("expected error writing oversized envelope")
	}
}
