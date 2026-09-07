package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

func tenancyKind(kind string) bool {
	return kind == "tenants" || kind == "operators" || kind == "role_bindings"
}

func (s *Store) tenancyCursor(p core.Principal, kind, cursor string, limit int) (string, error) {
	if cursor == "" {
		return "", nil
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || len(s.CursorKey) < 32 {
		return "", storeError("invalid_cursor", 400)
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	sig, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	mac := hmac.New(sha256.New, s.CursorKey)
	mac.Write(b)
	var c resourceCursor
	if e != nil || e2 != nil || !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(b, &c) != nil || c.Tenant != p.TenantID || c.Subject != p.SubjectID || c.Role != p.Role || c.Kind != kind || c.Limit != limit || c.Expires < time.Now().Unix() {
		return "", storeError("invalid_cursor", 400)
	}
	return c.After, nil
}

// Canonical reads and their membership decision share the serialization boundary.
func (s *Store) readTenancy(ctx context.Context, p core.Principal, kind, id, cursor string, limit int, single bool) (core.ResourcePage, error) {
	out := core.ResourcePage{Items: []core.Resource{}}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return out, storeError("invalid_limit", 400)
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
		if e != nil {
			return e
		}
		p = current
		if e = authorize(p, kind, false); e != nil {
			return e
		}
		after, e := s.tenancyCursor(p, kind, cursor, limit)
		if e != nil {
			return e
		}
		var q string
		var args []any
		switch kind {
		case "tenants":
			q = "SELECT t.id,t.version,t.data,o.data FROM tenants t JOIN role_bindings b ON b.tenant_id=t.id JOIN operators o ON o.id=b.subject_id WHERE b.subject_id=?"
			args = []any{p.SubjectID}
		case "operators":
			q = "SELECT t.id,t.version,t.data FROM operators t JOIN role_bindings b ON b.subject_id=t.id WHERE b.tenant_id=?"
			args = []any{p.TenantID}
		case "role_bindings":
			q = "SELECT t.subject_id,t.version,t.data FROM role_bindings t WHERE t.tenant_id=?"
			args = []any{p.TenantID}
		default:
			return storeError("invalid_kind", 400)
		}
		column := "t.id"
		if kind == "role_bindings" {
			column = "t.subject_id"
		}
		if single {
			q += " AND " + column + "=?"
			args = append(args, id)
		} else {
			q += " AND " + column + ">? ORDER BY " + column + " LIMIT ?"
			args = append(args, after, limit+1)
		}
		rows, e := tx.QueryContext(ctx, s.Query(q), args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			r := core.Resource{Kind: kind, TenantID: p.TenantID}
			var data string
			if kind == "tenants" {
				var operatorData string
				if e = rows.Scan(&r.ID, &r.Version, &data, &operatorData); e != nil {
					return e
				}
				var td admin.TenantData
				var od admin.OperatorData
				if strictDecode([]byte(data), &td) != nil || strictDecode([]byte(operatorData), &od) != nil || !od.Enabled || od.Subject != p.SubjectID {
					continue
				}
			} else if e = rows.Scan(&r.ID, &r.Version, &data); e != nil {
				return e
			}
			if kind == "tenants" {
				r.TenantID = r.ID
			}
			r.Data = json.RawMessage(data)
			out.Items = append(out.Items, r)
		}
		if e = rows.Err(); e != nil {
			return e
		}
		if single && len(out.Items) == 0 {
			return storeError("not_found", 404)
		}
		if len(out.Items) > limit {
			out.Items = out.Items[:limit]
			out.NextCursor, e = s.encodeCursor(resourceCursor{p.TenantID, p.SubjectID, p.Role, kind, out.Items[limit-1].ID, limit, time.Now().Add(15 * time.Minute).Unix()})
		}
		return e
	})
	return out, err
}

func (s *Store) requireTenantOwnerTx(ctx context.Context, tx *sql.Tx, tenant, subject string) error {
	p, e := s.resolveMembershipTx(ctx, tx, tenant, subject)
	if e != nil {
		return e
	}
	if p.Role != "owner" {
		return storeError("forbidden", 403)
	}
	return nil
}

// Tenant management may re-enable a disabled target, but still requires an
// enabled owner operator binding. The target tenant's enabled flag is the only
// membership check intentionally omitted here.
func (s *Store) requireTenantOwnerManagementTx(ctx context.Context, tx *sql.Tx, tenant, subject string) error {
	var raw string
	if e := tx.QueryRowContext(ctx, s.Query("SELECT o.data FROM role_bindings b JOIN operators o ON o.id=b.subject_id WHERE b.tenant_id=? AND b.subject_id=? AND b.role='owner'"), tenant, subject).Scan(&raw); e != nil {
		return e
	}
	var operator admin.OperatorData
	if strictDecode([]byte(raw), &operator) != nil || operator.Subject != subject || !operator.Enabled {
		return storeError("unauthorized", 401)
	}
	return nil
}

