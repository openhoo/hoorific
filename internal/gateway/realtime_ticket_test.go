package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/realtime"
)

type ticketStoreStub struct {
	ticket realtime.Ticket
	calls  int
}

func (s *ticketStoreStub) PutTicket(_ context.Context, ticket realtime.Ticket) error {
	s.ticket = ticket
	return nil
}

func (s *ticketStoreStub) ConsumeTicketBound(_ context.Context, _ []byte, _ time.Time, _, _ string) (realtime.Ticket, error) {
	s.calls++
	return s.ticket, nil
}

func newTicketStub() (*ticketStoreStub, string) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	hash := sha256.Sum256(raw)
	return &ticketStoreStub{ticket: realtime.Ticket{
		IDHash:       append([]byte(nil), hash[:]...),
		TenantID:     "tenant",
		ConnectionID: "connection",
		ModelID:      "model",
		Principal: core.Principal{
			TenantID:      "tenant",
			KeyID:         "key",
			Realtime:      true,
			NativeAccount: true,
		},
		Grants:    []string{"realtime"},
		ExpiresAt: time.Now().Add(time.Minute),
	}}, base64.RawURLEncoding.EncodeToString(raw)
}

func TestAuthTicketRejectsNonRealtimeRoutesBeforeConsume(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		route   route
		upgrade string
	}{
		{
			name:   "portable request",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			route:  route{protocol: "openai-chat", operation: "generate"},
		},
		{
			name:   "native JSON action",
			method: http.MethodPost,
			path:   "/connect/connection/openai/v1/realtime",
			route:  route{protocol: "openai-chat", native: true, connection: "connection", path: "realtime", model: "model"},
		},
		{
			name:    "unrelated WebSocket path",
			method:  http.MethodGet,
			path:    "/connect/connection/openai/v1/responses",
			route:   route{protocol: "openai-chat", native: true, connection: "connection", path: "responses", model: "model"},
			upgrade: "websocket",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, token := newTicketStub()
			request := httptest.NewRequest(test.method, "http://gateway"+test.path+"?ticket="+token+"&model=model", strings.NewReader("{}"))
			if test.upgrade != "" {
				request.Header.Set("Upgrade", test.upgrade)
			}
			gateway := &Gateway{deps: Dependencies{Tickets: store}}
			if _, err := gateway.authTicket(request, test.route); err == nil {
				t.Fatal("non-realtime route accepted a realtime ticket")
			}
			if store.calls != 0 {
				t.Fatalf("ticket store consumed %d times, want 0", store.calls)
			}
		})
	}
}

func TestAuthTicketAcceptsDocumentedRealtimeWebSocket(t *testing.T) {
	store, token := newTicketStub()
	request := httptest.NewRequest(http.MethodGet, "http://gateway/connect/connection/openai/v1/realtime?ticket="+token+"&model=model", nil)
	request.Header.Set("Upgrade", "websocket")
	gateway := &Gateway{deps: Dependencies{Tickets: store}}
	principal, err := gateway.authTicket(request, route{protocol: "openai-chat", native: true, connection: "connection", path: "realtime", model: "model"})
	if err != nil {
		t.Fatalf("valid realtime ticket rejected: %v", err)
	}
	if principal.TenantID != "tenant" || !principal.Realtime || !principal.NativeAccount {
		t.Fatalf("unexpected ticket principal: %+v", principal)
	}
	if store.calls != 1 {
		t.Fatalf("ticket store consumed %d times, want 1", store.calls)
	}
	if request.URL.Query().Get("ticket") != "" {
		t.Fatal("ticket remained in upstream-visible query")
	}
}

func TestCaptureWithNilSpoolReturnsUnavailable(t *testing.T) {
	for _, contentType := range []string{"application/octet-stream", "multipart/form-data; boundary=missing"} {
		t.Run(contentType, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://gateway", strings.NewReader("body"))
			request.Header.Set("Content-Type", contentType)
			_, _, _, err := (&Gateway{}).capture(context.Background(), httptest.NewRecorder(), request, core.Principal{TenantID: "tenant"}, "request")
			if err == nil {
				t.Fatal("nil spool capture unexpectedly succeeded")
			}
			gatewayErr, ok := err.(core.GatewayError)
			if !ok || gatewayErr.Code != "unavailable" {
				t.Fatalf("capture error = %v, want unavailable gateway error", err)
			}
		})
	}
}
