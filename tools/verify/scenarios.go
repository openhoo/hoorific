package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// extendedScenarios is deliberately sequential: fixture modes and administrative
// mutations must never race unrelated assertions. Only isolated verification
// credentials and loopback upstreams are used; no provider discovery is implicit.
func (e *environment) extendedScenarios() []result {
	out := []result{}
	add := func(r result) { verificationResult(r); out = append(out, r) }
	if e.key == "" || e.fixture == nil || e.cookie == "" {
		return []result{{"extended/setup", "failed", "isolated seeded key, fixture and administration session required", 0, nil}}
	}
	original := e.fixture.mode.Load().(string)
	defer e.fixture.mode.Store(original)
	e.fixture.mode.Store("normal")
	for _, s := range []struct{ name, path, body, code string }{
		{"root-unknown-openai", "/v1/chat/completions", `{"model":"assistant","messages":[{"role":"user","content":"OK"}],"max_tokens":16,"fixture_unknown":{"nested":true}}`, "unsupported_feature"},
		{"root-unknown-anthropic", "/v1/messages", `{"model":"assistant","messages":[{"role":"user","content":"OK"}],"max_tokens":16,"fixture_unknown":{"nested":true}}`, "unsupported_feature"},
		{"root-unknown-gemini", "/v1beta/models/assistant:generateContent", `{"contents":[{"role":"user","parts":[{"text":"OK"}]}],"fixture_unknown":true}`, "unsupported_feature"},
		{"root-stored-chat", "/v1/chat/completions", `{"model":"assistant","messages":[{"role":"user","content":"OK"}],"max_tokens":16,"store":true}`, "connection_required"},
		{"root-responses-default-store", "/v1/responses", `{"model":"assistant","input":"OK"}`, "connection_required"},
		{"root-responses-store", "/v1/responses", `{"model":"assistant","input":"OK","store":true}`, "connection_required"},
		{"root-stored-response-reference", "/v1/responses", `{"model":"assistant","input":"OK","store":false,"previous_response_id":"resp_same"}`, "connection_required"},
		{"root-batch-reference", "/v1/chat/completions", `{"model":"assistant","messages":[],"batch_id":"batch_same"}`, "connection_required"},
	} {
		add(e.extReject("protocol/"+s.name, "POST", s.path, []byte(s.body), nil, 400, s.code))
	}
	add(e.extNativeFields())
	for _, mode := range []string{"normal", "unicode", "tool", "named-error", "truncate"} {
		add(e.extStream(mode))
	}
	add(e.extCancel())
	for _, r := range e.extReplay() {
		add(r)
	}
	for _, r := range e.extSecurity() {
		add(r)
	}
	add(e.extReject("realtime/ticket-needs-explicit-grants", "POST", "/gateway/v1/realtime/tickets", []byte(`{"connection_id":"fixture-connection","model":"fixture-model"}`), nil, 403, "forbidden"))
	add(e.extReject("realtime/invalid-ticket", "GET", "/connect/fixture-connection/openai/v1/realtime?model=fixture-model&ticket=invalid", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "Upgrade")
	}, 401, "unauthorized"))
	for _, r := range e.extAdmin() {
		add(r)
	}
	verificationProgress("credentials/lifecycle", "started", 0)
	for _, r := range e.credentialLifecycle() {
		add(r)
	}
	verificationProgress("realtime/success", "started", 0)
	for _, r := range e.realtimeSuccess() {
		add(r)
	}
	verificationProgress("operations/success", "started", 0)
	for _, r := range e.operationsSuccess() {
		add(r)
	}
	verificationProgress("sdk", "started", 0)
	for _, r := range e.extSDK() {
		add(r)
	}
	return out
}

