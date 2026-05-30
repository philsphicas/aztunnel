package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DelayProfile parameterizes the synthetic sleeps mockrelay inserts at
// every step of the hyco rendezvous protocol so the in-process mock
// reproduces the wall-clock shape we measured against real Azure Relay
// (see docs/internals/sequences/) rather than returning instantly. The
// zero value (DelayProfileZero) applies no synthetic delay and is the
// default for tests that don't care about timing fidelity.
//
// The struct has three groups of knobs:
//
//   - Per-lane one-way latencies (SLatency, LLatency). The relay
//     sleeps for the appropriate multiple of these to model the
//     sender↔relay and listener↔relay wire transit time. "Lane"
//     means the network path from one of the two clients to the
//     relay; both lanes are independent so the profile can model
//     either-side-remote and both-sides-remote configurations.
//
//   - Per-fresh-dial DNSLookup. Go does no DNS caching across separate
//     net.Dial calls, so every cold WebSocket upgrade (handleListen,
//     handleConnect, handleAccept) pays a fresh A+AAAA resolution.
//
//   - Per-handler relay-internal costs (AuthInternal, MatchMakeInternal).
//     These model the wedge between the request landing at the relay
//     and the response being emitted that is NOT explained by lane
//     transit: SAS-token validation for AuthInternal, listener lookup
//     plus cross-relay-node dispatch for MatchMakeInternal.
type DelayProfile struct {
	// SLatency is the one-way wire transit time between the sender
	// and the relay. Used to model TCP+TLS+WS upgrade hops on the
	// sender side during rendezvous setup, and (combined with
	// LLatency) the end-to-end propagation of each bridge message.
	// Zero disables the synthetic delay.
	SLatency time.Duration

	// LLatency is the one-way wire transit time between the listener
	// and the relay. Used to model TCP+TLS+WS upgrade hops on the
	// listener side, the accept-frame transit, and (combined with
	// SLatency) the end-to-end propagation of each bridge message.
	// Zero disables the synthetic delay.
	LLatency time.Duration

	// DNSLookup is the cost of a fresh hostname resolution. Applied
	// once at the entry of each handler (handleListen, handleConnect,
	// handleAccept), because every handler corresponds to a fresh
	// client-side net.Dial and the Go resolver does no DNS caching by
	// default. Zero disables the synthetic delay.
	DNSLookup time.Duration

	// AuthInternal models the relay-side cost of SAS-token validation
	// (token parse + signature check + hub-routing-table lookup).
	// Applied in handleListen and handleConnect, both of which carry
	// a sb-hc-token query parameter. NOT applied in handleAccept —
	// the accept-id is the auth there; no token is present.
	AuthInternal time.Duration

	// MatchMakeInternal models the relay-side cost of locating the
	// listener's control session, RPC-ing to the listener-owning
	// relay node if needed, and constructing the accept frame to write
	// onto the listener's control WS. Applied only in handleConnect,
	// in addition to AuthInternal (the two costs sum in handleConnect
	// because matchmake happens after auth).
	MatchMakeInternal time.Duration
}

// Hop accounting constants. The decomposition comes from wireshark
// captures (see docs/internals/sequences/port-forward.md): a cold
// hyco WS upgrade involves a 4-hop TCP+TLS handshake, a 5th hop that
// carries the TLS Finished + WS GET (the moment the SAS token reaches
// the relay), and a 6th hop carrying the 101 response. The accept
// frame is one hop on the listener's existing control WS.
const (
	hopsHandshake   = 4 // legs 1-4: SYN, SYN+ACK, TLS ClientHello, TLS ServerHello
	hopsWSGet       = 1 // leg 5: TLS Fin + WS GET (SAS token arrives at relay)
	hopsResponse    = 1 // leg 6: 101 / 401 / 503 / 404 / etc.
	hopsAcceptFrame = 1 // accept frame written on listener's existing control WS
)

// DelayProfileZero applies no synthetic delay anywhere. This is the
// Go zero value of DelayProfile, named for clarity at call sites.
var DelayProfileZero = DelayProfile{}

