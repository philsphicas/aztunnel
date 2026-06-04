package sender

import (
	"testing"

	"github.com/philsphicas/aztunnel/internal/protocol"
)

// TestDefaultSenderMaxProtocolVersion_Is1 pins the 0.4.0 sender
// default to v1 (mux opt-in). When this constant flips to
// protocol.MuxVersion in 0.5.0, update this test in the SAME commit
// — the test exists to make that flip a deliberate, single-line code
// change rather than a silent default shift that goes through CI
// unnoticed.
func TestDefaultSenderMaxProtocolVersion_Is1(t *testing.T) {
	if got, want := DefaultSenderMaxProtocolVersion, 1; got != want {
		t.Errorf("DefaultSenderMaxProtocolVersion = %d, want %d (0.5.0 flip: bump together with cli/help/docs)", got, want)
	}
}

func TestNormalizeSenderMaxProtocolVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero takes the default", 0, DefaultSenderMaxProtocolVersion},
		{"v1 passes through", 1, 1},
		{"v2 passes through", protocol.MuxVersion, protocol.MuxVersion},
		{"negative clamps up to 1", -3, 1},
		{"above-max clamps down to MuxVersion", protocol.MuxVersion + 5, protocol.MuxVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeSenderMaxProtocolVersion(tt.in); got != tt.want {
				t.Errorf("NormalizeSenderMaxProtocolVersion(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
