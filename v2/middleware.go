package x402

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// V2 header names.
const (
	HeaderPaymentSignature = "PAYMENT-SIGNATURE"
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"

	// V1 legacy header names.
	HeaderLegacyPayment         = "X-PAYMENT"
	HeaderLegacyPaymentResponse = "X-PAYMENT-RESPONSE"
)

var errHandlerStatus = errors.New("x402: handler wrote a non-success status")

// PaymentMiddleware enforces x402 payment requirements on an HTTP handler.
// PAYMENT-SIGNATURE is the v2 header. X-PAYMENT is accepted as a v1 fallback.
// The default flow verifies, runs the handler, settles, then releases a 2xx body.
func PaymentMiddleware(cfg Config) func(http.Handler) http.Handler {
	srv, err := NewServer(cfg)
	if err != nil {
		panic(fmt.Sprintf("invalid x402 middleware configuration: %v", err))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			srv.serveHTTP(next, w, r)
		})
	}
}

func (s *Server) serveHTTP(next http.Handler, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		applyCORS(w.Header(), r)
		next.ServeHTTP(w, r)
		return
	}

	if _, priced := s.match(Call{RouteKey: r.URL.Path}); !priced {
		applyCORS(w.Header(), r)
		next.ServeHTTP(w, r)
		return
	}

	header := r.Header.Get(HeaderPaymentSignature)
	legacy := false
	if header == "" {
		header = r.Header.Get(HeaderLegacyPayment)
		legacy = header != ""
	}

	var deadline time.Duration
	if until, ok := r.Context().Deadline(); ok {
		deadline = time.Until(until)
	}

	bw := newBufferWriter(s.cfg.MaxBufferBytes)
	call := Call{
		RouteKey:      r.URL.Path,
		ResourceURL:   requestResourceURL(r),
		Deadline:      deadline,
		PaymentHeader: header,
		Legacy:        legacy,
	}

	result := s.Execute(r.Context(), call, func(ctx context.Context) (HandlerOutput, error) {
		next.ServeHTTP(bw, r.WithContext(ctx))
		if bw.overflow {
			return HandlerOutput{}, ErrBufferOverflow
		}
		status := bw.status
		if status == 0 {
			status = http.StatusOK
		}
		out := HandlerOutput{
			Body:   bw.body(),
			Status: status,
			Header: bw.headerCopy(),
		}
		if status < 200 || status >= 300 {
			return out, errHandlerStatus
		}
		return out, nil
	})

	s.writeHTTP(w, r, bw, result)
}

func (s *Server) writeHTTP(w http.ResponseWriter, r *http.Request, bw *bufferWriter, result Result) {
	switch result.Outcome {
	case OutcomeFree:
		applyCORS(w.Header(), r)
		copyHeaders(w.Header(), bw.header)
		status := bw.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(bw.body())
	case OutcomePaymentRequired:
		s.writeChallenge(w, r, result)
	case OutcomeInvalid:
		applyCORS(w.Header(), r)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payment header"})
	case OutcomeInternal:
		applyCORS(w.Header(), r)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "facilitator request failed"})
	case OutcomeBufferOverflow:
		applyCORS(w.Header(), r)
		if result.Receipt != nil {
			s.setPaymentHeader(w.Header(), result)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "response exceeds MaxBufferBytes; set PaymentFlow to upfront",
		})
	case OutcomeHandlerFailed:
		// Authorization did not settle. Upfront and escrow already did, and the
		// receipt is attached so the client can see that it was charged.
		flushBuffered(w, r, bw, result, false)
	case OutcomeSettled:
		flushBuffered(w, r, bw, result, true)
	case OutcomeReplay:
		writeReplay(w, r, result)
	case OutcomeSettleFailed:
		s.writeSettlementFailure(w, r, result)
	default:
		applyCORS(w.Header(), r)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "payment processing failed"})
	}
}

func (s *Server) writeChallenge(w http.ResponseWriter, r *http.Request, result Result) {
	h := w.Header()
	applyCORS(h, r)
	h.Set("Cache-Control", "no-store")
	if result.Challenge != nil {
		if encoded, err := encodeJSON(result.Challenge); err == nil {
			h.Set(HeaderPaymentRequired, encoded)
		}
	}
	if s.cfg.CustomPaywallHTML != "" && isBrowserRequest(r) {
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(s.cfg.CustomPaywallHTML))
		return
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	if result.Challenge != nil {
		_ = json.NewEncoder(w).Encode(result.Challenge)
	}
}

