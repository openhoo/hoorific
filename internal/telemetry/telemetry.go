// Package telemetry configures Hoorific's optional OpenTelemetry runtime.
//
// The package deliberately owns the SDK providers and their lifecycle. A
// disabled configuration is a true no-op: it does not replace global
// providers, propagators, error handlers, or the process slog handler.
package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"hoorific/internal/core"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otelLog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"google.golang.org/grpc/credentials"
)

const (
	// ExportTimeout bounds one destination export. ShutdownTimeout is kept
	// below the gateway's 30 second server shutdown budget.
	ExportTimeout   = 5 * time.Second
	ShutdownTimeout = 25 * time.Second
	BatchTimeout    = 1 * time.Second
	MetricInterval  = 15 * time.Second
	MaxQueueSize    = 2048
	MaxBatchSize    = 512
	MaxLogAttrs     = 64
	MaxLogValueLen  = 2048
	MaxHTTPBody     = 64 << 10
)

var signalNames = [...]string{"traces", "metrics", "logs"}
var explicitExporterEnvMu sync.Mutex

// Runtime owns all providers installed by Setup. It is safe to call Shutdown
// and ForceFlush concurrently, and Shutdown is idempotent.
type Runtime struct {
	enabled bool

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
	resource       *resource.Resource

	previousTracer     trace.TracerProvider
	previousMeter      otelmetric.MeterProvider
	previousLogger     otelLog.LoggerProvider
	previousPropagator propagation.TextMapPropagator
	previousError      otel.ErrorHandler
	previousSlog       *slog.Logger
	previousLogWriter  io.Writer
	previousLogFlags   int
	installedSlog      *slog.Logger

	shutdownOnce sync.Once
	shutdownErr  error
}

// Setup creates providers and installs them globally before returning. No
// exporter is created when cfg.Enabled is false. Explicit exporters are
// isolated from all OTEL exporter endpoint, header, and security environment
// variables by passing every destination setting explicitly.
func Setup(ctx context.Context, cfg core.TelemetryConfig) (*Runtime, error) {
	if !cfg.Enabled {
		return &Runtime{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	previousError := otel.GetErrorHandler()
	// SDK errors can contain URLs, response bodies, or authorization metadata.
	// Install the non-recursive, type-only handler before resource detection and
	// exporter construction. It is restored if setup fails.
	otel.SetErrorHandler(safeErrorHandler{})

	res, err := buildResource(ctx, cfg)
	if err != nil {
		otel.SetErrorHandler(previousError)
		return nil, errors.New("telemetry resource initialization failed")
	}

	traceExporters, metricExporters, logExporters, err := buildExporters(ctx, cfg)
	if err != nil {
		cleanupExporters(traceExporters, metricExporters, logExporters)
		otel.SetErrorHandler(previousError)
		return nil, err
	}

	sampler := samplerFor(cfg.EffectiveSampleRatio(), cfg.SampleRatio != nil)
	traceOptions := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	for _, exporter := range traceExporters {
		traceOptions = append(traceOptions, sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(
			exporter,
			sdktrace.WithMaxQueueSize(MaxQueueSize),
			sdktrace.WithMaxExportBatchSize(MaxBatchSize),
			sdktrace.WithBatchTimeout(BatchTimeout),
			sdktrace.WithExportTimeout(ExportTimeout),
		)))
	}
	tracerProvider := sdktrace.NewTracerProvider(traceOptions...)

	metricOptions := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithCardinalityLimit(2000),
	}
	for _, exporter := range metricExporters {
		metricOptions = append(metricOptions, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(
			exporter,
			sdkmetric.WithInterval(MetricInterval),
			sdkmetric.WithTimeout(ExportTimeout),
		)))
	}
	meterProvider := sdkmetric.NewMeterProvider(metricOptions...)

	logOptions := []sdklog.LoggerProviderOption{
		sdklog.WithResource(res),
		sdklog.WithAttributeCountLimit(MaxLogAttrs),
		sdklog.WithAttributeValueLengthLimit(MaxLogValueLen),
	}
	for _, exporter := range logExporters {
		logOptions = append(logOptions, sdklog.WithProcessor(sdklog.NewBatchProcessor(
			exporter,
			sdklog.WithMaxQueueSize(MaxQueueSize),
			sdklog.WithExportMaxBatchSize(MaxBatchSize),
			sdklog.WithExportInterval(BatchTimeout),
			sdklog.WithExportTimeout(ExportTimeout),
		)))
	}
	loggerProvider := sdklog.NewLoggerProvider(logOptions...)

	runtime := &Runtime{
		enabled:            true,
		tracerProvider:     tracerProvider,
		meterProvider:      meterProvider,
		loggerProvider:     loggerProvider,
		resource:           res,
		previousTracer:     otel.GetTracerProvider(),
		previousMeter:      otel.GetMeterProvider(),
		previousLogger:     logglobal.GetLoggerProvider(),
		previousPropagator: otel.GetTextMapPropagator(),
		previousError:      previousError,
		previousSlog:       slog.Default(),
		previousLogWriter:  log.Writer(),
		previousLogFlags:   log.Flags(),
	}

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	logglobal.SetLoggerProvider(loggerProvider)
	// Hoorific propagates W3C trace context only. Baggage is intentionally not
	// accepted or forwarded because it can carry unbounded sensitive metadata.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	bridge := slog.New(newSlogHandler(runtime.previousSlog.Handler(), logglobal.Logger("hoorific/slog")))
	runtime.installedSlog = bridge
	slog.SetDefault(bridge)
	// slog.SetDefault links the standard log package to non-default slog
	// handlers. Restore the prior writer immediately: the prior built-in
	// defaultHandler itself writes through that writer, and leaving the
	// link installed would recurse through this bridge.
	log.SetOutput(runtime.previousLogWriter)
	log.SetFlags(runtime.previousLogFlags)
	return runtime, nil
}

