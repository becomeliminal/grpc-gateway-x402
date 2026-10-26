package grpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const bindingMethod = "/test.v1.TestService/Paid"

// bindingVerifier records the requirements a payment is checked against.
// forbidden fails the test if Verify or Settle runs.
type bindingVerifier struct {
	t         *testing.T
	forbidden bool
	verified  *x402.PaymentRequirements
	settled   *x402.PaymentRequirements
}

func (v *bindingVerifier) Verify(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.VerificationResult, error) {
	if v.forbidden {
		v.t.Errorf("Verify called with requirements %+v", requirements)
	}
	copied := *requirements
	v.verified = &copied
	return &x402.VerificationResult{Valid: true, PayerAddress: "0xPayer", Amount: requirements.Amount}, nil
}

func (v *bindingVerifier) Settle(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.SettlementResult, error) {
	if v.forbidden {
		v.t.Errorf("Settle called with requirements %+v", requirements)
	}
	copied := *requirements
	v.settled = &copied
	return &x402.SettlementResult{Success: true, TransactionHash: "0xtx", Network: requirements.Network, Amount: requirements.Amount, PayerAddress: "0xPayer"}, nil
}

func (v *bindingVerifier) SupportedKinds() []x402.SupportedKind { return nil }

func bindingRule() x402.PricingRule {
	return x402.PricingRule{
		AcceptedTokens: []x402.TokenRequirement{
			{Network: "eip155:84532", Symbol: "USDC", AssetContract: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", Recipient: "0xRecipientA", Amount: "1000000", TokenName: "USDC", TokenVersion: "2"},
			{Network: "eip155:42161", Symbol: "USDC", AssetContract: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", Recipient: "0xRecipientB", Amount: "2000000", TokenName: "USD Coin", TokenVersion: "2"},
		},
	}
}

func bindingConfig(v *bindingVerifier) x402.Config {
	return x402.Config{
		Verifier:      v,
		MethodPricing: map[string]x402.PricingRule{bindingMethod: bindingRule()},
	}
}

func bindingMetadata(t *testing.T, accepted x402.PaymentRequirements) metadata.MD {
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

func bindingLegacyMetadata(t *testing.T) metadata.MD {
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

func wantPaymentRequired(t *testing.T, err error) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Fatalf("error = %v, want ResourceExhausted", err)
	}
}

func TestUnary_UnacceptedTokenDoesNotVerify(t *testing.T) {
	v := &bindingVerifier{t: t, forbidden: true}
	interceptor := UnaryServerInterceptor(bindingConfig(v))
	accepted := x402.PaymentRequirements{
		Scheme: "exact", Network: "eip155:84532", Amount: "1000000",
		Asset: "0x0000000000000000000000000000000000000bad", PayTo: "0xRecipientA",
		MaxTimeoutSeconds: 300,
		Extra:             map[string]interface{}{"name": "USDC", "version": "2"},
	}
	ctx := metadata.NewIncomingContext(context.Background(), bindingMetadata(t, accepted))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: bindingMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			t.Error("handler reached for an unaccepted token")
			return nil, nil
		})
	wantPaymentRequired(t, err)
}

func TestUnary_MismatchedAmountDoesNotVerify(t *testing.T) {
	v := &bindingVerifier{t: t, forbidden: true}
	interceptor := UnaryServerInterceptor(bindingConfig(v))
	// The asset is accepted, but the client claims a different price and recipient.
	accepted := x402.PaymentRequirements{
		Scheme: "exact", Network: "eip155:42161", Amount: "1",
		Asset: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", PayTo: "0xPayer",
	}
	ctx := metadata.NewIncomingContext(context.Background(), bindingMetadata(t, accepted))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: bindingMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			t.Error("handler reached for a mismatched accepted option")
			return nil, nil
		})
	wantPaymentRequired(t, err)
}

func TestUnary_EchoedTokenUsesTheRulesRequirements(t *testing.T) {
	v := &bindingVerifier{t: t}
	interceptor := UnaryServerInterceptor(bindingConfig(v))
	rule := bindingRule()
	want := x402.BuildAccepts(&rule, 5*time.Minute)[1]
	var paid *x402.PaymentContext
	ctx := metadata.NewIncomingContext(context.Background(), bindingMetadata(t, want))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: bindingMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			paid, _ = GetPaymentFromContext(ctx)
			return "ok", nil
		})
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if v.verified == nil || !reflect.DeepEqual(*v.verified, want) || v.settled == nil || !reflect.DeepEqual(*v.settled, want) {
		t.Errorf("verified %+v, settled %+v, want %+v", v.verified, v.settled, want)
	}
	if paid == nil || paid.Network != "eip155:42161" || paid.TokenSymbol != "USDC" || !paid.Verified || paid.TransactionHash != "" {
		t.Errorf("payment context = %+v", paid)
	}
}

func TestUnary_V1PaymentPaysTheFirstToken(t *testing.T) {
	v := &bindingVerifier{t: t}
	interceptor := UnaryServerInterceptor(bindingConfig(v))
	rule := bindingRule()
	want := x402.BuildAccepts(&rule, 5*time.Minute)[0]
	ctx := metadata.NewIncomingContext(context.Background(), bindingLegacyMetadata(t))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: bindingMethod},
		func(ctx context.Context, req interface{}) (interface{}, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if v.verified == nil || !reflect.DeepEqual(*v.verified, want) {
		t.Errorf("verified %+v, want %+v", v.verified, want)
	}
}

type bindingStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *bindingStream) Context() context.Context { return s.ctx }
func (s *bindingStream) SetTrailer(metadata.MD)   {}

func TestStream_UnacceptedTokenDoesNotVerify(t *testing.T) {
	v := &bindingVerifier{t: t, forbidden: true}
	interceptor := StreamServerInterceptor(bindingConfig(v))
	accepted := x402.PaymentRequirements{
		Scheme: "exact", Network: "eip155:84532", Amount: "1000000",
		Asset: "0x0000000000000000000000000000000000000bad", PayTo: "0xRecipientA",
		MaxTimeoutSeconds: 300,
		Extra:             map[string]interface{}{"name": "USDC", "version": "2"},
	}
	ss := &bindingStream{ctx: metadata.NewIncomingContext(context.Background(), bindingMetadata(t, accepted))}
	err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: bindingMethod},
		func(srv interface{}, stream grpc.ServerStream) error {
			t.Error("handler reached for an unaccepted token")
			return nil
		})
	wantPaymentRequired(t, err)
}

func TestStream_EchoedTokenUsesTheRulesRequirements(t *testing.T) {
	v := &bindingVerifier{t: t}
	interceptor := StreamServerInterceptor(bindingConfig(v))
	rule := bindingRule()
	want := x402.BuildAccepts(&rule, 5*time.Minute)[1]
	ss := &bindingStream{ctx: metadata.NewIncomingContext(context.Background(), bindingMetadata(t, want))}
	err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: bindingMethod},
		func(srv interface{}, stream grpc.ServerStream) error { return nil })
	if err != nil {
		t.Fatalf("interceptor error: %v", err)
	}
	if v.verified == nil || !reflect.DeepEqual(*v.verified, want) || v.settled == nil || !reflect.DeepEqual(*v.settled, want) {
		t.Errorf("verified %+v, settled %+v, want %+v", v.verified, v.settled, want)
	}
}
