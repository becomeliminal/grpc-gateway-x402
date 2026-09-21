package evm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
)

const (
	testNetwork   = "eip155:84532"
	testAsset     = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	testPayTo     = "0x1111111111111111111111111111111111111111"
	testPayer     = "0x2222222222222222222222222222222222222222"
	testAttacker  = "0x3333333333333333333333333333333333333333"
	testAmount    = "1000000"
	testNonce     = "0x0000000000000000000000000000000000000000000000000000000000000001"
	testSignature = "0xsig"
)

// fakeFacilitator serves the facilitator's V2 endpoints and records the body of
// every verify and settle call it receives.
type fakeFacilitator struct {
	server   *httptest.Server
	verifies []map[string]interface{}
	settles  []map[string]interface{}
}

func newFakeFacilitator(t *testing.T) *fakeFacilitator {
	t.Helper()
	f := &fakeFacilitator{}
	record := func(t *testing.T, r *http.Request) map[string]interface{} {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading facilitator request: %v", err)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decoding facilitator request: %v", err)
		}
		return decoded
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/x402/supported":
			json.NewEncoder(w).Encode(FacilitatorSupportedResponse{
				Kinds: []SupportedKind{{Scheme: "exact", Network: testNetwork}},
			})
		case "/v2/x402/verify":
			f.verifies = append(f.verifies, record(t, r))
			json.NewEncoder(w).Encode(FacilitatorVerifyResponse{IsValid: true, Payer: testPayer})
		case "/v2/x402/settle":
			f.settles = append(f.settles, record(t, r))
			json.NewEncoder(w).Encode(FacilitatorSettleResponse{
				Success: true, Payer: testPayer, Transaction: "0xtx", Network: testNetwork,
			})
		default:
			t.Errorf("unexpected facilitator call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func newTestVerifier(t *testing.T, f *fakeFacilitator) *EVMVerifier {
	t.Helper()
	v, err := NewEVMVerifier(f.server.URL)
	if err != nil {
		t.Fatalf("creating verifier: %v", err)
	}
	return v
}

func testRequirements() *x402.PaymentRequirements {
	return &x402.PaymentRequirements{
		Scheme:  "exact",
		Network: testNetwork,
		Amount:  testAmount,
		Asset:   testAsset,
		PayTo:   testPayTo,
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
			"authorization": map[string]interface{}{
				"from":        testPayer,
				"to":          to,
				"value":       value,
				"validAfter":  float64(0),
				"validBefore": float64(9999999999),
				"nonce":       testNonce,
			},
		},
	}
}

func TestEVMVerifier_Verify_MatchingAuthorizationReachesFacilitator(t *testing.T) {
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
}

func TestEVMVerifier_RejectsAuthorizationNotBoundToRequirements(t *testing.T) {
	tests := []struct {
		name       string
		to         string
		value      string
		wantReason string
	}{
		{name: "pays someone else", to: testAttacker, value: testAmount, wantReason: ReasonRecipientMismatch},
		{name: "pays less than required", to: testPayTo, value: "1", wantReason: ReasonAuthorizationValue},
		// The exact scheme binds the value exactly.
		{name: "pays more than required", to: testPayTo, value: "1000001", wantReason: ReasonAuthorizationValue},
		{name: "value is not a number", to: testPayTo, value: "lots", wantReason: ReasonAuthorizationValue},
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

func TestEVMVerifier_RecipientComparisonIgnoresAddressCase(t *testing.T) {
	// GIVEN a payment to the required recipient, written in another letter case
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)
	requirements := testRequirements()
	requirements.PayTo = "0xAbCdEf0000000000000000000000000000000001"

	// WHEN it is verified
	result, err := v.Verify(context.Background(), testPayload("0xabcdef0000000000000000000000000000000001", testAmount), requirements)

	// THEN the recipient matches
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !result.Valid {
		t.Errorf("result = %+v, want valid", result)
	}
}

func TestEVMVerifier_RejectsIncompleteRequirements(t *testing.T) {
	tests := []struct {
		name         string
		requirements *x402.PaymentRequirements
	}{
		{name: "no requirements", requirements: nil},
		{name: "no recipient", requirements: &x402.PaymentRequirements{Scheme: "exact", Network: testNetwork, Amount: testAmount, Asset: testAsset}},
		{name: "no amount", requirements: &x402.PaymentRequirements{Scheme: "exact", Network: testNetwork, Asset: testAsset, PayTo: testPayTo}},
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

func TestEVMVerifier_ForwardsOnlyTheSchemesPayloadFields(t *testing.T) {
	// GIVEN a matching payment that also carries a field outside the exact
	// scheme's payload — one a facilitator might act on
	f := newFakeFacilitator(t)
	v := newTestVerifier(t, f)
	payload := testPayload(testPayTo, testAmount)
	payload.Payload.(map[string]interface{})["authorization"].(map[string]interface{})["tokenContract"] = testAttacker

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
		"authorization": map[string]interface{}{
			"from":        testPayer,
			"to":          testPayTo,
			"value":       testAmount,
			"validAfter":  float64(0),
			"validBefore": float64(9999999999),
			"nonce":       testNonce,
		},
	}
	for name, calls := range map[string][]map[string]interface{}{"verify": f.verifies, "settle": f.settles} {
		if len(calls) != 1 {
			t.Fatalf("facilitator %s calls = %d, want 1", name, len(calls))
		}
		forwarded := calls[0]["payload"].(map[string]interface{})["payload"]
		if !reflect.DeepEqual(forwarded, wantPayload) {
			t.Errorf("%s forwarded payload = %+v, want %+v", name, forwarded, wantPayload)
		}
	}
}
