package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const codexParityUpstreamSecret = "verify-operation-secret"

// This is the smallest Responses stream accepted by both the native gateway
// codec and Codex exec. Keep it deterministic so both runs prove successful
// Responses consumption; request bytes and header values are compared separately.
const codexParitySSE = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_parity","object":"response","created_at":1,"status":"in_progress","model":"gpt-5.4","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_parity","type":"message","status":"in_progress","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_parity","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_parity","output_index":0,"content_index":0,"delta":"PARITY_OK"}

event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":4,"item_id":"msg_parity","output_index":0,"content_index":0,"text":"PARITY_OK"}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":"msg_parity","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"PARITY_OK","annotations":[]}]}}

event: response.completed
data: {"type":"response.completed","sequence_number":6,"response":{"id":"resp_parity","object":"response","created_at":1,"status":"completed","model":"gpt-5.4","output":[{"id":"msg_parity","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"PARITY_OK","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}

`

const codexParityBodyLimit = 16 << 20
const codexParityOutputLimit = 1 << 20

type codexParityRequest struct {
	method        string
	proto         string
	effectivePath string
	headers       map[string][]string
	body          []byte
}

type codexParityProxy struct {
	server          *httptest.Server
	gatewayAddress  string
	connectionID    string
	gatewayKey      string
	upstreamKey     string
	client          *http.Client
	forwardMu       sync.Mutex
	expectationMu   sync.Mutex
	direct          []codexParityRequest
	ingress         []codexParityRequest
	expected        []codexParityRequest
	egress          []codexParityRequest
	pairs           int
	unmatchedEgress int
	methodDiff      int
	protocolDiff    int
	pathDiff        int
	headerDiff      int
	bodyDiff        int
	differences     []string
	faults          []string
}

func newCodexParityProxy(gatewayAddress, connectionID, gatewayKey string) *codexParityProxy {
	transport, _ := http.DefaultTransport.(*http.Transport)
	if transport != nil {
		transport = transport.Clone()
		transport.DisableCompression = true
	}
	proxy := &codexParityProxy{
		gatewayAddress: gatewayAddress,
		connectionID:   connectionID,
		gatewayKey:     gatewayKey,
		upstreamKey:    codexParityUpstreamSecret,
		client: &http.Client{
			Transport:     transport,
			Timeout:       45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	proxy.server = httptest.NewServer(http.HandlerFunc(proxy.serveHTTP))
	return proxy
}

func (p *codexParityProxy) close() {
	if p != nil && p.server != nil {
		p.server.Close()
	}
}

func (p *codexParityProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/direct/v1/responses"):
		p.serveDirect(w, r)
	case strings.HasPrefix(r.URL.Path, "/ingress/v1/responses"):
		p.serveIngress(w, r)
	case strings.HasPrefix(r.URL.Path, "/egress/v1/responses"):
		p.serveEgress(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *codexParityProxy) serveDirect(w http.ResponseWriter, r *http.Request) {
	observed, err := codexParityReadRequest(r, "/direct")
	if err != nil {
		p.recordFault("direct request could not be recorded")
		http.Error(w, "direct recording failed", http.StatusBadRequest)
		return
	}
	p.expectationMu.Lock()
	p.direct = append(p.direct, observed)
	p.expectationMu.Unlock()
	if observed.method != http.MethodPost {
		p.recordFault("direct request method was not POST")
		http.Error(w, "Responses parity requires POST", http.StatusMethodNotAllowed)
		return
	}
	if !codexParityHasCredential(observed.headers, p.upstreamKey) {
		p.recordFault("direct request did not carry the synthetic upstream credential")
		http.Error(w, "synthetic credential rejected", http.StatusUnauthorized)
		return
	}
	codexParityWriteSSE(w)
}

func (p *codexParityProxy) serveIngress(w http.ResponseWriter, r *http.Request) {
	// A single in-flight ingress request makes FIFO expectation pairing
	// deterministic if a future Codex release emits concurrent turns.
	p.forwardMu.Lock()
	defer p.forwardMu.Unlock()

	observed, err := codexParityReadRequest(r, "/ingress")
	if err != nil {
		p.recordFault("ingress request could not be recorded")
		http.Error(w, "ingress recording failed", http.StatusBadRequest)
		return
	}
	p.expectationMu.Lock()
	p.ingress = append(p.ingress, observed)
	p.expected = append(p.expected, observed)
	p.expectationMu.Unlock()
	if observed.method != http.MethodPost {
		p.recordFault("ingress request method was not POST")
		http.Error(w, "Responses parity requires POST", http.StatusMethodNotAllowed)
		return
	}
	if !codexParityHasCredential(observed.headers, p.upstreamKey) {
		p.recordFault("ingress request did not carry the synthetic upstream credential")
		http.Error(w, "synthetic credential rejected", http.StatusUnauthorized)
		return
	}

	target := &url.URL{
		Scheme: "http",
		Host:   p.gatewayAddress,
		Path:   "/connect/" + url.PathEscape(p.connectionID) + "/native/responses",
	}
	target.RawQuery = r.URL.RawQuery
	forward, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(observed.body))
	if err != nil {
		p.recordFault("ingress gateway request could not be constructed")
		http.Error(w, "gateway request construction failed", http.StatusBadGateway)
		return
	}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "host" || lower == "content-length" {
			continue
		}
		for _, value := range values {
			forward.Header.Add(name, value)
		}
	}
	// Remove every case variant before setting the temporary gateway key. The
	// net/http map is normally canonicalized, but iterating above deliberately
	// treats a raw duplicate with arbitrary casing as protected too.
	for name := range forward.Header {
		if strings.EqualFold(name, "Authorization") {
			delete(forward.Header, name)
		}
	}
	forward.Header.Set("Authorization", "Bearer "+p.gatewayKey)

	response, err := p.client.Do(forward)
	if err != nil {
		p.recordFault("ingress forwarding to the gateway failed")
		http.Error(w, "gateway forwarding failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, codexParityBodyLimit+1))
	if readErr != nil || len(body) > codexParityBodyLimit {
		p.recordFault("gateway response exceeded the recording bound")
		http.Error(w, "gateway response recording failed", http.StatusBadGateway)
		return
	}
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func (p *codexParityProxy) serveEgress(w http.ResponseWriter, r *http.Request) {
	observed, err := codexParityReadRequest(r, "/egress")
	if err != nil {
		p.recordFault("egress request could not be recorded")
		http.Error(w, "egress recording failed", http.StatusBadRequest)
		return
	}
	p.expectationMu.Lock()
	p.egress = append(p.egress, observed)
	var expected *codexParityRequest
	pair := 0
	if len(p.expected) != 0 {
		copyOfExpected := p.expected[0]
		p.expected = p.expected[1:]
		p.pairs++
		pair = p.pairs
		expected = &copyOfExpected
	} else {
		p.unmatchedEgress++
	}
	p.expectationMu.Unlock()
	if expected != nil {
		p.compare(pair, *expected, observed)
	}
	if observed.method != http.MethodPost {
		p.recordFault("egress request method was not POST")
		http.Error(w, "Responses parity requires POST", http.StatusMethodNotAllowed)
		return
	}
	if !codexParityHasCredential(observed.headers, p.upstreamKey) {
		p.recordFault("egress request did not carry the configured upstream credential")
		http.Error(w, "configured credential rejected", http.StatusUnauthorized)
		return
	}
	codexParityWriteSSE(w)
}

func (p *codexParityProxy) compare(pair int, expected, actual codexParityRequest) {
	methodDiff := expected.method != actual.method
	protocolDiff := expected.proto != actual.proto
	pathDiff := expected.effectivePath != actual.effectivePath
	headerDiff := !codexParityHeadersEqual(expected.headers, actual.headers)
	bodyDiff := !bytes.Equal(expected.body, actual.body)
	p.expectationMu.Lock()
	if methodDiff {
		p.methodDiff++
		p.addDifferenceLocked(fmt.Sprintf("pair-%d:method", pair))
	}
	if protocolDiff {
		p.protocolDiff++
		p.addDifferenceLocked(fmt.Sprintf("pair-%d:protocol", pair))
	}
	if pathDiff {
		p.pathDiff++
		p.addDifferenceLocked(fmt.Sprintf("pair-%d:effective_path", pair))
	}
	if headerDiff {
		p.headerDiff++
		p.addDifferenceLocked(fmt.Sprintf("pair-%d:headers", pair))
	}
	if bodyDiff {
		p.bodyDiff++
		p.addDifferenceLocked(fmt.Sprintf("pair-%d:body", pair))
	}
	p.expectationMu.Unlock()
}

func (p *codexParityProxy) addDifferenceLocked(value string) {
	if len(p.differences) < 16 {
		p.differences = append(p.differences, value)
	}
}

func (p *codexParityProxy) recordFault(value string) {
	p.expectationMu.Lock()
	if len(p.faults) < 16 {
		p.faults = append(p.faults, value)
	}
	p.expectationMu.Unlock()
}

func codexParityReadRequest(r *http.Request, instrumentationPrefix string) (codexParityRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, codexParityBodyLimit+1))
	if err != nil {
		return codexParityRequest{}, err
	}
	if len(body) > codexParityBodyLimit {
		return codexParityRequest{}, errors.New("request body exceeded recording bound")
	}
	requestURI := r.URL.RequestURI()
	if requestURI == "" {
		requestURI = r.URL.Path
	}
	effectivePath := requestURI
	if strings.HasPrefix(effectivePath, instrumentationPrefix) {
		effectivePath = strings.TrimPrefix(effectivePath, instrumentationPrefix)
	}
	headers := codexParityNormalizeHeaders(r)
	return codexParityRequest{
		method:        r.Method,
		proto:         r.Proto,
		effectivePath: effectivePath,
		headers:       headers,
		body:          body,
	}, nil
}

func codexParityNormalizeHeaders(r *http.Request) map[string][]string {
	normalized := make(map[string][]string, len(r.Header)+2)
	for name, values := range r.Header {
		key := strings.ToLower(name)
		normalized[key] = append(normalized[key], values...)
	}
	// Host is special in net/http and is not present in Request.Header.
	normalized["host"] = []string{r.Host}
	return normalized
}

func codexParityHeadersEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, values := range a {
		other, ok := b[name]
		if !ok || len(values) != len(other) {
			return false
		}
		for i := range values {
			if values[i] != other[i] {
				return false
			}
		}
	}
	return true
}

func codexParityHasCredential(headers map[string][]string, secret string) bool {
	values := headers["authorization"]
	return len(values) == 1 && values[0] == "Bearer "+secret
}

func codexParityWriteSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Length", strconv.Itoa(len(codexParitySSE)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, codexParitySSE)
}

func (p *codexParityProxy) evidence(connectionID string) map[string]any {
	p.expectationMu.Lock()
	direct := append([]codexParityRequest(nil), p.direct...)
	ingress := append([]codexParityRequest(nil), p.ingress...)
	egress := append([]codexParityRequest(nil), p.egress...)
	unmatchedIngress := len(p.expected)
	pairs := p.pairs
	unmatchedEgress := p.unmatchedEgress
	methodDiff := p.methodDiff
	protocolDiff := p.protocolDiff
	pathDiff := p.pathDiff
	headerDiff := p.headerDiff
	bodyDiff := p.bodyDiff
	differences := append([]string(nil), p.differences...)
	faults := append([]string(nil), p.faults...)
	p.expectationMu.Unlock()

	firstIngressProto := codexParityFirstProtocol(ingress)
	firstEgressProto := codexParityFirstProtocol(egress)
	hostEqual := false
	if len(ingress) != 0 && len(egress) != 0 {
		hostEqual = codexParityFirstHeader(ingress[0], "host") == codexParityFirstHeader(egress[0], "host")
	}
	return map[string]any{
		"direct_requests":            len(direct),
		"ingress_requests":           len(ingress),
		"egress_requests":            len(egress),
		"paired_requests":            pairs,
		"unmatched_ingress":          unmatchedIngress,
		"unmatched_egress":           unmatchedEgress,
		"method_differences":         methodDiff,
		"protocol_differences":       protocolDiff,
		"effective_path_differences": pathDiff,
		"header_differences":         headerDiff,
		"body_differences":           bodyDiff,
		"differences":                differences,
		"proxy_faults":               faults,
		"header_names": map[string]any{
			"direct":  codexParityHeaderNames(direct),
			"ingress": codexParityHeaderNames(ingress),
			"egress":  codexParityHeaderNames(egress),
		},
		"identities": map[string]any{
			"direct":  codexParityIdentities(direct, p.upstreamKey, p.gatewayKey),
			"ingress": codexParityIdentities(ingress, p.upstreamKey, p.gatewayKey),
			"egress":  codexParityIdentities(egress, p.upstreamKey, p.gatewayKey),
		},
		"body_sha256": map[string]any{
			"direct":  codexParityBodyHashes(direct),
			"ingress": codexParityBodyHashes(ingress),
			"egress":  codexParityBodyHashes(egress),
		},
		"body_bytes": map[string]any{
			"direct":  codexParityBodySizes(direct),
			"ingress": codexParityBodySizes(ingress),
			"egress":  codexParityBodySizes(egress),
		},
		"caller_protocol": firstIngressProto,
		"egress_protocol": firstEgressProto,
		"same_proxy_host": hostEqual,
		"transport_claim": map[string]any{
			"tls":                       false,
			"raw_header_order_compared": false,
			"note":                      "HTTP/1.1 loopback only; header casing and wire ordering are not claimed",
		},
		"route_rewrite": map[string]any{
			"ingress":       "/ingress/v1/responses",
			"gateway":       "/connect/" + url.PathEscape(connectionID) + "/native/responses",
			"egress":        "/egress/v1/responses",
			"comparison":    "Only the local /ingress and /egress instrumentation prefixes are removed; /v1/responses and its query are compared exactly",
			"host_contract": "Ingress and egress use this same recording-proxy host",
		},
	}
}

func (p *codexParityProxy) validationError() error {
	p.expectationMu.Lock()
	defer p.expectationMu.Unlock()
	if len(p.faults) != 0 {
		return fmt.Errorf("recording proxy contract failures: %s", strings.Join(p.faults, "; "))
	}
	if p.pairs == 0 {
		return errors.New("no paired Codex request reached the gateway")
	}
	if len(p.expected) != 0 || p.unmatchedEgress != 0 {
		return fmt.Errorf("unmatched parity expectations: ingress=%d egress=%d", len(p.expected), p.unmatchedEgress)
	}
	if p.methodDiff != 0 || p.protocolDiff != 0 || p.pathDiff != 0 || p.headerDiff != 0 || p.bodyDiff != 0 {
		return fmt.Errorf("paired request differences: method=%d protocol=%d effective_path=%d headers=%d body=%d", p.methodDiff, p.protocolDiff, p.pathDiff, p.headerDiff, p.bodyDiff)
	}
	return nil
}

func codexParityFirstProtocol(requests []codexParityRequest) string {
	if len(requests) == 0 {
		return ""
	}
	return requests[0].proto
}

func codexParityFirstHeader(request codexParityRequest, name string) string {
	values := request.headers[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func codexParityHeaderNames(requests []codexParityRequest) []string {
	set := map[string]struct{}{}
	for _, request := range requests {
		for name := range request.headers {
			set[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func codexParityIdentities(requests []codexParityRequest, upstreamKey, gatewayKey string) map[string]any {
	out := map[string]any{"originator": []string{}, "user_agent": []string{}}
	for _, request := range requests {
		for _, name := range []string{"originator", "user-agent"} {
			values := request.headers[name]
			for _, value := range values {
				safe := codexParityRedact(value, upstreamKey, gatewayKey)
				if len(safe) > 512 {
					safe = safe[:512] + "[truncated]"
				}
				key := "originator"
				if name == "user-agent" {
					key = "user_agent"
				}
				list := out[key].([]string)
				if !containsString(list, safe) {
					out[key] = append(list, safe)
				}
			}
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func codexParityRedact(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func codexParityBodyHashes(requests []codexParityRequest) []string {
	out := make([]string, 0, len(requests))
	for _, request := range requests {
		digest := sha256.Sum256(request.body)
		out = append(out, hex.EncodeToString(digest[:]))
	}
	return out
}

func codexParityBodySizes(requests []codexParityRequest) []int {
	out := make([]int, 0, len(requests))
	for _, request := range requests {
		out = append(out, len(request.body))
	}
	return out
}

type codexParityOutput struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (o *codexParityOutput) Write(data []byte) (int, error) {
	if o.limit <= o.buf.Len() {
		o.truncated = true
		return len(data), nil
	}
	remaining := o.limit - o.buf.Len()
	if len(data) > remaining {
		_, _ = o.buf.Write(data[:remaining])
		o.truncated = true
		return len(data), nil
	}
	_, _ = o.buf.Write(data)
	return len(data), nil
}

func (o *codexParityOutput) Bytes() []byte { return o.buf.Bytes() }

type codexParityExecution struct {
	JSONLines      int
	AgentMessages  int
	ToolEvents     int
	AgentMessageOK bool
	StdoutBytes    int
	StderrBytes    int
	StdoutCapped   bool
	StderrCapped   bool
}

func (e codexParityExecution) evidence() map[string]any {
	return map[string]any{
		"json_lines":       e.JSONLines,
		"agent_messages":   e.AgentMessages,
		"agent_message_ok": e.AgentMessageOK,
		"tool_events":      e.ToolEvents,
		"stdout_bytes":     e.StdoutBytes,
		"stderr_bytes":     e.StderrBytes,
		"stdout_capped":    e.StdoutCapped,
		"stderr_capped":    e.StderrCapped,
	}
}

func runCodexParity(ctx context.Context, binary, baseURL, work, home, codexHome string) (codexParityExecution, error) {
	if err := os.MkdirAll(work, 0700); err != nil {
		return codexParityExecution{}, err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return codexParityExecution{}, err
	}
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		return codexParityExecution{}, err
	}
	provider := fmt.Sprintf(`{name="parity",base_url=%s,env_key="PARITY_API_KEY",wire_api="responses",request_max_retries=0,stream_max_retries=0}`, strconv.Quote(baseURL))
	args := []string{
		"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check",
		"--json", "--color", "never", "-C", work, "-s", "read-only", "-m", "gpt-5.4",
		"-c", `model_provider="parity"`,
		"-c", "model_providers.parity=" + provider,
		"-c", `web_search="disabled"`,
		"-c", `approval_policy="never"`,
		"Reply exactly PARITY_OK. Do not use tools.",
	}
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, binary, args...)
	command.Dir = work
	command.Env = []string{
		"PATH=" + pathEnv,
		"HOME=" + home,
		"CODEX_HOME=" + codexHome,
		"TERM=dumb",
		"LANG=C.UTF-8",
		"PARITY_API_KEY=" + codexParityUpstreamSecret,
	}
	// An explicit empty reader gives exec a closed stdin instead of inheriting
	// the verifier's terminal, where Codex would wait for more prompt input.
	command.Stdin = bytes.NewReader(nil)
	var stdout, stderr codexParityOutput
	stdout.limit, stderr.limit = codexParityOutputLimit, codexParityOutputLimit
	command.Stdout, command.Stderr = &stdout, &stderr
	commandErr := command.Run()
	execution, parseErr := codexParityParseJSONL(stdout.Bytes())
	execution.StdoutBytes = len(stdout.Bytes())
	execution.StderrBytes = len(stderr.Bytes())
	execution.StdoutCapped = stdout.truncated
	execution.StderrCapped = stderr.truncated
	if parseErr != nil {
		return execution, parseErr
	}
	if commandErr != nil {
		if runCtx.Err() != nil {
			return execution, errors.New("Codex execution exceeded the 45 second bound")
		}
		return execution, errors.New("Codex execution exited unsuccessfully")
	}
	if !execution.AgentMessageOK {
		return execution, errors.New("Codex JSONL did not contain an agent_message with exact PARITY_OK")
	}
	if execution.ToolEvents != 0 {
		return execution, errors.New("Codex JSONL reported tool execution despite the no-tools prompt")
	}
	return execution, nil
}

func codexParityParseJSONL(raw []byte) (codexParityExecution, error) {
	var execution codexParityExecution
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		execution.JSONLines++
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return execution, errors.New("Codex --json emitted malformed JSONL")
		}
		if codexParityContainsToolEvent(event) {
			execution.ToolEvents++
		}
		if typ, _ := event["type"].(string); typ == "item.completed" {
			if item, ok := event["item"].(map[string]any); ok {
				if itemType, _ := item["type"].(string); itemType == "agent_message" {
					execution.AgentMessages++
					if text, _ := item["text"].(string); text == "PARITY_OK" {
						execution.AgentMessageOK = true
					} else {
						return execution, errors.New("Codex agent_message was not exactly PARITY_OK")
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return execution, errors.New("Codex JSONL output could not be read")
	}
	if execution.JSONLines == 0 {
		return execution, errors.New("Codex --json emitted no JSONL")
	}
	if execution.AgentMessages == 0 || !execution.AgentMessageOK {
		return execution, errors.New("Codex JSONL did not contain an agent_message with exact PARITY_OK")
	}
	if execution.ToolEvents != 0 {
		return execution, errors.New("Codex JSONL reported tool execution")
	}
	return execution, nil
}

func codexParityContainsToolEvent(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		if typ, ok := item["type"].(string); ok && codexParityToolType(typ) {
			return true
		}
		for _, nested := range item {
			if codexParityContainsToolEvent(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range item {
			if codexParityContainsToolEvent(nested) {
				return true
			}
		}
	}
	return false
}

func codexParityToolType(value string) bool {
	typ := strings.ToLower(strings.TrimSpace(value))
	switch typ {
	case "command_execution", "web_search_call", "file_search_call", "computer_call", "function_call", "custom_tool_call", "mcp_tool_call", "tool_call", "tool_use", "local_shell_call", "local_shell_output":
		return true
	default:
		return strings.Contains(typ, "tool") || strings.Contains(typ, "command_execution") || strings.Contains(typ, "web_search")
	}
}

func codexParityExecutable() (string, string) {
	if configured := strings.TrimSpace(os.Getenv("HOORIFIC_VERIFY_CODEX")); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", "HOORIFIC_VERIFY_CODEX is not an executable path; install Codex or set HOORIFIC_VERIFY_CODEX to its path"
		}
		if info, statErr := os.Stat(absolute); statErr != nil || info.IsDir() {
			return "", "Codex executable is missing; install Codex or set HOORIFIC_VERIFY_CODEX to its path"
		}
		return absolute, ""
	}
	found, err := exec.LookPath("codex")
	if err != nil {
		return "", "Codex executable is missing; install Codex or set HOORIFIC_VERIFY_CODEX to its path"
	}
	absolute, err := filepath.Abs(found)
	if err != nil {
		return "", "Codex executable path could not be resolved; install Codex or set HOORIFIC_VERIFY_CODEX"
	}
	return absolute, ""
}

func codexParityUpdateResource(e *environment, path string, mutate func(map[string]any)) error {
	current, err := e.extRequest(context.Background(), true, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	if current.Status != http.StatusOK {
		return fmt.Errorf("resource read returned HTTP %d", current.Status)
	}
	var resource struct {
		Version int64          `json:"version"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(current.Body), &resource); err != nil || resource.Data == nil {
		return errors.New("resource read returned no editable data")
	}
	mutate(resource.Data)
	body, err := json.Marshal(map[string]any{"data": resource.Data})
	if err != nil {
		return err
	}
	updated, err := e.extRequest(context.Background(), true, http.MethodPut, path, body, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(resource.Version, 10))
	})
	if err != nil {
		return err
	}
	if updated.Status < 200 || updated.Status >= 300 {
		return fmt.Errorf("resource update returned HTTP %d", updated.Status)
	}
	return nil
}

