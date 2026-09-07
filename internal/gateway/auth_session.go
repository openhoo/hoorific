package gateway

import (
	"context"
	"net/http"
	"strings"

	"hoorific/internal/core"
)

func validateIngressHeaders(r *http.Request) error {
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-hoorific-") {
			return failure("forbidden", 403, "caller-supplied gateway claims are forbidden")
		}
		switch lower {
		case "x-tenant", "x-tenant-id", "x-scope", "x-scopes", "x-role", "x-roles", "x-key-id":
			return failure("forbidden", 403, "caller-supplied authorization claims are forbidden")
		}
	}
	return nil
}

// Only the management server injects session principals. SQL admission checks
// the current session and operator role again atomically before dispatch.
func (g *Gateway) sessionGrants(ctx context.Context, p core.Principal) (core.Principal, error) {
	switch p.Role {
	case "owner", "admin", "operator":
	default:
		return p, failure("forbidden", 403, "operator role cannot execute playground")
	}
	s, err := g.deps.Snapshots.Snapshot(core.WithPrincipal(ctx, p))
	if err != nil {
		return p, err
	}
	p.Portable = true
	p.NativeAccount = true
	p.Realtime = true
	p.Aliases = nil
	p.Connections = nil
	p.Operations = nil
	for alias := range s.Aliases {
		p.Aliases = append(p.Aliases, alias)
	}
	operations := map[core.Operation]bool{}
	for id, c := range s.Connections {
		if c.TenantID != p.TenantID {
			continue
		}
		p.Connections = append(p.Connections, id)
		if connector := g.deps.Connectors[c.Connector]; connector != nil {
			descriptor := connector.Descriptor()
			if configured, ok := connector.(core.ConnectionDescriptor); ok {
				descriptor, err = configured.DescriptorFor(c)
				if err != nil {
					return p, err
				}
			}
			for _, op := range descriptor.Operations {
				operations[op] = true
			}
		}
	}
	for op := range operations {
		p.Operations = append(p.Operations, op)
	}
	return p, nil
}
func (g *Gateway) Ready() bool { return !g.draining.Load() }
