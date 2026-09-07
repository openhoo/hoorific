package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	es "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/coder/websocket"
	"hoorific/internal/core"
	"hoorific/internal/realtime"
)

// executeRealtime owns transport only. The caller owns the single SQL permit and
// idempotent finalization. Errors are returned only before downstream commitment.
func (g *Gateway) executeRealtime(ctx context.Context, w http.ResponseWriter, r *http.Request, p core.Principal, t selected, b core.Binding, plan core.AttemptPlan, permit core.AttemptPermit, up *http.Request, client *http.Client, id string) (core.AttemptOutcome, error) {
	outcome := core.AttemptOutcome{TenantID: permit.TenantID, RequestID: permit.RequestID, AttemptID: permit.AttemptID, State: "not_executed"}
	if up == nil || up.URL == nil || client == nil {
		return outcome, failure("unavailable", 503, "realtime upstream client is unavailable")
	}
	if err := validateRealtimePolicy(p, b, plan); err != nil {
		return outcome, err
	}
	if b.Framing != "websocket" {
		return outcome, failure("unsupported_operation", 400, "transport is not a WebSocket binding")
	}
	if t.model.ID == "" {
		return outcome, failure("model_not_found", 404, "realtime requires an admitted model")
	}
	machine, err := realtime.NewMachine(realtime.Policy{Protocol: realtime.Protocol(b.Realtime.Protocol), ModelID: t.model.ID, AllowTools: t.model.Features["custom_tools"] == core.Supported, AllowResponseCreate: true, AllowSessionUpdate: true, MaxResponses: b.Realtime.MaxResponses})
	if err != nil {
		return outcome, failure("unsupported_operation", 400, "realtime control protocol unavailable")
	}
	// Origin validation happens before upstream execution. Browser tickets do not
	// grant cross-origin access and upstream credentials are never subprotocols.
	snapshot, err := g.deps.Snapshots.Snapshot(ctx)
	if err != nil {
		return outcome, failure("unavailable", 503, "configuration unavailable")
	}
	if !g.originAllowed(r, snapshot.Policy) {
		return outcome, failure("forbidden", 403, "realtime origin is not allowed")
	}
	var offered []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			token := strings.TrimSpace(part)
			if token == "" || strings.IndexFunc(token, func(c rune) bool { return c <= 32 || c >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", c) }) >= 0 {
				return outcome, failure("invalid_request", 400, "invalid WebSocket subprotocol")
			}
			offered = append(offered, token)
		}
	}
	protocols := make([]string, 0, len(offered))
	for _, v := range offered {
		if contains(b.Realtime.Subprotocols, v) {
			protocols = append(protocols, v)
		}
	}
	maxSession := 60 * time.Minute
	if b.Realtime.MaxSessionSeconds > 0 {
		maxSession = min(maxSession, time.Duration(b.Realtime.MaxSessionSeconds)*time.Second)
	}
	sessionCtx, cancel := context.WithTimeout(ctx, maxSession)
	defer cancel()
	u := *up.URL
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return outcome, failure("unsupported_operation", 400, "invalid realtime endpoint scheme")
	}
	headers := up.Header.Clone()
	for _, key := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol", "Origin"} {
		headers.Del(key)
	}
	if len(b.Realtime.Subprotocols) > 0 && len(protocols) == 0 {
		return outcome, failure("unsupported_operation", 400, "no supported realtime subprotocol was offered")
	}
	outcome.State = "outcome_unknown"
	server, response, err := websocket.Dial(sessionCtx, u.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers, Subprotocols: protocols, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if response != nil {
			if response.Body != nil {
				_ = response.Body.Close()
			}
			if response.StatusCode == 429 || response.StatusCode == 401 || response.StatusCode == 403 {
				outcome.State = "not_executed"
			}
		}
		if response != nil && response.StatusCode == 429 {
			return outcome, failure("quota_exceeded", 429, "realtime upstream rejected the session")
		}
		return outcome, failure("upstream_outcome_unknown", 502, "realtime upstream connection failed")
	}
	defer server.CloseNow()
	if err = g.deps.Admission.MarkAccepted(sessionCtx, permit, "websocket:101"); err != nil {
		outcome.Cancellation = "requested"
		return outcome, failure("upstream_outcome_unknown", 503, "upstream acceptance could not be persisted")
	}
	selectedProtocols := []string(nil)
	if server.Subprotocol() != "" {
		selectedProtocols = []string{server.Subprotocol()}
	}
	// The shared gateway origin policy has already validated Origin, including
	// explicit tenant allowlists. Disable the library's narrower host check so
	// an authorized tenant origin is not rejected a second time.
	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: selectedProtocols, CompressionMode: websocket.CompressionDisabled, InsecureSkipVerify: true})
	if err != nil {
		outcome.Cancellation = "requested"
		return outcome, nil
	} // Accept owns its HTTP error response.
	defer downstream.CloseNow()
	result := realtime.Relay(sessionCtx, downstream, server, realtime.RelayOptions{MaxFrameBytes: eventLimit(sessionCtx), IdleTimeout: 120 * time.Second, MaxSession: maxSession, ValidateClientFrame: func(typ websocket.MessageType, data []byte) error {
		if typ == websocket.MessageBinary {
			if !b.Realtime.AllowBinary {
				return failure("unsupported_feature", 400, "binary realtime frames are not supported")
			}
			// Binary media is opaque; it is bounded by Relay and must not be decoded
			// as JSON or reassembled by the gateway.
			return realtime.ValidateBinaryControl(data)
		}
		return machine.ValidateControl(data)
	}})
	if result.CancellationRequested {
		outcome.Cancellation = "requested"
	}
	// Socket closure is not authoritative terminal usage or stopped billing.
	return outcome, nil
}

