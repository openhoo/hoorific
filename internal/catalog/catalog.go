// Package catalog compiles a runtime snapshot into immutable lookup data.
package catalog

import (
	"fmt"
	"hoorific/internal/core"
)

type Catalog struct {
	revision           int64
	policy             core.TenantPolicy
	connections        map[string]core.Connection
	models             map[string]core.Model
	modelsByConnection map[[2]string]core.Model
	uniqueModels       map[string]core.Model
	ambiguousModels    map[string]bool
	aliases            map[string][]core.RouteTarget
	routePolicies      map[string]core.RoutePolicy
	accountPools       map[string]core.AccountPool
}

func Compile(s core.RuntimeSnapshot) (*Catalog, error) {
	if s.Revision < 0 {
		return nil, fmt.Errorf("negative snapshot revision")
	}
	c := &Catalog{
		revision:           s.Revision,
		policy:             cloneTenantPolicy(s.Policy),
		connections:        make(map[string]core.Connection, len(s.Connections)),
		models:             make(map[string]core.Model, len(s.Models)),
		modelsByConnection: make(map[[2]string]core.Model, len(s.Models)),
		uniqueModels:       make(map[string]core.Model),
		ambiguousModels:    make(map[string]bool),
		aliases:            make(map[string][]core.RouteTarget, len(s.Aliases)),
		routePolicies:      make(map[string]core.RoutePolicy, len(s.RoutePolicies)),
		accountPools:       make(map[string]core.AccountPool, len(s.AccountPools)),
	}
	for key, value := range s.Connections {
		if key == "" || value.ID == "" || key != value.ID {
			return nil, fmt.Errorf("connection key and ID must match")
		}
		if _, ok := c.connections[key]; ok {
			return nil, fmt.Errorf("duplicate connection %q", key)
		}
		c.connections[key] = cloneConnection(value)
	}
	for key, value := range s.Models {
		if key == "" || value.CatalogID == "" || key != value.CatalogID || value.ID == "" {
			return nil, fmt.Errorf("model identity is invalid")
		}
		if value.ConnectionID == "" {
			return nil, fmt.Errorf("model %q has no connection", key)
		}
		connection, ok := c.connections[value.ConnectionID]
		if !ok {
			return nil, fmt.Errorf("model %q references unknown connection", key)
		}
		if connection.Settings["disabled"] == "true" {
			return nil, fmt.Errorf("model %q references disabled connection", key)
		}
		if value.ContextLimit != nil && *value.ContextLimit < 0 {
			return nil, fmt.Errorf("model %q has negative context limit", key)
		}
		if value.OutputLimit != nil && *value.OutputLimit < 0 {
			return nil, fmt.Errorf("model %q has negative output limit", key)
		}
		for feature, support := range value.Features {
			if feature == "" || (support != core.Supported && support != core.Unsupported && support != core.Unknown) {
				return nil, fmt.Errorf("model %q has invalid feature declaration", key)
			}
		}
		if value.Price != nil {
			if value.Price.Version == "" {
				return nil, fmt.Errorf("model %q has invalid price version", key)
			}
			for _, rate := range []*int64{
				value.Price.InputPerMillion,
				value.Price.OutputPerMillion,
				value.Price.CachedInputPerMillion,
				value.Price.CacheWriteInputPerMillion,
				value.Price.CacheWrite5mPerMillion,
				value.Price.CacheWrite1hPerMillion,
				value.Price.MaximumUnitCost,
			} {
				if rate != nil && *rate < 0 {
					return nil, fmt.Errorf("model %q has negative price bound", key)
				}
			}
		}
		model := cloneModel(value)
		pair := [2]string{value.ConnectionID, value.CatalogID}
		if _, ok := c.modelsByConnection[pair]; ok {
			return nil, fmt.Errorf("duplicate model %q on connection %q", value.CatalogID, value.ConnectionID)
		}
		c.models[key] = model
		c.modelsByConnection[pair] = model
		if _, ok := c.uniqueModels[value.CatalogID]; ok {
			c.ambiguousModels[value.CatalogID] = true
			delete(c.uniqueModels, value.CatalogID)
		} else if !c.ambiguousModels[value.CatalogID] {
			c.uniqueModels[value.CatalogID] = model
		}
	}
	for alias, targets := range s.Aliases {
		if alias == "" {
			return nil, fmt.Errorf("empty alias")
		}
		copied := make([]core.RouteTarget, len(targets))
		seen := make(map[[2]string]bool, len(targets))
		for i, target := range targets {
			model, ok := c.modelsByConnection[[2]string{target.ConnectionID, target.ModelID}]
			if !ok || model.ConnectionID != target.ConnectionID {
				return nil, fmt.Errorf("alias %q references incompatible model", alias)
			}
			connection := c.connections[target.ConnectionID]
			if connection.Settings["disabled"] == "true" {
				return nil, fmt.Errorf("alias %q references disabled connection", alias)
			}
			if target.Priority < 0 || target.Weight <= 0 {
				return nil, fmt.Errorf("alias %q has invalid priority or weight", alias)
			}
			if target.Region != "" && connection.Region != "" && target.Region != connection.Region {
				return nil, fmt.Errorf("alias %q target region mismatches connection", alias)
			}
			pair := [2]string{target.ConnectionID, target.ModelID}
			if seen[pair] {
				return nil, fmt.Errorf("alias %q has duplicate target", alias)
			}
			seen[pair] = true
			copied[i] = target
		}
		c.aliases[alias] = copied
	}
	for id, pool := range s.AccountPools {
		if id == "" || pool.Provider == "" {
			return nil, fmt.Errorf("account pool %q identity is invalid", id)
		}
		if len(pool.AccountIDs) == 0 {
			return nil, fmt.Errorf("account pool %q has no accounts", id)
		}
		seen := make(map[string]bool, len(pool.AccountIDs))
		for _, account := range pool.AccountIDs {
			if account == "" || seen[account] {
				return nil, fmt.Errorf("account pool %q has invalid account membership", id)
			}
			seen[account] = true
		}
		c.accountPools[id] = cloneAccountPool(pool)
	}
	for alias, policy := range s.RoutePolicies {
		targets, ok := c.aliases[alias]
		if !ok {
			return nil, fmt.Errorf("route policy %q references unknown alias", alias)
		}
		for _, residency := range policy.Residency {
			if residency == "" {
				return nil, fmt.Errorf("route policy %q has invalid residency", alias)
			}
		}
		if policy.AccountPoolID != "" {
			pool, ok := c.accountPools[policy.AccountPoolID]
			if !ok {
				return nil, fmt.Errorf("route policy %q references unknown account pool", alias)
			}
			for _, target := range targets {
				connection := c.connections[target.ConnectionID]
				if (pool.Provider != connection.Connector && pool.Provider != connection.Settings["provider"]) || !containsString(pool.AccountIDs, connection.AccountID) {
					return nil, fmt.Errorf("route policy %q account pool does not contain target account", alias)
				}
			}
		}
		regions := make([]string, 0, len(targets))
		for _, target := range targets {
			connection := c.connections[target.ConnectionID]
			region := target.Region
			if region == "" {
				region = connection.Region
			}
			regions = append(regions, region)
		}
		for _, targetRegion := range regions {
			if len(policy.Residency) > 0 && !containsString(policy.Residency, targetRegion) {
				return nil, fmt.Errorf("route policy %q residency does not match target region", alias)
			}
		}
		c.routePolicies[alias] = cloneRoutePolicy(policy)
	}
	return c, nil
}
func (c *Catalog) Revision() int64 { return c.revision }
func (c *Catalog) Connection(id string) (core.Connection, bool) {
	v, ok := c.connections[id]
	if !ok {
		return core.Connection{}, false
	}
	return cloneConnection(v), true
}
func (c *Catalog) Model(id string) (core.Model, bool) {
	v, ok := c.models[id]
	if ok {
		return cloneModel(v), true
	}
	v, ok = c.uniqueModels[id]
	if !ok {
		return core.Model{}, false
	}
	return cloneModel(v), true
}
func (c *Catalog) ModelFor(connectionID, modelID string) (core.Model, bool) {
	v, ok := c.modelsByConnection[[2]string{connectionID, modelID}]
	if !ok {
		return core.Model{}, false
	}
	return cloneModel(v), true
}
func (c *Catalog) Targets(alias string) []core.RouteTarget {
	v := c.aliases[alias]
	return append([]core.RouteTarget(nil), v...)
}
func (c *Catalog) Snapshot() core.RuntimeSnapshot {
	s := core.RuntimeSnapshot{Revision: c.revision, Policy: cloneTenantPolicy(c.policy), Connections: make(map[string]core.Connection, len(c.connections)), Models: make(map[string]core.Model, len(c.models)), Aliases: make(map[string][]core.RouteTarget, len(c.aliases))}
	s.RoutePolicies = make(map[string]core.RoutePolicy, len(c.routePolicies))
	s.AccountPools = make(map[string]core.AccountPool, len(c.accountPools))
	for k, v := range c.routePolicies {
		s.RoutePolicies[k] = cloneRoutePolicy(v)
	}
	for k, v := range c.accountPools {
		s.AccountPools[k] = cloneAccountPool(v)
	}
	for k, v := range c.connections {
		s.Connections[k] = cloneConnection(v)
	}
	for k, v := range c.models {
		s.Models[k] = cloneModel(v)
	}
	for k, v := range c.aliases {
		s.Aliases[k] = append([]core.RouteTarget(nil), v...)
	}
	return s
}