type extObservation struct {
	Status        int    `json:"status"`
	ErrorCode     string `json:"error_code,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	ContentType   string `json:"content_type,omitempty"`
	Body          string `json:"body"`
	UpstreamCalls int    `json:"upstream_calls"`
}

func (e *environment) extRequest(ctx context.Context, management bool, method, path string, body []byte, headers func(*http.Request)) (extObservation, error) {
	address := e.inference
	if management {
		address = e.management
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, bytes.NewReader(body))
	if err != nil {
		return extObservation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if management {
		req.Header.Set("Cookie", e.cookie)
		req.Header.Set("Origin", "http://"+e.management)
		req.Header.Set("X-CSRF-Token", e.csrf)
	} else {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	if headers != nil {
		headers(req)
	}
	before := e.fixture.count()
	// Redirects must remain visible: never send a gateway credential to Location.
	client := *e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return extObservation{UpstreamCalls: e.fixture.count() - before}, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	obs := extObservation{resp.StatusCode, resp.Header.Get("X-Hoorific-Error-Code"), resp.Header.Get("X-Request-ID"), resp.Header.Get("Content-Type"), e.extRedact(string(raw)), e.fixture.count() - before}
	if len(raw) > 4<<20 {
		return obs, fmt.Errorf("gateway response exceeded 4 MiB evidence limit")
	}
	return obs, readErr
}

func (e *environment) extRedact(s string) string {
	for _, secret := range []string{e.key, e.cookie, e.csrf, "fixture-upstream-secret", "verify-replay-secret"} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}

func extResult(name string, start time.Time, evidence any, err error) result {
	r := result{Name: name, Status: "passed", Detail: "observed actual gateway behavior", DurationMS: time.Since(start).Milliseconds(), Evidence: evidence}
	if err != nil {
		r.Status = "failed"
		r.Detail = err.Error()
	}
	verificationResult(r)
	return r
}

func (e *environment) extReject(name, method, path string, body []byte, headers func(*http.Request), status int, code string) result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	obs, err := e.extRequest(ctx, false, method, path, body, headers)
	if err == nil && (obs.Status != status || obs.ErrorCode != code || obs.UpstreamCalls != 0) {
		err = fmt.Errorf("expected HTTP %d %s with zero dispatch; got HTTP %d %s and %d upstream calls", status, code, obs.Status, obs.ErrorCode, obs.UpstreamCalls)
	}
	return extResult(name, start, obs, err)
}

func (e *environment) extNativeFields() result {
	start := time.Now()
	token, err := e.extIssue("verify-native-fields", []string{"assistant"}, []string{"fixture-connection"}, true)
	if err != nil {
		return extResult("protocol/pinned-native-unknown-field", start, nil, err)
	}
	body := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"OK"}],"max_tokens":16,"fixture_unknown":{"nested":[true,3,"keep"]}}`)
	obs, err := e.extRequest(context.Background(), false, "POST", "/connect/fixture-connection/anthropic/v1/messages", body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	if err == nil && (obs.Status != 200 || obs.UpstreamCalls != 1) {
		err = fmt.Errorf("native call expected 200 and one dispatch, got %d/%d", obs.Status, obs.UpstreamCalls)
	}
	var got map[string]json.RawMessage
	last := e.fixture.last()
	if err == nil {
		err = json.Unmarshal(last.Body, &got)
	}
	credentialMatched := last.CredentialHeader == "X-Api-Key" && last.CredentialCount == 1 && !last.CredentialConflict && last.Credential == "fixture-upstream-secret"
	if err == nil && (string(got["fixture_unknown"]) != `{"nested":[true,3,"keep"]}` || last.Path != "/messages" || !credentialMatched) {
		err = fmt.Errorf("native unknown semantics, path or configured upstream credential not preserved")
	}
	return extResult("protocol/pinned-native-unknown-field", start, map[string]any{"response": obs, "upstream_path": last.Path, "upstream_model": string(got["model"]), "unknown_field_preserved": string(got["fixture_unknown"]) == `{"nested":[true,3,"keep"]}`, "configured_credential_matched": credentialMatched, "credential_header": last.CredentialHeader}, err)
}

