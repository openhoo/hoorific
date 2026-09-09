package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"hoorific/internal/core"
)

func TestGatewayTelemetrySuccessRetryAndPrivacy(t *testing.T) {
	recorder, reader, restore := installGatewayTelemetryTestProviders()
	defer restore()

	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	ctx, request := gatewayStartRequest(r, route{protocol: "openai-chat", operation: "generate", model: "requested-model"})
	payload := core.Conversation{
		Model: "requested-model",
		Cache: &core.PromptCache{Key: "private-cache-key"},
		Tools: []core.Tool{{Name: "private-tool", Description: "private description", Schema: []byte(`{"type":"object"`)}},
	}
	gatewaySetRequestPayload(ctx, payload)

	binding := core.Binding{Codec: core.CodecKey{Protocol: "openai-chat", Operation: "generate"}, Framing: "json"}
	attemptCtx, attempt := gatewayStartAttempt(ctx, route{protocol: "openai-chat"}, binding, "generate", "requested-model", "openai", false, false)
	gatewayEndAttempt(attemptCtx, attempt, core.AttemptOutcome{State: "not_executed"}, core.GatewayError{Code: "quota_exceeded"})
	gatewayRecordRetry(ctx)

	attemptCtx, attempt = gatewayStartAttempt(ctx, route{protocol: "openai-chat"}, binding, "generate", "requested-model", "openai", false, true)
	inputTokens, outputTokens, cachedTokens, reasoningTokens := new(int64), new(int64), new(int64), new(int64)
	*inputTokens, *outputTokens, *cachedTokens, *reasoningTokens = 7, 3, 2, 1
	usage := core.Usage{Input: inputTokens, Output: outputTokens, CachedInput: cachedTokens, ReasoningOutput: reasoningTokens}
	gatewayObserveResult(attemptCtx, core.GenerationResult{Model: "actual-model", Blocks: []core.ContentBlock{{Kind: "tool_call", Name: "private-call", Arguments: "private args"}}, Usage: &usage})
	gatewayEndAttempt(attemptCtx, attempt, core.AttemptOutcome{State: "settled", Usage: &usage}, nil)
	gatewayEndRequest(ctx, request)

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("expected request plus two attempt spans, got %d", len(spans))
	}
	var root, client sdktrace.ReadOnlySpan
	for _, span := range spans {
		switch span.Name() {
		case "gateway.request":
			root = span
		case "gateway.attempt":
			if span.SpanKind() != trace.SpanKindClient {
				t.Errorf("attempt span kind = %v, want client", span.SpanKind())
			}
			client = span
		}
	}
	if root == nil || client == nil {
		t.Fatalf("missing root or client spans")
	}
	if value, ok := spanAttribute(root, gatewayRequestModelAttr); !ok || value.AsString() != "requested-model" {
		t.Fatalf("requested model attribute missing or changed: %v", value)
	}
	if value, ok := spanAttribute(root, genAIResponseModel); !ok || value.AsString() != "actual-model" {
		t.Fatalf("observed response model attribute missing or changed: %v", value)
	}
	if value, ok := spanAttribute(root, gatewayRetryCountAttr); !ok || value.AsString() != "1" {
		t.Fatalf("retry count missing or unbounded: %v", value)
	}
	if _, ok := spanAttribute(root, "gen_ai.input.messages"); ok {
		t.Fatal("prompt/message content was exported")
	}
	if _, ok := spanAttribute(root, gatewayToolDefinitions); !ok {
		t.Fatal("tool definition count was not recorded")
	}
	if _, ok := spanAttribute(root, "private-tool"); ok {
		t.Fatal("tool name was exported")
	}
	if value, ok := spanAttribute(root, gatewayToolCalls); !ok || value.AsInt64() != 1 {
		t.Fatalf("unary tool-call count missing: %v", value)
	}
	if value, ok := spanAttribute(client, genAICacheRead); !ok || value.AsInt64() != 2 {
		t.Fatalf("known cache usage missing: %v", value)
	}

	assertGatewayMetricPrivacy(t, reader, true)
}

