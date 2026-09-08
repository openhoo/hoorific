package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// costSafetyScenarios owns the user-visible cost contracts that are easiest to
// regress while changing codecs, admission, and durable replay. Every case
// below creates its own operation fixture and mutates only resources in this
// qualification's temporary store.
func (e *environment) costSafetyScenarios() []result {
	if e == nil || e.client == nil || e.fixture == nil || e.key == "" || e.cookie == "" || e.csrf == "" || e.tenantID == "" {
		return []result{{Name: "cost-safety/setup", Status: "failed", Detail: "isolated seeded key, fixture, administration session, and tenant are required"}}
	}
	return []result{
		e.costCacheSettlement(),
		e.costCacheControls(),
		e.costTranslatedAnthropicOpenAI(),
		e.costIdempotency(),
		e.costStreamingReplay(),
		e.costRetryAfter(),
		e.costCohereCancel(),
		e.costReplicateControls(),
		e.costBudgetAndReconcile(),
	}
}

func costPrice() map[string]any {
	return map[string]any{
		"version":            "cost-safety-v1",
		"input_per_million":  int64(1_000_000_000),
		"output_per_million": int64(2_000_000_000),
		"maximum_unit_cost":  int64(1_000_000_000_000),
		"unit_operation":     "generate",
	}
}

func costPriceWithCacheRead() map[string]any {
	price := costPrice()
	price["cached_input_per_million"] = int64(500_000_000)
	return price
}

func costPriceForProvider(provider string) map[string]any {
	price := costPriceWithCacheRead()
	if provider == "anthropic" {
		price["cache_write_input_per_million"] = int64(3_000_000_000)
		price["cache_write_5m_per_million"] = int64(4_000_000_000)
		price["cache_write_1h_per_million"] = int64(5_000_000_000)
	}
	return price
}

