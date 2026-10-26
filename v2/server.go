package x402

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Outcome values from Server.Execute. Adapters map them to HTTP and gRPC.
const (
	OutcomeFree            = "free"
	OutcomePaymentRequired = "payment_required"
	OutcomeInvalid         = "invalid"
	OutcomeInternal        = "internal"
	OutcomeHandlerFailed   = "handler_failed"
	OutcomeSettled         = "settled"
	OutcomeSettleFailed    = "settle_failed"
	OutcomeReplay          = "replay"
	OutcomeBufferOverflow  = "buffer_overflow"
)

// ErrBufferOverflow is returned by an HTTP handler when the response exceeds MaxBufferBytes.
// Execute does not settle.
var ErrBufferOverflow = errors.New("x402: response exceeds MaxBufferBytes")

// HandlerOutput is what a protected handler produced.
// HTTP fills Body, Status, and Header. gRPC fills GRPC.
type HandlerOutput struct {
	GRPC    interface{}
	HasGRPC bool
	Body    []byte
	Status  int
	Header  map[string][]string
}

// Handler runs the protected call. A nil error means the call succeeded
// and, on the authorization flow, may be settled.
type Handler func(ctx context.Context) (HandlerOutput, error)

// Call is one priced request. RouteKey is an HTTP path or a gRPC full method.
type Call struct {
	RouteKey      string
	GRPC          bool
	ResourceURL   string
	Deadline      time.Duration
	PaymentHeader string
	Legacy        bool
}

// Result is the payment decision and the handler output an adapter should return.
type Result struct {
	Outcome   string
	Reason    string
	Challenge *PaymentRequiredResponse
	Receipt   *PaymentResponse
	Payment   *PaymentContext
	Legacy    bool
	Output    HandlerOutput
	Err       error
	// ExtensionResponses is the facilitator side channel, if any.
	ExtensionResponses map[string]interface{}
}

// Server is the resource server shared by the HTTP and gRPC adapters.
type Server struct {
	cfg       Config
	endpoints routeTable
	methods   routeTable
	inflight  sync.Map
}

// NewServer validates cfg and compiles the route tables.
func NewServer(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.ReceiptStore == nil {
		cfg.ReceiptStore = NewMemoryReceiptStore(1024)
	}
	return &Server{
		cfg:       cfg,
		endpoints: compileRoutes(cfg.SkipPaths, cfg.EndpointPricing, cfg.DefaultPricing),
		methods:   compileRoutes(cfg.SkipMethods, cfg.MethodPricing, cfg.DefaultPricing),
	}, nil
}

// Usage calls the optional upto meter. Fixed-price routes do not call it.
func (s *Server) Usage(ctx context.Context) (string, bool) {
	if s.cfg.Usage == nil {
		return "", false
	}
	return s.cfg.Usage(ctx)
}

type patternRule struct {
	pattern string
	rule    *PricingRule
}

type routeTable struct {
	skips    []string
	exact    map[string]*PricingRule
	patterns []patternRule
	def      *PricingRule
}

func compileRoutes(skips []string, rules map[string]PricingRule, def *PricingRule) routeTable {
	table := routeTable{
		skips: append([]string(nil), skips...),
		exact: make(map[string]*PricingRule),
	}
	if def != nil {
		copied := *def
		table.def = &copied
	}
	for pattern, rule := range rules {
		copied := rule
		if isExactPattern(pattern) {
			table.exact[pattern] = &copied
			continue
		}
		table.patterns = append(table.patterns, patternRule{pattern: pattern, rule: &copied})
	}
	sort.SliceStable(table.patterns, func(i, j int) bool {
		if len(table.patterns[i].pattern) == len(table.patterns[j].pattern) {
			return table.patterns[i].pattern > table.patterns[j].pattern
		}
		return len(table.patterns[i].pattern) > len(table.patterns[j].pattern)
	})
	return table
}

