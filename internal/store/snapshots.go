package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"hoorific/internal/admin"
	"hoorific/internal/catalog"
	"hoorific/internal/core"
)

type configEnvelope struct {
	ExpectedRevision int64           `json:"expected_revision"`
	Config           json.RawMessage `json:"config"`
	Prune            bool            `json:"prune"`
}

// Snapshot reads one tenant's complete runtime graph under one serializable
// transaction, so revision and all resources are from one committed view.
func (s *Store) Snapshot(ctx context.Context) (core.RuntimeSnapshot, error) {
	p, ok := core.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" {
		return core.RuntimeSnapshot{}, problem("unauthorized", 401, "tenant principal required")
	}
	return s.snapshot(ctx, p, true)
}

func (s *Store) snapshot(ctx context.Context, p core.Principal, resolve bool) (core.RuntimeSnapshot, error) {
	out := core.RuntimeSnapshot{Connections: map[string]core.Connection{}, Models: map[string]core.Model{}, Aliases: map[string][]core.RouteTarget{}, RoutePolicies: map[string]core.RoutePolicy{}, AccountPools: map[string]core.AccountPool{}}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		if resolve && p.SubjectID != "" {
			var current core.Principal
			var e error
			if p.KeyID != "" {
				current, e = s.resolveActiveKeyTx(ctx, tx, p.TenantID, p.KeyID, p.KeyRevision)
			} else {
				current, e = s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
			}
			if e != nil {
				return e
			}
			p = current
		}
		var tenantRaw string
		if e := tx.QueryRowContext(ctx, s.Query("SELECT data FROM tenants WHERE id=?"), p.TenantID).Scan(&tenantRaw); e == nil {
			var tenantData admin.TenantData
			if e := strictDecode([]byte(tenantRaw), &tenantData); e != nil {
				return problem("invalid_configuration", 500, "tenant policy invalid")
			}
			out.Policy = core.TenantPolicy{AllowedOrigins: append([]string(nil), tenantData.AllowedOrigins...), MaxBodyBytes: tenantData.MaxBodyBytes, MaxEventBytes: tenantData.MaxEventBytes}
		} else if e != sql.ErrNoRows {
			return e
		}
		var rev int64
		if err := tx.QueryRowContext(ctx, s.Query("SELECT revision FROM config_state WHERE id=1")).Scan(&rev); err != nil {
			return err
		}
		out.Revision = rev
		aliases := map[string]AliasData{}
		routes := map[string]RoutePolicyData{}
		rows, err := tx.QueryContext(ctx, s.Query("SELECT kind,id,version,data FROM resources WHERE tenant_id=? AND kind IN ('connections','models','model_aliases','route_policies','account_pools') ORDER BY kind,id"), p.TenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, id, data string
			var version int64
			if err = rows.Scan(&kind, &id, &version, &data); err != nil {
				return err
			}
			switch kind {
			case "connections":
				var x ConnectionData
				if strictDecode([]byte(data), &x) != nil {
					return problem("invalid_configuration", 500, "connection resource invalid")
				}
				c := core.Connection{TenantID: p.TenantID, ID: id, Version: version, Connector: x.Connector, AccountID: x.AccountID, BaseURL: x.BaseURL, Region: x.Region, Project: x.Project, Dedicated: x.Dedicated, Settings: copyStringMap(x.Settings)}
				if !x.Enabled {
					if c.Settings == nil {
						c.Settings = map[string]string{}
					}
					c.Settings["disabled"] = "true"
				}
				out.Connections[id] = c
			case "models":
				var x ModelData
				if strictDecode([]byte(data), &x) != nil {
					return problem("invalid_configuration", 500, "model resource invalid")
				}
				if !x.Enabled {
					continue
				}
				m := core.Model{CatalogID: id, ID: x.UpstreamID, ConnectionID: x.ConnectionID, Version: version, InputModalities: append([]string(nil), x.InputModalities...), OutputModalities: append([]string(nil), x.OutputModalities...), Provenance: x.Provenance, Price: clonePrice(x.Price)}
				for _, op := range x.Operations {
					m.Operations = append(m.Operations, core.Operation(op))
				}
				m.Features = map[string]core.Support{}
				for k, v := range x.Features {
					m.Features[k] = core.Support(v)
				}
				if x.ContextLimit != nil {
					v := *x.ContextLimit
					m.ContextLimit = &v
				}
				if x.OutputLimit != nil {
					v := *x.OutputLimit
					m.OutputLimit = &v
				}
				out.Models[id] = m
			case "model_aliases":
				var x AliasData
				if strictDecode([]byte(data), &x) != nil {
					return problem("invalid_configuration", 500, "alias resource invalid")
				}
				if !x.Enabled {
					continue
				}
				aliases[id] = x
			case "route_policies":
				var x RoutePolicyData
				if strictDecode([]byte(data), &x) != nil {
					return problem("invalid_configuration", 500, "route policy invalid")
				}
				routes[id] = x
			case "account_pools":
				var x AccountData
				if strictDecode([]byte(data), &x) != nil {
					return problem("invalid_configuration", 500, "account pool invalid")
				}
				out.AccountPools[id] = core.AccountPool{Provider: x.Provider, AccountIDs: append([]string(nil), x.AccountIDs...)}
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		for alias, x := range aliases {
			for _, mid := range x.ModelIDs {
				m, ok := out.Models[mid]
				if !ok {
					continue
				}
				if c, ok := out.Connections[m.ConnectionID]; !ok || c.Settings["disabled"] == "true" {
					continue
				}
				out.Aliases[alias] = append(out.Aliases[alias], core.RouteTarget{ConnectionID: m.ConnectionID, ModelID: mid, Priority: 0, Weight: 1})
			}
		}
		for id, r := range routes {
			if _, ok := aliases[r.Alias]; !ok {
				continue
			}
			out.RoutePolicies[r.Alias] = core.RoutePolicy{Residency: append([]string(nil), r.Residency...), Fallback: r.Fallback, AccountPoolID: r.AccountPoolID, Affinity: r.Affinity}
			out.Aliases[r.Alias] = nil
			for _, t := range r.Targets {
				m, ok := out.Models[t.ModelID]
				if !ok || m.ConnectionID != t.ConnectionID {
					continue
				}
				c, ok := out.Connections[t.ConnectionID]
				if !ok || c.Settings["disabled"] == "true" {
					continue
				}
				out.Aliases[r.Alias] = append(out.Aliases[r.Alias], core.RouteTarget{ConnectionID: t.ConnectionID, ModelID: t.ModelID, Priority: t.Priority, Weight: t.Weight, Region: t.Region})
			}
			_ = id
		}
		for id, m := range out.Models {
			if m.ConnectionID == "" {
				return problem("invalid_configuration", 500, "model connection required")
			}
			c, ok := out.Connections[m.ConnectionID]
			if !ok {
				return problem("invalid_configuration", 500, "model connection invalid")
			}
			if c.Settings["disabled"] == "true" {
				delete(out.Models, id)
			}
		}
		return nil
	})
	return out, err
}