func cloneConnection(v core.Connection) core.Connection {
	v.Settings = copyStrings(v.Settings)
	return v
}
func cloneModel(v core.Model) core.Model {
	v.Operations = append([]core.Operation(nil), v.Operations...)
	v.InputModalities = append([]string(nil), v.InputModalities...)
	v.OutputModalities = append([]string(nil), v.OutputModalities...)
	v.Features = copySupport(v.Features)
	if v.ContextLimit != nil {
		x := *v.ContextLimit
		v.ContextLimit = &x
	}
	if v.OutputLimit != nil {
		x := *v.OutputLimit
		v.OutputLimit = &x
	}
	v.Price = clonePrice(v.Price)
	return v
}
func clonePrice(src *core.PriceSchedule) *core.PriceSchedule {
	if src == nil {
		return nil
	}
	dst := *src
	if src.InputPerMillion != nil {
		x := *src.InputPerMillion
		dst.InputPerMillion = &x
	}
	if src.OutputPerMillion != nil {
		x := *src.OutputPerMillion
		dst.OutputPerMillion = &x
	}
	if src.CachedInputPerMillion != nil {
		x := *src.CachedInputPerMillion
		dst.CachedInputPerMillion = &x
	}
	if src.CacheWriteInputPerMillion != nil {
		x := *src.CacheWriteInputPerMillion
		dst.CacheWriteInputPerMillion = &x
	}
	if src.CacheWrite5mPerMillion != nil {
		x := *src.CacheWrite5mPerMillion
		dst.CacheWrite5mPerMillion = &x
	}
	if src.CacheWrite1hPerMillion != nil {
		x := *src.CacheWrite1hPerMillion
		dst.CacheWrite1hPerMillion = &x
	}
	if src.MaximumUnitCost != nil {
		x := *src.MaximumUnitCost
		dst.MaximumUnitCost = &x
	}
	return &dst
}
func copyStrings(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	n := make(map[string]string, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}
func copySupport(m map[string]core.Support) map[string]core.Support {
	if m == nil {
		return nil
	}
	n := make(map[string]core.Support, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}
func cloneRoutePolicy(v core.RoutePolicy) core.RoutePolicy {
	if v.Residency != nil {
		v.Residency = append([]string{}, v.Residency...)
	}
	return v
}
func cloneAccountPool(v core.AccountPool) core.AccountPool {
	if v.AccountIDs != nil {
		v.AccountIDs = append([]string{}, v.AccountIDs...)
	}
	return v
}
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func cloneTenantPolicy(v core.TenantPolicy) core.TenantPolicy {
	v.AllowedOrigins = append([]string(nil), v.AllowedOrigins...)
	return v
}
