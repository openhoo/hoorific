package admin

import (
	"context"
	"encoding/json"
	"github.com/danielgtaylor/huma/v2"
	"hoorific/internal/core"
	"net/http"
	"time"
)

type Session struct {
	Hash      string
	CSRFHash  string
	Principal core.Principal
	ExpiresAt time.Time
}
type LoginState struct {
	Hash        string
	Nonce       string
	Verifier    string
	RedirectURI string
	ExpiresAt   time.Time
}
type TenantMembership struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}
type AuthStore interface {
	ResolveSession(context.Context, string) (Session, error)
	CreateSession(context.Context, Session) error
	DeleteSession(context.Context, string) error
	ConsumeBootstrap(context.Context, string) (core.Principal, error)
	SaveAdminLogin(context.Context, LoginState) error
	ConsumeAdminLogin(context.Context, string) (LoginState, error)
	ResolveIdentity(context.Context, string, string) (core.Principal, error)
	ResolveAdminToken(context.Context, string) (core.Principal, error)
	AvailableTenants(context.Context, core.Principal) ([]TenantMembership, error)
	SelectSessionTenant(context.Context, string, string) (Session, error)
	IssueAdminToken(context.Context, core.Principal, string, []string, time.Time) (string, error)
	RevokeAdminToken(context.Context, core.Principal, string) error
}
type OIDCConfig struct{ Issuer, ClientID, ClientSecret, RedirectURI string }
type ActionService interface {
	Execute(context.Context, core.Principal, string, string, string, int64, json.RawMessage) (json.RawMessage, error)
}
type Reconciler interface {
	Reconcile(context.Context, core.Principal, string, int64, core.Reconciliation) (core.Resource, error)
}
type Dependencies struct {
	Repository    core.AdminRepository
	Auth          AuthStore
	Actions       ActionService
	Reconciler    Reconciler
	Playground    http.Handler
	OIDC          *OIDCConfig
	PublicOrigin  string
	SecureCookies bool
	Ready         func(context.Context) error
	Metrics       http.Handler
}
type Server struct {
	deps Dependencies
	mux  *http.ServeMux
	api  huma.API
}
