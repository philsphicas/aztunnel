// Package relay implements the Azure Relay Hybrid Connections WebSocket protocol.
//
// It provides token-based authentication (SAS and Entra ID), control channel
// management, sender-side relay dialing, and bidirectional stream bridging.
package relay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// entraRefreshSkew is the safety margin before an access token's ExpiresOn at
// which EntraTokenProvider considers its cached entry stale and refreshes it.
// Set small (5 minutes) so the cache remains useful even when the underlying
// credential returns tokens with short remaining lifetimes — notably
// AzureCLICredential, which passes through whatever the `az` shell-out
// reports, often only 10–15 minutes when az's own MSAL cache is mid-cycle.
// A larger skew here would invalidate the cache on nearly every call in that
// regime, restoring the per-dial `az` shell-out the cache exists to avoid.
//
// This skew does not — and cannot — guarantee that the token handed to
// control.go's renewLoop survives until the next renewal pass: the upstream
// token lifetime is whatever the credential gives us, with or without the
// cache. A short-lifetime token reaching renewLoop is a property of the
// underlying credential, not of the cache.
const entraRefreshSkew = 5 * time.Minute

// entraTokenScope is the OAuth2 scope Azure Relay requires for Entra ID
// authentication. The control plane rejects tokens issued for any other
// audience, so the scope is fixed regardless of the caller's resource URI.
const entraTokenScope = "https://relay.azure.net/.default"

// TokenProvider generates authentication tokens for Azure Relay.
type TokenProvider interface {
	// GetToken returns a token string suitable for the sb-hc-token query
	// parameter or the renewToken control message.
	GetToken(ctx context.Context, resourceURI string) (string, error)
}

// Provider label values for TokenFetchObserver. These are the values
// passed as the `provider` label on aztunnel_token_fetch_seconds /
// _total when WithMetrics wraps a TokenProvider. Defined here so the
// label vocabulary is shared between the wiring side (cmd/aztunnel)
// and the wrapper itself, and not duplicated as string literals.
const (
	ProviderSAS   = "sas"
	ProviderEntra = "entra"
)

// TokenFetchObserver receives one observation per TokenProvider.GetToken
// call when WithMetrics is applied. The metrics package's *Metrics type
// satisfies this interface via its ObserveTokenFetch method; declaring
// the interface here lets internal/relay wrap a provider without
// importing internal/metrics, which would create an import cycle
// (metrics already imports relay).
type TokenFetchObserver interface {
	// ObserveTokenFetch records one GetToken call. result is "ok" for
	// successful fetches and "error" for failures; durationSec is the
	// wall-clock time spent inside the wrapped provider's GetToken.
	ObserveTokenFetch(provider, result string, durationSec float64)
}

// metricsTokenProvider wraps another TokenProvider and reports each
// GetToken call to a TokenFetchObserver. It is intentionally
// transparent to callers: the inner provider's token and error are
// returned unchanged.
//
// Placement matters. Wrapping an EntraTokenProvider (the cached
// provider) means cache-hit returns are observed too, with near-zero
// latency — what an operator dashboard sees is effective end-to-end
// GetToken latency, not pure upstream-credential refresh latency. The
// metric Help text reflects that contract.
type metricsTokenProvider struct {
	inner    TokenProvider
	provider string
	obs      TokenFetchObserver
}

// GetToken delegates to the wrapped provider and reports latency +
// outcome to the observer. The observer is never nil here — WithMetrics
// short-circuits a nil observer at construction time, so this hot path
// pays no per-call branch.
func (p *metricsTokenProvider) GetToken(ctx context.Context, resourceURI string) (string, error) {
	start := time.Now()
	tok, err := p.inner.GetToken(ctx, resourceURI)
	result := "ok"
	if err != nil {
		result = "error"
	}
	p.obs.ObserveTokenFetch(p.provider, result, time.Since(start).Seconds())
	return tok, err
}

