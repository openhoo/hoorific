package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAggregateCodexSSEUsesNestedTerminalUsage(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":9,\"total_tokens\":21,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n"))}
	body, usage, err := aggregateCodexSSE(context.Background(), response)
	if err != nil {
		t.Fatalf("aggregateCodexSSE() error = %v", err)
	}
	if string(body) != `{"id":"resp-1","status":"completed","usage":{"input_tokens":12,"output_tokens":9,"total_tokens":21,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}` {
		t.Fatalf("aggregated terminal response = %s", body)
	}
	if usage == nil || usage.Input == nil || *usage.Input != 12 || usage.Output == nil || *usage.Output != 9 || usage.Total == nil || *usage.Total != 21 || usage.CachedInput == nil || *usage.CachedInput != 3 || usage.ReasoningOutput == nil || *usage.ReasoningOutput != 2 {
		t.Fatalf("nested terminal usage was not observed: %#v", usage)
	}
}

func TestAggregateCodexSSERejectsFailedAndTruncatedTerminals(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "failed",
			body: "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n",
		},
		{
			name: "truncated",
			body: "event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
			want: errCodexStreamTruncated,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}
			body, usage, err := aggregateCodexSSE(context.Background(), response)
			if err == nil {
				t.Fatal("aggregateCodexSSE() unexpectedly succeeded")
			}
			if test.want != nil && err != test.want {
				t.Fatalf("aggregateCodexSSE() error = %v, want %v", err, test.want)
			}
			if body != nil || usage != nil {
				t.Fatalf("failed aggregation returned body/usage: %q %#v", body, usage)
			}
		})
	}
}
func TestAggregateCodexSSEAcceptsIncompleteTerminal(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-1\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\n\n"))}
	body, usage, err := aggregateCodexSSE(context.Background(), response)
	if err != nil {
		t.Fatalf("aggregateCodexSSE() error = %v", err)
	}
	if len(body) == 0 || usage == nil || usage.Input == nil || *usage.Input != 4 || usage.Output == nil || *usage.Output != 2 || usage.Total == nil || *usage.Total != 6 {
		t.Fatalf("incomplete terminal body/usage was not preserved: %q %#v", body, usage)
	}
}

func TestCodexTerminalResponseRejectsMalformedStatus(t *testing.T) {
	object := map[string]json.RawMessage{
		"type":     json.RawMessage(`"response.completed"`),
		"response": json.RawMessage(`{"id":"resp-1","status":"incomplete"}`),
	}
	body, _, usage, err := codexTerminalResponse(object)
	if err == nil {
		t.Fatal("codexTerminalResponse() unexpectedly accepted incomplete status")
	}
	if body != nil || usage != nil {
		t.Fatalf("malformed terminal returned body/usage: %q %#v", body, usage)
	}
}
