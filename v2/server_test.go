package x402

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func countingVerifier() (*MockVerifier, *[]string) {
	var mu sync.Mutex
	order := []string{}
	note := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	v := &MockVerifier{
		VerifyFunc: func(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*VerificationResult, error) {
			note("verify")
			return &VerificationResult{Valid: true, PayerAddress: "0xPayer", Amount: requirements.Amount}, nil
		},
		SettleFunc: func(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*SettlementResult, error) {
			note("settle")
			return &SettlementResult{
				Success:         true,
				TransactionHash: "0xtxhash123",
				Status:          "success",
				Amount:          requirements.Amount,
				PayerAddress:    "0xPayer",
				Network:         requirements.Network,
			}, nil
		},
	}
	return v, &order
}

func pricedConfig(v ChainVerifier, flow string) Config {
	return Config{
		Verifier: v,
		EndpointPricing: map[string]PricingRule{
			"/v1/paid": {
				Description: "premium",
				MimeType:    "application/json",
				PaymentFlow: flow,
				ResourceURL: "https://api.example.com/v1/paid",
				AcceptedTokens: []TokenRequirement{
					{
						Network:       "eip155:84532",
						Symbol:        "USDC",
						AssetContract: "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
						Recipient:     "0xRecipient",
						Amount:        "1000000",
						EIP712Name:    "USD Coin",
						EIP712Version: "2",
					},
				},
			},
		},
	}
}

func TestSpecSamples(t *testing.T) {
	tests := []struct {
		name string
		file string
		want func(t *testing.T, raw []byte)
	}{
		{
			name: "payment required",
			file: "testdata/payment_required.json",
			want: func(t *testing.T, raw []byte) {
				var got PaymentRequiredResponse
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if got.X402Version != 2 {
					t.Errorf("x402Version = %d", got.X402Version)
				}
				if got.Resource == nil || got.Resource.URL != "https://api.example.com/premium-data" {
					t.Errorf("resource.url = %+v", got.Resource)
				}
				if len(got.Accepts) != 1 || got.Accepts[0].MaxTimeoutSeconds != 60 {
					t.Errorf("accepts = %+v", got.Accepts)
				}
				if got.Accepts[0].Amount != "10000" || got.Accepts[0].Network != "eip155:84532" {
					t.Errorf("accept = %+v", got.Accepts[0])
				}
			},
		},
		{
			name: "payment payload keeps string timestamps",
			file: "testdata/payment_payload.json",
			want: func(t *testing.T, raw []byte) {
				var got PaymentPayload
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				body, ok := got.Payload.(map[string]interface{})
				if !ok {
					t.Fatalf("payload type %T", got.Payload)
				}
				auth, ok := body["authorization"].(map[string]interface{})
				if !ok {
					t.Fatalf("authorization type %T", body["authorization"])
				}
				if _, ok := auth["validAfter"].(string); !ok {
					t.Fatalf("validAfter = %T", auth["validAfter"])
				}
				if got.Resource == nil || got.Resource.URL == "" {
					t.Error("resource.url is empty")
				}
			},
		},
		{
			name: "settlement success",
			file: "testdata/payment_response.json",
			want: func(t *testing.T, raw []byte) {
				var got PaymentResponse
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if !got.Success || got.Transaction == "" || got.Network != "eip155:84532" {
					t.Errorf("response = %+v", got)
				}
			},
		},
		{
			name: "settlement failure keeps empty transaction",
			file: "testdata/payment_response_failure.json",
			want: func(t *testing.T, raw []byte) {
				var decoded map[string]interface{}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if _, ok := decoded["transaction"]; !ok {
					t.Error("transaction field missing")
				}
				if _, ok := decoded["network"]; !ok {
					t.Error("network field missing")
				}
				var got PaymentResponse
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if got.Success || got.ErrorReason != "insufficient_funds" || got.Transaction != "" {
					t.Errorf("response = %+v", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(tt.file)
			if err != nil {
				t.Fatal(err)
			}
			tt.want(t, raw)
		})
	}
}

func TestPaymentResponseAlwaysEncodesTransactionAndNetwork(t *testing.T) {
	raw, err := json.Marshal(PaymentResponse{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["transaction"]; !ok {
		t.Error("transaction omitted")
	}
	if _, ok := decoded["network"]; !ok {
		t.Error("network omitted")
	}
}

func TestChallengeResourceAndTimeout(t *testing.T) {
	v, _ := countingVerifier()
	handler := PaymentMiddleware(pricedConfig(v, ""))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/paid?x=1", nil)
	req.Host = "pay.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "PAYMENT-REQUIRED") {
		t.Errorf("expose headers = %q", w.Header().Get("Access-Control-Expose-Headers"))
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("allow origin = %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(w.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q", w.Header().Get("Vary"))
	}

	decoded := decodeRequired(t, w)
	if decoded.Resource == nil || decoded.Resource.URL != "https://api.example.com/v1/paid" {
		t.Errorf("resource = %+v", decoded.Resource)
	}
	if decoded.Resource.Description != "premium" || decoded.Resource.MimeType != "application/json" {
		t.Errorf("resource fields = %+v", decoded.Resource)
	}
	if len(decoded.Accepts) != 1 {
		t.Fatalf("accepts = %d", len(decoded.Accepts))
	}
	accept := decoded.Accepts[0]
	if accept.MaxTimeoutSeconds != 300 {
		t.Errorf("maxTimeoutSeconds = %d", accept.MaxTimeoutSeconds)
	}
	if accept.Extra["name"] != "USD Coin" || accept.Extra["version"] != "2" {
		t.Errorf("extra = %+v", accept.Extra)
	}
	if _, ok := accept.Extra["assetTransferMethod"]; ok {
		t.Error("eip3009 assetTransferMethod should be omitted")
	}
}

func TestRequestURLWhenRuleDoesNotOverride(t *testing.T) {
	cfg := testConfig()
	handler := PaymentMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set("X-Forwarded-Proto", "https, http")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	decoded := decodeRequired(t, w)
	if decoded.Resource.URL != "https://example.com/v1/paid" {
		t.Errorf("resource.url = %s", decoded.Resource.URL)
	}
}

func TestDeadlineShrinksMaxTimeout(t *testing.T) {
	cfg := testConfig()
	handler := PaymentMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 45*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	got := decodeRequired(t, w).Accepts[0].MaxTimeoutSeconds
	if got >= 300 || got < 40 {
		t.Errorf("maxTimeoutSeconds = %d, want a value near the 45s deadline", got)
	}
}

func TestAcceptedMismatchDoesNotCallFacilitator(t *testing.T) {
	v, order := countingVerifier()
	handler := PaymentMiddleware(pricedConfig(v, ""))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran")
	}))

	payload := PaymentPayload{
		X402Version: 2,
		Accepted: PaymentRequirements{
			Scheme:            "exact",
			Network:           "eip155:84532",
			Amount:            "1",
			Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			PayTo:             "0xRecipient",
			MaxTimeoutSeconds: 300,
			Extra:             map[string]interface{}{"name": "USD Coin", "version": "2"},
		},
		Payload: map[string]interface{}{"signature": "0xsig"},
	}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set(HeaderPaymentSignature, base64.StdEncoding.EncodeToString(raw))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if len(*order) != 0 {
		t.Errorf("facilitator calls = %v", *order)
	}
}