func (s *Server) writeSettlementFailure(w http.ResponseWriter, r *http.Request, result Result) {
	h := w.Header()
	applyCORS(h, r)
	h.Set("Cache-Control", "no-store")
	s.setPaymentHeader(h, result)
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = w.Write([]byte("{}\n"))
}

func flushBuffered(w http.ResponseWriter, r *http.Request, bw *bufferWriter, result Result, paid bool) {
	h := w.Header()
	copyHeaders(h, bw.header)
	applyCORS(h, r)
	if result.Receipt != nil {
		setReceiptHeader(h, result)
	}
	if paid {
		appendCachePrivate(h)
	}
	status := bw.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(bw.body())
}

func writeReplay(w http.ResponseWriter, r *http.Request, result Result) {
	h := w.Header()
	copyHeaders(h, result.Output.Header)
	applyCORS(h, r)
	setReceiptHeader(h, result)
	appendCachePrivate(h)
	status := result.Output.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(result.Output.Body)
}

func (s *Server) setPaymentHeader(h http.Header, result Result) {
	setReceiptHeader(h, result)
}

func setReceiptHeader(h http.Header, result Result) {
	if result.Receipt == nil {
		return
	}
	encoded, err := encodeJSON(result.Receipt)
	if err != nil {
		return
	}
	if result.Legacy {
		h.Set(HeaderLegacyPaymentResponse, encoded)
		return
	}
	h.Set(HeaderPaymentResponse, encoded)
}

func encodeJSON(v interface{}) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func requestResourceURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := forwardedProto(r); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "example.com"
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

func forwardedProto(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(proto, ",")[0])
}

func applyCORS(h http.Header, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if h.Get("Access-Control-Allow-Origin") == "" {
			h.Set("Access-Control-Allow-Origin", "*")
		}
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		appendVary(h, "Origin")
	}
	h.Set("Access-Control-Expose-Headers", mergeList(h.Get("Access-Control-Expose-Headers"),
		HeaderPaymentRequired, HeaderPaymentResponse, HeaderLegacyPaymentResponse))
	h.Set("Access-Control-Allow-Headers", mergeList(h.Get("Access-Control-Allow-Headers"),
		HeaderPaymentSignature, "Content-Type", HeaderLegacyPayment))
	if h.Get("Access-Control-Allow-Methods") == "" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	}
	if h.Get("Access-Control-Max-Age") == "" {
		h.Set("Access-Control-Max-Age", "600")
	}
}

func mergeList(existing string, add ...string) string {
	seen := make(map[string]struct{})
	var out []string
	appendPart := func(part string) {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		key := strings.ToLower(part)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, part)
	}
	for _, part := range strings.Split(existing, ",") {
		appendPart(part)
	}
	for _, part := range add {
		appendPart(part)
	}
	return strings.Join(out, ", ")
}

func appendVary(h http.Header, value string) {
	existing := h.Get("Vary")
	if existing == "" {
		h.Set("Vary", value)
		return
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(part), value) {
			return
		}
	}
	h.Set("Vary", existing+", "+value)
}

func appendCachePrivate(h http.Header) {
	cc := h.Get("Cache-Control")
	if cc == "" {
		h.Set("Cache-Control", "private")
		return
	}
	for _, part := range strings.Split(cc, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "private") {
			return
		}
	}
	h.Set("Cache-Control", cc+", private")
}

func copyHeaders(dst http.Header, src map[string][]string) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type bufferWriter struct {
	header   http.Header
	buf      bytes.Buffer
	status   int
	max      int
	overflow bool
	wrote    bool
}

func newBufferWriter(max int) *bufferWriter {
	return &bufferWriter{header: make(http.Header), max: max}
}

func (b *bufferWriter) Header() http.Header { return b.header }

func (b *bufferWriter) WriteHeader(status int) {
	if b.wrote {
		return
	}
	b.wrote = true
	b.status = status
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	if b.overflow {
		return 0, ErrBufferOverflow
	}
	if b.max > 0 && b.buf.Len()+len(p) > b.max {
		b.overflow = true
		b.buf.Reset()
		return 0, ErrBufferOverflow
	}
	return b.buf.Write(p)
}

func (b *bufferWriter) body() []byte {
	return b.buf.Bytes()
}

func (b *bufferWriter) headerCopy() map[string][]string {
	if len(b.header) == 0 {
		return nil
	}
	out := make(map[string][]string, len(b.header))
	for key, values := range b.header {
		out[key] = append([]string(nil), values...)
	}
	return out
}

// buildRequirementsFromRule returns requirements for the first accepted token.
// V1 payments use it when the route advertises one option.
func buildRequirementsFromRule(rule *PricingRule) *PaymentRequirements {
	accepts := BuildAccepts(rule, 0)
	if len(accepts) == 0 {
		return nil
	}
	return &accepts[0]
}

