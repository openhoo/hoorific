package ollama

import (
	"context"
	"testing"

	"hoorific/internal/core"
)

func TestNativeStreamingRoutesExposeNDJSONFraming(t *testing.T) {
	connector := New()
	connection := core.Connection{BaseURL: DefaultBaseURL, Settings: map[string]string{"wire_protocol": "ollama"}}
	model := core.Model{ID: "llama3", Operations: []core.Operation{"generate"}}
	for _, action := range []string{"chat", "generate"} {
		var endpoint core.NativeEndpoint
		for _, candidate := range connector.Endpoints() {
			if candidate.Action == action {
				endpoint = candidate
				break
			}
		}
		if endpoint.Action == "" {
			t.Fatalf("Ollama %s endpoint is missing", action)
		}
		binding, err := connector.BindEndpoint(context.Background(), connection, endpoint, map[string]string{"model": model.ID})
		if err != nil {
			t.Fatalf("bind Ollama %s: %v", action, err)
		}
		if binding.Framing != "ndjson" {
			t.Fatalf("Ollama %s framing = %q, want ndjson", action, binding.Framing)
		}
	}
}
