package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type capture struct {
	Method, Path       string
	Body               []byte
	Credential         string
	CredentialHeader   string
	CredentialCount    int
	CredentialConflict bool
}
type fixture struct {
	server   *httptest.Server
	mu       sync.Mutex
	calls    []capture
	mode     atomic.Value
	canceled atomic.Int64
}

func newFixture() *fixture {
	f := &fixture{}
	f.mode.Store("normal")
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}
func (f *fixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func (f *fixture) last() capture {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return capture{}
	}
	return f.calls[len(f.calls)-1]
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 34<<20))
	if err != nil {
		http.Error(w, "fixture read", 400)
		return
	}
	credential := r.Header.Get("Authorization")
	credentialHeader := "Authorization"
	if credential == "" {
		credential = r.Header.Get("X-Api-Key")
		credentialHeader = "X-Api-Key"
	}
	credentialHeaders := 0
	for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"} {
		if len(r.Header.Values(header)) != 0 {
			credentialHeaders++
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, capture{Method: r.Method, Path: r.URL.Path, Body: b, Credential: credential, CredentialHeader: credentialHeader, CredentialCount: len(r.Header.Values(credentialHeader)), CredentialConflict: credentialHeaders > 1})
	f.mu.Unlock()
	mode := f.mode.Load().(string)
	if mode == "429" {
		w.Header().Set("Retry-After", "0")
		http.Error(w, `{"error":{"type":"rate_limit_error","message":"fixture rejection"}}`, 429)
		return
	}
	if mode == "disconnect" {
		if h, ok := w.(http.Hijacker); ok {
			c, _, e := h.Hijack()
			if e == nil {
				c.Close()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/models") {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-Api-Key") != "fixture-upstream-secret" || len(r.Header.Values("X-Api-Key")) != 1 || credentialHeaders != 1 || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			http.Error(w, "configured fixture discovery credential and version required", http.StatusUnauthorized)
			return
		}
		query := r.URL.Query()
		if len(query) != 1 || len(query["limit"]) != 1 || query.Get("limit") != "1000" {
			http.Error(w, "fixture discovery requires limit=1000", http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"data":[{"id":"fixture-chat","type":"model","display_name":"Deterministic fixture chat","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"first_id":"fixture-chat","last_id":"fixture-chat"}`)
		return
	}
	if strings.Contains(r.URL.Path, "/files") {
		if r.Method == "DELETE" {
			io.WriteString(w, `{"id":"file-same-id","deleted":true}`)
		} else if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/files") {
			io.WriteString(w, `{"object":"list","data":[{"id":"file-same-id","object":"file","bytes":3,"filename":"proof.txt","purpose":"assistants"}]}`)
		} else {
			io.WriteString(w, `{"id":"file-same-id","object":"file","bytes":3,"filename":"proof.txt","purpose":"assistants"}`)
		}
		return
	}
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(b, &payload)
	stream := string(payload["stream"]) == "true"
	anthropic := strings.HasSuffix(r.URL.Path, "/messages")
	if mode == "unicode" && !stream {
		if anthropic {
			io.WriteString(w, `{"id":"msg_unicode","type":"message","role":"assistant","model":"fixture-chat","content":[{"type":"text","text":"Grüße 🌍"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":2}}`)
		} else {
			io.WriteString(w, `{"id":"chatcmpl_unicode","object":"chat.completion","model":"fixture-chat","choices":[{"message":{"role":"assistant","content":"Grüße 🌍"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
		}
		return
	}
	if mode == "tool" && !stream {
		if anthropic {
			io.WriteString(w, `{"id":"msg_tool","type":"message","role":"assistant","model":"fixture-chat","content":[{"type":"tool_use","id":"tool_fixture","name":"lookup","input":{"city":"Zürich","note":"line\nline\nline\nline\n"}}],"stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":2}}`)
		} else {
			io.WriteString(w, `{"id":"chatcmpl_tool","object":"chat.completion","model":"fixture-chat","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"tool_fixture","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Zürich\",\"note\":\"line\\nline\\nline\\nline\\n\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
		}
		return
	}
	if !stream {
		if anthropic {
			io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture-chat","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":2}}`)
		} else {
			io.WriteString(w, `{"id":"chatcmpl-fixture","object":"chat.completion","model":"fixture-chat","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	frames := make([]string, 0, 16)
	switch mode {
	case "named-error":
		if anthropic {
			frames = []string{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_error\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-chat\",\"content\":[]}}\n\n", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"O\"}}\n\n", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"fixture named error\"}}\n\n"}
		} else {
			frames = []string{"data: {\"id\":\"chatcmpl_error\",\"choices\":[{\"delta\":{\"content\":\"O\"},\"finish_reason\":null}]}\n\n", "data: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"fixture named error\"}}\n\n"}
		}
	case "unicode":
		text := "Grüße 🌍"
		if anthropic {
			frames = append(frames, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_unicode\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-chat\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			for _, r := range text {
				frames = append(frames, fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", string(r)))
			}
			frames = append(frames, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n", "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			for _, r := range text {
				frames = append(frames, fmt.Sprintf("data: {\"id\":\"chatcmpl_unicode\",\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", string(r)))
			}
			frames = append(frames, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n", "data: [DONE]\n\n")
		}
	case "tool":
		note := strings.Repeat("line\n", 4096)
		arg, _ := json.Marshal(map[string]string{"city": "Zürich", "note": note})
		if anthropic {
			frames = []string{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_tool\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-chat\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_fixture\",\"name\":\"lookup\",\"input\":{}}}\n\n", fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%q}}\n\n", string(arg)), "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n", "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
		} else {
			frames = []string{fmt.Sprintf("data: {\"id\":\"chatcmpl_tool\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool_fixture\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", string(arg)), "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n", "data: [DONE]\n\n"}
		}
	default:
		if anthropic {
			frames = []string{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-chat\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"O\"}}\n\n", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"K\"}}\n\n", "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n", "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
		} else {
			frames = []string{"data: {\"id\":\"chatcmpl-fixture\",\"choices\":[{\"delta\":{\"content\":\"O\"},\"finish_reason\":null}]}\n\n", "data: {\"id\":\"chatcmpl-fixture\",\"choices\":[{\"delta\":{\"content\":\"K\"},\"finish_reason\":null}]}\n\n", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n", "data: [DONE]\n\n"}
		}
	}
	for index, frame := range frames {
		if mode == "truncate" && index == len(frames)-2 {
			return
		}
		for _, part := range []byte(frame) {
			if _, err = w.Write([]byte{part}); err != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if mode == "slow" {
			select {
			case <-r.Context().Done():
				f.canceled.Add(1)
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}