func (s *Store) operatorTenantsTx(ctx context.Context, tx *sql.Tx, subject string) ([]string, error) {
	rows, e := tx.QueryContext(ctx, s.Query("SELECT tenant_id FROM role_bindings WHERE subject_id=? ORDER BY tenant_id"), subject)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var tenant string
		if e = rows.Scan(&tenant); e != nil {
			return nil, e
		}
		tenants = append(tenants, tenant)
	}
	return tenants, rows.Err()
}

// Evaluate the candidate graph, not the caller's cached role or owner row count.
func (s *Store) preserveEnabledOwnerTx(ctx context.Context, tx *sql.Tx, tenant string) error {
	rows, e := tx.QueryContext(ctx, s.Query("SELECT o.data FROM role_bindings b JOIN operators o ON o.id=b.subject_id WHERE b.tenant_id=? AND b.role='owner'"), tenant)
	if e != nil {
		return e
	}
	defer rows.Close()
	enabled := false
	for rows.Next() {
		var raw string
		var o admin.OperatorData
		if e = rows.Scan(&raw); e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(raw), &o); e != nil {
			return e
		}
		enabled = enabled || o.Enabled
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if !enabled {
		return storeError("last_enabled_owner", 409)
	}
	return nil
}

func (s *Store) claimIdentitySubjectTx(ctx context.Context, tx *sql.Tx, subject, issuer, identitySubject string, allowBootstrapReplacement bool) error {
	var existingIssuer, existingIdentity string
	err := tx.QueryRowContext(ctx, s.Query("SELECT issuer,identity_subject FROM identity_subjects WHERE subject_id=?"), subject).Scan(&existingIssuer, &existingIdentity)
	if err == nil {
		if existingIssuer == issuer && existingIdentity == identitySubject {
			return nil
		}
		if allowBootstrapReplacement && strings.HasPrefix(existingIssuer, "bootstrap:") {
			var conflictingSubject string
			conflictErr := tx.QueryRowContext(ctx, s.Query("SELECT subject_id FROM identity_subjects WHERE issuer=? AND identity_subject=?"), issuer, identitySubject).Scan(&conflictingSubject)
			if conflictErr == nil && conflictingSubject != subject {
				return storeError("identity_collision", 409)
			}
			if conflictErr != nil && !errors.Is(conflictErr, sql.ErrNoRows) {
				return conflictErr
			}
			_, err = tx.ExecContext(ctx, s.Query("UPDATE identity_subjects SET issuer=?,identity_subject=? WHERE subject_id=?"), issuer, identitySubject, subject)
			return err
		}
		return storeError("identity_collision", 409)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var existingSubject string
	err = tx.QueryRowContext(ctx, s.Query("SELECT subject_id FROM identity_subjects WHERE issuer=? AND identity_subject=?"), issuer, identitySubject).Scan(&existingSubject)
	if err == nil && existingSubject != subject {
		return storeError("identity_collision", 409)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, s.Query("INSERT INTO identity_subjects(subject_id,issuer,identity_subject) VALUES(?,?,?) ON CONFLICT DO NOTHING"), subject, issuer, identitySubject); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, s.Query("SELECT issuer,identity_subject FROM identity_subjects WHERE subject_id=?"), subject).Scan(&existingIssuer, &existingIdentity); err != nil {
		return err
	}
	if existingIssuer != issuer || existingIdentity != identitySubject {
		return storeError("identity_collision", 409)
	}
	return nil
}

func validateTenantData(raw json.RawMessage) (json.RawMessage, error) {
	var d admin.TenantData
	if strictDecode(raw, &d) != nil || strings.TrimSpace(d.Name) == "" || d.MaxBodyBytes < 0 || d.MaxEventBytes < 0 {
		return nil, storeError("invalid_tenant", 400)
	}
	for _, origin := range d.AllowedOrigins {
		u, e := url.Parse(origin)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") {
			return nil, storeError("invalid_origin", 400)
		}
	}
	return json.Marshal(d)
}