// buildAcceptsFromRule constructs every PaymentRequirements value for a rule.
func buildAcceptsFromRule(rule *PricingRule, validityDuration time.Duration) []PaymentRequirements {
	return BuildAccepts(rule, validityDuration)
}

// parsePaymentPayload decodes a V2 PAYMENT-SIGNATURE header.
func parsePaymentPayload(header string) (*PaymentPayload, error) {
	payloadBytes, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	var payload PaymentPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	if payload.X402Version < 2 {
		return nil, fmt.Errorf("PAYMENT-SIGNATURE header requires x402Version >= 2, got %d", payload.X402Version)
	}

	if payload.Payload == nil {
		return nil, fmt.Errorf("payload is required")
	}

	return &payload, nil
}

// parseLegacyPayment decodes a V1 X-PAYMENT header into a V2 PaymentPayload.
// Network is left as the client sent it. Amount, asset, and payTo are filled
// from the single advertised option.
func parseLegacyPayment(header string, requirements *PaymentRequirements) (*PaymentPayload, error) {
	payloadBytes, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	var legacy LegacyPayment
	if err := json.Unmarshal(payloadBytes, &legacy); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	if legacy.X402Version == 0 {
		return nil, fmt.Errorf("x402Version is required")
	}
	if legacy.Scheme == "" {
		return nil, fmt.Errorf("scheme is required")
	}
	if legacy.Network == "" {
		return nil, fmt.Errorf("network is required")
	}
	if legacy.Payload == nil {
		return nil, fmt.Errorf("payload is required")
	}

	accepted := PaymentRequirements{
		Scheme:  legacy.Scheme,
		Network: legacy.Network,
	}
	if requirements != nil {
		accepted.Amount = requirements.Amount
		accepted.Asset = requirements.Asset
		accepted.PayTo = requirements.PayTo
		accepted.MaxTimeoutSeconds = requirements.MaxTimeoutSeconds
		accepted.Extra = requirements.Extra
	}

	return &PaymentPayload{
		X402Version: legacy.X402Version,
		Accepted:    accepted,
		Payload:     legacy.Payload,
	}, nil
}

// GetPaymentFromContext extracts payment information from the request context.
func GetPaymentFromContext(ctx context.Context) (*PaymentContext, bool) {
	payment, ok := ctx.Value(PaymentContextKey).(*PaymentContext)
	return payment, ok
}

// RequirePayment extracts payment from context and returns an error if it is missing.
func RequirePayment(ctx context.Context) (*PaymentContext, error) {
	payment, ok := GetPaymentFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("payment context not found")
	}
	if !payment.Verified {
		return nil, fmt.Errorf("payment not verified")
	}
	return payment, nil
}

// EncodePaymentPayload encodes a PaymentPayload to base64 JSON for PAYMENT-SIGNATURE.
func EncodePaymentPayload(payload *PaymentPayload) (string, error) {
	return encodeJSON(payload)
}

// DecodePaymentResponse decodes a PAYMENT-RESPONSE header.
func DecodePaymentResponse(header string) (*PaymentResponse, error) {
	responseBytes, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	var response PaymentResponse
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &response, nil
}

// ReadPaymentRequirements extracts payment requirements from a 402 response.
func ReadPaymentRequirements(resp *http.Response) (*PaymentRequiredResponse, error) {
	if resp.StatusCode != http.StatusPaymentRequired {
		return nil, fmt.Errorf("expected status 402, got %d", resp.StatusCode)
	}

	if header := resp.Header.Get(HeaderPaymentRequired); header != "" {
		decoded, err := base64.StdEncoding.DecodeString(header)
		if err == nil {
			var paymentReq PaymentRequiredResponse
			if err := json.Unmarshal(decoded, &paymentReq); err == nil {
				return &paymentReq, nil
			}
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var paymentReq PaymentRequiredResponse
	if err := json.Unmarshal(body, &paymentReq); err != nil {
		return nil, fmt.Errorf("failed to parse payment requirements: %w", err)
	}

	return &paymentReq, nil
}

func isBrowserRequest(r *http.Request) bool {
	userAgent := r.Header.Get("User-Agent")
	if userAgent == "" {
		return false
	}

	browserIndicators := []string{"Mozilla/", "Chrome/", "Safari/", "Firefox/", "Edge/", "Opera/"}
	for _, indicator := range browserIndicators {
		if strings.Contains(userAgent, indicator) {
			return true
		}
	}

	return false
}
