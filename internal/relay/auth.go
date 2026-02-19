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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// TokenProvider generates authentication tokens for Azure Relay.
type TokenProvider interface {
	// GetToken returns a token string suitable for the sb-hc-token query
	// parameter or the renewToken control message.
	GetToken(ctx context.Context, resourceURI string) (string, error)
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

// EntraTokenProvider obtains OAuth2 tokens via Azure Identity (DefaultAzureCredential).
type EntraTokenProvider struct {
	cred azcore.TokenCredential
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

// GetToken obtains an OAuth2 token for Azure Relay.
// The resourceURI parameter is ignored; the token is scoped to
// https://relay.azure.net/.default as required by Azure Relay.
func (p *EntraTokenProvider) GetToken(ctx context.Context, _ string) (string, error) {
	tk, err := p.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://relay.azure.net/.default"},
	})
	if err != nil {
		return "", fmt.Errorf("acquire Entra token: %w", err)
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
		uri, url.QueryEscape(sig), exp, keyName), nil
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

// sanitizeErr strips token query parameters from WebSocket dial errors
// to avoid leaking credentials in log output.
func sanitizeErr(err error) error {
	s := err.Error()
	// Strip sb-hc-token=... from URLs in the error message.
	if i := strings.Index(s, "sb-hc-token="); i != -1 {
		end := strings.IndexAny(s[i:], "\" ")
		if end == -1 {
			s = s[:i] + "sb-hc-token=REDACTED"
		} else {
			s = s[:i] + "sb-hc-token=REDACTED" + s[i+end:]
		}
	}
	return fmt.Errorf("%s", s)
}
