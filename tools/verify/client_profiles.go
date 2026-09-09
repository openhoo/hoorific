package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// clientProfileScenario exercises persisted configuration through the real
// gateway, discovery, and connection-test paths with isolated credentials.
func (e *environment) clientProfileScenario() result {
	start := time.Now()
	evidence := []map[string]any{}
	cases := []struct {
		name, provider string
		profile        map[string]any
		want           map[string]string
	}{
		{"native", "openai", nil, map[string]string{"Originator": "", "X-Zcode-App-Version": ""}},
		{"custom", "openai", map[string]any{"preset": "custom", "headers": map[string]string{"User-Agent": "fixture-client/1", "X-Stainless-OS": "Linux"}}, map[string]string{"User-Agent": "fixture-client/1", "X-Stainless-OS": "Linux"}},
		{"cli", "openai", map[string]any{"preset": "codex-cli", "version": "1.2.3"}, map[string]string{"Originator": "codex_cli_rs", "User-Agent": "codex_cli_rs/1.2.3"}},
		{"desktop", "openai", map[string]any{"preset": "codex-desktop", "headers": map[string]string{"originator": "fixture-desktop", "USER-AGENT": "fixture-desktop/2"}}, map[string]string{"Originator": "fixture-desktop", "User-Agent": "fixture-desktop/2"}},
		{"zcode", "openai", map[string]any{"preset": "zcode-desktop", "version": "3.1.8", "headers": map[string]string{"X-Title": "Fixture title"}}, map[string]string{"User-Agent": "ZCode/3.1.8", "X-Zcode-App-Version": "3.1.8", "HTTP-Referer": "https://zcode.z.ai", "X-Title": "Fixture title"}},
		{"zcode-anthropic", "anthropic", map[string]any{"preset": "zcode-desktop", "version": "3.1.8"}, map[string]string{"User-Agent": "ZCode/3.1.8", "HTTP-Referer": "https://zcode.z.ai"}},
	}
	for _, tc := range cases {
		err := func() error {
			f, err := e.operationFixture("profile-"+tc.name, tc.provider, []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
				for name, want := range tc.want {
					if got := r.Header.Get(name); got != want {
						return fmt.Errorf("profile %s: %s = %q, want %q", tc.name, name, got, want)
					}
				}
				if r.Method == http.MethodGet {
					return operationJSON(w, map[string]any{"data": []any{map[string]any{"id": "fixture-model", "object": "model"}}, "has_more": false})
				}
				if tc.provider == "anthropic" {
					return operationJSON(w, map[string]any{"id": "profile-response", "type": "message", "role": "assistant", "model": "fixture-model", "content": []any{map[string]any{"type": "text", "text": "PROFILE_OK"}}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
				}
				return operationJSON(w, map[string]any{"id": "profile-response", "object": "chat.completion", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "PROFILE_OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
			})
			if err != nil {
				return err
			}
			defer f.server.Close()
			if err := e.costConfigureModel(f, costPrice(), false); err != nil {
				return err
			}
			path := "/admin/api/v1/connections/" + f.id
			current, err := e.extRequest(context.Background(), true, http.MethodGet, path, nil, nil)
			if err != nil {
				return err
			}
			var resource struct {
				Version int64          `json:"version"`
				Data    map[string]any `json:"data"`
			}
			if current.Status != 200 || json.Unmarshal([]byte(current.Body), &resource) != nil || resource.Data == nil {
				return fmt.Errorf("profile connection read HTTP %d", current.Status)
			}
			if tc.profile != nil {
				resource.Data["client_profile"] = tc.profile
			}
			body, _ := json.Marshal(map[string]any{"data": resource.Data})
			updated, err := e.extRequest(context.Background(), true, http.MethodPut, path, body, func(r *http.Request) { r.Header.Set("If-Match", fmt.Sprint(resource.Version)) })
			if err != nil {
				return err
			}
			if updated.Status != 200 {
				return fmt.Errorf("profile connection update HTTP %d/%s", updated.Status, updated.ErrorCode)
			}
			for _, portable := range []bool{false, true} {
				requestPath, model := "chat/completions", "fixture-model"
				if tc.provider == "anthropic" {
					requestPath = "messages"
				}
				if portable {
					requestPath, model = "/v1/chat/completions", f.alias
				}
				payload, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "max_tokens": 8})
				reply, err := f.requestHeaders(http.MethodPost, requestPath, "application/json", payload, map[string]string{"Originator": "untrusted-caller", "User-Agent": "untrusted-caller", "X-Title": "untrusted-caller"})
				if err != nil {
					return err
				}
				if err := costAssertResponse(reply, 200, "PROFILE_OK"); err != nil {
					return err
				}
			}
			for _, action := range []string{"discover", "test"} {
				response, err := e.extRequest(context.Background(), true, http.MethodPost, path+"/"+action, []byte(`{}`), nil)
				if err != nil {
					return err
				}
				if response.Status != 200 {
					return fmt.Errorf("profile %s HTTP %d/%s", action, response.Status, response.ErrorCode)
				}
			}
			// A protected override must fail before it can replace credentials.
			if err := json.Unmarshal([]byte(updated.Body), &resource); err != nil {
				return err
			}
			resource.Data["client_profile"] = map[string]any{"preset": "custom", "headers": map[string]string{"Authorization": "forbidden"}}
			body, _ = json.Marshal(map[string]any{"data": resource.Data})
			rejected, err := e.extRequest(context.Background(), true, http.MethodPut, path, body, func(r *http.Request) { r.Header.Set("If-Match", fmt.Sprint(resource.Version)) })
			if err != nil {
				return err
			}
			if rejected.Status != 400 && rejected.Status != 422 {
				return fmt.Errorf("protected profile header accepted: HTTP %d", rejected.Status)
			}
			f.mu.Lock()
			faults := append([]string(nil), f.faults...)
			f.mu.Unlock()
			if len(faults) != 0 {
				return fmt.Errorf("profile wire faults: %v", faults)
			}
			calls := f.count()
			if calls != 4 {
				return fmt.Errorf("profile upstream calls = %d, want 4", calls)
			}
			evidence = append(evidence, map[string]any{"profile": tc.name, "upstream_calls": calls, "portable": true, "native": true, "discovery": true, "connection_test": true, "protected_header_rejected": rejected.Status})
			return nil
		}()
		if err != nil {
			return extResult("admin/client-profiles", start, evidence, err)
		}
	}
	return extResult("admin/client-profiles", start, evidence, nil)
}
