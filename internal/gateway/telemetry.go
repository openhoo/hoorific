package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"hoorific/internal/core"
)

const (
	telemetryTracerName = "hoorific/gateway"
	telemetryMeterName  = "hoorific/gateway"

	genAIOperationName = "gen_ai.operation.name"
	genAIProviderName  = "gen_ai.provider.name"
	genAIRequestModel  = "gen_ai.request.model"
	genAIResponseModel = "gen_ai.response.model"
	genAIRequestStream = "gen_ai.request.stream"
	genAIInputTokens   = "gen_ai.usage.input_tokens"
	genAIOutputTokens  = "gen_ai.usage.output_tokens"
	genAICacheRead     = "gen_ai.usage.cache_read.input_tokens"
	genAICacheWrite    = "gen_ai.usage.cache_write.input_tokens"
	genAIReasoning     = "gen_ai.usage.reasoning.output_tokens"
	genAITokenType     = "gen_ai.token.type"
	genAIErrorType     = "error.type"

	gatewayRequestModelAttr = "hoorific.gateway.request.model"
	gatewayOutcomeAttr      = "hoorific.gateway.outcome"
	gatewayErrorTypeAttr    = "hoorific.gateway.error_type"
	gatewayRetryCountAttr   = "hoorific.gateway.retry_count"
	gatewayAttemptRetryAttr = "hoorific.gateway.retry"
	gatewayProtocolAttr     = "hoorific.gateway.protocol"
	gatewayFramingAttr      = "hoorific.gateway.framing"
	gatewayNativeAttr       = "hoorific.gateway.native"
	gatewayReconcileAttr    = "hoorific.gateway.resource_reconcile"
	gatewayCacheRequested   = "hoorific.gen_ai.cache.requested"
	gatewayCacheReadKnown   = "hoorific.gen_ai.cache_read.known"
	gatewayCacheWriteKnown  = "hoorific.gen_ai.cache_write.known"
	gatewayReasoningKnown   = "hoorific.gen_ai.reasoning.known"
	gatewayInputKnown       = "hoorific.gen_ai.input_tokens.known"
	gatewayOutputKnown      = "hoorific.gen_ai.output_tokens.known"
	gatewayTotalKnown       = "hoorific.gen_ai.total_tokens.known"
	gatewayCostKnown        = "hoorific.gen_ai.cost.known"
	gatewayCostUSD          = "hoorific.gen_ai.cost.usd"
	gatewayToolDefinitions  = "hoorific.gen_ai.tool_definitions.count"
	gatewayToolCalls        = "hoorific.gen_ai.tool_calls.count"
	gatewayFirstUseful      = "hoorific.gateway.first_useful_content"
)

var (
	gatewayTracer = otel.Tracer(telemetryTracerName)
	gatewayMeter  = otel.Meter(telemetryMeterName)

	gatewayOperationDuration = newGatewayDurationHistogram()
	gatewayTokenUsage        = newGatewayTokenHistogram()
	gatewayFirstChunk        = newGatewayFirstChunkHistogram()
)

func newGatewayDurationHistogram() metric.Float64Histogram {
	h, _ := gatewayMeter.Float64Histogram("gen_ai.client.operation.duration", metric.WithUnit("s"), metric.WithDescription("Duration of a logical gateway generative AI operation."))
	return h
}

func newGatewayTokenHistogram() metric.Int64Histogram {
	h, _ := gatewayMeter.Int64Histogram("gen_ai.client.token.usage", metric.WithUnit("{token}"), metric.WithDescription("Known input and output token usage for a gateway generative AI operation."))
	return h
}

func newGatewayFirstChunkHistogram() metric.Float64Histogram {
	h, _ := gatewayMeter.Float64Histogram("gen_ai.client.operation.time_to_first_chunk", metric.WithUnit("s"), metric.WithDescription("Time from a streaming attempt to the first useful streamed content."))
	return h
}

type gatewayRequestTelemetryKey struct{}
type gatewayAttemptTelemetryKey struct{}

type gatewayRequestTelemetry struct {
	mu sync.Mutex

	span    trace.Span
	started time.Time

	operation string
	provider  string
	outcome   string
	errorType string
	retries   int

	usage          *core.Usage
	responseModel  string
	firstUseful    float64
	hasFirstUseful bool
	toolCalls      int
	toolDefs       int
	cacheRequested bool
	costKnown      bool
	costUSD        float64
	ended          bool
}

