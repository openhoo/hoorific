package core

import (
	"context"
	"time"
)

// HealthScope binds a disposable routing hint to the authenticated tenant and
// the immutable configuration snapshot that selected the target. Revision is
// the process-wide configuration counter; it intentionally advances for a
// mutation in any tenant, while hint reads and writes remain tenant-scoped.
// This broad invalidation is safe and prevents a target from an older
// connection or credential generation from poisoning a newer snapshot.
type HealthScope struct {
	TenantID string
	Revision int64
}

func (s HealthScope) Valid() bool { return s.TenantID != "" && s.Revision >= 0 }

// ScopedRoutingHints is the explicit, request-path adapter for disposable
// health and affinity hints. Implementations must fail open on invalid or
// unavailable reads and must reject writes whose scope is invalid; they must
// never write an unscoped hint.
type ScopedRoutingHints interface {
	HealthyScoped(context.Context, HealthScope, RouteTarget) (bool, error)
	SetScoped(context.Context, HealthScope, RouteTarget, bool, time.Duration) error
	GetAffinityScoped(context.Context, HealthScope, string) (RouteTarget, bool, error)
	SetAffinityScoped(context.Context, HealthScope, string, RouteTarget, time.Duration) error
}