func validateRealtimePolicy(p core.Principal, b core.Binding, plan core.AttemptPlan) error {
	if !p.Realtime || !p.NativeAccount {
		return failure("forbidden", 403, "realtime and native account grants are required")
	}
	if b.Realtime == nil {
		return failure("unsupported_operation", 400, "realtime policy descriptor is unavailable")
	}
	if plan.MaximumCost != nil && !b.Realtime.WholeSessionBound {
		return failure("unsupported_policy", 400, "realtime hard spending requires an enforceable whole-session maximum")
	}
	for _, a := range plan.Allowances {
		if (a.Kind == "tokens" || a.Kind == "cost" || a.Kind == "budget") && !b.Realtime.WholeSessionBound {
			return failure("unsupported_policy", 400, "realtime allowance requires an enforceable whole-session maximum")
		}
		if b.Realtime.DirectWebRTC && (a.Kind == "concurrency" || a.Kind == "session_concurrency") {
			return failure("unsupported_policy", 400, "direct media cannot enforce gateway session concurrency")
		}
	}
	if b.Realtime.DirectWebRTC && b.Realtime.RequirePayloadEnforcement {
		return failure("unsupported_policy", 400, "direct media cannot enforce ongoing payload policy")
	}
	return nil
}

// executeWebRTC proxies only the SDP setup exchange. Media remains direct
// client-to-provider; no upstream ephemeral credential is manufactured or
// returned by the gateway.
func (g *Gateway) executeWebRTC(ctx context.Context, w http.ResponseWriter, r *http.Request, p core.Principal, t selected, b core.Binding, plan core.AttemptPlan, permit core.AttemptPermit, up *http.Request, client *http.Client, id string) (core.AttemptOutcome, error) {
	outcome := core.AttemptOutcome{TenantID: permit.TenantID, RequestID: permit.RequestID, AttemptID: permit.AttemptID, State: "not_executed"}
	if up == nil || up.URL == nil || client == nil {
		return outcome, failure("unavailable", 503, "WebRTC upstream client is unavailable")
	}
	if err := validateRealtimePolicy(p, b, plan); err != nil {
		return outcome, err
	}
	if b.Framing != "text/sdp" || !b.Realtime.DirectWebRTC || r.Method != http.MethodPost {
		return outcome, failure("unsupported_operation", 400, "invalid WebRTC setup binding")
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/sdp") {
		return outcome, failure("invalid_request", 400, "WebRTC setup requires application/sdp")
	}
	up.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	outcome.State = "outcome_unknown"
	response, err := client.Do(up)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return outcome, failure("upstream_outcome_unknown", 502, "WebRTC setup outcome is unknown")
	}
	if response == nil || response.Body == nil {
		return outcome, failure("upstream_outcome_unknown", 502, "WebRTC setup returned no response")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return outcome, failure("upstream_error", 502, "WebRTC setup was rejected")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/sdp") {
		return outcome, failure("upstream_error", 502, "WebRTC setup returned an invalid content type")
	}
	sdp, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(sdp) > 1<<20 || (!bytes.HasPrefix(sdp, []byte("v=0\r\n")) && !bytes.HasPrefix(sdp, []byte("v=0\n"))) {
		return outcome, failure("upstream_outcome_unknown", 502, "WebRTC setup returned invalid or oversized SDP")
	}
	if err = g.deps.Admission.MarkAccepted(ctx, permit, "webrtc:2xx"); err != nil {
		outcome.Cancellation = "requested"
		return outcome, failure("upstream_outcome_unknown", 503, "upstream acceptance could not be persisted")
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(response.StatusCode)
	if _, err = w.Write(sdp); err != nil {
		outcome.Cancellation = "requested"
		return outcome, nil
	}
	outcome.State = "settled"
	return outcome, nil
}

