package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	logscollectorpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscollectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracescollectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const (
	telemetryServiceName    = "hoorific-verify"
	telemetryServiceVersion = "qualification"
	telemetryEnvironment    = "qualification"
	telemetryPrimaryPrefix  = "/telemetry-primary"
	telemetryMetricsPrefix  = "/telemetry-metrics"
	telemetryPrimaryAuth    = "Bearer verify-telemetry-primary"
	telemetryMetricsAuth    = "Bearer verify-telemetry-metrics"
	telemetryNormalCanary   = "telemetry-normal-private-canary-7b1e"
	telemetryStreamCanary   = "telemetry-stream-private-canary-2d9f"
	telemetryRetryCanary    = "telemetry-retry-private-canary-4a6c"
)

type telemetryReceived struct {
	Method            string
	Path              string
	Signal            string
	ContentType       string
	Authorization     string
	AuthorizationSize int
	Body              []byte
}

type telemetryCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	items  []telemetryReceived
}

func newTelemetryCollector() *telemetryCollector {
	collector := &telemetryCollector{}
	collector.server = httptest.NewServer(http.HandlerFunc(collector.receive))
	return collector
}

func (c *telemetryCollector) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	item := telemetryReceived{
		Method:            r.Method,
		Path:              r.URL.Path,
		Signal:            telemetrySignalFromPath(r.URL.Path),
		ContentType:       r.Header.Get("Content-Type"),
		Authorization:     r.Header.Get("Authorization"),
		AuthorizationSize: len(r.Header.Values("Authorization")),
		Body:              append([]byte(nil), body...),
	}
	c.mu.Lock()
	c.items = append(c.items, item)
	c.mu.Unlock()
	// The verifier deliberately accepts the HTTP envelope and performs strict
	// protobuf, routing, resource, and privacy assertions after shutdown. This
	// keeps exporter retries from masking the first malformed payload.
	w.WriteHeader(http.StatusOK)
}

func (c *telemetryCollector) snapshot() []telemetryReceived {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]telemetryReceived, len(c.items))
	for i, item := range c.items {
		out[i] = item
		out[i].Body = append([]byte(nil), item.Body...)
	}
	return out
}

func (c *telemetryCollector) counts() map[string]int {
	counts := map[string]int{"traces": 0, "metrics": 0, "logs": 0}
	for _, item := range c.snapshot() {
		if _, ok := counts[item.Signal]; ok {
			counts[item.Signal]++
		}
	}
	return counts
}

func telemetrySignalFromPath(path string) string {
	for _, signal := range []string{"traces", "metrics", "logs"} {
		if strings.HasSuffix(path, "/v1/"+signal) {
			return signal
		}
	}
	return ""
}

func telemetryDestination(path string) string {
	switch {
	case strings.HasPrefix(path, telemetryPrimaryPrefix+"/v1/"):
		return "primary"
	case strings.HasPrefix(path, telemetryMetricsPrefix+"/v1/"):
		return "metrics"
	default:
		return ""
	}
}

func telemetryExpectedAuthorization(destination string) string {
	if destination == "primary" {
		return telemetryPrimaryAuth
	}
	if destination == "metrics" {
		return telemetryMetricsAuth
	}
	return ""
}

type telemetryQualification struct {
	env       *environment
	collector *telemetryCollector
}

func newTelemetryQualification(mode string) (*telemetryQualification, error) {
	// newEnvironment owns the fixture, temporary directory, keyring, listeners,
	// and optional cluster resources. Telemetry only rewrites its private config;
	// it never creates a second application fixture or touches operator state.
	env, err := newEnvironment(mode, "", "normal")
	if err != nil {
		return nil, err
	}
	collector := newTelemetryCollector()
	qualification := &telemetryQualification{env: env, collector: collector}
	if err := qualification.configure(); err != nil {
		collector.server.Close()
		env.Close()
		return nil, err
	}
	return qualification, nil
}

