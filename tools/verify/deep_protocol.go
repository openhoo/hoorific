package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// deepProtocolScenarios deliberately owns only protocol-facing verification.
// Every provider call below uses an operationFixture (or the seeded fixture for
// ingress rejection checks), so no ambient credentials or paid upstreams are
// involved.  The cases are kept sequential because each fixture owns mutable
// provider state and some cases restart the real gateway process.
func (e *environment) deepProtocolScenarios() []result {
	if e == nil || e.client == nil || e.fixture == nil || e.key == "" || e.cookie == "" || e.csrf == "" || e.tenantID == "" {
		return []result{{Name: "deep-protocol/setup", Status: "failed", Detail: "isolated seeded key, fixture, administration session, and tenant are required"}}
	}
	out := []result{}
	out = append(out, e.deepPortableOpenAI())
	out = append(out, e.deepPortableAnthropic())
	out = append(out, e.deepPortableGemini())
	out = append(out, e.deepProtocolBoundaries()...)
	out = append(out, e.deepProviderFaults())
	out = append(out, e.deepNativeRecovery())
	out = append(out, e.deepContinuationRecovery()...)
	return out
}

func deepOperationError(reply operationReply, want int, code string) error {
	if reply.status != want || reply.header.Get("X-Hoorific-Error-Code") != code {
		return fmt.Errorf("expected HTTP %d/%s, got HTTP %d/%s body=%s", want, code, reply.status, reply.header.Get("X-Hoorific-Error-Code"), trim(string(reply.body)))
	}
	return nil
}

func deepJSONMap(body []byte) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("JSON object was null")
	}
	return value, nil
}

func deepMapString(value map[string]any, key string) string {
	v, _ := value[key].(string)
	return v
}

