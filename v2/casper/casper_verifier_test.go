package casper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
)

const (
	testNetwork   = NetworkTestnet
	testAsset     = "9f1f7a2b6c1f4d0a8e3b5c7d9e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4"
	testPayTo     = "0011111111111111111111111111111111111111111111111111111111111111"
	testPayer     = "0022222222222222222222222222222222222222222222222222222222222222"
	testAttacker  = "0033333333333333333333333333333333333333333333333333333333333333"
	testAmount    = "7500000000"
	testNonce     = "0000000000000000000000000000000000000000000000000000000000000001"
	testSignature = "02sig"
	testPublicKey = "0188a3a0d3e0e0d8f2b7f9d3a1c5e7b9d1f3a5c7e9b1d3f5a7c9e1b3d5f7a9c1e3"
)

// fakeFacilitator serves the Casper facilitator's endpoints and records the
// body of every verify and settle call it receives.
type fakeFacilitator struct {
	server   *httptest.Server
	verifies []map[string]interface{}
	settles  []map[string]interface{}

	// kinds, when set, replaces what GET /supported reports.
	kinds []SupportedKind
	// verifyResponse and settleResponse, when set, replace the successful
	// defaults.
	verifyResponse *FacilitatorVerifyResponse
	settleResponse *FacilitatorSettleResponse
	// status, when non-zero, is returned for every verify and settle call
	// with body as the payload instead of the usual JSON.
	status int
	body   string
	// delay is how long the facilitator takes to answer.
	delay time.Duration
}