func TestPaymentFlows(t *testing.T) {
	tests := []struct {
		name      string
		flow      string
		handler   int
		settle    *SettlementResult
		settleErr error
		wantCode  int
		wantOrder []string
		wantBody  string
		wantHash  string
	}{
		{
			name:      "authorization settles after a 200",
			flow:      "",
			handler:   http.StatusOK,
			wantCode:  http.StatusOK,
			wantOrder: []string{"verify", "handler", "settle"},
			wantBody:  "paid-body",
			wantHash:  "0xtxhash123",
		},
		{
			name:      "handler 500 does not settle",
			flow:      "",
			handler:   http.StatusInternalServerError,
			wantCode:  http.StatusInternalServerError,
			wantOrder: []string{"verify", "handler"},
			wantBody:  "paid-body",
		},
		{
			name:      "settle failure is 402 with receipt and no body",
			flow:      "",
			handler:   http.StatusOK,
			settle:    &SettlementResult{Success: false, ErrorReason: "insufficient_funds", TransactionHash: "", Network: "eip155:84532", PayerAddress: "0xPayer"},
			wantCode:  http.StatusPaymentRequired,
			wantOrder: []string{"verify", "handler", "settle"},
		},
		{
			name:      "settlement pending keeps the transaction",
			flow:      "",
			handler:   http.StatusOK,
			settle:    &SettlementResult{Success: false, ErrorReason: "settlement_pending", TransactionHash: "0xpending", Network: "eip155:84532", PayerAddress: "0xPayer"},
			wantCode:  http.StatusPaymentRequired,
			wantOrder: []string{"verify", "handler", "settle"},
			wantHash:  "0xpending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, order := countingVerifier()
			if tt.settle != nil {
				settled := *tt.settle
				v.SettleFunc = func(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*SettlementResult, error) {
					*order = append(*order, "settle")
					return &settled, nil
				}
			}
			var sawHash string
			handler := PaymentMiddleware(pricedConfig(v, tt.flow))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*order = append(*order, "handler")
				if payment, ok := GetPaymentFromContext(r.Context()); ok {
					sawHash = payment.TransactionHash
				}
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(tt.handler)
				_, _ = w.Write([]byte("paid-body"))
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/paid", strings.NewReader("q"))
			req.Header.Set(HeaderPaymentSignature, makeV2PaymentHeader(t))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Fatalf("status = %d body %s", w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(*order, tt.wantOrder) {
				t.Errorf("order = %v, want %v", *order, tt.wantOrder)
			}
			if tt.flow == "" && tt.handler == http.StatusOK && tt.settle == nil && sawHash != "" {
				t.Errorf("tx hash visible during authorization handler: %s", sawHash)
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Errorf("body = %q", w.Body.String())
			}
			if tt.wantBody == "" && strings.Contains(w.Body.String(), "paid-body") {
				t.Errorf("handler body was released: %s", w.Body.String())
			}
			if tt.settle != nil {
				receipt := decodeReceipt(t, w.Header().Get(HeaderPaymentResponse))
				if receipt.Transaction != tt.wantHash {
					t.Errorf("transaction = %q", receipt.Transaction)
				}
				if receipt.Success {
					t.Error("expected success false")
				}
				if receipt.ErrorReason != tt.settle.ErrorReason {
					t.Errorf("errorReason = %q", receipt.ErrorReason)
				}
			}
			if tt.wantCode == http.StatusOK {
				if !strings.Contains(w.Header().Get("Cache-Control"), "private") {
					t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
				}
				if w.Header().Get("Content-Type") != "text/plain" {
					t.Errorf("content type = %q", w.Header().Get("Content-Type"))
				}
			}
		})
	}
}

