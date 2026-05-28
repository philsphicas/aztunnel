// Package server implements an Azure-Relay-compatible Hybrid
// Connections server. It speaks the subset of the wire protocol that
// aztunnel clients use:
//
//   - Listeners open  /$hc/<entity>?sb-hc-action=listen   (control WS).
//   - Senders open    /$hc/<entity>?sb-hc-action=connect  (sender's half
//     of the rendezvous).
//   - The server replies on the listener's control channel with a JSON
//     accept message containing a rendezvous URL the listener dials
//     (sb-hc-action=accept&id=<UUID>), giving the listener half of the
//     rendezvous.
//   - The server then bridges the two halves byte-for-byte, preserving
//     WebSocket message boundaries.
//
// This package is a mock relay intended for local development, CI, and
// offline end-to-end testing of aztunnel — for example, exercising
// connect/listen flows without an Azure Relay subscription. It is *not*
// a drop-in replacement for Azure Relay and is not intended for
// production traffic: SAS validation uses a fixed dummy key (printed by
// aztunnel-relay on startup) and the server has no listener auth/authz
// or HA/clustering.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Config holds parameters for a relay Server. The zero value is usable
// for tests — NewServer fills in defaults for Logger, ListenerIdleTimeout,
// and RendezvousTimeout, and MaxConnections=0 (unlimited) is a valid
// setting. PublicURL is the only field that may need to be set
// explicitly when running behind a proxy or TLS terminator.
type Config struct {
	// Logger is used for server-side logging. If nil, slog.Default() is used.
	Logger *slog.Logger

	// MaxConnections is the maximum number of concurrent rendezvous
	// connections per entity (0 = unlimited). Senders that would
	// exceed the cap receive HTTP 503 pre-upgrade, which the
	// aztunnel client treats as a transient retryable error.
	MaxConnections int

	// ListenerIdleTimeout is the maximum time a listener control
	// channel may go without any activity. Both data messages and
	// ping frames count as activity, so a healthy client that pings
	// at a shorter interval will keep the channel open indefinitely.
	// Zero defaults to 2 minutes, matching Azure Relay's model.
	ListenerIdleTimeout time.Duration

	// RendezvousTimeout is how long a sender waits for the chosen
	// listener to dial the rendezvous URL after receiving the accept
	// message. Zero defaults to 30 seconds.
	RendezvousTimeout time.Duration

	// PublicURL is the absolute base URL used when minting rendezvous
	// addresses. Must use scheme http/https/ws/wss and include host
	// and (when non-default) port. When empty, rendezvous URLs are
	// derived from the inbound request's Host header and the server's
	// TLS state. This is fine for local development and tests but
	// must be set behind a reverse proxy or TLS terminator.
	PublicURL string

	// SASKeyName and SASKey configure the dummy Shared Access Signature
	// the server validates on listen and connect requests. When empty,
	// DefaultSASKeyName / DefaultSASKey are used. The values are NOT
	// secret — they exist so aztunnel clients can authenticate against
	// the mock with real --key-name/--key or AZTUNNEL_KEY_NAME/AZTUNNEL_KEY,
	// exercising the real SAS code path end-to-end.
	SASKeyName string
	SASKey     string

	// SkipAuth disables SAS validation entirely. Intended for tests
	// that want to drive the protocol without having to mint tokens
	// (e.g. low-level handler tests). Production aztunnel-relay should
	// never set this — set SASKey / SASKeyName to something specific
	// instead.
	SkipAuth bool
}

// Defaults applied when fields are zero-valued.
const (
	DefaultListenerIdleTimeout = 2 * time.Minute
	DefaultRendezvousTimeout   = 30 * time.Second
)

// Server is a relay server. Use NewServer to construct one and pass
// Handler() to an http.Server (or http.ListenAndServe) to serve.
//
// Server is intended to be allocated once and accessed through a
// *Server; do not copy the value (the embedded faults struct holds
// atomics that go vet would flag on copy).
type Server struct {
	cfg Config
	hub *hub
	log *slog.Logger
	// faults holds fault-injection state set via NewServerForTesting.
	// Zero value means no faults active; the production NewServer
	// constructor never touches it.
	faults faults
	// delayProfile parameterizes the synthetic per-lane delays applied
	// to every relay-side step. Zero value (DelayProfileZero) means no
	// synthetic delay anywhere, matching production behavior. Set via
	// WithDelayProfile in NewServerForTesting; production NewServer
	// never touches it.
	delayProfile DelayProfile
}

// NewServer constructs a Server with the given config. It validates the
// PublicURL if set.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ListenerIdleTimeout == 0 {
		cfg.ListenerIdleTimeout = DefaultListenerIdleTimeout
	}
	if cfg.RendezvousTimeout == 0 {
		cfg.RendezvousTimeout = DefaultRendezvousTimeout
	}
	if cfg.PublicURL != "" {
		if err := validatePublicURL(cfg.PublicURL); err != nil {
			return nil, fmt.Errorf("invalid PublicURL: %w", err)
		}
	}
	if cfg.SASKeyName == "" {
		cfg.SASKeyName = DefaultSASKeyName
	}
	if cfg.SASKey == "" {
		cfg.SASKey = DefaultSASKey
	}
	return &Server{
		cfg: cfg,
		hub: newHub(),
		log: cfg.Logger,
	}, nil
}