// Enabled reports whether this Runtime installed SDK providers.
func (r *Runtime) Enabled() bool { return r != nil && r.enabled }

// Resource returns the immutable resource associated with all providers.
func (r *Runtime) Resource() *resource.Resource {
	if r == nil {
		return nil
	}
	return r.resource
}

// TracerProvider returns the provider installed by this Runtime, or nil for a
// disabled Runtime.
func (r *Runtime) TracerProvider() trace.TracerProvider {
	if r == nil {
		return nil
	}
	return r.tracerProvider
}

// MeterProvider returns the provider installed by this Runtime, or nil for a
// disabled Runtime.
func (r *Runtime) MeterProvider() otelmetric.MeterProvider {
	if r == nil {
		return nil
	}
	return r.meterProvider
}

// LoggerProvider returns the provider installed by this Runtime, or nil for a
// disabled Runtime.
func (r *Runtime) LoggerProvider() otelLog.LoggerProvider {
	if r == nil {
		return nil
	}
	return r.loggerProvider
}

// ForceFlush asks every signal provider to export queued data. Each signal is
// flushed independently so one failed destination does not replay successful
// destinations.
func (r *Runtime) ForceFlush(ctx context.Context) error {
	if r == nil || !r.enabled {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var wg sync.WaitGroup
	results := make(chan error, 3)
	wg.Add(3)
	go func() { defer wg.Done(); results <- r.tracerProvider.ForceFlush(ctx) }()
	go func() { defer wg.Done(); results <- r.meterProvider.ForceFlush(ctx) }()
	go func() { defer wg.Done(); results <- r.loggerProvider.ForceFlush(ctx) }()
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			return errors.New("telemetry flush failed")
		}
	}
	return nil
}

