package gateway

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"hoorific/internal/core"
)

// Metrics records gateway exchanges separately from independently admitted attempts.
// Labels never contain caller, connection, model, account, or request identities.
// Construct once per registry; duplicate registration is a startup configuration error.
// All methods accept a nil receiver, allowing instrumentation to be disabled.
type Metrics struct {
	requests      *prometheus.CounterVec
	active        *prometheus.GaugeVec
	duration      *prometheus.HistogramVec
	firstPayload  *prometheus.HistogramVec
	bytes         *prometheus.CounterVec
	attempts      *prometheus.CounterVec
	retries       *prometheus.CounterVec
	outcomes      *prometheus.CounterVec
	cancellations *prometheus.CounterVec
	usage         *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	m := &Metrics{
		requests:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_requests_total", Help: "Completed gateway HTTP exchanges, including rejected requests."}, []string{"protocol", "status"}),
		active:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "hoorific_gateway_active_requests", Help: "Gateway requests currently held, including streaming and realtime sessions."}, []string{"protocol"}),
		duration:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "hoorific_gateway_request_duration_seconds", Help: "Full gateway exchange duration through response or session completion.", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}}, []string{"protocol"}),
		firstPayload:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "hoorific_gateway_first_payload_seconds", Help: "Time until the first successful HTTP response body write; headers, flushes and hijacked frames are excluded.", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}}, []string{"protocol"}),
		bytes:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_response_bytes_total", Help: "HTTP response body bytes accepted by the downstream writer; excludes headers and hijacked frames."}, []string{"protocol"}),
		attempts:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_attempts_total", Help: "Upstream dispatch attempts after an independent admission permit."}, []string{"protocol", "operation", "connector"}),
		retries:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_retries_total", Help: "Requests for another independent admission permit after a retryable attempt."}, []string{"protocol"}),
		outcomes:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_attempt_outcomes_total", Help: "Observed attempt outcomes at finalization; does not assert durable settlement succeeded."}, []string{"protocol", "outcome"}),
		cancellations: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_cancellations_total", Help: "Attempt cancellation evidence, without inferring remote execution stopped."}, []string{"protocol", "cancellation"}),
		usage:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoorific_gateway_usage_tokens_total", Help: "Known nonnegative provider input or output token usage; missing usage is never estimated."}, []string{"protocol", "direction"}),
	}
	registerer.MustRegister(m.requests, m.active, m.duration, m.firstPayload, m.bytes, m.attempts, m.retries, m.outcomes, m.cancellations, m.usage)
	return m
}

func metricsNoop() {}

// Wrap measures until finish, which must run after all response/session work stops.
// Like net/http.ResponseWriter, the wrapper requires serialized writer access.
func (m *Metrics) Wrap(w http.ResponseWriter, protocol core.Protocol) (http.ResponseWriter, func()) {
	if m == nil {
		return w, metricsNoop
	}
	label := metricProtocol(protocol)
	active := m.active.WithLabelValues(label)
	active.Inc()
	observed := &metricsWriter{ResponseWriter: w, started: time.Now(), first: m.firstPayload.WithLabelValues(label)}
	var once sync.Once
	return observed, func() {
		once.Do(func() {
			active.Dec()
			status := observed.status
			if status == 0 && !observed.hijacked {
				status = http.StatusOK
			}
			m.requests.WithLabelValues(label, metricStatus(status)).Inc()
			m.duration.WithLabelValues(label).Observe(time.Since(observed.started).Seconds())
			m.bytes.WithLabelValues(label).Add(float64(observed.bytes))
		})
	}
}

func (m *Metrics) Attempt(protocol core.Protocol, operation core.Operation, connector string) {
	if m != nil {
		m.attempts.WithLabelValues(metricProtocol(protocol), metricOperation(operation), metricConnector(connector)).Inc()
	}
}

func (m *Metrics) Retry(protocol core.Protocol) {
	if m != nil {
		m.retries.WithLabelValues(metricProtocol(protocol)).Inc()
	}
}

