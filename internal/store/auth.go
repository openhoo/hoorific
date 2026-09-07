package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hoorific/internal/admin"
	"hoorific/internal/core"
	"time"
)

type storedSession struct {
	Hash, CSRFHash string
	Principal      core.Principal
	ExpiresAt      time.Time
}
type storedBootstrap struct {
	Principal core.Principal
	ExpiresAt time.Time
}
type storedBootstrapClaim struct {
	TenantID, SubjectID string
	ClaimedAt           time.Time
}
type storedPrincipal struct{ Principal core.Principal }
type storedToken struct {
	Principal core.Principal
	ExpiresAt time.Time
}

func (s *Store) durableGetAny(ctx context.Context, tx *sql.Tx, kind, id string, out any) (string, int64, error) {
	q := "SELECT tenant_id,version,data FROM durable_records WHERE kind=? AND id=?"
	if s.Dialect == "postgres" {
		q += " FOR UPDATE"
	}
	var tenant, data string
	var version int64
	e := tx.QueryRowContext(ctx, s.Query(q), kind, id).Scan(&tenant, &version, &data)
	if e != nil {
		return "", 0, e
	}
	return tenant, version, json.Unmarshal([]byte(data), out)
}
func (s *Store) ensurePrincipal(ctx context.Context, tx *sql.Tx, p core.Principal) error {
	if p.TenantID == "" || p.SubjectID == "" {
		return errors.New("principal identity required")
	}
	id := durableKey(p.TenantID, p.SubjectID)
	var old storedPrincipal
	if _, _, e := s.durableGetAny(ctx, tx, "principal", id, &old); e == nil {
		return nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	return s.durablePut(ctx, tx, p.TenantID, "principal", id, 0, storedPrincipal{Principal: p})
}
func (s *Store) ResolveSession(ctx context.Context, hash string) (out admin.Session, err error) {
	if hash == "" {
		return out, sql.ErrNoRows
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedSession
		_, _, e := s.durableGetAny(ctx, tx, "admin_session", hash, &row)
		if e != nil {
			return e
		}
		if !row.ExpiresAt.After(time.Now()) {
			return sql.ErrNoRows
		}
		p, e := s.resolveMembershipTx(ctx, tx, row.Principal.TenantID, row.Principal.SubjectID)
		if e != nil {
			return e
		}
		out = admin.Session{Hash: row.Hash, CSRFHash: row.CSRFHash, Principal: p, ExpiresAt: row.ExpiresAt}
		return nil
	})
	return
}
func (s *Store) CreateSession(ctx context.Context, sess admin.Session) error {
	if sess.Hash == "" || sess.Principal.TenantID == "" || sess.Principal.SubjectID == "" || !sess.ExpiresAt.After(time.Now()) {
		return errors.New("invalid session")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		p, e := s.resolveMembershipTx(ctx, tx, sess.Principal.TenantID, sess.Principal.SubjectID)
		if e != nil {
			return e
		}
		sess.Principal = p
		return s.durablePut(ctx, tx, sess.Principal.TenantID, "admin_session", sess.Hash, 0, storedSession{Hash: sess.Hash, CSRFHash: sess.CSRFHash, Principal: p, ExpiresAt: sess.ExpiresAt})
	})
}
func (s *Store) ConsumeBootstrap(ctx context.Context, code string) (out core.Principal, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedBootstrap
		tenant, _, e := s.durableGetAny(ctx, tx, "bootstrap", code, &row)
		if e != nil {
			return e
		}
		if !row.ExpiresAt.After(time.Now()) {
			return sql.ErrNoRows
		}
		claimID := durableKey(tenant)
		var claim storedBootstrapClaim
		if _, _, e = s.durableGetAny(ctx, tx, "bootstrap_owner", claimID, &claim); e == nil {
			return errors.New("owner already bootstrapped")
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var owner string
		e = tx.QueryRowContext(ctx, s.Query("SELECT subject_id FROM role_bindings WHERE tenant_id=? AND role='owner' LIMIT 1"), tenant).Scan(&owner)
		if e == nil {
			return errors.New("owner already bootstrapped")
		}
		if e != sql.ErrNoRows {
			return e
		}
		var reservation storedBootstrapClaim
		if _, _, e = s.durableGetAny(ctx, tx, "bootstrap_reservation", claimID, &reservation); e != nil {
			return e
		}
		if reservation.TenantID != tenant || reservation.SubjectID != row.Principal.SubjectID {
			return storeError("identity_collision", 409)
		}
		var reservedIssuer, reservedSubject string
		e = tx.QueryRowContext(ctx, s.Query("SELECT issuer,identity_subject FROM identity_subjects WHERE subject_id=?"), row.Principal.SubjectID).Scan(&reservedIssuer, &reservedSubject)
		if e != nil {
			return e
		}
		if row.Principal.TenantID != tenant || reservedIssuer != "bootstrap:"+tenant || reservedSubject != row.Principal.SubjectID {
			return storeError("identity_collision", 409)
		}
		tenantData, _ := json.Marshal(map[string]any{"name": tenant, "enabled": true, "allowed_origins": []string{}, "max_body_bytes": int64(0), "max_event_bytes": int64(0)})
		operatorData, _ := json.Marshal(map[string]any{"subject": row.Principal.SubjectID, "display_name": "", "enabled": true})
		bindingData, _ := json.Marshal(map[string]any{"subject": row.Principal.SubjectID, "tenant_id": tenant, "role": "owner"})
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO tenants(id,version,data) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING"), tenant, 1, string(tenantData)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO operators(id,version,data) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING"), row.Principal.SubjectID, 1, string(operatorData)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO role_bindings(tenant_id,subject_id,role,version,data) VALUES(?,?,?,?,?) ON CONFLICT(tenant_id,subject_id) DO NOTHING"), tenant, row.Principal.SubjectID, "owner", 1, string(bindingData)); e != nil {
			return e
		}
		var identity storedPrincipal
		_, _, identityErr := s.durableGetAny(ctx, tx, "identity", durableKey("bootstrap:"+tenant, row.Principal.SubjectID), &identity)
		if identityErr == nil {
			if identity.Principal.SubjectID != row.Principal.SubjectID || identity.Principal.TenantID != tenant {
				return storeError("identity_collision", 409)
			}
		} else if errors.Is(identityErr, sql.ErrNoRows) {
			if e = s.durablePut(ctx, tx, tenant, "identity", durableKey("bootstrap:"+tenant, row.Principal.SubjectID), 0, storedPrincipal{Principal: core.Principal{TenantID: tenant, SubjectID: row.Principal.SubjectID}}); e != nil {
				return e
			}
		} else {
			return identityErr
		}
		if e = s.durablePut(ctx, tx, tenant, "bootstrap_owner", claimID, 0, storedBootstrapClaim{TenantID: tenant, SubjectID: row.Principal.SubjectID, ClaimedAt: time.Now().UTC()}); e != nil {
			return e
		}
		if e = s.ensurePrincipal(ctx, tx, row.Principal); e != nil {
			return e
		}
		if e = s.durableDelete(ctx, tx, tenant, "bootstrap", code); e != nil {
			return e
		}
		out = row.Principal
		return nil
	})
	return
}
func (s *Store) SaveAdminLogin(ctx context.Context, l admin.LoginState) error {
	if l.Hash == "" || !l.ExpiresAt.After(time.Now()) {
		return errors.New("invalid login state")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error { return s.durablePut(ctx, tx, "", "admin_login", l.Hash, 0, l) })
}

