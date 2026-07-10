package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/philsphicas/aztunnel/internal/aadssh"
)

// ArcAadCertCmd mints (or refreshes) a Microsoft Entra ID SSH certificate for
// AADSSHLoginForLinux. It is intended to be invoked from an SSH config
// `Match final exec` directive so that `ssh` to an Arc-connected machine
// acquires a certificate automatically. On success the derived login username
// is printed to stderr and the process exits 0 (so the enclosing Match block
// applies).
type ArcAadCertCmd struct {
	Identity   string        `help:"Path to the private key file (created if missing). Alternative to --dir."`
	Cert       string        `help:"Path to write the certificate. Defaults to <identity>-cert.pub."`
	Dir        string        `help:"Directory holding this connection's key and certificate (id, id-cert.pub). Point it at ~/.ssh/arc/%C in a 'Match final exec' directive so ssh supplies its own connection hash."`
	User       string        `help:"SSH login name (ssh %r). Selects the matching cached Entra account and is used as the interactive login hint."`
	TokenCache string        `name:"token-cache" help:"Path to the MSAL token cache for silent renewal."`
	ClientID   string        `name:"client-id" help:"OAuth public client ID." default:"04b07795-8ddb-461a-bbee-02f9e1bf7b46"`
	Tenant     string        `help:"Entra ID tenant (or 'organizations'/'common')." default:"organizations"`
	Scope      string        `help:"Token scope that yields the SSH certificate." default:"ce6ff14a-7fdc-4685-bbe0-f6afdfcfa8e0/.default"`
	MinValid   time.Duration `name:"min-valid" help:"Reuse an existing certificate if it stays valid at least this long." default:"5m"`
	SSHKeygen  string        `name:"ssh-keygen" help:"Path to the ssh-keygen executable." default:"ssh-keygen"`
}

// Run executes the arc aad-cert command.
func (c *ArcAadCertCmd) Run(globals *Globals, arcCmd *ArcCmd) error {
	logger := newLogger(globals.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	identity, certPath, err := c.resolvePaths(arcCmd, logger)
	if err != nil {
		return err
	}
	logger.Debug("aad-cert requested", "resource", arcCmd.ResourceID, "identity", identity)

	tokenCache := c.TokenCache
	if tokenCache == "" {
		tokenCache = defaultTokenCachePath()
	}
	tokenCache = expandTilde(tokenCache)

	username, err := aadssh.EnsureCert(ctx, aadssh.Options{
		Identity:       identity,
		CertPath:       certPath,
		Username:       c.User,
		TokenCachePath: tokenCache,
		ClientID:       c.ClientID,
		Tenant:         c.Tenant,
		Scope:          c.Scope,
		SSHKeygen:      c.SSHKeygen,
		MinValidity:    c.MinValid,
		Stderr:         os.Stderr,
	})
	if err != nil {
		return err
	}

	// Print the login name so the user knows which username to connect as
	// (ssh <username>@/subscriptions/...). Match final exec ignores stdout, so
	// use stderr, which is visible during connection setup.
	_, _ = fmt.Fprintf(os.Stderr, "aztunnel: AAD SSH username: %s\n", username)
	return nil
}

// resolvePaths determines the private-key and certificate paths. When --dir is
// set, aad-cert treats it as the literal directory for this connection and
// writes id/id-cert.pub inside it; an SSH config points --dir at ~/.ssh/arc/%C
// (via `Match final exec`) so ssh computes its own connection hash and reads
// exactly what aad-cert writes. Otherwise it uses the explicit --identity (and
// --cert) paths, expanding a leading ~ for parity with ssh.
func (c *ArcAadCertCmd) resolvePaths(arcCmd *ArcCmd, logger *slog.Logger) (identity, certPath string, err error) {
	if c.Dir != "" {
		dir := expandTilde(c.Dir)

		// Record which machine this directory maps to so an opaque %C path stays
		// human-navigable. Best-effort: never fail the mint over it.
		if arcCmd.ResourceID != "" {
			if err := writeResourceMarker(dir, arcCmd.ResourceID); err != nil {
				logger.Debug("could not write resource-id marker", "dir", dir, "err", err)
			}
		}

		return filepath.Join(dir, "id"), filepath.Join(dir, "id-cert.pub"), nil
	}

	if c.Identity == "" {
		return "", "", fmt.Errorf("either --identity or --dir is required")
	}
	return expandTilde(c.Identity), expandTilde(c.Cert), nil
}

// defaultTokenCachePath returns a per-user location for the MSAL token cache.
func defaultTokenCachePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			dir = filepath.Join(home, ".aztunnel")
		} else {
			return ""
		}
	} else {
		dir = filepath.Join(dir, "aztunnel")
	}
	return filepath.Join(dir, "msal_token_cache.json")
}

// expandTilde expands a leading ~ (optionally followed by a separator) to the
// user's home directory. ssh expands ~ in its own path directives, but a --dir
// value passed through a Match exec command is handed to aztunnel verbatim on
// Windows (cmd.exe performs no tilde expansion), so aad-cert expands it for
// cross-platform parity.
func expandTilde(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// writeResourceMarker records the resource ID that a %C hash directory belongs
// to, so the hashed layout under --dir can be mapped back to a machine.
func writeResourceMarker(dir, resourceID string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "resource-id"), []byte(resourceID+"\n"), 0o644)
}
