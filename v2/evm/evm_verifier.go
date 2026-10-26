package evm

import (
	"context"
	"fmt"
	"sync"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
)

const supportedRefresh = 10 * time.Minute

// EVMVerifier settles through a facilitator. Payment payloads are opaque:
// this process does not parse EIP-3009, Permit2, or other scheme bodies.
type EVMVerifier struct {
	facilitator *FacilitatorClient

	mu    sync.RWMutex
	kinds []x402.SupportedKind

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewEVMVerifier fetches /supported once and fails closed when that call fails.
// A background refresh updates the cached kinds. A request never calls /supported.
func NewEVMVerifier(facilitatorURL string) (*EVMVerifier, error) {
	client := NewFacilitatorClient(facilitatorURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	supported, err := client.GetSupported(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to fetch supported kinds: %w", err)
	}

	v := &EVMVerifier{
		facilitator: client,
		kinds:       append([]x402.SupportedKind(nil), supported.Kinds...),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go v.refreshLoop()
	return v, nil
}

// Close stops the background refresh and closes idle facilitator connections.
func (v *EVMVerifier) Close() {
	v.once.Do(func() {
		close(v.stop)
		<-v.done
		v.facilitator.Close()
	})
}

// Verify forwards the payload and requirements to POST /verify.
func (v *EVMVerifier) Verify(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.VerificationResult, error) {
	resp, err := v.facilitator.Verify(ctx, &FacilitatorPaymentRequest{
		X402Version:         versionOf(payload),
		PaymentPayload:      payload,
		PaymentRequirements: requirements,
	})
	if err != nil {
		return nil, fmt.Errorf("facilitator verification failed: %w", err)
	}
	amount := ""
	if requirements != nil {
		amount = requirements.Amount
	}
	return &x402.VerificationResult{
		Valid:              resp.IsValid,
		Reason:             resp.InvalidReason,
		PayerAddress:       resp.Payer,
		Amount:             amount,
		Extensions:         resp.Extensions,
		ExtensionResponses: resp.ExtensionResponses,
	}, nil
}

// Settle forwards the payload and requirements to POST /settle.
// A decoded failure, including settlement_pending, is a result with Success false.
func (v *EVMVerifier) Settle(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.SettlementResult, error) {
	resp, err := v.facilitator.Settle(ctx, &FacilitatorPaymentRequest{
		X402Version:         versionOf(payload),
		PaymentPayload:      payload,
		PaymentRequirements: requirements,
	})
	if err != nil {
		return nil, fmt.Errorf("facilitator settlement failed: %w", err)
	}
	status := "success"
	if !resp.Success {
		status = "failed"
		if resp.ErrorReason == "settlement_pending" {
			status = "pending"
		}
	}
	amount := resp.Amount
	if amount == "" && requirements != nil {
		amount = requirements.Amount
	}
	network := resp.Network
	if network == "" && requirements != nil {
		network = requirements.Network
	}
	return &x402.SettlementResult{
		Success:            resp.Success,
		ErrorReason:        resp.ErrorReason,
		TransactionHash:    resp.Transaction,
		Status:             status,
		SettledAt:          time.Now(),
		Amount:             amount,
		PayerAddress:       resp.Payer,
		Network:            network,
		Extensions:         resp.Extensions,
		ExtensionResponses: resp.ExtensionResponses,
	}, nil
}

// SupportedKinds returns the kinds cached from the facilitator.
func (v *EVMVerifier) SupportedKinds() []x402.SupportedKind {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]x402.SupportedKind, len(v.kinds))
	copy(out, v.kinds)
	return out
}

func (v *EVMVerifier) refreshLoop() {
	defer close(v.done)
	ticker := time.NewTicker(supportedRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-ticker.C:
			v.refresh()
		}
	}
}

func (v *EVMVerifier) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	supported, err := v.facilitator.GetSupported(ctx)
	if err != nil || supported == nil {
		return
	}
	v.mu.Lock()
	v.kinds = append([]x402.SupportedKind(nil), supported.Kinds...)
	v.mu.Unlock()
}

func versionOf(payload *x402.PaymentPayload) int {
	if payload != nil && payload.X402Version > 0 {
		return payload.X402Version
	}
	return 2
}