// deepEnableFixtureStreaming opts the operation fixture model into streaming
// after its ordinary unary/count assertions.  The verifier owns this model, so
// a versioned management update is preferable to weakening target selection.
func (e *environment) deepEnableFixtureStreaming(f *operationFixture) error {
	if f == nil {
		return fmt.Errorf("streaming fixture is nil")
	}
	modelID := f.id + "-model"
	current, err := e.deepCall(true, http.MethodGet, "/admin/api/v1/models/"+url.PathEscape(modelID), nil, nil)
	if err != nil {
		return err
	}
	if current.Status != http.StatusOK {
		return fmt.Errorf("fixture model read returned HTTP %d", current.Status)
	}
	var resource struct {
		Version int64          `json:"version"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(current.Body, &resource); err != nil {
		return fmt.Errorf("fixture model read was not JSON: %w", err)
	}
	if resource.Version < 1 || resource.Data == nil {
		return fmt.Errorf("fixture model read omitted version/data")
	}
	features, _ := resource.Data["features"].(map[string]any)
	if features == nil {
		features = map[string]any{}
	}
	features["streaming"] = "supported"
	resource.Data["features"] = features
	updated, err := e.deepCall(true, http.MethodPut, "/admin/api/v1/models/"+url.PathEscape(modelID), map[string]any{"data": resource.Data}, func(r *http.Request) {
		r.Header.Set("If-Match", fmt.Sprint(resource.Version))
	})
	if err != nil {
		return err
	}
	if updated.Status != http.StatusOK {
		return fmt.Errorf("fixture model streaming update returned HTTP %d/%s", updated.Status, updated.ErrorCode)
	}
	return nil
}

func (e *environment) deepPortableOpenAI() result {
	return e.operationRun("protocol-openai-portable", "openai", []string{"generate", "complete"}, func(w http.ResponseWriter, r *http.Request) error {
		switch r.URL.Path {
		case "/chat/completions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["model"] != "fixture-model" || body["stream"] != nil {
				return fmt.Errorf("OpenAI chat request was not translated: %v", body)
			}
			return operationJSON(w, map[string]any{
				"id": "deep-chat", "object": "chat.completion", "model": "fixture-model",
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
			})
		case "/completions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["model"] != "fixture-model" || body["prompt"] != "needle" {
				return fmt.Errorf("OpenAI completion request changed: %v", body)
			}
			return operationJSON(w, map[string]any{
				"id": "deep-completion", "object": "text_completion", "model": "fixture-model",
				"choices": []any{map[string]any{"text": "completed", "index": 0, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
			})
		case "/responses":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["model"] != "fixture-model" || body["store"] != false || body["input"] == nil {
				return fmt.Errorf("OpenAI Responses request changed: %v", body)
			}
			return operationJSON(w, map[string]any{
				"id": "deep-response", "object": "response", "model": "fixture-model", "status": "completed",
				"output": []any{map[string]any{"type": "message", "id": "deep-message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}},
				"usage":  map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5},
			})
		default:
			return fmt.Errorf("unexpected OpenAI portable path %s", r.URL.Path)
		}
	}, func(f *operationFixture) error {
		chat, err := f.request("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+f.alias+`","messages":[{"role":"user","content":"OK"}],"max_tokens":4}`))
		if err != nil {
			return err
		}
		if err = deepOperationError(chat, http.StatusOK, ""); err != nil || !bytes.Contains(chat.body, []byte(`"content":"OK"`)) {
			if err == nil {
				err = fmt.Errorf("chat consumer response omitted translated OK: %s", trim(string(chat.body)))
			}
			return err
		}
		completion, err := f.request("POST", "/v1/completions", "application/json", []byte(`{"model":"`+f.alias+`","prompt":"needle","max_tokens":4}`))
		if err != nil {
			return err
		}
		if err = deepOperationError(completion, http.StatusOK, ""); err != nil || !bytes.Contains(completion.body, []byte(`"text":"completed"`)) {
			if err == nil {
				err = fmt.Errorf("completion consumer response omitted translated text: %s", trim(string(completion.body)))
			}
			return err
		}
		responses, err := f.request("POST", "/v1/responses", "application/json", []byte(`{"model":"`+f.alias+`","input":"OK","store":false,"max_output_tokens":4}`))
		if err != nil {
			return err
		}
		if err = deepOperationError(responses, http.StatusOK, ""); err != nil || !bytes.Contains(responses.body, []byte(`"status":"completed"`)) || !bytes.Contains(responses.body, []byte(`"output_text"`)) {
			if err == nil {
				err = fmt.Errorf("Responses consumer response omitted completed output: %s", trim(string(responses.body)))
			}
			return err
		}
		return nil
	})
}

func (e *environment) deepPortableAnthropic() result {
	return e.operationRun("protocol-anthropic-portable", "anthropic", []string{"generate", "count_tokens"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/messages/count_tokens" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["model"] != "fixture-model" || body["max_tokens"] != nil {
				return fmt.Errorf("Anthropic count request changed: %v", body)
			}
			return operationJSON(w, map[string]any{"input_tokens": 7})
		}
		if r.URL.Path != "/messages" {
			return fmt.Errorf("unexpected Anthropic portable path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if body["model"] != "fixture-model" || body["max_tokens"] == nil {
			return fmt.Errorf("Anthropic message request changed: %v", body)
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, err := io.WriteString(w,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"deep-msg-stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n"+
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return err
		}
		return operationJSON(w, map[string]any{
			"id": "deep-msg", "type": "message", "role": "assistant", "model": "fixture-model",
			"content": []any{map[string]any{"type": "text", "text": "OK"}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 2},
		})
	}, func(f *operationFixture) error {
		body := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"OK"}],"max_tokens":4}`)
		unary, err := f.request("POST", "/v1/messages", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(unary, http.StatusOK, ""); err != nil || !bytes.Contains(unary.body, []byte(`"text":"OK"`)) {
			if err == nil {
				err = fmt.Errorf("Anthropic unary consumer response omitted OK: %s", trim(string(unary.body)))
			}
			return err
		}
		countBody := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"OK"}]}`)
		count, err := f.request("POST", "/v1/messages/count_tokens", "application/json", countBody)
		if err != nil {
			return err
		}
		if err = deepOperationError(count, http.StatusOK, ""); err != nil || !bytes.Contains(count.body, []byte(`"input_tokens":7`)) {
			if err == nil {
				err = fmt.Errorf("Anthropic count consumer response changed: %s", trim(string(count.body)))
			}
			return err
		}
		if err := e.deepEnableFixtureStreaming(f); err != nil {
			return err
		}
		stream, err := f.request("POST", "/v1/messages", "application/json", []byte(`{"model":"`+f.alias+`","messages":[{"role":"user","content":"OK"}],"max_tokens":4,"stream":true}`))
		if err != nil {
			return err
		}
		if err = deepOperationError(stream, http.StatusOK, ""); err != nil {
			return err
		}
		for _, marker := range []string{"message_start", "content_block_delta", "message_delta", "message_stop"} {
			if !bytes.Contains(stream.body, []byte(marker)) {
				return fmt.Errorf("Anthropic stream omitted terminal/lifecycle event %q: %s", marker, trim(string(stream.body)))
			}
		}
		return nil
	})
}

