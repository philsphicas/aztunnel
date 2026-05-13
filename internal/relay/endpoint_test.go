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
		{"https:// uri with default port", "https://my-relay.servicebus.windows.net:443/", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"wss:// uri", "wss://my-relay.servicebus.windows.net", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"wss:// uri with port", "wss://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443"},
		{"https:// uri with port", "https://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443"},
		{"wss:// bare → wss + suffix", "wss://my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"whitespace", "  my-relay  ", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"custom suffix", "my-relay", ".servicebus.chinacloudapi.cn", "my-relay.servicebus.chinacloudapi.cn"},
		{"bare name usgov", "my-relay", ".servicebus.usgovcloudapi.net", "my-relay.servicebus.usgovcloudapi.net"},
		// Edge cases.
		{"empty string", "", DefaultRelaySuffix, ""},
		{"whitespace only", "   ", DefaultRelaySuffix, ""},
		{"bare name with dot", "my.relay", DefaultRelaySuffix, "my.relay"},
		{"sb:// no host", "sb://", DefaultRelaySuffix, ""},
		{"malformed scheme", "://invalid", DefaultRelaySuffix, ""},
		{"uri with path + default port", "https://my-relay.servicebus.windows.net:443/some/path", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		// Bare host:port (mock/self-hosted relays). Implicit wss.
		{"bare host:port", "localhost:8080", DefaultRelaySuffix, "localhost:8080"},
		{"bare ipv4:port", "127.0.0.1:8080", DefaultRelaySuffix, "127.0.0.1:8080"},
		// IPv6 literals: bare forms get bracketed, URLs pass through.
		{"ipv6 bracketed url", "wss://[::1]:9000/", DefaultRelaySuffix, "[::1]:9000"},
		{"ipv6 bracketed default port", "wss://[::1]:443/", DefaultRelaySuffix, "[::1]"},
		{"bare ipv6 ::1", "::1", DefaultRelaySuffix, "[::1]"},
		{"bare ipv6 2001:db8::1", "2001:db8::1", DefaultRelaySuffix, "[2001:db8::1]"},
		{"bare ipv4 unchanged", "127.0.0.1", DefaultRelaySuffix, "127.0.0.1"},
		// Default port (443) stripped on bare host:port too. Matters
		// for SAS audience canonicalization: a token signed for
		// "https://host/entity" must not include the default port.
		{"bare host:443 default port stripped", "host:443", DefaultRelaySuffix, "host"},
		{"bare ipv4:443 default port stripped", "127.0.0.1:443", DefaultRelaySuffix, "127.0.0.1"},
		// Plain-text schemes are rejected: aztunnel only dials TLS.
		// Use --relay-insecure-tls for self-signed mock relays.
		{"ws:// rejected", "ws://localhost:8080", DefaultRelaySuffix, ""},
		{"http:// rejected", "http://localhost:8080/", DefaultRelaySuffix, ""},
		{"unknown scheme rejected", "ftp://relay.example.com:8443", DefaultRelaySuffix, ""},
		// Invalid ports must be rejected — url.Parse rejects non-numeric
		// ports for URI inputs, but empty / zero / out-of-range numerics
		// pass and would later fail at dial time with a worse error.
		{"url trailing colon", "https://host:", DefaultRelaySuffix, ""},
		{"url port zero", "https://host:0", DefaultRelaySuffix, ""},
		{"url port too large", "https://host:99999", DefaultRelaySuffix, ""},
		{"url ipv6 trailing colon", "https://[::1]:", DefaultRelaySuffix, ""},
		{"url ipv6 port too large", "https://[::1]:99999", DefaultRelaySuffix, ""},
		{"url with userinfo", "https://user:pass@host:8080", DefaultRelaySuffix, ""},
		{"bare trailing colon", "host:", DefaultRelaySuffix, ""},
		{"bare port non-numeric", "host:bad", DefaultRelaySuffix, ""},
		{"bare port zero", "host:0", DefaultRelaySuffix, ""},
		{"bare port too large", "host:99999", DefaultRelaySuffix, ""},
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