func (g *Gateway) executeDuplex(ctx context.Context, w http.ResponseWriter, r *http.Request, p core.Principal, t selected, b core.Binding, plan core.AttemptPlan, permit core.AttemptPermit, lease core.CredentialLease, client *http.Client, id string) (core.AttemptOutcome, error) {
	outcome := core.AttemptOutcome{TenantID: permit.TenantID, RequestID: permit.RequestID, AttemptID: permit.AttemptID, State: "not_executed"}
	if !p.Realtime {
		return outcome, failure("forbidden", 403, "duplex access is not granted")
	}
	if b.Framing != "h2-eventstream-duplex" {
		return outcome, failure("unsupported_operation", 400, "binding is not native duplex")
	}
	if ct := strings.ToLower(r.Header.Get("Content-Type")); ct != "" && !strings.Contains(ct, "eventstream") {
		return outcome, failure("invalid_request", 400, "duplex requires an EventStream body")
	}
	if t.model.ID == "" {
		return outcome, failure("model_not_found", 404, "duplex requires an admitted model")
	}
	if plan.MaximumCost != nil && (b.Realtime == nil || !b.Realtime.WholeSessionBound) {
		return outcome, failure("unsupported_policy", 400, "duplex hard spending requires an enforceable whole-session maximum")
	}
	executor, ok := t.connector.(core.StreamDuplex)
	if !ok {
		return outcome, failure("unsupported_operation", 400, "connector has no native duplex executor")
	}
	if lease == nil || client == nil {
		return outcome, failure("unavailable", 503, "duplex upstream authorization is unavailable")
	}
	var err error
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	var owned io.ReadCloser
	if captured, ok := ctx.Value(requestBodyKey{}).(*capturedBody); ok {
		opened, e := captured.body.Open(runCtx)
		if e != nil {
			return outcome, e
		}
		owned = opened
		body = opened
	}
	if body == nil {
		return outcome, failure("invalid_request", 400, "duplex request body is required")
	}
	var closeBodyOnce sync.Once
	closeBody := func() {
		closeBodyOnce.Do(func() {
			if owned != nil {
				_ = owned.Close()
			} else if r.Body != nil {
				_ = r.Body.Close()
			}
		})
	}
	stopBody := context.AfterFunc(runCtx, closeBody)
	defer stopBody()
	reader := bufio.NewReaderSize(body, 64<<10)
	input := make(chan []byte, 2)
	firstFrameErr := make(chan error, 1)
	readErr := make(chan error, 1)
	readerDone := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(input)
		defer close(readerDone)
		decoder := es.NewDecoder()
		payloadBuf := make([]byte, 128<<10)
		first := true
		signalFirst := func(e error) {
			if !first {
				return
			}
			first = false
			if e != nil {
				firstFrameErr <- e
			}
			close(ready)
		}
		sendReadErr := func(e error) {
			if e == nil || e == io.EOF {
				return
			}
			select {
			case readErr <- e:
			default:
			}
		}
		for {
			msg, e := readEventFrame(reader, decoder, payloadBuf, eventLimit(runCtx))
			if e != nil {
				if first {
					signalFirst(e)
				} else {
					sendReadErr(e)
				}
				return
			}
			chunk, e := decodeBedrockChunk(msg)
			if e != nil {
				if first {
					signalFirst(e)
				} else {
					sendReadErr(e)
				}
				return
			}
			select {
			case input <- chunk:
				signalFirst(nil)
			case <-runCtx.Done():
				signalFirst(runCtx.Err())
				return
			}
		}
	}()
	defer func() {
		cancel()
		closeBody()
		<-readerDone
	}()
	select {
	case <-ready:
	case <-runCtx.Done():
		outcome.State = "not_executed"
		outcome.Cancellation = "requested"
		return outcome, runCtx.Err()
	}
	select {
	case firstErr := <-firstFrameErr:
		outcome.State = "not_executed"
		if firstErr == io.EOF {
			return outcome, failure("invalid_request", 400, "duplex request body is empty")
		}
		return outcome, failure("invalid_request", 400, "invalid duplex request body")
	default:
	}
	committed := false
	accepted := false
	encoder := es.NewEncoder()
	output := func(data []byte) error {
		if !accepted {
			if e := g.deps.Admission.MarkAccepted(runCtx, permit, "eventstream:upstream-event"); e != nil {
				cancel()
				return failure("upstream_outcome_unknown", 503, "upstream acceptance could not be persisted")
			}
			accepted = true
		}
		if len(data) > 64<<10 {
			return fmt.Errorf("duplex output chunk exceeds 64 KiB")
		}
		payload, e := json.Marshal(struct {
			Bytes []byte `json:"bytes"`
		}{Bytes: data})
		if e != nil {
			return e
		}
		msg := es.Message{Headers: es.Headers{{Name: ":message-type", Value: es.StringValue("event")}, {Name: ":event-type", Value: es.StringValue("chunk")}, {Name: ":content-type", Value: es.StringValue("application/json")}}, Payload: payload}
		if !committed {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.WriteHeader(http.StatusOK)
			committed = true
		}
		if e = encoder.Encode(w, msg); e != nil {
			return e
		}
		return http.NewResponseController(w).Flush()
	}
	outcome.State = "outcome_unknown"
	err = executor.ExecuteDuplex(runCtx, t.connection, t.model.ID, lease, client, input, output)
	cancel()
	closeBody()
	<-readerDone
	select {
	case inputErr := <-readErr:
		if err == nil && inputErr != nil {
			err = inputErr
		}
	default:
	}
	if err != nil {
		if ctx.Err() != nil {
			outcome.Cancellation = "requested"
			return outcome, nil
		}
		if committed {
			outcome.Cancellation = "requested"
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			// Preserve native exception framing after HTTP headers are committed.
			// The fixed message discloses no upstream URL, credentials or payload.
			failureEvent := es.Message{Headers: es.Headers{
				{Name: ":message-type", Value: es.StringValue("exception")},
				{Name: ":exception-type", Value: es.StringValue("modelStreamErrorException")},
				{Name: ":content-type", Value: es.StringValue("application/json")},
			}, Payload: []byte(`{"message":"upstream duplex execution failed"}`)}
			if writeErr := encoder.Encode(w, failureEvent); writeErr == nil {
				_ = http.NewResponseController(w).Flush()
			}
			return outcome, nil
		}
		return outcome, failure("upstream_outcome_unknown", 502, "duplex execution outcome is unknown")
	}
	outcome.State = "settled"
	return outcome, nil
}
func readEventFrame(reader *bufio.Reader, decoder *es.Decoder, payload []byte, maxFrame int64) (es.Message, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(reader, prelude[:]); err != nil {
		return es.Message{}, err
	}
	total := int64(binary.BigEndian.Uint32(prelude[0:4]))
	headers := int64(binary.BigEndian.Uint32(prelude[4:8]))
	if total < 16 || total > maxFrame || headers > total-16 || total-headers-16 > 128<<10 {
		return es.Message{}, fmt.Errorf("invalid or oversized EventStream frame")
	}
	framed := io.MultiReader(bytes.NewReader(prelude[:]), io.LimitReader(reader, total-12))
	return decoder.Decode(framed, payload[:0])
}

