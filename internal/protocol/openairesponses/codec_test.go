package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hoorific/internal/core"
)

func TestDecodeResultPreservesResponsesUsageDetails(t *testing.T) {
	body := `{"id":"resp-1","object":"response","model":"gpt-5.6","status":"completed","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}],"usage":{"input_tokens":12,"output_tokens":9,"total_tokens":21,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`
	payload, err := New().DecodeResult(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	usage := payload.(core.GenerationResult).Usage
	if got := *usage.CachedInput; got != 3 {
		t.Fatalf("cached input = %d, want 3", got)
	}
	if got := *usage.ReasoningOutput; got != 2 {
		t.Fatalf("reasoning output = %d, want 2", got)
	}
	if got := *usage.Input; got != 12 {
		t.Fatalf("input = %d, want 12", got)
	}
}

func TestResponsesPromptCacheBreakpointRoundTrip(t *testing.T) {
	body := `{"model":"gpt-5.6","store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}],"prompt_cache_key":"tenant/session","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"explicit","ttl":"1h"}}`
	payload, err := New().DecodeRequest(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	conversation := payload.(core.Conversation)
	if conversation.Cache == nil || conversation.Cache.Key != "tenant/session" || conversation.Cache.Mode != "explicit" || conversation.Cache.TTL != "1h" {
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
	var wire map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire["prompt_cache_key"] != "tenant/session" {
		t.Fatalf("prompt cache key missing: %s", encoded.Bytes())
	}
}

func TestResponsesRejectsMalformedPromptCacheDirectives(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "key number",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_key":42}`,
		},
		{
			name: "key null",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_key":null}`,
		},
		{
			name: "key empty",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_key":""}`,
		},
		{
			name: "key invalid",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_key":"bad\nkey"}`,
		},
		{
			name: "retention number",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_retention":42}`,
		},
		{
			name: "retention null",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_retention":null}`,
		},
		{
			name: "retention empty",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_retention":""}`,
		},
		{
			name: "retention invalid",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_retention":"7d"}`,
		},
		{
			name: "mode number",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"mode":42}}`,
		},
		{
			name: "mode null",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"mode":null}}`,
		},
		{
			name: "mode empty",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"mode":""}}`,
		},
		{
			name: "mode invalid",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"mode":"unsupported"}}`,
		},
		{
			name: "ttl number",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"ttl":42}}`,
		},
		{
			name: "ttl null",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"ttl":null}}`,
		},
		{
			name: "ttl empty",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"ttl":""}}`,
		},
		{
			name: "ttl invalid",
			body: `{"model":"gpt-5.6","store":false,"input":"hello","prompt_cache_options":{"ttl":"7d"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().DecodeRequest(context.Background(), strings.NewReader(tt.body))
			if err == nil {
				t.Fatal("malformed prompt cache directive was accepted")
			}
			gatewayErr, ok := err.(core.GatewayError)
			if !ok {
				t.Fatalf("error type = %T, want core.GatewayError", err)
			}
			if gatewayErr.HTTPStatus != 400 || gatewayErr.Code != "unsupported_feature" {
				t.Fatalf("error = %#v, want unsupported_feature HTTP 400", gatewayErr)
			}
		})
	}
}

func TestResponsesRejectsForeignCacheControl(t *testing.T) {
	conversation := core.Conversation{
		Model:    "gpt-5.6",
		Messages: []core.Message{{Role: "user", Content: []core.ContentBlock{{Kind: "text", Text: "hello", CacheControl: &core.CacheControl{Type: "ephemeral"}}}}},
		Cache:    &core.PromptCache{Protocol: "openai-responses"},
	}
	var out bytes.Buffer
	if err := New().EncodeRequest(context.Background(), conversation, &out); err == nil {
		t.Fatal("expected unsupported cache control to be rejected")
	}
}