// Only the durable bootstrap claimant may POST over their version-one seed.
func (s *Store) bootstrapResourceTx(ctx context.Context, tx *sql.Tx, p core.Principal, m core.Mutation, version int64) (bool, error) {
	if m.Delete || m.ExpectedVersion != 0 || version != 1 {
		return false, nil
	}
	if (m.Kind == "tenants" && m.ID != p.TenantID) || (m.Kind != "tenants" && m.ID != p.SubjectID) {
		return false, nil
	}
	var claim storedBootstrapClaim
	_, _, e := s.durableGetAny(ctx, tx, "bootstrap_owner", durableKey(p.TenantID), &claim)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return claim.TenantID == p.TenantID && claim.SubjectID == p.SubjectID && p.Role == "owner", nil
}

func (s *Store) mutateTenancy(ctx context.Context, p core.Principal, m core.Mutation) (core.Resource, error) {
	var out core.Resource
	var revision int64
	var affected []string
	if !validRef(m.ID) {
		return out, storeError("invalid_resource", 400)
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
		if e != nil {
			return e
		}
		p = current
		tenant := p.TenantID
		if m.Kind == "tenants" {
			tenant = m.ID
		}
		affected = []string{tenant}
		table := m.Kind
		where := "id=?"
		args := []any{m.ID}
		if m.Kind == "role_bindings" {
			where = "tenant_id=? AND subject_id=?"
			args = []any{tenant, m.ID}
		}
		var version int64
		var old string
		e = tx.QueryRowContext(ctx, s.Query("SELECT version,data FROM "+table+" WHERE "+where), args...).Scan(&version, &old)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		exists := e == nil
		ownerTenant := tenant
		if m.Kind == "tenants" && !exists {
			ownerTenant = p.TenantID
		}
		if m.Kind == "tenants" && exists {
			if e = s.requireTenantOwnerManagementTx(ctx, tx, ownerTenant, p.SubjectID); e != nil {
				return e
			}
		} else if e = s.requireTenantOwnerTx(ctx, tx, ownerTenant, p.SubjectID); e != nil {
			return e
		}
		if m.Kind == "operators" && exists {
			affected, e = s.operatorTenantsTx(ctx, tx, m.ID)
			if e != nil {
				return e
			}
			if !contains(affected, p.TenantID) {
				return storeError("operator_collision", 409)
			}
			for _, bound := range affected {
				if e = s.requireTenantOwnerTx(ctx, tx, bound, p.SubjectID); e != nil {
					return e
				}
			}
		}
		bootstrap := false
		if exists {
			bootstrap, e = s.bootstrapResourceTx(ctx, tx, p, m, version)
			if e != nil {
				return e
			}
		}
		if (!exists && (m.ExpectedVersion != 0 || m.Delete)) || (exists && !bootstrap && (m.ExpectedVersion <= 0 || m.ExpectedVersion != version)) {
			return storeError("version_conflict", 412)
		}
		if version == int64(^uint64(0)>>1) {
			return storeError("version_exhausted", 409)
		}
		var data json.RawMessage
		var role string
		switch m.Kind {
		case "tenants":
			if m.Delete {
				return storeError("tenant_delete_not_supported", 409)
			}
			data, e = validateTenantData(m.Data)
			if e != nil {
				return e
			}
		case "operators":
			if exists {
				var previous admin.OperatorData
				if e = json.Unmarshal([]byte(old), &previous); e != nil {
					return e
				}
				if previous.Issuer == "" && (m.Delete || !bootstrap) {
					return storeError("bootstrap_identity_protected", 409)
				}
			}
			if !m.Delete {
				var d admin.OperatorData
				if strictDecode(m.Data, &d) != nil || d.Subject != m.ID || ((d.Issuer == "") != (d.IdentitySubject == "")) {
					return storeError("invalid_operator", 400)
				}
				if (!exists && d.Issuer == "") || strings.HasPrefix(d.Issuer, "bootstrap:") {
					return storeError("invalid_operator", 400)
				}
				if exists {
					var previous admin.OperatorData
					if e = json.Unmarshal([]byte(old), &previous); e != nil {
						return e
					}
					if previous.Subject != d.Subject || (previous.Issuer != "" || previous.IdentitySubject != "") && (previous.Issuer != d.Issuer || previous.IdentitySubject != d.IdentitySubject) {
						return storeError("immutable_identity", 409)
					}
					if previous.Issuer == "" && !bootstrap {
						return storeError("bootstrap_identity_protected", 409)
					}
				}
				if d.Issuer != "" {
					if e = s.claimIdentitySubjectTx(ctx, tx, m.ID, d.Issuer, d.IdentitySubject, bootstrap); e != nil {
						return e
					}
					var identity storedPrincipal
					_, _, identityErr := s.durableGetAny(ctx, tx, "identity", durableKey(d.Issuer, d.IdentitySubject), &identity)
					if identityErr == nil {
						if identity.Principal.SubjectID != m.ID || !exists {
							return storeError("identity_collision", 409)
						}
					} else if !errors.Is(identityErr, sql.ErrNoRows) {
						return identityErr
					} else if e = s.durablePut(ctx, tx, "", "identity", durableKey(d.Issuer, d.IdentitySubject), 0, storedPrincipal{Principal: core.Principal{TenantID: tenant, SubjectID: m.ID}}); e != nil {
						return e
					}
				}
				data, e = json.Marshal(d)
				if e != nil {
					return e
				}
				if !exists {
					if e = s.ensurePrincipal(ctx, tx, core.Principal{TenantID: tenant, SubjectID: m.ID, Role: "viewer"}); e != nil {
						return e
					}
				}
			}
		case "role_bindings":
			if !m.Delete {
				var d admin.RoleBindingData
				if strictDecode(m.Data, &d) != nil || d.Subject != m.ID || d.TenantID != tenant {
					return storeError("invalid_role_binding", 400)
				}
				switch d.Role {
				case "owner", "admin", "operator", "auditor", "viewer":
				default:
					return storeError("invalid_role", 400)
				}
				var operator string
				if e = tx.QueryRowContext(ctx, s.Query("SELECT data FROM operators WHERE id=?"), m.ID).Scan(&operator); errors.Is(e, sql.ErrNoRows) {
					return storeError("operator_not_found", 404)
				} else if e != nil {
					return e
				}
				role = d.Role
				data, e = json.Marshal(d)
				if e != nil {
					return e
				}
			}
		default:
			return storeError("invalid_kind", 400)
		}
		if m.Delete {
			_, e = tx.ExecContext(ctx, s.Query("DELETE FROM "+table+" WHERE "+where), args...)
		} else if exists {
			set := "version=?,data=?"
			values := []any{version + 1, string(data)}
			if m.Kind == "role_bindings" {
				set += ",role=?"
				values = append(values, role)
			}
			values = append(values, args...)
			_, e = tx.ExecContext(ctx, s.Query("UPDATE "+table+" SET "+set+" WHERE "+where), values...)
		} else if m.Kind == "role_bindings" {
			_, e = tx.ExecContext(ctx, s.Query("INSERT INTO role_bindings(tenant_id,subject_id,role,version,data) VALUES(?,?,?,1,?)"), tenant, m.ID, role, string(data))
		} else {
			_, e = tx.ExecContext(ctx, s.Query("INSERT INTO "+table+"(id,version,data) VALUES(?,1,?)"), m.ID, string(data))
		}
		if e != nil {
			return e
		}
		if m.Kind == "tenants" && !exists {
			binding, _ := json.Marshal(admin.RoleBindingData{Subject: p.SubjectID, TenantID: tenant, Role: "owner"})
			if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO role_bindings(tenant_id,subject_id,role,version,data) VALUES(?,?,'owner',1,?)"), tenant, p.SubjectID, string(binding)); e != nil {
				return e
			}
			actor := p
			actor.TenantID = tenant
			if e = s.auditTx(ctx, tx, actor, "role_bindings", p.SubjectID, "upsert", 1); e != nil {
				return e
			}
		}
		if m.Kind == "operators" && !exists {
			// Creating an operator enrolls it in this tenant without granting write power.
			binding, _ := json.Marshal(admin.RoleBindingData{Subject: m.ID, TenantID: tenant, Role: "viewer"})
			if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO role_bindings(tenant_id,subject_id,role,version,data) VALUES(?,?,'viewer',1,?)"), tenant, m.ID, string(binding)); e != nil {
				return e
			}
			if e = s.auditTx(ctx, tx, p, "role_bindings", m.ID, "upsert", 1); e != nil {
				return e
			}
		}
		if m.Kind == "operators" && m.Delete {
			// Identity mappings remain reserved tombstones, preventing subject reuse.
			if _, e = tx.ExecContext(ctx, s.Query("DELETE FROM role_bindings WHERE subject_id=?"), m.ID); e != nil {
				return e
			}
		}
		for _, bound := range affected {
			if e = s.preserveEnabledOwnerTx(ctx, tx, bound); e != nil {
				return e
			}
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
		for _, bound := range affected {
			actor := p
			actor.TenantID = bound
			if e = s.auditTx(ctx, tx, actor, m.Kind, m.ID, action, version+1); e != nil {
				return e
			}
		}
		out = core.Resource{ID: m.ID, TenantID: tenant, Kind: m.Kind, Version: version + 1, Data: data}
		return nil
	})
	if err == nil {
		for _, tenant := range affected {
			s.notifyConfig(tenant, revision)
		}
	}
	return out, err
}
