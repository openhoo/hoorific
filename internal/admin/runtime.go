package admin

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"hoorific/internal/core"
	"net/http"
	"strings"
)

type requestContextKey struct{}
type humaCollectionInput struct {
	Cursor string `query:"cursor"`
	Limit  int    `query:"limit"`
}
type humaCreateInput struct{ Body resourceInput }
type humaGetInput struct {
	ID string `path:"id"`
}
type humaDeleteInput struct {
	ID      string `path:"id"`
	IfMatch string `header:"If-Match"`
}
type humaBodyInput struct {
	ID      string `path:"id"`
	Body    resourceInput
	IfMatch string `header:"If-Match"`
}
type humaRuntimeActionInput struct {
	ID      string `path:"id"`
	Body    actionInput
	IfMatch string `header:"If-Match"`
}
type humaRuntimeReconcileInput struct {
	ID      string `path:"id"`
	Body    core.Reconciliation
	IfMatch string `header:"If-Match"`
}
type humaRuntimeResourceOutput struct{ Body core.Resource }
type humaRuntimePageOutput struct{ Body core.ResourcePage }

func requestFromContext(ctx context.Context) *http.Request {
	r, _ := ctx.Value(requestContextKey{}).(*http.Request)
	return r
}
func runtimePermission(kind string, write bool) string {
	domain := "resource"
	switch kind {
	case "tenants", "operators", "role_bindings":
		domain = "tenant"
	case "connections", "credentials":
		domain = "connection"
	case "models", "model_aliases", "account_pools":
		domain = "catalog"
	case "api_keys":
		domain = "key"
	case "route_policies":
		domain = "route"
	case "policy_limits":
		domain = "budget"
	case "usage_ledger", "admissions", "reconciliations":
		domain = "usage"
	case "audit_events":
		domain = "audit"
	case "upstream_operations":
		domain = "job"
	case "oauth_sessions":
		domain = "session"
	}
	if write {
		return domain + ":write"
	}
	return domain + ":read"
}
func runtimeActionPermission(kind, op string) string {
	if kind == "connections" && (op == "test" || op == "discover" || op == "status") {
		return "connection:" + map[string]string{"test": "test", "discover": "discover", "status": "read"}[op]
	}
	if kind == "upstream_operations" {
		return "job:write"
	}
	if kind == "api_keys" {
		return "key:write"
	}
	if kind == "route_policies" {
		return "route:write"
	}
	if kind == "admissions" {
		return "accounting:reconcile"
	}
	return "resource:write"
}
func (s *Server) runtimePrincipalWith(ctx context.Context, permission string) (core.Principal, error) {
	p, ok := core.PrincipalFromContext(ctx)
	if !ok {
		return core.Principal{}, huma.Error401Unauthorized("authentication required")
	}
	if hasPermission(p, permission) {
		return p, nil
	}
	if p.SessionID != "" && permission == "resource:read" && (p.Role == "admin" || p.Role == "owner" || p.Role == "operator" || p.Role == "auditor" || p.Role == "viewer") {
		return p, nil
	}
	if p.SessionID != "" && permission == "resource:write" && (p.Role == "admin" || p.Role == "owner") {
		return p, nil
	}
	return core.Principal{}, huma.Error403Forbidden("permission denied")
}
func runtimeRepositoryError(err error) error {
	var gateway core.GatewayError
	if errors.As(err, &gateway) && gateway.HTTPStatus >= 400 && gateway.HTTPStatus < 600 {
		return gatewayProblem(gateway)
	}
	var gatewayPointer *core.GatewayError
	if errors.As(err, &gatewayPointer) && gatewayPointer != nil && gatewayPointer.HTTPStatus >= 400 && gatewayPointer.HTTPStatus < 600 {
		return gatewayProblem(*gatewayPointer)
	}
	var status huma.StatusError
	if errors.As(err, &status) && status.GetStatus() >= 400 && status.GetStatus() < 600 {
		if status.GetStatus() >= 500 {
			return huma.NewError(status.GetStatus(), "Administrative operation failed")
		}
		return err
	}
	return huma.Error500InternalServerError("Administrative operation failed")
}