func newFakeFacilitator(t *testing.T) *fakeFacilitator {
	t.Helper()
	f := &fakeFacilitator{}
	record := func(t *testing.T, r *http.Request) map[string]interface{} {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading facilitator request: %v", err)
			return nil
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("decoding facilitator request: %v", err)
			return nil
		}
		return decoded
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.delay > 0 && r.URL.Path != "/supported" {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		switch r.URL.Path {
		case "/supported":
			kinds := f.kinds
			if kinds == nil {
				kinds = []SupportedKind{{X402Version: 2, Scheme: Scheme, Network: testNetwork}}
			}
			json.NewEncoder(w).Encode(FacilitatorSupportedResponse{Kinds: kinds})
		case "/verify":
			f.verifies = append(f.verifies, record(t, r))
			if f.status != 0 {
				http.Error(w, f.body, f.status)
				return
			}
			resp := f.verifyResponse
			if resp == nil {
				resp = &FacilitatorVerifyResponse{IsValid: true, Payer: testPayer}
			}
			json.NewEncoder(w).Encode(resp)
		case "/settle":
			f.settles = append(f.settles, record(t, r))
			if f.status != 0 {
				http.Error(w, f.body, f.status)
				return
			}
			resp := f.settleResponse
			if resp == nil {
				resp = &FacilitatorSettleResponse{
					Success: true, Payer: testPayer, Transaction: "abcdef", Network: testNetwork,
				}
			}
			json.NewEncoder(w).Encode(resp)
		default:
			t.Errorf("unexpected facilitator call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func newTestVerifier(t *testing.T, f *fakeFacilitator) *CasperVerifier {
	t.Helper()
	v, err := NewCasperVerifier(f.server.URL)
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	return v
}

func testRequirements() *x402.PaymentRequirements {
	return &x402.PaymentRequirements{
		Scheme:  Scheme,
		Network: testNetwork,
		Amount:  testAmount,
		Asset:   testAsset,
		PayTo:   testPayTo,
		Extra:   map[string]interface{}{"name": "Wrapped CSPR", "version": "1", "decimals": "9"},
	}
}

// testPayload is a payment as a client sends it: a raw JSON map, as decoded
// from the payment header.
func testPayload(to, value string) *x402.PaymentPayload {
	return &x402.PaymentPayload{
		X402Version: 2,
		Accepted:    *testRequirements(),
		Payload: map[string]interface{}{
			"signature": testSignature,
			"publicKey": testPublicKey,
			"authorization": map[string]interface{}{
				"from":        testPayer,
				"to":          to,
				"value":       value,
				"validAfter":  "0",
				"validBefore": "9999999999",
				"nonce":       testNonce,
			},
		},
	}
}

func TestCasperVerifier_Verify_MatchingAuthorizationReachesFacilitator(t *testing.T) {
	// GIVEN a payment whose signed authorization pays the required amount to
	// the required recipient
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)

	// WHEN it is verified
	result, err := v.Verify(context.Background(), testPayload(testPayTo, testAmount), testRequirements())

	// THEN it is valid
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	want := &x402.VerificationResult{Valid: true, PayerAddress: testPayer, Amount: testAmount}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %+v, want %+v", result, want)
	}
	// AND the facilitator received the payment and the requirements
	if len(f.verifies) != 1 {
		t.Fatalf("facilitator verify calls = %d, want 1", len(f.verifies))
	}
	if _, ok := f.verifies[0]["paymentRequirements"]; !ok {
		t.Errorf("facilitator verify call = %+v, want it to carry paymentRequirements", f.verifies[0])
	}
}

func TestCasperVerifier_Verify_ReportsFacilitatorRejection(t *testing.T) {
	// GIVEN a facilitator that rejects the payment, for example because the
	// signature does not verify under the payer's key
	f := newFakeFacilitator(t)
	f.verifyResponse = &FacilitatorVerifyResponse{
		IsValid:        false,
		InvalidReason:  "invalid_exact_casper_invalid_signature",
		InvalidMessage: "signature does not match public key",
	}
	v := newTestVerifier(t, f)

	// WHEN a well-formed, correctly bound payment is verified
	result, err := v.Verify(context.Background(), testPayload(testPayTo, testAmount), testRequirements())

	// THEN the facilitator's reason is reported, and it is not an error
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if result.Valid {
		t.Errorf("result = %+v, want invalid", result)
	}
	if result.Reason != "invalid_exact_casper_invalid_signature" {
		t.Errorf("reason = %q, want %q", result.Reason, "invalid_exact_casper_invalid_signature")
	}
}

func TestCasperVerifier_Settle_SuccessfulSettlementReturnsDeployHash(t *testing.T) {
	// GIVEN a facilitator that settles the payment on Casper
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)

	// WHEN a correctly bound payment is settled
	result, err := v.Settle(context.Background(), testPayload(testPayTo, testAmount), testRequirements())

	// THEN the deploy hash and the parties come back
	if err != nil {
		t.Fatalf("Settle error: %v", err)
	}
	if result.TransactionHash != "abcdef" {
		t.Errorf("transaction hash = %q, want %q", result.TransactionHash, "abcdef")
	}
	if result.Status != "success" {
		t.Errorf("status = %q, want %q", result.Status, "success")
	}
	if result.PayerAddress != testPayer || result.RecipientAddress != testPayTo {
		t.Errorf("payer/recipient = %q/%q, want %q/%q", result.PayerAddress, result.RecipientAddress, testPayer, testPayTo)
	}
	if result.Amount != testAmount {
		t.Errorf("amount = %q, want %q", result.Amount, testAmount)
	}
	if result.Network != testNetwork {
		t.Errorf("network = %q, want %q", result.Network, testNetwork)
	}
	if len(f.settles) != 1 {
		t.Errorf("facilitator settle calls = %d, want 1", len(f.settles))
	}
}

