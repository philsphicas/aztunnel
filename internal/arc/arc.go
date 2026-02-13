// Package arc provides client-side support for connecting through Azure Arc
// managed Azure Relay hybrid connections.
//
// When an Azure Arc-connected machine has the OpenSSH extension installed,
// Azure automatically provisions an Azure Relay namespace and hybrid
// connection. The Arc agent on the VM acts as the relay listener, forwarding
// traffic to the local SSH daemon (or other configured service).
//
// This package handles the HybridConnectivity ARM API interactions:
// ensuring the endpoint and service configuration exist, obtaining relay
// credentials, and dialing the relay.
package arc

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/coder/websocket"
	"github.com/philsphicas/aztunnel/internal/relay"
)

const (
	hybridConnectivityAPIVersion = "2023-03-15"
	defaultExpiresin             = 10800 // 3 hours (maximum)
	defaultServiceName           = "SSH"
	defaultPort                  = 22
)

// RelayInfo holds the relay credentials returned by the listCredentials API.
type RelayInfo struct {
	NamespaceName             string `json:"namespaceName"`
	NamespaceNameSuffix       string `json:"namespaceNameSuffix"`
	HybridConnectionName      string `json:"hybridConnectionName"`
	AccessKey                 string `json:"accessKey"`
	ExpiresOn                 int64  `json:"expiresOn"`
	ServiceConfigurationToken string `json:"serviceConfigurationToken"`
}

// Endpoint returns the Azure Relay sb:// endpoint.
func (r *RelayInfo) Endpoint() string {
	return "sb://" + r.NamespaceName + "." + r.NamespaceNameSuffix
}

// listCredentialsResponse is the top-level response from listCredentials.
type listCredentialsResponse struct {
	Relay RelayInfo `json:"relay"`
}

// Client interacts with the HybridConnectivity ARM APIs.
type Client struct {
	arm    *arm.Client
	logger *slog.Logger
}

// NewClient creates a Client using DefaultAzureCredential.
// Options may be nil for Azure Public Cloud defaults.
func NewClient(logger *slog.Logger, options *arm.ClientOptions) (*Client, error) {
	var credOpts *azidentity.DefaultAzureCredentialOptions
	if options != nil {
		credOpts = &azidentity.DefaultAzureCredentialOptions{
			ClientOptions: options.ClientOptions,
		}
	}
	cred, err := azidentity.NewDefaultAzureCredential(credOpts)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	return NewClientWithCredential(cred, logger, options)
}

// NewClientWithCredential creates a Client with a specific TokenCredential.
// Options may be nil for Azure Public Cloud defaults.
func NewClientWithCredential(cred azcore.TokenCredential, logger *slog.Logger, options *arm.ClientOptions) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	armClient, err := arm.NewClient("aztunnel-arc", "v1.0.0", cred, options)
	if err != nil {
		return nil, fmt.Errorf("create ARM client: %w", err)
	}
	return &Client{arm: armClient, logger: logger}, nil
}

// EnsureHybridConnectivity creates the HybridConnectivity endpoint and
// service configuration if they don't already exist. Both calls are
// idempotent PUTs.
//
// CAUTION: Calling this when the endpoint already exists may disrupt the
// Arc agent's relay listener, causing subsequent connections to fail with
// 404 "Endpoint does not exist" until the listener recovers. Prefer
// calling GetRelayCredentials first and only calling this if it fails.
func (c *Client) EnsureHybridConnectivity(ctx context.Context, resourceID, serviceName string, port int) error {
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	if port == 0 {
		port = defaultPort
	}

	endpointPath := fmt.Sprintf("%s/providers/Microsoft.HybridConnectivity/endpoints/default", resourceID)
	endpointURL := runtime.JoinPaths(c.arm.Endpoint(), endpointPath) + "?api-version=" + hybridConnectivityAPIVersion

	serviceConfigPath := fmt.Sprintf("%s/providers/Microsoft.HybridConnectivity/endpoints/default/serviceConfigurations/%s",
		resourceID, serviceName)
	serviceConfigURL := runtime.JoinPaths(c.arm.Endpoint(), serviceConfigPath) + "?api-version=" + hybridConnectivityAPIVersion

	// Create endpoint.
	c.logger.Debug("ensuring HybridConnectivity endpoint", "resourceID", resourceID)
	endpointBody := `{"properties": {"type": "default"}}`
	if err := c.armPUT(ctx, endpointURL, endpointBody); err != nil {
		return fmt.Errorf("create HybridConnectivity endpoint: %w", err)
	}

	// Create service configuration.
	c.logger.Debug("ensuring service configuration", "service", serviceName, "port", port)
	serviceBody := fmt.Sprintf(`{"properties": {"serviceName": %q, "port": %d}}`, serviceName, port)
	if err := c.armPUT(ctx, serviceConfigURL, serviceBody); err != nil {
		return fmt.Errorf("create service configuration: %w", err)
	}

	return nil
}