type extStreamSummary struct {
	Text      string `json:"text"`
	Finish    string `json:"finish"`
	Done      int    `json:"done"`
	Usage     int    `json:"usage_events"`
	Input     int    `json:"input_tokens"`
	Output    int    `json:"output_tokens"`
	Errors    int    `json:"errors"`
	ToolID    string `json:"tool_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func extParseOpenAI(raw string) (extStreamSummary, error) {
	var sum extStreamSummary
	if !utf8.ValidString(raw) {
		return sum, fmt.Errorf("stream contains corrupt UTF-8")
	}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	data := []string{}
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		text := strings.Join(data, "\n")
		data = nil
		if text == "[DONE]" {
			sum.Done++
			return nil
		}
		if sum.Done != 0 {
			return fmt.Errorf("event arrived after terminal DONE")
		}
		var event struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index  int     `json:"index"`
				Finish *string `json:"finish_reason"`
				Delta  struct {
					Content string `json:"content"`
					Tools   []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(text), &event); err != nil {
			return fmt.Errorf("invalid SSE JSON: %w", err)
		}
		if len(event.Error) > 0 && string(event.Error) != "null" {
			sum.Errors++
		}
		if event.Usage != nil {
			sum.Usage++
			sum.Input = event.Usage.Input
			sum.Output = event.Usage.Output
		}
		for _, choice := range event.Choices {
			if choice.Index != 0 {
				return fmt.Errorf("unexpected choice index %d", choice.Index)
			}
			sum.Text += choice.Delta.Content
			if choice.Finish != nil {
				if sum.Finish != "" {
					return fmt.Errorf("duplicate finish event")
				}
				sum.Finish = *choice.Finish
			}
			for _, call := range choice.Delta.Tools {
				if call.Index != 0 {
					return fmt.Errorf("unexpected tool index")
				}
				if call.ID != "" {
					if sum.ToolID != "" && sum.ToolID != call.ID {
						return fmt.Errorf("tool id changed")
					}
					sum.ToolID = call.ID
				}
				sum.ToolName += call.Function.Name
				sum.Arguments += call.Function.Arguments
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return sum, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return sum, err
	}
	if len(data) > 0 {
		return sum, fmt.Errorf("unterminated SSE frame")
	}
	return sum, nil
}

func (e *environment) extStream(mode string) result {
	start := time.Now()
	e.fixture.mode.Store(mode)
	defer e.fixture.mode.Store("normal")
	obs, readErr := e.extRequest(context.Background(), false, "POST", "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"OK"}],"max_tokens":16,"stream":true,"stream_options":{"include_usage":true}}`), nil)
	sum, parseErr := extParseOpenAI(obs.Body)
	err := readErr
	if err == nil {
		err = parseErr
	}
	if obs.Status != 200 {
		err = fmt.Errorf("stream returned HTTP %d", obs.Status)
	}
	if obs.UpstreamCalls != 1 {
		err = fmt.Errorf("stream must dispatch exactly once, observed %d", obs.UpstreamCalls)
	}
	if !strings.Contains(obs.ContentType, "text/event-stream") {
		err = fmt.Errorf("stream content type %q", obs.ContentType)
	}
	if mode == "truncate" || mode == "named-error" {
		// An explicit error or transport failure is required; a clean EOF without
		// terminal/error is not a successful representation of upstream truncation.
		if sum.Finish != "" || sum.Done != 0 {
			err = fmt.Errorf("synthetic successful terminal after upstream %s", mode)
		} else if sum.Errors == 0 && readErr == nil {
			err = fmt.Errorf("upstream %s became clean EOF without explicit error", mode)
		} else if obs.Status == 200 && obs.UpstreamCalls == 1 {
			err = nil
		}
	} else if err == nil {
		expected := "OK"
		finish := "stop"
		if mode == "unicode" {
			expected = "Grüße 🌍"
		}
		if mode == "tool" {
			expected = ""
			finish = "tool_calls"
		}
		if sum.Text != expected || sum.Finish != finish || sum.Done != 1 || sum.Usage != 1 || sum.Input != 9 || sum.Output != 2 || sum.Errors != 0 {
			err = fmt.Errorf("incorrect text/finish/terminal/usage: %+v", sum)
		}
		if mode == "tool" && err == nil {
			var args struct {
				City string `json:"city"`
				Note string `json:"note"`
			}
			if json.Unmarshal([]byte(sum.Arguments), &args) != nil || args.City != "Zürich" || args.Note != strings.Repeat("line\n", 4096) || sum.ToolID != "tool_fixture" || sum.ToolName != "lookup" {
				err = fmt.Errorf("ordered fragmented tool call was not reconstructed exactly")
			}
		}
	}
	return extResult("stream/"+mode, start, map[string]any{"response": obs, "decoded": sum}, err)
}

