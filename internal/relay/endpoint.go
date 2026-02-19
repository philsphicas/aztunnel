package relay

import (
	"net/url"
	"strings"
)

// DefaultRelaySuffix is the Azure Relay namespace suffix for the public cloud.
const DefaultRelaySuffix = ".servicebus.windows.net"

// ParseRelayEndpoint normalizes a relay input to a bare FQDN.
//
// Accepted input formats:
//   - Bare namespace name: "my-relay" → "my-relay" + defaultSuffix
//   - FQDN: "my-relay.servicebus.windows.net" → used as-is
//   - URI with scheme: "sb://my-relay.servicebus.windows.net" → host extracted
//   - URI with port: "https://my-relay.servicebus.windows.net:443/" → host extracted
//
// Detection: if input contains "://", parse as URL and extract host.
// If input contains ".", treat as FQDN. Otherwise append defaultSuffix.
// Empty input is returned as-is (callers should validate before calling).
func ParseRelayEndpoint(input, defaultSuffix string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err == nil && u != nil && u.Hostname() != "" {
			host := u.Hostname()
			if strings.Contains(host, ".") {
				return host
			}
			return host + defaultSuffix
		}
	}

	if strings.Contains(input, ".") {
		return input
	}

	return input + defaultSuffix
}