type gatewayAttemptTelemetry struct {
	root           *gatewayRequestTelemetry
	span           trace.Span
	started        time.Time
	issued         time.Time
	operation      string
	provider       string
	model          string
	stream         bool
	usage          *core.Usage
	toolCalls      int
	responseModel  string
	firstUseful    float64
	hasFirstUseful bool
}

func gatewayStartRequest(r *http.Request, x route) (context.Context, *gatewayRequestTelemetry) {
	parent := r.Context()
	ctx, span := gatewayTracer.Start(parent, "gateway.request", trace.WithSpanKind(trace.SpanKindInternal))
	state := &gatewayRequestTelemetry{
		span:      span,
		started:   time.Now(),
		operation: telemetryOperation(x.operation, x.protocol),
		outcome:   "error",
		errorType: "request_rejected",
	}
	ctx = context.WithValue(ctx, gatewayRequestTelemetryKey{}, state)
	attrs := []attribute.KeyValue{
		attribute.String(genAIOperationName, state.operation),
		attribute.String(gatewayProtocolAttr, boundedProtocol(string(x.protocol))),
		attribute.Bool(gatewayNativeAttr, x.native),
	}
	if x.model != "" {
		attrs = append(attrs, attribute.String(gatewayRequestModelAttr, x.model))
	}
	span.SetAttributes(attrs...)
	return ctx, state
}

func gatewayStartAttempt(ctx context.Context, x route, binding core.Binding, operation core.Operation, model, connector string, stream bool, retry bool) (context.Context, *gatewayAttemptTelemetry) {
	state, _ := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry)
	provider := telemetryProvider(connector)
	kind := telemetryOperation(operation, binding.Codec.Protocol)
	attemptCtx, span := gatewayTracer.Start(ctx, "gateway.attempt", trace.WithSpanKind(trace.SpanKindClient))
	attempt := &gatewayAttemptTelemetry{root: state, span: span, started: time.Now(), operation: kind, provider: provider, model: model, stream: stream}
	attemptCtx = context.WithValue(attemptCtx, gatewayAttemptTelemetryKey{}, attempt)
	attrs := []attribute.KeyValue{
		attribute.String(genAIOperationName, kind),
		attribute.Bool(genAIRequestStream, stream),
		attribute.Bool(gatewayAttemptRetryAttr, retry),
		attribute.String(gatewayProtocolAttr, boundedProtocol(string(binding.Codec.Protocol))),
		attribute.String(gatewayFramingAttr, boundedFraming(string(binding.Framing))),
		attribute.Bool(gatewayNativeAttr, x.native),
	}
	if provider != "" {
		attrs = append(attrs, attribute.String(genAIProviderName, provider))
	}
	if model != "" {
		attrs = append(attrs, attribute.String(genAIRequestModel, model))
	}
	span.SetAttributes(attrs...)
	if state != nil {
		state.mu.Lock()
		if provider != "" {
			state.provider = provider
		}
		state.operation = kind
		state.mu.Unlock()
		state.span.SetAttributes(attribute.String(genAIOperationName, kind))
		if provider != "" {
			state.span.SetAttributes(attribute.String(genAIProviderName, provider))
		}
	}
	return attemptCtx, attempt
}

func gatewayRecordRetry(ctx context.Context) {
	if state, ok := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry); ok && state != nil {
		state.mu.Lock()
		state.retries++
		state.mu.Unlock()
	}
}

func gatewayIsRetry(ctx context.Context) bool {
	if state, ok := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry); ok && state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.retries > 0
	}
	return false
}
func gatewayMarkRequestSuccess(ctx context.Context) {
	if state, ok := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry); ok && state != nil {
		state.mu.Lock()
		state.outcome = "success"
		state.errorType = ""
		state.mu.Unlock()
	}
}
func gatewaySetRequestedModel(ctx context.Context, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if state, ok := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry); ok && state != nil {
		state.span.SetAttributes(attribute.String(gatewayRequestModelAttr, model))
	}
}