func (e *environment) costConfigureModel(f *operationFixture, price map[string]any, streaming bool) error {
	if f == nil {
		return fmt.Errorf("cost fixture is nil")
	}
	obs, err := e.extRequest(context.Background(), true, http.MethodGet, "/admin/api/v1/models/"+f.id+"-model", nil, nil)
	if err != nil {
		return err
	}
	if obs.Status != http.StatusOK {
		return fmt.Errorf("cost fixture model read returned HTTP %d", obs.Status)
	}
	var current struct {
		Version int64          `json:"version"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(obs.Body), &current); err != nil {
		return fmt.Errorf("cost fixture model read was not JSON: %w", err)
	}
	if current.Version < 1 || current.Data == nil {
		return fmt.Errorf("cost fixture model read omitted version/data")
	}
	current.Data["price"] = price
	if streaming {
		features, _ := current.Data["features"].(map[string]any)
		if features == nil {
			features = map[string]any{}
		}
		features["streaming"] = "supported"
		current.Data["features"] = features
	}
	body, _ := json.Marshal(map[string]any{"data": current.Data})
	updated, err := e.extRequest(context.Background(), true, http.MethodPut, "/admin/api/v1/models/"+f.id+"-model", body, func(r *http.Request) {
		r.Header.Set("If-Match", fmt.Sprint(current.Version))
	})
	if err != nil {
		return err
	}
	if updated.Status != http.StatusOK {
		return fmt.Errorf("cost fixture model price update returned HTTP %d/%s", updated.Status, updated.ErrorCode)
	}
	return nil
}

func (f *operationFixture) count() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.calls {
		total += count
	}
	return total
}

func (e *environment) costRequestWithToken(ctx context.Context, f *operationFixture, token, method, path, contentType string, body []byte, headers map[string]string) (operationReply, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/connect/" + f.id + "/native/" + path
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+e.inference+path, bytes.NewReader(body))
	if err != nil {
		return operationReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := *e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return operationReply{header: make(http.Header)}, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if len(raw) > 4<<20 {
		return operationReply{}, fmt.Errorf("cost response exceeded fixture bound")
	}
	return operationReply{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}, readErr
}

func costFieldRaw(data map[string]json.RawMessage, names ...string) json.RawMessage {
	normalize := func(value string) string {
		value = strings.ToLower(value)
		value = strings.ReplaceAll(value, "_", "")
		return value
	}
	for _, name := range names {
		want := normalize(name)
		for key, raw := range data {
			if normalize(key) == want {
				return raw
			}
		}
	}
	return nil
}

func costFieldInt(data map[string]json.RawMessage, names ...string) (int64, bool) {
	raw := costFieldRaw(data, names...)
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value int64
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		value, err := number.Int64()
		return value, err == nil
	}
	return 0, false
}
func costFieldString(data map[string]json.RawMessage, names ...string) string {
	raw := costFieldRaw(data, names...)
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func (e *environment) costRevokeKey(id string) (int, error) {
	observed, err := e.extRequest(context.Background(), true, http.MethodGet, "/admin/api/v1/api_keys/"+id, nil, nil)
	if err != nil {
		return 0, err
	}
	if observed.Status != http.StatusOK {
		return observed.Status, fmt.Errorf("API key read returned HTTP %d", observed.Status)
	}
	var current struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal([]byte(observed.Body), &current); err != nil || current.Version < 1 {
		return observed.Status, fmt.Errorf("API key read omitted version")
	}
	revoked, err := e.extRequest(context.Background(), true, http.MethodPost, "/admin/api/v1/api_keys/"+id+"/revoke", []byte(`{"data":{}}`), func(r *http.Request) {
		r.Header.Set("If-Match", fmt.Sprint(current.Version))
	})
	if err != nil {
		return 0, err
	}
	if revoked.Status != http.StatusOK {
		return revoked.Status, fmt.Errorf("API key revoke returned HTTP %d", revoked.Status)
	}
	return revoked.Status, nil
}

type costAdmissionRecord struct {
	ID      string
	Version int64
	Data    map[string]json.RawMessage
}

func (e *environment) costWaitAdmission(requestID, want string, timeout time.Duration) (costAdmissionRecord, error) {
	deadline := time.Now().Add(timeout)
	var last costAdmissionRecord
	for time.Now().Before(deadline) {
		obs, err := e.extRequest(context.Background(), true, http.MethodGet, "/admin/api/v1/admissions?limit=200", nil, nil)
		if err == nil && obs.Status == http.StatusOK {
			var page struct {
				Items []struct {
					ID      string                     `json:"id"`
					Version int64                      `json:"version"`
					Data    map[string]json.RawMessage `json:"data"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(obs.Body), &page) == nil {
				for _, item := range page.Items {
					if costFieldString(item.Data, "request_id", "RequestID") != requestID {
						continue
					}
					last = costAdmissionRecord{ID: item.ID, Version: item.Version, Data: item.Data}
					if costFieldString(item.Data, "state", "State") == want {
						return last, nil
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last, fmt.Errorf("admission %s did not reach %s (last state %s)", requestID, want, costFieldString(last.Data, "state", "State"))
}

func costUsage(record costAdmissionRecord) map[string]json.RawMessage {
	raw := costFieldRaw(record.Data, "usage", "Usage")
	if len(raw) == 0 {
		return nil
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	return usage
}

func costAssertUsage(record costAdmissionRecord, expected map[string]int64) error {
	usage := costUsage(record)
	if usage == nil {
		return fmt.Errorf("admission omitted settled usage")
	}
	for field, want := range expected {
		got, ok := costFieldInt(usage, field)
		if !ok || got != want {
			return fmt.Errorf("admission usage %s=%d want %d", field, got, want)
		}
	}
	return nil
}

func costAssertResponse(reply operationReply, status int, contains string) error {
	if reply.status != status {
		return fmt.Errorf("gateway returned HTTP %d want %d code=%s body=%s", reply.status, status, reply.header.Get("X-Hoorific-Error-Code"), trim(string(reply.body)))
	}
	if contains != "" && !bytes.Contains(reply.body, []byte(contains)) {
		return fmt.Errorf("gateway response omitted %q: %s", contains, trim(string(reply.body)))
	}
	return nil
}

func (e *environment) costCacheSettlement() result {
	start := time.Now()
	type cacheCase struct {
		name     string
		provider string
		path     string
		body     string
		handler  func(http.ResponseWriter, *http.Request) error
		expected map[string]int64
		cost     int64
	}
	cases := []cacheCase{
		{
			name:     "anthropic",
			provider: "anthropic",
			path:     "/v1/messages",
			body:     `{"model":"ALIAS","messages":[{"role":"user","content":"cache"}],"max_tokens":8}`,
			handler: func(w http.ResponseWriter, r *http.Request) error {
				return operationJSON(w, map[string]any{"id": "cost-anthropic", "type": "message", "role": "assistant", "model": "fixture-model", "content": []any{map[string]any{"type": "text", "text": "OK"}}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 13, "output_tokens": 7, "cache_read_input_tokens": 5, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": 2, "ephemeral_1h_input_tokens": 2}, "output_tokens_details": map[string]any{"thinking_tokens": 3}}})
			},
			expected: map[string]int64{"Input": 22, "Output": 7, "Total": 29, "CachedInput": 5, "CacheWriteInput": 4, "CacheWrite5mInput": 2, "CacheWrite1hInput": 2, "ReasoningOutput": 3},
			cost:     47500,
		},
		{
			name:     "openai",
			provider: "openai",
			path:     "/v1/chat/completions",
			body:     `{"model":"ALIAS","messages":[{"role":"user","content":"cache"}],"max_tokens":8}`,
			handler: func(w http.ResponseWriter, r *http.Request) error {
				return operationJSON(w, map[string]any{"id": "cost-openai", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 22, "completion_tokens": 7, "total_tokens": 29, "prompt_tokens_details": map[string]any{"cached_tokens": 5}, "completion_tokens_details": map[string]any{"reasoning_tokens": 3}}})
			},
			expected: map[string]int64{"Input": 22, "Output": 7, "Total": 29, "CachedInput": 5, "ReasoningOutput": 3},
			cost:     33500,
		},
		{
			name:     "gemini",
			provider: "gemini",
			path:     "/v1beta/models/ALIAS:generateContent",
			body:     `{"contents":[{"role":"user","parts":[{"text":"cache"}]}]}`,
			handler: func(w http.ResponseWriter, r *http.Request) error {
				return operationJSON(w, map[string]any{"responseId": "cost-gemini", "modelVersion": "fixture-model", "candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "OK"}}}, "finishReason": "STOP"}}, "usageMetadata": map[string]any{"promptTokenCount": 22, "candidatesTokenCount": 7, "totalTokenCount": 29, "cachedContentTokenCount": 5}})
			},
			expected: map[string]int64{"Input": 22, "Output": 7, "Total": 29, "CachedInput": 5},
			cost:     33500,
		},
	}
	evidence := make([]map[string]any, 0, len(cases))
	var err error
	for _, tc := range cases {
		f, setupErr := e.operationFixture("cost-cache-"+tc.name, tc.provider, []string{"generate"}, tc.handler)
		if setupErr != nil {
			err = setupErr
			break
		}
		body := strings.ReplaceAll(tc.body, "ALIAS", f.alias)
		path := strings.ReplaceAll(tc.path, "ALIAS", f.alias)
		if setupErr = e.costConfigureModel(f, costPriceForProvider(tc.provider), false); setupErr != nil {
			f.server.Close()
			err = setupErr
			break
		}
		reply, requestErr := f.request("POST", path, "application/json", []byte(body))
		if requestErr == nil {
			requestErr = costAssertResponse(reply, http.StatusOK, "OK")
		}
		if requestErr == nil && reply.header.Get("X-Request-ID") == "" {
			requestErr = fmt.Errorf("%s response omitted X-Request-ID required for settlement lookup", tc.name)
		}
		var admission costAdmissionRecord
		if requestErr == nil {
			admission, requestErr = e.costWaitAdmission(reply.header.Get("X-Request-ID"), "settled", 5*time.Second)
		}
		if requestErr == nil {
			requestErr = costAssertUsage(admission, tc.expected)
		}
		if requestErr == nil {
			actual, ok := costFieldInt(admission.Data, "actual_cost", "ActualCost")
			if !ok || actual != tc.cost {
				requestErr = fmt.Errorf("admission actual_cost=%d want %d", actual, tc.cost)
			}
		}
		evidence = append(evidence, map[string]any{"provider": tc.provider, "status": reply.status, "request_id": reply.header.Get("X-Request-ID"), "upstream_calls": f.count(), "usage": tc.expected, "actual_cost": tc.cost})
		f.server.Close()
		if requestErr != nil {
			err = fmt.Errorf("%s cache settlement: %w", tc.name, requestErr)
			break
		}
	}
	return extResult("cost/cache-settlement", start, evidence, err)
}

func (e *environment) costCacheControls() result {
	start := time.Now()
	evidence := []map[string]any{}
	control := func(name, provider, path, body string, check func(map[string]any) error) (map[string]any, error) {
		f, err := e.operationFixture(name, provider, []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				return err
			}
			if err := check(payload); err != nil {
				return err
			}
			switch provider {
			case "anthropic":
				return operationJSON(w, map[string]any{"id": name, "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "OK"}}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
			case "gemini":
				return operationJSON(w, map[string]any{"responseId": name, "candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "OK"}}}, "finishReason": "STOP"}}, "usageMetadata": map[string]any{"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2}})
			default:
				return operationJSON(w, map[string]any{"id": name, "object": "chat.completion", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
			}
		})
		if err == nil {
			err = e.costConfigureModel(f, costPrice(), false)
		}
		var reply operationReply
		if err == nil {
			path = strings.Replace(path, "REPLACE", f.id, 1)
			reply, err = f.request("POST", path, "application/json", []byte(body))
		}
		if err == nil {
			err = costAssertResponse(reply, http.StatusOK, "OK")
		}
		calls := 0
		if f != nil {
			calls = f.count()
			f.server.Close()
		}
		return map[string]any{"provider": provider, "status": reply.status, "upstream_calls": calls}, err
	}
	anthropicEvidence, err := control("cost-cache-control-anthropic", "anthropic", "/connect/REPLACE/native/messages", `{"model":"fixture-model","messages":[{"role":"user","content":[{"type":"text","text":"cache","cache_control":{"type":"ephemeral"}}]}],"max_tokens":8}`, func(payload map[string]any) error {
		messages, _ := payload["messages"].([]any)
		if len(messages) != 1 {
			return fmt.Errorf("Anthropic cache control was not forwarded: %v", payload)
		}
		message, _ := messages[0].(map[string]any)
		content, _ := message["content"].([]any)
		if len(content) != 1 {
			return fmt.Errorf("Anthropic cache content was not forwarded: %v", payload)
		}
		block, _ := content[0].(map[string]any)
		if _, ok := block["cache_control"]; !ok {
			return fmt.Errorf("Anthropic cache_control was dropped: %v", payload)
		}
		return nil
	})
	evidence = append(evidence, anthropicEvidence)
	openaiEvidence, openaiErr := control("cost-cache-control-openai", "openai", "/connect/REPLACE/native/chat/completions", `{"model":"fixture-model","messages":[{"role":"user","content":"cache"}],"max_tokens":8,"prompt_cache_key":"cost-session"}`, func(payload map[string]any) error {
		if payload["prompt_cache_key"] != "cost-session" {
			return fmt.Errorf("OpenAI prompt_cache_key was not forwarded: %v", payload)
		}
		return nil
	})
	evidence = append(evidence, openaiEvidence)
	if err == nil {
		err = openaiErr
	}
	geminiEvidence, geminiErr := control("cost-cache-control-gemini", "gemini", "/connect/REPLACE/native/v1beta/models/fixture-model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"cache"}]}],"cachedContent":"cachedContents/cost"}`, func(payload map[string]any) error {
		if payload["cachedContent"] != "cachedContents/cost" {
			return fmt.Errorf("Gemini cachedContent was not forwarded: %v", payload)
		}
		return nil
	})
	evidence = append(evidence, geminiEvidence)
	if err == nil {
		err = geminiErr
	}
	// The helper owns the generated connection ID; REPLACE is substituted
	// immediately before dispatch above.
	cross, crossErr := e.operationFixture("cost-cache-control-cross-protocol", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("incompatible cache directive reached OpenAI")
	})
	if crossErr == nil {
		crossErr = e.costConfigureModel(cross, costPrice(), false)
	}
	if crossErr == nil {
		reply, requestErr := cross.request("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+cross.alias+`","messages":[{"role":"user","content":"cache"}],"max_tokens":8,"cache_control":{"type":"ephemeral"}}`))
		crossErr = requestErr
		if crossErr == nil && (reply.status != http.StatusBadRequest || reply.header.Get("X-Hoorific-Error-Code") != "unsupported_feature" || reply.header.Get("X-Request-ID") == "" || cross.count() != 0) {
			crossErr = fmt.Errorf("cross-protocol cache directive was not rejected before dispatch: HTTP %d/%s request_id=%q calls=%d", reply.status, reply.header.Get("X-Hoorific-Error-Code"), reply.header.Get("X-Request-ID"), cross.count())
		}
		evidence = append(evidence, map[string]any{"provider": "openai", "cross_protocol_status": reply.status, "cross_protocol_error": reply.header.Get("X-Hoorific-Error-Code"), "cross_protocol_upstream_calls": cross.count()})
	}
	if cross != nil {
		cross.server.Close()
	}
	if err == nil {
		err = crossErr
	}
	return extResult("cost/cache-controls", start, evidence, err)
}

func (e *environment) costTranslatedAnthropicOpenAI() result {
	start := time.Now()
	f, err := e.operationFixture("cost-translated-anthropic-openai", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
			return decodeErr
		}
		if body["model"] != "fixture-model" || body["messages"] == nil {
			return fmt.Errorf("translated Anthropic request was not represented as OpenAI chat: %v", body)
		}
		return operationJSON(w, map[string]any{"id": "cost-translated", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 22, "completion_tokens": 7, "total_tokens": 29, "prompt_tokens_details": map[string]any{"cached_tokens": 5}, "completion_tokens_details": map[string]any{"reasoning_tokens": 3}}})
	})
	if err == nil {
		err = e.costConfigureModel(f, costPriceWithCacheRead(), false)
	}
	var reply operationReply
	if err == nil {
		reply, err = f.request("POST", "/v1/messages", "application/json", []byte(`{"model":"`+f.alias+`","messages":[{"role":"user","content":"translated"}],"max_tokens":8}`))
	}
	if err == nil {
		err = costAssertResponse(reply, http.StatusOK, "OK")
	}
	if err == nil {
		admission, waitErr := e.costWaitAdmission(reply.header.Get("X-Request-ID"), "settled", 5*time.Second)
		err = waitErr
		if err == nil {
			err = costAssertUsage(admission, map[string]int64{"Input": 22, "Output": 7, "Total": 29, "CachedInput": 5, "ReasoningOutput": 3})
		}
	}
	calls := 0
	if f != nil {
		calls = f.count()
		f.server.Close()
	}
	return extResult("cost/translated-anthropic-openai", start, map[string]any{"status": reply.status, "upstream_calls": calls, "response_contains_ok": bytes.Contains(reply.body, []byte("OK"))}, err)
}

func (e *environment) costIdempotency() result {
	start := time.Now()
	f, err := e.operationFixture("cost-idempotency", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		return operationJSON(w, map[string]any{"id": "cost-idempotent", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	})
	if err != nil {
		return extResult("cost/idempotency", start, nil, err)
	}
	defer f.server.Close()
	if err = e.costConfigureModel(f, costPrice(), false); err != nil {
		return extResult("cost/idempotency", start, nil, err)
	}
	body := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"same"}],"max_tokens":8}`)
	first, firstErr := f.requestHeaders("POST", "/v1/chat/completions", "application/json", body, map[string]string{"Idempotency-Key": "cost-exact-replay"})
	second, secondErr := f.requestHeaders("POST", "/v1/chat/completions", "application/json", body, map[string]string{"Idempotency-Key": "cost-exact-replay"})
	if firstErr == nil && secondErr == nil && (first.status != http.StatusOK || second.status != http.StatusOK || !bytes.Equal(first.body, second.body) || f.count() != 1) {
		err = fmt.Errorf("completed idempotency replay was not exact or redispatched: statuses=%d/%d calls=%d", first.status, second.status, f.count())
	} else if firstErr != nil {
		err = firstErr
	} else if secondErr != nil {
		err = secondErr
	}
	whitespaceEvidence := map[string]any{}
	if err == nil {
		whitespace, whitespaceErr := f.requestHeaders("POST", "/v1/chat/completions", "application/json", body, map[string]string{"Idempotency-Key": "cost-exact replay"})
		whitespaceEvidence = map[string]any{"status": whitespace.status, "upstream_calls": f.count()}
		if whitespaceErr != nil {
			err = whitespaceErr
		} else if whitespace.status != http.StatusOK || f.count() != 2 {
			err = fmt.Errorf("whitespace-distinct idempotency key was reused: status=%d calls=%d", whitespace.status, f.count())
		}
	}
	// A second in-flight request must receive a durable pending conflict and
	// must not reach the provider while the first owner is blocked.
	entered := sync.Once{}
	enteredCh := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseOwner := func() { releaseOnce.Do(func() { close(release) }) }
	var blockedCalls int32
	blocked, blockedErr := e.operationFixture("cost-idempotency-pending", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		atomic.AddInt32(&blockedCalls, 1)
		entered.Do(func() { close(enteredCh) })
		<-release
		return operationJSON(w, map[string]any{"id": "cost-pending", "object": "chat.completion", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	if blockedErr == nil {
		blockedErr = e.costConfigureModel(blocked, costPrice(), false)
	}
	pendingEvidence := map[string]any{}
	if blockedErr == nil {
		pendingBody := []byte(`{"model":"` + blocked.alias + `","messages":[{"role":"user","content":"pending"}],"max_tokens":8}`)
		done := make(chan operationReply, 1)
		go func() {
			reply, _ := blocked.requestHeaders("POST", "/v1/chat/completions", "application/json", pendingBody, map[string]string{"Idempotency-Key": "cost-pending"})
			done <- reply
		}()
		select {
		case <-enteredCh:
		case <-time.After(3 * time.Second):
			blockedErr = fmt.Errorf("idempotency owner did not reach upstream fixture")
		}
		var pending operationReply
		if blockedErr == nil {
			pending, blockedErr = blocked.requestHeaders("POST", "/v1/chat/completions", "application/json", pendingBody, map[string]string{"Idempotency-Key": "cost-pending"})
			if blockedErr == nil && (pending.status != http.StatusConflict || pending.header.Get("X-Hoorific-Error-Code") != "idempotency_in_progress" || atomic.LoadInt32(&blockedCalls) != 1) {
				blockedErr = fmt.Errorf("pending idempotency request was not rejected without dispatch: HTTP %d/%s calls=%d", pending.status, pending.header.Get("X-Hoorific-Error-Code"), atomic.LoadInt32(&blockedCalls))
			}
		}
		releaseOwner()
		var firstPending operationReply
		select {
		case firstPending = <-done:
		case <-time.After(3 * time.Second):
			if blockedErr == nil {
				blockedErr = fmt.Errorf("idempotency owner did not complete after release")
			}
		}
		pendingEvidence = map[string]any{"pending_status": pending.status, "pending_error": pending.header.Get("X-Hoorific-Error-Code"), "owner_status": firstPending.status, "upstream_calls": atomic.LoadInt32(&blockedCalls)}
	}
	if blocked != nil {
		releaseOwner()
		blocked.server.Close()
	}
	if err == nil {
		err = blockedErr
	}
	// Fingerprints include semantic headers/body: a different body must not
	// reuse a completed response.
	mismatch, mismatchErr := e.operationFixture("cost-idempotency-mismatch", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		return operationJSON(w, map[string]any{"id": "cost-mismatch", "object": "chat.completion", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	if mismatchErr == nil {
		mismatchErr = e.costConfigureModel(mismatch, costPrice(), false)
	}
	mismatchEvidence := map[string]any{}
	if mismatchErr == nil {
		key := map[string]string{"Idempotency-Key": "cost-fingerprint"}
		firstMismatch, requestErr := mismatch.requestHeaders("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+mismatch.alias+`","messages":[{"role":"user","content":"one"}],"max_tokens":8}`), key)
		if requestErr == nil {
			var secondMismatch operationReply
			secondMismatch, requestErr = mismatch.requestHeaders("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+mismatch.alias+`","messages":[{"role":"user","content":"two"}],"max_tokens":8}`), key)
			mismatchEvidence = map[string]any{"first_status": firstMismatch.status, "second_status": secondMismatch.status, "second_error": secondMismatch.header.Get("X-Hoorific-Error-Code"), "upstream_calls": mismatch.count()}
			if requestErr == nil && (firstMismatch.status != http.StatusOK || secondMismatch.status != http.StatusConflict || secondMismatch.header.Get("X-Hoorific-Error-Code") != "idempotency_conflict" || mismatch.count() != 1) {
				requestErr = fmt.Errorf("idempotency fingerprint mismatch was not rejected: statuses=%d/%d code=%s calls=%d", firstMismatch.status, secondMismatch.status, secondMismatch.header.Get("X-Hoorific-Error-Code"), mismatch.count())
			}
		}
		mismatchErr = requestErr
	}
	if mismatch != nil {
		mismatch.server.Close()
	}
	if err == nil {
		err = mismatchErr
	}
	if err == nil {
		if restart := e.restartRecovery(); restart.Status != "passed" {
			err = fmt.Errorf("idempotency restart recovery: %s", restart.Detail)
		} else {
			replayed, replayErr := f.requestHeaders("POST", "/v1/chat/completions", "application/json", body, map[string]string{"Idempotency-Key": "cost-exact-replay"})
			if replayErr != nil {
				err = replayErr
			} else if replayed.status != first.status || !bytes.Equal(replayed.body, first.body) || f.count() != 2 {
				err = fmt.Errorf("completed idempotency record did not replay after restart: status=%d calls=%d", replayed.status, f.count())
			}
		}
	}
	// A different authorized key has an independent idempotency namespace.
	isolationEvidence := map[string]any{}
	if err == nil {
		secondToken, issueErr := e.extIssue("cost-idempotency-second-key", []string{f.alias}, []string{f.id}, false)
		if issueErr != nil {
			err = issueErr
		} else {
			other, requestErr := e.costRequestWithToken(context.Background(), f, secondToken, "POST", "/v1/chat/completions", "application/json", body, map[string]string{"Idempotency-Key": "cost-exact-replay"})
			isolationEvidence = map[string]any{"second_key_status": other.status, "upstream_calls": f.count()}
			if requestErr != nil {
				err = requestErr
			} else if other.status != http.StatusOK || f.count() != 3 {
				err = fmt.Errorf("authorized key isolation reused another key's idempotency response: status=%d calls=%d", other.status, f.count())
			} else {
				revokeStatus, revokeErr := e.costRevokeKey("cost-idempotency-second-key")
				replay, replayErr := e.costRequestWithToken(context.Background(), f, secondToken, "POST", "/v1/chat/completions", "application/json", body, map[string]string{"Idempotency-Key": "cost-exact-replay"})
				isolationEvidence["revoke_status"] = revokeStatus
				isolationEvidence["replay_after_revoke_status"] = replay.status
				isolationEvidence["replay_after_revoke_upstream_calls"] = f.count()
				if revokeErr != nil {
					err = revokeErr
				} else if replayErr != nil {
					err = replayErr
				} else if replay.status != http.StatusUnauthorized || f.count() != 3 {
					err = fmt.Errorf("revoked key replay was admitted: status=%d calls=%d", replay.status, f.count())
				}
			}
		}
	}
	return extResult("cost/idempotency", start, map[string]any{"exact_statuses": []int{first.status, second.status}, "exact_upstream_calls": f.count(), "whitespace_distinct": whitespaceEvidence, "pending": pendingEvidence, "mismatch": mismatchEvidence, "authorized_key_isolation": isolationEvidence}, err)
}

func (e *environment) costStreamingReplay() result {
	start := time.Now()
	f, err := e.operationFixture("cost-streaming-idempotency", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
			return decodeErr
		}
		if body["stream"] != true {
			return fmt.Errorf("streaming fixture request lost stream=true")
		}
		if raw, ok := body["stream_options"]; ok {
			options, ok := raw.(map[string]any)
			if !ok || options["include_usage"] != true {
				return fmt.Errorf("streaming fixture include_usage was not true: %v", raw)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w, "data: {\"id\":\"cost-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"delta\":{\"content\":\"O\"},\"finish_reason\":null}]}\n\n"+"data: {\"id\":\"cost-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"delta\":{\"content\":\"K\"},\"finish_reason\":null}]}\n\n"+"data: {\"id\":\"cost-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"+"data: [DONE]\n\n")
		return err
	})
	if err == nil {
		err = e.costConfigureModel(f, costPrice(), true)
	}
	hiddenEvidence := map[string]any{}
	if err != nil {
		if f != nil {
			f.server.Close()
		}
		return extResult("cost/streaming-idempotency", start, nil, err)
	}
	visibleBody := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"stream"}],"max_tokens":8,"stream":true,"stream_options":{"include_usage":true}}`)
	first, firstErr := f.requestHeaders("POST", "/v1/chat/completions", "application/json", visibleBody, map[string]string{"Idempotency-Key": "cost-stream-replay"})
	second, secondErr := f.requestHeaders("POST", "/v1/chat/completions", "application/json", visibleBody, map[string]string{"Idempotency-Key": "cost-stream-replay"})
	if firstErr == nil && secondErr == nil && (first.status != http.StatusOK || second.status != http.StatusOK || !bytes.Equal(first.body, second.body) || !bytes.Contains(first.body, []byte(`"content":"O"`)) || !bytes.Contains(first.body, []byte(`"completion_tokens":2`)) || f.count() != 1) {
		err = fmt.Errorf("stream first response or exact replay failed: statuses=%d/%d calls=%d body=%s", first.status, second.status, f.count(), trim(string(first.body)))
	} else if firstErr != nil {
		err = firstErr
	} else if secondErr != nil {
		err = secondErr
	}
	if err == nil {
		admission, waitErr := e.costWaitAdmission(first.header.Get("X-Request-ID"), "settled", 5*time.Second)
		if waitErr != nil {
			err = waitErr
		} else {
			err = costAssertUsage(admission, map[string]int64{"Input": 3, "Output": 2, "Total": 5})
		}
	}
	if err == nil {
		hiddenBody := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"hidden"}],"max_tokens":8,"stream":true}`)
		hidden, hiddenErr := f.request("POST", "/v1/chat/completions", "application/json", hiddenBody)
		hiddenEvidence = map[string]any{
			"status":         hidden.status,
			"usage_visible":  bytes.Contains(hidden.body, []byte(`"completion_tokens"`)),
			"upstream_calls": f.count(),
		}
		if hiddenErr != nil {
			err = hiddenErr
		} else if hidden.status != http.StatusOK || !bytes.Contains(hidden.body, []byte(`"content":"O"`)) || bytes.Contains(hidden.body, []byte(`"prompt_tokens"`)) || bytes.Contains(hidden.body, []byte(`"completion_tokens"`)) {
			err = fmt.Errorf("stream usage was not hidden when include_usage was omitted: status=%d body=%s", hidden.status, trim(string(hidden.body)))
		} else {
			admission, waitErr := e.costWaitAdmission(hidden.header.Get("X-Request-ID"), "settled", 5*time.Second)
			if waitErr != nil {
				err = waitErr
			} else {
				err = costAssertUsage(admission, map[string]int64{"Input": 3, "Output": 2, "Total": 5})
			}
		}
	}
	f.server.Close()

	responseEvidence := map[string]any{}
	var responses *operationFixture
	var responseErr error
	if err == nil {
		responses, responseErr = e.operationFixture("cost-responses-stream", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
			if r.URL.Path != "/chat/completions" {
				return fmt.Errorf("translated Responses fixture received unexpected path %s", r.URL.Path)
			}
			var body map[string]any
			if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
				return decodeErr
			}
			options, _ := body["stream_options"].(map[string]any)
			if body["stream"] != true || options["include_usage"] != true {
				return fmt.Errorf("translated Responses request lost upstream streaming usage")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := io.WriteString(w,
				"data: {\"id\":\"cost-response\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n"+
					"data: {\"id\":\"cost-response\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":22,\"completion_tokens\":7,\"total_tokens\":29,\"prompt_tokens_details\":{\"cached_tokens\":5},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n"+
					"data: [DONE]\n\n")
			return err
		})
		if responseErr == nil {
			responseErr = e.costConfigureModel(responses, costPriceWithCacheRead(), true)
		}
		if responseErr == nil {
			body := []byte(`{"model":"` + responses.alias + `","input":"responses stream","store":false,"stream":true}`)
			reply, requestErr := responses.request("POST", "/v1/responses", "application/json", body)
			responseEvidence = map[string]any{
				"status":                   reply.status,
				"response_completed":       bytes.Contains(reply.body, []byte("response.completed")),
				"cached_tokens_visible":    bytes.Contains(reply.body, []byte(`"cached_tokens":5`)),
				"reasoning_tokens_visible": bytes.Contains(reply.body, []byte(`"reasoning_tokens":3`)),
				"upstream_calls":           responses.count(),
			}
			if requestErr != nil {
				responseErr = requestErr
			} else if reply.status != http.StatusOK || !bytes.Contains(reply.body, []byte("response.completed")) || !bytes.Contains(reply.body, []byte(`"cached_tokens":5`)) || !bytes.Contains(reply.body, []byte(`"reasoning_tokens":3`)) || responses.count() != 1 {
				responseErr = fmt.Errorf("Responses stream usage proof failed: status=%d calls=%d body=%s", reply.status, responses.count(), trim(string(reply.body)))
			} else {
				admission, waitErr := e.costWaitAdmission(reply.header.Get("X-Request-ID"), "settled", 5*time.Second)
				if waitErr != nil {
					responseErr = waitErr
				} else {
					responseErr = costAssertUsage(admission, map[string]int64{"Input": 22, "Output": 7, "Total": 29, "CachedInput": 5, "ReasoningOutput": 3})
				}
			}
		}
	}
	if responses != nil {
		responses.server.Close()
	}
	if err == nil {
		err = responseErr
	}
	return extResult("cost/streaming-idempotency", start, map[string]any{"first_byte_content": bytes.Contains(first.body, []byte(`"content":"O"`)), "terminal_usage": bytes.Contains(first.body, []byte(`"completion_tokens":2`)), "hidden_usage": hiddenEvidence, "responses_stream": responseEvidence, "upstream_calls": f.count()}, err)
}

func (e *environment) costRetryAfter() result {
	start := time.Now()
	f, err := e.operationFixture("cost-retry-after-90", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Retry-After", "90")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, err := io.WriteString(w, "fixture rate limit")
		return err
	})
	if err == nil {
		err = e.costConfigureModel(f, costPrice(), false)
	}
	var portable, native operationReply
	if err == nil {
		portable, err = f.request("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+f.alias+`","messages":[{"role":"user","content":"retry"}],"max_tokens":8}`))
	}
	if err == nil {
		native, err = f.request("POST", "/connect/"+f.id+"/native/chat/completions", "application/json", []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"retry"}],"max_tokens":8}`))
	}
	if err == nil {
		if portable.status != http.StatusTooManyRequests || native.status != http.StatusTooManyRequests || portable.header.Get("Retry-After") != "90" || native.header.Get("Retry-After") != "90" {
			err = fmt.Errorf("Retry-After was not preserved: portable=%d/%q native=%d/%q", portable.status, portable.header.Get("Retry-After"), native.status, native.header.Get("Retry-After"))
		}
	}
	if err == nil {
		for _, requestID := range []string{portable.header.Get("X-Request-ID"), native.header.Get("X-Request-ID")} {
			admission, waitErr := e.costWaitAdmission(requestID, "not_executed", 5*time.Second)
			if waitErr != nil {
				err = waitErr
				break
			}
			if costFieldString(admission.Data, "state", "State") == "outcome_unknown" {
				err = fmt.Errorf("known provider 429 was classified as outcome_unknown")
				break
			}
		}
	}
	if f != nil {
		f.server.Close()
	}
	return extResult("cost/retry-after-90", start, map[string]any{"portable_status": portable.status, "portable_retry_after": portable.header.Get("Retry-After"), "native_status": native.status, "native_retry_after": native.header.Get("Retry-After"), "upstream_calls": f.count()}, err)
}

func (e *environment) costCohereCancel() result {
	start := time.Now()
	cancelCalls := 0
	f, err := e.operationFixture("cost-cohere-cancel", "cohere", []string{"batch"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			cancelCalls++
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, "{}")
			return err
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/embed-jobs" {
			return operationJSON(w, map[string]any{"job_id": "cost-cohere-job", "status": "processing"})
		}
		return fmt.Errorf("unexpected Cohere lifecycle route %s %s", r.Method, r.URL.Path)
	})
	if err == nil {
		err = e.costConfigureModel(f, costPrice(), false)
	}
	var create, canceled operationReply
	var adminCanceled deepRawObservation
	var requestErr error
	if err == nil {
		create, err = f.request("POST", "/connect/"+f.id+"/native/v1/embed-jobs", "application/json", []byte(`{"model":"fixture-model","dataset_id":"cost-data"}`))
		if err == nil {
			err = costAssertResponse(create, http.StatusOK, "cost-cohere-job")
		}
	}
	var job struct {
		Version int64                      `json:"version"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if err == nil {
		jobObs, requestErr := e.extRequest(context.Background(), true, http.MethodGet, "/admin/api/v1/upstream_operations/cost-cohere-job", nil, nil)
		err = requestErr
		if err == nil && jobObs.Status == http.StatusOK {
			err = json.Unmarshal([]byte(jobObs.Body), &job)
		}
		if err == nil && (job.Version < 1 || job.Data == nil) {
			err = fmt.Errorf("Cohere job omitted durable version/data")
		}
	}
	if err == nil {
		canceled, err = f.request("POST", "/connect/"+f.id+"/native/v1/embed-jobs/cost-cohere-job/cancel", "application/json", nil)
		if err == nil && (canceled.status != http.StatusOK || strings.TrimSpace(string(canceled.body)) != "{}") {
			err = fmt.Errorf("Cohere native cancel did not return empty JSON: HTTP %d body=%s", canceled.status, trim(string(canceled.body)))
		}
	}
	if err == nil {
		adminCanceled, requestErr = e.deepCall(true, http.MethodPost, "/admin/api/v1/upstream_operations/cost-cohere-job/cancel", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
			r.Header.Set("If-Match", fmt.Sprint(job.Version))
		})
		err = requestErr
		if err == nil && (adminCanceled.Status < 200 || adminCanceled.Status >= 300 || cancelCalls < 2) {
			err = fmt.Errorf("admin Cohere cancel did not dispatch actual endpoint: HTTP %d/%s body=%s cancel_calls=%d", adminCanceled.Status, adminCanceled.ErrorCode, trim(string(adminCanceled.Body)), cancelCalls)
		}
	}
	if f != nil {
		f.server.Close()
	}
	return extResult("cost/cohere-cancel", start, map[string]any{"create_status": create.status, "native_cancel_status": canceled.status, "native_cancel_body": strings.TrimSpace(string(canceled.body)), "admin_cancel_status": adminCanceled.Status, "admin_cancel_error_code": adminCanceled.ErrorCode, "admin_cancel_body": trim(string(adminCanceled.Body)), "admin_cancel_calls": cancelCalls, "upstream_calls": f.count()}, err)
}

func (e *environment) costReplicateControls() result {
	start := time.Now()
	var received http.Header
	f, err := e.operationFixture("cost-replicate-controls", "replicate", []string{"prediction.create"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/predictions" {
			received = r.Header.Clone()
			return operationJSON(w, map[string]any{"id": "cost-replicate-prediction", "status": "starting"})
		}
		return fmt.Errorf("unexpected Replicate route %s %s", r.Method, r.URL.Path)
	})
	if err == nil {
		err = e.costConfigureModel(f, costPrice(), false)
	}
	body := []byte(`{"version":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","input":{"x":1}}`)
	var valid operationReply
	if err == nil {
		valid, err = f.requestHeaders("POST", "/connect/"+f.id+"/native/v1/predictions", "application/json", body, map[string]string{"Cancel-After": "10s", "Prefer": "wait=5"})
	}
	if err == nil && (valid.status != http.StatusOK || received.Get("Cancel-After") != "10s" || received.Get("Prefer") != "wait=5") {
		err = fmt.Errorf("Replicate control headers were not retained: status=%d Cancel-After=%q Prefer=%q", valid.status, received.Get("Cancel-After"), received.Get("Prefer"))
	}
	before := f.count()
	invalid, requestErr := f.requestHeaders("POST", "/connect/"+f.id+"/native/v1/predictions", "application/json", body, map[string]string{"Cancel-After": "1s"})
	if err == nil {
		err = requestErr
	}
	if err == nil && (invalid.status != http.StatusBadRequest || f.count() != before) {
		err = fmt.Errorf("invalid Replicate deadline was not rejected before dispatch: HTTP %d calls_before=%d calls_after=%d", invalid.status, before, f.count())
	}
	if f != nil {
		f.server.Close()
	}
	return extResult("cost/replicate-controls", start, map[string]any{"valid_status": valid.status, "cancel_after": received.Get("Cancel-After"), "prefer": received.Get("Prefer"), "invalid_status": invalid.status, "upstream_calls": f.count()}, err)
}

func (e *environment) costBudgetAndReconcile() result {
	start := time.Now()
	evidence := map[string]any{}
	budget, err := e.operationFixture("cost-budget", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		return operationJSON(w, map[string]any{"id": "cost-budget", "object": "chat.completion", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	if err == nil {
		err = e.costConfigureModel(budget, costPrice(), false)
	}
	const policyID = "cost-safety-budget"
	if err == nil {
		err = e.extCreate("policy_limits", policyID, map[string]any{"scope": "tenant", "scope_id": e.tenantID, "max_cost": int64(1), "cost_window": "total"})
	}
	var rejected operationReply
	if err == nil {
		before := budget.count()
		rejected, err = budget.request("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+budget.alias+`","messages":[{"role":"user","content":"budget"}],"max_tokens":8}`))
		evidence["budget_upstream_calls"] = budget.count() - before
		if err == nil && (rejected.status != http.StatusTooManyRequests || rejected.header.Get("X-Hoorific-Error-Code") != "quota_exceeded" || budget.count() != before) {
			err = fmt.Errorf("budget did not reject before paid dispatch: HTTP %d/%s calls=%d", rejected.status, rejected.header.Get("X-Hoorific-Error-Code"), budget.count()-before)
		}
	}
	if err == nil {
		removed, removeErr := e.extRequest(context.Background(), true, http.MethodDelete, "/admin/api/v1/policy_limits/"+policyID, nil, func(r *http.Request) { r.Header.Set("If-Match", "1") })
		err = removeErr
		if err == nil && removed.Status < 200 || err == nil && removed.Status >= 300 {
			err = fmt.Errorf("budget policy cleanup returned HTTP %d", removed.Status)
		}
	}
	if budget != nil {
		budget.server.Close()
	}
	const missingPolicyID = "cost-safety-missing-usage"
	missingPolicyCreated := false
	if err == nil {
		err = e.extCreate("policy_limits", missingPolicyID, map[string]any{"scope": "tenant", "scope_id": e.tenantID, "max_cost": int64(1_000_000_000_000), "cost_window": "total"})
		missingPolicyCreated = err == nil
	}
	missing, setupErr := e.operationFixture("cost-missing-usage", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		return operationJSON(w, map[string]any{"id": "cost-missing-usage", "object": "chat.completion", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}})
	})
	if err == nil {
		err = setupErr
	}
	if err == nil {
		err = e.costConfigureModel(missing, costPrice(), false)
	}
	var unknown operationReply
	var admission, settled costAdmissionRecord
	if err == nil {
		unknown, err = missing.request("POST", "/v1/chat/completions", "application/json", []byte(`{"model":"`+missing.alias+`","messages":[{"role":"user","content":"missing usage"}],"max_tokens":8}`))
		if err == nil && (unknown.status != http.StatusOK || unknown.header.Get("X-Request-ID") == "" || missing.count() != 1) {
			err = fmt.Errorf("missing provider usage was not held for reconciliation: HTTP %d/%s request_id=%q calls=%d", unknown.status, unknown.header.Get("X-Hoorific-Error-Code"), unknown.header.Get("X-Request-ID"), missing.count())
		}
	}
	if err == nil {
		admission, err = e.costWaitAdmission(unknown.header.Get("X-Request-ID"), "outcome_unknown", 5*time.Second)
	}
	if err == nil {
		reconciled, reconcileErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/admissions/"+admission.ID+"/reconcile", map[string]any{"reconciliation_id": "cost-missing-usage-reconciliation", "mode": "provider_evidence", "reason": "deterministic provider evidence", "source_reference": "cost-safety-fixture", "usage": map[string]any{"Input": int64(13), "Output": int64(7), "Total": int64(20), "Source": "fixture"}}, func(r *http.Request) {
			r.Header.Set("If-Match", fmt.Sprint(admission.Version))
		})
		err = reconcileErr
		if err == nil && (reconciled.Status < 200 || reconciled.Status >= 300) {
			err = fmt.Errorf("missing usage reconciliation returned HTTP %d/%s body=%s", reconciled.Status, reconciled.ErrorCode, trim(string(reconciled.Body)))
		}
		if err == nil {
			var waitErr error
			// Reconciliation preserves the observed execution state and records
			// the accounting resolution separately.
			settled, waitErr = e.costWaitAdmission(unknown.header.Get("X-Request-ID"), "outcome_unknown", 5*time.Second)
			err = waitErr
			if err == nil {
				if !costFieldBool(settled.Data, "reconciled", "Reconciled") {
					err = fmt.Errorf("reconciled admission did not expose reconciled=true")
				} else if actual, ok := costFieldInt(settled.Data, "actual_cost", "ActualCost"); !ok || actual != 27000 {
					err = fmt.Errorf("reconciled admission actual_cost=%d want 27000", actual)
				}
			}
		}
	}
	if missing != nil {
		missing.server.Close()
	}
	if missingPolicyCreated {
		removed, removeErr := e.extRequest(context.Background(), true, http.MethodDelete, "/admin/api/v1/policy_limits/"+missingPolicyID, nil, func(r *http.Request) {
			r.Header.Set("If-Match", "1")
		})
		if err == nil {
			if removeErr != nil {
				err = removeErr
			} else if removed.Status < 200 || removed.Status >= 300 {
				err = fmt.Errorf("missing usage policy cleanup returned HTTP %d", removed.Status)
			}
		}
	}
	evidence["budget_status"] = rejected.status
	evidence["missing_usage_status"] = unknown.status
	evidence["missing_usage_error"] = unknown.header.Get("X-Hoorific-Error-Code")
	evidence["missing_usage_reconciled"] = costFieldBool(settled.Data, "reconciled", "Reconciled")
	return extResult("cost/budget-and-reconcile", start, evidence, err)
}

func costFieldBool(data map[string]json.RawMessage, names ...string) bool {
	raw := costFieldRaw(data, names...)
	var value bool
	_ = json.Unmarshal(raw, &value)
	return value
}
