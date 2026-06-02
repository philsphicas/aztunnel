package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// pendingRendezvous represents a sender that has been upgraded and is
// awaiting the listener to dial the rendezvous URL.
//
// Ownership of listenerWS, once set, is handed off from the listener
// goroutine to the sender goroutine via the senderTook channel:
//   - claim() (called from handleAccept) sets listenerWS and closes ready.
//   - abort() (called from handleAccept on upgrade failure) closes ready
//     without setting listenerWS — the sender then bails out.
//   - handleConnect closes senderTook *just before* bridging — at that
//     point handleAccept's defer treats listenerWS as taken.
//   - If senderTook is never closed (timeout/error path), handleAccept's
//     defer is responsible for closing listenerWS.
//
// paired is a separate timing barrier closed by handleAccept the
// instant takePending succeeds — BEFORE handleAccept's lane-side
// hopsResponse sleep. Sender's handleConnect waits on paired (rather
// than on ready) so the sender's hopsResponse 101 transit runs in
// parallel with handleAccept's own hopsResponse 101 transit, matching
// the wire-observed dual-101 timing. paired is closed exactly once
// (handleAccept owns it and never re-runs takePending).
type pendingRendezvous struct {
	senderWS *websocket.Conn

	once       sync.Once
	listenerWS *websocket.Conn // set only when claim() wins; nil if aborted
	ready      chan struct{}   // closed by claim() OR abort()
	paired     chan struct{}   // closed by handleAccept right after takePending
	bridgeDone chan struct{}   // closed when handleConnect returns
	senderTook chan struct{}   // closed by handleConnect just before bridging
}

// claim associates a listener WS with this pending entry. Returns false
// if abort() or another claim() already won the race. The once guard is
// the single source of truth — hub.takePending also defends, but the
// once makes any further misuse a no-op rather than a corruption.
func (p *pendingRendezvous) claim(listenerWS *websocket.Conn) bool {
	ok := false
	p.once.Do(func() {
		p.listenerWS = listenerWS
		close(p.ready)
		ok = true
	})
	return ok
}

// abort signals to handleConnect that no listener will ever pair with
// this rendezvous (typically because the listener's WebSocket upgrade
// failed). The sender is woken immediately rather than waiting for the
// rendezvous timeout. After abort(), pending.listenerWS stays nil.
func (p *pendingRendezvous) abort() {
	p.once.Do(func() {
		close(p.ready)
	})
}