// Shutdown flushes and closes all providers, then restores the providers and
// local handlers that were active before Setup. It never returns raw SDK or
// exporter errors because those can contain endpoint or credential material.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || !r.enabled {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.shutdownOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
		defer cancel()

		var wg sync.WaitGroup
		results := make(chan error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); results <- r.tracerProvider.Shutdown(shutdownCtx) }()
		go func() { defer wg.Done(); results <- r.meterProvider.Shutdown(shutdownCtx) }()
		go func() { defer wg.Done(); results <- r.loggerProvider.Shutdown(shutdownCtx) }()
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				r.shutdownErr = errors.New("telemetry shutdown failed")
				break
			}
		}

		if otel.GetTracerProvider() == r.tracerProvider {
			otel.SetTracerProvider(r.previousTracer)
		}
		if otel.GetMeterProvider() == r.meterProvider {
			otel.SetMeterProvider(r.previousMeter)
		}
		if logglobal.GetLoggerProvider() == r.loggerProvider {
			logglobal.SetLoggerProvider(r.previousLogger)
		}
		otel.SetTextMapPropagator(r.previousPropagator)
		otel.SetErrorHandler(r.previousError)
		if slog.Default() == r.installedSlog {
			slog.SetDefault(r.previousSlog)
			log.SetOutput(r.previousLogWriter)
			log.SetFlags(r.previousLogFlags)
		}
	})
	return r.shutdownErr
}

func buildResource(ctx context.Context, cfg core.TelemetryConfig) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, errors.New("telemetry resource initialization failed")
	}

	attrs := make([]attribute.KeyValue, 0, 3)
	if cfg.ServiceName != "" {
		attrs = append(attrs, attribute.String("service.name", cfg.ServiceName))
	} else if _, ok := res.Set().Value(attribute.Key("service.name")); !ok {
		// OTEL_SERVICE_NAME and service.name in OTEL_RESOURCE_ATTRIBUTES are
		// honored by WithFromEnv; use the application default only if neither
		// supplied one.
		attrs = append(attrs, attribute.String("service.name", "hoorific"))
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", cfg.Environment))
	}
	if len(attrs) == 0 {
		return res, nil
	}
	res, err = resource.Merge(res, resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, errors.New("telemetry resource initialization failed")
	}
	return res, nil
}

func samplerFor(ratio float64, explicit ...bool) sdktrace.Sampler {
	if ratio < 0 || ratio > 1 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		ratio = core.DefaultTelemetrySampleRatio
	}
	isExplicit := len(explicit) > 0 && explicit[0]
	if !isExplicit {
		if sampler, ok := samplerFromEnv(); ok {
			return sampler
		}
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

func samplerFromEnv() (sdktrace.Sampler, bool) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	switch name {
	case "":
		return nil, false
	case "always_on":
		return sdktrace.AlwaysSample(), true
	case "always_off":
		return sdktrace.NeverSample(), true
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), true
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), true
	case "traceidratio", "parentbased_traceidratio":
		ratio, err := parseRatio(arg)
		if err != nil {
			return nil, false
		}
		s := sdktrace.TraceIDRatioBased(ratio)
		if name == "parentbased_traceidratio" {
			return sdktrace.ParentBased(s), true
		}
		return s, true
	default:
		return nil, false
	}
}

func parseRatio(value string) (float64, error) {
	var ratio float64
	if _, err := fmt.Sscanf(value, "%f", &ratio); err != nil || ratio < 0 || ratio > 1 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0, errors.New("invalid sampling ratio")
	}
	return ratio, nil
}

