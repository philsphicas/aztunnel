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
// The zero value uses the secure wss:// scheme and is compatible with
// real Azure Relay.
type ClientOptions struct {
	// TLSConfig, when non-nil, supplies extra TLS settings used for
	// the wss dial. Typically used to set InsecureSkipVerify for mock
	// or self-signed relays. The shared ClientSessionCache and TLS
	// 1.3 minimum are always applied — caller fields for those two
	// knobs are overridden.
	TLSConfig *tls.Config
}

// sessionCache is the process-wide TLS client session cache shared by
// every aztunnel relay dial (this package and internal/arc). Sharing
// one cache means a listener's many accept dials and a sender's many
// rendezvous dials to the same Azure Relay frontend reuse session
// tickets and skip the full handshake on repeat dials. The LRU is
// safe for concurrent use and is bounded by NewLRUClientSessionCache.
var sessionCache = tls.NewLRUClientSessionCache(0)

// wssBase returns the URL prefix for relay URLs given this client's
// endpoint host[:port]. aztunnel only dials TLS-protected relays, so
// the scheme is always "wss".
func (o ClientOptions) wssBase(endpoint string) string {
	return "wss://" + endpoint
}

// dialOptions returns websocket.DialOptions for this ClientOptions.
// Delegates to WSDialOptions so the relay package's own dials get the
// same shared session cache / TLS-hygiene defaults as callers in
// internal/arc.
func (o ClientOptions) dialOptions() *websocket.DialOptions {
	return WSDialOptions(nil, o.TLSConfig)
}

// WSDialOptions builds *websocket.DialOptions for a wss dial to Azure
// Relay. The returned options carry a per-dial *http.Client whose
// transport is a clone of the *http.Transport that http.DefaultClient
// would have used (preferring http.DefaultClient.Transport when set,
// falling back to http.DefaultTransport — see defaultTransportClone),
// with a TLS configuration that always attaches the shared session
// cache and forces a TLS 1.3 minimum.
//
// headers may be nil; baseTLS may be nil. When baseTLS is nil and the
// cloned transport already carries a TLSClientConfig (test harnesses
// install one on http.DefaultTransport to inject InsecureSkipVerify
// for self-signed test servers), that config is carried forward —
// then the cache and MinVersion are stamped on top, overriding any
// caller-supplied values for those two fields.
//
// The supplied TLSConfig is cloned per dial so concurrent dials don't
// share mutable state (http.Transport's lazy ALPN initialization may
// touch TLSConfig.NextProtos). The ClientSessionCache field is a
// pointer to a shared, concurrency-safe LRU, so cloning still shares
// the same cache across dials — which is what makes resumption work.
func WSDialOptions(headers http.Header, baseTLS *tls.Config) *websocket.DialOptions {
	tr := defaultTransportClone()
	if baseTLS == nil && tr.TLSClientConfig != nil {
		baseTLS = tr.TLSClientConfig
	}
	tr.TLSClientConfig = tlsConfigForDial(baseTLS)

	var c http.Client
	if dc := http.DefaultClient; dc != nil {
		c = *dc
	}
	c.Transport = tr
	return &websocket.DialOptions{
		HTTPHeader: headers,
		HTTPClient: &c,
	}
}

// tlsConfigForDial returns a fresh *tls.Config derived from base with
// the shared ClientSessionCache and a TLS 1.3 minimum stamped on
// unconditionally. aztunnel only dials Azure Relay, which supports
// TLS 1.3 across all regions, so we don't fall back to TLS 1.2.
// Caller-supplied ClientSessionCache and MinVersion are overwritten;
// other fields (notably InsecureSkipVerify for mock-relay tests) are
// preserved.
func tlsConfigForDial(base *tls.Config) *tls.Config {
	var cfg *tls.Config
	if base != nil {
		cfg = base.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.ClientSessionCache = sessionCache
	cfg.MinVersion = tls.VersionTLS13
	return cfg
}

// defaultTransportClone returns a clone of the *http.Transport that
// http.DefaultClient would have used: prefer http.DefaultClient.Transport
// when it is an *http.Transport, otherwise fall back to
// http.DefaultTransport, otherwise return a fresh *http.Transport.
//
// A non-*http.Transport RoundTripper installed on
// http.DefaultClient.Transport (e.g. an observability wrapper) is not
// honored, because TLSClientConfig must be set per dial and arbitrary
// RoundTrippers expose no general way to do that. No caller in this
// codebase installs a wrapped RoundTripper.
func defaultTransportClone() *http.Transport {
	if dc := http.DefaultClient; dc != nil {
		if t, ok := dc.Transport.(*http.Transport); ok {
			return t.Clone()
		}
	}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{}
}