func (e *environment) deepPortableGemini() result {
	return e.operationRun("protocol-gemini-portable", "gemini", []string{"generate", "count_tokens"}, func(w http.ResponseWriter, r *http.Request) error {
		switch {
		case strings.HasSuffix(r.URL.Path, ":countTokens"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["contents"] == nil {
				return fmt.Errorf("Gemini count request omitted contents: %v", body)
			}
			return operationJSON(w, map[string]any{"totalTokens": 7})
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["contents"] == nil {
				return fmt.Errorf("Gemini generate request omitted contents: %v", body)
			}
			return operationJSON(w, map[string]any{"responseId": "deep-gemini", "modelVersion": "fixture-model", "candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "OK"}}}, "finishReason": "STOP"}}, "usageMetadata": map[string]any{"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5}})
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, err := io.WriteString(w, "data: {\"responseId\":\"deep-gemini-stream\",\"modelVersion\":\"fixture-model\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"OK\"}]}}]}\n\n"+
				"data: {\"responseId\":\"deep-gemini-stream\",\"modelVersion\":\"fixture-model\",\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n")
			return err
		default:
			return fmt.Errorf("unexpected Gemini portable path %s", r.URL.Path)
		}
	}, func(f *operationFixture) error {
		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"OK"}]}]}`)
		generate, err := f.request("POST", "/v1beta/models/"+f.alias+":generateContent", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(generate, http.StatusOK, ""); err != nil || !bytes.Contains(generate.body, []byte(`"text":"OK"`)) {
			if err == nil {
				err = fmt.Errorf("Gemini generate consumer response omitted OK: %s", trim(string(generate.body)))
			}
			return err
		}
		count, err := f.request("POST", "/v1beta/models/"+f.alias+":countTokens", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(count, http.StatusOK, ""); err != nil || !bytes.Contains(count.body, []byte(`"totalTokens":7`)) {
			if err == nil {
				err = fmt.Errorf("Gemini count consumer response changed: %s", trim(string(count.body)))
			}
			return err
		}
		if err := e.deepEnableFixtureStreaming(f); err != nil {
			return err
		}
		stream, err := f.request("POST", "/v1beta/models/"+f.alias+":streamGenerateContent?alt=sse", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(stream, http.StatusOK, ""); err != nil {
			return err
		}
		for _, marker := range []string{"data:", "OK", "STOP"} {
			if !bytes.Contains(stream.body, []byte(marker)) {
				return fmt.Errorf("Gemini stream omitted lifecycle marker %q: %s", marker, trim(string(stream.body)))
			}
		}
		return nil
	})
}

func (e *environment) deepProtocolBoundaries() []result {
	out := []result{}
	out = append(out, e.extReject("protocol/method-boundary", http.MethodGet, "/v1/chat/completions", nil, nil, http.StatusNotFound, "unsupported_operation"))
	out = append(out, e.extReject("protocol/trailing-json", http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"OK"}]} {}`), nil, http.StatusBadRequest, "invalid_request"))
	out = append(out, e.extReject("protocol/non-object-json", http.MethodPost, "/v1/chat/completions", []byte(`[]`), nil, http.StatusBadRequest, "invalid_request"))

	start := time.Now()
	obs, err := e.extRequest(context.Background(), false, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"OK"}]}`), func(r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
	})
	if err == nil && (obs.Status < 400 || obs.UpstreamCalls != 0 || obs.ErrorCode != "invalid_request" && obs.ErrorCode != "unsupported_feature") {
		err = fmt.Errorf("non-JSON content type was not rejected before dispatch: HTTP %d/%s calls=%d", obs.Status, obs.ErrorCode, obs.UpstreamCalls)
	}
	out = append(out, extResult("protocol/content-type-boundary", start, obs, err))
	out = append(out, e.deepMultipartBoundary())
	return out
}

func (e *environment) deepMultipartBoundary() result {
	start := time.Now()
	f, err := e.operationFixture("protocol-multipart-boundary", "openai", []string{"audio.transcribe"}, func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("malformed multipart reached upstream")
	})
	if err != nil {
		return extResult("protocol/multipart-duplicate-metadata", start, nil, err)
	}
	defer f.server.Close()
	var body bytes.Buffer
	mw := newMultipartWriter(&body)
	if err = mw.field("model", f.alias); err == nil {
		err = mw.field("model", f.alias)
	}
	if err == nil {
		err = mw.file("file", "fixture.wav", []byte("RIFF"))
	}
	if err == nil {
		err = mw.close()
	}
	if err != nil {
		return extResult("protocol/multipart-duplicate-metadata", start, nil, err)
	}
	reply, requestErr := f.request("POST", "/v1/audio/transcriptions", mw.contentType, body.Bytes())
	if requestErr != nil {
		err = requestErr
	} else if reply.status != http.StatusBadRequest || reply.header.Get("X-Hoorific-Error-Code") != "invalid_request" {
		err = fmt.Errorf("duplicate multipart metadata expected HTTP 400 invalid_request, got HTTP %d/%s body=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"), trim(string(reply.body)))
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if err == nil && calls != 0 {
		err = fmt.Errorf("duplicate multipart metadata dispatched %d upstream calls", calls)
	}
	return extResult("protocol/multipart-duplicate-metadata", start, map[string]any{"response": map[string]any{"status": reply.status, "error_code": reply.header.Get("X-Hoorific-Error-Code"), "body": trim(string(reply.body))}, "upstream_calls": calls}, err)
}

// multipartWriter keeps the boundary helper local to this file while avoiding
// a broad multipart fixture that could accidentally exercise a valid provider.
type multipartWriter struct {
	body        *bytes.Buffer
	boundary    string
	contentType string
	parts       []string
}

func newMultipartWriter(body *bytes.Buffer) *multipartWriter {
	boundary := "deep-protocol-boundary"
	return &multipartWriter{body: body, boundary: boundary, contentType: "multipart/form-data; boundary=" + boundary}
}
func (m *multipartWriter) field(name, value string) error {
	m.parts = append(m.parts, "--"+m.boundary+"\r\nContent-Disposition: form-data; name=\""+name+"\"\r\n\r\n"+value+"\r\n")
	return nil
}
func (m *multipartWriter) file(name, filename string, data []byte) error {
	m.parts = append(m.parts, "--"+m.boundary+"\r\nContent-Disposition: form-data; name=\""+name+"\"; filename=\""+filename+"\"\r\nContent-Type: application/octet-stream\r\n\r\n"+string(data)+"\r\n")
	return nil
}
func (m *multipartWriter) close() error {
	for _, p := range m.parts {
		if _, err := io.WriteString(m.body, p); err != nil {
			return err
		}
	}
	_, err := io.WriteString(m.body, "--"+m.boundary+"--\r\n")
	return err
}

func (e *environment) deepProviderFaults() result {
	var mode atomic.Int32
	return e.operationRun("protocol-provider-faults", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path != "/chat/completions" {
			return fmt.Errorf("unexpected fault fixture path %s", r.URL.Path)
		}
		switch mode.Load() {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, "{\"id\":")
			return err
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_, err := io.WriteString(w, "provider failure")
			return err
		case 3:
			if h, ok := w.(http.Hijacker); ok {
				conn, _, err := h.Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return err
			}
			return fmt.Errorf("fixture does not support disconnect")
		default:
			return operationJSON(w, map[string]any{"id": "fault-ok", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}})
		}
	}, func(f *operationFixture) error {
		body := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"OK"}],"max_tokens":4}`)
		mode.Store(1)
		badJSON, err := f.request("POST", "/v1/messages", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(badJSON, http.StatusBadGateway, "upstream_outcome_unknown"); err != nil {
			return err
		}
		mode.Store(2)
		serverError, err := f.request("POST", "/v1/messages", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(serverError, http.StatusInternalServerError, "upstream_error"); err != nil {
			return err
		}
		mode.Store(3)
		disconnected, err := f.request("POST", "/v1/messages", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(disconnected, http.StatusBadGateway, "upstream_outcome_unknown"); err != nil {
			return err
		}
		mode.Store(0)
		recovered, err := f.request("POST", "/v1/messages", "application/json", body)
		if err != nil {
			return err
		}
		if err = deepOperationError(recovered, http.StatusOK, ""); err != nil || !bytes.Contains(recovered.body, []byte(`"text":"OK"`)) {
			if err == nil {
				err = fmt.Errorf("provider-fault recovery response omitted OK")
			}
			return err
		}
		calls := fCallCount(f, "POST /chat/completions")
		if calls != 4 {
			return fmt.Errorf("fault requests retried or were dropped: expected 4 upstream calls, got %d", calls)
		}
		return nil
	})
}

