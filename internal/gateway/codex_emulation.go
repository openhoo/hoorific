package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
	"hoorific/internal/transport"
)

const (
	codexWireMaxBodyBytes  = 32 << 20
	codexWireMaxEventBytes = 32 << 20
)

var errCodexStreamTruncated = errors.New("Codex Responses stream ended before a terminal event")

func codexInvocationError(param, message string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: http.StatusBadRequest, Param: param, Message: message, Origin: "gateway"}
}

func codexUnsupported(message string) error {
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: http.StatusBadRequest, Param: "client_profile.preset", Message: message, Origin: "gateway"}
}

func codexProfile(profile *core.ClientProfile) bool {
	return profile != nil && profile.EmulatesCodex()
}

func codexRequestBody(profile *core.ClientProfile, body []byte) ([]byte, core.CodexInvocation, error) {
	if !codexProfile(profile) {
		return body, core.CodexInvocation{}, nil
	}
	if len(body) == 0 {
		return nil, core.CodexInvocation{}, codexInvocationError("body", "codex-exec requires a JSON Responses request")
	}
	fields, err := strictObject(body)
	if err != nil {
		return nil, core.CodexInvocation{}, codexInvocationError("body", "codex-exec requires a JSON Responses request")
	}
	invocation, err := core.NewCodexInvocation(profile.InstallationID)
	if err != nil {
		return nil, core.CodexInvocation{}, codexInvocationError("client_profile.installation_id", "codex-exec identity is unavailable")
	}
	if err := validateCodexControls(fields); err != nil {
		return nil, core.CodexInvocation{}, err
	}
	input, err := normalizeCodexInput(fields["input"])
	if err != nil {
		return nil, core.CodexInvocation{}, err
	}
	fields["input"] = input
	metadata, err := invocation.ClientMetadata()
	if err != nil {
		return nil, core.CodexInvocation{}, codexInvocationError("client_profile", "codex-exec metadata is unavailable")
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, core.CodexInvocation{}, codexInvocationError("client_profile", "codex-exec metadata is unavailable")
	}
	fields["store"] = json.RawMessage("false")
	fields["stream"] = json.RawMessage("true")
	fields["tool_choice"] = codexDefaultJSON(fields, "tool_choice", []byte(`"auto"`))
	fields["parallel_tool_calls"] = codexDefaultJSON(fields, "parallel_tool_calls", []byte("true"))
	fields["include"] = codexInclude(fields["include"])
	fields["prompt_cache_key"] = json.RawMessage(strconvQuote(invocation.SessionID))
	fields["client_metadata"] = metadataJSON
	if _, ok := fields["instructions"]; !ok {
		fields["instructions"] = json.RawMessage(`""`)
	}
	if _, ok := fields["tools"]; !ok {
		fields["tools"] = json.RawMessage(`[]`)
	}
	projected, err := encodeCodexRequest(fields)
	if err != nil {
		return nil, core.CodexInvocation{}, codexInvocationError("body", "codex-exec request could not be encoded")
	}
	if len(projected) > codexWireMaxBodyBytes {
		return nil, core.CodexInvocation{}, codexInvocationError("body", "codex-exec request exceeds the native wire size limit")
	}
	return projected, invocation, nil
}

var codexRequestFieldOrder = [...]string{
	"model",
	"instructions",
	"input",
	"tools",
	"tool_choice",
	"parallel_tool_calls",
	"reasoning",
	"store",
	"stream",
	"include",
	"prompt_cache_key",
	"text",
	"client_metadata",
}

