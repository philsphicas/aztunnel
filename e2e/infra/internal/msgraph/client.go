// Package msgraph is a thin client for the Microsoft Graph REST API,
// scoped to the operations the e2e-infra CLI needs (resolve users + apps +
// SPs by various identifiers; create app registrations + service
// principals + federated credentials). It avoids the heavyweight
// msgraph-sdk-go in favor of raw HTTP against a small, stable subset of
// endpoints.
package msgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const graphBase = "https://graph.microsoft.com/v1.0"

// Client speaks Microsoft Graph. Construct with New.
type Client struct {
	cred   azcore.TokenCredential
	http   *http.Client
	scopes []string
}

// New builds a Graph client backed by the given Azure credential.
func New(cred azcore.TokenCredential) (*Client, error) {
	if cred == nil {
		return nil, errors.New("nil credential")
	}
	return &Client{
		cred:   cred,
		http:   &http.Client{Timeout: 30 * time.Second},
		scopes: []string{"https://graph.microsoft.com/.default"},
	}, nil
}

// User is a minimal Microsoft Graph user.
type User struct {
	ID                string `json:"id"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// App is a minimal Microsoft Graph application registration.
type App struct {
	ID          string `json:"id"`    // object ID
	AppID       string `json:"appId"` // client ID
	DisplayName string `json:"displayName"`
}

// ServicePrincipal is a minimal Microsoft Graph service principal.
type ServicePrincipal struct {
	ID    string `json:"id"`
	AppID string `json:"appId"`
}

// FederatedCredential describes a workload-identity federation credential
// on an app registration.
type FederatedCredential struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Issuer      string   `json:"issuer"`
	Subject     string   `json:"subject"`
	Audiences   []string `json:"audiences"`
	Description string   `json:"description,omitempty"`
}

// SignedInUserObjectID returns the (objectID, UPN) of the currently
// authenticated principal. Errors if the credential does not represent a
// user (e.g. when running as a service principal).
func (c *Client) SignedInUserObjectID(ctx context.Context) (string, string, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/me", nil, &u); err != nil {
		return "", "", err
	}
	return u.ID, u.UserPrincipalName, nil
}

// UserObjectIDByUPN resolves a user by UPN/email to their object ID.
func (c *Client) UserObjectIDByUPN(ctx context.Context, upn string) (string, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(upn), nil, &u); err != nil {
		return "", err
	}
	return u.ID, nil
}

type appListResponse struct {
	Value []App `json:"value"`
}

// AppByDisplayName looks up an app registration by displayName. Returns
// (nil, ErrNotFound) when zero matches; errors when more than one match.
// The display name must contain only printable characters (no control
// characters or NUL bytes); otherwise an error is returned without a Graph
// call. This matches the Graph constraint and avoids a confusing 400.
func (c *Client) AppByDisplayName(ctx context.Context, name string) (*App, error) {
	escaped, err := escapeODataString(name)
	if err != nil {
		return nil, fmt.Errorf("AppByDisplayName: %w", err)
	}
	q := url.Values{}
	q.Set("$filter", fmt.Sprintf("displayName eq '%s'", escaped))
	q.Set("$select", "id,appId,displayName")
	var out appListResponse
	if err := c.do(ctx, http.MethodGet, "/applications?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	switch len(out.Value) {
	case 0:
		return nil, ErrNotFound
	case 1:
		app := out.Value[0]
		return &app, nil
	default:
		return nil, fmt.Errorf("multiple apps named %q — use a unique display name", name)
	}
}

// EnsureApp returns an existing app by displayName or creates one.
func (c *Client) EnsureApp(ctx context.Context, name string) (*App, error) {
	app, err := c.AppByDisplayName(ctx, name)
	if err == nil {
		return app, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	body := map[string]string{"displayName": name}
	var created App
	if err := c.do(ctx, http.MethodPost, "/applications", body, &created); err != nil {
		return nil, fmt.Errorf("create app: %w", err)
	}
	return &created, nil
}

type spListResponse struct {
	Value []ServicePrincipal `json:"value"`
}

// ServicePrincipalByAppID looks up the SP for a given client ID.
func (c *Client) ServicePrincipalByAppID(ctx context.Context, appID string) (*ServicePrincipal, error) {
	escaped, err := escapeODataString(appID)
	if err != nil {
		return nil, fmt.Errorf("ServicePrincipalByAppID: %w", err)
	}
	q := url.Values{}
	q.Set("$filter", fmt.Sprintf("appId eq '%s'", escaped))
	q.Set("$select", "id,appId")
	var out spListResponse
	if err := c.do(ctx, http.MethodGet, "/servicePrincipals?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Value) == 0 {
		return nil, ErrNotFound
	}
	sp := out.Value[0]
	return &sp, nil
}

// EnsureServicePrincipal returns an existing SP for the given client ID
// or creates one.
func (c *Client) EnsureServicePrincipal(ctx context.Context, appID string) (*ServicePrincipal, error) {
	sp, err := c.ServicePrincipalByAppID(ctx, appID)
	if err == nil {
		return sp, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	body := map[string]string{"appId": appID}
	var created ServicePrincipal
	if err := c.do(ctx, http.MethodPost, "/servicePrincipals", body, &created); err != nil {
		return nil, fmt.Errorf("create SP: %w", err)
	}
	return &created, nil
}

type fedCredListResponse struct {
	Value []FederatedCredential `json:"value"`
}

// ListFederatedCredentials returns existing federated credentials for an app.
func (c *Client) ListFederatedCredentials(ctx context.Context, appObjectID string) ([]FederatedCredential, error) {
	path := fmt.Sprintf("/applications/%s/federatedIdentityCredentials", appObjectID)
	var out fedCredListResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Value, nil
}

// EnsureFederatedCredential creates the given federated credential on the
// app if no existing credential matches by (name) or by the
// (subject, issuer, audiences) tuple. We deliberately do NOT treat a
// subject collision with a different (issuer, audiences) as equivalent —
// that would silently mask a misconfigured credential.
func (c *Client) EnsureFederatedCredential(ctx context.Context, appObjectID string, fc FederatedCredential) error {
	existing, err := c.ListFederatedCredentials(ctx, appObjectID)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.Name == fc.Name {
			return nil
		}
		if e.Subject == fc.Subject && e.Issuer == fc.Issuer && audiencesEqual(e.Audiences, fc.Audiences) {
			return nil
		}
	}
	path := fmt.Sprintf("/applications/%s/federatedIdentityCredentials", appObjectID)
	return c.do(ctx, http.MethodPost, path, fc, nil)
}

func audiencesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// TenantID returns the tenant ID for the credential by parsing the tid
// claim from a fresh Graph access token.
func (c *Client) TenantID(ctx context.Context) (string, error) {
	tk, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: c.scopes})
	if err != nil {
		return "", err
	}
	return parseTenantIDFromJWT(tk.Token)
}

// ErrNotFound is returned by lookup functions when the entity does not
// exist. Caller code typically checks errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("not found")

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	tk, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: c.scopes})
	if err != nil {
		return fmt.Errorf("acquire graph token: %w", err)
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, graphBase+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tk.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req) //nolint:gosec // URL is constructed from the fixed graphBase constant + caller-provided path
	if err != nil {
		return fmt.Errorf("graph %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read graph response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("graph %s %s: 404: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("graph %s %s: %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode graph response: %w", err)
	}
	return nil
}

// escapeODataString escapes single quotes in an OData filter value by
// doubling them, per OData v4 grammar. Non-printable characters
// (control codes, including NUL) are rejected — Graph rejects them with
// a 400 that surfaces as an unhelpful error from the caller's POV.
func escapeODataString(s string) (string, error) {
	for i, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("non-printable character 0x%02x at offset %d", r, i)
		}
	}
	return strings.ReplaceAll(s, "'", "''"), nil
}