type typedResourceEnvelope[T any] struct {
	ID   string `json:"id"`
	Data T      `json:"data"`
}
type typedResourceInput[T any] struct{ Body typedResourceEnvelope[T] }
type typedResourceUpdate[T any] struct {
	ID   string `path:"id"`
	Body struct {
		Data T `json:"data"`
	}
	IfMatch string `header:"If-Match"`
}
type typedResourceGet struct {
	ID string `path:"id"`
}
type typedResource[T any] struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Kind     string `json:"kind"`
	Version  int64  `json:"version"`
	Data     T      `json:"data"`
}
type typedResourceOutput[T any] struct{ Body typedResource[T] }
type typedResourcePage[T any] struct {
	Items      []typedResource[T] `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}
type typedResourcePageOutput[T any] struct{ Body typedResourcePage[T] }

func decodeTypedResource[T any](r core.Resource) (typedResource[T], error) {
	var d T
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return typedResource[T]{}, err
	}
	return typedResource[T]{ID: r.ID, TenantID: r.TenantID, Kind: r.Kind, Version: r.Version, Data: d}, nil
}
func (s *Server) registerHumaRuntime(api huma.API) {
	s.registerConnectionCapabilities(api)
	for _, kind := range resourceKinds {
		switch kind {
		case "tenants":
			registerTypedResourceRuntime[TenantData](api, s, kind)
		case "operators":
			registerTypedResourceRuntime[OperatorData](api, s, kind)
		case "role_bindings":
			registerTypedResourceRuntime[RoleBindingData](api, s, kind)
		case "connections":
			registerTypedResourceRuntime[ConnectionData](api, s, kind)
		case "credentials":
			s.registerCredentialResources(api)
		case "api_keys":
			registerReadOnlyResource[APIKeyMetadataData](api, s, kind, "key:read")
		case "models":
			registerTypedResourceRuntime[ModelData](api, s, kind)
		case "model_aliases":
			registerTypedResourceRuntime[AliasData](api, s, kind)
		case "route_policies":
			registerTypedResourceRuntime[RoutePolicyData](api, s, kind)
		case "policy_limits":
			registerTypedResourceRuntime[PolicyLimitData](api, s, kind)
		case "usage_ledger":
			registerReadOnlyResource[UsageLedgerData](api, s, kind, "usage:read")
		case "audit_events":
			registerReadOnlyResource[AuditEventData](api, s, kind, "audit:read")
		case "upstream_operations":
			registerReadOnlyResource[UpstreamOperationData](api, s, kind, "job:read")
		case "admissions":
			registerReadOnlyResource[AdmissionData](api, s, kind, "usage:read")
		case "reconciliations":
			registerReadOnlyResource[ReconciliationData](api, s, kind, "usage:read")
		case "account_pools":
			registerTypedResourceRuntime[AccountPoolData](api, s, kind)
		case "oauth_sessions":
			registerReadOnlyResource[OAuthSessionData](api, s, kind, "session:read")
		}
	}
}
func registerTypedResourceRuntime[T any](api huma.API, s *Server, kind string) {
	huma.Register(api, huma.Operation{OperationID: "list_" + kind, Method: "GET", Path: "/admin/api/v1/" + kind}, func(ctx context.Context, in *humaCollectionInput) (*typedResourcePageOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, runtimePermission(kind, false))
		if e != nil {
			return nil, e
		}
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 200 {
			return nil, errors.New("limit must be between 1 and 200")
		}
		page, e := s.deps.Repository.List(ctx, p, kind, in.Cursor, limit)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		out := typedResourcePage[T]{NextCursor: page.NextCursor}
		for _, r := range page.Items {
			r = sanitizeResource(r)
			v, e := decodeTypedResource[T](r)
			if e != nil {
				return nil, e
			}
			out.Items = append(out.Items, v)
		}
		return &typedResourcePageOutput[T]{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "create_" + kind, Method: "POST", Path: "/admin/api/v1/" + kind}, func(ctx context.Context, in *typedResourceInput[T]) (*typedResourceOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, runtimePermission(kind, true))
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		data, e := json.Marshal(in.Body.Data)
		if e != nil || in.Body.ID == "" || validateResource(kind, data) != nil {
			return nil, huma.Error400BadRequest("invalid typed resource")
		}
		out, e := s.deps.Repository.Mutate(ctx, p, core.Mutation{Kind: kind, ID: in.Body.ID, Data: data})
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		v, e := decodeTypedResource[T](sanitizeResource(out))
		if e != nil {
			return nil, e
		}
		return &typedResourceOutput[T]{Body: v}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "get_" + kind, Method: "GET", Path: "/admin/api/v1/" + kind + "/{id}"}, func(ctx context.Context, in *typedResourceGet) (*typedResourceOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, runtimePermission(kind, false))
		if e != nil {
			return nil, e
		}
		out, e := s.deps.Repository.Get(ctx, p, kind, in.ID)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		v, e := decodeTypedResource[T](sanitizeResource(out))
		if e != nil {
			return nil, e
		}
		return &typedResourceOutput[T]{Body: v}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "update_" + kind, Method: "PUT", Path: "/admin/api/v1/" + kind + "/{id}"}, func(ctx context.Context, in *typedResourceUpdate[T]) (*typedResourceOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, runtimePermission(kind, true))
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		v, e := parseIfMatch(r)
		if e != nil {
			return nil, huma.NewError(428, "If-Match is required", e)
		}
		data, e := json.Marshal(in.Body.Data)
		if e != nil || validateResource(kind, data) != nil {
			return nil, huma.Error400BadRequest("invalid typed resource")
		}
		out, e := s.deps.Repository.Mutate(ctx, p, core.Mutation{Kind: kind, ID: in.ID, ExpectedVersion: v, Data: data})
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		rv, e := decodeTypedResource[T](sanitizeResource(out))
		if e != nil {
			return nil, e
		}
		return &typedResourceOutput[T]{Body: rv}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "delete_" + kind, Method: "DELETE", Path: "/admin/api/v1/" + kind + "/{id}"}, func(ctx context.Context, in *typedResourceGet) (*typedResourceOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, runtimePermission(kind, true))
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		v, e := parseIfMatch(r)
		if e != nil {
			return nil, huma.NewError(428, "If-Match is required", e)
		}
		out, e := s.deps.Repository.Mutate(ctx, p, core.Mutation{Kind: kind, ID: in.ID, ExpectedVersion: v, Delete: true})
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		safe := sanitizeResource(out)
		rv := typedResource[T]{ID: safe.ID, TenantID: safe.TenantID, Kind: safe.Kind, Version: safe.Version}
		if len(safe.Data) != 0 {
			rv, e = decodeTypedResource[T](safe)
			if e != nil {
				return nil, e
			}
		}
		return &typedResourceOutput[T]{Body: rv}, nil
	})
}

func (s *Server) registerHumaActions(api huma.API) {
	s.registerAPIKeyActions(api)
	for _, x := range []struct{ path, id, kind, op string }{{"/admin/api/v1/connections/{id}/test", "testConnection", "connections", "test"}, {"/admin/api/v1/connections/{id}/discover", "discoverConnection", "connections", "discover"}, {"/admin/api/v1/connections/{id}/disable", "disableConnection", "connections", "disable"}, {"/admin/api/v1/route_policies/{id}/dry-run", "dryRunRoute", "route_policies", "dry-run"}, {"/admin/api/v1/upstream_operations/{id}/cancel", "cancelOperation", "upstream_operations", "cancel"}} {
		x := x
		huma.Register(api, huma.Operation{OperationID: x.id, Method: "POST", Path: x.path}, func(ctx context.Context, in *humaRuntimeActionInput) (*humaActionOutput, error) {
			p, e := s.runtimePrincipalWith(ctx, runtimeActionPermission(x.kind, x.op))
			if e != nil {
				return nil, e
			}
			r := requestFromContext(ctx)
			if r == nil || s.checkMutation(r) != nil {
				return nil, huma.Error403Forbidden("mutation authentication failed")
			}
			if s.deps.Actions == nil {
				return nil, errors.New("action service unavailable")
			}
			v := int64(0)
			if x.op != "test" && x.op != "discover" && x.op != "dry-run" && x.op != "issue" {
				v, e = parseIfMatch(r)
				if e != nil {
					return nil, huma.NewError(428, "If-Match is required", e)
				}
			}
			out, e := s.deps.Actions.Execute(ctx, p, x.kind, in.ID, x.op, v, in.Body.Data)
			if e != nil {
				return nil, runtimeRepositoryError(e)
			}
			var data map[string]any
			_ = json.Unmarshal(out, &data)
			return &humaActionOutput{Body: data}, nil
		})
	}
}
func (s *Server) registerHumaReconcile(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "reconcileAdmission", Method: "POST", Path: "/admin/api/v1/admissions/{id}/reconcile"}, func(ctx context.Context, in *humaRuntimeReconcileInput) (*humaRuntimeResourceOutput, error) {
		p, e := s.runtimePrincipalWith(ctx, "accounting:reconcile")
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		if s.deps.Reconciler == nil {
			return nil, errors.New("reconciliation unavailable")
		}
		v, e := parseIfMatch(r)
		if e != nil {
			return nil, huma.NewError(428, "If-Match is required", e)
		}
		if in.Body.ReconciliationID == "" || in.Body.Mode != "provider_evidence" && in.Body.Mode != "charge_reserved_maximum" || in.Body.Reason == "" {
			return nil, errors.New("invalid reconciliation")
		}
		out, e := s.deps.Reconciler.Reconcile(ctx, p, in.ID, v, in.Body)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		out = sanitizeResource(out)
		return &humaRuntimeResourceOutput{Body: out}, nil
	})
}
func (s *Server) registerAdditionalHumaActions(api huma.API) {
	s.registerHumaCredentialActions(api)
	for _, x := range []struct{ path, id, kind, op string }{{"/admin/api/v1/upstream_operations/{id}/result", "operationResult", "upstream_operations", "result"}} {
		x := x
		huma.Register(api, huma.Operation{OperationID: x.id, Method: "POST", Path: x.path}, func(ctx context.Context, in *humaRuntimeActionInput) (*humaActionOutput, error) {
			p, e := s.runtimePrincipalWith(ctx, runtimeActionPermission(x.kind, x.op))
			if e != nil {
				return nil, e
			}
			r := requestFromContext(ctx)
			if r == nil || s.checkMutation(r) != nil {
				return nil, huma.Error403Forbidden("mutation authentication failed")
			}
			if s.deps.Actions == nil {
				return nil, errors.New("action service unavailable")
			}
			v := int64(0)
			if x.op != "status" {
				v, e = parseIfMatch(r)
				if e != nil {
					return nil, huma.NewError(428, "If-Match is required", e)
				}
			}
			out, e := s.deps.Actions.Execute(ctx, p, x.kind, in.ID, x.op, v, in.Body.Data)
			if e != nil {
				return nil, e
			}
			var data map[string]any
			_ = json.Unmarshal(out, &data)
			return &humaActionOutput{Body: data}, nil
		})
	}
}
func runtimePath(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/admin/api/v1/") }