func TestGatewayTelemetryStreamingUsefulContentAndUnknownUsage(t *testing.T) {
	recorder, reader, restore := installGatewayTelemetryTestProviders()
	defer restore()

	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", nil)
	ctx, request := gatewayStartRequest(r, route{protocol: "openai-chat", operation: "generate"})
	binding := core.Binding{Codec: core.CodecKey{Protocol: "openai-chat", Operation: "generate"}, Framing: "sse-data"}
	attemptCtx, attempt := gatewayStartAttempt(ctx, route{protocol: "openai-chat"}, binding, "generate", "model", "openai", true, false)
	gatewayMarkAttemptIssued(attemptCtx)
	gatewayObserveEvent(attemptCtx, core.TextDelta{Text: "useful"})
	unknownTokens := new(int64)
	*unknownTokens = -1
	unknown := core.Usage{Input: unknownTokens}
	gatewayEndAttempt(attemptCtx, attempt, core.AttemptOutcome{State: "settled", Usage: &unknown}, nil)
	gatewayEndRequest(ctx, request)

	var streamed sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "gateway.attempt" {
			streamed = span
		}
	}
	if streamed == nil {
		t.Fatal("missing stream attempt span")
	}
	if _, ok := spanAttribute(streamed, "gen_ai.response.time_to_first_chunk"); !ok {
		t.Fatal("first useful streamed content timing was not recorded")
	}
	if value, ok := spanAttribute(streamed, gatewayInputKnown); !ok || value.AsBool() {
		t.Fatalf("negative usage was not marked unknown: %v", value)
	}
	if _, ok := spanAttribute(streamed, genAIInputTokens); ok {
		t.Fatal("negative input token count was exported")
	}
	assertGatewayMetricPrivacy(t, reader, false)
}

func installGatewayTelemetryTestProviders() (*tracetest.SpanRecorder, *sdkmetric.ManualReader, func()) {
	oldTracer := otel.GetTracerProvider()
	oldMeter := otel.GetMeterProvider()
	oldPropagator := otel.GetTextMapPropagator()
	oldGatewayTracer, oldGatewayMeter := gatewayTracer, gatewayMeter
	oldDuration, oldTokens, oldFirstChunk := gatewayOperationDuration, gatewayTokenUsage, gatewayFirstChunk
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	gatewayTracer = tracerProvider.Tracer(telemetryTracerName)
	gatewayMeter = meterProvider.Meter(telemetryMeterName)
	gatewayOperationDuration = newGatewayDurationHistogram()
	gatewayTokenUsage = newGatewayTokenHistogram()
	gatewayFirstChunk = newGatewayFirstChunkHistogram()
	return recorder, reader, func() {
		_ = tracerProvider.Shutdown(context.Background())
		_ = meterProvider.Shutdown(context.Background())
		otel.SetTracerProvider(oldTracer)
		otel.SetMeterProvider(oldMeter)
		otel.SetTextMapPropagator(oldPropagator)
		gatewayTracer, gatewayMeter = oldGatewayTracer, oldGatewayMeter
		gatewayOperationDuration, gatewayTokenUsage, gatewayFirstChunk = oldDuration, oldTokens, oldFirstChunk
	}
}

func spanAttribute(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	if span == nil {
		return attribute.Value{}, false
	}
	for _, value := range span.Attributes() {
		if string(value.Key) == key {
			return value.Value, true
		}
	}
	return attribute.Value{}, false
}

func assertGatewayMetricPrivacy(t *testing.T, reader *sdkmetric.ManualReader, wantTokens bool) {
	t.Helper()
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	seenDuration, seenTokens := false, false
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch metric.Name {
			case "gen_ai.client.operation.duration":
				seenDuration = true
			case "gen_ai.client.token.usage":
				seenTokens = true
			}
			if metric.Name != "gen_ai.client.operation.duration" && metric.Name != "gen_ai.client.token.usage" {
				continue
			}
			if histogram, ok := metric.Data.(metricdata.Histogram[int64]); ok {
				for _, point := range histogram.DataPoints {
					for _, value := range point.Attributes.ToSlice() {
						if value.Key == genAIRequestModel || value.Key == "private-tool" {
							t.Errorf("model or tool identifier was used as a metric dimension: %s", value.Key)
						}
					}
				}
			}
		}
	}
	if !seenDuration || seenTokens != wantTokens {
		t.Fatalf("expected duration and token metric presence %v, got duration=%v tokens=%v", wantTokens, seenDuration, seenTokens)
	}
}