func encodeCodexRequest(fields map[string]json.RawMessage) ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	writeField := func(key string, value json.RawMessage) error {
		if !json.Valid(value) {
			return errors.New("invalid Codex request field")
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return err
		}
		out.Write(encodedKey)
		out.WriteByte(':')
		out.Write(value)
		return nil
	}
	known := make(map[string]struct{}, len(codexRequestFieldOrder))
	for _, key := range codexRequestFieldOrder {
		known[key] = struct{}{}
		if value, ok := fields[key]; ok {
			if err := writeField(key, value); err != nil {
				return nil, err
			}
		}
	}
	remaining := make([]string, 0, len(fields))
	for key := range fields {
		if _, ok := known[key]; !ok {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	for _, key := range remaining {
		if err := writeField(key, fields[key]); err != nil {
			return nil, err
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func validateCodexControls(fields map[string]json.RawMessage) error {
	if value, ok := fields["store"]; ok {
		var store bool
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &store) != nil || store {
			return codexInvocationError("store", "codex-exec requires store:false")
		}
	}
	if value, ok := fields["stream"]; ok {
		var stream bool
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &stream) != nil {
			return codexInvocationError("stream", "stream must be a boolean")
		}
	}
	if value, ok := fields["background"]; ok {
		var background bool
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &background) != nil {
			return codexInvocationError("background", "background must be false for codex-exec")
		}
		if background {
			return codexInvocationError("background", "codex-exec does not support background responses")
		}
	}
	for _, field := range []string{"client_metadata", "prompt_cache_key", "prompt_cache_retention", "prompt_cache_options"} {
		if _, ok := fields[field]; ok {
			return codexInvocationError(field, "codex-exec owns this request identity")
		}
	}
	if value, ok := fields["include"]; ok {
		var include []string
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &include) != nil {
			return codexInvocationError("include", "include must be an array of strings")
		}
		for _, item := range include {
			if item == "" || strings.ContainsAny(item, "\x00\r\n") {
				return codexInvocationError("include", "include contains an invalid value")
			}
		}
	}
	if value, ok := fields["tool_choice"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return codexInvocationError("tool_choice", "tool_choice cannot be null")
		}
		var valid any
		if json.Unmarshal(value, &valid) != nil {
			return codexInvocationError("tool_choice", "tool_choice is invalid")
		}
	}
	if value, ok := fields["parallel_tool_calls"]; ok {
		var parallel bool
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &parallel) != nil {
			return codexInvocationError("parallel_tool_calls", "parallel_tool_calls must be a boolean")
		}
	}
	for _, field := range []string{"reasoning", "text"} {
		if value, ok := fields[field]; ok {
			var object map[string]json.RawMessage
			if json.Unmarshal(value, &object) != nil || object == nil {
				return codexInvocationError(field, field+" must be an object")
			}
		}
	}
	return nil
}

func normalizeCodexInput(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, codexInvocationError("input", "input is required")
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		message, err := codexMessage("user", json.RawMessage(strconvQuote(text)), true)
		if err != nil {
			return nil, err
		}
		return json.Marshal([]map[string]json.RawMessage{message})
	}
	var items []json.RawMessage
	if json.Unmarshal(value, &items) != nil {
		return nil, codexInvocationError("input", "input must be a string or array")
	}
	normalized := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		converted, err := normalizeCodexInputItem(item)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, converted)
	}
	return json.Marshal(normalized)
}

func normalizeCodexInputItem(value json.RawMessage) (json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, codexInvocationError("input", "input items must be objects or strings")
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		message, err := codexMessage("user", json.RawMessage(strconvQuote(text)), true)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(message)
		return encoded, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(value, &object) != nil || object == nil {
		return nil, codexInvocationError("input", "input items must be objects")
	}
	var typ, role string
	_ = json.Unmarshal(object["type"], &typ)
	_ = json.Unmarshal(object["role"], &role)
	if typ != "" && typ != "message" {
		return value, nil
	}
	if role == "" {
		return value, nil
	}
	return codexMessageObject(object, role)
}

func codexMessage(role string, content json.RawMessage, contentIsString bool) (map[string]json.RawMessage, error) {
	object := map[string]json.RawMessage{
		"type": json.RawMessage(`"message"`),
		"role": json.RawMessage(strconvQuote(role)),
	}
	if contentIsString {
		object["content"] = codexTextContent(content)
	} else {
		object["content"] = content
	}
	id, err := core.NewCodexMessageID()
	if err != nil {
		return nil, codexInvocationError("input", "message identity is unavailable")
	}
	object["id"] = json.RawMessage(strconvQuote(id))
	return object, nil
}

func codexMessageObject(object map[string]json.RawMessage, role string) (json.RawMessage, error) {
	if value, ok := object["id"]; ok {
		var id string
		if json.Unmarshal(value, &id) != nil || !validCodexMessageID(id) {
			return nil, codexInvocationError("input.id", "message id is invalid")
		}
	} else {
		id, err := core.NewCodexMessageID()
		if err != nil {
			return nil, codexInvocationError("input.id", "message identity is unavailable")
		}
		object["id"] = json.RawMessage(strconvQuote(id))
	}
	if value, ok := object["content"]; ok {
		normalized, err := normalizeCodexContent(value)
		if err != nil {
			return nil, err
		}
		object["content"] = normalized
	}
	if _, ok := object["type"]; !ok {
		object["type"] = json.RawMessage(`"message"`)
	}
	if _, ok := object["role"]; !ok {
		object["role"] = json.RawMessage(strconvQuote(role))
	}
	return json.Marshal(object)
}