func TestCasperVerifier_Settle_ReportsFacilitatorFailure(t *testing.T) {
	// GIVEN a facilitator whose settlement deploy fails on-chain
	f := newFakeFacilitator(t)
	f.settleResponse = &FacilitatorSettleResponse{
		Success:      false,
		ErrorReason:  "invalid_exact_casper_put_deploy_failed",
		ErrorMessage: "node rejected the deploy",
		Network:      testNetwork,
	}
	v := newTestVerifier(t, f)

	// WHEN a correctly bound payment is settled
	result, err := v.Settle(context.Background(), testPayload(testPayTo, testAmount), testRequirements())

	// THEN settlement fails, naming the facilitator's reason
	if err == nil {
		t.Fatalf("Settle succeeded with %+v, want an error", result)
	}
	for _, want := range []string{"invalid_exact_casper_put_deploy_failed", "node rejected the deploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestCasperVerifier_RejectsAuthorizationNotBoundToRequirements(t *testing.T) {
	tests := []struct {
		name       string
		to         string
		value      string
		wantReason string
	}{
		{name: "pays someone else", to: testAttacker, value: testAmount, wantReason: ReasonPayToMismatch},
		{name: "pays less than required", to: testPayTo, value: "1", wantReason: ReasonAmountMismatch},
		// The exact scheme binds the value exactly.
		{name: "pays more than required", to: testPayTo, value: "7500000001", wantReason: ReasonAmountMismatch},
		{name: "value is not a number", to: testPayTo, value: "lots", wantReason: ReasonAmountMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// GIVEN a validly shaped payment whose signed authorization does
			// not match the requirements
			f := newFakeFacilitator(t)
			v := newTestVerifier(t, f)
			payload := testPayload(tt.to, tt.value)

			// WHEN it is verified
			result, err := v.Verify(context.Background(), payload, testRequirements())

			// THEN it is invalid for that reason
			if err != nil {
				t.Fatalf("Verify error: %v", err)
			}
			want := &x402.VerificationResult{Valid: false, Reason: tt.wantReason}
			if !reflect.DeepEqual(result, want) {
				t.Errorf("result = %+v, want %+v", result, want)
			}

			// WHEN it is settled anyway
			settlement, err := v.Settle(context.Background(), payload, testRequirements())

			// THEN settlement is refused
			if err == nil {
				t.Errorf("Settle succeeded with %+v, want an error", settlement)
			}
			// AND the facilitator was never asked to verify or settle it
			if len(f.verifies) != 0 || len(f.settles) != 0 {
				t.Errorf("facilitator calls: verify=%d settle=%d, want 0 and 0", len(f.verifies), len(f.settles))
			}
		})
	}
}

func TestCasperVerifier_RejectsNetworkTheFacilitatorDoesNotSettle(t *testing.T) {
	// GIVEN a facilitator that settles only Casper testnet
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)
	requirements := testRequirements()
	requirements.Network = NetworkMainnet

	// WHEN a payment for mainnet is verified
	result, err := v.Verify(context.Background(), testPayload(testPayTo, testAmount), requirements)

	// THEN it is rejected as an unsupported network
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	want := &x402.VerificationResult{Valid: false, Reason: ReasonUnsupportedNetwork}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %+v, want %+v", result, want)
	}
	// AND it never reached the facilitator
	if len(f.verifies) != 0 {
		t.Errorf("facilitator verify calls = %d, want 0", len(f.verifies))
	}
}

func TestCasperVerifier_RecipientComparisonIgnoresAccountHashCase(t *testing.T) {
	// GIVEN a payment to the required recipient, written in another letter case
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)
	requirements := testRequirements()
	requirements.PayTo = "00ABCDEF00000000000000000000000000000000000000000000000000000001"

	// WHEN it is verified
	result, err := v.Verify(context.Background(), testPayload("00abcdef00000000000000000000000000000000000000000000000000000001", testAmount), requirements)

	// THEN the recipient matches
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !result.Valid {
		t.Errorf("result = %+v, want valid", result)
	}
}

