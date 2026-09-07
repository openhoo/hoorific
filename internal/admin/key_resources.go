package admin

import (
	"context"
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
	"hoorific/internal/core"
)

type APIKeyGrantData struct {
	Name          string           `json:"name,omitempty"`
	Role          string           `json:"role" enum:"viewer,operator,admin"`
	Permissions   []string         `json:"permissions"`
	Aliases       []string         `json:"aliases,omitempty"`
	Connections   []string         `json:"connections,omitempty"`
	Operations    []core.Operation `json:"operations,omitempty"`
	Portable      bool             `json:"portable"`
	NativeAccount bool             `json:"native_account"`
	Realtime      bool             `json:"realtime"`
}
type APIKeyMetadataData struct{ APIKeyGrantData }
type APIKeyIssuedData struct {
	Resource typedResource[APIKeyMetadataData] `json:"resource"`
	Token    string                            `json:"token" writeOnly:"true"`
}
type apiKeyIssueInput struct {
	ID   string `path:"id"`
	Body struct {
		Data APIKeyGrantData `json:"data"`
	}
}
type apiKeyActionInput struct {
	ID      string `path:"id"`
	IfMatch string `header:"If-Match"`
	Body    struct {
		Data struct{} `json:"data"`
	}
}
type apiKeyIssuedOutput struct{ Body APIKeyIssuedData }
type APIKeyRevokedData struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Version  int64  `json:"version"`
	Revoked  bool   `json:"revoked"`
}
type apiKeyRevokedOutput struct{ Body APIKeyRevokedData }

func (s *Server) registerAPIKeyActions(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "issueKey", Method: "POST", Path: "/admin/api/v1/api_keys/{id}/issue"}, func(ctx context.Context, in *apiKeyIssueInput) (*apiKeyIssuedOutput, error) {
		p, e := s.runtimePrincipalWith(ctx, "key:write")
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		if s.deps.Actions == nil {
			return nil, huma.NewError(503, "key service unavailable")
		}
		data, e := json.Marshal(in.Body.Data)
		if e != nil {
			return nil, huma.Error400BadRequest("invalid key grant")
		}
		raw, e := s.deps.Actions.Execute(ctx, p, "api_keys", in.ID, "issue", 0, data)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		var out APIKeyIssuedData
		if e = json.Unmarshal(raw, &out); e != nil {
			return nil, e
		}
		return &apiKeyIssuedOutput{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "rotateKey", Method: "POST", Path: "/admin/api/v1/api_keys/{id}/rotate"}, func(ctx context.Context, in *apiKeyActionInput) (*apiKeyIssuedOutput, error) {
		p, e := s.runtimePrincipalWith(ctx, "key:write")
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		version, e := parseIfMatch(r)
		if e != nil {
			return nil, huma.NewError(428, "If-Match is required", e)
		}
		if s.deps.Actions == nil {
			return nil, huma.NewError(503, "key service unavailable")
		}
		raw, e := s.deps.Actions.Execute(ctx, p, "api_keys", in.ID, "rotate", version, nil)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		var out APIKeyIssuedData
		if e = json.Unmarshal(raw, &out); e != nil {
			return nil, e
		}
		return &apiKeyIssuedOutput{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "revokeKey", Method: "POST", Path: "/admin/api/v1/api_keys/{id}/revoke"}, func(ctx context.Context, in *apiKeyActionInput) (*apiKeyRevokedOutput, error) {
		p, e := s.runtimePrincipalWith(ctx, "key:write")
		if e != nil {
			return nil, e
		}
		r := requestFromContext(ctx)
		if r == nil || s.checkMutation(r) != nil {
			return nil, huma.Error403Forbidden("mutation authentication failed")
		}
		version, e := parseIfMatch(r)
		if e != nil {
			return nil, huma.NewError(428, "If-Match is required", e)
		}
		if s.deps.Actions == nil {
			return nil, huma.NewError(503, "key service unavailable")
		}
		raw, e := s.deps.Actions.Execute(ctx, p, "api_keys", in.ID, "revoke", version, nil)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		var resource core.Resource
		if e = json.Unmarshal(raw, &resource); e != nil {
			return nil, e
		}
		// Deleted resources do not retain data; return only the revoked identifier.
		out := APIKeyRevokedData{ID: resource.ID, TenantID: resource.TenantID, Version: resource.Version, Revoked: true}
		return &apiKeyRevokedOutput{Body: out}, nil
	})
}