func buildExporters(ctx context.Context, cfg core.TelemetryConfig) ([]sdktrace.SpanExporter, []sdkmetric.Exporter, []sdklog.Exporter, error) {
	var traces []sdktrace.SpanExporter
	var metrics []sdkmetric.Exporter
	var logs []sdklog.Exporter
	if len(cfg.Exporters) == 0 {
		for _, signal := range signalNames {
			enabled, err := envExporterEnabled(signal)
			if err != nil {
				return traces, metrics, logs, err
			}
			if !enabled {
				continue
			}
			protocolName, err := envProtocol(signal)
			if err != nil {
				return traces, metrics, logs, fmt.Errorf("telemetry %s exporter configuration is invalid", signal)
			}
			exporter, err := newEnvExporter(ctx, signal, protocolName)
			if err != nil {
				return traces, metrics, logs, fmt.Errorf("telemetry %s exporter initialization failed", signal)
			}
			switch signal {
			case "traces":
				traces = append(traces, exporter.(sdktrace.SpanExporter))
			case "metrics":
				metrics = append(metrics, exporter.(sdkmetric.Exporter))
			case "logs":
				logs = append(logs, exporter.(sdklog.Exporter))
			}
		}
		return traces, metrics, logs, nil
	}

	for _, destination := range cfg.Exporters {
		headers, err := readHeaders(destination.HeadersFile)
		if err != nil {
			return traces, metrics, logs, fmt.Errorf("telemetry exporter %q initialization failed", destination.Name)
		}
		selected := destination.Signals
		if len(selected) == 0 {
			selected = signalNames[:]
		}
		for _, signal := range selected {
			exporter, err := newExplicitExporter(ctx, signal, destination, headers)
			if err != nil {
				cleanupExporters(traces, metrics, logs)
				return nil, nil, nil, fmt.Errorf("telemetry %s exporter %q initialization failed", signal, destination.Name)
			}
			switch signal {
			case "traces":
				traces = append(traces, exporter.(sdktrace.SpanExporter))
			case "metrics":
				metrics = append(metrics, exporter.(sdkmetric.Exporter))
			case "logs":
				logs = append(logs, exporter.(sdklog.Exporter))
			}
		}
	}
	return traces, metrics, logs, nil
}

func envExporterEnabled(signal string) (bool, error) {
	value := strings.TrimSpace(os.Getenv("OTEL_" + strings.ToUpper(signal) + "_EXPORTER"))
	if value == "" {
		// OTEL's default exporter is OTLP for each signal.
		return true, nil
	}
	parts := strings.Split(value, ",")
	seen := false
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "otlp":
			seen = true
		case "none":
			if len(parts) != 1 {
				return false, errors.New("none cannot be combined with another exporter")
			}
			return false, nil
		default:
			return false, errors.New("unsupported exporter")
		}
	}
	return seen, nil
}

func envProtocol(signal string) (string, error) {
	key := "OTEL_EXPORTER_OTLP_" + strings.ToUpper(signal) + "_PROTOCOL"
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		value = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	if value == "" {
		value = "http/protobuf"
	}
	if value != "http/protobuf" && value != "grpc" {
		return "", errors.New("unsupported protocol")
	}
	return value, nil
}

func newEnvExporter(ctx context.Context, signal, protocolName string) (any, error) {
	switch signal {
	case "traces":
		if protocolName == "grpc" {
			return otlptracegrpc.New(ctx, otlptracegrpc.WithTimeout(ExportTimeout))
		}
		return otlptracehttp.New(ctx, otlptracehttp.WithTimeout(ExportTimeout))
	case "metrics":
		if protocolName == "grpc" {
			return otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithTimeout(ExportTimeout))
		}
		return otlpmetrichttp.New(ctx, otlpmetrichttp.WithTimeout(ExportTimeout))
	case "logs":
		if protocolName == "grpc" {
			return otlploggrpc.New(ctx, otlploggrpc.WithTimeout(ExportTimeout))
		}
		return otlploghttp.New(ctx, otlploghttp.WithTimeout(ExportTimeout))
	default:
		return nil, errors.New("unsupported signal")
	}
}

