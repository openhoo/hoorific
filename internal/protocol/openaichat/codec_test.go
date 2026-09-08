package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"hoorific/internal/core"
	"hoorific/internal/protocol/openairesponses"
)

func TestDecodeResultPreservesDetailedUsage(t *testing.T) {
	body := `{"id":"chat-1","model":"gpt-5.6","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":4},"completion_tokens_details":{"reasoning_tokens":2}}}`
	payload, err := New().DecodeResult(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	result := payload.(core.GenerationResult)
	if got := *result.Usage.CachedInput; got != 3 {
		t.Fatalf("cached input = %d, want 3", got)
	}
	if got := *result.Usage.CacheWriteInput; got != 4 {
		t.Fatalf("cache write input = %d, want 4", got)
	}
	if got := *result.Usage.ReasoningOutput; got != 2 {
		t.Fatalf("reasoning output = %d, want 2", got)
	}
	if got := *result.Usage.Input; got != 12 {
		t.Fatalf("input = %d, want aggregate 12", got)
	}
	if got := *result.Usage.Output; got != 9 {
		t.Fatalf("output = %d, want aggregate 9", got)
	}
}

func TestEncodeUsageAggregatesCacheWriteSubcategories(t *testing.T) {
	result := core.GenerationResult{
		ID:     "chat-1",
		Model:  "gpt-5.6",
		Blocks: []core.ContentBlock{{Kind: "text", Text: "done"}},
		Finish: core.Finish{Status: "completed", Reason: "stop"},
		Usage: &core.Usage{
			Input:             ptrInt64(12),
			Output:            ptrInt64(9),
			Total:             ptrInt64(21),
			CachedInput:       ptrInt64(3),
			CacheWrite1hInput: ptrInt64(4),
			ReasoningOutput:   ptrInt64(2),
			ToolInput:         ptrInt64(7),
		},
	}
	var out bytes.Buffer
	if err := New().EncodeResult(context.Background(), result, &out); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens     *int64 `json:"cached_tokens"`
				CacheWriteTokens *int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if got := *wire.Usage.PromptTokensDetails.CachedTokens; got != 3 {
		t.Fatalf("cached tokens = %d, want 3", got)
	}
	if got := *wire.Usage.PromptTokensDetails.CacheWriteTokens; got != 4 {
		t.Fatalf("cache write tokens = %d, want 4", got)
	}
	if got := *wire.Usage.CompletionTokensDetails.ReasoningTokens; got != 2 {
		t.Fatalf("reasoning tokens = %d, want 2", got)
	}
	if bytes.Contains(out.Bytes(), []byte("tool_input")) || bytes.Contains(out.Bytes(), []byte("cache_write_1h")) {
		t.Fatalf("unsupported detail leaked onto Chat wire: %s", out.Bytes())
	}
}

func TestChatCacheDirectivesRoundTrip(t *testing.T) {
	body := `{"model":"gpt-5.6","messages":[{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}],"prompt_cache_key":"tenant/session","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"explicit","ttl":"1h"}}`
	payload, err := New().DecodeRequest(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	conversation := payload.(core.Conversation)
	if conversation.Cache == nil || conversation.Cache.Key != "tenant/session" || conversation.Cache.Retention != "24h" || conversation.Cache.Mode != "explicit" || conversation.Cache.TTL != "1h" {
		t.Fatalf("cache metadata not retained: %#v", conversation.Cache)
	}
	if !conversation.Messages[0].Content[0].CacheBreakpoint {
		t.Fatal("prompt cache breakpoint was not retained")
	}
	var encoded bytes.Buffer
	if err := New().EncodeRequest(context.Background(), conversation, &encoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded.Bytes(), []byte(`"prompt_cache_breakpoint":{"mode":"explicit"}`)) {
		t.Fatalf("breakpoint missing from encoded request: %s", encoded.Bytes())
	}
}

func TestChatPreservesResponsesPromptCacheDirectives(t *testing.T) {
	body := `{"model":"gpt-5.6","store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}],"prompt_cache_key":"tenant/session","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"explicit","ttl":"1h"}}`
	payload, err := openairesponses.New().DecodeRequest(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	conversation := payload.(core.Conversation)
	var encoded bytes.Buffer
	if err := New().EncodeRequest(context.Background(), conversation, &encoded); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		PromptCacheKey       string `json:"prompt_cache_key"`
		PromptCacheRetention string `json:"prompt_cache_retention"`
		PromptCacheOptions   *struct {
			Mode string `json:"mode"`
			TTL  string `json:"ttl"`
		} `json:"prompt_cache_options"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.PromptCacheKey != "tenant/session" || wire.PromptCacheRetention != "24h" {
		t.Fatalf("Responses cache identity was not preserved: %s", encoded.Bytes())
	}
	if wire.PromptCacheOptions == nil || wire.PromptCacheOptions.Mode != "explicit" || wire.PromptCacheOptions.TTL != "1h" {
		t.Fatalf("Responses cache options were not preserved: %s", encoded.Bytes())
	}
	if !bytes.Contains(encoded.Bytes(), []byte(`"prompt_cache_breakpoint":{"mode":"explicit"}`)) {
		t.Fatalf("Responses text breakpoint was not preserved: %s", encoded.Bytes())
	}
}

func TestOpenRouterCacheControlsRoundTrip(t *testing.T) {
	body := `{"model":"openrouter/model","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{}},"cache_control":{"type":"ephemeral","ttl":"1h"}}],"session_id":"session-1"}`
	payload, err := New().DecodeRequest(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	conversation := payload.(core.Conversation)
	if conversation.Cache == nil || conversation.Cache.Protocol != "openrouter" || conversation.Cache.SessionID != "session-1" {
		t.Fatalf("OpenRouter cache identity not retained: %#v", conversation.Cache)
	}
	if conversation.Messages[0].Content[0].CacheControl == nil || conversation.Tools[0].CacheControl == nil {
		t.Fatal("OpenRouter cache controls not retained")
	}
	var encoded bytes.Buffer
	if err := New().EncodeRequest(context.Background(), conversation, &encoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded.Bytes(), []byte(`"session_id":"session-1"`)) || !bytes.Contains(encoded.Bytes(), []byte(`"cache_control":{"type":"ephemeral","ttl":"5m"}`)) {
		t.Fatalf("OpenRouter directives missing from encoded request: %s", encoded.Bytes())
	}
}

func TestChatRejectsCrossProtocolCacheControl(t *testing.T) {
	conversation := core.Conversation{
		Model:    "gpt-5.6",
		Messages: []core.Message{{Role: "user", Content: []core.ContentBlock{{Kind: "text", Text: "hello", CacheControl: &core.CacheControl{Type: "ephemeral"}}}}},
		Cache:    &core.PromptCache{Protocol: "openai-chat"},
	}
	if err := New().EncodeRequest(context.Background(), conversation, io.Discard); err == nil {
		t.Fatal("expected OpenRouter cache control to be rejected for OpenAI Chat")
	}
}

func TestDecodeStreamPreservesDetailedUsage(t *testing.T) {
	stream := "data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":9,\"total_tokens\":21,\"prompt_tokens_details\":{\"cached_tokens\":3,\"cache_write_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	decoder, err := New().NewDecoder(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var usage *core.Usage
	for {
		event, nextErr := decoder.Next(context.Background())
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if value, ok := event.(core.Usage); ok {
			usage = &value
		}
	}
	if usage == nil || usage.CacheWriteInput == nil || *usage.CacheWriteInput != 4 || usage.ReasoningOutput == nil || *usage.ReasoningOutput != 2 {
		t.Fatalf("stream usage details not retained: %#v", usage)
	}
}

func ptrInt64(v int64) *int64 { return &v }
