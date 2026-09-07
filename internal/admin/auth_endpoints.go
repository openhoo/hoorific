package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"hoorific/internal/core"
)

type SessionData struct {
	Principal   core.Principal     `json:"principal"`
	Permissions []string           `json:"permissions"`
	CSRFToken   string             `json:"csrf_token,omitempty"`
	Tenants     []TenantMembership `json:"tenants"`
}
type SessionOutput struct{ Body SessionData }
type sessionTenantInput struct {
	Body struct {
		TenantID string `json:"tenant_id"`
	}
}
type sessionTenantOutput struct{ Body SessionData }
type adminTokenIssueInput struct {
	Body struct {
		Subject     string    `json:"subject"`
		Permissions []string  `json:"permissions"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
}
type adminTokenIssueOutput struct{ Body adminTokenIssueData }
type adminTokenIssueData struct {
	TokenHash   string    `json:"token_hash"`
	Secret      string    `json:"secret" writeOnly:"true"`
	Subject     string    `json:"subject"`
	Permissions []string  `json:"permissions"`
	ExpiresAt   time.Time `json:"expires_at"`
}
type adminTokenRevokeInput struct {
	Hash string `path:"hash"`
}

func sessionData(ctx context.Context, s *Server, p core.Principal, secret string) (SessionData, error) {
	tenants, err := s.deps.Auth.AvailableTenants(ctx, p)
	if err != nil {
		return SessionData{}, err
	}
	out := SessionData{Principal: p, Permissions: permissionsFor(p), Tenants: tenants}
	if secret != "" {
		out.CSRFToken = csrfToken(secret)
	}
	return out, nil
}

func (s *Server) registerHumaAuth(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "session", Method: "GET", Path: "/admin/api/v1/session"}, func(ctx context.Context, _ *struct{}) (*SessionOutput, error) {
		r := requestFromContext(ctx)
		if r == nil {
			return nil, errors.New("authentication required")
		}
		p, err := s.authenticate(r)
		if err != nil {
			return nil, err
		}
		_, bearer, err := bearerToken(r)
		if err != nil {
			return nil, err
		}
		secret := ""
		if !bearer {
			_, secret, err = s.resolveSession(r)
			if err != nil {
				return nil, err
			}
		}
		data, err := sessionData(ctx, s, p, secret)
		if err != nil {
			return nil, err
		}
		return &SessionOutput{Body: data}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "selectSessionTenant", Method: "POST", Path: "/admin/api/v1/session/tenant"}, func(ctx context.Context, in *sessionTenantInput) (*sessionTenantOutput, error) {
		r := requestFromContext(ctx)
		if r == nil {
			return nil, errors.New("authentication required")
		}
		if _, present, err := bearerToken(r); err != nil || present {
			return nil, errors.New("tenant selection requires cookie session")
		}
		if err := s.checkMutation(r); err != nil {
			return nil, err
		}
		session, secret, err := s.resolveSession(r)
		if err != nil {
			return nil, err
		}
		tenantID := strings.TrimSpace(in.Body.TenantID)
		if tenantID == "" {
			return nil, errors.New("tenant_id is required")
		}
		selected, err := s.deps.Auth.SelectSessionTenant(ctx, session.Hash, tenantID)
		if err != nil {
			return nil, runtimeRepositoryError(err)
		}
		if selected.Hash != session.Hash || !selected.ExpiresAt.After(time.Now()) || !validPrincipal(selected.Principal) {
			return nil, errors.New("tenant selection rejected")
		}
		selected.Principal.SessionID = authHash(secret)
		data, err := sessionData(ctx, s, selected.Principal, secret)
		if err != nil {
			return nil, err
		}
		return &sessionTenantOutput{Body: data}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "issueAdminToken", Method: "POST", Path: "/admin/api/v1/auth/tokens"}, func(ctx context.Context, in *adminTokenIssueInput) (*adminTokenIssueOutput, error) {
		r := requestFromContext(ctx)
		if r == nil {
			return nil, errors.New("authentication required")
		}
		if _, present, e := bearerToken(r); e != nil || present {
			return nil, errors.New("token issuance requires cookie owner session")
		}
		p, err := s.authenticate(r)
		if err != nil {
			return nil, err
		}
		if p.Role != "owner" || !hasPermission(p, "session:read") {
			return nil, errors.New("owner permission required")
		}
		if err := s.checkMutation(r); err != nil {
			return nil, err
		}
		subject := strings.TrimSpace(in.Body.Subject)
		if subject == "" {
			return nil, errors.New("subject is required")
		}
		if len(in.Body.Permissions) == 0 {
			return nil, errors.New("permissions are required")
		}
		scopes := make([]string, 0, len(in.Body.Permissions))
		seen := make(map[string]struct{}, len(scopes))
		for _, scope := range in.Body.Permissions {
			scope = strings.TrimSpace(scope)
			if scope == "" {
				return nil, errors.New("permissions must not be empty")
			}
			if _, ok := seen[scope]; ok {
				return nil, errors.New("permissions must be unique")
			}
			seen[scope] = struct{}{}
			scopes = append(scopes, scope)
		}
		if in.Body.ExpiresAt.IsZero() || !in.Body.ExpiresAt.After(time.Now()) {
			return nil, errors.New("expires_at must be in the future")
		}
		secret, err := s.deps.Auth.IssueAdminToken(ctx, p, subject, scopes, in.Body.ExpiresAt)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(secret))
		hash := hex.EncodeToString(sum[:])
		return &adminTokenIssueOutput{Body: adminTokenIssueData{TokenHash: hash, Secret: secret, Subject: subject, Permissions: scopes, ExpiresAt: in.Body.ExpiresAt}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "revokeAdminToken", Method: "DELETE", Path: "/admin/api/v1/auth/tokens/{hash}"}, func(ctx context.Context, in *adminTokenRevokeInput) (*struct{}, error) {
		r := requestFromContext(ctx)
		if r == nil {
			return nil, errors.New("authentication required")
		}
		if _, present, e := bearerToken(r); e != nil || present {
			return nil, errors.New("token revocation requires cookie owner session")
		}
		p, err := s.authenticate(r)
		if err != nil {
			return nil, err
		}
		if p.Role != "owner" || !hasPermission(p, "session:read") {
			return nil, errors.New("owner permission required")
		}
		if err := s.checkMutation(r); err != nil {
			return nil, err
		}
		if len(in.Hash) != 64 {
			return nil, errors.New("token hash is required")
		}
		if _, err := hex.DecodeString(in.Hash); err != nil {
			return nil, errors.New("token hash is invalid")
		}
		if err := s.deps.Auth.RevokeAdminToken(ctx, p, strings.ToLower(in.Hash)); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})
}