func gatewaySetRequestPayload(ctx context.Context, payload core.RequestPayload) {
	conversation, ok := payload.(core.Conversation)
	if !ok {
		return
	}
	state, _ := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry)
	attempt, _ := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry)
	attrs := make([]attribute.KeyValue, 0, 3)
	if conversation.Cache != nil {
		attrs = append(attrs, attribute.Bool(gatewayCacheRequested, true))
	}
	if len(conversation.Tools) > 0 {
		attrs = append(attrs, attribute.Int(gatewayToolDefinitions, len(conversation.Tools)))
	}
	if len(attrs) == 0 {
		return
	}
	if state != nil {
		state.mu.Lock()
		state.cacheRequested = conversation.Cache != nil
		state.toolDefs = len(conversation.Tools)
		state.mu.Unlock()
		state.span.SetAttributes(attrs...)
	}
	if attempt != nil {
		attempt.span.SetAttributes(attrs...)
	}
}

func gatewayMarkAttemptIssued(ctx context.Context) {
	if attempt, ok := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry); ok && attempt != nil {
		if attempt.issued.IsZero() {
			attempt.issued = time.Now()
		}
	}
}

func gatewayObserveUsage(ctx context.Context, usage *core.Usage) {
	if usage == nil {
		return
	}
	attempt, _ := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry)
	if attempt == nil {
		return
	}
	attempt.usage = cloneGatewayUsage(usage)
	gatewaySetUsageAttributes(attempt.span, usage)
}

func gatewayObserveResult(ctx context.Context, result core.ResultPayload) {
	if result == nil {
		return
	}
	var model string
	var usage *core.Usage
	switch value := result.(type) {
	case core.GenerationResult:
		model, usage = value.Model, value.Usage
		for _, block := range value.Blocks {
			if block.Kind == "tool_call" {
				gatewayObserveToolCall(ctx)
			}
		}
	case core.CompletionResult:
		model, usage = value.Model, value.Usage
	case core.EmbeddingResult:
		model, usage = value.Model, value.Usage
	case core.RerankResult:
		usage = value.Usage
	}
	if model != "" {
		gatewaySetResponseModel(ctx, model)
	}
	gatewayObserveUsage(ctx, usage)
}

func gatewayObserveToolCall(ctx context.Context) {
	attempt, _ := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry)
	if attempt == nil {
		return
	}
	attempt.toolCalls++
	attempt.span.SetAttributes(attribute.Int(gatewayToolCalls, attempt.toolCalls))
	if attempt.stream {
		gatewayMarkFirstUseful(ctx)
	}
}

func gatewayObserveEvent(ctx context.Context, event core.Event) {
	attempt, _ := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry)
	if attempt == nil {
		return
	}
	switch value := event.(type) {
	case core.Usage:
		gatewayObserveUsage(ctx, &value)
	case core.Start:
		if value.Model != "" {
			gatewaySetResponseModel(ctx, value.Model)
		}
	case core.ToolCallStart:
		gatewayObserveToolCall(ctx)
	case core.TextDelta:
		if value.Text != "" {
			gatewayMarkFirstUseful(ctx)
		}
	}
}

func gatewayMarkFirstUseful(ctx context.Context) {
	attempt, _ := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry)
	if attempt == nil || attempt.issued.IsZero() || attempt.hasFirstUseful {
		return
	}
	attempt.hasFirstUseful = true
	attempt.firstUseful = time.Since(attempt.issued).Seconds()
	if attempt.firstUseful < 0 {
		attempt.firstUseful = 0
	}
	attempt.span.SetAttributes(attribute.Float64("gen_ai.response.time_to_first_chunk", attempt.firstUseful), attribute.Bool(gatewayFirstUseful, true))
	if attempt.root != nil {
		attempt.root.mu.Lock()
		if !attempt.root.hasFirstUseful {
			attempt.root.hasFirstUseful = true
			attempt.root.firstUseful = attempt.firstUseful
		}
		attempt.root.mu.Unlock()
	}
}

func gatewaySetResponseModel(ctx context.Context, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if attempt, ok := ctx.Value(gatewayAttemptTelemetryKey{}).(*gatewayAttemptTelemetry); ok && attempt != nil {
		attempt.responseModel = model
		attempt.span.SetAttributes(attribute.String(genAIResponseModel, model))
	}
	if state, ok := ctx.Value(gatewayRequestTelemetryKey{}).(*gatewayRequestTelemetry); ok && state != nil {
		state.mu.Lock()
		state.responseModel = model
		state.mu.Unlock()
		state.span.SetAttributes(attribute.String(genAIResponseModel, model))
	}
}

