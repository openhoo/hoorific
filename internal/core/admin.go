package core

import (
	"context"
	"encoding/json"
	"net/http"
)

// Principal is established by a server-side credential/session lookup.
type Principal struct {
	TenantID, SubjectID, KeyID, Role  string
	SessionID                         string `json:"-"`
	KeyRevision                       int64
	Permissions                       []string
	Aliases, Connections              []string
	Operations                        []Operation
	Portable, NativeAccount, Realtime bool
}
type Resource struct {
	ID       string          `json:"id"`
	TenantID string          `json:"tenant_id"`
	Kind     string          `json:"kind"`
	Version  int64           `json:"version"`
	Data     json.RawMessage `json:"data"`
}
type ResourcePage struct {
	Items      []Resource `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
type Mutation struct {
	Kind, ID        string
	ExpectedVersion int64
	Data            json.RawMessage
	Delete          bool
}
type AdminRepository interface {
	List(context.Context, Principal, string, string, int) (ResourcePage, error)
	Get(context.Context, Principal, string, string) (Resource, error)
	Mutate(context.Context, Principal, Mutation) (Resource, error)
}
type KeyAuthenticator interface {
	AuthenticateKey(context.Context, string) (Principal, error)
}
type TenantPolicy struct {
	AllowedOrigins              []string
	MaxBodyBytes, MaxEventBytes int64
}

// TenantPolicySource is an optional snapshot extension. A zero byte limit
// inherits the process-wide limit; it does not mean unlimited.
type TenantPolicySource interface{ TenantPolicy() TenantPolicy }
type RuntimeSnapshot struct {
	Revision      int64
	Connections   map[string]Connection
	Models        map[string]Model
	Aliases       map[string][]RouteTarget
	RoutePolicies map[string]RoutePolicy
	AccountPools  map[string]AccountPool
	Policy        TenantPolicy
}
type RouteTarget struct {
	ConnectionID, ModelID string
	Priority, Weight      int
	Region                string
}
type RoutePolicy struct {
	Residency     []string
	Fallback      bool
	AccountPoolID string
	Affinity      bool
}
type AccountPool struct {
	Provider   string
	AccountIDs []string
}
type SnapshotSource interface {
	Snapshot(context.Context) (RuntimeSnapshot, error)
}
type CredentialSource interface {
	Lease(context.Context, Connection) (CredentialLease, error)
}

// NativeEndpoint is an explicit, relative upstream operation inventory entry.
type NativeEndpoint struct {
	Method, Path, Action string
	Operation            Operation
	ModelLocation        string
	Framing              Framing
	Stateful             bool
	ResourceIDField      string
}
type EndpointInventory interface{ Endpoints() []NativeEndpoint }

// Reconciliation.Cost is an integer USD nanodollar amount.
type Reconciliation struct {
	ReconciliationID string `json:"reconciliation_id"`
	Mode             string `json:"mode"`
	Reason           string `json:"reason"`
	SourceReference  string `json:"source_reference,omitempty"`
	Cost             *int64 `json:"cost,omitempty"`
	Usage            *Usage `json:"usage,omitempty"`
}
type ScopeInspector interface {
	Inspect(context.Context, Target, Operation, []byte) error
}
type EndpointBinder interface {
	BindEndpoint(context.Context, Connection, NativeEndpoint, map[string]string) (Binding, error)
}
type AdmissionPlanner interface {
	PlanAttempt(context.Context, Principal, RuntimeSnapshot, Target, Operation, []byte, AttemptPlan) (AttemptPlan, error)
}
type principalContextKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}

type ConnectionInventory interface {
	EndpointsFor(Connection) ([]NativeEndpoint, error)
}
type StreamBinder interface {
	BindStream(context.Context, Target, Operation, bool) (Binding, error)
}
type WireAdapter interface {
	AdaptRequest(context.Context, Target, Binding, []byte) ([]byte, error)
	AdaptResponse(context.Context, Binding, *http.Response) error
}
type StreamDuplex interface {
	ExecuteDuplex(context.Context, Connection, string, CredentialLease, *http.Client, <-chan []byte, func([]byte) error) error
}
