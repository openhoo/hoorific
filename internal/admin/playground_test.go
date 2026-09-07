package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hoorific/internal/core"
)

type playgroundSessionStore struct {
	AuthStore
	session Session
}

func (s playgroundSessionStore) ResolveSession(_ context.Context, hash string) (Session, error) {
	if hash != s.session.Hash {
		return Session{}, errUnauthenticated
	}
	return s.session, nil
}

func TestPlaygroundValidatesManagementOriginBeforeInternalDispatch(t *testing.T) {
	secret, err := randomSecret()
	if err != nil {
		t.Fatal(err)
	}
	session := Session{Hash: authHash(secret), CSRFHash: authHash(csrfToken(secret)), Principal: core.Principal{TenantID: "tenant", SubjectID: "owner", Role: "owner"}, ExpiresAt: time.Now().Add(time.Hour)}
	cases := []struct {
		name, origin, csrf string
		status             int
	}{
		{"trusted management origin", "http://127.0.0.1:8001", csrfToken(secret), 200},
		{"foreign origin", "http://127.0.0.1:8002", csrfToken(secret), 403},
		{"missing CSRF", "http://127.0.0.1:8001", "", 403},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			dispatched := false
			s := &Server{deps: Dependencies{Auth: playgroundSessionStore{session: session}, PublicOrigin: "http://127.0.0.1:8001", Playground: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dispatched = true
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("inference path=%q", r.URL.Path)
				}
				if r.Header.Get("Origin") != "" {
					t.Error("management Origin leaked into inference CORS boundary")
				}
				principal, ok := core.PrincipalFromContext(r.Context())
				if !ok || principal.TenantID != "tenant" || principal.SessionID != session.Hash {
					t.Error("trusted principal missing")
				}
				w.WriteHeader(200)
			})}}
			req := httptest.NewRequest("POST", "http://127.0.0.1:8001/admin/api/v1/playground/v1/chat/completions", strings.NewReader(`{"model":"assistant"}`))
			req.RemoteAddr = "127.0.0.1:12345"
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: secret})
			req.Header.Set("Origin", test.origin)
			if test.csrf != "" {
				req.Header.Set("X-CSRF-Token", test.csrf)
			}
			recorder := httptest.NewRecorder()
			s.playground(recorder, req)
			if recorder.Code != test.status {
				t.Fatalf("status=%d want=%d", recorder.Code, test.status)
			}
			if dispatched != (test.status == 200) {
				t.Fatal("unexpected internal dispatch")
			}
			if req.Header.Get("Origin") != test.origin || req.URL.Path != "/admin/api/v1/playground/v1/chat/completions" {
				t.Fatal("internal forwarding mutated original browser request")
			}
		})
	}
}