func gatewayEndAttempt(ctx context.Context, attempt *gatewayAttemptTelemetry, outcome core.AttemptOutcome, returnErr error) {
	if attempt == nil {
		return
	}
	outcomeName, errorType := gatewayOutcome(outcome, returnErr)
	observedUsage := outcome.Usage
	if observedUsage == nil {
		observedUsage = attempt.usage
	}
	attrs := []attribute.KeyValue{
		attribute.String(gatewayOutcomeAttr, outcomeName),
		attribute.String(gatewayErrorTypeAttr, errorType),
	}
	if errorType != "" && outcomeName != "success" {
		attrs = append(attrs, attribute.String(genAIErrorType, errorType))
	}
	if outcome.Cancellation != "" {
		attrs = append(attrs, attribute.String("hoorific.gateway.cancellation", outcome.Cancellation))
	}
	attempt.span.SetAttributes(attrs...)
	gatewaySetUsageAttributes(attempt.span, observedUsage)
	if outcomeName == "success" || outcomeName == "job_pending" {
		attempt.span.SetStatus(codes.Ok, "")
	} else {
		attempt.span.SetStatus(codes.Error, "")
	}
	costKnown := outcome.ActualCost != nil && *outcome.ActualCost >= 0
	attempt.span.SetAttributes(attribute.Bool(gatewayCostKnown, costKnown))
	if costKnown {
		attempt.span.SetAttributes(attribute.Float64(gatewayCostUSD, float64(*outcome.ActualCost)/1e9))
	}
	if attempt.responseModel != "" {
		attempt.span.SetAttributes(attribute.String(genAIResponseModel, attempt.responseModel))
	}
	if attempt.root != nil {
		attempt.root.mu.Lock()
		attempt.root.outcome = outcomeName
		attempt.root.errorType = errorType
		if attempt.provider != "" {
			attempt.root.provider = attempt.provider
		}
		if attempt.toolCalls > 0 {
			attempt.root.toolCalls = attempt.toolCalls
		}
		if observedUsage != nil {
			attempt.root.usage = cloneGatewayUsage(observedUsage)
		}
		if outcome.ActualCost != nil && *outcome.ActualCost >= 0 {
			attempt.root.costKnown = true
			attempt.root.costUSD = float64(*outcome.ActualCost) / 1e9
		}
		attempt.root.mu.Unlock()
	}
	attempt.span.End()
}