func (e *environment) extCancel() result {
	start := time.Now()
	e.fixture.mode.Store("slow")
	defer e.fixture.mode.Store("normal")
	before := e.fixture.canceled.Load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+e.inference+"/v1/chat/completions", strings.NewReader(`{"model":"assistant","messages":[{"role":"user","content":"OK"}],"max_tokens":16,"stream":true}`))
	if err != nil {
		return extResult("stream/disconnect-propagation", start, nil, err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	var cancelMS int64
	if err == nil {
		reader := bufio.NewReader(resp.Body)
		// Wait for a complete committed frame, not merely response headers.
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				err = readErr
				break
			}
			if line == "\n" || line == "\r\n" {
				break
			}
		}
		at := time.Now()
		cancel()
		resp.Body.Close()
		for e.fixture.canceled.Load() == before && time.Since(at) < time.Second {
			time.Sleep(5 * time.Millisecond)
		}
		cancelMS = time.Since(at).Milliseconds()
		if resp.StatusCode != 200 {
			err = fmt.Errorf("stream returned %d", resp.StatusCode)
		}
		if e.fixture.canceled.Load() != before+1 {
			err = fmt.Errorf("upstream cancellation not observed within one second")
		}
	}
	return extResult("stream/disconnect-propagation", start, map[string]any{"cancellations": e.fixture.canceled.Load() - before, "cancel_ms": cancelMS}, err)
}

func (e *environment) extCreate(kind, id string, data any) error {
	raw, err := json.Marshal(map[string]any{"id": id, "data": data})
	if err != nil {
		return err
	}
	obs, err := e.extRequest(context.Background(), true, "POST", "/admin/api/v1/"+kind, raw, nil)
	if err == nil && (obs.Status < 200 || obs.Status >= 300) {
		err = fmt.Errorf("seed %s/%s: HTTP %d %s", kind, id, obs.Status, obs.Body)
	}
	return err
}

func (e *environment) extImportCredential(id, provider, account, secret string) error {
	raw, err := json.Marshal(map[string]any{"data": map[string]any{"provider": provider, "account_id": account, "kind": "api_key", "secret": secret}})
	if err != nil {
		return err
	}
	obs, err := e.extRequest(context.Background(), true, "POST", "/admin/api/v1/connections/"+id+"/import", raw, func(r *http.Request) {
		r.Header.Set("If-Match", "1")
	})
	if err == nil && (obs.Status < 200 || obs.Status >= 300) {
		err = fmt.Errorf("seed credential import/%s: HTTP %d %s", id, obs.Status, obs.Body)
	}
	return err
}

