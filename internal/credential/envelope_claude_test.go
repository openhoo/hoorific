package credential

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClaudeSubscriptionOAuthHeadersAreCapturedAndMerged(t *testing.T) {
	const token = "oauth-access-token"
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "empty caller header", want: claudeSubscriptionOAuthBeta},
		{name: "caller capability", values: []string{"context-management-2025-06-27"}, want: "context-management-2025-06-27," + claudeSubscriptionOAuthBeta},
		{name: "duplicate capabilities", values: []string{"context-management-2025-06-27,context-management-2025-06-27", claudeSubscriptionOAuthBeta}, want: "context-management-2025-06-27," + claudeSubscriptionOAuthBeta},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			captured := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range test.values {
				req.Header.Add("anthropic-beta", value)
			}
			lease := &lease{
				record: Record{Identity: Identity{Provider: claudeSubscriptionProvider}},
				secret: Secret{OAuth: &OAuthToken{AccessToken: token, TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}},
			}
			if err := lease.Authorize(context.Background(), req); err != nil {
				t.Fatalf("authorize: %v", err)
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			got := <-captured
			if got.Get("Authorization") != "Bearer "+token {
				t.Fatalf("captured Authorization = %q", got.Get("Authorization"))
			}
			if got.Get("anthropic-beta") != test.want {
				t.Fatalf("captured anthropic-beta = %q, want %q", got.Get("anthropic-beta"), test.want)
			}
		})
	}
}

func TestClaudeSubscriptionOAuthRejectsMalformedBetaBeforeDispatch(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
	req.Header.Set("anthropic-beta", "context-management-2025-06-27,\r\ninvalid")
	lease := &lease{
		record: Record{Identity: Identity{Provider: claudeSubscriptionProvider}},
		secret: Secret{OAuth: &OAuthToken{AccessToken: "oauth-access-token", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}},
	}
	if err := lease.Authorize(context.Background(), req); err == nil {
		t.Fatal("malformed anthropic-beta was accepted")
	}
}