func TestCasperVerifier_RejectsMalformedPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload interface{}
	}{
		{name: "not a Casper payload at all", payload: "PAYMENT-SIGNATURE"},
		{name: "no signature", payload: map[string]interface{}{
			"publicKey":     testPublicKey,
			"authorization": map[string]interface{}{"from": testPayer, "to": testPayTo, "value": testAmount, "nonce": testNonce},
		}},
		// Casper cannot recover the payer's key from the signature.
		{name: "no public key", payload: map[string]interface{}{
			"signature":     testSignature,
			"authorization": map[string]interface{}{"from": testPayer, "to": testPayTo, "value": testAmount, "nonce": testNonce},
		}},
		{name: "no authorization", payload: map[string]interface{}{
			"signature": testSignature, "publicKey": testPublicKey,
		}},
		{name: "authorization missing fields", payload: map[string]interface{}{
			"signature": testSignature, "publicKey": testPublicKey,
			"authorization": map[string]interface{}{"from": testPayer, "to": testPayTo},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// GIVEN a payment the exact Casper scheme cannot read
			f := newFakeFacilitator(t)
			v := newTestVerifier(t, f)
			payload := testPayload(testPayTo, testAmount)
			payload.Payload = tt.payload

			// WHEN it is verified
			result, err := v.Verify(context.Background(), payload, testRequirements())

			// THEN it is invalid, and never reaches the facilitator
			if err != nil {
				t.Fatalf("Verify error: %v", err)
			}
			if result.Valid {
				t.Errorf("result = %+v, want invalid", result)
			}
			// AND settling it is refused
			if _, err := v.Settle(context.Background(), payload, testRequirements()); err == nil {
				t.Errorf("Settle succeeded, want an error")
			}
			if len(f.verifies) != 0 || len(f.settles) != 0 {
				t.Errorf("facilitator calls: verify=%d settle=%d, want 0 and 0", len(f.verifies), len(f.settles))
			}
		})
	}
}

func TestCasperVerifier_RejectsIncompleteRequirements(t *testing.T) {
	tests := []struct {
		name         string
		requirements *x402.PaymentRequirements
	}{
		{name: "no requirements", requirements: nil},
		{name: "no recipient", requirements: &x402.PaymentRequirements{Scheme: Scheme, Network: testNetwork, Amount: testAmount, Asset: testAsset}},
		{name: "no amount", requirements: &x402.PaymentRequirements{Scheme: Scheme, Network: testNetwork, Asset: testAsset, PayTo: testPayTo}},
		{name: "amount is not a number", requirements: &x402.PaymentRequirements{Scheme: Scheme, Network: testNetwork, Asset: testAsset, PayTo: testPayTo, Amount: "7.5 CSPR"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// GIVEN requirements that do not say what must be paid
			f := newFakeFacilitator(t)
			v := newTestVerifier(t, f)

			// WHEN a payment is verified against them
			result, err := v.Verify(context.Background(), testPayload(testPayTo, testAmount), tt.requirements)

			// THEN it is never treated as valid, and never reaches the facilitator
			if err == nil && result.Valid {
				t.Errorf("result = %+v, want invalid or an error", result)
			}
			if len(f.verifies) != 0 {
				t.Errorf("facilitator verify calls = %d, want 0", len(f.verifies))
			}
		})
	}
}

func TestCasperVerifier_ForwardsOnlyTheSchemesPayloadFields(t *testing.T) {
	// GIVEN a matching payment that also carries a field outside the exact
	// scheme's payload — one a facilitator might act on
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)
	payload := testPayload(testPayTo, testAmount)
	payload.Payload.(map[string]interface{})["authorization"].(map[string]interface{})["contractPackageHash"] = testAttacker

	// WHEN it is verified and settled
	if _, err := v.Verify(context.Background(), payload, testRequirements()); err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if _, err := v.Settle(context.Background(), payload, testRequirements()); err != nil {
		t.Fatalf("Settle error: %v", err)
	}

	// THEN the facilitator receives exactly the scheme's fields, and nothing else
	wantPayload := map[string]interface{}{
		"signature": testSignature,
		"publicKey": testPublicKey,
		"authorization": map[string]interface{}{
			"from":        testPayer,
			"to":          testPayTo,
			"value":       testAmount,
			"validAfter":  "0",
			"validBefore": "9999999999",
			"nonce":       testNonce,
		},
	}
	for name, calls := range map[string][]map[string]interface{}{"verify": f.verifies, "settle": f.settles} {
		if len(calls) != 1 {
			t.Fatalf("facilitator %s calls = %d, want 1", name, len(calls))
		}
		sent := calls[0]["paymentPayload"].(map[string]interface{})
		if !reflect.DeepEqual(sent["payload"], wantPayload) {
			t.Errorf("%s forwarded payload = %+v, want %+v", name, sent["payload"], wantPayload)
		}
		// AND the scheme and network it must settle on
		if sent["scheme"] != Scheme || sent["network"] != testNetwork {
			t.Errorf("%s sent scheme/network = %v/%v, want %q/%q", name, sent["scheme"], sent["network"], Scheme, testNetwork)
		}
	}
}