// ValidateRuntime checks every persisted tenant graph before readiness. It is
// intentionally uncached and never mutates configuration.
func (s *Store) ValidateRuntime(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, s.Query("SELECT DISTINCT tenant_id FROM resources ORDER BY tenant_id"))
	if err != nil {
		return err
	}
	var tenants []string
	for rows.Next() {
		var tenant string
		if err := rows.Scan(&tenant); err != nil {
			rows.Close()
			return err
		}
		tenants = append(tenants, tenant)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, tenant := range tenants {
		p := core.Principal{TenantID: tenant, SubjectID: "startup", Role: "owner"}
		snap, err := s.snapshot(ctx, p, false)
		if err != nil {
			return err
		}
		if _, err := catalog.Compile(snap); err != nil {
			return problem("invalid_configuration", 500, err.Error())
		}
	}
	return nil
}

func copyStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
func clonePrice(src *core.PriceSchedule) *core.PriceSchedule {
	if src == nil {
		return nil
	}
	dst := *src
	if src.InputPerMillion != nil {
		v := *src.InputPerMillion
		dst.InputPerMillion = &v
	}
	if src.OutputPerMillion != nil {
		v := *src.OutputPerMillion
		dst.OutputPerMillion = &v
	}
	if src.CachedInputPerMillion != nil {
		v := *src.CachedInputPerMillion
		dst.CachedInputPerMillion = &v
	}
	if src.CacheWriteInputPerMillion != nil {
		v := *src.CacheWriteInputPerMillion
		dst.CacheWriteInputPerMillion = &v
	}
	if src.CacheWrite5mPerMillion != nil {
		v := *src.CacheWrite5mPerMillion
		dst.CacheWrite5mPerMillion = &v
	}
	if src.CacheWrite1hPerMillion != nil {
		v := *src.CacheWrite1hPerMillion
		dst.CacheWrite1hPerMillion = &v
	}
	if src.MaximumUnitCost != nil {
		v := *src.MaximumUnitCost
		dst.MaximumUnitCost = &v
	}
	return &dst
}