func isExactPattern(pattern string) bool {
	if len(pattern) == 0 {
		return false
	}
	for _, r := range pattern {
		switch r {
		case '*', '?', '[', '\\':
			return false
		}
	}
	return true
}

func (t routeTable) match(key string) (*PricingRule, bool) {
	for _, skip := range t.skips {
		if matchPath(key, skip) {
			return nil, false
		}
	}
	if rule, ok := t.exact[key]; ok {
		return rule, true
	}
	for _, pattern := range t.patterns {
		if matchPath(key, pattern.pattern) {
			return pattern.rule, true
		}
	}
	if t.def != nil {
		return t.def, true
	}
	return nil, false
}

func (s *Server) match(call Call) (*PricingRule, bool) {
	if call.GRPC {
		return s.methods.match(call.RouteKey)
	}
	return s.endpoints.match(call.RouteKey)
}

// Execute runs verify, the handler, and settle in the route's payment flow.
func (s *Server) Execute(ctx context.Context, call Call, handler Handler) Result {
	rule, priced := s.match(call)
	if !priced {
		out, err := handler(ctx)
		return Result{Outcome: OutcomeFree, Output: out, Err: err, Legacy: call.Legacy}
	}

	validity := s.timeout(call.Deadline)
	accepts := BuildAccepts(rule, validity)
	challenge := s.challenge(rule, call, "PAYMENT-SIGNATURE header is required", accepts)

	if call.PaymentHeader == "" {
		return Result{
			Outcome:   OutcomePaymentRequired,
			Reason:    "payment_required",
			Challenge: challenge,
			Legacy:    call.Legacy,
		}
	}

	requirements, payload, symbol, flow, failed := s.bindPayment(call, rule, accepts, validity)
	if failed.Outcome != "" {
		if failed.Challenge == nil && failed.Outcome == OutcomePaymentRequired {
			failed.Challenge = challenge
		}
		return failed
	}

	if id := paymentIdentifier(payload); id != "" {
		if rec, ok := s.cfg.ReceiptStore.Lookup(id); ok {
			return replayResult(rec, call.Legacy)
		}
		unlock := s.lockIdentifier(id)
		defer unlock()
		if rec, ok := s.cfg.ReceiptStore.Lookup(id); ok {
			return replayResult(rec, call.Legacy)
		}
		result := s.runFlow(ctx, call, handler, rule, payload, requirements, symbol, flow)
		if result.Outcome == OutcomeSettled {
			s.remember(id, result)
		}
		return result
	}

	return s.runFlow(ctx, call, handler, rule, payload, requirements, symbol, flow)
}

func (s *Server) bindPayment(call Call, rule *PricingRule, accepts []PaymentRequirements, validity time.Duration) (*PaymentRequirements, *PaymentPayload, string, string, Result) {
	if call.Legacy {
		// A V1 payload names no token, so it pays the route's first option.
		if len(rule.AcceptedTokens) == 0 || len(accepts) == 0 {
			return nil, nil, "", "", s.required(rule, call, accepts, "payment requirements mismatch")
		}
		req := accepts[0]
		payload, err := parseLegacyPayment(call.PaymentHeader, &req)
		if err != nil {
			return nil, nil, "", "", Result{Outcome: OutcomeInvalid, Reason: "invalid_payment", Err: err, Legacy: true}
		}
		return &accepts[0], payload, rule.AcceptedTokens[0].Symbol, rule.Flow(), Result{}
	}

	payload, err := parsePaymentPayload(call.PaymentHeader)
	if err != nil {
		return nil, nil, "", "", Result{Outcome: OutcomeInvalid, Reason: "invalid_payment", Err: err}
	}
	matched, symbol := MatchClientToken(rule, payload, validity)
	if matched == nil {
		return nil, nil, "", "", s.required(rule, call, accepts, "payment requirements mismatch")
	}
	if !extensionsPreserved(rule.Extensions, payload.Extensions) {
		return nil, nil, "", "", s.required(rule, call, accepts, "payment requirements mismatch")
	}
	flow, ok := resolveFlow(rule.Flow(), payload.Accepted.Extra)
	if !ok {
		return nil, nil, "", "", s.required(rule, call, accepts, "unsupported payment flow")
	}
	return matched, payload, symbol, flow, Result{}
}

