package evm

// EVMPayload is the EIP-3009 payload shape. The verifier does not parse it.
// Scheme payloads are forwarded to the facilitator as opaque JSON, so string
// timestamps and Permit2 bodies are not rejected in this process.
type EVMPayload struct {
	Signature     string         `json:"signature"`
	Authorization *Authorization `json:"authorization"`
}

// Authorization contains EIP-3009 authorization parameters.
// ValidAfter and ValidBefore are JSON values because the spec encodes them
// as strings. A local integer type would reject a compliant payload.
type Authorization struct {
	From        string      `json:"from"`
	To          string      `json:"to"`
	Value       string      `json:"value"`
	ValidAfter  interface{} `json:"validAfter"`
	ValidBefore interface{} `json:"validBefore"`
	Nonce       string      `json:"nonce"`
}

// FacilitatorPaymentRequest is the body of POST /verify and POST /settle.
type FacilitatorPaymentRequest struct {
	X402Version         int         `json:"x402Version"`
	PaymentPayload      interface{} `json:"paymentPayload"`
	PaymentRequirements interface{} `json:"paymentRequirements"`
}

// FacilitatorVerifyRequest is the body of POST /verify.
type FacilitatorVerifyRequest = FacilitatorPaymentRequest

// FacilitatorSettleRequest is the body of POST /settle.
type FacilitatorSettleRequest = FacilitatorPaymentRequest

// FacilitatorVerifyResponse is the decoded /verify response.
// ExtensionResponses is the EXTENSION-RESPONSES side channel, not part of the JSON body.
type FacilitatorVerifyResponse struct {
	IsValid            bool                   `json:"isValid"`
	InvalidReason      string                 `json:"invalidReason,omitempty"`
	Payer              string                 `json:"payer,omitempty"`
	Extensions         map[string]interface{} `json:"extensions,omitempty"`
	ExtensionResponses map[string]interface{} `json:"-"`
}

// FacilitatorSettleResponse is the decoded /settle response.
// Success false is a settlement result. It includes settlement_pending,
// which keeps Transaction.
type FacilitatorSettleResponse struct {
	Success            bool                   `json:"success"`
	ErrorReason        string                 `json:"errorReason,omitempty"`
	Payer              string                 `json:"payer,omitempty"`
	Transaction        string                 `json:"transaction"`
	Network            string                 `json:"network"`
	Amount             string                 `json:"amount,omitempty"`
	Extensions         map[string]interface{} `json:"extensions,omitempty"`
	ExtensionResponses map[string]interface{} `json:"-"`
}
