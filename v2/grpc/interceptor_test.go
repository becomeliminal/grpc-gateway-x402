package grpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const paidMethod = "/test.v1.TestService/Paid"

// recordingVerifier records the requirements each payment is verified and
// settled against. When forbidden is set, reaching it fails the test.
type recordingVerifier struct {
	t         *testing.T
	forbidden bool
	verified  *x402.PaymentRequirements
	settled   *x402.PaymentRequirements
}

func (v *recordingVerifier) Verify(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.VerificationResult, error) {
	if v.forbidden {
		v.t.Errorf("Verify called with requirements %+v", requirements)
	}
	v.verified = requirements
	return &x402.VerificationResult{Valid: true, PayerAddress: "0xPayer", Amount: requirements.Amount}, nil
}

func (v *recordingVerifier) Settle(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.SettlementResult, error) {
	if v.forbidden {
		v.t.Errorf("Settle called with requirements %+v", requirements)
	}
	v.settled = requirements
	return &x402.SettlementResult{TransactionHash: "0xtx", Network: requirements.Network}, nil
}

func (v *recordingVerifier) SupportedKinds() []x402.SupportedKind { return nil }

// twoTokenRule accepts USDC on Base Sepolia or on Arbitrum, each paid to its
// own recipient.
func twoTokenRule() x402.PricingRule {
	return x402.PricingRule{
		AcceptedTokens: []x402.TokenRequirement{
			{Network: "eip155:84532", Symbol: "USDC", AssetContract: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", Recipient: "0xRecipientA", Amount: "1000000", TokenName: "USDC", TokenVersion: "2"},
			{Network: "eip155:42161", Symbol: "USDC", AssetContract: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", Recipient: "0xRecipientB", Amount: "2000000", TokenName: "USD Coin", TokenVersion: "2"},
		},
	}
}

func twoTokenConfig(v *recordingVerifier) x402.Config {
	return x402.Config{
		Verifier:      v,
		MethodPricing: map[string]x402.PricingRule{paidMethod: twoTokenRule()},
	}
}

// unacceptedV2 is a V2 payment in a token the rule does not accept.
func unacceptedV2(t *testing.T) metadata.MD {
	t.Helper()
	return v2Metadata(t, x402.PaymentRequirements{
		Scheme: "exact", Network: "eip155:84532", Amount: "1",
		Asset: "0x0000000000000000000000000000000000000bad", PayTo: "0xPayer",
	})
}

// secondTokenV2 is a V2 payment choosing the rule's second token, claiming a
// lower amount and another recipient than the rule sets.
func secondTokenV2(t *testing.T) metadata.MD {
	t.Helper()
	return v2Metadata(t, x402.PaymentRequirements{
		Scheme: "exact", Network: "eip155:42161", Amount: "1",
		Asset: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", PayTo: "0xPayer",
	})
}

func v2Metadata(t *testing.T, accepted x402.PaymentRequirements) metadata.MD {
	t.Helper()
	encoded, err := EncodePaymentPayload(&x402.PaymentPayload{
		X402Version: 2,
		Accepted:    accepted,
		Payload:     map[string]interface{}{"signature": "0xsig"},
	})
	if err != nil {
		t.Fatalf("encoding payment: %v", err)
	}
	return metadata.Pairs(MetadataKeyPaymentSignature, encoded)
}

func v1Metadata(t *testing.T) metadata.MD {
	t.Helper()
	legacyJSON, err := json.Marshal(x402.LegacyPayment{
		X402Version: 1, Scheme: "exact", Network: "base-sepolia",
		Payload: map[string]interface{}{"signature": "0xsig"},
	})
	if err != nil {
		t.Fatalf("encoding legacy payment: %v", err)
	}
	return metadata.Pairs(MetadataKeyLegacyPayment, base64.StdEncoding.EncodeToString(legacyJSON))
}

// secondTokenRequirements are the rule's own requirements for its second token.
var secondTokenRequirements = &x402.PaymentRequirements{
	Scheme: "exact", Network: "eip155:42161", Amount: "2000000",
	Asset: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", PayTo: "0xRecipientB",
}

// firstTokenRequirements are the rule's own requirements for its first token.
var firstTokenRequirements = &x402.PaymentRequirements{
	Scheme: "exact", Network: "eip155:84532", Amount: "1000000",
	Asset: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", PayTo: "0xRecipientA",
}

// requirePaymentRequired asserts err is the payment-required status carrying
// the method's requirements.
func requirePaymentRequired(t *testing.T, err error) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("error = %v, want a ResourceExhausted payment-required status", err)
	}
	decoded, decErr := DecodePaymentRequirements(st.Message())
	if decErr != nil {
		t.Fatalf("status message is not encoded requirements: %v", decErr)
	}
	rule := twoTokenRule()
	want := BuildPaymentRequirements(&rule, paidMethod, nil)
	if !reflect.DeepEqual(decoded.Accepts, want) {
		t.Errorf("requirements = %+v, want %+v", decoded.Accepts, want)
	}
}