func TestUpfrontSettlesBeforeHandler(t *testing.T) {
	v, order := countingVerifier()
	handler := PaymentMiddleware(pricedConfig(v, FlowUpfront))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*order = append(*order, "handler")
		payment, ok := GetPaymentFromContext(r.Context())
		if !ok || payment.TransactionHash != "0xtxhash123" {
			t.Errorf("upfront hash = %+v", payment)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("failed"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set(HeaderPaymentSignature, paymentWithFlow(t, FlowUpfront))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !reflect.DeepEqual(*order, []string{"settle", "handler"}) {
		t.Errorf("order = %v", *order)
	}
	if w.Code != http.StatusInternalServerError || w.Body.String() != "failed" {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	receipt := decodeReceipt(t, w.Header().Get(HeaderPaymentResponse))
	if !receipt.Success || receipt.Transaction != "0xtxhash123" {
		t.Errorf("receipt = %+v", receipt)
	}
}

func TestOptionsIsNotPriced(t *testing.T) {
	v, order := countingVerifier()
	handler := PaymentMiddleware(pricedConfig(v, ""))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodOptions, "/v1/paid", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Headers", "payment-signature, content-type")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if len(*order) != 0 {
		t.Errorf("facilitator calls = %v", *order)
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "PAYMENT-SIGNATURE") {
		t.Errorf("allow headers = %q", w.Header().Get("Access-Control-Allow-Headers"))
	}
	if w.Header().Get(HeaderPaymentRequired) != "" {
		t.Error("OPTIONS was priced")
	}
}

func TestBufferOverflowDoesNotSettle(t *testing.T) {
	v, order := countingVerifier()
	cfg := pricedConfig(v, "")
	cfg.MaxBufferBytes = 8
	handler := PaymentMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*order = append(*order, "handler")
		_, _ = io.WriteString(w, "this is far too long")
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set(HeaderPaymentSignature, makeV2PaymentHeader(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if !reflect.DeepEqual(*order, []string{"verify", "handler"}) {
		t.Errorf("order = %v", *order)
	}
	if strings.Contains(w.Body.String(), "far too long") {
		t.Error("overflow body was released")
	}
}

func TestIdempotentRetrySkipsSettle(t *testing.T) {
	v, order := countingVerifier()
	calls := 0
	handler := PaymentMiddleware(pricedConfig(v, ""))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		*order = append(*order, "handler")
		_, _ = w.Write([]byte("once"))
	}))

	payload := paymentWithID(t, "pay_123")
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
		req.Header.Set(HeaderPaymentSignature, payload)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK || w.Body.String() != "once" {
			t.Fatalf("try %d status %d body %s", i, w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Errorf("handler calls = %d", calls)
	}
	if !reflect.DeepEqual(*order, []string{"verify", "handler", "settle"}) {
		t.Errorf("order = %v", *order)
	}
}

func TestV1HeaderWithSeveralTokensPaysTheFirst(t *testing.T) {
	var verified *PaymentRequirements
	v := &MockVerifier{
		VerifyFunc: func(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*VerificationResult, error) {
			copied := *requirements
			verified = &copied
			return &VerificationResult{Valid: true, PayerAddress: "0xPayer", Amount: requirements.Amount}, nil
		},
		SettleFunc: func(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*SettlementResult, error) {
			return &SettlementResult{Success: true, TransactionHash: "0xtx", Status: "success", Amount: requirements.Amount, Network: requirements.Network, PayerAddress: "0xPayer"}, nil
		},
	}
	rule := PricingRule{
		AcceptedTokens: []TokenRequirement{
			{Network: "eip155:84532", Symbol: "USDC", AssetContract: "0x1", Recipient: "0xRecipientA", Amount: "1000000", EIP712Name: "USD Coin", EIP712Version: "2"},
			{Network: "eip155:8453", Symbol: "USDC", AssetContract: "0x2", Recipient: "0xRecipientB", Amount: "2000000", EIP712Name: "USD Coin", EIP712Version: "2"},
		},
	}
	cfg := pricedConfig(v, "")
	cfg.EndpointPricing["/v1/paid"] = rule
	handler := PaymentMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set(HeaderLegacyPayment, makeV1PaymentHeader(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	want := BuildAccepts(&rule, 5*time.Minute)[0]
	if verified == nil || !reflect.DeepEqual(*verified, want) {
		t.Errorf("verified %+v, want %+v", verified, want)
	}
}

func TestExtensionInfoMustBePreserved(t *testing.T) {
	v, order := countingVerifier()
	cfg := pricedConfig(v, "")
	rule := cfg.EndpointPricing["/v1/paid"]
	rule.Extensions = map[string]interface{}{
		"bazaar": map[string]interface{}{"info": map[string]interface{}{"category": "data"}},
	}
	cfg.EndpointPricing["/v1/paid"] = rule

	handler := PaymentMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	changed := paymentWithExtensions(t, map[string]interface{}{
		"bazaar": map[string]interface{}{"info": map[string]interface{}{"category": "other"}},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set(HeaderPaymentSignature, changed)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("changed info status = %d", w.Code)
	}
	if len(*order) != 0 {
		t.Errorf("facilitator was called for a changed extension: %v", *order)
	}

	omitted := makeV2PaymentHeader(t)
	req = httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	req.Header.Set(HeaderPaymentSignature, omitted)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("omitted extension status = %d body %s", w.Code, w.Body.String())
	}
}

func paymentWithID(t *testing.T, id string) string {
	t.Helper()
	return paymentWithExtensions(t, map[string]interface{}{
		ExtensionPaymentIdentifier: map[string]interface{}{
			"info": map[string]interface{}{"id": id},
		},
	})
}

func paymentWithFlow(t *testing.T, flow string) string {
	t.Helper()
	extra := map[string]interface{}{"name": "USD Coin", "version": "2", "paymentFlow": flow}
	payload := PaymentPayload{
		X402Version: 2,
		Accepted: PaymentRequirements{
			Scheme:            "exact",
			Network:           "eip155:84532",
			Amount:            "1000000",
			Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			PayTo:             "0xRecipient",
			MaxTimeoutSeconds: 300,
			Extra:             extra,
		},
		Payload: map[string]interface{}{"signature": "0xsig"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func paymentWithExtensions(t *testing.T, extensions map[string]interface{}) string {
	t.Helper()
	payload := PaymentPayload{
		X402Version: 2,
		Accepted: PaymentRequirements{
			Scheme:            "exact",
			Network:           "eip155:84532",
			Amount:            "1000000",
			Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			PayTo:             "0xRecipient",
			MaxTimeoutSeconds: 300,
			Extra:             map[string]interface{}{"name": "USD Coin", "version": "2"},
		},
		Payload:    map[string]interface{}{"signature": "0xsig"},
		Extensions: extensions,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func decodeRequired(t *testing.T, w *httptest.ResponseRecorder) PaymentRequiredResponse {
	t.Helper()
	encoded := w.Header().Get(HeaderPaymentRequired)
	if encoded == "" {
		t.Fatal("missing PAYMENT-REQUIRED")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PaymentRequiredResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func decodeReceipt(t *testing.T, encoded string) PaymentResponse {
	t.Helper()
	if encoded == "" {
		t.Fatal("missing payment response header")
	}
	got, err := DecodePaymentResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return *got
}
