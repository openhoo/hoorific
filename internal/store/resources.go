package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hoorific/internal/core"
	"strings"
	"time"
)

func storeError(code string, status int) error {
	return core.GatewayError{Code: code, HTTPStatus: status, Message: strings.ReplaceAll(code, "_", " ")}
}
func authorize(p core.Principal, kind string, write bool) error {
	if p.TenantID == "" || p.SubjectID == "" {
		return storeError("unauthorized", 401)
	}
	switch p.Role {
	case "owner", "admin":
		return nil
	case "operator":
		if !write {
			return nil
		}
	case "auditor", "viewer":
		if !write {
			return nil
		}
	}
	return storeError("forbidden", 403)
}
func knownKind(k string) bool {
	switch k {
	case "tenants", "operators", "role_bindings", "connections", "models", "model_aliases", "route_policies", "policy_limits", "api_keys", "account_pools", "prices":
		return true
	}
	return false
}
func (s *Store) Get(ctx context.Context, p core.Principal, kind, id string) (core.Resource, error) {
	var r core.Resource
	if operationalKind(kind) {
		page, err := s.readOperationalResources(ctx, p, kind, id, "", 1, true)
		if err != nil {
			return r, err
		}
		return page.Items[0], nil
	}
	if kind == "credentials" {
		page, err := s.readCredentialMetadata(ctx, p, id, "", 1, true)
		if err != nil {
			return r, err
		}
		return page.Items[0], nil
	}
	if tenancyKind(kind) {
		page, e := s.readTenancy(ctx, p, kind, id, "", 1, true)
		if e != nil {
			return r, e
		}
		return page.Items[0], nil
	}
	if !knownKind(kind) {
		return r, storeError("invalid_kind", 400)
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolvePrincipalTx(ctx, tx, p)
		if e != nil {
			return e
		}
		if e = authorize(current, kind, false); e != nil {
			return e
		}
		var data string
		e = tx.QueryRowContext(ctx, s.Query("SELECT version,data FROM resources WHERE tenant_id=? AND kind=? AND id=?"), current.TenantID, kind, id).Scan(&r.Version, &data)
		if errors.Is(e, sql.ErrNoRows) {
			return storeError("not_found", 404)
		}
		if e != nil {
			return e
		}
		r.ID = id
		r.Kind = kind
		r.TenantID = current.TenantID
		r.Data = json.RawMessage(data)
		return nil
	})
	return r, err
}

type resourceCursor struct {
	Tenant, Subject, Role, Kind, After string
	Limit                              int
	Expires                            int64
}