func (s *Server) runFlow(ctx context.Context, call Call, handler Handler, rule *PricingRule, payload *PaymentPayload, requirements *PaymentRequirements, symbol, flow string) Result {
	switch flow {
	case FlowUpfront:
		return s.upfront(ctx, call, handler, payload, requirements, symbol)
	case FlowEscrow:
		return s.escrow(ctx, call, handler, payload, requirements, symbol)
	default:
		return s.authorization(ctx, call, handler, rule, payload, requirements, symbol)
	}
}

func (s *Server) authorization(ctx context.Context, call Call, handler Handler, rule *PricingRule, payload *PaymentPayload, requirements *PaymentRequirements, symbol string) Result {
	verified, result := s.verify(ctx, call, rule, payload, requirements, symbol)
	if result.Outcome != "" {
		return result
	}
	out, err := handler(verified.ctx)
	if err != nil {
		if errors.Is(err, ErrBufferOverflow) {
			return Result{Outcome: OutcomeBufferOverflow, Reason: "response_too_large", Err: err, Legacy: call.Legacy}
		}
		return Result{
			Outcome: OutcomeHandlerFailed,
			Reason:  "handler_failed",
			Payment: verified.payment,
			Output:  out,
			Err:     err,
			Legacy:  call.Legacy,
		}
	}
	if ctx.Err() != nil {
		return Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: ctx.Err(), Legacy: call.Legacy}
	}
	settled, err := s.cfg.Verifier.Settle(ctx, payload, requirements)
	if err != nil {
		return Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: err, Legacy: call.Legacy}
	}
	s.noteExtensions(settled.ExtensionResponses)
	receipt := paymentResponseFromSettlement(settled, requirements.Amount, requirements.Network)
	if !settled.Success {
		return Result{
			Outcome:            OutcomeSettleFailed,
			Reason:             reasonOr(settled.ErrorReason, "settlement_failed"),
			Receipt:            &receipt,
			Payment:            verified.payment,
			Legacy:             call.Legacy,
			ExtensionResponses: settled.ExtensionResponses,
		}
	}
	// Copy so the handler's context keeps an empty transaction hash.
	done := *verified.payment
	done.TransactionHash = settled.TransactionHash
	done.SettledAt = settled.SettledAt
	return Result{
		Outcome:            OutcomeSettled,
		Reason:             "ok",
		Receipt:            &receipt,
		Payment:            &done,
		Output:             out,
		Legacy:             call.Legacy,
		ExtensionResponses: settled.ExtensionResponses,
	}
}

func (s *Server) upfront(ctx context.Context, call Call, handler Handler, payload *PaymentPayload, requirements *PaymentRequirements, symbol string) Result {
	if ctx.Err() != nil {
		return Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: ctx.Err(), Legacy: call.Legacy}
	}
	settled, err := s.cfg.Verifier.Settle(ctx, payload, requirements)
	if err != nil {
		return Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: err, Legacy: call.Legacy}
	}
	s.noteExtensions(settled.ExtensionResponses)
	receipt := paymentResponseFromSettlement(settled, requirements.Amount, requirements.Network)
	if !settled.Success {
		return Result{
			Outcome:            OutcomeSettleFailed,
			Reason:             reasonOr(settled.ErrorReason, "settlement_failed"),
			Receipt:            &receipt,
			Legacy:             call.Legacy,
			ExtensionResponses: settled.ExtensionResponses,
		}
	}
	payment := paymentContext(requirements, symbol, settled.PayerAddress, requirements.Amount)
	payment.TransactionHash = settled.TransactionHash
	payment.SettledAt = settled.SettledAt
	payCtx := context.WithValue(ctx, PaymentContextKey, payment)
	out, err := handler(payCtx)
	result := Result{
		Outcome:            OutcomeSettled,
		Reason:             "ok",
		Receipt:            &receipt,
		Payment:            payment,
		Output:             out,
		Legacy:             call.Legacy,
		ExtensionResponses: settled.ExtensionResponses,
	}
	if err != nil {
		if errors.Is(err, ErrBufferOverflow) {
			return Result{Outcome: OutcomeBufferOverflow, Reason: "response_too_large", Err: err, Receipt: &receipt, Legacy: call.Legacy}
		}
		result.Outcome = OutcomeHandlerFailed
		result.Reason = "handler_failed"
		result.Err = err
	}
	return result
}