func (t *telemetryQualification) configure() error {
	if t == nil || t.env == nil || t.collector == nil {
		return errors.New("telemetry qualification is not initialized")
	}
	raw, err := os.ReadFile(t.env.config)
	if err != nil {
		return fmt.Errorf("read isolated telemetry config: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode isolated telemetry config: %w", err)
	}
	primaryHeaders := filepath.Join(t.env.root, "telemetry-primary-headers.json")
	metricsHeaders := filepath.Join(t.env.root, "telemetry-metrics-headers.json")
	if err := os.WriteFile(primaryHeaders, []byte(`{"Authorization":"`+telemetryPrimaryAuth+`"}`+"\n"), 0600); err != nil {
		return fmt.Errorf("write isolated primary telemetry headers: %w", err)
	}
	if err := os.WriteFile(metricsHeaders, []byte(`{"Authorization":"`+telemetryMetricsAuth+`"}`+"\n"), 0600); err != nil {
		return fmt.Errorf("write isolated metrics telemetry headers: %w", err)
	}
	endpoint := t.collector.server.URL
	document["telemetry"] = map[string]any{
		"enabled":         true,
		"service_name":    telemetryServiceName,
		"service_version": telemetryServiceVersion,
		"environment":     telemetryEnvironment,
		"sample_ratio":    1,
		"exporters": []any{
			map[string]any{
				"name":         "verify-traces-logs",
				"signals":      []string{"traces", "logs"},
				"protocol":     "http/protobuf",
				"endpoint":     endpoint + telemetryPrimaryPrefix,
				"headers_file": primaryHeaders,
				"insecure":     true,
			},
			map[string]any{
				"name":         "verify-metrics",
				"signals":      []string{"metrics"},
				"protocol":     "http/protobuf",
				"endpoint":     endpoint + telemetryMetricsPrefix,
				"headers_file": metricsHeaders,
				"insecure":     true,
			},
		},
	}
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode isolated telemetry config: %w", err)
	}
	if err := os.WriteFile(t.env.config, updated, 0600); err != nil {
		return fmt.Errorf("write isolated telemetry config: %w", err)
	}
	return nil
}

func (t *telemetryQualification) closeCollector() {
	if t != nil && t.collector != nil && t.collector.server != nil {
		t.collector.server.Close()
	}
}

