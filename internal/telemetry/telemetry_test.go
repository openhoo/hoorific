package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hoorific/internal/core"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	logglobal "go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"log/slog"
)

type httpCapture struct {
	mu      sync.Mutex
	counts  map[string]int
	headers map[string]string
	bodies  map[string][][]byte
	errors  []error
}

func newHTTPCapture() *httpCapture {
	return &httpCapture{counts: map[string]int{}, headers: map[string]string{}, bodies: map[string][][]byte{}}
}

func (c *httpCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	signal := strings.TrimPrefix(r.URL.Path, "/v1/")
	body, err := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.errors = append(c.errors, err)
	} else {
		c.counts[signal]++
		c.headers[signal] = r.Header.Get("x-test-header")
		c.bodies[signal] = append(c.bodies[signal], body)
	}
	w.WriteHeader(http.StatusOK)
}

func (c *httpCapture) snapshot(signal string) (int, string, []byte, []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var body []byte
	if bodies := c.bodies[signal]; len(bodies) != 0 {
		body = append([]byte(nil), bodies[0]...)
	}
	return c.counts[signal], c.headers[signal], body, append([]error(nil), c.errors...)
}

type grpcCapture struct {
	mu      sync.Mutex
	traces  []*tracepb.ExportTraceServiceRequest
	metrics []*metricspb.ExportMetricsServiceRequest
	logs    []*logspb.ExportLogsServiceRequest
	header  map[string]string
}

func newGRPCCapture() *grpcCapture {
	return &grpcCapture{header: map[string]string{}}
}

func (c *grpcCapture) saveMetadata(ctx context.Context) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return
	}
	for key, values := range md {
		if len(values) != 0 {
			c.header[key] = values[0]
		}
	}
}

type traceCaptureService struct {
	tracepb.UnimplementedTraceServiceServer
	capture *grpcCapture
}

func (s traceCaptureService) Export(ctx context.Context, req *tracepb.ExportTraceServiceRequest) (*tracepb.ExportTraceServiceResponse, error) {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	s.capture.saveMetadata(ctx)
	s.capture.traces = append(s.capture.traces, req)
	return &tracepb.ExportTraceServiceResponse{}, nil
}

type metricsCaptureService struct {
	metricspb.UnimplementedMetricsServiceServer
	capture *grpcCapture
}

func (s metricsCaptureService) Export(ctx context.Context, req *metricspb.ExportMetricsServiceRequest) (*metricspb.ExportMetricsServiceResponse, error) {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	s.capture.saveMetadata(ctx)
	s.capture.metrics = append(s.capture.metrics, req)
	return &metricspb.ExportMetricsServiceResponse{}, nil
}

type logsCaptureService struct {
	logspb.UnimplementedLogsServiceServer
	capture *grpcCapture
}

func (s logsCaptureService) Export(ctx context.Context, req *logspb.ExportLogsServiceRequest) (*logspb.ExportLogsServiceResponse, error) {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	s.capture.saveMetadata(ctx)
	s.capture.logs = append(s.capture.logs, req)
	return &logspb.ExportLogsServiceResponse{}, nil
}

func startGRPCCapture(t *testing.T) (*grpcCapture, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capture := newGRPCCapture()
	server := grpc.NewServer()
	tracepb.RegisterTraceServiceServer(server, traceCaptureService{capture: capture})
	metricspb.RegisterMetricsServiceServer(server, metricsCaptureService{capture: capture})
	logspb.RegisterLogsServiceServer(server, logsCaptureService{capture: capture})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return capture, listener.Addr().String()
}

func ratio(value float64) *float64 { return &value }