func decodeBedrockChunk(msg es.Message) ([]byte, error) {
	messageType, ok := msg.Headers.Get(":message-type").(es.StringValue)
	if !ok || string(messageType) != "event" {
		return nil, fmt.Errorf("unsupported EventStream message type")
	}
	eventType, ok := msg.Headers.Get(":event-type").(es.StringValue)
	if !ok || string(eventType) != "chunk" {
		return nil, fmt.Errorf("unsupported Bedrock duplex event")
	}
	contentType, ok := msg.Headers.Get(":content-type").(es.StringValue)
	if !ok || !strings.EqualFold(string(contentType), "application/json") {
		return nil, fmt.Errorf("unsupported Bedrock duplex content type")
	}
	fields, err := strictObject(msg.Payload)
	if err != nil {
		return nil, fmt.Errorf("invalid Bedrock duplex payload: %w", err)
	}
	for key := range fields {
		if key != "bytes" {
			return nil, fmt.Errorf("unknown Bedrock duplex payload field: %s", key)
		}
	}
	raw, ok := fields["bytes"]
	if !ok || string(raw) == "null" {
		return nil, fmt.Errorf("Bedrock duplex chunk bytes are required")
	}
	var chunk []byte
	if err = json.Unmarshal(raw, &chunk); err != nil {
		return nil, fmt.Errorf("invalid Bedrock duplex chunk bytes: %w", err)
	}
	if len(chunk) > 64<<10 {
		return nil, fmt.Errorf("Bedrock duplex chunk exceeds 64 KiB")
	}
	return chunk, nil
}
