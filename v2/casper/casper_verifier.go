package casper

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
)

// Scheme is the x402 scheme the Casper mechanism implements.
const Scheme = "exact"

// CAIP-2 identifiers of the Casper networks.
const (
	NetworkMainnet = "casper:casper"
	NetworkTestnet = "casper:casper-test"
)

// Reasons a payment's signed authorization does not satisfy its requirements,
// as the exact Casper scheme names them.
const (
	ReasonNetworkMismatch    = "invalid_exact_casper_network_mismatch"
	ReasonPayToMismatch      = "invalid_exact_casper_payto_mismatch"
	ReasonAmountMismatch     = "invalid_exact_casper_amount_mismatch"
	ReasonUnsupportedNetwork = "invalid_exact_casper_unsupported_network"
)

// supportedKindsTimeout bounds the constructor's call to the facilitator.
const supportedKindsTimeout = 10 * time.Second

// CasperVerifier implements x402.ChainVerifier for the Casper network using a
// facilitator service.
//
// Casper is not EVM: accounts are account hashes rather than 20-byte
// addresses, and settlement moves a CEP-18 token (wCSPR by default) instead of
// an ERC-20. Everything chain-specific — signature checks against the
// TransferWithAuthorization digest, deploy construction, gas — lives in the
// facilitator, exactly as it does for the EVM verifier.
type CasperVerifier struct {
	facilitator *FacilitatorClient
	kinds       []x402.SupportedKind
}

// NewCasperVerifier creates a Casper verifier that delegates to a facilitator
// service. An empty facilitatorURL uses DefaultFacilitatorURL.
//
// The supported scheme+network pairs are read from the facilitator at
// construction, so a verifier only ever advertises what its facilitator can
// actually settle.
func NewCasperVerifier(facilitatorURL string, opts ...FacilitatorOption) (*CasperVerifier, error) {
	client := NewFacilitatorClient(facilitatorURL, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), supportedKindsTimeout)
	defer cancel()

	supported, err := client.GetSupported(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch supported kinds: %w", err)
	}

	kinds := make([]x402.SupportedKind, 0, len(supported.Kinds))
	for _, k := range supported.Kinds {
		kinds = append(kinds, x402.SupportedKind{
			Scheme:  k.Scheme,
			Network: k.Network,
		})
	}

	return &CasperVerifier{
		facilitator: client,
		kinds:       kinds,
	}, nil
}

// Verify checks if a payment is valid without settling it.
func (v *CasperVerifier) Verify(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.VerificationResult, error) {
	casperPayload, err := parseCasperPayload(payload.Payload)
	if err != nil {
		return &x402.VerificationResult{
			Valid:  false,
			Reason: fmt.Sprintf("invalid payload: %v", err),
		}, nil
	}

	reason, err := v.checkBinding(casperPayload.Authorization, requirements)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return &x402.VerificationResult{Valid: false, Reason: reason}, nil
	}

	verifyResp, err := v.facilitator.Verify(ctx, &FacilitatorVerifyRequest{
		PaymentPayload:      facilitatorPayload(payload, casperPayload, requirements),
		PaymentRequirements: requirements,
	})
	if err != nil {
		return nil, fmt.Errorf("facilitator verification failed: %w", err)
	}

	return &x402.VerificationResult{
		Valid:        verifyResp.IsValid,
		Reason:       verifyResp.InvalidReason,
		PayerAddress: casperPayload.Authorization.From,
		Amount:       casperPayload.Authorization.Value,
	}, nil
}

