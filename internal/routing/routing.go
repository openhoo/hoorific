// Package routing selects explicitly declared compatible targets.
package routing

import (
	"context"
	"crypto/rand"
	"fmt"
	"hoorific/internal/catalog"
	"hoorific/internal/core"
	"hoorific/internal/policy"
	"math/big"
	"sort"
)

type Requirements struct {
	Operation                 core.Operation
	Features                  []string
	InputModalities           []string
	OutputModalities          []string
	InputTokens, OutputTokens *int64
	Region                    string
	// Preferred is a disposable affinity hint, never an eligibility override.
	Preferred *core.RouteTarget
}
type Health interface {
	HealthyScoped(context.Context, core.HealthScope, core.RouteTarget) (bool, error)
}

// TargetScope resolves the tenant and immutable snapshot generation for a
// route target. The global configuration revision intentionally acts as the
// connection/account/credential generation: every committed mutation advances
// it, while health data remains keyed by the target's tenant.
func TargetScope(s core.RuntimeSnapshot, target core.RouteTarget) (core.HealthScope, bool) {
	if s.Revision < 0 || target.ConnectionID == "" || target.ModelID == "" {
		return core.HealthScope{}, false
	}
	connection, ok := s.Connections[target.ConnectionID]
	if !ok || connection.ID != target.ConnectionID || connection.TenantID == "" {
		return core.HealthScope{}, false
	}
	model, ok := s.Models[target.ModelID]
	if !ok || model.ConnectionID != target.ConnectionID {
		return core.HealthScope{}, false
	}
	return core.HealthScope{TenantID: connection.TenantID, Revision: s.Revision}, true
}

type Router struct {
	catalog *catalog.Catalog
	health  Health
}

func New(c *catalog.Catalog, h Health) (*Router, error) {
	if c == nil {
		return nil, fmt.Errorf("catalog is required")
	}
	return &Router{catalog: c, health: h}, nil
}
func (r *Router) Select(ctx context.Context, alias string, req Requirements) (core.ModelCall, error) {
	s := r.catalog.Snapshot()
	targets, err := Order(ctx, s, alias, req, r.health)
	if err != nil {
		return core.ModelCall{}, err
	}
	t := targets[0]
	return core.ModelCall{Connection: s.Connections[t.ConnectionID], Model: s.Models[t.ModelID], Alias: alias}, nil
}

// Order returns a weighted permutation of eligible targets, strictly grouped by
// priority. With fallback disabled it returns only the selected target. Callers
// must obtain a fresh SQL permit before each attempt, regardless of these hints.
func Order(ctx context.Context, s core.RuntimeSnapshot, alias string, req Requirements, health Health) ([]core.RouteTarget, error) {
	if alias == "" || req.Operation == "" {
		return nil, fmt.Errorf("alias and operation are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := s.RoutePolicies[alias]
	eligible := make([]core.RouteTarget, 0, len(s.Aliases[alias]))
	for _, t := range s.Aliases[alias] {
		c, ok := s.Connections[t.ConnectionID]
		if !ok || c.Settings["disabled"] == "true" {
			continue
		}
		m, ok := s.Models[t.ModelID]
		if !ok || m.ConnectionID != t.ConnectionID || !supports(m, req) {
			continue
		}
		region := t.Region
		if region == "" {
			region = c.Region
		}
		if t.Region != "" && c.Region != "" && t.Region != c.Region {
			continue
		}
		if req.Region != "" && region != req.Region {
			continue
		}
		if len(p.Residency) > 0 && !contains(p.Residency, region) {
			continue
		}
		if p.AccountPoolID != "" {
			pool, ok := s.AccountPools[p.AccountPoolID]
			if !ok || pool.Provider != c.Connector || !contains(pool.AccountIDs, c.AccountID) {
				continue
			}
		}
		if t.Priority < 0 || t.Weight <= 0 {
			return nil, fmt.Errorf("invalid route priority or weight")
		}
		if health != nil {
			scope, valid := TargetScope(s, t)
			healthy := true
			var err error
			if valid {
				healthy, err = health.HealthyScoped(ctx, scope, t)
			}
			// A malformed scope cannot safely read a hint. Treat the
			// disposable signal as absent rather than suppressing a target.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err == nil && !healthy {
				continue
			}
		}
		eligible = append(eligible, t)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no healthy compatible target for alias %q", alias)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].Priority < eligible[j].Priority })
	ordered := make([]core.RouteTarget, 0, len(eligible))
	for len(eligible) > 0 {
		end := 1
		for end < len(eligible) && eligible[end].Priority == eligible[0].Priority {
			end++
		}
		tier := eligible[:end:end]
		rest := eligible[end:]
		if len(ordered) == 0 && p.Affinity && req.Preferred != nil {
			for i, t := range tier {
				if same(t, *req.Preferred) {
					ordered = append(ordered, t)
					tier = append(tier[:i], tier[i+1:]...)
					break
				}
			}
		}
		for len(tier) > 0 {
			if len(ordered) > 0 && !p.Fallback {
				return ordered[:1], nil
			}
			i, err := weightedIndex(tier)
			if err != nil {
				return nil, err
			}
			ordered = append(ordered, tier[i])
			tier = append(tier[:i], tier[i+1:]...)
		}
		if !p.Fallback {
			return ordered[:1], nil
		}
		eligible = rest
	}
	return ordered, nil
}
func same(a, b core.RouteTarget) bool {
	return a.ConnectionID == b.ConnectionID && a.ModelID == b.ModelID && a.Region == b.Region
}
func supports(m core.Model, req Requirements) bool {
	if !containsOp(m.Operations, req.Operation) {
		return false
	}
	for _, f := range req.Features {
		if f == "" || m.Features[f] != core.Supported {
			return false
		}
	}
	for _, x := range req.InputModalities {
		if x == "" || !contains(m.InputModalities, x) {
			return false
		}
	}
	for _, x := range req.OutputModalities {
		if x == "" || !contains(m.OutputModalities, x) {
			return false
		}
	}
	if req.InputTokens != nil || req.OutputTokens != nil {
		in, out := int64(0), int64(0)
		if req.InputTokens != nil {
			in = *req.InputTokens
		}
		if req.OutputTokens != nil {
			out = *req.OutputTokens
		}
		if policy.ValidateTokens(in, out, m.ContextLimit, m.OutputLimit) != nil {
			return false
		}
	}
	return true
}
func containsOp(xs []core.Operation, x core.Operation) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func weightedIndex(xs []core.RouteTarget) (int, error) {
	total := int64(0)
	for _, x := range xs {
		if x.Weight <= 0 || int64(x.Weight) > int64(^uint64(0)>>1)-total {
			return 0, fmt.Errorf("route weight overflow")
		}
		total += int64(x.Weight)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(total))
	if err != nil {
		return 0, fmt.Errorf("weighted selection: %w", err)
	}
	pick := n.Int64()
	for i, x := range xs {
		if pick < int64(x.Weight) {
			return i, nil
		}
		pick -= int64(x.Weight)
	}
	return 0, fmt.Errorf("weighted selection exhausted")
}
