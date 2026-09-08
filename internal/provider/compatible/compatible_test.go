package compatible

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"hoorific/internal/core"
)

func TestOpenRouterValidatesSessionAndAttributionHeaders(t *testing.T) {
	connector := NewOpenRouter()
	var endpoint core.NativeEndpoint
	for _, candidate := range connector.Endpoints() {
		if candidate.Action == "chat.completions" {
			endpoint = candidate
			break
		}
	}
	if endpoint.Action == "" {
		t.Fatal("OpenRouter chat endpoint is missing")
	}
	binding, err := connector.BindEndpoint(context.Background(), core.Connection{}, endpoint, map[string]string{"model": "openai/gpt-5"})
	if err != nil {
		t.Fatalf("bind OpenRouter chat: %v", err)
	}
	if err := binding.ValidateRequestHeaders(http.Header{"X-Session-Id": []string{"session-123"}, "Http-Referer": []string{"https://example.test"}, "X-Title": []string{"Example"}}); err != nil {
		t.Fatalf("valid OpenRouter controls rejected: %v", err)
	}
	if err := binding.ValidateRequestHeaders(http.Header{"X-Session-Id": []string{strings.Repeat("x", 257)}}); err == nil {
		t.Fatal("oversized x-session-id was accepted")
	}
}

func TestOpenRouterAcceptsValidatedCacheControls(t *testing.T) {
	connector := NewOpenRouter()
	body := []byte(`{"model":"openai/gpt-5","session_id":"sticky-session","prompt_cache_key":"cache-key","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"cache_control":{"type":"ephemeral","ttl":"5m"},"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{}},"cache_control":{"type":"ephemeral"}}]}`)
	target := core.ModelCall{Model: core.Model{ID: "openai/gpt-5", Operations: []core.Operation{"generate"}, Features: map[string]core.Support{"custom_tools": core.Supported}}}
	if err := connector.Inspect(context.Background(), target, "generate", body); err != nil {
		t.Fatalf("documented OpenRouter cache controls rejected: %v", err)
	}
	invalid := []byte(`{"model":"openai/gpt-5","session_id":"` + strings.Repeat("x", 257) + `","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	if err := connector.Inspect(context.Background(), target, "generate", invalid); err == nil {
		t.Fatal("oversized OpenRouter session_id was accepted")
	}
	invalid = []byte(`{"model":"openai/gpt-5","cache_control":{"type":"ephemeral","unsupported":true},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	if err := connector.Inspect(context.Background(), target, "generate", invalid); err == nil {
		t.Fatal("unsupported OpenRouter cache-control field was accepted")
	}
}

func TestOpenAIShapedCompatibleRejectsForeignCacheControls(t *testing.T) {
	for _, connector := range []*Connector{NewGroq(), NewCerebras()} {
		target := core.ModelCall{Model: core.Model{ID: "llama", Operations: []core.Operation{"generate"}, Features: map[string]core.Support{"custom_tools": core.Supported}}}
		for _, body := range [][]byte{
			[]byte(`{"model":"llama","session_id":"sticky","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
			[]byte(`{"model":"llama","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`),
			[]byte(`{"model":"llama","tools":[{"type":"function","function":{"name":"lookup","parameters":{}},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`),
		} {
			if err := connector.Inspect(context.Background(), target, "generate", body); err == nil {
				t.Fatalf("foreign cache directive was accepted: %s", body)
			}
		}
	}
}

func TestOpenAIShapedCompatiblePreservesStandardPromptCacheFields(t *testing.T) {
	for _, connector := range []*Connector{NewGroq(), NewCerebras()} {
		target := core.ModelCall{Model: core.Model{ID: "llama", Operations: []core.Operation{"generate"}}}
		body := []byte(`{"model":"llama","prompt_cache_key":"sticky","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"messages":[{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`)
		if err := connector.Inspect(context.Background(), target, "generate", body); err != nil {
			t.Fatalf("standard OpenAI prompt-cache fields were rejected: %v", err)
		}
	}
}

func TestDeepSeekAnthropicUsesAnthropicCacheContract(t *testing.T) {
	connector := NewDeepSeekAnthropic()
	target := core.ModelCall{Model: core.Model{ID: "deepseek-chat", Operations: []core.Operation{"generate"}}}
	body := []byte(`{"model":"deepseek-chat","cache_control":{"type":"ephemeral","ttl":"5m"},"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	if err := connector.Inspect(context.Background(), target, "generate", body); err != nil {
		t.Fatalf("DeepSeek Anthropic cache controls were incorrectly rejected: %v", err)
	}
	routing := []byte(`{"model":"deepseek-chat","session_id":"provider-session","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	if err := connector.Inspect(context.Background(), target, "generate", routing); err == nil {
		t.Fatal("OpenRouter session routing was accepted on DeepSeek Anthropic")
	}
}