func (s *Store) encodeCursor(c resourceCursor) (string, error) {
	if len(s.CursorKey) < 32 {
		return "", fmt.Errorf("cursor signing key unavailable")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, s.CursorKey)
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}
func (s *Store) List(ctx context.Context, p core.Principal, kind, cursor string, limit int) (core.ResourcePage, error) {
	out := core.ResourcePage{Items: []core.Resource{}}
	if operationalKind(kind) {
		return s.readOperationalResources(ctx, p, kind, "", cursor, limit, false)
	}
	if kind == "credentials" {
		return s.readCredentialMetadata(ctx, p, "", cursor, limit, false)
	}
	if tenancyKind(kind) {
		return s.readTenancy(ctx, p, kind, "", cursor, limit, false)
	}
	if !knownKind(kind) {
		return out, storeError("invalid_kind", 400)
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return out, storeError("invalid_limit", 400)
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolvePrincipalTx(ctx, tx, p)
		if e != nil {
			return e
		}
		if e = authorize(current, kind, false); e != nil {
			return e
		}
		after := ""
		if cursor != "" {
			parts := strings.Split(cursor, ".")
			if len(parts) != 2 || len(s.CursorKey) < 32 {
				return storeError("invalid_cursor", 400)
			}
			b, e := base64.RawURLEncoding.DecodeString(parts[0])
			sig, e2 := base64.RawURLEncoding.DecodeString(parts[1])
			m := hmac.New(sha256.New, s.CursorKey)
			m.Write(b)
			var c resourceCursor
			if e != nil || e2 != nil || !hmac.Equal(sig, m.Sum(nil)) || json.Unmarshal(b, &c) != nil || c.Tenant != current.TenantID || c.Subject != current.SubjectID || c.Role != current.Role || c.Kind != kind || c.Limit != limit || c.Expires < time.Now().Unix() {
				return storeError("invalid_cursor", 400)
			}
			after = c.After
		}
		rows, e := tx.QueryContext(ctx, s.Query("SELECT id,version,data FROM resources WHERE tenant_id=? AND kind=? AND id>? ORDER BY id LIMIT ?"), current.TenantID, kind, after, limit+1)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			r := core.Resource{TenantID: current.TenantID, Kind: kind}
			var data string
			if e = rows.Scan(&r.ID, &r.Version, &data); e != nil {
				return e
			}
			r.Data = json.RawMessage(data)
			out.Items = append(out.Items, r)
		}
		if e = rows.Err(); e != nil {
			return e
		}
		if len(out.Items) > limit {
			out.Items = out.Items[:limit]
			out.NextCursor, e = s.encodeCursor(resourceCursor{current.TenantID, current.SubjectID, current.Role, kind, out.Items[limit-1].ID, limit, time.Now().Add(15 * time.Minute).Unix()})
		}
		return e
	})
	return out, err
}
func (s *Store) Mutate(ctx context.Context, p core.Principal, m core.Mutation) (core.Resource, error) {
	if tenancyKind(m.Kind) {
		return s.mutateTenancy(ctx, p, m)
	}
	var out core.Resource
	var revision int64
	if !knownKind(m.Kind) || !validRef(m.ID) {
		return out, storeError("invalid_resource", 400)
	}
	var current core.Principal
	if e := s.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		current, e = s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
		if e == nil {
			e = authorize(current, m.Kind, true)
		}
		return e
	}); e != nil {
		return out, e
	}
	var data json.RawMessage
	var err error
	if !m.Delete {
		data, err = validateResource(m.Kind, m.Data)
		if err != nil {
			return out, err
		}
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		current, e = s.resolveMembershipTx(ctx, tx, current.TenantID, current.SubjectID)
		if e != nil {
			return e
		}
		if e = authorize(current, m.Kind, true); e != nil {
			return e
		}
		var version int64
		var old string
		e = tx.QueryRowContext(ctx, s.Query("SELECT version,data FROM resources WHERE tenant_id=? AND kind=? AND id=?"), current.TenantID, m.Kind, m.ID).Scan(&version, &old)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		exists := e == nil
		if (!exists && (m.ExpectedVersion != 0 || m.Delete)) || (exists && (m.ExpectedVersion <= 0 || m.ExpectedVersion != version)) {
			return storeError("version_conflict", 412)
		}
		if version == int64(^uint64(0)>>1) {
			return storeError("version_exhausted", 409)
		}
		if exists && !m.Delete && (m.Kind == "connections" || m.Kind == "accounts") {
			if e = preserveIdentity(m.Kind, json.RawMessage(old), data); e != nil {
				return e
			}
		}
		if m.Kind == "api_keys" {
			if e = s.syncKeyMetadataTx(ctx, tx, current, m, data, version+1); e != nil {
				return e
			}
		}
		if e = s.syncPolicyLimit(ctx, tx, current, m); e != nil {
			return e
		}
		if m.Delete {
			_, e = tx.ExecContext(ctx, s.Query("DELETE FROM resources WHERE tenant_id=? AND kind=? AND id=? AND version=?"), current.TenantID, m.Kind, m.ID, version)
		} else if exists {
			_, e = tx.ExecContext(ctx, s.Query("UPDATE resources SET version=?,data=? WHERE tenant_id=? AND kind=? AND id=? AND version=?"), version+1, string(data), current.TenantID, m.Kind, m.ID, version)
		} else {
			_, e = tx.ExecContext(ctx, s.Query("INSERT INTO resources(tenant_id,kind,id,version,data) VALUES(?,?,?,?,?)"), current.TenantID, m.Kind, m.ID, 1, string(data))
		}
		if e != nil {
			return e
		}
		if e = s.validateTenantTx(ctx, tx, current.TenantID); e != nil {
			return e
		}
		if e = s.bumpRevisionTx(ctx, tx); e != nil {
			return e
		}
		if e = tx.QueryRowContext(ctx, s.Query("SELECT revision FROM config_state WHERE id=1")).Scan(&revision); e != nil {
			return e
		}
		action := "upsert"
		if m.Delete {
			action = "delete"
		}
		if e = s.auditTx(ctx, tx, current, m.Kind, m.ID, action, version+1); e != nil {
			return e
		}
		out = core.Resource{ID: m.ID, TenantID: current.TenantID, Kind: m.Kind, Version: version + 1, Data: data}
		return nil
	})
	if err == nil {
		s.notifyConfig(current.TenantID, revision)
	}
	return out, err
}
func (s *Store) bumpRevisionTx(ctx context.Context, tx *sql.Tx) error {
	r, e := tx.ExecContext(ctx, s.Query("UPDATE config_state SET revision=revision+1 WHERE id=1"))
	if e != nil {
		return e
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("configuration revision row missing")
	}
	r, e = tx.ExecContext(ctx, s.Query("UPDATE config_revisions SET revision=revision+1 WHERE id=1"))
	if e != nil {
		return e
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("configuration revision history row missing")
	}
	return nil
}
func randomID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (s *Store) auditTx(ctx context.Context, tx *sql.Tx, p core.Principal, kind, id, action string, version int64) error {
	aid, err := randomID()
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"subject_id": p.SubjectID, "kind": kind, "resource_id": id, "action": action, "version": version})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.Query("INSERT INTO audit_events(tenant_id,id,subject_id,kind,resource_id,action,version,created_at,data) VALUES(?,?,?,?,?,?,?,?,?)"), p.TenantID, aid, p.SubjectID, kind, id, action, version, time.Now().Unix(), string(data))
	return err
}
func preserveIdentity(kind string, old, next json.RawMessage) error {
	var a, b map[string]json.RawMessage
	if json.Unmarshal(old, &a) != nil || json.Unmarshal(next, &b) != nil {
		return storeError("invalid_identity", 400)
	}
	keys := []string{"id", "ID", "tenant_id", "TenantID", "account_id", "AccountID"}
	if kind == "accounts" {
		keys = append(keys, "provider", "subject", "connector")
	}
	for _, k := range keys {
		if string(a[k]) != string(b[k]) {
			return storeError("immutable_identity", 409)
		}
	}
	return nil
}