func (t *telemetryQualification) exercise(key string) []result {
	if t == nil || t.env == nil || t.collector == nil {
		return []result{{Name: "telemetry/setup", Status: "failed", Detail: "telemetry qualification is not initialized"}}
	}
	if key == "" {
		return []result{{Name: "telemetry/inference", Status: "failed", Detail: "isolated gateway key is required for authenticated telemetry requests"}}
	}
	out := make([]result, 0, 3)
	t.env.fixture.mode.Store("normal")

	normalStart := time.Now()
	normalBefore := t.env.fixture.count()
	normalStatus, normalBody, normalErr := telemetryInference(t.env, key, []byte(`{"model":"assistant","messages":[{"role":"user","content":"`+telemetryNormalCanary+`"}],"max_tokens":16}`))
	normalCalls := t.env.fixture.count() - normalBefore
	if normalErr == nil && (normalStatus != http.StatusOK || !bytes.Contains(normalBody, []byte("OK"))) {
		normalErr = fmt.Errorf("normal authenticated inference returned HTTP %d without fixture response", normalStatus)
	}
	if normalErr == nil && normalCalls != 1 {
		normalErr = fmt.Errorf("normal authenticated inference dispatched %d fixture calls, want 1", normalCalls)
	}
	if normalErr == nil {
		normalErr = telemetryAssertFixtureAuth(t.env.fixture, normalBefore, normalCalls)
	}
	out = append(out, telemetryResult("telemetry/inference-normal", normalStart, map[string]any{
		"status":                    normalStatus,
		"fixture_dispatches":        normalCalls,
		"authenticated_upstream":    normalErr == nil,
		"response_contains_fixture": bytes.Contains(normalBody, []byte("OK")),
	}, normalErr))

	streamStart := time.Now()
	streamBefore := t.env.fixture.count()
	streamStatus, streamBody, streamErr := telemetryInference(t.env, key, []byte(`{"model":"assistant","messages":[{"role":"user","content":"`+telemetryStreamCanary+`"}],"max_tokens":16,"stream":true}`))
	streamCalls := t.env.fixture.count() - streamBefore
	var streamText strings.Builder
	for _, line := range bytes.Split(streamBody, []byte("\n")) {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			continue
		}
		var event struct {
			Choices []struct {
				Delta struct{ Content string }
			}
		}
		if err := json.Unmarshal(data, &event); err != nil {
			streamErr = fmt.Errorf("decode fixture stream: %w", err)
			break
		}
		for _, choice := range event.Choices {
			streamText.WriteString(choice.Delta.Content)
		}
	}
	if streamErr == nil && (streamStatus != http.StatusOK || !bytes.Contains(streamBody, []byte("[DONE]")) || streamText.String() != "OK") {
		streamErr = fmt.Errorf("streaming authenticated inference returned HTTP %d without complete fixture stream", streamStatus)
	}
	if streamErr == nil && streamCalls != 1 {
		streamErr = fmt.Errorf("streaming authenticated inference dispatched %d fixture calls, want 1", streamCalls)
	}
	if streamErr == nil {
		streamErr = telemetryAssertFixtureAuth(t.env.fixture, streamBefore, streamCalls)
	}
	out = append(out, telemetryResult("telemetry/inference-stream", streamStart, map[string]any{
		"status":                    streamStatus,
		"fixture_dispatches":        streamCalls,
		"authenticated_upstream":    streamErr == nil,
		"terminal_marker":           bytes.Contains(streamBody, []byte("[DONE]")),
		"response_contains_fixture": streamText.String() == "OK",
	}, streamErr))

	retryStart := time.Now()
	t.env.fixture.mode.Store("429")
	retryBefore := t.env.fixture.count()
	retryStatus, _, retryErr := telemetryInference(t.env, key, []byte(`{"model":"assistant","messages":[{"role":"user","content":"`+telemetryRetryCanary+`"}],"max_tokens":16}`))
	retryCalls := t.env.fixture.count() - retryBefore
	t.env.fixture.mode.Store("normal")
	if retryErr == nil && retryStatus != http.StatusTooManyRequests && retryStatus != http.StatusBadGateway {
		retryErr = fmt.Errorf("retry fixture returned HTTP %d, want upstream 429 or gateway 502", retryStatus)
	}
	if retryErr == nil && retryCalls < 1 {
		retryErr = errors.New("retry fixture did not receive an authenticated upstream dispatch")
	}
	if retryErr == nil {
		retryErr = telemetryAssertFixtureAuth(t.env.fixture, retryBefore, retryCalls)
	}
	out = append(out, telemetryResult("telemetry/inference-retry", retryStart, map[string]any{
		"status":                 retryStatus,
		"fixture_dispatches":     retryCalls,
		"authenticated_upstream": retryErr == nil,
		"fixture_mode":           "429",
	}, retryErr))
	return out
}

func telemetryInference(e *environment, key string, body []byte) (int, []byte, error) {
	response, err := e.do("telemetry", key, body)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if readErr != nil {
		return response.StatusCode, payload, readErr
	}
	return response.StatusCode, payload, nil
}