func normalizeCodexContent(value json.RawMessage) (json.RawMessage, error) {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, codexInvocationError("input.content", "message content must be a string or array")
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return codexTextContent(value), nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(value, &parts) != nil {
		return nil, codexInvocationError("input.content", "message content must be a string or array")
	}
	normalized := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		var text string
		if json.Unmarshal(part, &text) == nil {
			normalized = append(normalized, codexTextContent(part))
			continue
		}
		normalized = append(normalized, part)
	}
	return json.Marshal(normalized)
}

func codexTextContent(value json.RawMessage) json.RawMessage {
	return json.RawMessage(`[{"type":"input_text","text":` + string(value) + `}]`)
}

func validCodexMessageID(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

func codexDefaultJSON(fields map[string]json.RawMessage, key string, fallback []byte) json.RawMessage {
	if value, ok := fields[key]; ok {
		return value
	}
	return json.RawMessage(fallback)
}

func codexInclude(value json.RawMessage) json.RawMessage {
	const encrypted = "reasoning.encrypted_content"
	if len(value) == 0 {
		return json.RawMessage(`["reasoning.encrypted_content"]`)
	}
	var values []string
	if json.Unmarshal(value, &values) != nil {
		return json.RawMessage(`["reasoning.encrypted_content"]`)
	}
	for _, item := range values {
		if item == encrypted {
			encoded, _ := json.Marshal(values)
			return encoded
		}
	}
	values = append(values, encrypted)
	encoded, _ := json.Marshal(values)
	return encoded
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func applyCodexInvocationHeaders(headers http.Header, invocation core.CodexInvocation) error {
	if headers == nil {
		return codexInvocationError("client_profile", "codex-exec requires request headers")
	}
	generated, err := invocation.Headers()
	if err != nil {
		return codexInvocationError("client_profile", "codex-exec metadata is unavailable")
	}
	reserved := map[string]struct{}{
		"accept-encoding": {},
		"traceparent":     {},
		"tracestate":      {},
		"baggage":         {},
	}
	for key := range generated {
		reserved[strings.ToLower(key)] = struct{}{}
	}
	for key := range headers {
		if _, ok := reserved[strings.ToLower(key)]; ok {
			delete(headers, key)
		}
	}
	for key, values := range generated {
		headers[key] = append([]string(nil), values...)
	}
	return nil
}

func codexNativeEndpointAllowed(ctx context.Context, connection core.Connection, connector core.Connector, endpoint core.NativeEndpoint, params map[string]string) bool {
	if !codexProfile(connection.ClientProfile) {
		return true
	}
	if endpoint.Method != http.MethodPost || endpoint.Operation != "generate" {
		return false
	}
	binder, ok := connector.(core.EndpointBinder)
	if !ok {
		return false
	}
	binding, err := binder.BindEndpoint(ctx, connection, endpoint, cloneStringMap(params))
	return err == nil && binding.Codec.Protocol == "openai-responses" && responsesBindingIsStream(binding)
}

func responsesBindingIsStream(binding core.Binding) bool {
	return binding.Framing == "sse-data" || binding.Framing == "sse-named"
}

func (g *Gateway) bindResponses(ctx context.Context, target selected, operation core.Operation, stream bool) (core.Binding, error) {
	inventory, ok := target.connector.(core.EndpointInventory)
	if !ok {
		return core.Binding{}, codexUnsupported("connector does not expose an explicit Responses binding")
	}
	binder, ok := target.connector.(core.EndpointBinder)
	if !ok {
		return core.Binding{}, codexUnsupported("connector does not expose an explicit Responses binding")
	}
	endpoints := inventory.Endpoints()
	if inv, ok := target.connector.(core.ConnectionInventory); ok {
		var err error
		endpoints, err = inv.EndpointsFor(target.connection)
		if err != nil {
			return core.Binding{}, err
		}
	}
	params := map[string]string{}
	if target.model.ID != "" {
		params["model"] = target.model.ID
	}
	if call, ok := target.target.(core.ModelCall); ok {
		params["model"] = call.Model.ID
	}
	var fallback core.Binding
	for _, endpoint := range endpoints {
		if endpoint.Method != http.MethodPost || endpoint.Operation != operation {
			continue
		}
		candidate, err := binder.BindEndpoint(ctx, target.connection, endpoint, cloneStringMap(params))
		if err != nil || candidate.Codec.Protocol != "openai-responses" || !responsesBindingIsStream(candidate) {
			continue
		}
		if stream {
			return candidate, nil
		}
		if fallback.Codec.Protocol == "" {
			fallback = candidate
		}
	}
	if fallback.Codec.Protocol != "" {
		return fallback, nil
	}
	return core.Binding{}, codexUnsupported("connector has no compatible explicit Responses SSE binding")
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func codexUsageFromObject(value map[string]json.RawMessage) (*core.Usage, error) {
	input, err := nullableNonNegativeInt(value["input_tokens"])
	if err != nil {
		return nil, err
	}
	output, err := nullableNonNegativeInt(value["output_tokens"])
	if err != nil {
		return nil, err
	}
	total, err := nullableNonNegativeInt(value["total_tokens"])
	if err != nil {
		return nil, err
	}
	var cached, reasoning *int64
	if nested, ok := value["input_tokens_details"]; ok {
		var details map[string]json.RawMessage
		if json.Unmarshal(nested, &details) != nil || details == nil {
			return nil, errors.New("invalid input token details")
		}
		cached, err = nullableNonNegativeInt(details["cached_tokens"])
		if err != nil {
			return nil, err
		}
	}
	if nested, ok := value["output_tokens_details"]; ok {
		var details map[string]json.RawMessage
		if json.Unmarshal(nested, &details) != nil || details == nil {
			return nil, errors.New("invalid output token details")
		}
		reasoning, err = nullableNonNegativeInt(details["reasoning_tokens"])
		if err != nil {
			return nil, err
		}
	}
	if input == nil && output == nil && total == nil && cached == nil && reasoning == nil {
		return nil, nil
	}
	return &core.Usage{Input: input, Output: output, Total: total, CachedInput: cached, ReasoningOutput: reasoning, Source: "provider"}, nil
}

func nullableNonNegativeInt(value json.RawMessage) (*int64, error) {
	if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return nil, err
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 {
		return nil, errors.New("invalid token count")
	}
	return &parsed, nil
}

func codexEventObject(event framing.SSEEvent) (map[string]json.RawMessage, error) {
	if event.Data == "[DONE]" {
		return nil, errors.New("Responses stream used an unexpected DONE marker")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(event.Data), &object); err != nil || object == nil {
		return nil, errors.New("Responses stream event is invalid")
	}
	return object, nil
}

func codexEventType(object map[string]json.RawMessage, event framing.SSEEvent) string {
	var typ string
	_ = json.Unmarshal(object["type"], &typ)
	if typ == "" {
		typ = event.Event
	}
	return typ
}

func codexTerminalResponse(object map[string]json.RawMessage) (json.RawMessage, string, *core.Usage, error) {
	typ := codexEventType(object, framing.SSEEvent{})
	switch typ {
	case "response.completed", "response.incomplete":
	case "response.failed":
		return nil, typ, nil, errors.New("Responses request failed")
	default:
		return nil, typ, nil, nil
	}
	if value, ok := object["error"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, typ, nil, errors.New("Responses request returned an error")
	}
	response, ok := object["response"]
	if !ok || !json.Valid(response) {
		return nil, typ, nil, errors.New("Responses terminal event omitted response")
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(response, &value) != nil || value == nil {
		return nil, typ, nil, errors.New("Responses terminal response is invalid")
	}
	var status string
	if json.Unmarshal(value["status"], &status) != nil {
		return nil, typ, nil, errors.New("Responses terminal response status is invalid")
	}
	expected := "completed"
	if typ == "response.incomplete" {
		expected = "incomplete"
	}
	if status != expected {
		return nil, typ, nil, errors.New("Responses terminal response status is invalid")
	}
	if nested, ok := value["error"]; ok && !bytes.Equal(bytes.TrimSpace(nested), []byte("null")) {
		return nil, typ, nil, errors.New("Responses request returned an error")
	}
	if typ == "response.incomplete" {
		var details map[string]json.RawMessage
		if json.Unmarshal(value["incomplete_details"], &details) != nil || details == nil {
			return nil, typ, nil, errors.New("Responses incomplete details are invalid")
		}
		var reason string
		if json.Unmarshal(details["reason"], &reason) != nil || reason == "" {
			return nil, typ, nil, errors.New("Responses incomplete details are invalid")
		}
	}
	var usage *core.Usage
	if nested, ok := value["usage"]; ok && !bytes.Equal(bytes.TrimSpace(nested), []byte("null")) {
		var usageObject map[string]json.RawMessage
		if json.Unmarshal(nested, &usageObject) != nil || usageObject == nil {
			return nil, typ, nil, errors.New("Responses terminal usage is invalid")
		}
		var err error
		usage, err = codexUsageFromObject(usageObject)
		if err != nil {
			return nil, typ, nil, err
		}
	}
	return append(json.RawMessage(nil), response...), typ, usage, nil
}

func aggregateCodexSSE(ctx context.Context, response *http.Response) ([]byte, *core.Usage, error) {
	if response == nil || response.Body == nil {
		return nil, nil, errors.New("upstream response body is unavailable")
	}
	reader := transport.NewIdleReader(ctx, response.Body, 120000000000)
	defer reader.Close()
	decoder := framing.NewSSEDecoder(reader, codexWireMaxEventBytes)
	total := 0
	var observedUsage *core.Usage
	for {
		event, err := decoder.Next(ctx)
		if err == io.EOF {
			return nil, nil, errCodexStreamTruncated
		}
		if err != nil {
			return nil, nil, err
		}
		total += len(event.Data)
		if total > codexWireMaxBodyBytes {
			return nil, nil, framing.ErrFrameTooLarge
		}
		object, err := codexEventObject(event)
		if err != nil {
			return nil, nil, err
		}
		typ := codexEventType(object, event)
		if typ == "error" || event.Event == "error" {
			return nil, nil, errors.New("Responses stream returned an error event")
		}
		if typ == "response.usage" {
			usage, err := codexUsageFromObject(object)
			if err != nil {
				return nil, nil, err
			}
			if usage != nil {
				observedUsage = usage
			}
		}
		terminal, _, usage, err := codexTerminalResponse(object)
		if err != nil {
			return nil, nil, err
		}
		if terminal != nil {
			if usage == nil {
				usage = observedUsage
			}
			return terminal, usage, nil
		}
	}
}

// relayCodexResponse preserves streaming bytes while observing only the
// bounded event envelope needed for settlement and usage accounting.
func relayCodexResponse(ctx context.Context, w http.ResponseWriter, response *http.Response, stream bool) (transport.RelayResult, observation, error) {
	if !stream {
		body, usage, err := aggregateCodexSSE(ctx, response)
		if err != nil {
			return transport.RelayResult{}, observation{err: err}, err
		}
		copyResponseHeaders(w.Header(), response.Header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		if _, err := w.Write(body); err != nil {
			return transport.RelayResult{Committed: true, CancellationRequested: true}, observation{usage: usage, terminal: true}, err
		}
		return transport.RelayResult{Committed: true, Bytes: int64(len(body))}, observation{usage: usage, terminal: true}, nil
	}
	readPipe, writePipe := io.Pipe()
	original := response.Body
	response.Body = teeBody{Reader: io.TeeReader(original, writePipe), closer: original}
	done := make(chan observation, 1)
	go func() {
		var seen observation
		decoder := framing.NewSSEDecoder(readPipe, codexWireMaxEventBytes)
		total := 0
		for {
			event, err := decoder.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				seen.err = err
				break
			}
			total += len(event.Data)
			if total > codexWireMaxBodyBytes {
				seen.err = framing.ErrFrameTooLarge
				continue
			}
			object, err := codexEventObject(event)
			if err != nil {
				seen.err = err
				continue
			}
			typ := codexEventType(object, event)
			if typ == "error" || event.Event == "error" {
				seen.err = errors.New("Responses stream returned an error event")
				continue
			}
			if typ == "response.usage" {
				usage, err := codexUsageFromObject(object)
				if err != nil {
					seen.err = err
				} else if usage != nil {
					seen.usage = usage
				}
			}
			terminal, _, usage, err := codexTerminalResponse(object)
			if err != nil {
				seen.err = err
			} else if terminal != nil {
				seen.terminal = true
				if usage != nil {
					seen.usage = usage
				}
			}
		}
		if !seen.terminal && seen.err == nil {
			seen.err = errCodexStreamTruncated
		}
		_, _ = io.Copy(io.Discard, readPipe)
		_ = readPipe.Close()
		done <- seen
	}()
	result, relayErr := transport.RelayHTTP(ctx, w, response, transport.RelayOptions{BufferSize: 32 << 10, ReadIdle: 120 * 1000000000, WriteTimeout: 30 * 1000000000})
	_ = writePipe.Close()
	seen := <-done
	if relayErr != nil {
		return result, seen, relayErr
	}
	return result, seen, nil
}