func gatewayEndRequest(ctx context.Context, state *gatewayRequestTelemetry) {
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.ended {
		state.mu.Unlock()
		return
	}
	state.ended = true
	operation, provider, outcome, errorType := state.operation, state.provider, state.outcome, state.errorType
	retries, usage := state.retries, cloneGatewayUsage(state.usage)
	responseModel, firstUseful, hasFirstUseful := state.responseModel, state.firstUseful, state.hasFirstUseful
	toolDefs, toolCalls, cacheRequested := state.toolDefs, state.toolCalls, state.cacheRequested
	costKnown, costUSD := state.costKnown, state.costUSD
	started := state.started
	span := state.span
	state.mu.Unlock()

	attrs := []attribute.KeyValue{
		attribute.String(gatewayOutcomeAttr, outcome),
		attribute.String(gatewayRetryCountAttr, boundedRetryCount(retries)),
		attribute.Int(gatewayToolDefinitions, toolDefs),
		attribute.Int(gatewayToolCalls, toolCalls),
		attribute.Bool(gatewayCostKnown, costKnown),
	}
	if provider != "" {
		attrs = append(attrs, attribute.String(genAIProviderName, provider))
	}
	if errorType != "" && outcome != "success" {
		attrs = append(attrs, attribute.String(gatewayErrorTypeAttr, errorType), attribute.String(genAIErrorType, errorType))
	}
	if responseModel != "" {
		attrs = append(attrs, attribute.String(genAIResponseModel, responseModel))
	}
	if cacheRequested {
		attrs = append(attrs, attribute.Bool(gatewayCacheRequested, true))
	}
	if hasFirstUseful {
		attrs = append(attrs, attribute.Float64("gen_ai.response.time_to_first_chunk", firstUseful), attribute.Bool(gatewayFirstUseful, true))
	}
	if costKnown {
		attrs = append(attrs, attribute.Float64(gatewayCostUSD, costUSD))
	}
	gatewaySetUsageAttributes(span, usage)
	span.SetAttributes(attrs...)
	if outcome == "success" || outcome == "job_pending" {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, "")
	}

	duration := time.Since(started).Seconds()
	metricAttrs := gatewayMetricAttributes(operation, provider, outcome, errorType, "")
	if provider != "" {
		gatewayOperationDuration.Record(ctx, duration, metric.WithAttributes(metricAttrs...))
		if usage != nil {
			gatewayRecordTokenUsage(ctx, operation, provider, outcome, errorType, usage)
		}
		if hasFirstUseful {
			gatewayFirstChunk.Record(ctx, firstUseful, metric.WithAttributes(metricAttrs...))
		}
	}
	logAttrs := []any{
		slog.String(genAIOperationName, operation),
		slog.String(gatewayOutcomeAttr, outcome),
		slog.Int(gatewayRetryCountAttr, retries),
	}
	if provider != "" {
		logAttrs = append(logAttrs, slog.String(genAIProviderName, provider))
	}
	if errorType != "" && outcome != "success" {
		logAttrs = append(logAttrs, slog.String(gatewayErrorTypeAttr, errorType))
	}
	slog.InfoContext(ctx, "gateway completion", logAttrs...)
	span.End()
}
func gatewayEndReconcile(ctx context.Context, span trace.Span, operation, provider, outcome string, err error, usage *core.Usage) {
	if span == nil {
		return
	}
	errorType := gatewayErrorType(err)
	span.SetAttributes(
		attribute.String(genAIOperationName, telemetryOperation(core.Operation(operation), "")),
		attribute.String(genAIProviderName, boundedProvider(provider)),
		attribute.Bool(gatewayReconcileAttr, true),
		attribute.String(gatewayOutcomeAttr, boundedOutcome(outcome)),
	)
	if errorType != "" || (outcome != "success" && outcome != "pending" && outcome != "job_pending") {
		if errorType == "" {
			errorType = boundedOutcome(outcome)
		}
		span.SetAttributes(attribute.String(genAIErrorType, errorType), attribute.String(gatewayErrorTypeAttr, errorType))
		span.SetStatus(codes.Error, "")
	} else if outcome == "success" || outcome == "pending" || outcome == "job_pending" {
		span.SetStatus(codes.Ok, "")
	}
	gatewaySetUsageAttributes(span, usage)
	if outcome == "success" {
		slog.InfoContext(ctx, "gateway completion", slog.String(genAIOperationName, telemetryOperation(core.Operation(operation), "")), slog.String(genAIProviderName, boundedProvider(provider)), slog.String(gatewayOutcomeAttr, "success"), slog.Bool(gatewayReconcileAttr, true))
	}
	span.End()
}