func (s *Server) escrow(ctx context.Context, call Call, handler Handler, payload *PaymentPayload, requirements *PaymentRequirements, symbol string) Result {
	first := s.upfront(ctx, call, handler, payload, requirements, symbol)
	if first.Outcome != OutcomeSettled {
		return first
	}
	if ctx.Err() != nil {
		return Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: ctx.Err(), Receipt: first.Receipt, Legacy: call.Legacy}
	}
	settled, err := s.cfg.Verifier.Settle(ctx, payload, requirements)
	if err != nil {
		return Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: err, Receipt: first.Receipt, Legacy: call.Legacy}
	}
	s.noteExtensions(settled.ExtensionResponses)
	receipt := paymentResponseFromSettlement(settled, requirements.Amount, requirements.Network)
	if !settled.Success {
		return Result{
			Outcome:            OutcomeSettleFailed,
			Reason:             reasonOr(settled.ErrorReason, "settlement_failed"),
			Receipt:            &receipt,
			Payment:            first.Payment,
			Legacy:             call.Legacy,
			ExtensionResponses: settled.ExtensionResponses,
		}
	}
	var done *PaymentContext
	if first.Payment != nil {
		copied := *first.Payment
		copied.TransactionHash = settled.TransactionHash
		copied.SettledAt = settled.SettledAt
		done = &copied
	}
	return Result{
		Outcome:            OutcomeSettled,
		Reason:             "ok",
		Receipt:            &receipt,
		Payment:            done,
		Output:             first.Output,
		Legacy:             call.Legacy,
		ExtensionResponses: settled.ExtensionResponses,
	}
}

type verifiedPayment struct {
	ctx     context.Context
	payment *PaymentContext
}

func (s *Server) verify(ctx context.Context, call Call, rule *PricingRule, payload *PaymentPayload, requirements *PaymentRequirements, symbol string) (verifiedPayment, Result) {
	if ctx.Err() != nil {
		return verifiedPayment{}, Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: ctx.Err(), Legacy: call.Legacy}
	}
	verified, err := s.cfg.Verifier.Verify(ctx, payload, requirements)
	if err != nil {
		return verifiedPayment{}, Result{Outcome: OutcomeInternal, Reason: "facilitator_error", Err: err, Legacy: call.Legacy}
	}
	if verified == nil || !verified.Valid {
		reason := "payment verification failed"
		if verified != nil && verified.Reason != "" {
			reason = verified.Reason
		}
		if verified != nil {
			s.noteExtensions(verified.ExtensionResponses)
		}
		accepts := BuildAccepts(rule, s.timeout(call.Deadline))
		return verifiedPayment{}, Result{
			Outcome:   OutcomePaymentRequired,
			Reason:    reason,
			Challenge: s.challenge(rule, call, reason, accepts),
			Legacy:    call.Legacy,
		}
	}
	s.noteExtensions(verified.ExtensionResponses)
	amount := verified.Amount
	if amount == "" {
		amount = requirements.Amount
	}
	if symbol == "" {
		symbol = verified.TokenSymbol
	}
	payer := verified.PayerAddress
	payment := paymentContext(requirements, symbol, payer, amount)
	return verifiedPayment{
		ctx:     context.WithValue(ctx, PaymentContextKey, payment),
		payment: payment,
	}, Result{}
}

