package claudesubscription

import (
	"context"
	"net/http"
	"testing"

	"hoorific/internal/core"
)

func TestClaudeSubscriptionBindsOAuthMessagesHeaders(t *testing.T) {
	connector := New()
	connection := core.Connection{
		TenantID: "tenant-a", ID: "connection-a", AccountID: "account-a", BaseURL: "https://upstream.example",
		Settings: map[string]string{"subscription_enabled": "true", "consent_ack": "true", "subscription_auth": "oauth"},
	}
	for _, operation := range []core.Operation{"generate", "count_tokens"} {
		binding, err := connector.Bind(context.Background(), core.ModelCall{
			Connection: connection,
			Model:      core.Model{ID: "claude-sonnet", Operations: []core.Operation{operation}},
		}, operation)
		if err != nil {
			t.Fatalf("bind %s: %v", operation, err)
		}
		if got := binding.Headers.Get("anthropic-version"); got != APIVersion {
			t.Fatalf("%s anthropic-version = %q, want %q", operation, got, APIVersion)
		}
		if len(binding.AllowedRequestHeaders) != 1 || binding.AllowedRequestHeaders[0] != "anthropic-beta" {
			t.Fatalf("%s allowed headers = %#v, want only anthropic-beta", operation, binding.AllowedRequestHeaders)
		}
		for _, test := range []struct {
			name   string
			header string
			wantOK bool
		}{
			{name: "absent", wantOK: true},
			{name: "future capability", header: "context-management-2025-06-27", wantOK: true},
			{name: "duplicate capability", header: "context-management-2025-06-27,context-management-2025-06-27", wantOK: true},
			{name: "empty token", header: "context-management-2025-06-27,,other", wantOK: false},
			{name: "control character", header: "context-management-2025-06-27\r\nother", wantOK: false},
		} {
			t.Run(test.name, func(t *testing.T) {
				headers := make(http.Header)
				if test.header != "" {
					headers.Set("anthropic-beta", test.header)
				}
				err := binding.ValidateRequestHeaders(headers)
				if (err == nil) != test.wantOK {
					t.Fatalf("validate anthropic-beta %q: err=%v, wantOK=%v", test.header, err, test.wantOK)
				}
			})
		}
	}
}