func telemetryAssertFixtureAuth(f *fixture, before, calls int) error {
	if calls <= 0 {
		return errors.New("fixture authentication cannot be checked without an upstream call")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if before < 0 || before+calls > len(f.calls) {
		return errors.New("fixture call accounting changed while checking authentication")
	}
	for _, call := range f.calls[before : before+calls] {
		if call.CredentialHeader != "X-Api-Key" || call.CredentialCount != 1 || call.CredentialConflict || call.Credential != "fixture-upstream-secret" {
			return errors.New("gateway did not send exactly one configured upstream authentication header")
		}
	}
	return nil
}

func telemetryResult(name string, start time.Time, evidence any, err error) result {
	out := result{Name: name, Status: "passed", Detail: "observed actual gateway and fixture behavior", DurationMS: time.Since(start).Milliseconds(), Evidence: evidence}
	if err != nil {
		out.Status = "failed"
		out.Detail = err.Error()
	}
	return out
}

func runTelemetryQualification(mode, binary, output string) []result {
	qualification, err := newTelemetryQualification(mode)
	if err != nil {
		return []result{{Name: "telemetry/setup", Status: "failed", Detail: "could not create isolated telemetry harness: " + err.Error()}}
	}
	defer qualification.closeCollector()
	out := make([]result, 0, 9)
	add := func(r result) { out = append(out, r) }
	add(result{Name: "telemetry/isolation", Status: "passed", Detail: "telemetry child owns a temporary gateway, fixture, headers, and loopback collector", Evidence: map[string]any{"directory": qualification.env.root, "collector": true}})
	if err := qualification.env.Start(context.Background(), binary); err != nil {
		detail := "telemetry gateway failed to start: " + err.Error()
		if output != "" {
			if diagnostics, diagnosticsErr := qualification.env.preserveDiagnostics(output); diagnosticsErr == nil {
				detail += "; diagnostics=" + diagnostics
			}
		}
		qualification.env.Close()
		add(result{Name: "telemetry/setup", Status: "failed", Detail: detail})
		return out
	}
	add(qualification.env.health())
	bootstrap := qualification.env.bootstrap()
	add(bootstrap)
	if qualification.env.key == "" {
		add(result{Name: "telemetry/inference", Status: "failed", Detail: "automatic fixture seeding did not issue an inference key"})
	} else {
		for _, r := range qualification.exercise(qualification.env.key) {
			add(r)
		}
	}
	qualification.env.Close()
	add(qualification.shutdownResult())
	return out
}

func (t *telemetryQualification) shutdownResult() result {
	start := time.Now()
	evidence := map[string]any{}
	if t == nil || t.env == nil || t.collector == nil {
		return telemetryResult("telemetry/shutdown", start, evidence, errors.New("telemetry qualification is not initialized at shutdown"))
	}
	state := t.env.server != nil && t.env.server.ProcessState != nil
	success := state && t.env.server.ProcessState.Success()
	evidence["gateway_exited"] = state
	evidence["gateway_exit_success"] = success
	if !state || !success {
		return telemetryResult("telemetry/shutdown", start, evidence, errors.New("gateway did not exit successfully after graceful shutdown"))
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		counts := t.collector.counts()
		if counts["traces"] > 0 && counts["metrics"] > 0 && counts["logs"] > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	decoded, err := t.decodeAndValidate()
	if err != nil {
		return telemetryResult("telemetry/shutdown", start, evidence, err)
	}
	for signal, count := range decoded.signalRequests {
		evidence[signal+"_requests"] = count
	}
	evidence["destinations"] = decoded.destinations
	evidence["server_roots"] = decoded.serverRoots
	evidence["gateway_request_spans"] = decoded.gatewayRequestSpans
	evidence["gen_ai_attempt_spans"] = decoded.genAIAttemptSpans
	evidence["stream_true_attempts"] = decoded.streamTrueAttempts
	evidence["stream_false_attempts"] = decoded.streamFalseAttempts
	evidence["parent_links"] = decoded.parentLinks
	evidence["token_output_sum"] = decoded.tokenOutputSum
	evidence["resource_attributes_verified"] = true
	evidence["authorization_headers_verified"] = true
	evidence["privacy_canaries_absent"] = true
	evidence["shutdown_flush_observed"] = true
	return telemetryResult("telemetry/shutdown", start, evidence, nil)
}

type telemetrySpanRecord struct {
	span     *tracepb.Span
	resource map[string]string
}

type telemetryMetricRecord struct {
	metric   *metricspb.Metric
	attrs    map[string]string
	resource map[string]string
	count    uint64
	sum      float64
}

type telemetryLogRecord struct {
	log      *logspb.LogRecord
	resource map[string]string
}

type telemetryDecoded struct {
	spans               []telemetrySpanRecord
	metrics             []telemetryMetricRecord
	logs                []telemetryLogRecord
	signalRequests      map[string]int
	destinations        map[string]int
	serverRoots         int
	gatewayRequestSpans int
	genAIAttemptSpans   int
	streamTrueAttempts  int
	streamFalseAttempts int
	parentLinks         int
	correlatedLogs      int
	tokenInputSum       float64
	tokenOutputSum      float64
}

func (t *telemetryQualification) decodeAndValidate() (telemetryDecoded, error) {
	decoded := telemetryDecoded{signalRequests: map[string]int{}, destinations: map[string]int{}}
	items := t.collector.snapshot()
	if len(items) == 0 {
		return decoded, errors.New("OTLP collector received no export requests after gateway shutdown")
	}
	for index, item := range items {
		if item.Method != http.MethodPost {
			return decoded, fmt.Errorf("collector request %d used %s instead of POST", index, item.Method)
		}
		if item.Signal == "" {
			return decoded, fmt.Errorf("collector request %d used an unknown OTLP path", index)
		}
		destination := telemetryDestination(item.Path)
		expectedAuth := telemetryExpectedAuthorization(destination)
		if destination == "" || expectedAuth == "" {
			return decoded, fmt.Errorf("collector request %d used an unconfigured destination path", index)
		}
		if (destination == "primary" && item.Signal == "metrics") || (destination == "metrics" && item.Signal != "metrics") {
			return decoded, fmt.Errorf("collector request %d routed %s to the wrong configured destination", index, item.Signal)
		}
		if !strings.HasPrefix(strings.ToLower(item.ContentType), "application/x-protobuf") {
			return decoded, fmt.Errorf("collector request %d for %s did not use application/x-protobuf", index, item.Signal)
		}
		if item.AuthorizationSize != 1 || item.Authorization != expectedAuth {
			return decoded, fmt.Errorf("collector request %d for %s did not carry its configured authorization header exactly once", index, item.Signal)
		}
		if len(item.Body) == 0 {
			return decoded, fmt.Errorf("collector request %d for %s had an empty protobuf body", index, item.Signal)
		}
		for _, canary := range []string{telemetryNormalCanary, telemetryStreamCanary, telemetryRetryCanary, telemetryPrimaryAuth, telemetryMetricsAuth, "fixture-upstream-secret", "verify-replay-secret", t.env.key, t.env.cookie, t.env.csrf} {
			if canary != "" && bytes.Contains(item.Body, []byte(canary)) {
				return decoded, fmt.Errorf("privacy canary appeared in exported %s payload", item.Signal)
			}
		}
		decoded.signalRequests[item.Signal]++
		decoded.destinations[destination]++
		switch item.Signal {
		case "traces":
			var envelope tracescollectorpb.ExportTraceServiceRequest
			if err := proto.Unmarshal(item.Body, &envelope); err != nil {
				return decoded, fmt.Errorf("decode exported traces request %d: %w", index, err)
			}
			for _, resourceSpans := range envelope.GetResourceSpans() {
				attrs := telemetryResourceAttributes(resourceSpans.GetResource())
				for _, scopeSpans := range resourceSpans.GetScopeSpans() {
					for _, span := range scopeSpans.GetSpans() {
						decoded.spans = append(decoded.spans, telemetrySpanRecord{span: span, resource: attrs})
					}
				}
			}
		case "metrics":
			var envelope metricscollectorpb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(item.Body, &envelope); err != nil {
				return decoded, fmt.Errorf("decode exported metrics request %d: %w", index, err)
			}
			for _, resourceMetrics := range envelope.GetResourceMetrics() {
				attrs := telemetryResourceAttributes(resourceMetrics.GetResource())
				for _, scopeMetrics := range resourceMetrics.GetScopeMetrics() {
					for _, metric := range scopeMetrics.GetMetrics() {
						histogram := metric.GetHistogram()
						if histogram == nil {
							decoded.metrics = append(decoded.metrics, telemetryMetricRecord{metric: metric, resource: attrs})
							continue
						}
						for _, point := range histogram.GetDataPoints() {
							decoded.metrics = append(decoded.metrics, telemetryMetricRecord{metric: metric, attrs: telemetryAttributes(point.GetAttributes()), resource: attrs, count: point.GetCount(), sum: point.GetSum()})
						}
					}
				}
			}
		case "logs":
			var envelope logscollectorpb.ExportLogsServiceRequest
			if err := proto.Unmarshal(item.Body, &envelope); err != nil {
				return decoded, fmt.Errorf("decode exported logs request %d: %w", index, err)
			}
			for _, resourceLogs := range envelope.GetResourceLogs() {
				attrs := telemetryResourceAttributes(resourceLogs.GetResource())
				for _, scopeLogs := range resourceLogs.GetScopeLogs() {
					for _, logRecord := range scopeLogs.GetLogRecords() {
						decoded.logs = append(decoded.logs, telemetryLogRecord{log: logRecord, resource: attrs})
					}
				}
			}
		}
	}
	if decoded.signalRequests["traces"] == 0 || decoded.signalRequests["metrics"] == 0 || decoded.signalRequests["logs"] == 0 {
		return decoded, fmt.Errorf("collector signal coverage incomplete: traces=%d metrics=%d logs=%d", decoded.signalRequests["traces"], decoded.signalRequests["metrics"], decoded.signalRequests["logs"])
	}
	if decoded.destinations["primary"] == 0 || decoded.destinations["metrics"] == 0 {
		return decoded, errors.New("both configured OTLP destinations did not receive exports")
	}
	if err := telemetryValidateResources(decoded); err != nil {
		return decoded, err
	}
	if err := telemetryValidateSpans(&decoded); err != nil {
		return decoded, err
	}
	if err := telemetryValidateMetrics(&decoded); err != nil {
		return decoded, err
	}
	if err := telemetryValidateLogs(&decoded); err != nil {
		return decoded, err
	}
	return decoded, nil
}

func telemetryResourceAttributes(resource *resourcepb.Resource) map[string]string {
	if resource == nil {
		return map[string]string{}
	}
	return telemetryAttributes(resource.GetAttributes())
}

func telemetryAttributes(attributes []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attributes))
	for _, attribute := range attributes {
		if attribute == nil || attribute.GetKey() == "" {
			continue
		}
		out[attribute.GetKey()] = telemetryAnyValue(attribute.GetValue())
	}
	return out
}