// codexParityScenario is deliberately opt-in. It executes the real Codex
// binary twice against local recording infrastructure and never substitutes a
// synthetic request when Codex is unavailable.
func (e *environment) codexParityScenario() result {
	start := time.Now()
	name := "codex/parity"
	if e == nil || e.client == nil || e.management == "" || e.inference == "" || e.cookie == "" || e.csrf == "" || e.fixture == nil {
		return extResult(name, start, nil, errors.New("isolated gateway, fixture, and administration session are required"))
	}
	binary, missing := codexParityExecutable()
	if missing != "" {
		return result{Name: name, Status: "not-run", Detail: missing, DurationMS: time.Since(start).Milliseconds(), Evidence: map[string]any{"real_codex_required": true, "request_source": "installed Codex executable"}}
	}

	fixture, err := e.operationFixture("codex-parity", "openai", []string{"generate"}, func(w http.ResponseWriter, _ *http.Request) error {
		codexParityWriteSSE(w)
		return nil
	})
	if err != nil {
		return extResult(name, start, nil, fmt.Errorf("fixture setup failed: %w", err))
	}
	defer fixture.server.Close()

	proxy := newCodexParityProxy(e.inference, fixture.id, fixture.key)
	defer proxy.close()
	codexRoot := filepath.Join(e.root, "codex-parity")
	defer os.RemoveAll(codexRoot)

	connectionPath := "/admin/api/v1/connections/" + url.PathEscape(fixture.id)
	if err := codexParityUpdateResource(e, connectionPath, func(data map[string]any) {
		data["base_url"] = proxy.server.URL + "/egress/v1"
		data["client_profile"] = map[string]any{"preset": "codex-passthrough"}
	}); err != nil {
		return extResult(name, start, proxy.evidence(fixture.id), fmt.Errorf("connection profile setup failed: %w", err))
	}
	modelPath := "/admin/api/v1/models/" + url.PathEscape(fixture.id+"-model")
	if err := codexParityUpdateResource(e, modelPath, func(data map[string]any) {
		data["upstream_id"] = "gpt-5.4"
		data["output_limit"] = 4096
		data["features"] = map[string]string{
			"custom_tools":      "supported",
			"structured_output": "supported",
			"streaming":         "supported",
			"parallel_tools":    "supported",
			"reasoning_blocks":  "supported",
			"citations":         "supported",
		}
		data["price"] = map[string]any{
			"version":            "fixture",
			"input_per_million":  int64(0),
			"output_per_million": int64(0),
		}
	}); err != nil {
		return extResult(name, start, proxy.evidence(fixture.id), fmt.Errorf("model fixture setup failed: %w", err))
	}

	directHome := filepath.Join(codexRoot, "direct-home")
	directCodexHome := filepath.Join(codexRoot, "direct-codex-home")
	directWork := filepath.Join(codexRoot, "direct-work")
	direct, directErr := runCodexParity(context.Background(), binary, proxy.server.URL+"/direct/v1", directWork, directHome, directCodexHome)
	evidence := proxy.evidence(fixture.id)
	evidence["direct_execution"] = direct.evidence()
	if directErr != nil {
		return extResult(name, start, evidence, fmt.Errorf("direct Codex execution failed: %w", directErr))
	}

	gatewayHome := filepath.Join(codexRoot, "gateway-home")
	gatewayCodexHome := filepath.Join(codexRoot, "gateway-codex-home")
	gatewayWork := filepath.Join(codexRoot, "gateway-work")
	gateway, gatewayErr := runCodexParity(context.Background(), binary, proxy.server.URL+"/ingress/v1", gatewayWork, gatewayHome, gatewayCodexHome)
	evidence = proxy.evidence(fixture.id)
	evidence["direct_execution"] = direct.evidence()
	evidence["gateway_execution"] = gateway.evidence()
	if gatewayErr != nil {
		return extResult(name, start, evidence, fmt.Errorf("gateway Codex execution failed: %w", gatewayErr))
	}
	if err := proxy.validationError(); err != nil {
		return extResult(name, start, evidence, err)
	}
	return extResult(name, start, evidence, nil)
}
