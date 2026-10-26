package evm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
)

const (
	// DefaultFacilitatorURL is the public facilitator. The path already includes its prefix.
	DefaultFacilitatorURL = "https://x402.org/facilitator"

	defaultVerifyTimeout = 5 * time.Second
	defaultSettleTimeout = 90 * time.Second

	supportedAttempts  = 3
	supportedBaseDelay = time.Second
	maxResponseBytes   = 1 << 20
)

// AuthHeaders are request headers for one facilitator call.
type AuthHeaders struct {
	Verify    map[string]string
	Settle    map[string]string
	Supported map[string]string
}

// AuthProvider supplies per-endpoint headers, for example CDP credentials.
type AuthProvider interface {
	GetAuthHeaders(ctx context.Context) (AuthHeaders, error)
}

// FacilitatorClient calls a facilitator's /verify, /settle, and /supported routes.
type FacilitatorClient struct {
	baseURL          string
	httpClient       *http.Client
	auth             AuthProvider
	verifyTimeout    time.Duration
	settleTimeout    time.Duration
	supportedRetries int
	retryBaseDelay   time.Duration
}

// NewFacilitatorClient returns a client for a facilitator base URL.
// The base URL already includes any vendor prefix, for example
// https://x402.org/facilitator or https://api.cdp.coinbase.com/platform/v2/x402.
func NewFacilitatorClient(baseURL string) *FacilitatorClient {
	if baseURL == "" {
		baseURL = DefaultFacilitatorURL
	}
	return &FacilitatorClient{
		baseURL:          strings.TrimRight(baseURL, "/"),
		httpClient:       defaultHTTPClient(),
		verifyTimeout:    defaultVerifyTimeout,
		settleTimeout:    defaultSettleTimeout,
		supportedRetries: supportedAttempts,
		retryBaseDelay:   supportedBaseDelay,
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// SetAuthProvider attaches a header hook. A nil provider clears it.
func (c *FacilitatorClient) SetAuthProvider(p AuthProvider) {
	c.auth = p
}

// SetHTTPClient replaces the shared keep-alive client.
func (c *FacilitatorClient) SetHTTPClient(client *http.Client) {
	if client != nil {
		c.httpClient = client
	}
}

// Close releases idle connections.
func (c *FacilitatorClient) Close() {
	c.httpClient.CloseIdleConnections()
}

// Verify calls POST {base}/verify. The request context is limited to 5 seconds
// unless the caller deadline is sooner.
func (c *FacilitatorClient) Verify(ctx context.Context, req *FacilitatorVerifyRequest) (*FacilitatorVerifyResponse, error) {
	ctx, cancel := c.withTimeout(ctx, c.verifyTimeout)
	defer cancel()

	body, err := c.post(ctx, "/verify", req, func(h AuthHeaders) map[string]string { return h.Verify })
	if err != nil {
		return nil, err
	}
	if body.status != http.StatusOK {
		parsed, ok := decodeVerifyFailure(body.payload)
		if !ok {
			return nil, fmt.Errorf("facilitator /verify returned status %d: %s", body.status, excerpt(body.payload))
		}
		parsed.ExtensionResponses = body.extensions
		return parsed, nil
	}

	parsed, err := parseVerify(body.payload)
	if err != nil {
		return nil, err
	}
	parsed.ExtensionResponses = body.extensions
	return parsed, nil
}

// Settle calls POST {base}/settle. The request context is limited to 90 seconds
// unless the caller deadline is sooner. Success false is returned as a response.
func (c *FacilitatorClient) Settle(ctx context.Context, req *FacilitatorSettleRequest) (*FacilitatorSettleResponse, error) {
	ctx, cancel := c.withTimeout(ctx, c.settleTimeout)
	defer cancel()

	body, err := c.post(ctx, "/settle", req, func(h AuthHeaders) map[string]string { return h.Settle })
	if err != nil {
		return nil, err
	}
	if body.status != http.StatusOK {
		parsed, ok := decodeSettleFailure(body.payload)
		if !ok {
			return nil, fmt.Errorf("facilitator /settle returned status %d: %s", body.status, excerpt(body.payload))
		}
		parsed.ExtensionResponses = body.extensions
		return parsed, nil
	}

	parsed, err := parseSettle(body.payload)
	if err != nil {
		return nil, err
	}
	parsed.ExtensionResponses = body.extensions
	return parsed, nil
}

// GetSupported calls GET {base}/supported.
// A 429 is retried up to three times with exponential backoff from 1 second.
func (c *FacilitatorClient) GetSupported(ctx context.Context) (*x402.SupportedResponse, error) {
	attempts := c.supportedRetries
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := c.getSupportedOnce(ctx)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var limited *statusError
		if !errorsAsStatus(err, &limited) || limited.code != http.StatusTooManyRequests || attempt == attempts-1 {
			return nil, err
		}
		delay := c.retryBaseDelay
		if delay <= 0 {
			delay = supportedBaseDelay
		}
		delay = delay * time.Duration(1<<uint(attempt))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (c *FacilitatorClient) getSupportedOnce(ctx context.Context) (*x402.SupportedResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/supported", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create supported request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if err := c.applyAuth(ctx, httpReq, func(h AuthHeaders) map[string]string { return h.Supported }); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("supported request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := readLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{code: resp.StatusCode, body: payload}
	}
	return parseSupported(payload)
}

type httpBody struct {
	payload    []byte
	extensions map[string]interface{}
	status     int
}

func (c *FacilitatorClient) post(ctx context.Context, path string, payload interface{}, headers func(AuthHeaders) map[string]string) (httpBody, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return httpBody{}, fmt.Errorf("failed to marshal facilitator request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return httpBody{}, fmt.Errorf("failed to create facilitator request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if err := c.applyAuth(ctx, httpReq, headers); err != nil {
		return httpBody{}, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return httpBody{}, fmt.Errorf("facilitator %s request failed: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body)
	if err != nil {
		return httpBody{}, err
	}
	return httpBody{payload: body, extensions: extensionResponses(resp.Header), status: resp.StatusCode}, nil
}

func (c *FacilitatorClient) applyAuth(ctx context.Context, req *http.Request, pick func(AuthHeaders) map[string]string) error {
	if c.auth == nil {
		return nil
	}
	headers, err := c.auth.GetAuthHeaders(ctx)
	if err != nil {
		return fmt.Errorf("facilitator auth: %w", err)
	}
	for key, value := range pick(headers) {
		req.Header.Set(key, value)
	}
	return nil
}

func (c *FacilitatorClient) withTimeout(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= limit {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, limit)
}

type statusError struct {
	code int
	body []byte
}

func (e *statusError) Error() string {
	return fmt.Sprintf("facilitator supported returned status %d: %s", e.code, excerpt(e.body))
}

func errorsAsStatus(err error, target **statusError) bool {
	se, ok := err.(*statusError)
	if !ok {
		return false
	}
	*target = se
	return true
}

func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read facilitator response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("facilitator response exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}

func excerpt(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		return text[:200]
	}
	return text
}

func extensionResponses(h http.Header) map[string]interface{} {
	raw := h.Get("EXTENSION-RESPONSES")
	if raw == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(decoded, &out); err != nil {
		return nil
	}
	return out
}

type verifyEnvelope struct {
	IsValid       *bool                  `json:"isValid"`
	InvalidReason string                 `json:"invalidReason,omitempty"`
	Payer         string                 `json:"payer,omitempty"`
	Extensions    map[string]interface{} `json:"extensions,omitempty"`
}

func decodeVerifyFailure(body []byte) (*FacilitatorVerifyResponse, bool) {
	var env verifyEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.InvalidReason == "" {
		return nil, false
	}
	return &FacilitatorVerifyResponse{
		IsValid:       false,
		InvalidReason: env.InvalidReason,
		Payer:         env.Payer,
		Extensions:    env.Extensions,
	}, true
}

func decodeSettleFailure(body []byte) (*FacilitatorSettleResponse, bool) {
	var env settleEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.ErrorReason == "" {
		return nil, false
	}
	transaction := ""
	if env.Transaction != nil {
		transaction = *env.Transaction
	}
	network := ""
	if env.Network != nil {
		network = *env.Network
	}
	return &FacilitatorSettleResponse{
		Success:     false,
		ErrorReason: env.ErrorReason,
		Payer:       env.Payer,
		Transaction: transaction,
		Network:     network,
		Amount:      env.Amount,
		Extensions:  env.Extensions,
	}, true
}

func parseVerify(body []byte) (*FacilitatorVerifyResponse, error) {
	var env verifyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to decode verify response: %w", err)
	}
	if env.IsValid == nil {
		return nil, fmt.Errorf("facilitator verify response missing isValid")
	}
	return &FacilitatorVerifyResponse{
		IsValid:       *env.IsValid,
		InvalidReason: env.InvalidReason,
		Payer:         env.Payer,
		Extensions:    env.Extensions,
	}, nil
}

type settleEnvelope struct {
	Success     *bool                  `json:"success"`
	ErrorReason string                 `json:"errorReason,omitempty"`
	Payer       string                 `json:"payer,omitempty"`
	Transaction *string                `json:"transaction"`
	Network     *string                `json:"network"`
	Amount      string                 `json:"amount,omitempty"`
	Extensions  map[string]interface{} `json:"extensions,omitempty"`
}

func parseSettle(body []byte) (*FacilitatorSettleResponse, error) {
	var env settleEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to decode settle response: %w", err)
	}
	if env.Success == nil || env.Transaction == nil || env.Network == nil {
		return nil, fmt.Errorf("facilitator settle response missing success, transaction, or network")
	}
	return &FacilitatorSettleResponse{
		Success:     *env.Success,
		ErrorReason: env.ErrorReason,
		Payer:       env.Payer,
		Transaction: *env.Transaction,
		Network:     *env.Network,
		Amount:      env.Amount,
		Extensions:  env.Extensions,
	}, nil
}

type supportedKindEnvelope struct {
	X402Version *int                   `json:"x402Version"`
	Scheme      string                 `json:"scheme"`
	Network     string                 `json:"network"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

type supportedEnvelope struct {
	Kinds      []supportedKindEnvelope `json:"kinds"`
	Extensions []string                `json:"extensions"`
	Signers    map[string][]string     `json:"signers"`
}

func parseSupported(body []byte) (*x402.SupportedResponse, error) {
	var env supportedEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to decode supported response: %w", err)
	}
	kinds := make([]x402.SupportedKind, 0, len(env.Kinds))
	for _, kind := range env.Kinds {
		if kind.X402Version == nil || kind.Scheme == "" || kind.Network == "" {
			return nil, fmt.Errorf("facilitator supported response missing kind fields")
		}
		kinds = append(kinds, x402.SupportedKind{
			X402Version: *kind.X402Version,
			Scheme:      kind.Scheme,
			Network:     kind.Network,
			Extra:       kind.Extra,
		})
	}
	extensions := env.Extensions
	if extensions == nil {
		extensions = []string{}
	}
	signers := env.Signers
	if signers == nil {
		signers = map[string][]string{}
	}
	return &x402.SupportedResponse{
		Kinds:      kinds,
		Extensions: extensions,
		Signers:    signers,
	}, nil
}