func telemetryAnyValue(value *commonpb.AnyValue) string {
	if value == nil {
		return ""
	}
	if value.GetStringValue() != "" {
		return value.GetStringValue()
	}
	if value.GetIntValue() != 0 {
		return strconv.FormatInt(value.GetIntValue(), 10)
	}
	if value.GetDoubleValue() != 0 {
		return strconv.FormatFloat(value.GetDoubleValue(), 'g', -1, 64)
	}
	if value.GetBoolValue() {
		return "true"
	}
	if value.GetArrayValue() != nil {
		return "[array]"
	}
	if value.GetKvlistValue() != nil {
		return "{map}"
	}
	return ""
}

func telemetryAttribute(attributes map[string]string, key string) string {
	return attributes[key]
}

func telemetryValidateResources(decoded telemetryDecoded) error {
	check := func(attrs map[string]string, signal string) error {
		if attrs["service.name"] != telemetryServiceName || attrs["service.version"] != telemetryServiceVersion || attrs["deployment.environment.name"] != telemetryEnvironment {
			return fmt.Errorf("%s resource attributes did not include the configured service identity and environment", signal)
		}
		return nil
	}
	for _, record := range decoded.spans {
		if err := check(record.resource, "trace"); err != nil {
			return err
		}
	}
	for _, record := range decoded.metrics {
		if err := check(record.resource, "metric"); err != nil {
			return err
		}
	}
	for _, record := range decoded.logs {
		if err := check(record.resource, "log"); err != nil {
			return err
		}
	}
	return nil
}

