package aadssh

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"
	"golang.org/x/crypto/ssh"
)

const (
	// azureCLIClientID is the well-known public client ID of the Azure CLI. It
	// is used by default so that first-time interactive/device-code sign-in
	// behaves like `az login` and can reuse a shared token cache.
	azureCLIClientID = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"

	// serverAppID is the AADSSHLoginForLinux server application ("PAS") whose
	// /.default scope yields the SSH certificate.
	serverAppID = "ce6ff14a-7fdc-4685-bbe0-f6afdfcfa8e0"

	// DefaultScope is the token scope that returns an SSH certificate.
	DefaultScope = serverAppID + "/.default"

	// DefaultTenant lets any work/school account through; it can be overridden
	// for guest scenarios where the resource lives in a specific tenant.
	DefaultTenant = "organizations"

	// DefaultMinValidity is how much validity a cached certificate must retain
	// to be reused without re-minting.
	DefaultMinValidity = 5 * time.Minute
)

// Options configures a certificate acquisition.
type Options struct {
	// Identity is the path to the private key file. It is created if missing.
	Identity string
	// CertPath is the path to write the certificate. Defaults to
	// "<Identity>-cert.pub".
	CertPath string
	// Username is the SSH login name the connection will use (ssh's %r). When
	// set, it selects the matching cached account for silent renewal and is used
	// as the interactive login hint, so a multi-account token cache cannot mint
	// a certificate for the wrong Entra principal.
	Username string
	// TokenCachePath is where MSAL refresh tokens are persisted for silent
	// renewal. Required for non-interactive reuse across invocations.
	TokenCachePath string

	// ClientID overrides the OAuth public client ID (default: Azure CLI).
	ClientID string
	// Tenant overrides the authority tenant (default: "organizations").
	Tenant string
	// Scope overrides the token scope (default: the AADSSHLoginForLinux app).
	Scope string

	// MinValidity is the remaining validity a cached cert must have to be
	// reused. A nil value applies DefaultMinValidity; a non-nil value is used
	// as-is, so a pointer to zero reuses any certificate that is currently
	// valid (no safety buffer).
	MinValidity *time.Duration

	// Stderr receives human-facing prompts (e.g. the device-code message).
	Stderr io.Writer
}

func (o *Options) withDefaults() {
	if o.CertPath == "" {
		o.CertPath = o.Identity + "-cert.pub"
	}
	if o.ClientID == "" {
		o.ClientID = azureCLIClientID
	}
	if o.Tenant == "" {
		o.Tenant = DefaultTenant
	}
	if o.Scope == "" {
		o.Scope = DefaultScope
	}
	if o.MinValidity == nil {
		d := DefaultMinValidity
		o.MinValidity = &d
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
}

// EnsureCert makes sure a currently-valid SSH certificate exists for the given
// identity, minting a new one from Entra ID if necessary. It returns the SSH
// login username derived from the certificate's first principal.
func EnsureCert(ctx context.Context, opts Options) (string, error) {
	opts.withDefaults()
	if opts.Identity == "" {
		return "", fmt.Errorf("identity key path is required")
	}
	if *opts.MinValidity < 0 {
		return "", fmt.Errorf("minimum certificate validity must not be negative")
	}

	// Fast path: reuse an existing, still-valid certificate whose key is intact.
	if u, ok := reuseExisting(&opts); ok {
		return u, nil
	}

	// Serialize concurrent mints for this identity so two simultaneous SSH
	// connections cannot interleave key generation and certificate writes and
	// leave a certificate bound to a different key.
	release, err := acquireDirLock(ctx, filepath.Dir(opts.Identity))
	if err != nil {
		return "", fmt.Errorf("lock identity directory: %w", err)
	}
	defer release()

	// Another process may have completed a full mint while we waited.
	if u, ok := reuseExisting(&opts); ok {
		return u, nil
	}

	km, err := loadOrGenerateKey(opts.Identity)
	if err != nil {
		return "", err
	}

	body, err := acquireCert(ctx, &opts, km)
	if err != nil {
		return "", err
	}

	user, err := validateCertificateBody(body, km.publicKey, opts.Username, time.Now())
	if err != nil {
		return "", err
	}
	if err := writeCert(opts.CertPath, body); err != nil {
		return "", err
	}
	return user, nil
}

// reuseExisting reports the login name when a still-valid certificate and a
// usable private key are already on disk, so no mint is needed. It returns false
// (triggering a re-mint) if the certificate is missing/expired, if the private
// key was removed or is unreadable, or if the certificate's principal does not
// match the requested username.
func reuseExisting(opts *Options) (string, bool) {
	cert, err := readCertificate(opts.CertPath)
	if err != nil || !certStillValid(cert, time.Now(), *opts.MinValidity) {
		return "", false
	}
	// A valid certificate is useless without its matching private key.
	priv, err := readPrivateKey(opts.Identity)
	if err != nil {
		return "", false
	}
	expectedKey, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		return "", false
	}
	if !certificateMatchesPublicKey(cert, expectedKey) {
		return "", false
	}
	user, err := certificateUsername(cert)
	if err != nil {
		return "", false
	}
	if opts.Username != "" && !strings.EqualFold(user, opts.Username) {
		return "", false
	}
	return user, true
}