// handleConnect handles the sender's connect WebSocket. The flow:
//  1. DelayProfile entry sleeps (DNS, handshake, WSGet) so the sender's
//     token "arrives" at the relay at the modeled wire time.
//  2. authCost + validateToken — the auth-validation cost (AuthInternal
//     for SAS, EntraValidate for Entra) then the matching validator;
//     401 on failure (pre-upgrade, with a hopsResponse leg paid before
//     the body is written).
//  3. tryReserve (per-entity cap) — 503 on failure.
//  4. Build id + rendezvous URL.
//  5. MatchMakeInternal + lookup listener controls. Empty → 404
//     (matches Azure Relay's no-listener semantics; DialWithRetry retries).
//  6. addPending BEFORE writing accept so handleAccept can never race
//     the lookup.
//  7. writeAccept on a chosen listener (per-call hopsAcceptFrame*L
//     sleep). All listeners failed → 503.
//  8. Wait for handleAccept to reach takePending (pending.paired). On
//     RendezvousTimeout, 504 pre-upgrade.
//  9. hopsResponse*S sleep — models the 101 transit to the sender,
//     running in parallel with handleAccept's own response leg.
//  10. Pre-Accept defensive re-check: if pending.ready has closed with
//     listenerWS still nil, the listener's WS Accept failed; 503
//     pre-upgrade rather than 101-then-close.
//  11. websocket.Accept → 101 emitted.
//  12. Wait for pending.ready (listener WS ready or aborted).
//  13. Hand off ownership of listenerWS via close(senderTook); bridge.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request, entity string) {
	ctx := r.Context()
	p := s.delayProfile

	// Pre-auth wire transit: DNS + TCP+TLS handshake + TLS Fin/WS GET.
	if !sleepContext(ctx, p.DNSLookup+hopsHandshake*p.SLatency+hopsWSGet*p.SLatency) {
		return
	}
	if !sleepContext(ctx, s.authCost(r)) {
		return
	}
	if err := s.validateToken(r); err != nil {
		s.log.Warn("sender auth failed", "entity", entity, "remote", r.RemoteAddr, "error", err)
		_ = sleepContext(ctx, hopsResponse*p.SLatency)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Reserve a rendezvous slot pre-upgrade so the cap is enforced
	// before any websocket resources are allocated. DialWithRetry
	// also retries 503, so callers see this as transient backpressure.
	if !s.hub.tryReserve(entity, s.cfg.MaxConnections) {
		s.log.Warn("max connections cap reached", "entity", entity, "cap", s.cfg.MaxConnections)
		_ = sleepContext(ctx, hopsResponse*p.SLatency)
		http.Error(w, "too many concurrent rendezvous", http.StatusServiceUnavailable)
		return
	}
	defer s.hub.release(entity)

	id, err := newRendezvousID()
	if err != nil {
		s.log.Warn("rendezvous id failed", "error", err)
		_ = sleepContext(ctx, hopsResponse*p.SLatency)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	rendezvousURL, err := s.rendezvousURL(r, entity, id)
	if err != nil {
		s.log.Warn("rendezvous URL build failed", "error", err)
		_ = sleepContext(ctx, hopsResponse*p.SLatency)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// MatchMakeInternal models the relay-side listener-routing cost.
	// We snapshot after the sleep so a fast listener can't race; if
	// no listener exists at this point we return 404 (Azure Relay's
	// no-listener semantics; DialWithRetry retries on 404).
	if !sleepContext(ctx, p.MatchMakeInternal) {
		return
	}
	controls := s.hub.snapshotControls(entity)
	if len(controls) == 0 {
		_ = sleepContext(ctx, hopsResponse*p.SLatency)
		http.Error(w, "no active listener", http.StatusNotFound)
		return
	}

	pending := &pendingRendezvous{
		ready:      make(chan struct{}),
		paired:     make(chan struct{}),
		bridgeDone: make(chan struct{}),
		senderTook: make(chan struct{}),
	}
	// Register BEFORE writing accept so the listener can never race the
	// hub lookup.
	s.hub.addPending(entity, id, pending)
	defer s.hub.removePending(entity, id)
	defer close(pending.bridgeDone)

	// Try each active listener in round-robin order until one accepts
	// the write. Each writeAccept sleeps hopsAcceptFrame*L for the
	// accept-frame transit before acquiring the control WS write lock.
	var writeErr error
	for _, c := range controls {
		writeErr = c.writeAccept(ctx, p, rendezvousURL, id)
		if writeErr == nil {
			break
		}
		s.log.Warn("accept write failed; trying next listener", "entity", entity, "error", writeErr)
	}
	if writeErr != nil {
		s.log.Warn("all listeners failed accept write", "entity", entity)
		_ = sleepContext(ctx, hopsResponse*p.SLatency)
		http.Error(w, "listener unavailable", http.StatusServiceUnavailable)
		return
	}

	// Wait for handleAccept to reach takePending (close(paired)).
	// RendezvousTimeout guards against a listener that received the
	// accept message but never followed up.
	waitCtx, waitCancel := context.WithTimeout(ctx, s.cfg.RendezvousTimeout)
	defer waitCancel()
	select {
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			s.log.Warn("rendezvous timeout (pre-101)", "entity", entity, "id", id)
			_ = sleepContext(ctx, hopsResponse*p.SLatency)
			http.Error(w, "rendezvous timeout", http.StatusGatewayTimeout)
			return
		}
		return // client gone
	case <-pending.paired:
	}

	// 101 transit leg to sender — runs in parallel with handleAccept's
	// own response leg (the listener-side 101). Use waitCtx so that
	// RendezvousTimeout is enforced through the 101 transit too; if
	// the timeout fires here we abort with 504 rather than emitting
	// 101 only to tear it down a moment later. Check the parent ctx
	// first so a parent cancellation (client gone) doesn't get
	// misreported as a rendezvous timeout.
	if !sleepContext(waitCtx, hopsResponse*p.SLatency) {
		if ctx.Err() == nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			s.log.Warn("rendezvous timeout (101 transit)", "entity", entity, "id", id)
			http.Error(w, "rendezvous timeout", http.StatusGatewayTimeout)
		}
		return
	}
	// Pre-Accept defensive re-check: if pending.ready closed during
	// the 101-transit sleep above AND listenerWS is nil, the
	// listener's WS Accept failed on its side. Bail with 503
	// pre-upgrade rather than emitting 101 just to close it.
	select {
	case <-pending.ready:
		if pending.listenerWS == nil {
			s.log.Warn("listener accept failed (pre-101)", "entity", entity, "id", id)
			http.Error(w, "listener accept failed", http.StatusServiceUnavailable)
			return
		}
	default:
		// Listener's accept-dial still in flight; emit 101 and wait
		// for ready below.
	}

	senderWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("sender upgrade failed", "entity", entity, "error", err)
		return
	}
	senderWS.SetReadLimit(bridgeReadLimit)
	pending.senderWS = senderWS
	closeWithErr := func(reason string) {
		_ = senderWS.Close(websocket.StatusInternalError, reason)
	}

	// Wait for listener WS to be ready (claim or abort).
	select {
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			s.log.Warn("rendezvous timeout (post-101)", "entity", entity, "id", id)
			closeWithErr("rendezvous timeout")
			return
		}
		closeWithErr("client gone")
		return
	case <-pending.ready:
	}

	// After ready fires, listenerWS is either set (claim won) or nil
	// (abort won). Reading the field is safe per the Go memory model:
	// the close-of-ready synchronizes-after the assignment in claim().
	listenerWS := pending.listenerWS
	if listenerWS == nil {
		s.log.Warn("listener accept failed", "entity", entity, "id", id)
		closeWithErr("listener accept failed")
		return
	}

	// Take ownership of listenerWS. From this point handleAccept's
	// defer treats the WS as ours and leaves it alone.
	close(pending.senderTook)
	defer func() { _ = listenerWS.CloseNow() }()

	s.log.Info("rendezvous bridge starting", "entity", entity, "id", id)
	bridgeErr := bridgeWS(ctx, p, senderWS, listenerWS)
	if bridgeErr != nil {
		s.log.Debug("rendezvous bridge ended", "entity", entity, "id", id, "error", bridgeErr)
	} else {
		s.log.Debug("rendezvous bridge ended", "entity", entity, "id", id)
	}
}

