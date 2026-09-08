package anthropic

import (
	"context"
	"io"
	"strings"
	"testing"

	"hoorific/internal/core"
)

func TestDecodeResultNormalizesCacheUsageAndToleratesMetadata(t *testing.T) {
	body := `{"id":"msg-1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":2},"output_tokens_details":{"thinking_tokens":1},"provider_future_field":true},"provider_future_field":"ok"}`
	payload, err := New().DecodeResult(context.Background(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	usage := payload.(core.GenerationResult).Usage
	if usage == nil || usage.Input == nil || *usage.Input != 115 || usage.Total == nil || *usage.Total != 119 {
		t.Fatalf("normalized usage = %#v", usage)
	}
	if usage.CacheWrite5mInput == nil || *usage.CacheWrite5mInput != 3 || usage.CacheWrite1hInput == nil || *usage.CacheWrite1hInput != 2 {
		t.Fatalf("cache write details = %#v", usage)
	}
	if usage.ReasoningOutput == nil || *usage.ReasoningOutput != 1 {
		t.Fatalf("reasoning output = %#v", usage.ReasoningOutput)
	}
}

func TestAnthropicStreamMergesCumulativeCacheUsage(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":100}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4,\"cache_creation_input_tokens\":5,\"cache_creation\":{\"ephemeral_5m_input_tokens\":3,\"ephemeral_1h_input_tokens\":2}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
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
	if usage == nil || usage.Input == nil || *usage.Input != 115 || usage.Output == nil || *usage.Output != 4 {
		t.Fatalf("stream usage = %#v", usage)
	}
}