func TestCasperVerifier_SupportedKindsComeFromTheFacilitator(t *testing.T) {
	// GIVEN a facilitator that settles both Casper networks
	f := newFakeFacilitator(t)
	f.kinds = []SupportedKind{
		{X402Version: 2, Scheme: Scheme, Network: NetworkMainnet, Extra: map[string]interface{}{"decimals": float64(9)}},
		{X402Version: 2, Scheme: Scheme, Network: NetworkTestnet},
	}
	v := newTestVerifier(t, f)

	// WHEN its supported kinds are asked for
	kinds := v.SupportedKinds()

	// THEN both Casper networks are advertised for the exact scheme
	want := []x402.SupportedKind{
		{Scheme: Scheme, Network: NetworkMainnet},
		{Scheme: Scheme, Network: NetworkTestnet},
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %+v, want %+v", kinds, want)
	}
}

func TestCasperVerifier_SurfacesFacilitatorHTTPErrors(t *testing.T) {
	// GIVEN a facilitator that is down
	f := newFakeFacilitator(t)
	f.status = http.StatusInternalServerError
	f.body = "upstream node unavailable"
	v := newTestVerifier(t, f)

	// WHEN a payment is verified
	_, err := v.Verify(context.Background(), testPayload(testPayTo, testAmount), testRequirements())

	// THEN the failure is an error, not an invalid payment: a broken
	// facilitator must not be read as a payer's fault
	var facErr *FacilitatorError
	if !errors.As(err, &facErr) {
		t.Fatalf("Verify error = %v, want a *FacilitatorError", err)
	}
	if facErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", facErr.StatusCode, http.StatusInternalServerError)
	}

	// AND settling gets the same treatment
	if _, err := v.Settle(context.Background(), testPayload(testPayTo, testAmount), testRequirements()); !errors.As(err, &facErr) {
		t.Errorf("Settle error = %v, want a *FacilitatorError", err)
	}
}

func TestCasperVerifier_SurfacesMalformedFacilitatorResponses(t *testing.T) {
	// GIVEN a facilitator that answers 200 with something that is not JSON
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/supported":
			json.NewEncoder(w).Encode(FacilitatorSupportedResponse{
				Kinds: []SupportedKind{{Scheme: Scheme, Network: testNetwork}},
			})
		default:
			w.Write([]byte("<html>gateway</html>"))
		}
	}))
	t.Cleanup(server.Close)
	v, err := NewCasperVerifier(server.URL)
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}

	// WHEN a payment is verified
	_, err = v.Verify(context.Background(), testPayload(testPayTo, testAmount), testRequirements())

	// THEN the undecodable response is an error
	if err == nil {
		t.Fatalf("Verify succeeded, want a decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, want it to mention decoding", err)
	}
}

func TestCasperVerifier_HonorsContextCancellation(t *testing.T) {
	// GIVEN a facilitator that answers slower than the caller will wait
	f := newFakeFacilitator(t)
	f.delay = 2 * time.Second
	v := newTestVerifier(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// WHEN a payment is verified
	start := time.Now()
	_, err := v.Verify(ctx, testPayload(testPayTo, testAmount), testRequirements())

	// THEN the call returns the context's error rather than blocking
	if err == nil {
		t.Fatalf("Verify succeeded, want a context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Verify took %v, want it to give up with the context", elapsed)
	}
}

func TestNewCasperVerifier_FailsWhenTheFacilitatorIsUnreachable(t *testing.T) {
	// GIVEN a facilitator that cannot report what it supports
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "authorization is not provided", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	// WHEN a verifier is created against it
	v, err := NewCasperVerifier(server.URL)

	// THEN construction fails rather than returning a verifier that
	// advertises nothing
	if err == nil {
		t.Fatalf("NewCasperVerifier succeeded with %+v, want an error", v)
	}
	var facErr *FacilitatorError
	if !errors.As(err, &facErr) || facErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("error = %v, want a *FacilitatorError with status 401", err)
	}
}
