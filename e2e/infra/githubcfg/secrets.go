// Package githubcfg programs the GitHub environment + repository
// variables and secrets used by the aztunnel E2E pipeline. Replaces the
// historic create-github-ci-secrets.sh.
//
// It uses go-github for the API surface and nacl/box for secret
// encryption against the per-environment public key, which is required
// by the encrypted-secrets endpoints.
package githubcfg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/google/go-github/v86/github"
	"golang.org/x/crypto/nacl/box"
)

// Config configures Configure.
type Config struct {
	// Repo is "owner/name".
	Repo string
	// Environment is the GitHub deployment environment name.
	Environment string

	// RelayName and ResourceGroup are written as plaintext variables.
	RelayName     string
	ResourceGroup string

	// ClientID, TenantID, SubscriptionID are written as encrypted secrets.
	ClientID       string
	TenantID       string
	SubscriptionID string

	// Dependabot, when true, also writes the AZURE_* values as Dependabot
	// repository secrets.
	Dependabot bool

	// DryRun, when true, only prints the operations that would be performed.
	DryRun bool
}

// Configure performs the full GitHub configuration. Requires a token in
// the GITHUB_TOKEN env var with repo + admin:org_environment scopes for
// the target repo.
func Configure(ctx context.Context, cfg Config) error {
	if cfg.Repo == "" {
		return errors.New("repo is required")
	}
	if cfg.Environment == "" {
		return errors.New("environment is required")
	}
	owner, repo, err := splitRepo(cfg.Repo)
	if err != nil {
		return err
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("GITHUB_TOKEN is required (with repo and admin:org_environment scopes)")
	}
	client := github.NewClient(nil).WithAuthToken(token)

	var repoID int
	if !cfg.DryRun {
		r, _, err := client.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return fmt.Errorf("get repo %s/%s (check GITHUB_TOKEN scopes — needs repo and admin:org_environment): %w", owner, repo, err)
		}
		repoID = int(r.GetID())
	}

	fmt.Fprintf(os.Stderr, "==> ensuring GitHub environment %s/%s/environments/%s\n", owner, repo, cfg.Environment)
	if !cfg.DryRun {
		if _, _, err := client.Repositories.CreateUpdateEnvironment(ctx, owner, repo, cfg.Environment, &github.CreateUpdateEnvironment{}); err != nil {
			return fmt.Errorf("create environment: %w", err)
		}
	}

	envVars := map[string]string{
		"E2E_RELAY_NAME":     cfg.RelayName,
		"E2E_RESOURCE_GROUP": cfg.ResourceGroup,
	}
	for _, name := range sortedKeys(envVars) {
		value := envVars[name]
		fmt.Fprintf(os.Stderr, "==> setting environment variable %s\n", name)
		if cfg.DryRun {
			continue
		}
		if err := setEnvVariable(ctx, client, owner, repo, cfg.Environment, name, value); err != nil {
			return fmt.Errorf("set env var %s: %w", name, err)
		}
	}

	envSecrets := map[string]string{
		"AZURE_CLIENT_ID":       cfg.ClientID,
		"AZURE_TENANT_ID":       cfg.TenantID,
		"AZURE_SUBSCRIPTION_ID": cfg.SubscriptionID,
	}
	if !cfg.DryRun {
		pubKey, _, err := client.Actions.GetEnvPublicKey(ctx, repoID, cfg.Environment)
		if err != nil {
			return fmt.Errorf("get env public key: %w", err)
		}
		for _, name := range sortedKeys(envSecrets) {
			value := envSecrets[name]
			fmt.Fprintf(os.Stderr, "==> setting environment secret %s\n", name)
			if err := putEnvSecret(ctx, client, repoID, cfg.Environment, pubKey, name, value); err != nil {
				return fmt.Errorf("set env secret %s: %w", name, err)
			}
		}
	} else {
		for _, name := range sortedKeys(envSecrets) {
			fmt.Fprintf(os.Stderr, "==> would set environment secret %s\n", name)
		}
	}

	if cfg.Dependabot {
		fmt.Fprintln(os.Stderr, "==> setting Dependabot secrets (repo scope)")
		if !cfg.DryRun {
			pubKey, _, err := client.Dependabot.GetRepoPublicKey(ctx, owner, repo)
			if err != nil {
				return fmt.Errorf("get dependabot public key: %w", err)
			}
			for _, name := range sortedKeys(envSecrets) {
				value := envSecrets[name]
				if err := putDependabotSecret(ctx, client, owner, repo, pubKey, name, value); err != nil {
					return fmt.Errorf("set dependabot secret %s: %w", name, err)
				}
			}
		} else {
			for _, name := range sortedKeys(envSecrets) {
				fmt.Fprintf(os.Stderr, "==> would set Dependabot secret %s\n", name)
			}
		}
	}

	return nil
}

