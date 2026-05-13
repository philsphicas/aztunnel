package relay

import (
	"crypto/tls"
	"net/http"

	"github.com/coder/websocket"
)

// ClientOptions controls the transport used for client-side relay
// connections (listener control channel, sender rendezvous, and the
// listener's outbound rendezvous dial).
//
// The zero value uses the secure wss:// scheme via http.DefaultClient and
// is compatible with real Azure Relay.
type ClientOptions struct {
	// TLSConfig, when non-nil, overrides the TLS settings used for the
	// wss dial. Typically used to set InsecureSkipVerify for mock or
	// self-signed relays.
	TLSConfig *tls.Config
}

// wssBase returns the URL prefix for relay URLs given this client's
// endpoint host[:port]. aztunnel only dials TLS-protected relays, so
// the scheme is always "wss".
func (o ClientOptions) wssBase(endpoint string) string {
	return "wss://" + endpoint
}

// dialOptions returns websocket.DialOptions suitable for the configured
// transport. Returns nil when no override is needed (zero options →
// websocket.Dial defaults).
//
// When TLSConfig is set, the returned options carry a custom *http.Client
// whose transport is a clone of http.DefaultTransport — this preserves
// proxy, dialer, and idle-timeout defaults that production deployments
// may depend on, instead of constructing a bare new transport. The
// supplied TLSConfig is cloned per dial so concurrent dials don't share
// mutable state (e.g. http.Transport's lazy ALPN initialization may
// touch TLSConfig.NextProtos).
func (o ClientOptions) dialOptions() *websocket.DialOptions {
	if o.TLSConfig == nil {
		return nil
	}
	tr := defaultTransportClone()
	tr.TLSClientConfig = o.TLSConfig.Clone()
	return &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: tr},
	}
}

// defaultTransportClone returns a clone of http.DefaultTransport, or a
// fresh *http.Transport if the default has been replaced with something
// that isn't an *http.Transport (defensive, should not happen in
// practice).
func defaultTransportClone() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{}
}