func writeHeaders(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "headers.json")
	data, err := json.Marshal(map[string]string{"x-test-header": "explicit", "authorization": "explicit-only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func allSignalConfig(httpEndpoint, grpcEndpoint, headersFile string) core.TelemetryConfig {
	return core.TelemetryConfig{
		Enabled:        true,
		ServiceName:    "capture-service",
		ServiceVersion: "test-version",
		Environment:    "test",
		SampleRatio:    ratio(1),
		Exporters: []core.TelemetryExporter{
			{Name: "http", Protocol: "http/protobuf", Endpoint: httpEndpoint, HeadersFile: headersFile, Insecure: true},
			{Name: "grpc", Protocol: "grpc", Endpoint: grpcEndpoint, HeadersFile: headersFile, Insecure: true},
		},
	}
}

func emitAllSignals(ctx context.Context) {
	tracer := otel.Tracer("telemetry-test")
	ctx, span := tracer.Start(ctx, "captured span")
	span.SetAttributes(attribute.String("test.attribute", "value"))
	span.End()

	meter := otel.Meter("telemetry-test")
	counter, _ := meter.Int64Counter("captured.requests")
	counter.Add(ctx, 1, otelmetric.WithAttributes(attribute.String("test.dimension", "value")))

	slog.InfoContext(ctx, "captured log",
		slog.String("prompt", "prompt-secret"),
		slog.String("tool.arguments", "tool-secret"),
		slog.String("authorization", "Bearer local-secret"),
		slog.Int("gen_ai.usage.input_tokens", 7),
	)
}

func resourceAttribute(attrs []*commonpb.KeyValue, key string) (*commonpb.AnyValue, bool) {
	for _, attr := range attrs {
		if attr.GetKey() == key {
			return attr.GetValue(), true
		}
	}
	return nil, false
}

func assertResource(t *testing.T, resource *resourcepb.Resource) {
	t.Helper()
	if resource == nil {
		t.Fatal("missing OTLP resource")
	}
	for key, want := range map[string]string{
		"service.name":                "capture-service",
		"service.version":             "test-version",
		"deployment.environment.name": "test",
	} {
		value, ok := resourceAttribute(resource.GetAttributes(), key)
		if !ok || value.GetStringValue() != want {
			t.Fatalf("resource %q = %q, want %q", key, value.GetStringValue(), want)
		}
	}
}

func TestSetupHTTPAndGRPCAllSignals(t *testing.T) {
	// Ambient configuration must not redirect or add credentials to explicit
	// destinations. The invalid certificate path also proves explicit TLS
	// configuration takes precedence over SDK environment parsing.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1/ambient")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "ambient=must-not-leak")
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", filepath.Join(t.TempDir(), "missing.pem"))
	t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", filepath.Join(t.TempDir(), "missing.crt"))
	t.Setenv("OTEL_EXPORTER_OTLP_CLIENT_KEY", filepath.Join(t.TempDir(), "missing.key"))

	capture := newHTTPCapture()
	httpServer := httptest.NewServer(capture)
	t.Cleanup(httpServer.Close)
	grpcCapture, grpcEndpoint := startGRPCCapture(t)

	var local bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&local, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	runtime, err := Setup(context.Background(), allSignalConfig(httpServer.URL, grpcEndpoint, writeHeaders(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	emitAllSignals(context.Background())
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, signal := range []string{"traces", "metrics", "logs"} {
		count, header, body, errors := capture.snapshot(signal)
		if len(errors) != 0 {
			t.Fatalf("HTTP collector errors: %v", errors)
		}
		if count == 0 || len(body) == 0 {
			t.Fatalf("HTTP %s export count=%d body=%d", signal, count, len(body))
		}
		if header != "explicit" {
			t.Fatalf("HTTP %s header=%q, want explicit", signal, header)
		}
	}

	grpcCapture.mu.Lock()
	defer grpcCapture.mu.Unlock()
	if len(grpcCapture.traces) == 0 || len(grpcCapture.metrics) == 0 || len(grpcCapture.logs) == 0 {
		t.Fatalf("gRPC signal counts traces=%d metrics=%d logs=%d", len(grpcCapture.traces), len(grpcCapture.metrics), len(grpcCapture.logs))
	}
	if grpcCapture.header["x-test-header"] != "explicit" {
		t.Fatalf("gRPC header=%q, want explicit", grpcCapture.header["x-test-header"])
	}
	assertResource(t, grpcCapture.traces[0].GetResourceSpans()[0].GetResource())
	assertResource(t, grpcCapture.metrics[0].GetResourceMetrics()[0].GetResource())
	logs := grpcCapture.logs[0].GetResourceLogs()[0]
	assertResource(t, logs.GetResource())
	logRecords := logs.GetScopeLogs()[0].GetLogRecords()
	if len(logRecords) == 0 {
		t.Fatal("missing exported log record")
	}
	attrs := logRecords[0].GetAttributes()
	for _, key := range []string{"prompt", "tool.arguments", "authorization"} {
		value, ok := resourceAttribute(attrs, key)
		if !ok || value.GetStringValue() != "[REDACTED]" {
			t.Fatalf("log privacy %q = %q, want redacted", key, value.GetStringValue())
		}
	}
	value, ok := resourceAttribute(attrs, "gen_ai.usage.input_tokens")
	if !ok || value.GetIntValue() != 7 {
		t.Fatalf("numeric usage attribute = %d, want 7", value.GetIntValue())
	}
	if !strings.Contains(local.String(), "prompt-secret") || !strings.Contains(local.String(), "tool-secret") {
		t.Fatal("local slog output did not retain original structured values")
	}
}

func TestSameSignalMultiExporterFanout(t *testing.T) {
	firstCapture := newHTTPCapture()
	first := httptest.NewServer(firstCapture)
	defer first.Close()
	secondCapture := newHTTPCapture()
	second := httptest.NewServer(secondCapture)
	defer second.Close()

	cfg := core.TelemetryConfig{
		Enabled:     true,
		ServiceName: "fanout",
		SampleRatio: ratio(1),
		Exporters: []core.TelemetryExporter{
			{Name: "one", Protocol: "http/protobuf", Endpoint: first.URL, Insecure: true, Signals: []string{"traces"}},
			{Name: "two", Protocol: "http/protobuf", Endpoint: second.URL, Insecure: true, Signals: []string{"traces"}},
		},
	}
	runtime, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	_, span := otel.Tracer("fanout-test").Start(context.Background(), "fanout span")
	span.End()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if count, _, _, _ := firstCapture.snapshot("traces"); count == 0 {
		t.Fatal("first same-signal destination received no trace")
	}
	if count, _, _, _ := secondCapture.snapshot("traces"); count == 0 {
		t.Fatal("second same-signal destination received no trace")
	}
}

func TestDisabledTelemetryDoesNotTouchGlobals(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1/disabled")
	beforeTracer := otel.GetTracerProvider()
	beforeMeter := otel.GetMeterProvider()
	beforeLogger := logglobal.GetLoggerProvider()
	beforePropagator := otel.GetTextMapPropagator()
	beforeErrorHandler := otel.GetErrorHandler()
	beforeSlog := slog.Default()

	runtime, err := Setup(context.Background(), core.TelemetryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Enabled() {
		t.Fatal("disabled telemetry installed providers")
	}
	if otel.GetTracerProvider() != beforeTracer || otel.GetMeterProvider() != beforeMeter || logglobal.GetLoggerProvider() != beforeLogger || otel.GetTextMapPropagator() != beforePropagator || otel.GetErrorHandler() != beforeErrorHandler || slog.Default() != beforeSlog {
		t.Fatal("disabled telemetry changed a global provider, propagator, error handler, or slog logger")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("disabled shutdown: %v", err)
	}
}

func TestSetupFailureCleansPartialExportersAndGlobals(t *testing.T) {
	beforeTracer := otel.GetTracerProvider()
	beforeMeter := otel.GetMeterProvider()
	beforeLogger := logglobal.GetLoggerProvider()
	beforePropagator := otel.GetTextMapPropagator()
	beforeErrorHandler := otel.GetErrorHandler()
	beforeSlog := slog.Default()
	cfg := core.TelemetryConfig{
		Enabled:     true,
		ServiceName: "failure",
		SampleRatio: ratio(1),
		Exporters: []core.TelemetryExporter{
			{Name: "valid-first", Protocol: "http/protobuf", Endpoint: "http://127.0.0.1:4318", Insecure: true, Signals: []string{"traces"}},
			{Name: "invalid-second", Protocol: "http/protobuf", Endpoint: "http://127.0.0.1:4318", Insecure: true, HeadersFile: filepath.Join(t.TempDir(), "missing.json"), Signals: []string{"traces"}},
		},
	}
	if _, err := Setup(context.Background(), cfg); err == nil {
		t.Fatal("invalid exporter setup unexpectedly succeeded")
	}
	if otel.GetTracerProvider() != beforeTracer || otel.GetMeterProvider() != beforeMeter || logglobal.GetLoggerProvider() != beforeLogger || otel.GetTextMapPropagator() != beforePropagator || otel.GetErrorHandler() != beforeErrorHandler || slog.Default() != beforeSlog {
		t.Fatal("failed telemetry setup did not restore globals")
	}
}

func TestHTTPHandlerExtractsContextEndsPanicSpansAndTracksFinalStatus(t *testing.T) {
	capture := newHTTPCapture()
	server := httptest.NewServer(capture)
	defer server.Close()
	runtime, err := Setup(context.Background(), core.TelemetryConfig{
		Enabled:     true,
		ServiceName: "http-handler",
		SampleRatio: ratio(1),
		Exporters:   []core.TelemetryExporter{{Name: "http", Protocol: "http/protobuf", Endpoint: server.URL, Insecure: true, Signals: []string{"traces"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	parentTrace, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	parentSpan, _ := trace.SpanIDFromHex("0102030405060708")
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: parentTrace, SpanID: parentSpan, TraceFlags: trace.FlagsSampled, Remote: true})
	request := httptest.NewRequest(http.MethodGet, server.URL+"/private/path?secret=query", nil)
	propagation.TraceContext{}.Inject(trace.ContextWithRemoteSpanContext(request.Context(), parent), propagation.HeaderCarrier(request.Header))
	handler := runtime.HTTPHandler("management", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("middleware removed http.Flusher")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "stream-body")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || recorder.Body.String() != "stream-body" {
		t.Fatalf("wrapped response code=%d body=%q", recorder.Code, recorder.Body.String())
	}

	panicHandler := runtime.HTTPHandler("management", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret panic payload") }))
	func() {
		defer func() { _ = recover() }()
		panicHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.errors) != 0 {
		t.Fatalf("trace export errors=%v", capture.errors)
	}
	spanCount, parentFound, panicFound := 0, false, false
	for _, body := range capture.bodies["traces"] {
		requestExport := &tracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, requestExport); err != nil {
			t.Fatal(err)
		}
		for _, resource := range requestExport.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				for _, span := range scope.GetSpans() {
					spanCount++
					parentFound = parentFound || bytes.Equal(span.GetParentSpanId(), parentSpan[:])
					panicFound = panicFound || span.GetStatus().GetCode().String() == "STATUS_CODE_ERROR"
					for _, attr := range span.GetAttributes() {
						if strings.Contains(strings.ToLower(attr.GetKey()), "secret") || strings.Contains(strings.ToLower(attr.GetKey()), "query") {
							t.Fatalf("HTTP span copied a sensitive request field %q", attr.GetKey())
						}
					}
				}
			}
		}
	}
	if spanCount != 2 || !parentFound || !panicFound {
		t.Fatalf("HTTP spans: count=%d parent=%v panic=%v", spanCount, parentFound, panicFound)
	}
}

type statusRecordingWriter struct {
	header   http.Header
	statuses []int
	body     bytes.Buffer
}

func (w *statusRecordingWriter) Header() http.Header { return w.header }

func (w *statusRecordingWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}

func (w *statusRecordingWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func TestResponseWriterForwardsInformationalHeaders(t *testing.T) {
	recorder := &statusRecordingWriter{header: make(http.Header)}
	wrapped := &responseWriter{ResponseWriter: recorder}
	wrapped.WriteHeader(http.StatusEarlyHints)
	wrapped.WriteHeader(http.StatusCreated)
	if wrapped.status != http.StatusCreated || !wrapped.wrote {
		t.Fatalf("tracked status=%d wrote=%v", wrapped.status, wrapped.wrote)
	}
	if len(recorder.statuses) != 2 || recorder.statuses[0] != http.StatusEarlyHints || recorder.statuses[1] != http.StatusCreated {
		t.Fatalf("underlying statuses=%v, want informational and final", recorder.statuses)
	}
}

func TestDefaultSlogBridgeNoRecursion(t *testing.T) {
	if os.Getenv("HOORIFIC_TELEMETRY_SLOG_HELPER") == "1" {
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")
		runtime, err := Setup(context.Background(), core.TelemetryConfig{
			Enabled:     true,
			ServiceName: "slog-default",
			SampleRatio: ratio(1),
		})
		if err != nil {
			t.Fatal(err)
		}
		slog.Info("default slog bridge")
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}

	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (key == "HOORIFIC_TELEMETRY_SLOG_HELPER" || key == "OTEL_TRACES_EXPORTER" || key == "OTEL_METRICS_EXPORTER" || key == "OTEL_LOGS_EXPORTER") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"HOORIFIC_TELEMETRY_SLOG_HELPER=1",
		"OTEL_TRACES_EXPORTER=none",
		"OTEL_METRICS_EXPORTER=none",
		"OTEL_LOGS_EXPORTER=none",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDefaultSlogBridgeNoRecursion$")
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("default slog helper hung: %v; output=%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("default slog helper failed: %v; output=%s", err, output)
	}
	if !bytes.Contains(output, []byte("default slog bridge")) {
		t.Fatalf("default slog helper emitted no local log: %s", output)
	}
}
