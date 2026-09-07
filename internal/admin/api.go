package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"hoorific/internal/core"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func New(deps Dependencies) (*Server, error) {
	if deps.Repository == nil {
		return nil, errors.New("admin repository is required")
	}
	if deps.Auth == nil {
		return nil, errors.New("admin auth store is required")
	}
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.api = humago.New(s.mux, huma.DefaultConfig("Hoorific administrative API", "1.0.0"))
	s.registerHumaRuntime(s.api)
	s.registerHumaActions(s.api)
	s.registerAdditionalHumaActions(s.api)
	s.registerHumaReconcile(s.api)
	s.registerRoutes()
	documentProblemCodes(s.api)
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if runtimePath(r) {
		ctx := context.WithValue(r.Context(), requestContextKey{}, r)
		if !strings.HasPrefix(r.URL.Path, "/admin/api/v1/auth/") {
			p, e := s.authenticate(r)
			if e != nil {
				writeError(w, 401, "authentication_required", "authentication required")
				return
			}
			ctx = core.WithPrincipal(ctx, p)
		}
		r = r.WithContext(ctx)
	}
	s.mux.ServeHTTP(w, r)
}
func (s *Server) registerRoutes() {
	s.registerAuth()
	s.mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	s.mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Ready == nil || s.deps.Ready(r.Context()) != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "management service is not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Metrics == nil {
			writeError(w, http.StatusServiceUnavailable, "metrics_unavailable", "metrics are unavailable")
			return
		}
		s.deps.Metrics.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("/admin/api/v1/config/export", s.exportConfig)
	s.mux.HandleFunc("/admin/api/v1/config/diff", s.configDiff)
	s.mux.HandleFunc("/admin/api/v1/config/apply", s.configApply)
	s.mux.HandleFunc("/admin/api/v1/playground/", s.playground)
}
func (s *Server) dispatchKind(kind string, w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	if kind == "admissions" && strings.HasSuffix(p, "/reconcile") {
		s.reconcile(w, r)
		return
	}
	if strings.HasSuffix(p, "/test") || strings.HasSuffix(p, "/discover") || strings.HasSuffix(p, "/disable") || strings.HasSuffix(p, "/rotate") || strings.HasSuffix(p, "/revoke") || strings.HasSuffix(p, "/issue") || strings.HasSuffix(p, "/dry-run") || strings.HasSuffix(p, "/cancel") {
		s.action(kind, w, r)
		return
	}
	s.resource(kind, w, r)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, msg string) {
	id := requestID(w)
	writeJSON(w, status, map[string]any{"type": "about:blank", "title": msg, "status": status, "code": code, "request_id": id})
}
func requestID(w http.ResponseWriter) string {
	b := make([]byte, 12)
	if _, e := rand.Read(b); e != nil {
		return "unknown"
	}
	id := hex.EncodeToString(b)
	w.Header().Set("X-Request-ID", id)
	return id
}
func decodeJSON(r *http.Request, v any) bool {
	d := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
}
func (s *Server) require(w http.ResponseWriter, r *http.Request, permission string) (core.Principal, bool) {
	p, e := s.authenticate(r)
	if e != nil {
		writeError(w, 401, "authentication_required", "authentication required")
		return p, false
	}
	allowed := hasPermission(p, permission)
	if permission == "resource:read" || permission == "resource:write" {
		allowed = hasPermission(p, permission) || p.Role == "admin" || p.Role == "owner" || (permission == "resource:read" && (p.Role == "operator" || p.Role == "auditor" || p.Role == "viewer"))
	}
	if !allowed {
		writeError(w, 403, "forbidden", "permission denied")
		return p, false
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		if e = s.checkMutation(r); e != nil {
			writeError(w, 403, "csrf_failed", "mutation authentication failed")
			return p, false
		}
	}
	return p, true
}
func parseIfMatch(r *http.Request) (int64, error) {
	v := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), "\"")
	if v == "" {
		return 0, errors.New("If-Match required")
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil || n < 1 {
		return 0, errors.New("invalid If-Match")
	}
	return n, nil
}
