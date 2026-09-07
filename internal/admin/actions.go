package admin

import (
	"encoding/json"
	"errors"
	"hoorific/internal/core"
	"net/http"
	"strings"
)

type actionInput struct {
	Data             json.RawMessage `json:"data,omitempty"`
	ExpectedRevision int64           `json:"expected_revision,omitempty"`
	Config           json.RawMessage `json:"config,omitempty"`
	Prune            bool            `json:"prune,omitempty"`
}
type reconcileInput struct{ core.Reconciliation }

func (s *Server) action(kind string, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/v1/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		writeError(w, 404, "not_found", "action not found")
		return
	}
	id, op := parts[1], parts[2]
	perm := "resource:write"
	if kind == "connections" && (op == "test" || op == "discover") {
		perm = "connection:" + op
	}
	if kind == "upstream_operations" && op == "cancel" {
		perm = "job:write"
	}
	if kind == "api_keys" && op == "issue" {
		perm = "key:write"
	}
	p, ok := s.require(w, r, perm)
	if !ok {
		return
	}
	if s.deps.Actions == nil {
		writeError(w, 503, "unsupported_operation", "action service unavailable")
		return
	}
	ver := int64(0)
	if op != "test" && op != "discover" && op != "dry-run" && op != "issue" {
		var e error
		ver, e = parseIfMatch(r)
		if e != nil {
			writeError(w, 428, "if_match_required", e.Error())
			return
		}
	}
	var in actionInput
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(r, &in) {
		writeError(w, 400, "invalid_request", "invalid action body")
		return
	}
	out, e := s.deps.Actions.Execute(r.Context(), p, kind, id, op, ver, in.Data)
	if e != nil {
		writeError(w, 400, "action_failed", e.Error())
		return
	}
	if out == nil {
		out = []byte(`{}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(out)
}
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	p, ok := s.require(w, r, "accounting:reconcile")
	if !ok {
		return
	}
	if s.deps.Reconciler == nil {
		writeError(w, 503, "unsupported_operation", "reconciliation unavailable")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/admin/api/v1/admissions/")
	id = strings.TrimSuffix(id, "/reconcile")
	ver, e := parseIfMatch(r)
	if e != nil {
		writeError(w, 428, "if_match_required", e.Error())
		return
	}
	var in reconcileInput
	if !decodeJSON(r, &in) || in.Reconciliation.ReconciliationID == "" || in.Mode != "provider_evidence" && in.Mode != "charge_reserved_maximum" || in.Reason == "" {
		writeError(w, 400, "invalid_reconciliation", "reconciliation_id, mode and reason are required")
		return
	}
	if in.Mode == "provider_evidence" && (in.SourceReference == "" || in.Usage == nil && in.Cost == nil) {
		writeError(w, 400, "invalid_reconciliation", "provider evidence and source reference are required")
		return
	}
	out, e := s.deps.Reconciler.Reconcile(r.Context(), p, id, ver, in.Reconciliation)
	if e != nil {
		writeError(w, 409, "reconciliation_conflict", e.Error())
		return
	}
	writeJSON(w, 200, sanitizeResource(out))
}
func (s *Server) configAction(op string, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && !(op == "export" && r.Method == "GET") {
		w.WriteHeader(405)
		return
	}
	perm := "config:read"
	if op != "export" {
		perm = "config:write"
	}
	p, ok := s.require(w, r, perm)
	if !ok {
		return
	}
	if s.deps.Actions == nil {
		writeError(w, 503, "unsupported_operation", "configuration service unavailable")
		return
	}
	var in actionInput
	if r.Method == "POST" && !decodeJSON(r, &in) {
		writeError(w, 400, "invalid_request", "invalid configuration request")
		return
	}
	if r.Method == "POST" && in.ExpectedRevision < 1 {
		writeError(w, 400, "invalid_request", "expected_revision is required")
		return
	}
	out, e := s.deps.Actions.Execute(r.Context(), p, "config", "", op, in.ExpectedRevision, mergeConfig(in))
	if e != nil {
		writeError(w, 409, "configuration_rejected", e.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(out)
}
func mergeConfig(in actionInput) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"expected_revision": in.ExpectedRevision, "config": json.RawMessage(in.Config), "prune": in.Prune})
	return b
}
func (s *Server) playground(w http.ResponseWriter, r *http.Request) {
	if s.deps.Playground == nil {
		writeError(w, 503, "unsupported_operation", "playground unavailable")
		return
	}
	p, ok := s.require(w, r, "playground:execute")
	if !ok {
		return
	}
	if len(r.Header.Values("Origin")) > 0 && s.checkOrigin(r) != nil {
		writeError(w, 403, "csrf_failed", "management origin required")
		return
	}
	// require has checked the durable session, CSRF and exact management origin.
	// This in-process dispatch is not a browser request to the inference listener.
	r = r.Clone(core.WithPrincipal(r.Context(), p))
	r.Header.Del("Origin")
	http.StripPrefix("/admin/api/v1/playground", s.deps.Playground).ServeHTTP(w, r)
}
func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) { s.configAction("export", w, r) }
func (s *Server) configDiff(w http.ResponseWriter, r *http.Request)   { s.configAction("diff", w, r) }
func (s *Server) configApply(w http.ResponseWriter, r *http.Request)  { s.configAction("apply", w, r) }

var _ = errors.New