// Settle executes the payment on-chain and returns settlement details.
func (v *CasperVerifier) Settle(ctx context.Context, payload *x402.PaymentPayload, requirements *x402.PaymentRequirements) (*x402.SettlementResult, error) {
	casperPayload, err := parseCasperPayload(payload.Payload)
	if err != nil {
		return nil, fmt.Errorf("invalid payload: %w", err)
	}

	reason, err := v.checkBinding(casperPayload.Authorization, requirements)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return nil, fmt.Errorf("payment does not satisfy its requirements: %s", reason)
	}

	settleResp, err := v.facilitator.Settle(ctx, &FacilitatorSettleRequest{
		PaymentPayload:      facilitatorPayload(payload, casperPayload, requirements),
		PaymentRequirements: requirements,
	})
	if err != nil {
		return nil, fmt.Errorf("facilitator settlement failed: %w", err)
	}

	if !settleResp.Success {
		return nil, fmt.Errorf("settlement failed: %s", settlementError(settleResp))
	}

	return &x402.SettlementResult{
		TransactionHash:  settleResp.Transaction,
		Status:           "success",
		SettledAt:        time.Now(),
		Amount:           casperPayload.Authorization.Value,
		PayerAddress:     casperPayload.Authorization.From,
		RecipientAddress: casperPayload.Authorization.To,
		Network:          settleResp.Network,
	}, nil
}

// SupportedKinds returns the supported scheme+network pairs.
func (v *CasperVerifier) SupportedKinds() []x402.SupportedKind {
	return v.kinds
}

// checkBinding holds the signed authorization to the requirements, as the
// exact scheme defines: it must pay exactly the required amount to the
// required recipient, on a network this verifier's facilitator settles. It
// returns the reason the authorization falls short, or "" when it is bound.
// Requirements that do not say what must be paid are an error.
func (v *CasperVerifier) checkBinding(auth *Authorization, requirements *x402.PaymentRequirements) (string, error) {
	if requirements == nil || requirements.PayTo == "" || requirements.Amount == "" {
		return "", fmt.Errorf("payment requirements must set payTo and amount")
	}
	required, ok := new(big.Int).SetString(requirements.Amount, 10)
	if !ok {
		return "", fmt.Errorf("payment requirements amount %q is not an integer", requirements.Amount)
	}

	if !v.supports(requirements.Network) {
		return ReasonUnsupportedNetwork, nil
	}
	// Casper account hashes are hex, so letter case carries no meaning.
	if !strings.EqualFold(auth.To, requirements.PayTo) {
		return ReasonPayToMismatch, nil
	}
	value, ok := new(big.Int).SetString(auth.Value, 10)
	if !ok || value.Cmp(required) != 0 {
		return ReasonAmountMismatch, nil
	}
	return "", nil
}

// supports reports whether the facilitator settles the exact scheme on the
// given CAIP-2 network.
func (v *CasperVerifier) supports(network string) bool {
	for _, kind := range v.kinds {
		if kind.Scheme == Scheme && kind.Network == network {
			return true
		}
	}
	return false
}

// facilitatorPayload returns the payment in the shape the Casper facilitator
// takes, carrying only the exact scheme's own fields so nothing else a client
// attached reaches the facilitator.
func facilitatorPayload(payload *x402.PaymentPayload, casperPayload *CasperPayload, requirements *x402.PaymentRequirements) *FacilitatorPaymentPayload {
	return &FacilitatorPaymentPayload{
		X402Version: payload.X402Version,
		Scheme:      Scheme,
		Network:     requirements.Network,
		Payload:     casperPayload,
	}
}

// settlementError describes why the facilitator refused to settle.
func settlementError(resp *FacilitatorSettleResponse) string {
	if resp.ErrorMessage == "" {
		return resp.ErrorReason
	}
	if resp.ErrorReason == "" {
		return resp.ErrorMessage
	}
	return resp.ErrorReason + ": " + resp.ErrorMessage
}

func parseCasperPayload(payload interface{}) (*CasperPayload, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	var casperPayload CasperPayload
	if err := json.Unmarshal(payloadBytes, &casperPayload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Casper payload: %w", err)
	}

	if casperPayload.Signature == "" {
		return nil, fmt.Errorf("signature is required")
	}

	// The facilitator cannot recover a Casper account hash from a signature,
	// so the payer's public key must travel with the payment.
	if casperPayload.PublicKey == "" {
		return nil, fmt.Errorf("publicKey is required")
	}

	if casperPayload.Authorization == nil {
		return nil, fmt.Errorf("authorization is required")
	}

	auth := casperPayload.Authorization
	if auth.From == "" || auth.To == "" || auth.Value == "" || auth.Nonce == "" {
		return nil, fmt.Errorf("authorization missing required fields")
	}

	return &casperPayload, nil
}
