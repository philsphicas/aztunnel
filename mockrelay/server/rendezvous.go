package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
type pendingRendezvous struct {
	senderWS *websocket.Conn

	once       sync.Once
	listenerWS *websocket.Conn // set only when claim() wins; nil if aborted
	ready      chan struct{}   // closed by claim() OR abort()
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
//  1. Pre-upgrade: verify ≥1 listener; else return HTTP 404 so
//     DialWithRetry can back off.
//  2. Upgrade the sender WS.
//  3. Build the rendezvous URL.
//  4. Register the pending entry BEFORE writing the accept message so a
//     fast listener cannot race.
//  5. Write accept to a chosen listener (round-robin). On write failure,
//     fall back to the next listener until either success or no more
//     listeners remain.
//  6. Wait for the listener to dial within RendezvousTimeout. On
//     timeout/failure, close the sender WS cleanly.
//  7. Bridge.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request, entity string) {
	if err := s.validateSAS(r); err != nil {
		s.log.Warn("sender auth failed", "entity", entity, "remote", r.RemoteAddr, "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.hub.hasControls(entity) {
		// Match Azure Relay's "no listener" semantics. The aztunnel
		// sender (DialWithRetry) retries on this status.
		http.Error(w, "no active listener", http.StatusNotFound)
		return
	}

	// Reserve a rendezvous slot pre-upgrade so the cap is enforced
	// before any websocket resources are allocated. DialWithRetry
	// also retries 503, so callers see this as transient backpressure.
	if !s.hub.tryReserve(entity, s.cfg.MaxConnections) {
		s.log.Warn("max connections cap reached", "entity", entity, "cap", s.cfg.MaxConnections)
		http.Error(w, "too many concurrent rendezvous", http.StatusServiceUnavailable)
		return
	}
	defer s.hub.release(entity)

	senderWS, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("sender upgrade failed", "entity", entity, "error", err)
		return
	}
	senderWS.SetReadLimit(bridgeReadLimit)
	// We own the senderWS from here on. If anything below fails we must
	// close it with a status code that the sender surfaces as an error
	// rather than a hang.
	closeWithErr := func(reason string) {
		_ = senderWS.Close(websocket.StatusInternalError, reason)
	}

	id, err := newRendezvousID()
	if err != nil {
		s.log.Warn("rendezvous id failed", "error", err)
		closeWithErr("server error")
		return
	}

	rendezvousURL, err := s.rendezvousURL(r, entity, id)
	if err != nil {
		s.log.Warn("rendezvous URL build failed", "error", err)
		closeWithErr("server error")
		return
	}

	pending := &pendingRendezvous{
		senderWS:   senderWS,
		ready:      make(chan struct{}),
		bridgeDone: make(chan struct{}),
		senderTook: make(chan struct{}),
	}
	// Register BEFORE writing accept so the listener can never race the
	// hub lookup.
	s.hub.addPending(entity, id, pending)
	defer s.hub.removePending(entity, id)
	defer close(pending.bridgeDone)

	// Try each active listener in round-robin order until one accepts the
	// write. We must not punt back to the sender's retry loop (DialWithRetry
	// only retries pre-upgrade 404/503).
	controls := s.hub.snapshotControls(entity)
	if len(controls) == 0 {
		s.log.Warn("no listeners at write time", "entity", entity)
		closeWithErr("no listener available")
		return
	}

	ctx := r.Context()
	var writeErr error
	for _, c := range controls {
		writeErr = c.writeAccept(ctx, rendezvousURL, id)
		if writeErr == nil {
			break
		}
		s.log.Warn("accept write failed; trying next listener", "entity", entity, "error", writeErr)
	}
	if writeErr != nil {
		s.log.Warn("all listeners failed accept write", "entity", entity)
		closeWithErr("listener unavailable")
		return
	}

	// Wait for the listener to dial, with a timeout. RendezvousTimeout
	// guards against a listener that received the accept message but
	// never followed up. abort() also closes ready when the listener
	// upgrade fails, so we wake immediately in that case.
	waitCtx, waitCancel := context.WithTimeout(ctx, s.cfg.RendezvousTimeout)
	defer waitCancel()
	select {
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			s.log.Warn("rendezvous timeout", "entity", entity, "id", id)
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
	bridgeErr := bridgeWS(ctx, senderWS, listenerWS)
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
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request, entity, id string) {
	// Fault injection: optionally sleep before completing the upgrade.
	// Applied to every accept for the lifetime of the Server. Honors
	// the request context so server shutdown is not blocked on a
	// pending delay.
	if d := time.Duration(s.faults.acceptDelay.Load()); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	pending := s.hub.takePending(entity, id)
	if pending == nil {
		http.Error(w, "unknown rendezvous id", http.StatusNotFound)
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