func (e *environment) deepNativeRecovery() (out result) {
	start := time.Now()
	policyID := fmt.Sprintf("deep-jobs-%d", time.Now().UnixNano())
	if err := e.extCreate("policy_limits", policyID, map[string]any{"scope": "tenant", "scope_id": e.tenantID, "outstanding_jobs": 1}); err != nil {
		return extResult("resource/native-job-restart-settlement", start, nil, err)
	}
	defer func() {
		cleanup, cleanupErr := e.extRequest(context.Background(), true, http.MethodDelete, "/admin/api/v1/policy_limits/"+url.PathEscape(policyID), nil, func(r *http.Request) {
			r.Header.Set("If-Match", "1")
		})
		if cleanupErr == nil && (cleanup.Status < http.StatusOK || cleanup.Status >= http.StatusMultipleChoices) {
			cleanupErr = fmt.Errorf("policy cleanup returned HTTP %d", cleanup.Status)
		}
		if cleanupErr == nil {
			return
		}
		if out.Status == "failed" {
			out.Detail += "; policy cleanup failed: " + cleanupErr.Error()
			return
		}
		out = extResult("resource/native-job-restart-settlement", start, map[string]any{"policy_cleanup": cleanup}, cleanupErr)
	}()
	var creates atomic.Int32
	var firstID atomic.Value
	var firstReleased atomic.Bool
	f, err := e.operationFixture("protocol-native-job-recovery", "openai", []string{"video"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == http.MethodPost && r.URL.Path == "/videos" {
			id := fmt.Sprintf("deep-video-%d", creates.Add(1))
			if creates.Load() == 1 {
				firstID.Store(id)
			}
			return operationJSON(w, map[string]any{"id": id, "status": "queued"})
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/videos/") {
			id := strings.TrimPrefix(r.URL.Path, "/videos/")
			if id == firstIDString(&firstID) && !firstReleased.Load() {
				return operationJSON(w, map[string]any{"id": id, "status": "queued"})
			}
			return operationJSON(w, map[string]any{"id": id, "status": "completed", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}})
		}
		return fmt.Errorf("unexpected native recovery path %s %s", r.Method, r.URL.Path)
	})
	if err != nil {
		return extResult("resource/native-job-restart-settlement", start, nil, err)
	}
	defer f.server.Close()
	body := []byte(`{"prompt":"durable fixture video"}`)
	first, requestErr := f.request("POST", "/connect/"+f.id+"/openai/v1/videos", "application/json", body)
	if requestErr != nil {
		return extResult("resource/native-job-restart-settlement", start, nil, requestErr)
	}
	if err = deepOperationError(first, http.StatusOK, ""); err != nil {
		return extResult("resource/native-job-restart-settlement", start, nil, err)
	}
	firstRequestID := first.header.Get("X-Request-ID")
	if firstRequestID == "" {
		return extResult("resource/native-job-restart-settlement", start, nil, fmt.Errorf("native acknowledgement omitted request identity"))
	}
	state, err := e.deepWaitAdmission(firstRequestID, "job_pending", 5*time.Second)
	if err != nil {
		return extResult("resource/native-job-restart-settlement", start, map[string]any{"request_id": firstRequestID, "state": state}, err)
	}
	pendingBefore := fCallCount(f, "POST /videos")
	pending, pendingErr := f.request("POST", "/connect/"+f.id+"/openai/v1/videos", "application/json", body)
	pendingAfter := fCallCount(f, "POST /videos")
	if pendingErr != nil {
		return extResult("resource/native-job-restart-settlement", start, map[string]any{"request_id": firstRequestID, "state": state}, pendingErr)
	}
	if pending.status != http.StatusTooManyRequests || pending.header.Get("X-Hoorific-Error-Code") != "quota_exceeded" || pendingBefore != pendingAfter {
		return extResult("resource/native-job-restart-settlement", start, map[string]any{"pending_status": pending.status, "pending_code": pending.header.Get("X-Hoorific-Error-Code"), "create_calls_before": pendingBefore, "create_calls_after": pendingAfter}, fmt.Errorf("outstanding_jobs=1 did not reject second pending admission without dispatch"))
	}
	if restart := e.restartRecovery(); restart.Status != "passed" {
		return extResult("resource/native-job-restart-settlement", start, map[string]any{"request_id": firstRequestID, "state": state}, fmt.Errorf("gateway restart failed: %s", restart.Detail))
	}
	firstReleased.Store(true)
	state, err = e.deepWaitAdmission(firstRequestID, "settled", 8*time.Second)
	if err != nil {
		return extResult("resource/native-job-restart-settlement", start, map[string]any{"request_id": firstRequestID, "state": state}, err)
	}
	second, requestErr := f.request("POST", "/connect/"+f.id+"/openai/v1/videos", "application/json", body)
	if requestErr != nil {
		return extResult("resource/native-job-restart-settlement", start, nil, requestErr)
	}
	if err = deepOperationError(second, http.StatusOK, ""); err != nil {
		return extResult("resource/native-job-restart-settlement", start, nil, err)
	}
	secondRequestID := second.header.Get("X-Request-ID")
	if secondRequestID == "" {
		return extResult("resource/native-job-restart-settlement", start, nil, fmt.Errorf("second native acknowledgement omitted request identity"))
	}
	secondState, waitErr := e.deepWaitAdmission(secondRequestID, "settled", 8*time.Second)
	if waitErr != nil {
		err = waitErr
	}
	read, readErr := e.extRequest(context.Background(), true, http.MethodGet, "/admin/api/v1/upstream_operations?limit=200", nil, nil)
	if readErr != nil && err == nil {
		err = readErr
	}
	if err == nil && read.Status != http.StatusOK {
		err = fmt.Errorf("upstream_operations read expected HTTP 200, got HTTP %d/%s body=%s", read.Status, read.ErrorCode, trim(read.Body))
	}
	jobEvidence := map[string]any{"first_request_id": firstRequestID, "first_state": state, "second_request_id": secondRequestID, "second_state": secondState, "upstream_operations_status": read.Status, "upstream_operations_body": trim(read.Body)}
	if err == nil {
		var page struct {
			Items []struct {
				ID      string          `json:"id"`
				Version int64           `json:"version"`
				Data    json.RawMessage `json:"data"`
			} `json:"items"`
		}
		if json.Unmarshal([]byte(read.Body), &page) != nil {
			err = fmt.Errorf("upstream_operations response was not a resource page")
		} else {
			found := false
			for _, item := range page.Items {
				var data struct {
					ResourceID string `json:"resource_id"`
					Status     string `json:"status"`
				}
				if json.Unmarshal(item.Data, &data) == nil && strings.HasPrefix(data.ResourceID, "deep-video-") {
					found = item.ID != "" && item.Version >= 1 && data.Status != ""
					if found {
						jobEvidence["operation_id"] = item.ID
						jobEvidence["operation_version"] = item.Version
						jobEvidence["operation_status"] = data.Status
						break
					}
				}
			}
			if !found {
				err = fmt.Errorf("upstream_operations page omitted durable deep-video operation")
			}
		}
	}
	return extResult("resource/native-job-restart-settlement", start, jobEvidence, err)
}