func TestUnaryInterceptor_V2UnacceptedToken_PaymentRequiredWithoutVerifying(t *testing.T) {
	// GIVEN a method accepting two tokens, and a verifier that must not run
	v := &recordingVerifier{t: t, forbidden: true}
	interceptor := UnaryServerInterceptor(twoTokenConfig(v))

	// WHEN a V2 payment arrives in a token the method does not accept
	ctx := metadata.NewIncomingContext(context.Background(), unacceptedV2(t))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: paidMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			t.Error("handler reached for a payment in an unaccepted token")
			return nil, nil
		})

	// THEN it is refused with the method's payment requirements
	requirePaymentRequired(t, err)
}

func TestUnaryInterceptor_V2AcceptedToken_UsesTheRulesRequirements(t *testing.T) {
	// GIVEN a method accepting two tokens
	v := &recordingVerifier{t: t}
	interceptor := UnaryServerInterceptor(twoTokenConfig(v))

	// WHEN a V2 payment arrives in the second token
	var paid *x402.PaymentContext
	ctx := metadata.NewIncomingContext(context.Background(), secondTokenV2(t))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: paidMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			paid, _ = GetPaymentFromContext(ctx)
			return "ok", nil
		})

	// THEN it is verified and settled against the rule's own requirements for
	// that token, and the handler sees the payment
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if !reflect.DeepEqual(v.verified, secondTokenRequirements) || !reflect.DeepEqual(v.settled, secondTokenRequirements) {
		t.Errorf("verified %+v, settled %+v, want %+v", v.verified, v.settled, secondTokenRequirements)
	}
	if paid == nil || paid.Network != "eip155:42161" || paid.TokenSymbol != "USDC" || !paid.Verified {
		t.Errorf("payment context = %+v", paid)
	}
}

func TestUnaryInterceptor_V1Payment_PaysTheFirstAcceptedToken(t *testing.T) {
	// GIVEN a method accepting two tokens
	v := &recordingVerifier{t: t}
	interceptor := UnaryServerInterceptor(twoTokenConfig(v))

	// WHEN a V1 payment arrives — V1 names no token
	ctx := metadata.NewIncomingContext(context.Background(), v1Metadata(t))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: paidMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) { return "ok", nil })

	// THEN it is verified against the first accepted token
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if !reflect.DeepEqual(v.verified, firstTokenRequirements) {
		t.Errorf("verified %+v, want %+v", v.verified, firstTokenRequirements)
	}
}

// fakeServerStream is a ServerStream carrying only a context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }
func (s *fakeServerStream) SetTrailer(metadata.MD)   {}

func TestStreamInterceptor_V2UnacceptedToken_PaymentRequiredWithoutVerifying(t *testing.T) {
	// GIVEN a streaming method accepting two tokens, and a verifier that must not run
	v := &recordingVerifier{t: t, forbidden: true}
	interceptor := StreamServerInterceptor(twoTokenConfig(v))

	// WHEN a V2 payment arrives in a token the method does not accept
	ss := &fakeServerStream{ctx: metadata.NewIncomingContext(context.Background(), unacceptedV2(t))}
	err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: paidMethod},
		func(srv interface{}, stream grpc.ServerStream) error {
			t.Error("handler reached for a payment in an unaccepted token")
			return nil
		})

	// THEN it is refused with the method's payment requirements
	requirePaymentRequired(t, err)
}

func TestStreamInterceptor_V2AcceptedToken_UsesTheRulesRequirements(t *testing.T) {
	// GIVEN a streaming method accepting two tokens
	v := &recordingVerifier{t: t}
	interceptor := StreamServerInterceptor(twoTokenConfig(v))

	// WHEN a V2 payment arrives in the second token
	ss := &fakeServerStream{ctx: metadata.NewIncomingContext(context.Background(), secondTokenV2(t))}
	err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: paidMethod},
		func(srv interface{}, stream grpc.ServerStream) error { return nil })

	// THEN it is verified and settled against the rule's own requirements for that token
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if !reflect.DeepEqual(v.verified, secondTokenRequirements) || !reflect.DeepEqual(v.settled, secondTokenRequirements) {
		t.Errorf("verified %+v, settled %+v, want %+v", v.verified, v.settled, secondTokenRequirements)
	}
}