// acquireCert requests an SSH certificate from Entra ID, trying a silent
// (cached) acquisition first and falling back to the interactive flow.
func acquireCert(ctx context.Context, opts *Options, km *keyMaterial) (string, error) {
	scheme := &sshCertScheme{reqCnf: km.reqCnf, keyID: km.keyID}

	if opts.TokenCachePath != "" {
		release, err := acquireFileLock(ctx, opts.TokenCachePath+".lock")
		if err != nil {
			return "", fmt.Errorf("lock token cache: %w", err)
		}
		defer release()
	}

	clientOpts := []public.Option{
		public.WithAuthority("https://login.microsoftonline.com/" + opts.Tenant),
	}
	if opts.TokenCachePath != "" {
		clientOpts = append(clientOpts, public.WithCache(newFileTokenCache(opts.TokenCachePath)))
	}
	app, err := public.New(opts.ClientID, clientOpts...)
	if err != nil {
		return "", fmt.Errorf("create msal client: %w", err)
	}

	scopes := []string{opts.Scope}

	// Try silent acquisition against the cached account that matches the
	// requested login name. Selecting by username (rather than the first cached
	// account) prevents minting a certificate for the wrong Entra principal when
	// the token cache holds several accounts.
	if account, ok := selectAccount(ctx, app, opts.Username); ok {
		res, err := app.AcquireTokenSilent(ctx, scopes,
			public.WithSilentAccount(account),
			public.WithAuthenticationScheme(scheme),
		)
		if err == nil {
			return res.AccessToken, nil
		}
	}

	// Fall back to the interactive (browser) flow. The ssh-cert authentication
	// scheme is only supported for silent and interactive acquisition in MSAL
	// Go, not the device-code flow.
	_, _ = fmt.Fprintln(opts.Stderr, "aztunnel: opening a browser to sign in to Microsoft Entra ID for an SSH certificate...")
	interactiveOpts := []public.AcquireInteractiveOption{public.WithAuthenticationScheme(scheme)}
	if opts.Username != "" {
		interactiveOpts = append(interactiveOpts, public.WithLoginHint(opts.Username))
	}
	res, err := app.AcquireTokenInteractive(ctx, scopes, interactiveOpts...)
	if err != nil {
		return "", fmt.Errorf("interactive authentication: %w", err)
	}
	return res.AccessToken, nil
}

// selectAccount returns the cached account whose username matches want
// (case-insensitively). When want is empty it returns the sole cached account
// if there is exactly one, and otherwise reports no match so the caller signs in
// interactively rather than guessing among several identities.
func selectAccount(ctx context.Context, app public.Client, want string) (public.Account, bool) {
	accounts, err := app.Accounts(ctx)
	if err != nil || len(accounts) == 0 {
		return public.Account{}, false
	}
	if want == "" {
		if len(accounts) == 1 {
			return accounts[0], true
		}
		return public.Account{}, false
	}
	for _, a := range accounts {
		if strings.EqualFold(a.PreferredUsername, want) {
			return a, true
		}
	}
	return public.Account{}, false
}
