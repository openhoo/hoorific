package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// CredentialMetadataData contains no encrypted envelope or secret-bearing fields.
type CredentialMetadataData struct {
	ConnectionID string    `json:"connection_id"`
	CredentialID string    `json:"credential_id"`
	Provider     string    `json:"provider"`
	AccountID    string    `json:"account_id"`
	Status       string    `json:"status"`
	Version      int64     `json:"version"`
	RotatedAt    time.Time `json:"rotated_at"`
}

// CredentialActionData mirrors the connection-bound encrypted credential service.
type CredentialActionData struct {
	Provider          string     `json:"provider"`
	AccountID         string     `json:"account_id"`
	CredentialVersion int64      `json:"credential_version,omitempty"`
	Kind              string     `json:"kind,omitempty"`
	Secret            string     `json:"secret,omitempty" writeOnly:"true"`
	AccessToken       string     `json:"access_token,omitempty" writeOnly:"true"`
	RefreshToken      string     `json:"refresh_token,omitempty" writeOnly:"true"`
	TokenType         string     `json:"token_type,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	Scopes            []string   `json:"scopes,omitempty"`
	State             string     `json:"state,omitempty" writeOnly:"true"`
	Code              string     `json:"code,omitempty" writeOnly:"true"`
	RedirectURI       string     `json:"redirect_uri,omitempty"`
	FlowID            string     `json:"flow_id,omitempty"`
}
type CredentialActionResult struct {
	CredentialID             string     `json:"credential_id,omitempty"`
	Provider                 string     `json:"provider,omitempty"`
	AccountID                string     `json:"account_id,omitempty"`
	Status                   string     `json:"status"`
	Version                  int64      `json:"version,omitempty"`
	RotatedAt                *time.Time `json:"rotated_at,omitempty"`
	AuthorizationURL         string     `json:"authorization_url,omitempty"`
	AuthorizationURLComplete string     `json:"authorization_url_complete,omitempty"`
	ExpiresAt                *time.Time `json:"expires_at,omitempty"`
	FlowID                   string     `json:"flow_id,omitempty"`
	UserCode                 string     `json:"user_code,omitempty"`
}
type credentialActionInput struct {
	ID      string `path:"id"`
	IfMatch string `header:"If-Match"`
	Body    struct {
		Data CredentialActionData `json:"data"`
	}
}
type credentialActionOutput struct{ Body CredentialActionResult }

func (s *Server) registerCredentialResources(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "list_credentials", Method: "GET", Path: "/admin/api/v1/credentials"}, func(ctx context.Context, in *humaCollectionInput) (*typedResourcePageOutput[CredentialMetadataData], error) {
		p, e := s.runtimePrincipalWith(ctx, "connection:read")
		if e != nil {
			return nil, e
		}
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 200 {
			return nil, huma.Error400BadRequest("limit must be between 1 and 200")
		}
		page, e := s.deps.Repository.List(ctx, p, "credentials", in.Cursor, limit)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		out := typedResourcePage[CredentialMetadataData]{Items: []typedResource[CredentialMetadataData]{}, NextCursor: page.NextCursor}
		for _, r := range page.Items {
			item, e := decodeTypedResource[CredentialMetadataData](r)
			if e != nil {
				return nil, e
			}
			out.Items = append(out.Items, item)
		}
		return &typedResourcePageOutput[CredentialMetadataData]{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "get_credentials", Method: "GET", Path: "/admin/api/v1/credentials/{id}"}, func(ctx context.Context, in *typedResourceGet) (*typedResourceOutput[CredentialMetadataData], error) {
		p, e := s.runtimePrincipalWith(ctx, "connection:read")
		if e != nil {
			return nil, e
		}
		r, e := s.deps.Repository.Get(ctx, p, "credentials", in.ID)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		out, e := decodeTypedResource[CredentialMetadataData](r)
		if e != nil {
			return nil, e
		}
		return &typedResourceOutput[CredentialMetadataData]{Body: out}, nil
	})
}

func (s *Server) registerHumaCredentialActions(api huma.API) {
	for _, entry := range []struct{ action, id string }{{"import", "importCredential"}, {"status", "connectionStatus"}, {"revoke-credential", "revokeCredential"}, {"oauth-start", "oauthStart"}, {"oauth-callback", "oauthCallback"}, {"device-start", "deviceStart"}, {"device-poll", "devicePoll"}} {
		entry := entry
		huma.Register(api, huma.Operation{OperationID: entry.id, Method: "POST", Path: "/admin/api/v1/connections/{id}/" + entry.action}, func(ctx context.Context, in *credentialActionInput) (*credentialActionOutput, error) {
			permission := "connection:write"
			if entry.action == "status" {
				permission = "connection:read"
			}
			p, e := s.runtimePrincipalWith(ctx, permission)
			if e != nil {
				return nil, e
			}
			r := requestFromContext(ctx)
			if r == nil || s.checkMutation(r) != nil {
				return nil, huma.Error403Forbidden("mutation authentication failed")
			}
			if s.deps.Actions == nil {
				return nil, huma.NewError(503, "credential service unavailable")
			}
			version := int64(0)
			if entry.action != "status" {
				version, e = parseIfMatch(r)
				if e != nil {
					return nil, huma.NewError(428, "If-Match is required", e)
				}
			}
			data, e := json.Marshal(in.Body.Data)
			if e != nil {
				return nil, huma.Error400BadRequest("invalid credential action data")
			}
			raw, e := s.deps.Actions.Execute(ctx, p, "connections", in.ID, entry.action, version, json.RawMessage(data))
			if e != nil {
				return nil, runtimeRepositoryError(e)
			}
			var result CredentialActionResult
			if e = json.Unmarshal(raw, &result); e != nil {
				return nil, e
			}
			return &credentialActionOutput{Body: result}, nil
		})
	}
}
