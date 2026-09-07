package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

// ResolveMembership reads the current enabled domain, operator and binding in
// the same serialization boundary used by membership mutations.
func (s *Store) ResolveMembership(ctx context.Context, tenant, subject string) (out core.Principal, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var e error
		out, e = s.resolveMembershipTx(ctx, tx, tenant, subject)
		return e
	})
	return
}
func (s *Store) resolveMembershipTx(ctx context.Context, tx *sql.Tx, tenant, subject string) (core.Principal, error) {
	p := core.Principal{TenantID: tenant, SubjectID: subject}
	if tenant == "" || subject == "" {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	var td, od, bd, role string
	err := tx.QueryRowContext(ctx, s.Query("SELECT t.data,o.data,b.data,b.role FROM tenants t JOIN role_bindings b ON b.tenant_id=t.id JOIN operators o ON o.id=b.subject_id WHERE t.id=? AND o.id=?"), tenant, subject).Scan(&td, &od, &bd, &role)
	if err != nil {
		return core.Principal{}, err
	}
	var t admin.TenantData
	var o admin.OperatorData
	var b admin.RoleBindingData
	if json.Unmarshal([]byte(td), &t) != nil || json.Unmarshal([]byte(od), &o) != nil || json.Unmarshal([]byte(bd), &b) != nil || !t.Enabled || !o.Enabled || o.Subject != subject || b.Subject != subject || b.TenantID != tenant || b.Role != role {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	var authoritativeIssuer, authoritativeSubject string
	if e := tx.QueryRowContext(ctx, s.Query("SELECT issuer,identity_subject FROM identity_subjects WHERE subject_id=?"), subject).Scan(&authoritativeIssuer, &authoritativeSubject); e != nil {
		return core.Principal{}, e
	}
	if o.Issuer != "" || o.IdentitySubject != "" {
		if o.Issuer == "" || o.IdentitySubject == "" {
			return core.Principal{}, storeError("unauthorized", 401)
		}
		if authoritativeIssuer != o.Issuer || authoritativeSubject != o.IdentitySubject {
			return core.Principal{}, storeError("unauthorized", 401)
		}
		var identity storedPrincipal
		if _, _, e := s.durableGetAny(ctx, tx, "identity", durableKey(o.Issuer, o.IdentitySubject), &identity); e != nil || identity.Principal.SubjectID != subject {
			return core.Principal{}, storeError("unauthorized", 401)
		}
	} else {
		if !strings.HasPrefix(authoritativeIssuer, "bootstrap:") || authoritativeSubject != subject {
			return core.Principal{}, storeError("unauthorized", 401)
		}
		originTenant := strings.TrimPrefix(authoritativeIssuer, "bootstrap:")
		if originTenant == "" {
			return core.Principal{}, storeError("unauthorized", 401)
		}
		var claim storedBootstrapClaim
		if _, _, e := s.durableGetAny(ctx, tx, "bootstrap_owner", durableKey(originTenant), &claim); e != nil || claim.TenantID != originTenant || claim.SubjectID != subject {
			return core.Principal{}, storeError("unauthorized", 401)
		}
	}
	switch role {
	case "owner", "admin", "operator", "auditor", "viewer":
	default:
		return core.Principal{}, storeError("unauthorized", 401)
	}
	p.Role = role
	return p, nil
}
func (s *Store) AvailableTenants(ctx context.Context, p core.Principal) (out []admin.TenantMembership, err error) {
	out = []admin.TenantMembership{}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, e := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID); e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, s.Query("SELECT tenant_id FROM role_bindings WHERE subject_id=? ORDER BY tenant_id"), p.SubjectID)
		if e != nil {
			return e
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range ids {
			member, e := s.resolveMembershipTx(ctx, tx, id, p.SubjectID)
			if e != nil {
				if e == sql.ErrNoRows {
					continue
				}
				if _, ok := e.(core.GatewayError); ok {
					continue
				}
				return e
			}
			var data string
			if e = tx.QueryRowContext(ctx, s.Query("SELECT data FROM tenants WHERE id=?"), id).Scan(&data); e != nil {
				return e
			}
			var t admin.TenantData
			if e = json.Unmarshal([]byte(data), &t); e != nil {
				return e
			}
			out = append(out, admin.TenantMembership{TenantID: id, Name: t.Name, Role: member.Role, Enabled: t.Enabled})
		}
		return nil
	})
	return
}