func firstIDString(value *atomic.Value) string {
	if value == nil {
		return ""
	}
	stored := value.Load()
	if stored == nil {
		return ""
	}
	return stored.(string)
}

func (e *environment) deepWaitAdmission(requestID, want string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		obs, err := e.extRequest(context.Background(), true, http.MethodGet, "/admin/api/v1/admissions?limit=200", nil, nil)
		if err == nil && obs.Status == http.StatusOK {
			var page struct {
				Items []struct {
					Data struct {
						RequestID string `json:"request_id"`
						State     string `json:"state"`
					} `json:"data"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(obs.Body), &page) == nil {
				for _, item := range page.Items {
					if item.Data.RequestID == requestID {
						last = item.Data.State
						if last == want {
							return last, nil
						}
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last, fmt.Errorf("admission %s did not reach %s (last state %s)", requestID, want, last)
}

func (e *environment) deepContinuationRecovery() []result {
	return []result{e.deepContinuationRestart(), e.deepContinuationOrigin()}
}

func (e *environment) deepContinuationRestart() result {
	start := time.Now()
	f, err := e.operationFixture("protocol-continuation-restart", "gemini", []string{"upload"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/upload/v1beta/files" {
			if r.Header.Get("X-Goog-Upload-Protocol") != "resumable" {
				return fmt.Errorf("upload protocol missing")
			}
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/deep-continue")
			w.WriteHeader(http.StatusOK)
			return nil
		}
		if r.Method == http.MethodPost && r.URL.Path == "/upload/v1beta/files/deep-continue" {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				return readErr
			}
			if !bytes.Equal(body, []byte("restart-continuation")) {
				return fmt.Errorf("continuation bytes changed: %q", body)
			}
			return operationJSON(w, map[string]any{"file": map[string]any{"name": "files/deep", "state": "ACTIVE"}})
		}
		return fmt.Errorf("unexpected continuation path %s %s", r.Method, r.URL.Path)
	})
	if err != nil {
		return extResult("resource/continuation-restart", start, nil, err)
	}
	defer f.server.Close()
	startReply, requestErr := f.requestHeaders("POST", "/connect/"+f.id+"/gemini/upload/v1beta/files", "application/json", []byte(`{"file":{"display_name":"deep"}}`), map[string]string{"X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start"})
	if requestErr != nil {
		return extResult("resource/continuation-restart", start, nil, requestErr)
	}
	continuation := startReply.header.Get("X-Goog-Upload-URL")
	u, parseErr := url.Parse(continuation)
	if startReply.status != http.StatusOK || parseErr != nil || !strings.Contains(u.Path, "/connect/"+f.id+"/continuations/") {
		return extResult("resource/continuation-restart", start, map[string]any{"status": startReply.status, "url_path": u.Path}, fmt.Errorf("upload acknowledgement was not gateway-owned: HTTP %d URL path %q", startReply.status, u.Path))
	}
	if restart := e.restartRecovery(); restart.Status != "passed" {
		return extResult("resource/continuation-restart", start, map[string]any{"url_path": u.Path}, fmt.Errorf("gateway restart failed: %s", restart.Detail))
	}
	final, requestErr := f.requestHeaders("POST", u.Path, "application/octet-stream", []byte("restart-continuation"), map[string]string{"X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"})
	if requestErr != nil {
		return extResult("resource/continuation-restart", start, nil, requestErr)
	}
	if final.status != http.StatusOK || !bytes.Contains(final.body, []byte("files/deep")) {
		return extResult("resource/continuation-restart", start, map[string]any{"status": final.status, "body": trim(string(final.body))}, fmt.Errorf("restart continuation finalization failed: HTTP %d/%s", final.status, final.header.Get("X-Hoorific-Error-Code")))
	}
	before := fCallCount(f, "POST /upload/v1beta/files/deep-continue")
	method, requestErr := f.requestHeaders(http.MethodGet, u.Path, "application/json", nil, nil)
	if requestErr != nil {
		return extResult("resource/continuation-restart", start, nil, requestErr)
	}
	after := fCallCount(f, "POST /upload/v1beta/files/deep-continue")
	if method.status != http.StatusMethodNotAllowed || method.header.Get("X-Hoorific-Error-Code") != "unsupported_operation" || before != after {
		return extResult("resource/continuation-restart", start, map[string]any{"method_status": method.status, "method_code": method.header.Get("X-Hoorific-Error-Code"), "continuation_calls": after}, fmt.Errorf("continuation method boundary failed: HTTP %d/%s calls %d->%d", method.status, method.header.Get("X-Hoorific-Error-Code"), before, after))
	}
	return extResult("resource/continuation-restart", start, map[string]any{"url_path": u.Path, "final_status": final.status, "continuation_calls": after}, nil)
}

func fCallCount(f *operationFixture, key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (e *environment) deepContinuationOrigin() result {
	start := time.Now()
	f, setupErr := e.operationFixture("protocol-continuation-origin", "gemini", []string{"upload"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path != "/upload/v1beta/files" {
			return fmt.Errorf("unexpected origin fixture path %s", r.URL.Path)
		}
		w.Header().Set("X-Goog-Upload-URL", "https://evil.example.invalid/upload")
		w.WriteHeader(http.StatusOK)
		return nil
	})
	if setupErr != nil {
		return extResult("resource/continuation-origin-boundary", start, nil, setupErr)
	}
	defer f.server.Close()
	reply, requestErr := f.requestHeaders("POST", "/connect/"+f.id+"/gemini/upload/v1beta/files", "application/json", []byte(`{"file":{"display_name":"evil"}}`), map[string]string{"X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start"})
	if requestErr != nil {
		return extResult("resource/continuation-origin-boundary", start, nil, requestErr)
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	var originErr error
	if reply.status != http.StatusBadGateway || reply.header.Get("X-Hoorific-Error-Code") != "unsupported_operation" || calls != 1 || strings.Contains(string(reply.body), "evil.example") {
		originErr = fmt.Errorf("unapproved continuation origin was not rejected safely: HTTP %d/%s calls=%d body=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"), calls, trim(string(reply.body)))
	}
	return extResult("resource/continuation-origin-boundary", start, map[string]any{"status": reply.status, "error_code": reply.header.Get("X-Hoorific-Error-Code"), "upstream_calls": calls}, originErr)
}