// WithMetrics returns a TokenProvider that reports each GetToken call
// to obs as (provider, result, durationSec). provider is a short label
// string such as ProviderSAS or ProviderEntra; result is "ok" or
// "error" depending on the inner call's outcome.
//
// If obs is the plain (untyped) nil, p is returned unchanged — no
// wrapping, no overhead. Callers passing an interface value whose
// underlying concrete pointer is nil (the classic "typed-nil-in-an-
// interface" pitfall) MUST handle that themselves at the call site,
// e.g.:
//
//	if m != nil {
//	    tp = relay.WithMetrics(tp, m, relay.ProviderEntra)
//	}
//
// because WithMetrics cannot tell from the interface value alone
// whether the underlying observer is nil without reflection.
func WithMetrics(p TokenProvider, obs TokenFetchObserver, provider string) TokenProvider {
	if obs == nil {
		return p
	}
	return &metricsTokenProvider{inner: p, provider: provider, obs: obs}
}

// SASTokenProvider generates Shared Access Signature tokens.
type SASTokenProvider struct {
	KeyName string
	Key     string
}

// GetToken generates a SAS token for the given resource URI.
func (p *SASTokenProvider) GetToken(_ context.Context, resourceURI string) (string, error) {
	return GenerateSASToken(resourceURI, p.KeyName, p.Key, tokenExpiry)
}

// EntraTokenProvider obtains OAuth2 tokens via Azure Identity
// (DefaultAzureCredential) and caches the most recent successful result in
// memory until shortly before its ExpiresOn. The cache eliminates per-dial
// blocking on credentials that lack their own in-memory cache — notably
// AzureCLICredential, which shells out to `az account get-access-token` on
// every call under a per-instance mutex.
//
// Concurrent callers single-flight via a refresh-in-flight channel: at most
// one goroutine ever calls into the underlying credential at a time, and
// queued callers can still abort via their own context (so a hung `az`
// invocation cannot keep a deadline-bound dial waiting past its deadline).
// Errors from the underlying credential are returned to the caller and
// intentionally not cached, so a subsequent call retries the underlying
// fetch instead of repeating a stale failure.
type EntraTokenProvider struct {
	cred azcore.TokenCredential

	mu         sync.Mutex
	cached     azcore.AccessToken
	refreshing chan struct{} // non-nil while a refresh is in flight; closed when it ends
}

// NewEntraTokenProvider creates a token provider using DefaultAzureCredential.
func NewEntraTokenProvider() (*EntraTokenProvider, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	return &EntraTokenProvider{cred: cred}, nil
}

// NewEntraTokenProviderWithCredential creates a token provider with a specific
// TokenCredential. This is primarily useful for testing.
func NewEntraTokenProviderWithCredential(cred azcore.TokenCredential) *EntraTokenProvider {
	return &EntraTokenProvider{cred: cred}
}