func telemetryValidateSpans(decoded *telemetryDecoded) error {
	if len(decoded.spans) == 0 {
		return errors.New("trace export decoded successfully but contained no spans")
	}
	byTrace := make(map[string]map[string]*tracepb.Span)
	serverRoots := 0
	requestSpans := 0
	requestChildren := 0
	genAI := 0
	qualified := 0
	successfulAttempts := 0
	responseModels := 0
	attemptSpans := 0
	streamTrueAttempts := 0
	streamFalseAttempts := 0
	for _, record := range decoded.spans {
		span := record.span
		if len(span.GetTraceId()) != 16 || len(span.GetSpanId()) != 8 {
			return errors.New("trace export contained a span with invalid trace or span ID length")
		}
		traceID := hex.EncodeToString(span.GetTraceId())
		spanID := hex.EncodeToString(span.GetSpanId())
		if byTrace[traceID] == nil {
			byTrace[traceID] = make(map[string]*tracepb.Span)
		}
		byTrace[traceID][spanID] = span
		if span.GetKind() == tracepb.Span_SPAN_KIND_SERVER && len(span.GetParentSpanId()) == 0 {
			serverRoots++
		}
		if span.GetName() == "gateway.request" {
			requestSpans++
			if span.GetKind() != tracepb.Span_SPAN_KIND_INTERNAL {
				return errors.New("gateway.request span was not internal")
			}
			if len(span.GetParentSpanId()) != 0 {
				requestChildren++
			}
		}
		attributes := telemetryAttributes(span.GetAttributes())
		operation := telemetryAttribute(attributes, "gen_ai.operation.name")
		provider := telemetryAttribute(attributes, "gen_ai.provider.name")
		requestModel := telemetryAttribute(attributes, "gen_ai.request.model")
		responseModel := telemetryAttribute(attributes, "gen_ai.response.model")
		if operation != "" || provider != "" || requestModel != "" || responseModel != "" {
			genAI++
			if span.GetName() == "gateway.attempt" && span.GetKind() == tracepb.Span_SPAN_KIND_CLIENT {
				attemptSpans++
				stream, present := attributes["gen_ai.request.stream"]
				if !present {
					return errors.New("client gateway.attempt span omitted gen_ai.request.stream")
				}
				if stream == "true" {
					streamTrueAttempts++
				} else {
					streamFalseAttempts++
				}
				if operation == "chat" && provider == "anthropic" && requestModel != "" {
					qualified++
				}
				if attributes["hoorific.gateway.outcome"] == "success" {
					successfulAttempts++
				}
				if responseModel != "" {
					responseModels++
				}
			}
		}
	}
	if serverRoots == 0 {
		return errors.New("trace export contained no root server span")
	}
	if requestSpans < 3 || requestChildren < 3 {
		return fmt.Errorf("trace export contained %d internal gateway.request spans with %d parented requests, want at least 3", requestSpans, requestChildren)
	}
	if genAI < 3 || qualified < 3 || attemptSpans < 3 {
		return fmt.Errorf("trace export contained insufficient qualified client attempt spans: gen_ai=%d qualified=%d attempts=%d", genAI, qualified, attemptSpans)
	}
	if successfulAttempts < 2 || responseModels < 2 || streamTrueAttempts < 1 || streamFalseAttempts < 1 {
		return fmt.Errorf("trace export omitted normal/stream attempt coverage: successful=%d response_models=%d stream_true=%d stream_false=%d", successfulAttempts, responseModels, streamTrueAttempts, streamFalseAttempts)
	}
	parentLinks := 0
	for traceID, spans := range byTrace {
		for _, span := range spans {
			parentID := span.GetParentSpanId()
			if len(parentID) == 0 {
				if span.GetName() == "gateway.request" {
					return errors.New("gateway.request span was unexpectedly root instead of a server child")
				}
				continue
			}
			if len(parentID) != 8 || hex.EncodeToString(parentID) == hex.EncodeToString(span.GetSpanId()) {
				return errors.New("trace export contained an invalid self-parent relationship")
			}
			parent, ok := spans[hex.EncodeToString(parentID)]
			if !ok {
				return fmt.Errorf("trace export contained an orphan parent link in trace %s", traceID)
			}
			if span.GetName() == "gateway.request" && (parent.GetKind() != tracepb.Span_SPAN_KIND_SERVER || len(parent.GetParentSpanId()) != 0) {
				return errors.New("gateway.request span was not a direct child of a root server span")
			}
			if span.GetName() == "gateway.attempt" && span.GetKind() == tracepb.Span_SPAN_KIND_CLIENT && (parent.GetName() != "gateway.request" || parent.GetKind() != tracepb.Span_SPAN_KIND_INTERNAL) {
				return errors.New("client gateway.attempt span was not a child of an internal gateway.request span")
			}
			parentLinks++
		}
	}
	if parentLinks == 0 {
		return errors.New("trace export contained no parent relationships")
	}
	decoded.serverRoots = serverRoots
	decoded.gatewayRequestSpans = requestSpans
	decoded.genAIAttemptSpans = attemptSpans
	decoded.streamTrueAttempts = streamTrueAttempts
	decoded.streamFalseAttempts = streamFalseAttempts
	decoded.parentLinks = parentLinks
	return nil
}

