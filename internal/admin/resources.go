package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"hoorific/internal/core"
	"net/http"
	"strings"
)

var resourceKinds = []string{"tenants", "operators", "role_bindings", "connections", "credentials", "api_keys", "models", "model_aliases", "route_policies", "policy_limits", "usage_ledger", "audit_events", "upstream_operations", "admissions", "reconciliations", "account_pools", "oauth_sessions"}

// Resource data is intentionally typed per kind at the API boundary. Unknown fields are rejected.
type TenantData struct {
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	MaxBodyBytes   int64    `json:"max_body_bytes,omitempty"`
	MaxEventBytes  int64    `json:"max_event_bytes,omitempty"`
}
type OperatorData struct {
	Subject         string `json:"subject"`
	Issuer          string `json:"issuer"`
	IdentitySubject string `json:"identity_subject"`
	DisplayName     string `json:"display_name"`
	Enabled         bool   `json:"enabled"`
}
type RoleBindingData struct {
	Subject  string `json:"subject"`
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}
type ConnectionData struct {
	Connector string            `json:"connector"`
	AccountID string            `json:"account_id"`
	BaseURL   string            `json:"base_url"`
	Region    string            `json:"region,omitempty"`
	Project   string            `json:"project,omitempty"`
	Dedicated bool              `json:"dedicated"`
	Enabled   bool              `json:"enabled"`
	Settings  map[string]string `json:"settings,omitempty"`
}
type ModelData struct {
	ConnectionID     string              `json:"connection_id"`
	UpstreamID       string              `json:"upstream_id"`
	Operations       []string            `json:"operations"`
	Features         map[string]string   `json:"features,omitempty"`
	InputModalities  []string            `json:"input_modalities,omitempty"`
	OutputModalities []string            `json:"output_modalities,omitempty"`
	ContextLimit     *int64              `json:"context_limit,omitempty"`
	OutputLimit      *int64              `json:"output_limit,omitempty"`
	Provenance       string              `json:"provenance,omitempty"`
	Price            *core.PriceSchedule `json:"price,omitempty"`
	Enabled          bool                `json:"enabled"`
}
type AliasData struct {
	ModelIDs    []string `json:"model_ids"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
}
type RoutePolicyData struct {
	Alias         string            `json:"alias"`
	Targets       []RouteTargetData `json:"targets"`
	Residency     []string          `json:"residency,omitempty"`
	Fallback      bool              `json:"fallback"`
	AccountPoolID string            `json:"account_pool_id,omitempty"`
	Affinity      bool              `json:"affinity"`
}
type RouteTargetData struct {
	ConnectionID string `json:"connection_id"`
	ModelID      string `json:"model_id"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
	Region       string `json:"region,omitempty"`
}
type PolicyLimitData struct {
	Scope             string `json:"scope"`
	ScopeID           string `json:"scope_id"`
	RequestsPerMinute int64  `json:"requests_per_minute,omitempty"`
	TokensPerMinute   int64  `json:"tokens_per_minute,omitempty"`
	MaxCost           int64  `json:"max_cost,omitempty"`
	Concurrency       int    `json:"concurrency,omitempty"`
	CostWindow        string `json:"cost_window,omitempty"`
	OutstandingJobs   int    `json:"outstanding_jobs,omitempty"`
}
type UsageLedgerData struct {
	AttemptID string      `json:"attempt_id"`
	Kind      string      `json:"kind"`
	Cost      *int64      `json:"cost,omitempty"`
	Usage     *core.Usage `json:"usage,omitempty"`
	CreatedAt string      `json:"created_at"`
}
type AuditEventData struct {
	Action          string `json:"action"`
	Actor           string `json:"actor"`
	CreatedAt       string `json:"created_at"`
	Summary         string `json:"summary"`
	Kind            string `json:"kind"`
	ResourceID      string `json:"resource_id"`
	ResourceVersion int64  `json:"resource_version"`
}
type UpstreamOperationData struct {
	ConnectionID string `json:"connection_id"`
	Operation    string `json:"operation"`
	ResourceID   string `json:"resource_id"`
	Status       string `json:"status"`
}
type AdmissionData struct {
	AttemptID   string      `json:"attempt_id"`
	State       string      `json:"state"`
	MaximumCost *int64      `json:"maximum_cost,omitempty"`
	Usage       *core.Usage `json:"usage,omitempty"`
	RequestID   string      `json:"request_id"`
	ActualCost  *int64      `json:"actual_cost,omitempty"`
	Reconciled  bool        `json:"reconciled"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
	Deadline    string      `json:"deadline,omitempty"`
}
type ReconciliationData struct {
	core.Reconciliation
	AdmissionID      string `json:"admission_id"`
	AdmissionVersion int64  `json:"admission_version"`
	State            string `json:"state"`
	Reconciled       bool   `json:"reconciled"`
	ChargedCost      *int64 `json:"charged_cost,omitempty"`
	Delta            *int64 `json:"delta,omitempty"`
}
type AccountPoolData struct {
	Provider   string   `json:"provider"`
	AccountIDs []string `json:"account_ids"`
}
type OAuthSessionData struct {
	Connector string `json:"connector"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

type resourceInput struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

func validateResource(kind string, data json.RawMessage) error {
	if len(data) == 0 || string(data) == "null" {
		return errors.New("data is required")
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	switch kind {
	case "tenants":
		v = &TenantData{}
	case "operators":
		v = &OperatorData{}
	case "role_bindings":
		v = &RoleBindingData{}
	case "connections":
		v = &ConnectionData{}
	case "credentials":
		return errors.New("credentials are read-only; use connection credential actions")
	case "api_keys":
		return errors.New("API keys are issued through the key issue action")
	case "models":
		v = &ModelData{}
	case "model_aliases":
		v = &AliasData{}
	case "route_policies":
		v = &RoutePolicyData{}
	case "policy_limits":
		v = &PolicyLimitData{}
	case "usage_ledger":
		v = &UsageLedgerData{}
	case "audit_events":
		v = &AuditEventData{}
	case "upstream_operations":
		v = &UpstreamOperationData{}
	case "admissions":
		v = &AdmissionData{}
	case "reconciliations":
		v = &ReconciliationData{}
	case "account_pools":
		v = &AccountPoolData{}
	case "oauth_sessions":
		v = &OAuthSessionData{}
	default:
		return errors.New("unknown resource kind")
	}
	if err := d.Decode(v); err != nil {
		return err
	}
	return nil
}
func sanitizeResource(r core.Resource) core.Resource {
	if r.Kind == "credentials" {
		var d map[string]any
		if json.Unmarshal(r.Data, &d) == nil {
			delete(d, "secret")
			delete(d, "access_token")
			delete(d, "refresh_token")
			delete(d, "private_key")
			r.Data, _ = json.Marshal(d)
		}
	}
	if r.Kind == "api_keys" {
		var d map[string]any
		if json.Unmarshal(r.Data, &d) == nil {
			delete(d, "secret")
			delete(d, "verifier")
			r.Data, _ = json.Marshal(d)
		}
	}
	return r
}
func resourcePermission(kind string, write bool) string { return runtimePermission(kind, write) }
func (s *Server) collection(kind string, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	perm := resourcePermission(kind, r.Method == "POST")
	p, ok := s.require(w, r, perm)
	if !ok {
		return
	}
	if r.Method == "GET" {
		limit := 50
		if x := r.URL.Query().Get("limit"); x != "" {
			if n, e := parseLimit(x); e == nil {
				limit = n
			} else {
				writeError(w, 400, "invalid_limit", "invalid limit")
				return
			}
		}
		page, e := s.deps.Repository.List(r.Context(), p, kind, r.URL.Query().Get("cursor"), limit)
		if e != nil {
			writeError(w, 500, "repository_error", "could not list resources")
			return
		}
		for i := range page.Items {
			page.Items[i] = sanitizeResource(page.Items[i])
		}
		writeJSON(w, 200, page)
		return
	}
	var in resourceInput
	if !decodeJSON(r, &in) || in.ID == "" || validateResource(kind, in.Data) != nil {
		writeError(w, 400, "invalid_resource", "invalid typed resource")
		return
	}
	out, e := s.deps.Repository.Mutate(r.Context(), p, core.Mutation{Kind: kind, ID: in.ID, Data: in.Data})
	if e != nil {
		writeError(w, 409, "mutation_failed", e.Error())
		return
	}
	writeJSON(w, 201, sanitizeResource(out))
}
func (s *Server) resource(kind string, w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/v1/"+kind+"/"), "/")
	if id == "" {
		writeError(w, 404, "not_found", "resource not found")
		return
	}
	perm := resourcePermission(kind, r.Method != "GET")
	p, ok := s.require(w, r, perm)
	if !ok {
		return
	}
	switch r.Method {
	case "GET":
		out, e := s.deps.Repository.Get(r.Context(), p, kind, id)
		if e != nil {
			writeError(w, 404, "not_found", "resource not found")
			return
		}
		writeJSON(w, 200, sanitizeResource(out))
	case "PUT":
		ver, e := parseIfMatch(r)
		if e != nil {
			writeError(w, 428, "if_match_required", e.Error())
			return
		}
		var in struct {
			Data json.RawMessage `json:"data"`
		}
		if !decodeJSON(r, &in) || validateResource(kind, in.Data) != nil {
			writeError(w, 400, "invalid_resource", "invalid typed resource")
			return
		}
		out, e := s.deps.Repository.Mutate(r.Context(), p, core.Mutation{Kind: kind, ID: id, ExpectedVersion: ver, Data: in.Data})
		if e != nil {
			writeError(w, 412, "version_conflict", e.Error())
			return
		}
		writeJSON(w, 200, sanitizeResource(out))
	case "DELETE":
		ver, e := parseIfMatch(r)
		if e != nil {
			writeError(w, 428, "if_match_required", e.Error())
			return
		}
		out, e := s.deps.Repository.Mutate(r.Context(), p, core.Mutation{Kind: kind, ID: id, ExpectedVersion: ver, Delete: true})
		if e != nil {
			writeError(w, 412, "version_conflict", e.Error())
			return
		}
		writeJSON(w, 200, sanitizeResource(out))
	default:
		w.WriteHeader(405)
	}
}
func parseLimit(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("number")
		}
		n = n*10 + int(c-'0')
		if n > 200 {
			return 0, errors.New("too large")
		}
	}
	if n < 1 {
		n = 1
	}
	return n, nil
}