// Outcome is called once per attempt, not once per durable finalization retry.
func (m *Metrics) Outcome(protocol core.Protocol, outcome core.AttemptOutcome) {
	if m == nil {
		return
	}
	label := metricProtocol(protocol)
	state := outcome.State
	switch state {
	case "prepared", "dispatch_intent", "accepted", "settled", "not_executed", "outcome_unknown", "job_pending", "failed", "cancelled", "usage_unknown":
	default:
		state = "unknown"
	}
	m.outcomes.WithLabelValues(label, state).Inc()
	if outcome.Cancellation != "" {
		cancellation := outcome.Cancellation
		switch cancellation {
		case "requested", "confirmed", "unknown":
		default:
			cancellation = "unknown"
		}
		m.cancellations.WithLabelValues(label, cancellation).Inc()
	}
	if outcome.Usage != nil {
		if outcome.Usage.Input != nil && *outcome.Usage.Input >= 0 {
			m.usage.WithLabelValues(label, "input").Add(float64(*outcome.Usage.Input))
		}
		if outcome.Usage.Output != nil && *outcome.Usage.Output >= 0 {
			m.usage.WithLabelValues(label, "output").Add(float64(*outcome.Usage.Output))
		}
	}
}

type metricsWriter struct {
	http.ResponseWriter
	started  time.Time
	first    prometheus.Observer
	status   int
	bytes    uint64
	hijacked bool
}

func (w *metricsWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *metricsWriter) WriteHeader(status int) {
	// Forward even invalid or redundant calls: the underlying writer owns their
	// panic/logging semantics. Informational responses do not commit final status.
	w.ResponseWriter.WriteHeader(status)
	if w.status == 0 && !w.hijacked && (status == http.StatusSwitchingProtocols || status >= 200) {
		w.status = status
	}
}

func (w *metricsWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if !w.hijacked {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		if n > 0 {
			if w.bytes == 0 {
				w.first.Observe(time.Since(w.started).Seconds())
			}
			w.bytes += uint64(n)
		}
	}
	return n, err
}

func (w *metricsWriter) Flush() { _ = w.FlushError() }

func (w *metricsWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil && w.status == 0 && !w.hijacked {
		w.status = http.StatusOK
	}
	return err
}

func (w *metricsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
		// A successful HTTP hijack is the upgrade boundary, even when the
		// handshake bytes are written directly to the returned connection.
		if w.status == 0 {
			w.status = http.StatusSwitchingProtocols
		}
	}
	return conn, buffered, err
}

// Preserve HTTP/2 server push for callers using the standard optional interface.
func (w *metricsWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func metricProtocol(protocol core.Protocol) string {
	switch protocol {
	case "openai-chat", "openai-responses", "openai-completion", "anthropic-messages", "gemini-content", "bedrock-converse", "cohere-v2", "ollama", "native":
		return string(protocol)
	default:
		return "unknown"
	}
}

func metricOperation(operation core.Operation) string {
	switch operation {
	case "generate", "complete", "embed", "rerank", "count_tokens", "image.generate", "image.edit", "image.variation", "audio.speech", "audio.transcribe", "audio.translate", "video", "realtime", "realtime.ticket", "file", "upload", "batch", "response.resource", "conversation.resource", "prediction", "model.list":
		return string(operation)
	default:
		return "unknown"
	}
}

func metricConnector(connector string) string {
	switch connector {
	case "openai", "anthropic", "gemini", "azure-openai", "vertex", "bedrock", "cohere", "ollama", "huggingface", "replicate", "fal", "compatible", "codex-subscription", "claude-subscription", "gemini-cli", "antigravity", "kimi-subscription", "xai-subscription":
		return connector
	default:
		return "unknown"
	}
}

// Bound numeric status labels even for nonstandard upstream HTTP responses.
func metricStatus(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return strconv.Itoa(status)
}