// GetToken obtains an OAuth2 token for Azure Relay, returning a cached token
// when one is available and not yet stale. Staleness mirrors azcore's
// canonical BearerTokenPolicy.shouldRefresh: if the credential set a
// non-zero RefreshOn, the token is stale once RefreshOn has passed (no
// offset; the authority has supplied the exact refresh window); otherwise
// the token is stale once it is within entraRefreshSkew of its ExpiresOn.
// Concurrent callers that arrive during an in-flight refresh wait on a
// channel rather than the cache mutex, so they can abort via their own
// context if the refresh stalls. The resourceURI parameter is ignored; the
// token is scoped to https://relay.azure.net/.default as required by Azure
// Relay.
func (p *EntraTokenProvider) GetToken(ctx context.Context, _ string) (string, error) {
	for {
		p.mu.Lock()
		if tokenFresh(p.cached) {
			tk := p.cached.Token
			p.mu.Unlock()
			return tk, nil
		}
		if p.refreshing != nil {
			// Another goroutine is fetching a fresh token. Wait for it to
			// finish (or for our context to expire). When it finishes we
			// loop and re-check: on success the cache will hit; on failure
			// we'll try our own refresh with our own context.
			ch := p.refreshing
			p.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		// Become the refresher. Publishing p.refreshing under the mutex
		// guarantees later arrivals either observe this channel and wait,
		// or arrive after the refresh ends and see the updated cache.
		ch := make(chan struct{})
		p.refreshing = ch
		p.mu.Unlock()

		token, err := p.refreshOnce(ctx, ch)
		if err != nil {
			return "", fmt.Errorf("acquire Entra token: %w", err)
		}
		return token, nil
	}
}

// tokenFresh reports whether a cached AccessToken can be served without
// refreshing it. The check mirrors azcore.runtime.shouldRefresh so cached
// tokens are not considered fresh past the credential's own refresh
// guidance: when RefreshOn is non-zero the authority has supplied an
// explicit refresh window (RefreshOn..ExpiresOn) and we refresh as soon as
// we pass RefreshOn; otherwise we fall back to ExpiresOn minus skew.
func tokenFresh(tk azcore.AccessToken) bool {
	if tk.Token == "" {
		return false
	}
	if !tk.RefreshOn.IsZero() {
		return time.Now().Before(tk.RefreshOn)
	}
	return time.Until(tk.ExpiresOn) > entraRefreshSkew
}

// refreshOnce performs a single underlying credential fetch on behalf of the
// caller that won the right to refresh. It always closes ch and clears
// p.refreshing — even if the underlying credential panics — so that waiters
// and future callers can make progress instead of wedging on the in-flight
// signal forever. Defer-driven cleanup is the cheapest correct insurance
// against an unlikely-but-possible panic in third-party credential code.
func (p *EntraTokenProvider) refreshOnce(ctx context.Context, ch chan struct{}) (string, error) {
	var (
		tk        azcore.AccessToken
		fetchErr  error
		completed bool
	)
	defer func() {
		p.mu.Lock()
		if completed && fetchErr == nil {
			p.cached = tk
		}
		p.refreshing = nil
		close(ch)
		p.mu.Unlock()
	}()

	tk, fetchErr = p.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{entraTokenScope},
	})
	completed = true
	if fetchErr != nil {
		return "", fetchErr
	}
	return tk.Token, nil
}

// GenerateSASToken creates a SharedAccessSignature token for Azure Relay.
// The key is the raw key value from the Azure portal.
func GenerateSASToken(resourceURI, keyName, key string, expiry time.Duration) (string, error) {
	uri := url.QueryEscape(strings.ToLower(resourceURI))
	exp := time.Now().Add(expiry).Unix()
	sig := sign(uri, exp, key)
	return fmt.Sprintf("SharedAccessSignature sr=%s&sig=%s&se=%d&skn=%s",
		uri, url.QueryEscape(sig), exp, url.QueryEscape(keyName)), nil
}

func sign(uri string, expiry int64, key string) string {
	str := fmt.Sprintf("%s\n%d", uri, expiry)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(str))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// EndpointToWSS converts an endpoint (FQDN or host:port) to a wss:// URL.
func EndpointToWSS(endpoint string) string {
	return "wss://" + endpoint
}

// EndpointToHTTPS converts an endpoint (FQDN or host:port) to an https:// URL.
func EndpointToHTTPS(endpoint string) string {
	return "https://" + endpoint
}

// ResourceURI returns the HTTPS resource URI for SAS token generation.
func ResourceURI(fqdn, entityPath string) string {
	base := EndpointToHTTPS(fqdn)
	if entityPath != "" {
		return base + "/" + entityPath
	}
	return base
}

// sanitizedError wraps an error with a redacted message while preserving
// the original error chain for errors.Is/As.
type sanitizedError struct {
	msg string
	err error
}

func (e *sanitizedError) Error() string { return e.msg }
func (e *sanitizedError) Unwrap() error { return e.err }

// sanitizeErr strips token query parameters from WebSocket dial errors
// to avoid leaking credentials in log output. The returned error preserves
// the original error chain for errors.Is/As.
func sanitizeErr(err error) error {
	s := err.Error()
	// Strip all occurrences of sb-hc-token=... from URLs in the error message.
	const marker = "sb-hc-token="
	const redacted = "sb-hc-token=REDACTED"
	pos := 0
	for pos < len(s) {
		i := strings.Index(s[pos:], marker)
		if i == -1 {
			break
		}
		i += pos // absolute position
		valStart := i + len(marker)
		end := strings.IndexAny(s[valStart:], "\" &")
		if end == -1 {
			s = s[:valStart] + "REDACTED"
		} else {
			s = s[:valStart] + "REDACTED" + s[valStart+end:]
		}
		pos = i + len(redacted)
	}
	return &sanitizedError{msg: s, err: err}
}