// handleAccept handles the listener's outbound dial to the rendezvous
// URL. The id query parameter must match a pending entry that hasn't
// been claimed yet. On success the listener WS is paired with the
// sender WS via the pending entry's ready channel; the sender goroutine
// performs the actual bridge.
//
// On WebSocket upgrade failure, abort() wakes the sender immediately so
// it doesn't wait for the full RendezvousTimeout. On successful claim,
// a deferred guard closes the listener WS if and only if the sender
// goroutine never took ownership (which can happen if the
// RendezvousTimeout fires concurrently with the claim).
//
// DelayProfile timing:
//  1. DNS + hopsHandshake*L + hopsWSGet*L before takePending (models
//     the listener's accept-dial wire transit). No SAS validation:
//     sb-hc-id IS the auth.
//  2. close(paired) the instant takePending succeeds — this is the
//     timing barrier the sender's handleConnect waits on so dual-101
//     timing matches the wireshark captures.
//  3. hopsResponse*L for the 101 transit back to the listener, then
//     websocket.Accept.
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request, entity, id string) {
	ctx := r.Context()
	p := s.delayProfile

	// Pre-takePending wire transit. No AuthInternal: accept-dial has
	// no sb-hc-token; sb-hc-id is the auth and is checked instantly
	// against the pending map.
	if !sleepContext(ctx, p.DNSLookup+hopsHandshake*p.LLatency+hopsWSGet*p.LLatency) {
		return
	}
	pending := s.hub.takePending(entity, id)
	if pending == nil {
		_ = sleepContext(ctx, hopsResponse*p.LLatency)
		http.Error(w, "unknown rendezvous id", http.StatusNotFound)
		return
	}
	// Signal paired ASAP so the sender's handleConnect wakes up.
	close(pending.paired)

	// 101 transit leg to listener — runs in parallel with sender's
	// own 101 transit in handleConnect.
	if !sleepContext(ctx, hopsResponse*p.LLatency) {
		// Context cancelled mid-sleep; abort so the sender wakes too.
		pending.abort()
		return
	}

	listenerWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("listener-rendezvous upgrade failed", "entity", entity, "id", id, "error", err)
		pending.abort()
		return
	}
	// Fault injection: emit a configurable close code on the next
	// accept. Single-shot; consumed via Swap(0). We abort the pending
	// entry BEFORE the close handshake so any concurrent sender
	// goroutine waking on pending.ready exits immediately rather than
	// waiting for our Close to drain.
	if code := s.faults.closeCodeOnAccept.Swap(0); code != 0 {
		s.log.Debug("fault: closing accept-side WS with code", "entity", entity, "id", id, "code", code)
		pending.abort()
		_ = listenerWS.Close(websocket.StatusCode(code), "fault: close on accept")
		return
	}
	listenerWS.SetReadLimit(bridgeReadLimit)
	if !pending.claim(listenerWS) {
		_ = listenerWS.Close(websocket.StatusInternalError, "already claimed")
		return
	}
	// If the sender goroutine takes ownership (closes senderTook), it
	// is responsible for closing listenerWS. Otherwise — e.g.
	// RendezvousTimeout fires at the same instant claim succeeds —
	// we close it here so the hijacked TCP socket isn't leaked.
	defer func() {
		select {
		case <-pending.senderTook:
		default:
			_ = listenerWS.CloseNow()
		}
	}()
	// We must NOT return from this handler until the bridge has
	// finished — returning would let the http server close the
	// underlying connection out from under the bridge goroutine.
	select {
	case <-pending.bridgeDone:
	case <-r.Context().Done():
	}
}