// Handler returns the http.Handler that serves all relay endpoints.
// Routes the request based on the URL path and the sb-hc-action query
// parameter:
//
//	GET /$hc/{entity}?sb-hc-action=listen        → control channel
//	GET /$hc/{entity}?sb-hc-action=connect       → sender rendezvous
//	GET /$hc/{entity}?sb-hc-action=accept&id=... → listener rendezvous
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/$hc/", s.handleHC)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Serve runs the server on the given listener until ctx is cancelled.
// If tlsOpts is non-nil the server serves over TLS using the provided
// cert/key. tlsOpts.CertFile and tlsOpts.KeyFile must both be non-empty
// — Go's net/http requires file paths to serve TLS, even when a
// fully-loaded *tls.Config is also provided.
func (s *Server) Serve(ctx context.Context, ln net.Listener, tlsOpts *TLSOptions) error {
	if tlsOpts != nil {
		if tlsOpts.CertFile == "" || tlsOpts.KeyFile == "" {
			return fmt.Errorf("TLSOptions.CertFile and TLSOptions.KeyFile must both be set; use LoadTLSFromFiles or SelfSignedTLS")
		}
	}
	if ln == nil {
		return fmt.Errorf("Serve: listener must not be nil")
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if tlsOpts != nil {
		srv.TLSConfig = tlsOpts.Config
	}

	errc := make(chan error, 1)
	go func() {
		if tlsOpts != nil {
			errc <- srv.ServeTLS(ln, tlsOpts.CertFile, tlsOpts.KeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errc
		return ctx.Err()
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleHC dispatches /$hc/{entity}?sb-hc-action=... requests.
func (s *Server) handleHC(w http.ResponseWriter, r *http.Request) {
	// Use EscapedPath so a %2F inside the entity name survives.
	entity, err := parseEntity(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, "bad path: "+err.Error(), http.StatusBadRequest)
		return
	}

	action := r.URL.Query().Get("sb-hc-action")
	switch action {
	case "listen":
		s.handleListen(w, r, entity)
	case "connect":
		s.handleConnect(w, r, entity)
	case "accept":
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		s.handleAccept(w, r, entity, id)
	default:
		http.Error(w, "unknown sb-hc-action: "+action, http.StatusBadRequest)
	}
}

// hub holds the per-entity state. It is safe for concurrent use.
type hub struct {
	mu       sync.Mutex
	entities map[string]*entityState
}

func newHub() *hub {
	return &hub{entities: map[string]*entityState{}}
}

// entityState holds the listeners and pending rendezvous for one entity
// path. All access is guarded by hub.mu.
type entityState struct {
	controls []*controlSession // active listener control channels
	nextIdx  int               // round-robin cursor into controls
	pending  map[string]*pendingRendezvous
	inflight int // current concurrent rendezvous, for MaxConnections enforcement
}

func (h *hub) entity(name string) *entityState {
	if e, ok := h.entities[name]; ok {
		return e
	}
	e := &entityState{pending: map[string]*pendingRendezvous{}}
	h.entities[name] = e
	return e
}

// snapshotControls returns the current active listeners for an entity in
// round-robin order starting from the next index. The returned slice is
// a copy and safe to iterate without holding hub.mu. The cursor advances
// by 1 to spread load on subsequent calls.
func (h *hub) snapshotControls(entity string) []*controlSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entities[entity]
	if !ok || len(e.controls) == 0 {
		return nil
	}
	n := len(e.controls)
	out := make([]*controlSession, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, e.controls[(e.nextIdx+i)%n])
	}
	e.nextIdx = (e.nextIdx + 1) % n
	return out
}

// hasControls returns true if there is at least one active listener
// registered for the given entity. Used for the pre-upgrade check.
func (h *hub) hasControls(entity string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entities[entity]
	return ok && len(e.controls) > 0
}

func (h *hub) addControl(entity string, c *controlSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entity(entity)
	e.controls = append(e.controls, c)
}

func (h *hub) removeControl(entity string, c *controlSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entities[entity]
	if !ok {
		return
	}
	out := e.controls[:0]
	for _, x := range e.controls {
		if x != c {
			out = append(out, x)
		}
	}
	e.controls = out
}

func (h *hub) addPending(entity, id string, p *pendingRendezvous) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entity(entity)
	e.pending[id] = p
}

// takePending atomically retrieves and removes the pending entry. This
// ensures only one listener can claim a rendezvous ID.
func (h *hub) takePending(entity, id string) *pendingRendezvous {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entities[entity]
	if !ok {
		return nil
	}
	p, ok := e.pending[id]
	if !ok {
		return nil
	}
	delete(e.pending, id)
	return p
}

// removePending unregisters a pending entry by ID (no-op if missing).
// Used by the sender on timeout/failure to clean up.
func (h *hub) removePending(entity, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.entities[entity]; ok {
		delete(e.pending, id)
	}
}

// tryReserve attempts to claim a rendezvous slot for the given entity.
// Returns true if the slot was reserved (the caller must later call
// release to free it). Returns false if the entity is at or above max
// concurrent rendezvous (max == 0 means unlimited).
func (h *hub) tryReserve(entity string, max int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entity(entity)
	if max > 0 && e.inflight >= max {
		return false
	}
	e.inflight++
	return true
}

// release decrements the in-flight rendezvous counter for an entity.
// No-op if the entity is unknown or the counter is already zero.
func (h *hub) release(entity string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.entities[entity]; ok && e.inflight > 0 {
		e.inflight--
	}
}
