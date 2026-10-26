package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402"
)

// Reasons a signed exact-scheme authorization does not satisfy its requirements.
const (
	ReasonRecipientMismatch  = "invalid_exact_evm_payload_recipient_mismatch"
	ReasonAuthorizationValue = "invalid_exact_evm_payload_authorization_value"
)

const supportedRefresh = 10 * time.Minute

// EVMVerifier settles through a facilitator.
// An exact EIP-3009 payload is bound to the requirements before it is sent:
// authorization.to must equal payTo and authorization.value must equal amount,
// and only the scheme's own fields are forwarded. Other schemes stay opaque.
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

// Verify binds an exact EIP-3009 authorization, then calls POST /verify.
func (v *EVMVerifier) Verify(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.VerificationResult, error) {
	forwarded, rejected, err := bindExact(payload, requirements)
	if err != nil || rejected != nil {
		return rejected, err
	}
	resp, err := v.facilitator.Verify(ctx, &FacilitatorPaymentRequest{
		X402Version:         versionOf(forwarded),
		PaymentPayload:      forwarded,
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

// Settle binds an exact EIP-3009 authorization, then calls POST /settle.
// A decoded failure, including settlement_pending, is a result with Success false.
func (v *EVMVerifier) Settle(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.SettlementResult, error) {
	forwarded, rejected, err := bindExact(payload, requirements)
	if err != nil {
		return nil, err
	}
	if rejected != nil {
		return nil, fmt.Errorf("payment does not satisfy its requirements: %s", rejected.Reason)
	}
	resp, err := v.facilitator.Settle(ctx, &FacilitatorPaymentRequest{
		X402Version:         versionOf(forwarded),
		PaymentPayload:      forwarded,
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

// bindExact returns the payload to forward. A non-nil result means the
// payment was refused locally and must not reach the facilitator.
func bindExact(payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.PaymentPayload, *x402.VerificationResult, error) {
	if !exactEIP3009(payload, requirements) {
		return payload, nil, nil
	}
	evmPayload, err := parseEVMPayload(payload)
	if err != nil {
		return nil, &x402.VerificationResult{Valid: false, Reason: fmt.Sprintf("invalid payload: %v", err)}, nil
	}
	reason, err := checkBinding(evmPayload.Authorization, requirements)
	if err != nil {
		return nil, nil, err
	}
	if reason != "" {
		return nil, &x402.VerificationResult{Valid: false, Reason: reason}, nil
	}
	return schemePayload(payload, evmPayload), nil, nil
}

func exactEIP3009(payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) bool {
	scheme := ""
	var extra map[string]interface{}
	if requirements != nil {
		scheme = requirements.Scheme
		extra = requirements.Extra
	}
	if scheme == "" && payload != nil {
		scheme = payload.Accepted.Scheme
	}
	if scheme != "" && scheme != "exact" {
		return false
	}
	if extra == nil && payload != nil {
		extra = payload.Accepted.Extra
	}
	if extra != nil {
		if method, _ := extra["assetTransferMethod"].(string); method != "" && !strings.EqualFold(method, "eip3009") {
			return false
		}
	}
	return true
}

// checkBinding holds the signed authorization to the requirements, as the exact
// scheme defines. It returns the reason the authorization falls short, or ""
// when it is bound. Requirements that do not say what must be paid are an error.
func checkBinding(auth *Authorization, requirements *x402.PaymentRequirements) (string, error) {
	if requirements == nil || requirements.PayTo == "" || requirements.Amount == "" {
		return "", fmt.Errorf("payment requirements must set payTo and amount")
	}
	required, ok := new(big.Int).SetString(requirements.Amount, 10)
	if !ok {
		return "", fmt.Errorf("payment requirements amount %q is not an integer", requirements.Amount)
	}
	if !strings.EqualFold(auth.To, requirements.PayTo) {
		return ReasonRecipientMismatch, nil
	}
	value, ok := new(big.Int).SetString(auth.Value, 10)
	if !ok || value.Cmp(required) != 0 {
		return ReasonAuthorizationValue, nil
	}
	return "", nil
}

// schemePayload returns the payment with only the exact scheme's fields in payload.
func schemePayload(payload *x402.PaymentPayload, evmPayload *EVMPayload) *x402.PaymentPayload {
	return &x402.PaymentPayload{
		X402Version: payload.X402Version,
		Resource:    payload.Resource,
		Accepted:    payload.Accepted,
		Payload:     evmPayload,
		Extensions:  payload.Extensions,
	}
}

func parseEVMPayload(payload *x402.PaymentPayload) (*EVMPayload, error) {
	if payload == nil {
		return nil, fmt.Errorf("payload is required")
	}
	payloadBytes, err := json.Marshal(payload.Payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}
	var evmPayload EVMPayload
	if err := json.Unmarshal(payloadBytes, &evmPayload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal EVM payload: %w", err)
	}
	if evmPayload.Signature == "" {
		return nil, fmt.Errorf("signature is required")
	}
	if evmPayload.Authorization == nil {
		return nil, fmt.Errorf("authorization is required")
	}
	auth := evmPayload.Authorization
	if auth.From == "" || auth.To == "" || auth.Value == "" || auth.Nonce == "" {
		return nil, fmt.Errorf("authorization missing required fields")
	}
	return &evmPayload, nil
}

func versionOf(payload *x402.PaymentPayload) int {
	if payload != nil && payload.X402Version > 0 {
		return payload.X402Version
	}
	return 2
}
