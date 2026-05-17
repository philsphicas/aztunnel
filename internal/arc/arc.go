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
	"time"

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
	AccessKey                 string `json:"accessKey"` //nolint:gosec // G117: deserialized from Azure ARM API
	ExpiresOn                 int64  `json:"expiresOn"`
	ServiceConfigurationToken string `json:"serviceConfigurationToken"` //nolint:gosec // G117: deserialized from Azure ARM API
}

// Endpoint returns the Azure Relay FQDN.
func (r *RelayInfo) Endpoint() string {
	return r.NamespaceName + "." + r.NamespaceNameSuffix
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
// Dial makes a single attempt. Use DialWithLogger for retry support.
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

// Retry parameters for DialWithOptions.
const (
	retryInitial    = 1 * time.Second
	retryMax        = 5 * time.Second
	retryMultiplier = 2
	dialTimeout     = 30 * time.Second
)

// Progress-log timings are vars (not consts) so tests can shorten them
// without using real time.
var (
	// progressLogPeriod controls how often progress INFO logs are emitted
	// while we wait for the Arc agent to register a relay listener.
	progressLogPeriod = 15 * time.Second
	// progressLogQuietDelay is the grace period before emitting the first
	// progress INFO when DialOptions.ExplainSetup is false. This keeps the
	// "agent listener is healthy" steady state quiet while still surfacing
	// real "listener never appears" cases.
	progressLogQuietDelay = 30 * time.Second
)

// DialOptions controls DialWithOptions behavior.
type DialOptions struct {
	// ExplainSetup enables INFO-level progress logging when the dial
	// initially returns retryable 404/503 errors. Set this immediately
	// after creating or updating the HybridConnectivity endpoint, or
	// after creating a missing service configuration on an existing
	// endpoint (e.g. first use of a new --service), so the user
	// understands why the first connection may take a moment while the
	// Arc agent registers a relay listener.
	//
	// When false, per-attempt retries are logged at DEBUG only and the
	// dial stays silent for the first ~30 seconds. If the listener still
	// has not appeared after that grace period, a generic progress INFO
	// is emitted periodically so operator-actionable failures are not
	// hidden.
	ExplainSetup bool
}

// DialWithLogger is like Dial but logs the connection attempt and retries
// on transient 404/503 errors (no active listener) with exponential backoff
// until ctx expires.
func DialWithLogger(ctx context.Context, info *RelayInfo, port int, logger *slog.Logger) (*websocket.Conn, error) {
	return DialWithOptions(ctx, info, port, logger, DialOptions{})
}

// DialWithOptions is like DialWithLogger but accepts options that adjust
// progress logging during retries.
func DialWithOptions(ctx context.Context, info *RelayInfo, port int, logger *slog.Logger, opts DialOptions) (*websocket.Conn, error) {
	if port == 0 {
		port = defaultPort
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("dialing arc relay",
		"namespace", info.NamespaceName,
		"hybridConnection", info.HybridConnectionName,
		"port", port)

	wssHost := info.NamespaceName + "." + info.NamespaceNameSuffix
	headers := http.Header{}
	headers.Set("Servicebusauthorization", info.AccessKey)
	headers.Set("Service-Configuration-Token", info.ServiceConfigurationToken)
	headers.Set("Microsoft-Guestgateway-Target", fmt.Sprintf("localhost:%d", port))

	delay := retryInitial
	start := time.Now()
	attempts := 0
	var lastStatus int
	var lastProgressLog time.Time
	progressLogged := false
	for {
		attempts++
		connectURL := fmt.Sprintf("wss://%s/$hc/%s?sb-hc-action=connect&sb-hc-id=%s",
			wssHost, info.HybridConnectionName, newUUID())

		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		ws, resp, err := websocket.Dial(dialCtx, connectURL, &websocket.DialOptions{
			HTTPHeader: headers,
		})
		cancel()

		if err == nil {
			// Only log success at INFO if we'd already announced a wait —
			// otherwise we'd suddenly produce a connected line out of nowhere
			// after a silent transient retry.
			if progressLogged {
				logger.Info("arc relay connected",
					"elapsed", time.Since(start).Truncate(time.Second),
					"attempts", attempts,
					"lastStatus", lastStatus)
			} else {
				logger.Debug("arc relay connected",
					"elapsed", time.Since(start).Truncate(time.Second),
					"attempts", attempts)
			}
			return ws, nil
		}

		// If the parent context was canceled mid-dial, surface the enriched
		// diagnostic instead of the generic non-retryable websocket error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, giveUpErr(attempts, time.Since(start), lastStatus, ctxErr)
		}

		if resp == nil || !relay.IsRetryableStatus(resp.StatusCode) {
			logger.Warn("arc relay dial failed", "error", sanitizeErr(err))
			return nil, fmt.Errorf("dial arc relay: %w", sanitizeErr(err))
		}

		lastStatus = resp.StatusCode
		logger.Debug("arc relay dial returned retryable status",
			"status", resp.StatusCode,
			"attempt", attempts,
			"delay", delay,
			"error", sanitizeErr(err))

		// Progress logging:
		//   - ExplainSetup=true: emit immediately on first retry, then every
		//     progressLogPeriod. This is the "we just created (or updated)
		//     the HybridConnectivity configuration, waiting for the agent
		//     to register" path -- covers both endpoint creation (404) and
		//     adding a missing service configuration to an existing
		//     endpoint (412).
		//   - ExplainSetup=false: stay quiet for progressLogQuietDelay (covers
		//     transient hiccups), then emit progress every progressLogPeriod
		//     so operators see real "listener never appears" cases.
		if opts.ExplainSetup {
			if attempts == 1 {
				logger.Info("waiting for Arc agent to register a relay listener (expected after creating or updating the HybridConnectivity configuration)",
					"status", resp.StatusCode)
				lastProgressLog = time.Now()
				progressLogged = true
			} else if time.Since(lastProgressLog) >= progressLogPeriod {
				logger.Info("still waiting for Arc agent to register a relay listener",
					"elapsed", time.Since(start).Truncate(time.Second),
					"attempts", attempts)
				lastProgressLog = time.Now()
				progressLogged = true
			}
		} else if time.Since(start) >= progressLogQuietDelay && time.Since(lastProgressLog) >= progressLogPeriod {
			logger.Info("still waiting for arc relay listener",
				"elapsed", time.Since(start).Truncate(time.Second),
				"attempts", attempts,
				"lastStatus", lastStatus)
			lastProgressLog = time.Now()
			progressLogged = true
		}

		select {
		case <-ctx.Done():
			return nil, giveUpErr(attempts, time.Since(start), lastStatus, ctx.Err())
		case <-time.After(delay):
		}

		delay = min(delay*retryMultiplier, retryMax)
	}
}

// giveUpErr formats the error returned when ctx terminates while we're
// retrying the dial. lastStatus is omitted when zero (cancellation arrived
// before we ever observed a retryable HTTP response).
func giveUpErr(attempts int, elapsed time.Duration, lastStatus int, cause error) error {
	noun := "attempts"
	if attempts == 1 {
		noun = "attempt"
	}
	if lastStatus == 0 {
		return fmt.Errorf("dial arc relay: gave up after %d %s in %s (no HTTP response): %w",
			attempts, noun, elapsed.Truncate(time.Second), cause)
	}
	return fmt.Errorf("dial arc relay: gave up after %d %s in %s (last status %d): %w",
		attempts, noun, elapsed.Truncate(time.Second), lastStatus, cause)
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

// ARMError is returned by Client methods when an ARM API call responds with
// an HTTP 4xx or 5xx status. Callers can use errors.As to inspect the
// status code (e.g. to detect first-time setup conditions: 404
// ResourceNotFound or 412 PreconditionFailed).
type ARMError struct {
	StatusCode int
	Body       []byte
}

func (e *ARMError) Error() string {
	return fmt.Sprintf("ARM API error (HTTP %d): %s", e.StatusCode, string(e.Body))
}

func newARMError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return &ARMError{StatusCode: resp.StatusCode, Body: body}
}

// sanitizedError wraps an error with a redacted message while preserving
// the original error chain for errors.Is/As.
type sanitizedError struct {
	msg string
	err error
}

func (e *sanitizedError) Error() string { return e.msg }
func (e *sanitizedError) Unwrap() error { return e.err }

// sanitizeErr strips sensitive tokens from WebSocket dial errors.
// The returned error preserves the original error chain for errors.Is/As.
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
	return &sanitizedError{msg: s, err: err}
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