// ApplyConfig atomically replaces the tenant runtime graph after candidate validation.
func (s *Store) ApplyConfig(ctx context.Context, p core.Principal, expected int64, config json.RawMessage, prune bool) (int64, error) {
	var candidate struct {
		Connections   map[string]json.RawMessage `json:"connections"`
		Models        map[string]json.RawMessage `json:"models"`
		ModelAliases  map[string]json.RawMessage `json:"model_aliases"`
		RoutePolicies map[string]json.RawMessage `json:"route_policies"`
		PolicyLimits  map[string]json.RawMessage `json:"policy_limits"`
		AccountPools  map[string]json.RawMessage `json:"account_pools"`
		Prices        map[string]json.RawMessage `json:"prices"`
	}
	d := json.NewDecoder(bytes.NewReader(config))
	d.DisallowUnknownFields()
	if err := d.Decode(&candidate); err != nil {
		return 0, problem("invalid_configuration", 400, "invalid configuration")
	}
	if candidate.Connections == nil {
		candidate.Connections = map[string]json.RawMessage{}
	}
	if candidate.Models == nil {
		candidate.Models = map[string]json.RawMessage{}
	}
	if candidate.ModelAliases == nil {
		candidate.ModelAliases = map[string]json.RawMessage{}
	}
	if candidate.RoutePolicies == nil {
		candidate.RoutePolicies = map[string]json.RawMessage{}
	}
	if candidate.PolicyLimits == nil {
		candidate.PolicyLimits = map[string]json.RawMessage{}
	}
	if candidate.AccountPools == nil {
		candidate.AccountPools = map[string]json.RawMessage{}
	}
	if candidate.Prices == nil {
		candidate.Prices = map[string]json.RawMessage{}
	}
	type item struct {
		k, id string
		data  json.RawMessage
	}
	items := []item{}
	for id, v := range candidate.Connections {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid connection identity")
		}
		if _, e := validateResource("connections", v); e != nil {
			return 0, e
		}
		items = append(items, item{"connections", id, v})
	}
	for id, v := range candidate.Models {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid model identity")
		}
		if _, e := validateResource("models", v); e != nil {
			return 0, e
		}
		items = append(items, item{"models", id, v})
	}
	for id, v := range candidate.ModelAliases {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid alias identity")
		}
		if _, e := validateResource("model_aliases", v); e != nil {
			return 0, e
		}
		items = append(items, item{"model_aliases", id, v})
	}
	for id, v := range candidate.RoutePolicies {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid route identity")
		}
		if _, e := validateResource("route_policies", v); e != nil {
			return 0, e
		}
		items = append(items, item{"route_policies", id, v})
	}
	for id, v := range candidate.PolicyLimits {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid policy identity")
		}
		if _, e := validateResource("policy_limits", v); e != nil {
			return 0, e
		}
		items = append(items, item{"policy_limits", id, v})
	}
	for id, v := range candidate.AccountPools {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid account-pool identity")
		}
		if _, e := validateResource("account_pools", v); e != nil {
			return 0, e
		}
		items = append(items, item{"account_pools", id, v})
	}
	for id, v := range candidate.Prices {
		if !validRef(id) {
			return 0, problem("invalid_configuration", 400, "invalid price identity")
		}
		if _, e := validateResource("prices", v); e != nil {
			return 0, e
		}
		items = append(items, item{"prices", id, v})
	}
	var next int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolvePrincipalTx(ctx, tx, p)
		if e != nil {
			return e
		}
		if e = authorize(current, "connections", true); e != nil {
			return e
		}
		p = current
		var cur int64
		if e = tx.QueryRowContext(ctx, s.Query("SELECT revision FROM config_state WHERE id=1")).Scan(&cur); e != nil {
			return e
		}
		if cur != expected {
			return storeError("version_conflict", 412)
		}
		if prune {
			if _, e := tx.ExecContext(ctx, s.Query("DELETE FROM policy_limits WHERE tenant_id=?"), p.TenantID); e != nil {
				return e
			}
			for _, kind := range []string{"connections", "models", "model_aliases", "route_policies", "policy_limits", "account_pools", "prices"} {
				if _, e := tx.ExecContext(ctx, s.Query("DELETE FROM resources WHERE tenant_id=? AND kind=?"), p.TenantID, kind); e != nil {
					return e
				}
			}
		}
		for _, it := range items {
			if it.k == "policy_limits" {
				if e := s.syncPolicyLimit(ctx, tx, p, core.Mutation{Kind: it.k, ID: it.id, Data: it.data}); e != nil {
					return e
				}
			}
			if _, e := tx.ExecContext(ctx, s.Query("INSERT INTO resources(tenant_id,kind,id,version,data) VALUES(?,?,?,?,?) ON CONFLICT(tenant_id,kind,id) DO UPDATE SET version=resources.version+1,data=excluded.data"), p.TenantID, it.k, it.id, 1, string(it.data)); e != nil {
				return e
			}
		}
		if e := s.validateTenantTx(ctx, tx, p.TenantID); e != nil {
			return e
		}
		if e := s.bumpRevisionTx(ctx, tx); e != nil {
			return e
		}
		if e := s.auditTx(ctx, tx, p, "config", "tenant", "apply", cur+1); e != nil {
			return e
		}
		next = cur + 1
		return nil
	})
	if err == nil {
		s.notifyConfig(p.TenantID, next)
	}
	return next, err
}

// ExportConfig returns only non-secret runtime resources and their revision.
func (s *Store) ExportConfig(ctx context.Context, p core.Principal) (json.RawMessage, error) {
	if err := authorize(p, "connections", false); err != nil {
		return nil, err
	}
	snap, err := s.Snapshot(core.WithPrincipal(ctx, p))
	if err != nil {
		return nil, err
	}
	return json.Marshal(snap)
}
func (s *Store) RetainAudit(ctx context.Context, p core.Principal, keep int) error {
	if keep < 1 {
		return storeError("forbidden", 403)
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		current, err := s.resolvePrincipalTx(ctx, tx, p)
		if err != nil {
			return err
		}
		if err = authorize(current, "config", true); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.Query("DELETE FROM audit_events WHERE tenant_id=? AND id NOT IN (SELECT id FROM audit_events WHERE tenant_id=? ORDER BY created_at DESC LIMIT ?)"), current.TenantID, current.TenantID, keep)
		return err
	})
}

var _ core.SnapshotSource = (*Store)(nil)
