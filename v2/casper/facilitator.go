package casper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultFacilitatorURL is the public Casper x402 facilitator operated by
// MAKE Software. See https://docs.cspr.cloud.
const DefaultFacilitatorURL = "https://x402-facilitator.cspr.cloud"

// defaultTimeout bounds a facilitator call that the caller's context does not.
const defaultTimeout = 30 * time.Second

// FacilitatorError is returned when the facilitator answers with a non-2xx
// status, so callers can tell a rejected payment from a broken facilitator.
type FacilitatorError struct {
	Endpoint   string
	StatusCode int
	Body       string
}

func (e *FacilitatorError) Error() string {
	return fmt.Sprintf("facilitator %s returned status %d: %s", e.Endpoint, e.StatusCode, e.Body)
}

// FacilitatorClient handles communication with a Casper x402 facilitator.
type FacilitatorClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// FacilitatorOption configures a FacilitatorClient.
type FacilitatorOption func(*FacilitatorClient)

// WithHTTPClient makes the client use the given *http.Client, for callers that
// need their own transport, proxy or timeout.
func WithHTTPClient(client *http.Client) FacilitatorOption {
	return func(c *FacilitatorClient) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithAPIKey sends key as the Authorization header on every facilitator call.
// The public Casper facilitator requires one; local facilitators may not.
func WithAPIKey(key string) FacilitatorOption {
	return func(c *FacilitatorClient) {
		c.apiKey = key
	}
}

// NewFacilitatorClient creates a client for the Casper facilitator at baseURL.
// An empty baseURL falls back to DefaultFacilitatorURL.
func NewFacilitatorClient(baseURL string, opts ...FacilitatorOption) *FacilitatorClient {
	if baseURL == "" {
		baseURL = DefaultFacilitatorURL
	}

	c := &FacilitatorClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Verify checks if a payment is valid via POST /verify.
func (c *FacilitatorClient) Verify(ctx context.Context, req *FacilitatorVerifyRequest) (*FacilitatorVerifyResponse, error) {
	var resp FacilitatorVerifyResponse
	if err := c.post(ctx, "/verify", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// Settle executes the payment on-chain via POST /settle.
func (c *FacilitatorClient) Settle(ctx context.Context, req *FacilitatorSettleRequest) (*FacilitatorSettleResponse, error) {
	var resp FacilitatorSettleResponse
	if err := c.post(ctx, "/settle", req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetSupported fetches the supported scheme+network pairs via GET /supported.
func (c *FacilitatorClient) GetSupported(ctx context.Context) (*FacilitatorSupportedResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/supported", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create supported request: %w", err)
	}

	var resp FacilitatorSupportedResponse
	if err := c.do(httpReq, "/supported", &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// post sends body as JSON to the endpoint and decodes the response into out.
func (c *FacilitatorClient) post(ctx context.Context, endpoint string, body, out interface{}) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal %s request: %w", endpoint, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("failed to create %s request: %w", endpoint, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	return c.do(httpReq, endpoint, out)
}

// do sends the request, turns a non-2xx status into a *FacilitatorError, and
// decodes a successful response into out.
func (c *FacilitatorClient) do(httpReq *http.Request, endpoint string, out interface{}) error {
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to call facilitator %s endpoint: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &FacilitatorError{
			Endpoint:   endpoint,
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to decode %s response: %w", endpoint, err)
	}

	return nil
}