// GetRelayCredentials obtains relay credentials by calling the
// listCredentials API.
func (c *Client) GetRelayCredentials(ctx context.Context, resourceID, serviceName string) (*RelayInfo, error) {
	if serviceName == "" {
		serviceName = defaultServiceName
	}

	credPath := fmt.Sprintf("%s/providers/Microsoft.HybridConnectivity/endpoints/default/listCredentials", resourceID)
	credURL := runtime.JoinPaths(c.arm.Endpoint(), credPath) + fmt.Sprintf("?expiresin=%d&api-version=%s",
		defaultExpiresin, hybridConnectivityAPIVersion)

	body := fmt.Sprintf(`{"serviceName": %q}`, serviceName)

	c.logger.Debug("requesting relay credentials", "resourceID", resourceID, "service", serviceName)
	resp, err := c.armPOST(ctx, credURL, body)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}

	var result listCredentialsResponse
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("parse credentials response: %w", err)
	}

	if result.Relay.NamespaceName == "" || result.Relay.HybridConnectionName == "" {
		return nil, fmt.Errorf("incomplete relay credentials in response")
	}

	c.logger.Debug("obtained relay credentials",
		"namespace", result.Relay.NamespaceName,
		"hybridConnection", result.Relay.HybridConnectionName,
		"expiresOn", result.Relay.ExpiresOn)

	return &result.Relay, nil
}

// Dial connects to the Azure Relay using credentials from RelayInfo.
// Unlike relay.Dial, this does NOT perform the aztunnel envelope exchange —
// the Arc agent on the VM handles the local TCP connection directly.
//
// Authentication uses three HTTP headers on the WebSocket upgrade:
//   - Servicebusauthorization: the SAS access key
//   - Service-Configuration-Token: the JWT from listCredentials
//   - Microsoft-Guestgateway-Target: localhost:{port}
func Dial(ctx context.Context, info *RelayInfo, port int) (*websocket.Conn, error) {
	if port == 0 {
		port = defaultPort
	}
	wssHost := info.NamespaceName + "." + info.NamespaceNameSuffix

	connectURL := fmt.Sprintf("wss://%s/$hc/%s?sb-hc-action=connect&sb-hc-id=%s",
		wssHost,
		info.HybridConnectionName,
		newUUID())

	headers := http.Header{}
	headers.Set("Servicebusauthorization", info.AccessKey)
	headers.Set("Service-Configuration-Token", info.ServiceConfigurationToken)
	headers.Set("Microsoft-Guestgateway-Target", fmt.Sprintf("localhost:%d", port))

	ws, _, err := websocket.Dial(ctx, connectURL, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("dial arc relay: %w", sanitizeErr(err))
	}
	return ws, nil
}

// DialWithLogger is like Dial but logs the connection attempt.
func DialWithLogger(ctx context.Context, info *RelayInfo, port int, logger *slog.Logger) (*websocket.Conn, error) {
	logger.Debug("dialing arc relay",
		"namespace", info.NamespaceName,
		"hybridConnection", info.HybridConnectionName,
		"port", port)
	ws, err := Dial(ctx, info, port)
	if err != nil {
		logger.Warn("arc relay dial failed", "error", err)
		return nil, err
	}
	logger.Debug("arc relay connected")
	return ws, nil
}

func (c *Client) armPUT(ctx context.Context, rawURL, body string) error {
	req, err := runtime.NewRequest(ctx, http.MethodPut, rawURL)
	if err != nil {
		return err
	}
	req.Raw().Header.Set("Content-Type", "application/json")
	if err := req.SetBody(streaming.NopCloser(strings.NewReader(body)), "application/json"); err != nil {
		return err
	}
	resp, err := c.arm.Pipeline().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return newARMError(resp)
	}
	return nil
}

func (c *Client) armPOST(ctx context.Context, rawURL, body string) ([]byte, error) {
	req, err := runtime.NewRequest(ctx, http.MethodPost, rawURL)
	if err != nil {
		return nil, err
	}
	req.Raw().Header.Set("Content-Type", "application/json")
	if err := req.SetBody(streaming.NopCloser(strings.NewReader(body)), "application/json"); err != nil {
		return nil, err
	}
	resp, err := c.arm.Pipeline().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, newARMError(resp)
	}
	return io.ReadAll(resp.Body)
}

func newARMError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("ARM API error (HTTP %d): %s", resp.StatusCode, string(body))
}

// sanitizeErr strips sensitive tokens from WebSocket dial errors.
func sanitizeErr(err error) error {
	s := err.Error()
	// Query-param tokens are delimited by & or whitespace.
	// Header tokens may contain spaces (e.g. "SharedAccessSignature sr=…"),
	// so we only split on newline or quote.
	type redaction struct {
		prefix     string
		delimiters string
	}
	patterns := []redaction{
		{"sb-hc-token=", "&\" \n"},
		{"Servicebusauthorization:", "\"\n"},
		{"Service-Configuration-Token:", "\"\n"},
	}
	for _, p := range patterns {
		idx := strings.Index(strings.ToLower(s), strings.ToLower(p.prefix))
		if idx == -1 {
			continue
		}
		afterPrefix := idx + len(p.prefix)
		// Skip optional whitespace between header name and value.
		for afterPrefix < len(s) && s[afterPrefix] == ' ' {
			afterPrefix++
		}
		end := strings.IndexAny(s[afterPrefix:], p.delimiters)
		if end == -1 {
			s = s[:idx] + p.prefix + "REDACTED"
		} else {
			s = s[:idx] + p.prefix + "REDACTED" + s[afterPrefix+end:]
		}
	}
	return fmt.Errorf("%s", s)
}

// newUUID generates a random UUID v4 string without external dependencies.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Bridge re-exports relay.Bridge for convenience.
var Bridge = relay.Bridge
