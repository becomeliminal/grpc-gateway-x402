package grpc

import (
	"context"
	"fmt"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// StreamServerInterceptor enforces x402 on a server stream.
// The default authorization flow verifies before the first message and settles
// only when the handler returns nil. Bytes already sent cannot be recalled.
// A route that must not leak unpaid bytes sets PaymentFlow to upfront.
// On upfront, a failed handler still gets the settlement receipt in the trailer.
func StreamServerInterceptor(cfg x402.Config) grpc.StreamServerInterceptor {
	srv, err := x402.NewServer(cfg)
	if err != nil {
		panic(fmt.Sprintf("invalid x402 config: %v", err))
	}

	return func(srvImpl interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		result := srv.Execute(ctx, callFromContext(ctx, info.FullMethod), func(payCtx context.Context) (x402.HandlerOutput, error) {
			err := handler(srvImpl, &paymentServerStream{ServerStream: ss, ctx: payCtx})
			return x402.HandlerOutput{}, err
		})
		if result.Outcome == x402.OutcomeReplay {
			payCtx := ctx
			if result.Payment != nil {
				payCtx = context.WithValue(ctx, x402.PaymentContextKey, result.Payment)
			}
			result.Err = handler(srvImpl, &paymentServerStream{ServerStream: ss, ctx: payCtx})
			if result.Err != nil {
				result.Outcome = x402.OutcomeHandlerFailed
			}
		}
		_, err := finishCall(ctx, result)
		return err
	}
}

type paymentServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *paymentServerStream) Context() context.Context {
	return s.ctx
}

func (s *paymentServerStream) SendHeader(md metadata.MD) error {
	return s.ServerStream.SendHeader(md)
}

func (s *paymentServerStream) SetTrailer(md metadata.MD) {
	s.ServerStream.SetTrailer(md)
}