func newExplicitExporter(ctx context.Context, signal string, destination core.TelemetryExporter, headers map[string]string) (any, error) {
	restore := isolateExporterEnvironment()
	defer restore()
	if endpoint, err := url.Parse(destination.Endpoint); err == nil && endpoint.Scheme == "http" && endpoint.Host != "" {
		destination.Insecure = true
	}

	switch signal {
	case "traces":
		if destination.Protocol == "grpc" {
			options := []otlptracegrpc.Option{
				otlptracegrpc.WithEndpointURL(grpcEndpoint(destination.Endpoint, destination.Insecure)),
				otlptracegrpc.WithHeaders(cloneHeaders(headers)),
				otlptracegrpc.WithTimeout(ExportTimeout),
			}
			if !destination.Insecure {
				options = append(options, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{})))
			}
			return otlptracegrpc.New(ctx, options...)
		}
		options := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(signalEndpoint(destination.Endpoint, signal)),
			otlptracehttp.WithHeaders(cloneHeaders(headers)),
			otlptracehttp.WithTimeout(ExportTimeout),
		}
		if !destination.Insecure {
			options = append(options, otlptracehttp.WithTLSClientConfig(&tls.Config{}))
		}
		return otlptracehttp.New(ctx, options...)
	case "metrics":
		if destination.Protocol == "grpc" {
			options := []otlpmetricgrpc.Option{
				otlpmetricgrpc.WithEndpointURL(grpcEndpoint(destination.Endpoint, destination.Insecure)),
				otlpmetricgrpc.WithHeaders(cloneHeaders(headers)),
				otlpmetricgrpc.WithTimeout(ExportTimeout),
			}
			if !destination.Insecure {
				options = append(options, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{})))
			}
			return otlpmetricgrpc.New(ctx, options...)
		}
		options := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(signalEndpoint(destination.Endpoint, signal)),
			otlpmetrichttp.WithHeaders(cloneHeaders(headers)),
			otlpmetrichttp.WithTimeout(ExportTimeout),
		}
		if !destination.Insecure {
			options = append(options, otlpmetrichttp.WithTLSClientConfig(&tls.Config{}))
		}
		return otlpmetrichttp.New(ctx, options...)
	case "logs":
		if destination.Protocol == "grpc" {
			options := []otlploggrpc.Option{
				otlploggrpc.WithEndpointURL(grpcEndpoint(destination.Endpoint, destination.Insecure)),
				otlploggrpc.WithHeaders(cloneHeaders(headers)),
				otlploggrpc.WithTimeout(ExportTimeout),
			}
			if !destination.Insecure {
				options = append(options, otlploggrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{})))
			}
			return otlploggrpc.New(ctx, options...)
		}
		options := []otlploghttp.Option{
			otlploghttp.WithEndpointURL(signalEndpoint(destination.Endpoint, signal)),
			otlploghttp.WithHeaders(cloneHeaders(headers)),
			otlploghttp.WithTimeout(ExportTimeout),
		}
		if !destination.Insecure {
			options = append(options, otlploghttp.WithTLSClientConfig(&tls.Config{}))
		}
		return otlploghttp.New(ctx, options...)
	default:
		return nil, errors.New("unsupported signal")
	}
}
func isolateExporterEnvironment() func() {
	explicitExporterEnvMu.Lock()
	saved := make([]string, 0)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(key, "OTEL_EXPORTER_OTLP_") {
			continue
		}
		saved = append(saved, entry)
		_ = os.Unsetenv(key)
	}
	return func() {
		for _, entry := range saved {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				_ = os.Setenv(key, value)
			}
		}
		explicitExporterEnvMu.Unlock()
	}
}

