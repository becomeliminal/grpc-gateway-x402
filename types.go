package x402

import (
	"context"
	"time"
)

// ResourceInfo identifies the resource a payment covers.
// URL is required on a challenge. The other fields are optional.
type ResourceInfo struct {
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	MimeType    string   `json:"mimeType,omitempty"`
	ServiceName string   `json:"serviceName,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	IconURL     string   `json:"iconUrl,omitempty"`
}

// PaymentRequirements describes one accepted way to pay for a resource.
// Network is a CAIP-2 identifier (for example "eip155:8453").
// Amount is an atomic-unit integer encoded as a string.
type PaymentRequirements struct {
	Scheme            string                 `json:"scheme"`
	Network           string                 `json:"network"`
	Amount            string                 `json:"amount"`
	Asset             string                 `json:"asset"`
	PayTo             string                 `json:"payTo"`
	MaxTimeoutSeconds int                    `json:"maxTimeoutSeconds"`
	Extra             map[string]interface{} `json:"extra,omitempty"`
}

// PaymentPayload is the object a client sends in PAYMENT-SIGNATURE.
// Payload is scheme-specific and is forwarded to the facilitator unchanged.
type PaymentPayload struct {
	X402Version int                    `json:"x402Version"`
	Resource    *ResourceInfo          `json:"resource,omitempty"`
	Accepted    PaymentRequirements    `json:"accepted"`
	Payload     interface{}            `json:"payload"`
	Extensions  map[string]interface{} `json:"extensions,omitempty"`
}

// SupportedKind is one scheme and network a facilitator can settle.
type SupportedKind struct {
	X402Version int                    `json:"x402Version,omitempty"`
	Scheme      string                 `json:"scheme"`
	Network     string                 `json:"network"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

// SupportedResponse is the facilitator's /supported document.
// Signers maps a CAIP-2 network to the facilitator addresses on that network.
type SupportedResponse struct {
	Kinds      []SupportedKind     `json:"kinds"`
	Extensions []string            `json:"extensions"`
	Signers    map[string][]string `json:"signers"`
}

// VerificationResult is the facilitator's decision on a payment payload.
// ExtensionResponses comes from the EXTENSION-RESPONSES side channel.
// It is for server hooks and is not copied onto the buyer-facing receipt.
type VerificationResult struct {
	Valid              bool
	Reason             string
	PayerAddress       string
	Amount             string
	TokenSymbol        string
	Extensions         map[string]interface{}
	ExtensionResponses map[string]interface{}
}

// SettlementResult is the facilitator's settlement receipt.
// Success is false for a decoded settlement failure, including settlement_pending.
// A transport failure is returned as an error instead of a result.
// TransactionHash is kept when the facilitator reports one, including settlement_pending.
// The resource server retries settlement_pending once when the hash is non-empty.
type SettlementResult struct {
	Success            bool
	ErrorReason        string
	TransactionHash    string
	Status             string
	SettledAt          time.Time
	Amount             string
	PayerAddress       string
	RecipientAddress   string
	Network            string
	Extensions         map[string]interface{}
	ExtensionResponses map[string]interface{}
}

// PaymentResponse is the PAYMENT-RESPONSE object.
// Transaction and network are always present. Transaction may be empty.
type PaymentResponse struct {
	Success     bool                   `json:"success"`
	Transaction string                 `json:"transaction"`
	Network     string                 `json:"network"`
	Payer       string                 `json:"payer,omitempty"`
	ErrorReason string                 `json:"errorReason,omitempty"`
	Amount      string                 `json:"amount,omitempty"`
	Extensions  map[string]interface{} `json:"extensions,omitempty"`
}

// PaymentRequiredResponse is the 402 PaymentRequired object.
type PaymentRequiredResponse struct {
	X402Version int                    `json:"x402Version"`
	Error       string                 `json:"error"`
	Resource    *ResourceInfo          `json:"resource,omitempty"`
	Accepts     []PaymentRequirements  `json:"accepts"`
	Extensions  map[string]interface{} `json:"extensions,omitempty"`
}

// NetworkInfo describes a supported blockchain network.
type NetworkInfo struct {
	Network        string
	ChainID        string
	ChainType      string
	NativeCurrency string
}

// ChainVerifier verifies and settles payments. Implementations must treat
// PaymentPayload.Payload as opaque bytes for the facilitator.
type ChainVerifier interface {
	// Verify checks a payment without moving funds.
	Verify(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*VerificationResult, error)

	// Settle asks the facilitator to move funds.
	// A decoded failure (Success == false) is returned as a result, not an error.
	Settle(ctx context.Context, payload *PaymentPayload, requirements *PaymentRequirements) (*SettlementResult, error)

	// SupportedKinds returns the scheme and network pairs known at startup.
	SupportedKinds() []SupportedKind
}

// PaymentContext is the verified payment a handler can read during the call.
// On the authorization flow TransactionHash is empty, because settlement
// runs only after the handler succeeds.
type PaymentContext struct {
	Verified        bool
	PayerAddress    string
	Amount          string
	TokenSymbol     string
	Network         string
	TransactionHash string
	SettledAt       time.Time
}

type contextKey string

const (
	// PaymentContextKey is the context key for *PaymentContext.
	PaymentContextKey contextKey = "x402-payment"
)

// LegacyPayment is a V1 X-PAYMENT header body.
type LegacyPayment struct {
	X402Version int         `json:"x402Version"`
	Scheme      string      `json:"scheme"`
	Network     string      `json:"network"`
	Payload     interface{} `json:"payload"`
}

// Payment flow names. The empty string means authorization.
const (
	FlowAuthorization = "authorization"
	FlowUpfront       = "upfront"
	FlowEscrow        = "escrow"
)

// ExtensionPaymentIdentifier is the extensions key whose id deduplicates retries.
const ExtensionPaymentIdentifier = "payment-identifier"
