package grpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type grpcMock struct {
	verify int
	settle int
	result *x402.SettlementResult
}

func (m *grpcMock) Verify(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.VerificationResult, error) {
	m.verify++
	return &x402.VerificationResult{Valid: true, PayerAddress: "0xPayer", Amount: "1000000"}, nil
}

func (m *grpcMock) Settle(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.SettlementResult, error) {
	m.settle++
	if m.result != nil {
		return m.result, nil
	}
	return &x402.SettlementResult{Success: true, TransactionHash: "0xtx", Network: "eip155:84532", PayerAddress: "0xPayer", Amount: "1000000"}, nil
}

func (m *grpcMock) SupportedKinds() []x402.SupportedKind { return nil }

func paidConfig(v x402.ChainVerifier) x402.Config {
	return x402.Config{
		Verifier: v,
		EndpointPricing: map[string]x402.PricingRule{
			"/v1/paid": paidRule(),
		},
		MethodPricing: map[string]x402.PricingRule{
			"/test.Service/Pay": paidRule(),
		},
	}
}

func paidRule() x402.PricingRule {
	return x402.PricingRule{
		Description: "premium",
		MimeType:    "application/json",
		ResourceURL: "https://api.example.com/v1/paid",
		AcceptedTokens: []x402.TokenRequirement{
			{
				Network:       "eip155:84532",
				Symbol:        "USDC",
				AssetContract: "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
				Recipient:     "0xRecipient",
				Amount:        "1000000",
				TokenName:     "USD Coin",
				TokenVersion:  "2",
			},
		},
	}
}

type trailerStream struct {
	trailer metadata.MD
}

func (s *trailerStream) Method() string                  { return "/test.Service/Pay" }
func (s *trailerStream) SetHeader(md metadata.MD) error  { return nil }
func (s *trailerStream) SendHeader(md metadata.MD) error { return nil }
func (s *trailerStream) SetTrailer(md metadata.MD) error {
	s.trailer = metadata.Join(s.trailer, md)
	return nil
}

func withTrailers(ctx context.Context) (context.Context, *trailerStream) {
	stream := &trailerStream{}
	return grpc.NewContextWithServerTransportStream(ctx, stream), stream
}

func TestGRPCChallengeMatchesHTTP(t *testing.T) {
	cfg := paidConfig(&grpcMock{})
	httpHandler := x402.PaymentMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/paid", nil)
	rec := httptest.NewRecorder()
	httpHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("http status %d", rec.Code)
	}
	httpChallenge, err := DecodePaymentRequirements(rec.Header().Get(x402.HeaderPaymentRequired))
	if err != nil {
		t.Fatal(err)
	}

	ctx, stream := withTrailers(context.Background())
	interceptor := UnaryServerInterceptor(cfg)
	_, err = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.Service/Pay"}, func(ctx context.Context, req interface{}) (interface{}, error) {
		t.Error("handler ran")
		return "nope", nil
	})
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted || st.Message() != "payment_required" {
		t.Fatalf("status = %v", err)
	}
	var info *errdetails.ErrorInfo
	for _, detail := range st.Details() {
		if got, ok := detail.(*errdetails.ErrorInfo); ok {
			info = got
		}
	}
	if info == nil || info.Reason != "PAYMENT_REQUIRED" || info.Domain != "x402.org" {
		t.Fatalf("error info = %+v", info)
	}
	values := stream.trailer.Get(MetadataKeyPaymentRequired)
	if len(values) != 1 {
		t.Fatalf("payment-required trailer = %v", stream.trailer)
	}
	grpcChallenge, err := DecodePaymentRequirements(values[0])
	if err != nil {
		t.Fatal(err)
	}

	httpJSON, _ := json.Marshal(httpChallenge)
	grpcJSON, _ := json.Marshal(grpcChallenge)
	if string(httpJSON) != string(grpcJSON) {
		t.Errorf("http %s\ngrpc %s", httpJSON, grpcJSON)
	}
}

