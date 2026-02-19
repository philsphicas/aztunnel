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
		{"https:// uri with port", "https://my-relay.servicebus.windows.net:443/", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"wss:// uri", "wss://my-relay.servicebus.windows.net", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"whitespace", "  my-relay  ", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
		{"custom suffix", "my-relay", ".servicebus.chinacloudapi.cn", "my-relay.servicebus.chinacloudapi.cn"},
		{"bare name usgov", "my-relay", ".servicebus.usgovcloudapi.net", "my-relay.servicebus.usgovcloudapi.net"},
		// Edge cases
		{"empty string", "", DefaultRelaySuffix, DefaultRelaySuffix},
		{"bare name with dot", "my.relay", DefaultRelaySuffix, "my.relay"},
		{"sb:// no host", "sb://", DefaultRelaySuffix, "sb://" + DefaultRelaySuffix},
		{"malformed scheme", "://invalid", DefaultRelaySuffix, "://invalid" + DefaultRelaySuffix},
		{"uri with path", "https://my-relay.servicebus.windows.net:443/some/path", DefaultRelaySuffix, "my-relay.servicebus.windows.net"},
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
