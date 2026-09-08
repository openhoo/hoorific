package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"hoorific/internal/core"
)

func TestAnthropicPortableWireAdaptationPreservesCacheContent(t *testing.T) {
	body := []byte(`{"model":"claude-test","max_tokens":32,"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	adapted, err := (&Connector{}).AdaptRequest(context.Background(), nil, core.Binding{Codec: core.CodecKey{Protocol: "anthropic-messages"}}, body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model            json.RawMessage `json:"model"`
		AnthropicVersion string          `json:"anthropic_version"`
		CacheControl     struct {
			Type string `json:"type"`
			TTL  string `json:"ttl"`
		} `json:"cache_control"`
		Messages []struct {
			Content []struct {
				Type         string `json:"type"`
				Text         string `json:"text"`
				CacheControl struct {
					Type string `json:"type"`
					TTL  string `json:"ttl"`
				} `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(adapted, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Model) != 0 {
		t.Fatalf("Vertex body retained model: %s", got.Model)
	}
	if got.AnthropicVersion != vertexAnthropicVersion {
		t.Fatalf("anthropic_version = %q, want %q", got.AnthropicVersion, vertexAnthropicVersion)
	}
	if got.CacheControl.Type != "ephemeral" || got.CacheControl.TTL != "1h" {
		t.Fatalf("top-level cache control changed: %#v", got.CacheControl)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 {
		t.Fatalf("message content changed: %#v", got.Messages)
	}
	block := got.Messages[0].Content[0]
	if block.Type != "text" || block.Text != "hello" || block.CacheControl.Type != "ephemeral" || block.CacheControl.TTL != "5m" {
		t.Fatalf("cached content changed: %#v", block)
	}
}

func TestNativeWireIsUntouched(t *testing.T) {
	body := []byte(`{"model":"native-model","payload":{"value":1}}`)
	adapted, err := (&Connector{}).AdaptRequest(context.Background(), nil, core.Binding{Codec: core.CodecKey{Protocol: "native"}}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(adapted, body) {
		t.Fatalf("native body changed: got %s, want %s", adapted, body)
	}
}
