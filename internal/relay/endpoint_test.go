package relay

import "testing"

func TestParseRelayEndpoint(t *testing.T) {
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
		{"whitespace", "  my-relay  ", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"custom suffix", "my-relay", ".servicebus.chinacloudapi.cn", "my-relay.servicebus.chinacloudapi.cn"},
		{"bare name usgov", "my-relay", ".servicebus.usgovcloudapi.net", "my-relay.servicebus.usgovcloudapi.net"},
		// Edge cases
		{"empty string", "", DefaultRelaySuffix, ""},
		{"whitespace only", "   ", DefaultRelaySuffix, ""},
		{"bare name with dot", "my.relay", DefaultRelaySuffix, "my.relay"},
		{"sb:// no host", "sb://", DefaultRelaySuffix, ""},
		{"malformed scheme", "://invalid", DefaultRelaySuffix, ""},
		{"uri with path + default port", "https://my-relay.servicebus.windows.net:443/some/path", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		// URL-driven behaviors (mock/self-hosted relays)
		{"bare host:port", "localhost:8080", DefaultRelaySuffix, "localhost:8080"},
		{"bare ipv4:port", "127.0.0.1:8080", DefaultRelaySuffix, "127.0.0.1:8080"},
		{"ws://host:port", "ws://localhost:8080", DefaultRelaySuffix, "localhost:8080"},
		{"wss://host:port", "wss://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443"},
		{"http://host:port", "http://localhost:8080/", DefaultRelaySuffix, "localhost:8080"},
		{"http:// default port stripped", "http://localhost:80/", DefaultRelaySuffix, "localhost"},
		{"ws:// default port stripped", "ws://localhost:80/", DefaultRelaySuffix, "localhost"},
		{"ipv6 bracketed url", "ws://[::1]:9000/", DefaultRelaySuffix, "[::1]:9000"},
		{"ipv6 bracketed default port", "wss://[::1]:443/", DefaultRelaySuffix, "[::1]"},
		// Bare IPv6 literals must be bracketed so they can be embedded
		// in a URL host. IPv4 literals pass through.
		{"bare ipv6 ::1", "::1", DefaultRelaySuffix, "[::1]"},
		{"bare ipv6 2001:db8::1", "2001:db8::1", DefaultRelaySuffix, "[2001:db8::1]"},
		{"bare ipv4 unchanged", "127.0.0.1", DefaultRelaySuffix, "127.0.0.1"},
		// Default port stripped on bare host:port too (implicit wss).
		// Matters for SAS audience canonicalization: a token signed
		// for "https://host/entity" must not include the default port.
		{"bare host:443 default port stripped", "host:443", DefaultRelaySuffix, "host"},
		{"bare ipv4:443 default port stripped", "127.0.0.1:443", DefaultRelaySuffix, "127.0.0.1"},
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
			got := ParseRelayEndpoint(tt.input, tt.suffix)
			if got != tt.want {
				t.Errorf("ParseRelayEndpoint(%q, %q) = %q, want %q", tt.input, tt.suffix, got, tt.want)
			}
		})
	}
}

func TestParseRelay(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		suffix     string
		wantHost   string
		wantScheme string
	}{
		{"bare name → wss", "my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net", SchemeWSS},
		{"fqdn → wss", "my-relay.servicebus.windows.net", DefaultRelaySuffix, "my-relay.servicebus.windows.net", SchemeWSS},
		{"bare host:port → wss, no suffix", "localhost:8080", DefaultRelaySuffix, "localhost:8080", SchemeWSS},
		{"ws://host:port → ws", "ws://localhost:8080", DefaultRelaySuffix, "localhost:8080", SchemeWS},
		{"http://host:port → ws", "http://localhost:8080/", DefaultRelaySuffix, "localhost:8080", SchemeWS},
		{"wss://host:port → wss", "wss://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443", SchemeWSS},
		{"https://host:port → wss", "https://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443", SchemeWSS},
		{"sb://bare → wss + suffix", "sb://my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net", SchemeWSS},
		{"wss://bare → wss + suffix", "wss://my-relay", DefaultRelaySuffix, "my-relay.servicebus.windows.net", SchemeWSS},
		{"default port stripped (wss/443)", "https://my-relay.servicebus.windows.net:443/", DefaultRelaySuffix, "my-relay.servicebus.windows.net", SchemeWSS},
		{"default port stripped (ws/80)", "http://localhost:80/", DefaultRelaySuffix, "localhost", SchemeWS},
		{"non-default port kept (wss/8443)", "wss://relay.example.com:8443", DefaultRelaySuffix, "relay.example.com:8443", SchemeWSS},
		{"non-default port kept (ws/8080)", "ws://localhost:8080", DefaultRelaySuffix, "localhost:8080", SchemeWS},
		{"ipv6 default port stripped", "wss://[::1]:443/", DefaultRelaySuffix, "[::1]", SchemeWSS},
		{"empty → empty", "", DefaultRelaySuffix, "", ""},
		{"malformed → empty", "://nope", DefaultRelaySuffix, "", ""},
		// Default port stripped on bare host:port too (implicit wss).
		{"bare host:443 stripped", "host:443", DefaultRelaySuffix, "host", SchemeWSS},
		{"bare ipv4:443 stripped", "127.0.0.1:443", DefaultRelaySuffix, "127.0.0.1", SchemeWSS},
		// Invalid ports / userinfo: same expectations as ParseRelayEndpoint.
		{"url trailing colon → empty", "https://host:", DefaultRelaySuffix, "", ""},
		{"url port zero → empty", "https://host:0", DefaultRelaySuffix, "", ""},
		{"url port too large → empty", "https://host:99999", DefaultRelaySuffix, "", ""},
		{"url userinfo → empty", "https://u:p@host:8080", DefaultRelaySuffix, "", ""},
		{"bare port non-numeric → empty", "host:bad", DefaultRelaySuffix, "", ""},
		{"bare port too large → empty", "host:99999", DefaultRelaySuffix, "", ""},
		{"bare trailing colon → empty", "host:", DefaultRelaySuffix, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, scheme := ParseRelay(tt.input, tt.suffix)
			if host != tt.wantHost || scheme != tt.wantScheme {
				t.Errorf("ParseRelay(%q, %q) = (%q, %q), want (%q, %q)",
					tt.input, tt.suffix, host, scheme, tt.wantHost, tt.wantScheme)
			}
		})
	}
}
