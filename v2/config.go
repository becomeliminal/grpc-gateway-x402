package x402

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
)

// DefaultMaxBufferBytes is the body retained until settlement on the authorization flow.
const DefaultMaxBufferBytes = 1 << 20

// UsageFunc reports units consumed by a handler.
// Fixed-price routes ignore it. A later upto settlement can read it
// without a second rewrite of the resource server.
type UsageFunc func(ctx context.Context) (amount string, ok bool)

// ExtensionHook receives facilitator EXTENSION-RESPONSES.
// The map is not written to the buyer-facing receipt and is not logged.
type ExtensionHook func(responses map[string]interface{})

// Config holds the middleware configuration.
type Config struct {
	// Verifier is the payment verification backend (for example EVMVerifier).
	Verifier ChainVerifier

	// EndpointPricing maps URL patterns to pricing rules.
	// Patterns support exact matches ("/v1/endpoint") and wildcards ("/v1/*").
	// Used by HTTP middleware (grpc-gateway). The priced URL is resource.url.
	EndpointPricing map[string]PricingRule

	// MethodPricing maps gRPC method names to pricing rules.
	// Methods are full names like "/package.Service/Method".
	// Supports wildcards: "/package.Service/*" matches all methods in a service.
	MethodPricing map[string]PricingRule

	// DefaultPricing is used when no pattern matches.
	// If nil, unmatched endpoints do not require payment.
	DefaultPricing *PricingRule

	// ValidityDuration is how long an advertised payment window lasts.
	// Defaults to 5 minutes. A caller deadline shorter than this reduces
	// maxTimeoutSeconds on that challenge.
	ValidityDuration time.Duration

	// MaxBufferBytes caps the HTTP response held until settlement.
	// Defaults to 1 MiB. A route that can exceed it must set PaymentFlow upfront.
	MaxBufferBytes int

	// ReceiptStore remembers settled responses keyed by payment-identifier.
	// Nil selects an in-memory store of 1024 entries.
	ReceiptStore ReceiptStore

	// Usage is reserved for a later upto meter. Fixed-price routes do not call it.
	Usage UsageFunc

	// OnExtensionResponses is invoked with facilitator EXTENSION-RESPONSES.
	OnExtensionResponses ExtensionHook

	// SkipPaths lists paths that bypass payment checks entirely.
	SkipPaths []string

	// SkipMethods lists gRPC methods that bypass payment checks.
	SkipMethods []string

	// CustomPaywallHTML is optional HTML for browser requests.
	// The PAYMENT-REQUIRED header is still set.
	CustomPaywallHTML string
}

// PricingRule defines payment requirements for an endpoint or method.
type PricingRule struct {
	// AcceptedTokens lists the currencies accepted for this route.
	// Each token specifies its own Amount in atomic units.
	AcceptedTokens []TokenRequirement

	// Description explains what this payment is for.
	Description string

	// MimeType of the resource being sold.
	MimeType string

	// OutputSchema is retained for callers. It is not part of the v2
	// PaymentRequirements object. Bazaar carries response schemas.
	OutputSchema map[string]interface{}

	// PaymentFlow is authorization, upfront, or escrow.
	// The empty string means authorization.
	PaymentFlow string

	// ResourceURL overrides the absolute URL advertised in resource.url.
	ResourceURL string

	// ServiceName, Tags, and IconURL are optional resource fields.
	ServiceName string
	Tags        []string
	IconURL     string

	// Extensions are advertised on the challenge. A client may omit a key
	// or add fields. It must not drop or change a server-supplied info value.
	Extensions map[string]interface{}
}

// TokenRequirement specifies one payment option.
type TokenRequirement struct {
	// Network is the blockchain network in CAIP-2 form (for example "eip155:8453").
	Network string

	// AssetContract is the token contract address.
	AssetContract string

	// Symbol is the token symbol (for example "USDC").
	// It is exposed to handlers and logs. It is not written as extra.name.
	Symbol string

	// Recipient is the address that receives payment.
	Recipient string

	// Amount is the price in atomic units.
	Amount string

	// TokenName is a human-readable name. It is the EIP-712 domain name
	// when EIP712Name is empty.
	TokenName string

	// TokenDecimals is the number of decimals for this token.
	TokenDecimals int

	// Scheme defaults to exact.
	Scheme string

	// EIP712Name is the EIP-712 domain name. For Base USDC this is "USD Coin",
	// not the symbol "USDC".
	EIP712Name string

	// EIP712Version is the EIP-712 domain version. For Base USDC this is "2".
	// It is required for exact EIP-3009 and is never defaulted.
	EIP712Version string

	// AssetTransferMethod is omitted from extra when empty or eip3009.
	// Other methods (permit2, erc7710) are written to extra.
	AssetTransferMethod string
}

