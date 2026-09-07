package store

import (
	"encoding/json"
	"testing"

	"hoorific/internal/core"
)

// Grants must use exact operations carried by connector descriptors, rather
// than broad action-prefix aliases. Both issuance and later metadata edits use
// the same operation vocabulary.
func TestKeyOperationGrantsMatchShippedDescriptors(t *testing.T) {
	cases := []struct {
		name       string
		operations []core.Operation
		allowed    bool
	}{
		{"portable generation", []core.Operation{"generate", "embed", "rerank"}, true},
		{"Replicate lifecycle", []core.Operation{"prediction.create", "prediction.get", "prediction.cancel"}, true},
		{"Replicate catalog", []core.Operation{"prediction.list", "model.get", "model.list", "model.create", "model.version.get", "model.version.list", "deployment.get", "deployment.list", "deployment.create"}, true},
		{"OpenAI native resources", []core.Operation{"chat.resource", "moderation", "audio.voice", "audio.voice_consent", "video.extension", "video.character"}, true},
		{"Ollama model resources", []core.Operation{"model.resource"}, true},
		{"native media", []core.Operation{"native", "video", "image.generate", "audio.speech"}, true},
		{"unknown operation", []core.Operation{"not-a-shipped-operation"}, false},
		{"unknown prediction suffix", []core.Operation{"prediction.arbitrary"}, false},
		{"action is not an operation", []core.Operation{"predictions.create"}, false},
		{"wildcard", []core.Operation{"*"}, false},
		{"duplicate", []core.Operation{"prediction.create", "prediction.create"}, false},
		{"empty element", []core.Operation{""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(keyData{Role: "operator", Permissions: []string{"inference:invoke"}, Operations: tc.operations})
			if err != nil {
				t.Fatal(err)
			}
			_, issueErr := decodeKeyData(body)
			_, updateErr := validateResource("api_keys", body)
			if (issueErr == nil) != tc.allowed {
				t.Fatalf("issuance allowed=%v want=%v: %v", issueErr == nil, tc.allowed, issueErr)
			}
			if (updateErr == nil) != tc.allowed {
				t.Fatalf("metadata update allowed=%v want=%v: %v", updateErr == nil, tc.allowed, updateErr)
			}
		})
	}
}