func readHeaders(filename string) (map[string]string, error) {
	if filename == "" {
		return map[string]string{}, nil
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limited := io.LimitReader(file, MaxHTTPBody+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > MaxHTTPBody {
		return nil, errors.New("headers file is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var headers map[string]string
	if err := decoder.Decode(&headers); err != nil {
		return nil, errors.New("headers file is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || headers == nil {
		return nil, errors.New("headers file is invalid")
	}
	for key, value := range headers {
		if !validHeaderName(key) || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("headers file is invalid")
		}
	}
	return headers, nil
}

func validHeaderName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r <= 32 || r >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", r) {
			return false
		}
	}
	return true
}

func cloneHeaders(headers map[string]string) map[string]string {
	clone := make(map[string]string, len(headers))
	for key, value := range headers {
		clone[key] = value
	}
	return clone
}

func signalEndpoint(raw, signal string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/" + signal
	parsed.RawPath = ""
	return parsed.String()
}

func grpcEndpoint(raw string, insecure bool) string {
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.IsAbs() && parsed.Host != "" {
		return scheme + "://" + parsed.Host
	}
	return scheme + "://" + raw
}

func cleanupExporters(traces []sdktrace.SpanExporter, metrics []sdkmetric.Exporter, logs []sdklog.Exporter) {
	ctx, cancel := context.WithTimeout(context.Background(), ExportTimeout)
	defer cancel()
	for _, exporter := range traces {
		_ = exporter.Shutdown(ctx)
	}
	for _, exporter := range metrics {
		_ = exporter.Shutdown(ctx)
	}
	for _, exporter := range logs {
		_ = exporter.Shutdown(ctx)
	}
}

// safeErrorHandler intentionally does not call slog: doing so would feed SDK
// exporter failures back into the bridge and recursively enqueue failures.
type safeErrorHandler struct{}

func (safeErrorHandler) Handle(err error) {
	if err == nil {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "hoorific telemetry error: %T\n", err)
}

// slogHandler mirrors every accepted record to the pre-existing local handler
// and to the OTel Logs API. It exports only bounded, filtered attributes.
type slogHandler struct {
	local  slog.Handler
	logger otelLog.Logger
	group  string
	attrs  []otelLog.KeyValue
}

func newSlogHandler(local slog.Handler, logger otelLog.Logger) slog.Handler {
	return &slogHandler{local: local, logger: logger}
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	localEnabled := h.local != nil && h.local.Enabled(ctx, level)
	otelEnabled := h.logger != nil && h.logger.Enabled(ctx, otelLog.EnabledParameters{Severity: slogSeverity(level)})
	return localEnabled || otelEnabled
}

func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	var localErr error
	if h.local != nil && h.local.Enabled(ctx, record.Level) {
		localErr = h.local.Handle(ctx, record)
	}
	if h.logger == nil || !h.logger.Enabled(ctx, otelLog.EnabledParameters{Severity: slogSeverity(record.Level)}) {
		return localErr
	}
	var output otelLog.Record
	output.SetTimestamp(record.Time)
	output.SetObservedTimestamp(time.Now())
	output.SetSeverity(slogSeverity(record.Level))
	output.SetSeverityText(record.Level.String())
	output.SetBody(otelLog.StringValue(record.Message))
	if h.attrs != nil {
		output.AddAttributes(h.attrs...)
	}
	record.Attrs(func(attr slog.Attr) bool {
		output.AddAttributes(slogKeyValue(attr, h.group))
		return true
	})
	// The SDK derives TraceId/SpanId from ctx when Emit is called. No context
	// reconstruction is needed, and local slog output remains unchanged.
	h.logger.Emit(ctx, output)
	return localErr
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	if h.local != nil {
		clone.local = h.local.WithAttrs(attrs)
	}
	clone.attrs = append(append([]otelLog.KeyValue(nil), h.attrs...), slogKeyValues(attrs, h.group)...)
	return &clone
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	if h.local != nil {
		clone.local = h.local.WithGroup(name)
	}
	if clone.group == "" {
		clone.group = name
	} else {
		clone.group += "." + name
	}
	return &clone
}

func slogSeverity(level slog.Level) otelLog.Severity {
	switch {
	case level < slog.LevelInfo:
		return otelLog.SeverityDebug
	case level < slog.LevelWarn:
		return otelLog.SeverityInfo
	case level < slog.LevelError:
		return otelLog.SeverityWarn
	default:
		return otelLog.SeverityError
	}
}

func slogKeyValues(attrs []slog.Attr, group string) []otelLog.KeyValue {
	out := make([]otelLog.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, slogKeyValue(attr, group))
	}
	return out
}