// SchemeName returns the advertised scheme. The empty value means exact.
func (t TokenRequirement) SchemeName() string {
	if t.Scheme == "" {
		return "exact"
	}
	return t.Scheme
}

// DomainName returns the EIP-712 domain name.
func (t TokenRequirement) DomainName() string {
	if t.EIP712Name != "" {
		return t.EIP712Name
	}
	return t.TokenName
}

// Flow returns the route payment flow. The empty value means authorization.
func (p PricingRule) Flow() string {
	if p.PaymentFlow == "" {
		return FlowAuthorization
	}
	return p.PaymentFlow
}

// Validate checks the configuration and fills defaults.
func (c *Config) Validate() error {
	if c.Verifier == nil {
		return fmt.Errorf("verifier is required")
	}

	if c.ValidityDuration == 0 {
		c.ValidityDuration = 5 * time.Minute
	}

	if c.MaxBufferBytes <= 0 {
		c.MaxBufferBytes = DefaultMaxBufferBytes
	}

	for pattern, rule := range c.EndpointPricing {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("invalid pricing rule for pattern %q: %w", pattern, err)
		}
	}

	for method, rule := range c.MethodPricing {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("invalid pricing rule for method %q: %w", method, err)
		}
	}

	if c.DefaultPricing != nil {
		if err := c.DefaultPricing.Validate(); err != nil {
			return fmt.Errorf("invalid default pricing rule: %w", err)
		}
	}

	return nil
}

// Validate checks the pricing rule.
func (p *PricingRule) Validate() error {
	switch p.Flow() {
	case FlowAuthorization, FlowUpfront, FlowEscrow:
	default:
		return fmt.Errorf("unsupported payment flow %q", p.PaymentFlow)
	}

	if len(p.AcceptedTokens) == 0 {
		return fmt.Errorf("at least one accepted token is required")
	}

	for i, token := range p.AcceptedTokens {
		if err := token.Validate(); err != nil {
			return fmt.Errorf("invalid token requirement at index %d: %w", i, err)
		}
	}

	return nil
}

// Validate checks the token requirement.
// exact with EIP-3009 (the default transfer method) requires an EIP-712
// domain name and version. The version is not defaulted to "2".
func (t *TokenRequirement) Validate() error {
	if t.Network == "" {
		return fmt.Errorf("network is required")
	}

	if t.Symbol == "" {
		return fmt.Errorf("symbol is required")
	}

	if t.Recipient == "" {
		return fmt.Errorf("recipient is required")
	}

	if t.AssetContract == "" {
		return fmt.Errorf("asset contract is required")
	}

	if t.Amount == "" {
		return fmt.Errorf("amount is required")
	}

	if t.requiresEIP712() {
		if t.DomainName() == "" {
			return fmt.Errorf("eip712 domain name is required for exact eip3009 (set EIP712Name or TokenName)")
		}
		if t.EIP712Version == "" {
			return fmt.Errorf("eip712 version is required for exact eip3009")
		}
	}

	return nil
}

func (t TokenRequirement) requiresEIP712() bool {
	if t.SchemeName() != "exact" {
		return false
	}
	method := strings.ToLower(t.AssetTransferMethod)
	return method == "" || method == "eip3009"
}

// MatchEndpoint finds the pricing rule for an HTTP path.
// Skip paths win, then an exact key, then the longest matching pattern,
// then default pricing.
func (c *Config) MatchEndpoint(requestPath string) (*PricingRule, bool) {
	return matchRule(requestPath, c.SkipPaths, c.EndpointPricing, c.DefaultPricing)
}

// MatchMethod finds the pricing rule for a gRPC full method name.
func (c *Config) MatchMethod(fullMethod string) (*PricingRule, bool) {
	return matchRule(fullMethod, c.SkipMethods, c.MethodPricing, c.DefaultPricing)
}

func matchRule(key string, skips []string, rules map[string]PricingRule, def *PricingRule) (*PricingRule, bool) {
	for _, skip := range skips {
		if matchPath(key, skip) {
			return nil, false
		}
	}

	if rule, ok := rules[key]; ok {
		return &rule, true
	}

	var bestMatch string
	var bestRule *PricingRule

	for pattern, rule := range rules {
		if matchPath(key, pattern) {
			if len(pattern) > len(bestMatch) {
				bestMatch = pattern
				ruleCopy := rule
				bestRule = &ruleCopy
			}
		}
	}

	if bestRule != nil {
		return bestRule, true
	}

	if def != nil {
		return def, true
	}

	return nil, false
}

func matchPath(requestPath, pattern string) bool {
	if requestPath == pattern {
		return true
	}

	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(requestPath, prefix+"/") || requestPath == prefix
	}

	matched, _ := path.Match(pattern, requestPath)
	return matched
}
