package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// bridgeReadLimit is the maximum WebSocket message size the server will
// relay in either direction. Default coder/websocket per-message read
// limit is 32 KiB; aztunnel clients write at most ~32 KiB per message
// (internal/relay/bridge.go), but we bump this higher on the server
// side so that future or third-party clients with larger messages still
// flow through.
const bridgeReadLimit = 16 * 1024 * 1024 // 16 MiB

// controlReadLimit caps messages on the listener control channel. Real
// traffic is JSON renewToken envelopes of a few hundred bytes — a much
// lower cap than bridgeReadLimit reduces the DoS surface available to
// unauthenticated clients.
const controlReadLimit = 64 * 1024 // 64 KiB

// controlSession holds the per-listener state for an active control
// channel WebSocket.
type controlSession struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

// writeJSON serializes msg to JSON and writes it as a text WebSocket
// message. writes are serialized via writeMu so concurrent writers don't
// interleave frames.
func (c *controlSession) writeJSON(ctx context.Context, msg interface{}) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// handleListen accepts the listener control channel WebSocket and reads
// messages from it (ignoring renewToken). It blocks until the WS is
// closed or the read loop hits an error.
//
// Idle-timeout handling: a one-element buffered channel signals
// "something happened on the connection" — both incoming data messages
// (post-Read) and incoming ping frames (via OnPingReceived). A
// background watchdog goroutine resets a timer on each notification
// and closes the WS when the timer fires, which in turn unblocks
// ws.Read. This approach correctly treats pings as activity (matching
// the documented behavior), whereas the previous per-iter
// context.WithTimeout did not.
//
// DelayProfile timing: handleListen models the wire as DNS lookup +
// hopsHandshake legs + hopsWSGet leg before the listener's SAS token
// arrives at the relay; then AuthInternal for token validation; then
// hopsResponse leg for the 101 (or error) reply. All sleeps honor the
// request context so server shutdown is not blocked.
func (s *Server) handleListen(w http.ResponseWriter, r *http.Request, entity string) {
	ctx := r.Context()
	p := s.delayProfile
	// Pre-auth wire transit: DNS lookup, TCP+TLS handshake, then the
	// TLS Fin + WS GET that delivers the SAS token to the relay.
	if !sleepContext(ctx, p.DNSLookup+hopsHandshake*p.LLatency+hopsWSGet*p.LLatency) {
		return
	}
	// Relay-side auth cost runs in parallel with the response leg in
	// the real relay, but we serialise here so AuthInternal is
	// observable as a separate knob.
	if !sleepContext(ctx, p.AuthInternal) {
		return
	}
	if err := s.validateSAS(r); err != nil {
		s.log.Warn("listener auth failed", "entity", entity, "remote", r.RemoteAddr, "error", err)
		_ = sleepContext(ctx, hopsResponse*p.LLatency)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Fault injection: optionally reject the next authenticated control
	// dial with 503 to exercise the listener's control-dial failure
	// code path. Single-shot; we check after auth so unauthenticated
	// probes cannot consume the fault.
	if s.faults.rejectControlDial.CompareAndSwap(true, false) {
		s.log.Debug("fault: rejecting control dial", "entity", entity)
		_ = sleepContext(ctx, hopsResponse*p.LLatency)
		http.Error(w, "service unavailable (fault injection)", http.StatusServiceUnavailable)
		return
	}
	// 101 transit leg: model the response trip back to the listener
	// before we emit the upgrade. The library completes the upgrade
	// synchronously; the wire model owns the transit.
	if !sleepContext(ctx, hopsResponse*p.LLatency) {
		return
	}
	activity := make(chan struct{}, 1)
	notify := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OnPingReceived: func(_ context.Context, _ []byte) bool {
			notify()
			return true // let library send the pong
		},
	})
	if err != nil {
		s.log.Warn("listener upgrade failed", "entity", entity, "error", err)
		return
	}
	ws.SetReadLimit(controlReadLimit)
	defer func() { _ = ws.CloseNow() }()

	sess := &controlSession{ws: ws}
	s.hub.addControl(entity, sess)
	defer s.hub.removeControl(entity, sess)

	s.log.Info("listener connected", "entity", entity, "remote", r.RemoteAddr)
	defer s.log.Info("listener disconnected", "entity", entity, "remote", r.RemoteAddr)

	if s.cfg.ListenerIdleTimeout > 0 {
		// handlerDone is closed when this handler returns. It signals
		// the watchdog goroutine to exit promptly even when r.Context()
		// is not cancelled (a hijacked WebSocket can outlive the
		// underlying request context if the peer disconnects).
		handlerDone := make(chan struct{})
		watchdogDone := make(chan struct{})
		go func() {
			defer close(watchdogDone)
			timer := time.NewTimer(s.cfg.ListenerIdleTimeout)
			defer timer.Stop()
			for {
				select {
				case <-timer.C:
					s.log.Info("listener idle timeout", "entity", entity)
					_ = ws.Close(websocket.StatusPolicyViolation, "idle timeout")
					return
				case <-activity:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(s.cfg.ListenerIdleTimeout)
				case <-handlerDone:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
		// Single defer that runs LIFO-first relative to subsequent
		// defers in this scope. Sequencing inside guarantees the
		// watchdog observes the signal before we wait on it.
		defer func() {
			close(handlerDone)
			<-watchdogDone
		}()
	}

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			// Normal closures and context cancellation all end the loop.
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			return
		}
		notify()
		// Parse the message to recognize known shapes (renewToken) and
		// log unknown shapes at debug level. We ignore renewToken
		// content in v1 (no token validation).
		var msg struct {
			RenewToken *struct {
				Token string `json:"token"`
			} `json:"renewToken,omitempty"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			s.log.Debug("listener sent non-JSON control message", "entity", entity, "len", len(data))
			continue
		}
		if msg.RenewToken != nil {
			s.log.Debug("listener renewed token", "entity", entity)
			// Fault injection: close the control WS as soon as we
			// observe a renewToken. Single-shot. The listener's
			// renewLoop / pingLoop will surface this as a forced
			// reconnect on its next read or write.
			if s.faults.closeControlOnRenew.CompareAndSwap(true, false) {
				s.log.Debug("fault: closing control on renew", "entity", entity)
				_ = ws.Close(websocket.StatusGoingAway, "fault: close on renew")
				return
			}
			continue
		}
		s.log.Debug("listener sent unrecognized control message", "entity", entity, "len", len(data))
	}
}

// acceptMessage matches the JSON shape the aztunnel listener expects on
// its control channel. See internal/relay/control.go:139-166.
type acceptMessage struct {
	Accept acceptBody `json:"accept"`
}

type acceptBody struct {
	Address        string            `json:"address"`
	ID             string            `json:"id"`
	ConnectHeaders map[string]string `json:"connectHeaders"`
}

// writeAccept sends the accept message to a chosen control session,
// instructing it to dial the rendezvous URL. Returns the error from the
// write so callers can fall back to another listener.
//
// DelayProfile timing: the accept frame is one hop on the listener's
// existing control WS. The sleep is paid BEFORE acquiring writeMu so
// concurrent writers don't see their own latency stack on top of the
// previous writer's transit time.
func (c *controlSession) writeAccept(ctx context.Context, p DelayProfile, addr, id string) error {
	if !sleepContext(ctx, hopsAcceptFrame*p.LLatency) {
		return ctx.Err()
	}
	msg := acceptMessage{
		Accept: acceptBody{
			Address:        addr,
			ID:             id,
			ConnectHeaders: map[string]string{},
		},
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.writeJSON(writeCtx, msg)
}
