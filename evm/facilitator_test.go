package evm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402"
)

func TestFacilitatorVerifyBodyAndPath(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("EXTENSION-RESPONSES", base64.StdEncoding.EncodeToString([]byte(`{"bazaar":{"status":"ok"}}`)))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"isValid": true,
			"payer":   "0xpayer",
		})
	}))
	defer srv.Close()

	client := NewFacilitatorClient(srv.URL)
	client.SetAuthProvider(staticAuth{})
	resp, err := client.Verify(context.Background(), &FacilitatorVerifyRequest{
		X402Version:         2,
		PaymentPayload:      map[string]interface{}{"x402Version": 2},
		PaymentRequirements: map[string]interface{}{"scheme": "exact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/verify" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if _, ok := gotBody["paymentPayload"]; !ok {
		t.Errorf("body missing paymentPayload: %v", gotBody)
	}
	if _, ok := gotBody["paymentRequirements"]; !ok {
		t.Errorf("body missing paymentRequirements: %v", gotBody)
	}
	if gotBody["x402Version"] != float64(2) {
		t.Errorf("x402Version = %v", gotBody["x402Version"])
	}
	if !resp.IsValid || resp.Payer != "0xpayer" {
		t.Errorf("response = %+v", resp)
	}
	bazaar, _ := resp.ExtensionResponses["bazaar"].(map[string]interface{})
	if bazaar["status"] != "ok" {
		t.Errorf("extension responses = %+v", resp.ExtensionResponses)
	}
}

func TestFacilitatorSignersAreArrays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/supported" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:8453"}],"extensions":[],"signers":{"eip155:8453":["0xabc","0xdef"]}}`))
	}))
	defer srv.Close()

	got, err := NewFacilitatorClient(srv.URL).GetSupported(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Signers["eip155:8453"]) != 2 || got.Signers["eip155:8453"][0] != "0xabc" {
		t.Errorf("signers = %+v", got.Signers)
	}
	if len(got.Kinds) != 1 || got.Kinds[0].X402Version != 2 {
		t.Errorf("kinds = %+v", got.Kinds)
	}
}

func TestGetSupportedRetries429(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:84532"}],"extensions":[],"signers":{}}`))
	}))
	defer srv.Close()

	client := NewFacilitatorClient(srv.URL)
	client.retryBaseDelay = time.Millisecond
	got, err := client.GetSupported(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("hits = %d", hits)
	}
	if len(got.Kinds) != 1 {
		t.Errorf("kinds = %+v", got.Kinds)
	}
}