func TestUnaryAuthorizationFlow(t *testing.T) {
	mock := &grpcMock{}
	interceptor := UnaryServerInterceptor(paidConfig(mock))
	header, err := EncodePaymentPayload(&x402.PaymentPayload{
		X402Version: 2,
		Accepted: x402.PaymentRequirements{
			Scheme:            "exact",
			Network:           "eip155:84532",
			Amount:            "1000000",
			Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			PayTo:             "0xRecipient",
			MaxTimeoutSeconds: 300,
			Extra:             map[string]interface{}{"name": "USD Coin", "version": "2"},
		},
		Payload: map[string]interface{}{"signature": "0xsig"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, stream := withTrailers(metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataKeyPaymentSignature, header)))
	var during string
	resp, err := interceptor(ctx, "req", &grpc.UnaryServerInfo{FullMethod: "/test.Service/Pay"}, func(ctx context.Context, req interface{}) (interface{}, error) {
		payment, ok := GetPaymentFromContext(ctx)
		if !ok {
			t.Fatal("missing payment context")
		}
		during = payment.TransactionHash
		return "resource", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp != "resource" {
		t.Errorf("resp = %v", resp)
	}
	if during != "" {
		t.Errorf("tx hash during handler = %q", during)
	}
	if mock.verify != 1 || mock.settle != 1 {
		t.Errorf("verify %d settle %d", mock.verify, mock.settle)
	}
	receipts := stream.trailer.Get(MetadataKeyPaymentResponse)
	if len(receipts) != 1 {
		t.Fatalf("trailer = %v", stream.trailer)
	}
	receipt, err := DecodePaymentResponse(receipts[0])
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Success || receipt.Transaction != "0xtx" {
		t.Errorf("receipt = %+v", receipt)
	}
}

func TestUnaryMalformedPayment(t *testing.T) {
	interceptor := UnaryServerInterceptor(paidConfig(&grpcMock{}))
	ctx, _ := withTrailers(metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataKeyPaymentSignature, "%%%")))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.Service/Pay"}, func(ctx context.Context, req interface{}) (interface{}, error) {
		t.Error("handler ran")
		return nil, nil
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v", err)
	}
}

func TestUnarySettleFailureDiscardsResponse(t *testing.T) {
	mock := &grpcMock{result: &x402.SettlementResult{
		Success: false, ErrorReason: "settlement_pending", TransactionHash: "0xpending", Network: "eip155:84532", PayerAddress: "0xPayer",
	}}
	interceptor := UnaryServerInterceptor(paidConfig(mock))
	header := mustPayload(t)
	ctx, stream := withTrailers(metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataKeyPaymentSignature, header)))
	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.Service/Pay"}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "secret", nil
	})
	if resp != nil {
		t.Errorf("response released: %v", resp)
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("code = %v", err)
	}
	receipt, decErr := DecodePaymentResponse(stream.trailer.Get(MetadataKeyPaymentResponse)[0])
	if decErr != nil {
		t.Fatal(decErr)
	}
	if receipt.Success || receipt.Transaction != "0xpending" {
		t.Errorf("receipt = %+v", receipt)
	}
}

func TestUnaryHandlerErrorDoesNotSettle(t *testing.T) {
	mock := &grpcMock{}
	interceptor := UnaryServerInterceptor(paidConfig(mock))
	ctx, _ := withTrailers(metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataKeyPaymentSignature, mustPayload(t))))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test.Service/Pay"}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, status.Error(codes.Internal, "boom")
	})
	if status.Code(err) != codes.Internal || mock.settle != 0 || mock.verify != 1 {
		t.Fatalf("err %v verify %d settle %d", err, mock.verify, mock.settle)
	}
}

func mustPayload(t *testing.T) string {
	t.Helper()
	header, err := EncodePaymentPayload(&x402.PaymentPayload{
		X402Version: 2,
		Accepted: x402.PaymentRequirements{
			Scheme:            "exact",
			Network:           "eip155:84532",
			Amount:            "1000000",
			Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			PayTo:             "0xRecipient",
			MaxTimeoutSeconds: 300,
			Extra:             map[string]interface{}{"name": "USD Coin", "version": "2"},
		},
		Payload: map[string]interface{}{"signature": "0xsig"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header
}