func telemetryMetricPointKey(record telemetryMetricRecord) string {
	keys := make([]string, 0, len(record.attrs)+len(record.resource))
	for key := range record.attrs {
		keys = append(keys, "a:"+key)
	}
	for key := range record.resource {
		keys = append(keys, "r:"+key)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(record.metric.GetName())
	for _, key := range keys {
		b.WriteByte('|')
		b.WriteString(key)
		b.WriteByte('=')
		if strings.HasPrefix(key, "a:") {
			b.WriteString(record.attrs[strings.TrimPrefix(key, "a:")])
		} else {
			b.WriteString(record.resource[strings.TrimPrefix(key, "r:")])
		}
	}
	return b.String()
}
func telemetryValidateMetrics(decoded *telemetryDecoded) error {
	if len(decoded.metrics) == 0 {
		return errors.New("metrics export decoded successfully but contained no histogram data points")
	}
	durationPoints := uint64(0)
	firstChunkPoints := uint64(0)
	inputSum := float64(0)
	outputSum := float64(0)
	inputCount := uint64(0)
	outputCount := uint64(0)
	// PeriodicReader and shutdown can export cumulative snapshots more than
	// once. Keep the last point for each resource/name/attribute identity
	// instead of double-counting every export envelope.
	latest := make(map[string]telemetryMetricRecord)
	for _, record := range decoded.metrics {
		if record.count == 0 || !strings.HasPrefix(record.metric.GetName(), "gen_ai.client.") {
			continue
		}
		latest[telemetryMetricPointKey(record)] = record
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		record := latest[key]
		name := record.metric.GetName()
		if record.attrs["gen_ai.operation.name"] != "chat" || record.attrs["gen_ai.provider.name"] != "anthropic" || record.attrs["hoorific.gateway.outcome"] == "" {
			return fmt.Errorf("metric %s omitted canonical operation/provider/outcome attributes", name)
		}
		if _, ok := record.attrs["gen_ai.request.model"]; ok {
			return fmt.Errorf("metric %s carried a request model attribute that is not allowed", name)
		}
		if _, ok := record.attrs["gen_ai.response.model"]; ok {
			return fmt.Errorf("metric %s carried a response model attribute that is not allowed", name)
		}
		switch name {
		case "gen_ai.client.operation.duration":
			durationPoints += record.count
		case "gen_ai.client.operation.time_to_first_chunk":
			firstChunkPoints += record.count
		case "gen_ai.client.token.usage":
			if record.attrs["hoorific.gateway.outcome"] != "success" {
				continue
			}
			switch record.attrs["gen_ai.token.type"] {
			case "input":
				inputCount += record.count
				inputSum += record.sum
			case "output":
				outputCount += record.count
				outputSum += record.sum
			default:
				return errors.New("token usage histogram omitted gen_ai.token.type input/output")
			}
		}
	}
	if durationPoints < 3 {
		return fmt.Errorf("gen_ai.client.operation.duration contained %d observations, want normal/stream/retry", durationPoints)
	}
	if firstChunkPoints == 0 {
		return errors.New("streaming request did not export gen_ai.client.operation.time_to_first_chunk")
	}
	if inputCount != 2 || outputCount != 2 || inputSum != 18 || outputSum != 4 {
		return fmt.Errorf("token histogram did not match two successful fixture attempts: input count/sum=%d/%g output count/sum=%d/%g", inputCount, inputSum, outputCount, outputSum)
	}
	decoded.tokenInputSum = inputSum
	decoded.tokenOutputSum = outputSum
	return nil
}

func telemetryValidateLogs(decoded *telemetryDecoded) error {
	if len(decoded.logs) == 0 {
		return errors.New("logs export decoded successfully but contained no log records")
	}
	spans := make(map[string]struct{})
	for _, record := range decoded.spans {
		spans[hex.EncodeToString(record.span.GetTraceId())+"/"+hex.EncodeToString(record.span.GetSpanId())] = struct{}{}
	}
	correlated := 0
	for _, record := range decoded.logs {
		logRecord := record.log
		if logRecord.GetBody() == nil && len(logRecord.GetAttributes()) == 0 {
			return errors.New("exported log record had neither a body nor attributes")
		}
		if len(logRecord.GetTraceId()) == 16 && len(logRecord.GetSpanId()) == 8 {
			if _, ok := spans[hex.EncodeToString(logRecord.GetTraceId())+"/"+hex.EncodeToString(logRecord.GetSpanId())]; ok {
				correlated++
			}
		}
	}
	if correlated == 0 {
		return errors.New("exported logs contained no record correlated to an exported gateway span")
	}
	decoded.correlatedLogs = correlated
	return nil
}
