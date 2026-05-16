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
		// Explicit default-port :443 is stripped on https/wss so the
		// canonical sr matches what Azure Relay expects (no port).
		{"wss:// explicit :443 stripped", "wss://my-relay.servicebus.windows.net:443", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"https:// explicit :443 stripped", "https://my-relay.servicebus.windows.net:443", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"wss:// bare name with :443", "wss://my-relay:443", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		// sb:// is accepted as a scheme but does not get default-port
		// stripping — aztunnel doesn't dial AMQP anyway.
		{"sb:// uri with :5671 preserved", "sb://my-relay.servicebus.windows.net:5671", DefaultRelaySuffix, "my-relay.servicebus.windows.net:5671"},
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
		// URL inputs are validated strictly; userinfo / paths / query
		// strings / fragments are rejected rather than silently dropped.
		{"wss:// with userinfo rejected", "wss://user@relay.example.com:8443", DefaultRelaySuffix, ""},
		{"wss:// with user:pass rejected", "wss://user:pass@relay.example.com:8443", DefaultRelaySuffix, ""},
		{"wss:// with path rejected", "wss://relay.example.com:8443/foo", DefaultRelaySuffix, ""},
		{"wss:// with trailing slash accepted", "wss://relay.example.com:8443/", DefaultRelaySuffix, "relay.example.com:8443"},
		{"wss:// with query rejected", "wss://relay.example.com:8443?foo=bar", DefaultRelaySuffix, ""},
		{"wss:// with empty query rejected", "wss://relay.example.com:8443?", DefaultRelaySuffix, ""},
		{"wss:// with fragment rejected", "wss://relay.example.com:8443#frag", DefaultRelaySuffix, ""},
		{"wss:// with empty fragment rejected", "wss://relay.example.com:8443#", DefaultRelaySuffix, ""},
		// Invalid explicit ports are rejected up front so the failure
		// is reported as "malformed URI" rather than an opaque dial error.
		{"wss:// trailing colon rejected", "wss://relay.example.com:", DefaultRelaySuffix, ""},
		{"wss:// port 0 rejected", "wss://relay.example.com:0", DefaultRelaySuffix, ""},
		{"wss:// port 65536 rejected", "wss://relay.example.com:65536", DefaultRelaySuffix, ""},
		{"wss:// non-numeric port rejected", "wss://relay.example.com:abc", DefaultRelaySuffix, ""},
		{"wss:// ipv6 trailing colon rejected", "wss://[::1]:", DefaultRelaySuffix, ""},
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