// CreateBootstrap issues one ten-minute loopback bootstrap code.  It stores
// only a SHA-256 verifier and atomically refuses issuance after an owner
// principal plus owner role binding already exist.
func (s *Store) CreateBootstrap(ctx context.Context, tenant, subject string) (string, error) {
	if tenant == "" || subject == "" {
		return "", errors.New("bootstrap identity required")
	}
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		return "", e
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(code))
	hash := hex.EncodeToString(sum[:])
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		claimID := durableKey(tenant)
		var claim storedBootstrapClaim
		if _, _, e := s.durableGetAny(ctx, tx, "bootstrap_owner", claimID, &claim); e == nil {
			return errors.New("owner already bootstrapped")
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var owner string
		e := tx.QueryRowContext(ctx, s.Query("SELECT subject_id FROM role_bindings WHERE tenant_id=? AND role='owner' LIMIT 1"), tenant).Scan(&owner)
		if e == nil {
			return errors.New("owner already bootstrapped")
		}
		if e != sql.ErrNoRows {
			return e
		}
		var existingOperator string
		e = tx.QueryRowContext(ctx, s.Query("SELECT id FROM operators WHERE id=?"), subject).Scan(&existingOperator)
		if e == nil {
			return storeError("identity_collision", 409)
		}
		if e != sql.ErrNoRows {
			return e
		}
		var reservation storedBootstrapClaim
		_, _, e = s.durableGetAny(ctx, tx, "bootstrap_reservation", claimID, &reservation)
		if e == nil {
			if reservation.TenantID != tenant || reservation.SubjectID != subject {
				return storeError("identity_collision", 409)
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		} else if e = s.durablePut(ctx, tx, tenant, "bootstrap_reservation", claimID, 0, storedBootstrapClaim{TenantID: tenant, SubjectID: subject}); e != nil {
			return e
		}
		if e = s.claimIdentitySubjectTx(ctx, tx, subject, "bootstrap:"+tenant, subject, false); e != nil {
			return e
		}
		return s.durablePut(ctx, tx, tenant, "bootstrap", hash, 0, storedBootstrap{Principal: core.Principal{TenantID: tenant, SubjectID: subject, Role: "owner", Permissions: []string{"*"}}, ExpiresAt: time.Now().Add(10 * time.Minute)})
	})
	if err != nil {
		return "", err
	}
	return code, nil
}
func (s *Store) ConsumeAdminLogin(ctx context.Context, hash string) (out admin.LoginState, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		tenant, _, e := s.durableGetAny(ctx, tx, "admin_login", hash, &out)
		if e != nil {
			return e
		}
		if !out.ExpiresAt.After(time.Now()) {
			return sql.ErrNoRows
		}
		return s.durableDelete(ctx, tx, tenant, "admin_login", hash)
	})
	return
}
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	if hash == "" {
		return errors.New("session hash required")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		tenant, _, e := s.durableGetAny(ctx, tx, "admin_session", hash, &storedSession{})
		if e != nil {
			return e
		}
		return s.durableDelete(ctx, tx, tenant, "admin_session", hash)
	})
}
func (s *Store) ResolveIdentity(ctx context.Context, issuer, subject string) (out core.Principal, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedPrincipal
		tenant, _, e := s.durableGetAny(ctx, tx, "identity", durableKey(issuer, subject), &row)
		if e != nil {
			return e
		}
		var authoritativeIssuer, authoritativeSubject, operatorRaw string
		e = tx.QueryRowContext(ctx, s.Query("SELECT i.issuer,i.identity_subject,o.data FROM identity_subjects i JOIN operators o ON o.id=i.subject_id WHERE i.subject_id=?"), row.Principal.SubjectID).Scan(&authoritativeIssuer, &authoritativeSubject, &operatorRaw)
		if e != nil {
			return e
		}
		var operator admin.OperatorData
		if issuer == "" || subject == "" || json.Unmarshal([]byte(operatorRaw), &operator) != nil || authoritativeIssuer != issuer || authoritativeSubject != subject || operator.Subject != row.Principal.SubjectID || operator.Issuer != issuer || operator.IdentitySubject != subject {
			return storeError("unauthorized", 401)
		}
		p, e := s.resolveMembershipTx(ctx, tx, row.Principal.TenantID, row.Principal.SubjectID)
		if e != nil {
			return e
		}
		out = p
		_ = tenant
		return nil
	})
	return
}
func (s *Store) ResolveAdminToken(ctx context.Context, hash string) (out core.Principal, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedToken
		_, _, e := s.durableGetAny(ctx, tx, "admin_token", hash, &row)
		if e != nil {
			return e
		}
		if !row.ExpiresAt.After(time.Now()) {
			return sql.ErrNoRows
		}
		p, e := s.resolveMembershipTx(ctx, tx, row.Principal.TenantID, row.Principal.SubjectID)
		if e != nil {
			return e
		}
		scopes := make([]string, 0, len(row.Principal.Permissions))
		for _, scope := range row.Principal.Permissions {
			if admin.HasPermission(p, scope) {
				scopes = append(scopes, scope)
			}
		}
		if len(scopes) == 0 {
			return sql.ErrNoRows
		}
		p.Permissions = scopes
		out = p
		return nil
	})
	return
}
func (s *Store) SelectSessionTenant(ctx context.Context, hash, tenant string) (out admin.Session, err error) {
	if hash == "" || tenant == "" {
		return out, errors.New("session tenant required")
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedSession
		oldTenant, ver, e := s.durableGetAny(ctx, tx, "admin_session", hash, &row)
		if e != nil {
			return e
		}
		if !row.ExpiresAt.After(time.Now()) {
			return sql.ErrNoRows
		}
		if _, e = s.resolveMembershipTx(ctx, tx, row.Principal.TenantID, row.Principal.SubjectID); e != nil {
			return e
		}
		p, e := s.resolveMembershipTx(ctx, tx, tenant, row.Principal.SubjectID)
		if errors.Is(e, sql.ErrNoRows) {
			return storeError("forbidden", 403)
		}
		if e != nil {
			return e
		}
		row.Principal = p
		row.Principal.Permissions = nil
		if e = s.durableDelete(ctx, tx, oldTenant, "admin_session", hash); e != nil {
			return e
		}
		if e = s.durablePut(ctx, tx, tenant, "admin_session", hash, 0, row); e != nil {
			return e
		}
		_ = ver
		out = admin.Session{Hash: row.Hash, CSRFHash: row.CSRFHash, Principal: p, ExpiresAt: row.ExpiresAt}
		return nil
	})
	return
}
func validAdminScope(scope string) bool {
	switch scope {
	case "tenant:read", "tenant:write", "connection:read", "connection:write", "connection:test", "connection:discover", "key:read", "key:write", "route:read", "route:write", "budget:read", "budget:write", "accounting:reconcile", "config:read", "config:write", "job:read", "job:write", "catalog:read", "catalog:write", "usage:read", "audit:read", "playground:execute", "session:read", "health:read":
		return true
	}
	return false
}
func (s *Store) IssueAdminToken(ctx context.Context, issuer core.Principal, subject string, permissions []string, expires time.Time) (string, error) {
	if issuer.TenantID == "" || issuer.SubjectID == "" || subject == "" || len(permissions) == 0 || !expires.After(time.Now()) {
		return "", errors.New("invalid admin token")
	}
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		return "", e
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	e := s.WithTx(ctx, func(tx *sql.Tx) error {
		owner, e := s.resolveMembershipTx(ctx, tx, issuer.TenantID, issuer.SubjectID)
		if e != nil || owner.Role != "owner" {
			return storeError("forbidden", 403)
		}
		target, e := s.resolveMembershipTx(ctx, tx, issuer.TenantID, subject)
		if e != nil {
			return e
		}
		seen := map[string]bool{}
		for _, scope := range permissions {
			if !validAdminScope(scope) || seen[scope] || !admin.HasPermission(target, scope) {
				return errors.New("invalid token scope")
			}
			seen[scope] = true
		}
		target.Permissions = append([]string(nil), permissions...)
		return s.durablePut(ctx, tx, issuer.TenantID, "admin_token", hash, 0, storedToken{Principal: target, ExpiresAt: expires})
	})
	if e != nil {
		return "", e
	}
	return secret, nil
}
func (s *Store) RevokeAdminToken(ctx context.Context, issuer core.Principal, hash string) error {
	if hash == "" {
		return errors.New("token hash required")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		p, e := s.resolveMembershipTx(ctx, tx, issuer.TenantID, issuer.SubjectID)
		if e != nil {
			return e
		}
		if p.Role != "owner" {
			return storeError("forbidden", 403)
		}
		tenant, _, e := s.durableGetAny(ctx, tx, "admin_token", hash, &storedToken{})
		if e != nil {
			return e
		}
		if tenant != issuer.TenantID {
			return storeError("forbidden", 403)
		}
		return s.durableDelete(ctx, tx, tenant, "admin_token", hash)
	})
}
