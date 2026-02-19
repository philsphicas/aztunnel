package relay

import (
	"fmt"
	"strings"
	"testing"
)

func TestSanitizeErr(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"single token", "dial wss://host/$hc?sb-hc-action=connect&sb-hc-token=SECRET"},
		{"token at end", "error sb-hc-token=SECRET"},
		{"token with trailing space", "error sb-hc-token=SECRET rest of message"},
		{"token with trailing quote", `error sb-hc-token=SECRET" more`},
		{"multiple tokens", "first sb-hc-token=SECRET1 second sb-hc-token=SECRET2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sanitizeErr(fmt.Errorf("%s", tt.input))
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("token not redacted: %v", err)
			}
			if !strings.Contains(err.Error(), "REDACTED") {
				t.Errorf("expected REDACTED in error: %v", err)
			}
		})
	}

	t.Run("no token", func(t *testing.T) {
		err := sanitizeErr(fmt.Errorf("connection refused"))
		if err.Error() != "connection refused" {
			t.Errorf("expected unchanged error, got %q", err.Error())
		}
	})
}
