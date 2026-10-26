package grpc

import (
	"context"
	"fmt"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor enforces x402 on a unary RPC.
// The handler return value is withheld until settlement finishes on the
// authorization flow, which is the gRPC equivalent of buffering an HTTP body.
func UnaryServerInterceptor(cfg x402.Config) grpc.UnaryServerInterceptor {
	srv, err := x402.NewServer(cfg)
	if err != nil {
		panic(fmt.Sprintf("invalid x402 config: %v", err))
	}

	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		result := srv.Execute(ctx, callFromContext(ctx, info.FullMethod), func(payCtx context.Context) (x402.HandlerOutput, error) {
			resp, err := handler(payCtx, req)
			return x402.HandlerOutput{GRPC: resp, HasGRPC: true}, err
		})
		if result.Outcome == x402.OutcomeReplay && !result.Output.HasGRPC {
			payCtx := ctx
			if result.Payment != nil {
				payCtx = context.WithValue(ctx, x402.PaymentContextKey, result.Payment)
			}
			resp, err := handler(payCtx, req)
			result.Output.GRPC = resp
			result.Output.HasGRPC = true
			result.Err = err
		}
		return finishCall(ctx, result)
	}
}

func callFromContext(ctx context.Context, fullMethod string) x402.Call {
	md, _ := metadata.FromIncomingContext(ctx)
	header, legacy := rawPayment(md)
	var deadline time.Duration
	if until, ok := ctx.Deadline(); ok {
		deadline = time.Until(until)
	}
	return x402.Call{
		RouteKey:      fullMethod,
		GRPC:          true,
		Deadline:      deadline,
		PaymentHeader: header,
		Legacy:        legacy,
		Method:        fullMethod,
	}
}

func finishCall(ctx context.Context, result x402.Result) (interface{}, error) {
	switch result.Outcome {
	case x402.OutcomeFree:
		return result.Output.GRPC, result.Err
	case x402.OutcomePaymentRequired:
		return nil, paymentRequiredError(ctx, result)
	case x402.OutcomeInvalid:
		reason := result.Reason
		if reason == "" {
			reason = "invalid_payment"
		}
		return nil, status.Error(codes.InvalidArgument, reason)
	case x402.OutcomeConflict:
		return nil, status.Error(codes.AlreadyExists, result.Reason)
	case x402.OutcomeInternal, x402.OutcomeBufferOverflow:
		return nil, status.Error(codes.Internal, result.Reason)
	case x402.OutcomeHandlerFailed:
		setReceiptTrailer(ctx, result)
		return result.Output.GRPC, result.Err
	case x402.OutcomeSettled, x402.OutcomeReplay:
		setReceiptTrailer(ctx, result)
		return result.Output.GRPC, result.Err
	case x402.OutcomeSettleFailed:
		setReceiptTrailer(ctx, result)
		return nil, status.Error(codes.ResourceExhausted, result.Reason)
	default:
		return nil, status.Error(codes.Internal, "payment_processing_failed")
	}
}

func paymentRequiredError(ctx context.Context, result x402.Result) error {
	reason := result.Reason
	if reason == "" {
		reason = "payment_required"
	}
	st := status.New(codes.ResourceExhausted, reason)
	if detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: "PAYMENT_REQUIRED",
		Domain: "x402.org",
	}); err == nil {
		st = detailed
	}
	if result.Challenge != nil {
		if encoded, err := EncodePaymentRequired(result.Challenge); err == nil {
			_ = grpc.SetTrailer(ctx, metadata.Pairs(MetadataKeyPaymentRequired, encoded))
		}
	}
	return st.Err()
}

func setReceiptTrailer(ctx context.Context, result x402.Result) {
	if result.Receipt == nil {
		return
	}
	encoded, err := EncodePaymentResponse(result.Receipt)
	if err != nil {
		return
	}
	key := MetadataKeyPaymentResponse
	if result.Legacy {
		key = MetadataKeyLegacyPaymentResponse
	}
	_ = grpc.SetTrailer(ctx, metadata.Pairs(key, encoded))
}

// GetPaymentFromContext extracts the verified payment from a handler context.
func GetPaymentFromContext(ctx context.Context) (*x402.PaymentContext, bool) {
	payment, ok := ctx.Value(x402.PaymentContextKey).(*PaymentContextValue)
	if ok {
		return payment, true
	}
	return x402Payment(ctx)
}

// PaymentContextValue is an alias kept so existing callers compile.
// The value stored by the server is *x402.PaymentContext.
type PaymentContextValue = x402.PaymentContext

func x402Payment(ctx context.Context) (*x402.PaymentContext, bool) {
	payment, ok := ctx.Value(x402.PaymentContextKey).(*x402.PaymentContext)
	return payment, ok
}

// RequirePayment extracts payment from context and returns an error if it is missing.
func RequirePayment(ctx context.Context) (*x402.PaymentContext, error) {
	payment, ok := GetPaymentFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.ResourceExhausted, "payment context not found")
	}
	if !payment.Verified {
		return nil, status.Error(codes.ResourceExhausted, "payment not verified")
	}
	return payment, nil
}