// DelayProfileDefault is the recommended profile for e2e-style runs
// that want the mock to approximate the wall-clock shape we observe
// against real Azure Relay from CI. Values are tunable — see
// docs/internals/sequences/ for the wireshark-based derivation.
//
// Predicted per-rendezvous wall-clock with these values (rendezvous
// only, no bridge data): 2*DNSLookup + (hopsHandshake + hopsWSGet)*SLatency
// + (hopsHandshake + hopsWSGet + hopsAcceptFrame)*LLatency
// + hopsResponse*max(S,L) + AuthInternal + MatchMakeInternal.
// Each bridge message in flight then pays SLatency + LLatency one-way.
var DelayProfileDefault = DelayProfile{
	SLatency:          30 * time.Millisecond,
	LLatency:          30 * time.Millisecond,
	DNSLookup:         40 * time.Millisecond,
	AuthInternal:      10 * time.Millisecond,
	MatchMakeInternal: 50 * time.Millisecond,
}

// registry is the single source of truth mapping canonical profile
// names to DelayProfiles. Selection sites — the E2E_DELAY env toggle,
// docs, and any future CLI or test-matrix axis — resolve through
// ProfileByName / ProfileNames rather than referencing the package
// vars directly, so adding a profile is a one-line change here that
// every consumer picks up automatically. Keep keys lowercase and
// hyphen-free so they read cleanly as env-var values and sub-test
// path segments.
var registry = map[string]DelayProfile{
	"zero":    DelayProfileZero,
	"default": DelayProfileDefault,
}

// ProfileByName returns the named profile from the registry. The error
// names the unknown profile and lists the known names (sorted) so a
// typo at a selection site fails loudly instead of silently selecting
// the wrong timing model.
func ProfileByName(name string) (DelayProfile, error) {
	p, ok := registry[name]
	if !ok {
		return DelayProfile{}, fmt.Errorf("unknown delay profile %q; known profiles: %s",
			name, strings.Join(ProfileNames(), ", "))
	}
	return p, nil
}

// ProfileNames returns the registered profile names in sorted order.
// Drives stable error messages and lets docs or a test-matrix axis
// enumerate the profiles from the single registry source of truth.
func ProfileNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// WithDelayProfile arms the per-lane synthetic-delay model. The zero
// profile applies no delay anywhere — pass DelayProfileDefault (or
// build your own) for fidelity. Only effective when set via
// NewServerForTesting; the production constructor never installs a
// profile, so a Server returned from NewServer is always profile-less.
// Returns an error if any field is negative — negative durations don't
// model a meaningful relay behaviour and would silently distort the
// drain budget in pipelinedCopy.
func WithDelayProfile(p DelayProfile) Option {
	return func(s *Server) error {
		if err := p.validate(); err != nil {
			return fmt.Errorf("WithDelayProfile: %w", err)
		}
		s.delayProfile = p
		return nil
	}
}

// validate rejects negative durations on any DelayProfile field.
// sleepContext already treats d <= 0 as zero, but a negative SLatency
// or LLatency would shrink the pipelinedCopy drain budget below the
// 100 ms floor and could cause premature in-flight frame loss on
// source close. Catching it at the Option boundary is cheaper than
// scattering clamps through the call sites.
func (p DelayProfile) validate() error {
	for _, f := range []struct {
		name string
		d    time.Duration
	}{
		{"SLatency", p.SLatency},
		{"LLatency", p.LLatency},
		{"DNSLookup", p.DNSLookup},
		{"AuthInternal", p.AuthInternal},
		{"MatchMakeInternal", p.MatchMakeInternal},
	} {
		if f.d < 0 {
			return fmt.Errorf("%s must be non-negative, got %v", f.name, f.d)
		}
	}
	return nil
}

// sleepContext sleeps for d unless ctx is cancelled first. Returns
// true if it slept the full duration, false on context cancellation.
// A non-positive d returns true immediately.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
