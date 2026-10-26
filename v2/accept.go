package x402

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// BuildAccepts returns the PaymentRequirements advertised for a rule.
// validity is the route window. Callers that also have a deadline pass the
// shorter of the two.
func BuildAccepts(rule *PricingRule, validity time.Duration) []PaymentRequirements {
	if rule == nil {
		return nil
	}
	secs := int(validity.Seconds())
	if secs < 0 {
		secs = 0
	}
	flow := rule.Flow()
	accepts := make([]PaymentRequirements, 0, len(rule.AcceptedTokens))
	for _, token := range rule.AcceptedTokens {
		accepts = append(accepts, PaymentRequirements{
			Scheme:            token.SchemeName(),
			Network:           token.Network,
			Amount:            token.Amount,
			Asset:             token.AssetContract,
			PayTo:             token.Recipient,
			MaxTimeoutSeconds: secs,
			Extra:             tokenExtra(token, flow),
		})
	}
	return accepts
}

func tokenExtra(token TokenRequirement, flow string) map[string]interface{} {
	extra := make(map[string]interface{})
	if token.TokenName != "" {
		extra["name"] = token.TokenName
	}
	if token.TokenVersion != "" {
		extra["version"] = token.TokenVersion
	}
	method := token.AssetTransferMethod
	if method != "" && !strings.EqualFold(method, "eip3009") {
		extra["assetTransferMethod"] = method
	}
	if flow != "" && flow != FlowAuthorization {
		extra["paymentFlow"] = flow
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

func resourceInfo(rule *PricingRule, rawURL string) *ResourceInfo {
	if rule != nil && rule.ResourceURL != "" {
		rawURL = rule.ResourceURL
	}
	info := &ResourceInfo{URL: rawURL}
	if rule == nil {
		return info
	}
	info.Description = rule.Description
	info.MimeType = rule.MimeType
	info.ServiceName = rule.ServiceName
	info.Tags = rule.Tags
	info.IconURL = rule.IconURL
	return info
}

// MatchClientToken returns the advertised option selected by the client.
// validity is the window used to build maxTimeoutSeconds. A mismatch,
// including maxTimeoutSeconds, returns nil. There is no fallthrough to the
// first token.
func MatchClientToken(rule *PricingRule, payload *PaymentPayload, validity time.Duration) (*PaymentRequirements, string) {
	if rule == nil || payload == nil {
		return nil, ""
	}
	accepts := BuildAccepts(rule, validity)
	for i := range accepts {
		if requirementsMatch(&accepts[i], &payload.Accepted) {
			return &accepts[i], rule.AcceptedTokens[i].Symbol
		}
	}
	return nil, ""
}

func requirementsMatch(server, client *PaymentRequirements) bool {
	if server.Scheme != client.Scheme || server.Network != client.Network {
		return false
	}
	if server.Amount != client.Amount || server.MaxTimeoutSeconds != client.MaxTimeoutSeconds {
		return false
	}
	if !strings.EqualFold(server.Asset, client.Asset) || !strings.EqualFold(server.PayTo, client.PayTo) {
		return false
	}
	return extraSubset(server.Extra, client.Extra)
}

func extraSubset(server, client map[string]interface{}) bool {
	if len(server) == 0 {
		return true
	}
	if client == nil {
		return false
	}
	for key, want := range server {
		got, ok := client[key]
		if !ok || !reflect.DeepEqual(canonicalJSON(want), canonicalJSON(got)) {
			return false
		}
	}
	return true
}

// extensionsPreserved reports whether the client kept every server-supplied
// extension info value. A missing extension key is allowed. Extra client
// fields are allowed, including fields added inside info. A present key with
// a changed or missing info value is not. Arrays and scalars must match exactly.
func extensionsPreserved(server, client map[string]interface{}) bool {
	for key, serverVal := range server {
		clientVal, ok := client[key]
		if !ok {
			continue
		}
		if !extensionValuePreserved(serverVal, clientVal) {
			return false
		}
	}
	return true
}

func extensionValuePreserved(serverVal, clientVal interface{}) bool {
	serverMap, serverOK := serverVal.(map[string]interface{})
	clientMap, clientOK := clientVal.(map[string]interface{})
	if !serverOK || !clientOK {
		return reflect.DeepEqual(serverVal, clientVal)
	}
	info, hasInfo := serverMap["info"]
	if !hasInfo {
		return true
	}
	clientInfo, ok := clientMap["info"]
	if !ok {
		return false
	}
	return valueSubset(info, clientInfo)
}

// valueSubset reports whether client keeps every value server supplied.
// Maps may gain keys. Arrays and scalars must be equal.
func valueSubset(server, client interface{}) bool {
	server = canonicalJSON(server)
	client = canonicalJSON(client)
	serverMap, serverOK := server.(map[string]interface{})
	clientMap, clientOK := client.(map[string]interface{})
	if serverOK && clientOK {
		for key, want := range serverMap {
			got, ok := clientMap[key]
			if !ok || !valueSubset(want, got) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(server, client)
}

func canonicalJSON(v interface{}) interface{} {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

func resolveFlow(routeFlow string, extra map[string]interface{}) (string, bool) {
	if extra == nil {
		return routeFlow, true
	}
	raw, ok := extra["paymentFlow"]
	if !ok {
		return routeFlow, true
	}
	flow, ok := raw.(string)
	if !ok || flow == "" {
		return "", false
	}
	switch flow {
	case FlowAuthorization, FlowUpfront, FlowEscrow:
	default:
		return "", false
	}
	if flow != routeFlow {
		return "", false
	}
	return flow, true
}

var paymentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func paymentIdentifierRequired(extensions map[string]interface{}) bool {
	raw, ok := extensions[ExtensionPaymentIdentifier]
	if !ok || raw == nil {
		return false
	}
	ext, ok := raw.(map[string]interface{})
	if !ok {
		return false
	}
	info, ok := ext["info"].(map[string]interface{})
	if !ok {
		return false
	}
	required, ok := info["required"].(bool)
	return ok && required
}

func validPaymentID(id string) bool {
	return paymentIDPattern.MatchString(id)
}

func paymentIdentifierError(rule *PricingRule, payload *PaymentPayload) string {
	id := paymentIdentifier(payload)
	if id == "" {
		if rule != nil && paymentIdentifierRequired(rule.Extensions) {
			return "payment_identifier_required"
		}
		return ""
	}
	if !validPaymentID(id) {
		return "invalid_payment_identifier"
	}
	return ""
}

func paymentFingerprint(payload *PaymentPayload, routeKey, method string) string {
	if payload == nil {
		return ""
	}
	raw, err := json.Marshal(struct {
		X402Version int                    `json:"x402Version"`
		Resource    *ResourceInfo          `json:"resource,omitempty"`
		Accepted    PaymentRequirements    `json:"accepted"`
		Payload     interface{}            `json:"payload,omitempty"`
		Extensions  map[string]interface{} `json:"extensions,omitempty"`
		Route       string                 `json:"route"`
		Method      string                 `json:"method"`
	}{
		X402Version: payload.X402Version,
		Resource:    payload.Resource,
		Accepted:    payload.Accepted,
		Payload:     payload.Payload,
		Extensions:  payload.Extensions,
		Route:       routeKey,
		Method:      method,
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

func paymentIdentifier(payload *PaymentPayload) string {
	if payload == nil || payload.Extensions == nil {
		return ""
	}
	raw, ok := payload.Extensions[ExtensionPaymentIdentifier]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case map[string]interface{}:
		if id, ok := v["id"].(string); ok && id != "" {
			return id
		}
		switch info := v["info"].(type) {
		case string:
			return info
		case map[string]interface{}:
			if id, ok := info["id"].(string); ok {
				return id
			}
		}
	}
	return ""
}

func paymentResponseFromSettlement(result *SettlementResult, fallbackAmount, fallbackNetwork string) PaymentResponse {
	resp := PaymentResponse{
		Transaction: "",
		Network:     fallbackNetwork,
	}
	if result == nil {
		resp.ErrorReason = "settlement_failed"
		return resp
	}
	resp.Success = result.Success
	resp.Transaction = result.TransactionHash
	resp.ErrorReason = result.ErrorReason
	resp.Payer = result.PayerAddress
	resp.Amount = result.Amount
	resp.Extensions = result.Extensions
	if result.Network != "" {
		resp.Network = result.Network
	}
	if resp.Amount == "" {
		resp.Amount = fallbackAmount
	}
	return resp
}