func (e *environment) extReplay() []result {
	out := []result{}
	for _, mode := range []string{"429", "disconnect"} {
		start := time.Now()
		prefix := "verify-replay-" + mode
		a, b := newFixture(), newFixture()
		a.mode.Store(mode)
		err := func() error {
			for index, f := range []*fixture{a, b} {
				id := fmt.Sprintf("%s-%d", prefix, index)
				if err := e.extCreate("connections", id, map[string]any{"connector": "anthropic", "account_id": id, "base_url": f.server.URL, "dedicated": true, "enabled": true, "settings": map[string]string{"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}}); err != nil {
					return err
				}
				if err := e.extImportCredential(id, "anthropic", id, "verify-replay-secret"); err != nil {
					return err
				}
				if err := e.extCreate("models", id, map[string]any{"connection_id": id, "upstream_id": "fixture-chat", "operations": []string{"generate"}, "input_modalities": []string{"text"}, "output_modalities": []string{"text"}, "context_limit": 32768, "output_limit": 1024, "price": map[string]any{"version": "verify-v1", "input_per_million": 1000000000, "output_per_million": 2000000000, "maximum_unit_cost": 1000000000, "unit_operation": "generate"}, "enabled": true}); err != nil {
					return err
				}
			}
			if err := e.extCreate("model_aliases", prefix, map[string]any{"model_ids": []string{prefix + "-0", prefix + "-1"}, "enabled": true}); err != nil {
				return err
			}
			targets := []map[string]any{}
			for index := 0; index < 2; index++ {
				id := fmt.Sprintf("%s-%d", prefix, index)
				targets = append(targets, map[string]any{"connection_id": id, "model_id": id, "priority": index, "weight": 100})
			}
			return e.extCreate("route_policies", prefix, map[string]any{"alias": prefix, "targets": targets, "fallback": true, "affinity": false})
		}()
		obs := extObservation{}
		if err == nil {
			token, issueErr := e.extIssue(prefix, []string{prefix}, []string{prefix + "-0", prefix + "-1"}, false)
			err = issueErr
			if err == nil {
				body, _ := json.Marshal(map[string]any{"model": prefix, "messages": []map[string]string{{"role": "user", "content": "OK"}}, "max_tokens": 16})
				obs, err = e.extRequest(context.Background(), false, "POST", "/v1/chat/completions", body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
			}
		}
		if err == nil {
			if a.count() != 1 {
				err = fmt.Errorf("A expected exactly one attempt, observed %d", a.count())
			}
			if mode == "429" && (b.count() != 1 || obs.Status != 200 || !strings.Contains(obs.Body, `"OK"`)) {
				err = fmt.Errorf("known non-executing 429 must authorize exactly one successful B: B=%d status=%d", b.count(), obs.Status)
			}
			if mode == "disconnect" && (b.count() != 0 || obs.ErrorCode != "upstream_outcome_unknown") {
				err = fmt.Errorf("ambiguous dispatch must not retry: B=%d error=%s", b.count(), obs.ErrorCode)
			}
		}
		out = append(out, extResult("replay/"+mode, start, map[string]any{"response": obs, "a_calls": a.count(), "b_calls": b.count()}, err))
		a.server.Close()
		b.server.Close()
	}
	return out
}

func (e *environment) extIssue(id string, aliases, connections []string, native bool) (string, error) {
	permissions := []string{"inference:invoke"}
	for _, connection := range connections {
		permissions = append(permissions, "connection:"+connection)
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"name": id, "role": "operator", "permissions": permissions, "aliases": aliases, "connections": connections, "operations": []string{"generate"}, "portable": true, "native_account": native, "realtime": false}})
	obs, err := e.extRequest(context.Background(), true, "POST", "/admin/api/v1/api_keys/"+id+"/issue", raw, nil)
	var issued struct {
		Token string `json:"token"`
	}
	if err == nil && (obs.Status != 200 || json.Unmarshal([]byte(obs.Body), &issued) != nil || issued.Token == "") {
		err = fmt.Errorf("key issuance HTTP %d (one-time token omitted from evidence)", obs.Status)
	}
	return issued.Token, err
}

func (e *environment) extSecurity() []result {
	out := []result{}
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"OK"}],"max_tokens":16}`)
	for _, s := range []struct {
		name, path string
		headers    func(*http.Request)
		status     int
		code       string
	}{
		{"encoded-traversal", "/connect/fixture-connection/anthropic/v1/%2e%2e/messages", nil, 404, "unsupported_operation"},
		{"encoded-separator", "/connect/fixture-connection/anthropic/v1%2fmessages", nil, 404, "unsupported_operation"},
		{"cross-connection", "/connect/ungranted/anthropic/v1/messages", nil, 403, "forbidden"},
		{"credential-confusion", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("X-Api-Key", "upstream-not-gateway") }, 401, "unauthorized"},
		{"duplicate-authorization", "/v1/chat/completions", func(r *http.Request) { r.Header.Add("Authorization", "Bearer conflicting") }, 401, "unauthorized"},
	} {
		out = append(out, e.extReject("security/"+s.name, "POST", s.path, body, s.headers, s.status, s.code))
	}
	late := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 300<<10) + `"}],"max_tokens":16,"model":"forbidden-model"}`)
	out = append(out, e.extReject("security/late-forbidden-model", "POST", "/v1/chat/completions", late, nil, 404, "model_not_found"))
	duplicate := []byte(`{"model":"assistant","messages":[{"role":"user","content":"OK","content":"changed"}],"max_tokens":16}`)
	out = append(out, e.extReject("security/nested-duplicate-json", "POST", "/v1/chat/completions", duplicate, nil, 400, "invalid_request"))
	oversized := []byte(`{"model":"assistant","messages":[{"role":"user","content":"` + strings.Repeat("x", 32<<20) + `"}]}`)
	out = append(out, e.extReject("security/oversized-json", "POST", "/v1/chat/completions", oversized, nil, 413, "invalid_request"))
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", "proof.wav")
	if err == nil {
		_, err = part.Write(bytes.Repeat([]byte{0x52, 0x49, 0x46, 0x46}, 80<<10))
	}
	if err == nil {
		err = writer.WriteField("model", "assistant")
	}
	if err == nil {
		err = writer.WriteField("forbidden_semantic", "true")
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	var uploadKey struct {
		Token string `json:"token"`
	}
	if err == nil {
		keyBody, marshalErr := json.Marshal(map[string]any{"data": map[string]any{
			"name": "multipart semantic boundary", "role": "operator",
			"permissions": []string{"inference:invoke", "connection:fixture-connection"},
			"aliases":     []string{"assistant"}, "connections": []string{"fixture-connection"},
			"operations": []string{"audio.transcribe"}, "portable": true, "native_account": true, "realtime": false,
		}})
		err = marshalErr
		if err == nil {
			issued, issueErr := e.extRequest(context.Background(), true, "POST", "/admin/api/v1/api_keys/verify-multipart/issue", keyBody, nil)
			err = issueErr
			if err == nil {
				err = json.Unmarshal([]byte(issued.Body), &uploadKey)
				if err == nil && (issued.Status < 200 || issued.Status >= 300 || uploadKey.Token == "") {
					err = fmt.Errorf("multipart key issue returned HTTP %d without usable token", issued.Status)
				}
			}
		}
	}
	if err != nil {
		out = append(out, extResult("security/late-multipart-semantics", time.Now(), nil, err))
	} else {
		out = append(out, e.extReject("security/late-multipart-semantics", "POST", "/v1/audio/transcriptions", upload.Bytes(), func(r *http.Request) {
			r.Header.Set("Content-Type", writer.FormDataContentType())
			r.Header.Set("Authorization", "Bearer "+uploadKey.Token)
		}, 400, "unsupported_feature"))
	}
	return out
}

