// Package entra provides idempotent creation of the GitHub Actions OIDC
// app registration + service principal + federated identity credential
// used by the aztunnel E2E pipeline.
package entra

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/philsphicas/aztunnel/e2e/infra/internal/msgraph"
)

// Config drives EnsureApp.
type Config struct {
	// DisplayName is the Entra app registration name (idempotent key).
	DisplayName string
	// Repo is the GitHub "owner/name" the federated credential targets.
	Repo string
	// Environment is the GitHub deployment environment name (forms part
	// of the federated subject string).
	Environment string
}

// App is the result of EnsureApp.
type App struct {
	ApplicationID            string // appId (client ID)
	ApplicationObjectID      string // app reg object ID
	ServicePrincipalObjectID string
	TenantID                 string
}

// EnsureApp finds or creates the app registration, finds or creates its
// service principal, and ensures a federated credential exists for
// `repo:OWNER/REPO:environment:ENV` against the GitHub Actions OIDC
// issuer. Returns the resolved identifiers.
func EnsureApp(ctx context.Context, cfg Config) (*App, error) {
	if cfg.DisplayName == "" {
		return nil, errors.New("DisplayName is required")
	}
	if cfg.Repo == "" {
		return nil, errors.New("repo is required")
	}
	if cfg.Environment == "" {
		return nil, errors.New("environment is required")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("default azure credential: %w", err)
	}
	g, err := msgraph.New(cred)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "==> ensuring Entra app %q\n", cfg.DisplayName)
	app, err := g.EnsureApp(ctx, cfg.DisplayName)
	if err != nil {
		return nil, fmt.Errorf("ensure app: %w", err)
	}

	fmt.Fprintf(os.Stderr, "==> ensuring service principal for appId %s\n", app.AppID)
	sp, err := g.EnsureServicePrincipal(ctx, app.AppID)
	if err != nil {
		return nil, fmt.Errorf("ensure SP: %w", err)
	}

	subject := fmt.Sprintf("repo:%s:environment:%s", cfg.Repo, cfg.Environment)
	fc := msgraph.FederatedCredential{
		Name:        sanitizeFCName(cfg.Environment),
		Issuer:      "https://token.actions.githubusercontent.com",
		Subject:     subject,
		Audiences:   []string{"api://AzureADTokenExchange"},
		Description: fmt.Sprintf("GitHub Actions OIDC for %s environment %s", cfg.Repo, cfg.Environment),
	}
	fmt.Fprintf(os.Stderr, "==> ensuring federated credential %s\n", fc.Subject)
	if err := g.EnsureFederatedCredential(ctx, app.ID, fc); err != nil {
		return nil, fmt.Errorf("ensure federated credential: %w", err)
	}

	tenantID, err := g.TenantID(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant id: %w", err)
	}

	return &App{
		ApplicationID:            app.AppID,
		ApplicationObjectID:      app.ID,
		ServicePrincipalObjectID: sp.ID,
		TenantID:                 tenantID,
	}, nil
}

// sanitizeFCName converts an environment name into something acceptable
// as a federated credential name. Graph allows alphanumerics, dashes,
// and underscores; we only need to drop pathological characters.
func sanitizeFCName(env string) string {
	out := make([]byte, 0, len(env)+len("gh-"))
	out = append(out, []byte("gh-")...)
	for i := 0; i < len(env); i++ {
		c := env[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