func slogKeyValue(attr slog.Attr, group string) otelLog.KeyValue {
	attr.Value = attr.Value.Resolve()
	key := attr.Key
	if group != "" && key != "" {
		key = group + "." + key
	}
	value := attr.Value
	if (sensitiveKey(key) && !safeNumericUsageKey(key, value.Kind())) ||
		(value.Kind() == slog.KindString && privateStringKey(key)) {
		return otelLog.String(key, "[REDACTED]")
	}
	switch value.Kind() {
	case slog.KindString:
		return otelLog.String(key, value.String())
	case slog.KindBool:
		return otelLog.Bool(key, value.Bool())
	case slog.KindInt64:
		return otelLog.Int64(key, value.Int64())
	case slog.KindUint64:
		if value.Uint64() > math.MaxInt64 {
			return otelLog.String(key, "[UNREPRESENTABLE_UINT64]")
		}
		return otelLog.Int64(key, int64(value.Uint64()))
	case slog.KindFloat64:
		return otelLog.Float64(key, value.Float64())
	case slog.KindDuration:
		return otelLog.Int64(key, value.Duration().Nanoseconds())
	case slog.KindTime:
		return otelLog.String(key, value.Time().UTC().Format(time.RFC3339Nano))
	case slog.KindGroup:
		children := value.Group()
		if len(children) == 0 {
			return otelLog.Empty(key)
		}
		return otelLog.Map(key, slogKeyValues(children, key)...)
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return otelLog.String(key, fmt.Sprintf("%T", err))
		}
		return otelLog.String(key, fmt.Sprintf("%T", value.Any()))
	default:
		return otelLog.Empty(key)
	}
}
func sensitiveKey(key string) bool {
	normalized := normalizeLogKey(key)
	for _, token := range []string{"authorization", "proxyauthorization", "cookie", "setcookie", "password", "passwd", "secret", "token", "credential", "apikey", "accesskey", "privatekey"} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func privateStringKey(key string) bool {
	normalized := normalizeLogKey(key)
	for _, token := range []string{"prompt", "completion", "toolargument", "functionargument", "toolinput"} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func safeNumericUsageKey(key string, kind slog.Kind) bool {
	normalized := normalizeLogKey(key)
	if !strings.HasPrefix(normalized, "genaiusage") || !strings.HasSuffix(normalized, "tokens") {
		return false
	}
	switch kind {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64:
		return true
	default:
		return false
	}
}

func normalizeLogKey(key string) string {
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(key))
}

// HTTPHandler instruments a listener with a server span while retaining the
// optional ResponseWriter interfaces needed by streaming and WebSocket code.
// Paths, queries, headers, and bodies are intentionally not copied to spans.
func (r *Runtime) HTTPHandler(listener string, next http.Handler) http.Handler {
	if r == nil || !r.enabled || next == nil {
		return next
	}
	if listener == "" {
		listener = "http"
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		spanName := "HTTP " + request.Method + " " + listener
		ctx, span := otel.Tracer("hoorific/http").Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
			attribute.String("http.request.method", request.Method),
			attribute.String("http.route", listener),
		))
		wrapped := &responseWriter{ResponseWriter: writer}
		finish := func() {
			status := wrapped.status
			if status == 0 {
				status = http.StatusOK
			}
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, "server error")
			}
			span.End()
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				span.RecordError(errors.New("http handler panic"))
				span.SetStatus(codes.Error, "handler panic")
				span.End()
				panic(recovered)
			}
			finish()
		}()
		next.ServeHTTP(wrapped, request.WithContext(ctx))
	})
}

type responseWriter struct {
	http.ResponseWriter
	status   int
	wrote    bool
	hijacked bool
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if status >= http.StatusOK || status == http.StatusSwitchingProtocols {
		if !w.wrote {
			w.status = status
			w.wrote = true
		}
	}
	// Let net/http preserve its native handling for informational, duplicate,
	// and invalid status codes; this wrapper only tracks the terminal status.
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseWriter) Flush() {
	_ = w.FlushError()
}

func (w *responseWriter) FlushError() error {
	if !w.wrote && !w.hijacked {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
		w.status = http.StatusSwitchingProtocols
		w.wrote = true
	}
	return conn, buffered, err
}

func (w *responseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (w *responseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if source, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return source.ReadFrom(reader)
	}
	return io.Copy(w.ResponseWriter, reader)
}

var _ http.Handler = (http.HandlerFunc(nil))