func TestEVMVerifierPassesStringTimestamps(t *testing.T) {
	var saw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/supported":
			_, _ = w.Write([]byte(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:84532"}],"extensions":[],"signers":{"eip155:84532":["0xfac"]}}`))
		case "/verify":
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			payload := body["paymentPayload"].(map[string]interface{})
			inner := payload["payload"].(map[string]interface{})
			auth := inner["authorization"].(map[string]interface{})
			saw, _ = auth["validAfter"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"isValid": true, "payer": "0xpayer"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	verifier, err := NewEVMVerifier(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()

	result, err := verifier.Verify(context.Background(), &x402.PaymentPayload{
		X402Version: 2,
		Payload: map[string]interface{}{
			"signature": "0xsig",
			"authorization": map[string]interface{}{
				"from":        "0xpayer",
				"to":          "0xdest",
				"value":       "10000",
				"validAfter":  "1740672089",
				"validBefore": "1740672154",
				"nonce":       "0xnonce",
			},
		},
	}, &x402.PaymentRequirements{
		Scheme:  "exact",
		Network: "eip155:84532",
		Amount:  "10000",
		Asset:   "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
		PayTo:   "0xdest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.PayerAddress != "0xpayer" {
		t.Errorf("result = %+v", result)
	}
	if saw != "1740672089" {
		t.Errorf("validAfter seen by facilitator = %q", saw)
	}
	if len(verifier.SupportedKinds()) != 1 {
		t.Errorf("kinds = %+v", verifier.SupportedKinds())
	}
}

func TestSettlePendingIsAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/supported":
			_, _ = w.Write([]byte(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:84532"}],"extensions":[],"signers":{}}`))
		case "/settle":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":     false,
				"errorReason": "settlement_pending",
				"transaction": "0xpending",
				"network":     "eip155:84532",
				"payer":       "0xpayer",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	verifier, err := NewEVMVerifier(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()

	payload := &x402.PaymentPayload{
		X402Version: 2,
		Accepted:    x402.PaymentRequirements{Scheme: "exact", Network: "eip155:84532", Amount: "1", PayTo: "0xdest"},
		Payload: map[string]interface{}{
			"signature": "0xsig",
			"authorization": map[string]interface{}{
				"from": "0xpayer", "to": "0xdest", "value": "1",
				"validAfter": "0", "validBefore": "1", "nonce": "0xnonce",
			},
		},
	}
	requirements := &x402.PaymentRequirements{Scheme: "exact", Network: "eip155:84532", Amount: "1", Asset: "0xabc", PayTo: "0xdest"}
	result, err := verifier.Settle(context.Background(), payload, requirements)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ErrorReason != "settlement_pending" || result.TransactionHash != "0xpending" {
		t.Errorf("result = %+v", result)
	}
}

func TestFacilitatorDecodesAPaymentFailureBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("EXTENSION-RESPONSES", base64.StdEncoding.EncodeToString([]byte(`{"bazaar":{"status":"ok"}}`)))
		w.WriteHeader(http.StatusBadRequest)
		switch r.URL.Path {
		case "/verify":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"isValid":       false,
				"invalidReason": "invalid_signature",
				"payer":         "0xpayer",
			})
		case "/settle":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":     false,
				"errorReason": "insufficient_allowance",
				"transaction": "",
				"network":     "eip155:84532",
				"payer":       "0xpayer",
			})
		default:
			_, _ = w.Write([]byte("Bad request"))
		}
	}))
	defer srv.Close()

	client := NewFacilitatorClient(srv.URL)
	verified, err := client.Verify(context.Background(), &FacilitatorVerifyRequest{X402Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if verified.IsValid || verified.InvalidReason != "invalid_signature" || verified.Payer != "0xpayer" {
		t.Errorf("verify = %+v", verified)
	}
	if verified.ExtensionResponses["bazaar"] == nil {
		t.Errorf("extension responses = %+v", verified.ExtensionResponses)
	}

	settled, err := client.Settle(context.Background(), &FacilitatorSettleRequest{X402Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if settled.Success || settled.ErrorReason != "insufficient_allowance" || settled.Transaction != "" || settled.Network != "eip155:84532" {
		t.Errorf("settle = %+v", settled)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Bad request"))
	}))
	defer plain.Close()
	client = NewFacilitatorClient(plain.URL)
	if _, err := client.Verify(context.Background(), &FacilitatorVerifyRequest{X402Version: 2}); err == nil {
		t.Fatal("expected transport error for a plain 400")
	}

	okFalse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"isValid":       false,
			"invalidReason": "insufficient_funds",
			"payer":         "0xpayer",
		})
	}))
	defer okFalse.Close()
	client = NewFacilitatorClient(okFalse.URL)
	verified, err = client.Verify(context.Background(), &FacilitatorVerifyRequest{X402Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if verified.IsValid || verified.InvalidReason != "insufficient_funds" {
		t.Errorf("verify = %+v", verified)
	}
}

func TestFacilitatorRejectsAnOversizedBody(t *testing.T) {
	calls := []struct {
		name   string
		status int
		run    func(*FacilitatorClient) error
	}{
		{name: "verify", status: http.StatusOK, run: func(c *FacilitatorClient) error {
			_, err := c.Verify(context.Background(), &FacilitatorVerifyRequest{X402Version: 2})
			return err
		}},
		{name: "settle", status: http.StatusInternalServerError, run: func(c *FacilitatorClient) error {
			_, err := c.Settle(context.Background(), &FacilitatorSettleRequest{X402Version: 2})
			return err
		}},
		{name: "supported", status: http.StatusTooManyRequests, run: func(c *FacilitatorClient) error {
			_, err := c.GetSupported(context.Background())
			return err
		}},
	}
	for _, tt := range calls {
		t.Run(tt.name, func(t *testing.T) {
			var attempts int
			body := &closeBody{Reader: io.LimitReader(zeroReader{}, maxResponseBytes+1)}
			client := NewFacilitatorClient("https://facilitator.example")
			client.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return &http.Response{
					StatusCode: tt.status,
					Header:     make(http.Header),
					Body:       body,
				}, nil
			})})
			if err := tt.run(client); err == nil {
				t.Fatal("expected an oversized body to fail")
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
			if attempts != 1 {
				t.Errorf("attempts = %d", attempts)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

type closeBody struct {
	io.Reader
	closed bool
}

func (b *closeBody) Close() error {
	b.closed = true
	return nil
}

type staticAuth struct{}

func (staticAuth) GetAuthHeaders(ctx context.Context) (AuthHeaders, error) {
	return AuthHeaders{Verify: map[string]string{"Authorization": "Bearer test"}}, nil
}