// rendezvousURL builds the absolute URL that the listener will dial to
// open its half of the rendezvous WebSocket. Format:
//
//	{scheme}://{host}/$hc/{entity}?sb-hc-action=accept&id={id}
//
// When Config.PublicURL is set we use that as the base; otherwise we
// derive scheme + host from the inbound request (Host header and TLS
// presence), which works for local development and tests.
func (s *Server) rendezvousURL(r *http.Request, entity, id string) (string, error) {
	scheme, host, err := s.publicSchemeHost(r)
	if err != nil {
		return "", err
	}
	switch scheme {
	case "http":
		scheme = "ws"
	case "https":
		scheme = "wss"
	}
	u := &url.URL{
		Scheme: scheme,
		Host:   host,
		// Path holds the decoded form; RawPath holds the escaped form
		// that url.URL.String() will emit verbatim. We must set both so
		// that "/" characters inside the entity name are emitted as
		// %2F on the wire (url.URL.String() otherwise treats them as
		// path separators).
		Path:     "/$hc/" + entity,
		RawPath:  "/$hc/" + url.PathEscape(entity),
		RawQuery: "sb-hc-action=accept&id=" + url.QueryEscape(id),
	}
	// url.URL.String() emits RawPath if and only if its decoded form
	// equals Path; url.PathEscape is the inverse of url.PathUnescape,
	// so the consistency check holds for any entity string.
	return u.String(), nil
}

// publicSchemeHost returns the scheme and host to use for minted URLs.
// PublicURL takes priority; otherwise derive from the request.
func (s *Server) publicSchemeHost(r *http.Request) (string, string, error) {
	if s.cfg.PublicURL != "" {
		u, err := url.Parse(s.cfg.PublicURL)
		if err != nil {
			return "", "", fmt.Errorf("parse PublicURL: %w", err)
		}
		return strings.ToLower(u.Scheme), u.Host, nil
	}
	host := r.Host
	if host == "" {
		return "", "", errors.New("no Host header and no PublicURL configured")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme, host, nil
}