func (e *environment) extAdmin() []result {
	out := []result{}
	for _, s := range []struct {
		name, path string
		headers    func(*http.Request)
		status     int
	}{
		{"csrf-missing", "/admin/api/v1/connections", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403},
		{"csrf-invalid", "/admin/api/v1/connections", func(r *http.Request) { r.Header.Set("X-CSRF-Token", "invalid") }, 403},
		{"cross-origin", "/admin/api/v1/connections", func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.invalid") }, 403},
		{"inference-key-rbac", "/admin/api/v1/connections", func(r *http.Request) {
			r.Header.Del("Cookie")
			r.Header.Del("X-CSRF-Token")
			r.Header.Set("Authorization", "Bearer "+e.key)
		}, 401},
	} {
		start := time.Now()
		obs, err := e.extRequest(context.Background(), true, "POST", s.path, []byte(`{"id":"must-not-exist","data":{"connector":"anthropic","account_id":"denied","base_url":"http://127.0.0.1:1","dedicated":true,"enabled":true}}`), s.headers)
		if err == nil && obs.Status != s.status {
			err = fmt.Errorf("expected authorization boundary HTTP %d, got %d", s.status, obs.Status)
		}
		if err == nil {
			read, readErr := e.extRequest(context.Background(), true, "GET", "/admin/api/v1/connections/must-not-exist", nil, nil)
			if readErr != nil {
				err = readErr
			} else if read.Status != 404 {
				err = fmt.Errorf("rejected mutation created visible resource, HTTP %d", read.Status)
			}
		}
		out = append(out, extResult("credentials/"+s.name, start, obs, err))
	}
	for _, resource := range []struct{ name, method, path string }{
		{"credentials", "POST", "/admin/api/v1/connections/fixture-connection/status"},
		{"api_keys", "GET", "/admin/api/v1/api_keys"},
	} {
		start := time.Now()
		var body []byte
		if resource.method == "POST" {
			body = []byte(`{"data":{"provider":"anthropic","account_id":"fixture-account"}}`)
		}
		obs, err := e.extRequest(context.Background(), true, resource.method, resource.path, body, nil)
		if err == nil && obs.Status != 200 {
			err = fmt.Errorf("secret-safe listing returned HTTP %d", obs.Status)
		}
		if err == nil && (strings.Contains(obs.Body, "[REDACTED]") || strings.Contains(obs.Body, `"secret":`) || strings.Contains(obs.Body, `"refresh_token":`) || strings.Contains(obs.Body, `"verifier":`)) {
			err = fmt.Errorf("read API exposed credential material")
		}
		out = append(out, extResult("credentials/redacted-"+resource.name, start, obs, err))
	}
	start := time.Now()
	obs, err := e.extRequest(context.Background(), true, "PUT", "/admin/api/v1/model_aliases/assistant", []byte(`{"data":{"model_ids":["fixture-model"],"enabled":true}}`), nil)
	if err == nil && obs.Status != 428 {
		err = fmt.Errorf("configuration edit without If-Match must return 428, got %d", obs.Status)
	}
	out = append(out, extResult("console-api/config-requires-revision", start, obs, err))
	return out
}

func (e *environment) extSDK() []result {
	start := time.Now()
	python := os.Getenv("HOORIFIC_VERIFY_PYTHON")
	if python == "" {
		python = "python3"
	}
	asset := os.Getenv("HOORIFIC_VERIFY_SDK_RUNNER")
	if asset == "" {
		asset = filepath.Join("tools", "verify", "sdk", "run.py")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, asset)
	cmd.Env = append(os.Environ(), "HOORIFIC_VERIFY_INFERENCE=http://"+e.inference, "HOORIFIC_VERIFY_MANAGEMENT=http://"+e.management, "HOORIFIC_VERIFY_KEY="+e.key, "HOORIFIC_VERIFY_COOKIE="+e.cookie, "HOORIFIC_VERIFY_CSRF="+e.csrf, "HOORIFIC_VERIFY_EVIDENCE="+e.root)
	var stdout, stderr extBoundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	evidence := map[string]any{"stdout": e.extRedact(stdout.String()), "stderr": e.extRedact(stderr.String()), "runner": asset}
	if stdout.overflow || stderr.overflow {
		return []result{extResult("sdk/runner", start, evidence, fmt.Errorf("SDK output exceeded bounded evidence capture"))}
	}
	var results []result
	parseErr := json.Unmarshal(stdout.Bytes(), &results)
	if parseErr != nil || len(results) == 0 {
		if err == nil {
			err = fmt.Errorf("SDK runner must return a nonempty JSON result array: %v", parseErr)
		}
		return []result{extResult("sdk/runner", start, evidence, err)}
	}
	for i := range results {
		if results[i].Status != "passed" && results[i].Status != "failed" && results[i].Status != "not-run" {
			results[i].Status = "failed"
			results[i].Detail = "SDK runner emitted invalid scenario status"
		}
		// Scrub the entire returned evidence, not only console diagnostics.
		raw, marshalErr := json.Marshal(results[i])
		if marshalErr == nil {
			_ = json.Unmarshal([]byte(e.extRedact(string(raw))), &results[i])
		}
	}
	if err != nil {
		results = append(results, extResult("sdk/process-exit", start, evidence, err))
	}
	return results
}

type extBoundedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *extBoundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (8 << 20) - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