func paymentContext(requirements *PaymentRequirements, symbol, payer, amount string) *PaymentContext {
	payment := &PaymentContext{
		Verified:     true,
		PayerAddress: payer,
		Amount:       amount,
		TokenSymbol:  symbol,
		Network:      requirements.Network,
	}
	return payment
}

func (s *Server) timeout(deadline time.Duration) time.Duration {
	validity := s.cfg.ValidityDuration
	if deadline > 0 && deadline < validity {
		return deadline
	}
	return validity
}

func (s *Server) challenge(rule *PricingRule, call Call, message string, accepts []PaymentRequirements) *PaymentRequiredResponse {
	var extensions map[string]interface{}
	if rule != nil && len(rule.Extensions) > 0 {
		extensions = rule.Extensions
	}
	return &PaymentRequiredResponse{
		X402Version: 2,
		Error:       message,
		Resource:    resourceInfo(rule, callURL(rule, call)),
		Accepts:     accepts,
		Extensions:  extensions,
	}
}

func callURL(rule *PricingRule, call Call) string {
	if rule != nil && rule.ResourceURL != "" {
		return rule.ResourceURL
	}
	if call.ResourceURL != "" {
		return call.ResourceURL
	}
	if call.GRPC {
		return "grpc://localhost" + call.RouteKey
	}
	return ""
}

func (s *Server) required(rule *PricingRule, call Call, accepts []PaymentRequirements, message string) Result {
	return Result{
		Outcome:   OutcomePaymentRequired,
		Reason:    "payment_required",
		Challenge: s.challenge(rule, call, message, accepts),
		Legacy:    call.Legacy,
	}
}

func (s *Server) noteExtensions(responses map[string]interface{}) {
	if len(responses) == 0 || s.cfg.OnExtensionResponses == nil {
		return
	}
	s.cfg.OnExtensionResponses(responses)
}

func (s *Server) remember(id string, result Result) {
	if result.Receipt == nil {
		return
	}
	rec := ReceiptRecord{
		Payment:    *result.Receipt,
		StatusCode: result.Output.Status,
		Body:       result.Output.Body,
		Header:     result.Output.Header,
		GRPCResp:   result.Output.GRPC,
		HasGRPC:    result.Output.HasGRPC,
	}
	if rec.StatusCode == 0 {
		rec.StatusCode = 200
	}
	_ = s.cfg.ReceiptStore.Store(id, rec, s.receiptTTL())
}

func (s *Server) receiptTTL() time.Duration {
	if s.cfg.ValidityDuration > 0 {
		return s.cfg.ValidityDuration
	}
	return 10 * time.Minute
}

func replayResult(rec ReceiptRecord, legacy bool) Result {
	receipt := rec.Payment
	payment := &PaymentContext{
		Verified:        true,
		PayerAddress:    receipt.Payer,
		Amount:          receipt.Amount,
		Network:         receipt.Network,
		TransactionHash: receipt.Transaction,
	}
	return Result{
		Outcome: OutcomeReplay,
		Reason:  "ok",
		Receipt: &receipt,
		Payment: payment,
		Legacy:  legacy,
		Output: HandlerOutput{
			GRPC:    rec.GRPCResp,
			HasGRPC: rec.HasGRPC,
			Body:    rec.Body,
			Status:  rec.StatusCode,
			Header:  rec.Header,
		},
	}
}

func (s *Server) lockIdentifier(id string) func() {
	mu, _ := s.inflight.LoadOrStore(id, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	return func() { lock.Unlock() }
}

func reasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}