func gatewayStartReconcile(ctx context.Context, operation core.Operation, provider string) (context.Context, trace.Span) {
	ctx, span := gatewayTracer.Start(ctx, "gateway.resource_reconcile.poll", trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(
		attribute.String(genAIOperationName, telemetryOperation(operation, "")),
		attribute.String(genAIProviderName, boundedProvider(provider)),
		attribute.Bool(gatewayReconcileAttr, true),
	)
	return ctx, span
}

func gatewayRecordStandaloneUsage(ctx context.Context, operation, provider, outcome string, usage *core.Usage) {
	if provider == "" || usage == nil {
		return
	}
	gatewayRecordTokenUsage(ctx, operation, provider, outcome, "", usage)
}

func gatewayRecordTokenUsage(ctx context.Context, operation, provider, outcome, errorType string, usage *core.Usage) {
	if usage == nil {
		return
	}
	attrs := gatewayMetricAttributes(operation, provider, outcome, errorType, "input")
	if value, ok := gatewayCount(usage.Input); ok {
		gatewayTokenUsage.Record(ctx, value, metric.WithAttributes(attrs...))
	}
	attrs = gatewayMetricAttributes(operation, provider, outcome, errorType, "output")
	if value, ok := gatewayCount(usage.Output); ok {
		gatewayTokenUsage.Record(ctx, value, metric.WithAttributes(attrs...))
	}
}

func gatewaySetUsageAttributes(span trace.Span, usage *core.Usage) {
	if span == nil {
		return
	}
	input, inputOK := gatewayCount(nil)
	output, outputOK := gatewayCount(nil)
	total, totalOK := gatewayCount(nil)
	cacheRead, cacheReadOK := gatewayCount(nil)
	cacheWrite, cacheWriteOK := gatewayCount(nil)
	reasoning, reasoningOK := gatewayCount(nil)
	if usage != nil {
		input, inputOK = gatewayCount(usage.Input)
		output, outputOK = gatewayCount(usage.Output)
		total, totalOK = gatewayCount(usage.Total)
		cacheRead, cacheReadOK = gatewayCount(usage.CachedInput)
		cacheWrite, cacheWriteOK = gatewayCount(usage.CacheWriteInput)
		reasoning, reasoningOK = gatewayCount(usage.ReasoningOutput)
	}
	attrs := []attribute.KeyValue{
		attribute.Bool(gatewayInputKnown, inputOK),
		attribute.Bool(gatewayOutputKnown, outputOK),
		attribute.Bool(gatewayTotalKnown, totalOK),
		attribute.Bool(gatewayCacheReadKnown, cacheReadOK),
		attribute.Bool(gatewayCacheWriteKnown, cacheWriteOK),
		attribute.Bool(gatewayReasoningKnown, reasoningOK),
	}
	if totalOK {
		attrs = append(attrs, attribute.Int64("hoorific.gen_ai.usage.total_tokens", total))
	}
	if inputOK {
		attrs = append(attrs, attribute.Int64(genAIInputTokens, input))
	}
	if outputOK {
		attrs = append(attrs, attribute.Int64(genAIOutputTokens, output))
	}
	if cacheReadOK {
		attrs = append(attrs, attribute.Int64(genAICacheRead, cacheRead))
	}
	if cacheWriteOK {
		attrs = append(attrs, attribute.Int64(genAICacheWrite, cacheWrite))
	}
	if reasoningOK {
		attrs = append(attrs, attribute.Int64(genAIReasoning, reasoning))
	}
	span.SetAttributes(attrs...)
}

func gatewayCount(value *int64) (int64, bool) {
	if value == nil || *value < 0 {
		return 0, false
	}
	return *value, true
}

func cloneGatewayUsage(value *core.Usage) *core.Usage {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func gatewayOutcome(outcome core.AttemptOutcome, err error) (string, string) {
	errorType := gatewayErrorType(err)
	if outcome.Cancellation != "" {
		if errorType == "" {
			errorType = "canceled"
		}
		return "cancelled", errorType
	}
	if errorType == "quota_exceeded" {
		return "rejected", errorType
	}
	switch outcome.State {
	case "settled", "job_pending":
		return "success", errorType
	case "not_executed":
		if errorType == "" {
			errorType = "not_executed"
		}
		return "rejected", errorType
	case "outcome_unknown":
		if errorType == "" {
			errorType = "upstream_outcome_unknown"
		}
		return "unknown", errorType
	default:
		if errorType == "" {
			errorType = "gateway_error"
		}
		return "error", errorType
	}
}

func gatewayErrorType(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	var ge core.GatewayError
	if errors.As(err, &ge) {
		switch ge.Code {
		case "quota_exceeded", "unauthorized", "forbidden", "invalid_request", "unsupported_operation", "unsupported_feature", "unsupported_policy", "unavailable", "upstream_error", "upstream_outcome_unknown", "configuration_stale", "model_not_found":
			return ge.Code
		default:
			return "gateway_error"
		}
	}
	return "gateway_error"
}

func gatewayMetricAttributes(operation, provider, outcome, errorType, tokenType string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(genAIOperationName, boundedOperation(operation)),
		attribute.String(genAIProviderName, boundedProvider(provider)),
		attribute.String(gatewayOutcomeAttr, boundedOutcome(outcome)),
	}
	if tokenType != "" {
		attrs = append(attrs, attribute.String(genAITokenType, tokenType))
	}
	if errorType != "" && outcome != "success" {
		attrs = append(attrs, attribute.String(genAIErrorType, boundedErrorType(errorType)))
	}
	return attrs
}

func telemetryOperation(operation core.Operation, protocol core.Protocol) string {
	switch strings.ToLower(string(operation)) {
	case "embed", "embeddings", "embedding":
		return "embeddings"
	case "complete", "completion", "text_completion":
		return "text_completion"
	case "generate", "realtime", "count_tokens":
		switch strings.ToLower(string(protocol)) {
		case "openai-chat", "openai-responses", "anthropic-messages", "openai-completion":
			if operation == "complete" || operation == "completion" {
				return "text_completion"
			}
			return "chat"
		default:
			return "generate_content"
		}
	default:
		return "other"
	}
}

func telemetryProvider(connector string) string {
	switch strings.ToLower(strings.TrimSpace(connector)) {
	case "openai", "openai-responses":
		return "openai"
	case "anthropic", "claude-subscription":
		return "anthropic"
	case "gemini", "gemini-cli", "antigravity":
		return "gcp.gen_ai"
	case "vertex", "vertex-ai", "vertex_ai":
		return "gcp.vertex_ai"
	case "bedrock":
		return "aws.bedrock"
	case "azure-openai", "azure-openai-legacy", "azure.ai.openai":
		return "azure.ai.openai"
	case "cohere":
		return "cohere"
	case "deepseek", "deepseek-anthropic":
		return "deepseek"
	case "groq":
		return "groq"
	case "mistral":
		return "mistral_ai"
	case "xai":
		return "x_ai"
	case "ollama":
		return "ollama"
	case "huggingface":
		return "huggingface"
	case "replicate":
		return "replicate"
	case "fal":
		return "fal"
	case "together":
		return "together"
	case "fireworks":
		return "fireworks_ai"
	case "openrouter":
		return "openrouter"
	case "cerebras":
		return "cerebras"
	case "codex-subscription":
		return "openai"
	case "kimi-subscription":
		return "moonshot_ai"
	default:
		return "other"
	}
}

func boundedProvider(value string) string {
	if value == "" {
		return "other"
	}
	return value
}
func boundedOperation(value string) string {
	switch value {
	case "chat", "text_completion", "embeddings", "generate_content", "other":
		return value
	default:
		return "other"
	}
}
func boundedOutcome(value string) string {
	switch value {
	case "success", "job_pending", "rejected", "unknown", "cancelled", "error":
		return value
	default:
		return "error"
	}
}
func boundedErrorType(value string) string {
	switch value {
	case "quota_exceeded", "unauthorized", "forbidden", "invalid_request", "unsupported_operation", "unsupported_feature", "unsupported_policy", "unavailable", "upstream_error", "upstream_outcome_unknown", "configuration_stale", "model_not_found", "canceled", "deadline_exceeded", "not_executed", "gateway_error":
		return value
	default:
		return "gateway_error"
	}
}
func boundedProtocol(value string) string {
	switch value {
	case "openai-chat", "openai-completion", "openai-responses", "anthropic-messages", "gemini-content", "bedrock-converse", "cohere-v2", "ollama", "native", "realtime":
		return value
	default:
		return "other"
	}
}
func boundedFraming(value string) string {
	switch value {
	case "json", "sse-data", "sse-named", "ndjson", "websocket", "h2-eventstream-duplex", "text/sdp":
		return value
	default:
		return "other"
	}
}
func boundedRetryCount(value int) string {
	if value <= 0 {
		return "0"
	}
	if value == 1 {
		return "1"
	}
	return "2+"
}

// gatewayTraceCarrier intentionally exposes only W3C trace context. A global
// composite propagator may include baggage; gateway requests must never carry
// baggage or caller credentials to a provider.
type gatewayTraceCarrier struct{ header http.Header }

func (c gatewayTraceCarrier) Get(key string) string {
	if !gatewayTraceKey(key) {
		return ""
	}
	return c.header.Get(key)
}
func (c gatewayTraceCarrier) Set(key, value string) {
	if gatewayTraceKey(key) {
		c.header.Set(key, value)
	}
}
func (gatewayTraceCarrier) Keys() []string { return []string{"traceparent", "tracestate"} }
func gatewayTraceKey(key string) bool {
	return strings.EqualFold(key, "traceparent") || strings.EqualFold(key, "tracestate")
}

func injectGatewayTrace(ctx context.Context, req *http.Request) {
	if req == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, gatewayTraceCarrier{header: req.Header})
	req.Header.Del("baggage")
}
func injectGatewayTraceHeaders(ctx context.Context, header http.Header) {
	if header == nil {
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, gatewayTraceCarrier{header: header})
	header.Del("baggage")
}

type gatewayTraceRoundTripper struct{ base http.RoundTripper }

func (t gatewayTraceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("nil HTTP request")
	}
	copy := req.Clone(req.Context())
	injectGatewayTrace(copy.Context(), copy)
	return t.base.RoundTrip(copy)
}

func gatewayTraceClient(client *http.Client) *http.Client {
	if client == nil {
		return nil
	}
	copy := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if _, wrapped := base.(gatewayTraceRoundTripper); !wrapped {
		copy.Transport = gatewayTraceRoundTripper{base: base}
	}
	return &copy
}
