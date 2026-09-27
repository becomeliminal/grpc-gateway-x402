package casper

import (
	x402 "github.com/becomeliminal/grpc-gateway-x402/v2"
)

// CasperPayload represents the Casper-specific payload in a payment.
// It authorizes a CEP-18 transfer, Casper's fungible token standard.
//
// Unlike EVM, the payer's public key travels with the payment: Casper
// addresses are account hashes, which a signature alone cannot be recovered
// to, so the facilitator needs the key to check the signature.
type CasperPayload struct {
	Signature     string         `json:"signature"`
	PublicKey     string         `json:"publicKey"`
	Authorization *Authorization `json:"authorization"`
}

// Authorization contains the signed transfer authorization parameters.
//
// From, To and Nonce are Casper hex strings, not 20-byte EVM addresses:
// the accounts are account hashes prefixed by their key algorithm
// ("00" for ED25519, "01" for SECP256K1), and the nonce is 32 bytes of hex.
// The validity window is a decimal string, as the Casper mechanism signs it.
type Authorization struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

// FacilitatorPaymentPayload is the payment as the Casper facilitator takes it:
// the scheme and network alongside the scheme's own payload.
type FacilitatorPaymentPayload struct {
	X402Version int            `json:"x402Version"`
	Scheme      string         `json:"scheme"`
	Network     string         `json:"network"` // CAIP-2
	Payload     *CasperPayload `json:"payload"`
}

// FacilitatorVerifyRequest is the request to POST /verify.
type FacilitatorVerifyRequest struct {
	PaymentPayload      *FacilitatorPaymentPayload `json:"paymentPayload"`
	PaymentRequirements *x402.PaymentRequirements  `json:"paymentRequirements"`
}

// FacilitatorVerifyResponse is the response from POST /verify.
type FacilitatorVerifyResponse struct {
	IsValid        bool   `json:"isValid"`
	InvalidReason  string `json:"invalidReason,omitempty"`
	InvalidMessage string `json:"invalidMessage,omitempty"`
	Payer          string `json:"payer,omitempty"`
}

// FacilitatorSettleRequest is the request to POST /settle.
// It has the same shape as the verify request.
type FacilitatorSettleRequest struct {
	PaymentPayload      *FacilitatorPaymentPayload `json:"paymentPayload"`
	PaymentRequirements *x402.PaymentRequirements  `json:"paymentRequirements"`
}

// FacilitatorSettleResponse is the response from POST /settle.
type FacilitatorSettleResponse struct {
	Success      bool   `json:"success"`
	ErrorReason  string `json:"errorReason,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Payer        string `json:"payer,omitempty"`
	Transaction  string `json:"transaction,omitempty"` // Casper deploy hash
	Network      string `json:"network,omitempty"`     // CAIP-2
}

// FacilitatorSupportedResponse is the response from GET /supported.
type FacilitatorSupportedResponse struct {
	Kinds []SupportedKind `json:"kinds"`
}

// SupportedKind represents a supported scheme+network pair. The Casper
// facilitator also reports the token metadata payers must sign over in Extra.
type SupportedKind struct {
	X402Version int                    `json:"x402Version,omitempty"`
	Scheme      string                 `json:"scheme"`
	Network     string                 `json:"network"` // CAIP-2
	Extra       map[string]interface{} `json:"extra,omitempty"`
}
