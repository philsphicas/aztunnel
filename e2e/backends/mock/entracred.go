package mock

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/philsphicas/aztunnel/internal/relay"
	"github.com/philsphicas/aztunnel/mockrelay/server"
)

// fakeEntraCredential is an azcore.TokenCredential that models the
// wall-clock cost of a real Entra ID token acquisition (the AAD round
// trip aztunnel pays inside EntraTokenProvider on a cache miss) without
// contacting any identity authority.
//
// It is delivered through the *real* relay.EntraTokenProvider (via
// relay.NewEntraTokenProviderWithCredential), so every call exercises
// the production cache: single-flight, tokenFresh, and the ExpiresOn/
// RefreshOn refresh window. That is what lets the mock assert the cache
// actually works — break EntraTokenProvider's caching and `calls` jumps
// from 1 to N.
//
// The token string it returns is a genuine SAS token signed with the
// mock relay's default key, because mockrelay/server.validateSAS only
// accepts SharedAccessSignature-shaped tokens. The mock does not compare
// the token audience (sr) against the request URI, so a single fixed
// resource URI signs a token valid for every entity. This is a
// deliberate fidelity trade: we model Entra *client acquisition timing +
// caching + metrics*, NOT Entra cryptography, OAuth scopes, audience
// binding, or server-side RBAC (none of which the mock server can
// validate). The trade is only safe because production EntraTokenProvider
// never parses the returned token string — it trusts AccessToken.ExpiresOn
// directly (internal/relay/auth.go) — so a SAS-shaped string is opaque to
// it. Revisit if EntraTokenProvider ever starts parsing the token body.
type fakeEntraCredential struct {
	// delay is the synthetic per-acquisition latency, modelling the
	// AAD round trip. Applied on every underlying GetToken call (i.e.
	// every cache miss), before the token is returned.
	delay time.Duration

	// calls counts underlying acquisitions. With a working cache this
	// is 1 per provider for the life of a steady-state test; a value
	// of N after N dials means the cache is not being consulted.
	calls atomic.Int64
}

func (c *fakeEntraCredential) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return azcore.AccessToken{}, ctx.Err()
		}
	}
	// Sign a SAS token the mock relay will accept. The audience is a
	// fixed placeholder — validateSAS does not bind sr to the request
	// URI — and outlives the test so the cache treats it as fresh.
	const tokenLifetime = time.Hour
	tok, err := relay.GenerateSASToken(
		"https://mock-entra.invalid/fake",
		server.DefaultSASKeyName,
		server.DefaultSASKey,
		tokenLifetime,
	)
	if err != nil {
		return azcore.AccessToken{}, err
	}
	return azcore.AccessToken{
		Token:     tok,
		ExpiresOn: time.Now().Add(tokenLifetime),
		// RefreshOn left zero: EntraTokenProvider falls back to the
		// ExpiresOn-minus-skew freshness rule, so the token stays a
		// cache hit until ~5 min before expiry (i.e. the whole test).
	}, nil
}

// newFakeEntraProvider builds a real EntraTokenProvider backed by a
// fakeEntraCredential, returning both so callers can assert on the
// credential's call count. Each call produces an independent provider
// with its own cache, mirroring a separate aztunnel process.
func newFakeEntraProvider(delay time.Duration) (relay.TokenProvider, *fakeEntraCredential) {
	cred := &fakeEntraCredential{delay: delay}
	return relay.NewEntraTokenProviderWithCredential(cred), cred
}
