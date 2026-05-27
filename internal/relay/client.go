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
	// knobs are overridden. CurvePreferences defaults to
	// relayCurvePreferences (P-384 first) when the caller leaves it
	// empty; a caller-supplied non-empty list is preserved.
	TLSConfig *tls.Config
}

// sessionCache is the process-wide TLS client session cache shared by
// every aztunnel relay dial (this package and internal/arc). Sharing
// one cache means a listener's many accept dials and a sender's many
// rendezvous dials to the same Azure Relay frontend reuse session
// tickets and skip the full handshake on repeat dials. The LRU is
// safe for concurrent use and is bounded by NewLRUClientSessionCache.
var sessionCache = tls.NewLRUClientSessionCache(0)

// relayCurvePreferences is the default TLS 1.3 key-exchange group set
// used for relay dials when the caller has not supplied their own
// CurvePreferences.
//
// Azure Relay's TLS frontends prefer secp384r1 (CurveP384) for the
// initial key share. Go's default supportedCurves list leads with
// X25519MLKEM768 and X25519 (and, on Go 1.26, two additional
// SecP*MLKEM* PQ hybrids), so an unconfigured client sends a
// ClientHello whose key_share is for X25519MLKEM768 (+ X25519
// fallback). Azure rejects this with a HelloRetryRequest naming
// P-384, costing a full extra network round trip on every cold relay
// dial.
//
// Important Go-internal behavior: as of Go 1.24+, the *order* of the
// user-supplied tls.Config.CurvePreferences slice is ignored. Go
// filters its internal default list to retain only the entries the
// user named and uses the (default-order) first surviving entry to
// pick the curve for the initial key_share. The doc on Config
// .CurvePreferences also warns that the implicit key_share selection
// "may change in the future" — see golang/go#69393, which signals
// the Go team intends to take full control of which key_shares get
// sent. The most defensive position against that future shift is to
// list a single curve: with only one allowed group, Go has no choice
// but to send a key_share for it (or for something that includes it
// as a fallback share).
//
// Therefore: a single-element list with CurveP384. No CurveP521 or
// other fallback — if Azure ever rotates its preference, the e2e
// suite against the real frontend will catch it loudly, which is
// preferable to silently masking a regression behind a longer
// supported_groups list that would HRR to whatever curve survived.
//
// Trade-off: this opts the relay channel out of the X25519MLKEM768
// post-quantum hybrid. Azure Relay clearly does not support that
// hybrid today (that is the source of the HRR), so there is no PQ
// protection to lose at present. Including SecP384r1MLKEM1024 would
// also send a P-384 fallback share alongside the hybrid and avoid
// HRR, but it bloats the ClientHello from ~300 bytes to ~1.8 KB
// (extra TCP segments), which is exactly the round-trip overhead
// this default is meant to remove. Revisit if/when Azure Relay
// advertises PQ support.
var relayCurvePreferences = []tls.CurveID{tls.CurveP384}

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
// cache, forces a TLS 1.3 minimum, and (when the caller has not
// supplied their own) installs relayCurvePreferences so the initial
// ClientHello key_share is P-384 and Azure Relay does not respond
// with a HelloRetryRequest.
//
// headers may be nil; baseTLS may be nil. When baseTLS is nil and the
// cloned transport already carries a TLSClientConfig (test harnesses
// install one on http.DefaultTransport to inject InsecureSkipVerify
// for self-signed test servers), that config is carried forward —
// then the cache, MinVersion, and (if empty) CurvePreferences are
// stamped on top, overriding any caller-supplied values for the cache
// and MinVersion fields.
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
//
// CurvePreferences defaults to relayCurvePreferences (P-384-first,
// see that var's doc) when base.CurvePreferences is empty, to avoid
// a one-RTT HelloRetryRequest from Azure Relay on every cold dial. A
// caller-supplied non-empty CurvePreferences slice is preserved — but
// any list whose default-order first survivor is not CurveP384 will
// reintroduce the HRR against real Azure Relay.
func tlsConfigForDial(base *tls.Config) *tls.Config {
	var cfg *tls.Config
	if base != nil {
		cfg = base.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.ClientSessionCache = sessionCache
	cfg.MinVersion = tls.VersionTLS13
	if len(cfg.CurvePreferences) == 0 {
		cfg.CurvePreferences = relayCurvePreferences
	}
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