// sortedKeys returns the keys of m in lexicographic order. Used to make
// idempotent reruns produce a deterministic log line ordering that the
// operator can diff.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func splitRepo(s string) (owner, repo string, err error) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("invalid repo %q: expected owner/name", s)
	}
	return s[:i], s[i+1:], nil
}

// setEnvVariable creates the variable if it doesn't exist, updates
// otherwise. We try Create first because that's the common path for a
// fresh environment; a 422 "already exists" response triggers a fall-back
// PATCH via UpdateEnvVariable.
func setEnvVariable(ctx context.Context, c *github.Client, owner, repo, env, name, value string) error {
	v := &github.ActionsVariable{Name: name, Value: value}
	resp, err := c.Actions.CreateEnvVariable(ctx, owner, repo, env, v)
	if err == nil {
		return nil
	}
	// go-github may return a nil response on transport errors; guard
	// before reading StatusCode.
	if resp == nil {
		return err
	}
	if resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusConflict {
		if _, err := c.Actions.UpdateEnvVariable(ctx, owner, repo, env, v); err != nil {
			return err
		}
		return nil
	}
	return err
}

// putEnvSecret encrypts value with pubKey and writes it as a GitHub
// environment secret. CreateOrUpdateEnvSecret is a PUT, so it handles
// both create and update.
func putEnvSecret(ctx context.Context, c *github.Client, repoID int, env string, pubKey *github.PublicKey, name, value string) error {
	enc, err := encryptForKey(pubKey.GetKey(), value)
	if err != nil {
		return err
	}
	secret := &github.EncryptedSecret{
		Name:           name,
		KeyID:          pubKey.GetKeyID(),
		EncryptedValue: enc,
	}
	if _, err := c.Actions.CreateOrUpdateEnvSecret(ctx, repoID, env, secret); err != nil {
		return err
	}
	return nil
}

// putDependabotSecret encrypts value with pubKey and writes it as a repo-
// scoped Dependabot secret.
func putDependabotSecret(ctx context.Context, c *github.Client, owner, repo string, pubKey *github.PublicKey, name, value string) error {
	enc, err := encryptForKey(pubKey.GetKey(), value)
	if err != nil {
		return err
	}
	secret := &github.DependabotEncryptedSecret{
		Name:           name,
		KeyID:          pubKey.GetKeyID(),
		EncryptedValue: enc,
	}
	if _, err := c.Dependabot.CreateOrUpdateRepoSecret(ctx, owner, repo, secret); err != nil {
		return err
	}
	return nil
}

// encryptForKey encrypts plaintext with the recipient's base64-encoded
// 32-byte Curve25519 public key using libsodium-compatible sealed boxes,
// per the GitHub API specification for encrypted secrets.
func encryptForKey(pubKeyB64, plaintext string) (string, error) {
	recipient, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		return "", fmt.Errorf("decode public key: %w", err)
	}
	if len(recipient) != 32 {
		return "", fmt.Errorf("public key length %d, want 32", len(recipient))
	}
	var recipientKey [32]byte
	copy(recipientKey[:], recipient)

	sealed, err := box.SealAnonymous(nil, []byte(plaintext), &recipientKey, rand.Reader)
	if err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}