// validateTenantTx checks the final candidate graph while it is still inside
// the mutation transaction. No revision is published until every reference is valid.
func (s *Store) validateTenantTx(ctx context.Context, tx *sql.Tx, tenant string) error {
	if tenant == "" {
		return storeError("unauthorized", 401)
	}
	type conn struct {
		enabled                   bool
		provider, account, region string
		settings                  map[string]string
	}
	type model struct {
		connection string
		enabled    bool
		upstream   string
	}
	type alias struct {
		enabled bool
		members []string
	}
	type route struct {
		alias     string
		targets   []RouteTargetData
		residency []string
		pool      string
	}
	conns := map[string]conn{}
	models := map[string]model{}
	aliases := map[string]alias{}
	routes := map[string]route{}
	pools := map[string]AccountData{}
	prices := map[string]PriceData{}
	limits := []PolicyLimitData{}
	rows, err := tx.QueryContext(ctx, s.Query("SELECT kind,id,data FROM resources WHERE tenant_id=? AND kind IN ('connections','models','model_aliases','route_policies','account_pools','policy_limits','prices') ORDER BY kind,id"), tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var kind, id, data string
		if err = rows.Scan(&kind, &id, &data); err != nil {
			_ = rows.Close()
			return err
		}
		switch kind {
		case "connections":
			var x ConnectionData
			if strictDecode([]byte(data), &x) != nil {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			conns[id] = conn{x.Enabled, x.Connector, x.AccountID, x.Region, x.Settings}
		case "models":
			var x ModelData
			if strictDecode([]byte(data), &x) != nil || x.ConnectionID == "" {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			models[id] = model{x.ConnectionID, x.Enabled, x.UpstreamID}
		case "model_aliases":
			var x AliasData
			if strictDecode([]byte(data), &x) != nil {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			aliases[id] = alias{x.Enabled, x.ModelIDs}
		case "route_policies":
			var x RoutePolicyData
			if strictDecode([]byte(data), &x) != nil {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			routes[id] = route{x.Alias, x.Targets, x.Residency, x.AccountPoolID}
		case "account_pools":
			var x AccountData
			if strictDecode([]byte(data), &x) != nil {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			pools[id] = x
		case "policy_limits":
			var x PolicyLimitData
			if strictDecode([]byte(data), &x) != nil {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			limits = append(limits, x)
		case "prices":
			var x PriceData
			if strictDecode([]byte(data), &x) != nil {
				_ = rows.Close()
				return storeError("invalid_configuration", 409)
			}
			prices[id] = x
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, m := range models {
		c, ok := conns[m.connection]
		if !ok {
			return storeError("invalid_configuration", 409)
		}
		if m.enabled && !c.enabled {
			return storeError("invalid_configuration", 409)
		}
	}
	for _, price := range prices {
		if _, ok := models[price.ModelID]; !ok {
			return storeError("invalid_configuration", 409)
		}
	}
	for _, a := range aliases {
		if !a.enabled {
			continue
		}
		if len(a.members) == 0 {
			return storeError("invalid_configuration", 409)
		}
		active := 0
		for _, mid := range a.members {
			m, ok := models[mid]
			if !ok {
				return storeError("invalid_configuration", 409)
			}
			if !m.enabled {
				continue
			}
			active++
			c, ok := conns[m.connection]
			if !ok || !c.enabled {
				return storeError("invalid_configuration", 409)
			}
		}
		if active == 0 {
			return storeError("invalid_configuration", 409)
		}
	}
	seenRoutes := map[string]bool{}
	for _, r := range routes {
		a, ok := aliases[r.alias]
		if !ok || !a.enabled {
			return storeError("invalid_configuration", 409)
		}
		if seenRoutes[r.alias] {
			return storeError("invalid_configuration", 409)
		}
		seenRoutes[r.alias] = true
		var pool AccountData
		if r.pool != "" {
			var found bool
			pool, found = pools[r.pool]
			if !found {
				return storeError("invalid_configuration", 409)
			}
		}
		for _, region := range r.residency {
			if !validRef(region) {
				return storeError("invalid_configuration", 409)
			}
		}
		for _, t := range r.targets {
			m, ok := models[t.ModelID]
			if !ok || !m.enabled || m.connection != t.ConnectionID {
				return storeError("invalid_configuration", 409)
			}
			c := conns[t.ConnectionID]
			if !c.enabled || !contains(a.members, t.ModelID) || t.Weight <= 0 || t.Priority < 0 {
				return storeError("invalid_configuration", 409)
			}
			if t.Region != "" && c.region != "" && t.Region != c.region {
				return storeError("invalid_configuration", 409)
			}
			if len(r.residency) > 0 && !contains(r.residency, c.region) {
				return storeError("invalid_configuration", 409)
			}
			if r.pool != "" {
				provider := c.provider
				if c.settings["provider"] != "" {
					provider = c.settings["provider"]
				}
				if pool.Provider != provider || !contains(pool.AccountIDs, c.account) {
					return storeError("invalid_configuration", 409)
				}
				if strings.EqualFold(c.settings["subscription"], "true") && c.settings["consent_ack"] != "true" {
					return storeError("consent_required", 403)
				}
			}
		}
	}
	for _, pool := range pools {
		if !validRef(pool.Provider) || !validNames(pool.AccountIDs, false) {
			return storeError("invalid_configuration", 409)
		}
		for _, account := range pool.AccountIDs {
			found := false
			for _, c := range conns {
				provider := c.provider
				if c.settings["provider"] != "" {
					provider = c.settings["provider"]
				}
				if c.enabled && c.account == account && provider == pool.Provider {
					if strings.EqualFold(c.settings["subscription"], "true") && c.settings["consent_ack"] != "true" {
						return storeError("consent_required", 403)
					}
					found = true
					break
				}
			}
			if !found {
				return storeError("invalid_configuration", 409)
			}
		}
	}
	for _, limit := range limits {
		switch limit.Scope {
		case "tenant":
			if limit.ScopeID != tenant {
				return storeError("invalid_configuration", 409)
			}
		case "connection":
			if c, ok := conns[limit.ScopeID]; !ok || !c.enabled {
				return storeError("invalid_configuration", 409)
			}
		case "model":
			if m, ok := models[limit.ScopeID]; !ok || !m.enabled {
				return storeError("invalid_configuration", 409)
			}
		case "account":
			found := false
			for _, c := range conns {
				if c.enabled && c.account == limit.ScopeID {
					found = true
					break
				}
			}
			if !found {
				return storeError("invalid_configuration", 409)
			}
		case "key":
			var n int
			if err := tx.QueryRowContext(ctx, s.Query("SELECT COUNT(*) FROM api_keys WHERE tenant_id=? AND id=?"), tenant, limit.ScopeID).Scan(&n); err != nil || n != 1 {
				return storeError("invalid_configuration", 409)
			}
		default:
			return storeError("invalid_configuration", 409)
		}
	}
	return nil
}
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

var _ core.AdminRepository = (*Store)(nil)
