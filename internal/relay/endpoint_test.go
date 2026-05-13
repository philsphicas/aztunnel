package relay

import "testing"

func TestParseRelay(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		suffix string
		want   string
	}{
		{"bare name", "my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"fqdn", "my-relay.servicebus.windows.net", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"fqdn china", "my-relay.servicebus.chinacloudapi.cn", DefaultRelaySuffix, "my-relay.servicebus.chinacloudapi.cn"},
		{"sb:// uri", "sb://my-relay.servicebus.windows.net", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"sb:// bare name", "sb://my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"wss:// uri", "wss://my-relay.servicebus.windows.net", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"wss:// uri with port", "wss://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443"},
		{"https:// uri with port", "https://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443"},
		{"wss:// bare → wss + suffix", "wss://my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"whitespace", "  my-relay  ", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"custom suffix", "my-relay", ".servicebus.chinacloudapi.cn", "my-relay.servicebus.chinacloudapi.cn"},
		{"bare name usgov", "my-relay", ".servicebus.usgovcloudapi.net", "my-relay.servicebus.usgovcloudapi.net"},
		{"bare name with dot", "my.relay", DefaultRelaySuffix, "my.relay"},
		// Edge cases.
		{"empty string", "", DefaultRelaySuffix, ""},
		{"whitespace only", "   ", DefaultRelaySuffix, ""},
		{"sb:// no host", "sb://", DefaultRelaySuffix, ""},
		{"malformed scheme", "://invalid", DefaultRelaySuffix, ""},
		// Plain-text schemes are rejected: aztunnel only dials TLS.
		// Use --relay-insecure-tls for self-signed mock relays.
		{"ws:// rejected", "ws://localhost:8080", DefaultRelaySuffix, ""},
		{"http:// rejected", "http://localhost:8080/", DefaultRelaySuffix, ""},
		{"unknown scheme rejected", "ftp://relay.example.com:8443", DefaultRelaySuffix, ""},
		// Bare host:port (and bare IPv6) are not accepted — clients
		// must use the wss:// form for those.
		{"bare host:port rejected", "localhost:8080", DefaultRelaySuffix, ""},
		{"bare ipv4:port rejected", "127.0.0.1:8080", DefaultRelaySuffix, ""},
		{"bare ipv6 rejected", "::1", DefaultRelaySuffix, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRelay(tt.input, tt.suffix)
			if got != tt.want {
				t.Errorf("ParseRelay(%q, %q) = %q, want %q", tt.input, tt.suffix, got, tt.want)
			}
		})
	}
}
