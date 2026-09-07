package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hoorific/internal/credential"
	"time"
)

// durableKey encodes composite identifiers without PostgreSQL-incompatible NUL bytes.
func durableKey(parts ...string) string {
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(b)
}

// All durable mutations run under WithTx's database-wide serialization boundary.
func (s *Store) durableGet(ctx context.Context, tx *sql.Tx, tenant, kind, id string, out any) (int64, error) {
	q := "SELECT version,data FROM durable_records WHERE tenant_id=? AND kind=? AND id=?"
	if s.Dialect == "postgres" {
		q += " FOR UPDATE"
	}
	var version int64
	var data string
	if err := tx.QueryRowContext(ctx, s.Query(q), tenant, kind, id).Scan(&version, &data); err != nil {
		return 0, err
	}
	return version, json.Unmarshal([]byte(data), out)
}
func (s *Store) durablePut(ctx context.Context, tx *sql.Tx, tenant, kind, id string, version int64, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if version == 0 {
		r, e := tx.ExecContext(ctx, s.Query("INSERT INTO durable_records(tenant_id,kind,id,version,data) VALUES(?,?,?,1,?) ON CONFLICT(tenant_id,kind,id) DO NOTHING"), tenant, kind, id, string(data))
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return credential.ErrConflict
		}
		return nil
	}
	r, err := tx.ExecContext(ctx, s.Query("UPDATE durable_records SET version=version+1,data=? WHERE tenant_id=? AND kind=? AND id=? AND version=?"), string(data), tenant, kind, id, version)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return credential.ErrConflict
	}
	return nil
}
func (s *Store) durableDelete(ctx context.Context, tx *sql.Tx, tenant, kind, id string) error {
	_, err := tx.ExecContext(ctx, s.Query("DELETE FROM durable_records WHERE tenant_id=? AND kind=? AND id=?"), tenant, kind, id)
	return err
}

type storedCredential struct {
	Record        credential.Record
	Intent        *credential.RefreshIntent
	RefreshStatus string
}

func (s *Store) LoadCredential(ctx context.Context, tenant, id string) (out credential.Record, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedCredential
		_, e := s.durableGet(ctx, tx, tenant, "credential", id, &row)
		out = row.Record
		return e
	})
	return
}

// LoadUsableCredential reads an outbound credential only while its refresh
// lifecycle is active. Explicit version-CAS PutCredential remains the recovery
// path for fresh reauthentication after an ambiguous attempt.
func (s *Store) LoadUsableCredential(ctx context.Context, tenant, id string) (out credential.Record, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedCredential
		_, e := s.durableGet(ctx, tx, tenant, "credential", id, &row)
		if e != nil {
			return e
		}
		if row.Record.Identity.TenantID != tenant || row.Record.Identity.ConnectionID != id {
			return credential.ErrInvalidCredential
		}
		if row.Intent != nil || row.RefreshStatus == "pending" || row.RefreshStatus == "reauth_required" {
			return credential.ErrReauthRequired
		}
		if row.Record.Status != "" && row.Record.Status != "active" {
			return credential.ErrInvalidCredential
		}
		out = row.Record
		return nil
	})
	return
}

func (s *Store) PutCredential(ctx context.Context, r credential.Record, expected int64) error {
	if expected < 0 || expected == int64(^uint64(0)>>1) || r.Identity.TenantID == "" || r.Identity.ConnectionID == "" || r.Identity.CredentialID == "" || r.Identity.AccountID == "" || r.Identity.Provider == "" || r.Identity.Version != expected+1 {
		return credential.ErrConflict
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var old storedCredential
		v, e := s.durableGet(ctx, tx, r.Identity.TenantID, "credential", r.Identity.ConnectionID, &old)
		if errors.Is(e, sql.ErrNoRows) && expected == 0 {
			return s.durablePut(ctx, tx, r.Identity.TenantID, "credential", r.Identity.ConnectionID, 0, storedCredential{Record: r, RefreshStatus: "completed"})
		}
		if e != nil {
			return e
		}
		if old.Record.Identity.Version != expected {
			return credential.ErrConflict
		}
		if old.Record.Identity.TenantID != r.Identity.TenantID || old.Record.Identity.ConnectionID != r.Identity.ConnectionID || old.Record.Identity.CredentialID != r.Identity.CredentialID || old.Record.Identity.AccountID != r.Identity.AccountID || old.Record.Identity.Provider != r.Identity.Provider {
			return credential.ErrConflict
		}
		// A versioned replacement is explicit fresh authentication. It
		// supersedes a pending/ambiguous attempt atomically; any late commit
		// still fails refreshMatches because the original identity version is
		// no longer current.
		return s.durablePut(ctx, tx, r.Identity.TenantID, "credential", r.Identity.ConnectionID, v, storedCredential{Record: r, RefreshStatus: "completed"})
	})
}

// RewrapCredential must never turn an unresolved rotating-token attempt into a
// usable credential. Check lifecycle state and identity in the same transaction.
func (s *Store) RewrapCredential(ctx context.Context, r credential.Record, expected int64) error {
	if expected < 1 || expected == int64(^uint64(0)>>1) || r.Identity.Version != expected+1 {
		return credential.ErrConflict
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var old storedCredential
		v, err := s.durableGet(ctx, tx, r.Identity.TenantID, "credential", r.Identity.ConnectionID, &old)
		if err != nil {
			return err
		}
		if old.Intent != nil || old.RefreshStatus == "pending" || old.RefreshStatus == "reauth_required" || old.Record.Status == "reauth_required" {
			return credential.ErrReauthRequired
		}
		next := old.Record.Identity
		if next.Version != expected {
			return credential.ErrConflict
		}
		next.Version++
		if next != r.Identity || r.Status != old.Record.Status {
			return credential.ErrConflict
		}
		old.Record = r
		return s.durablePut(ctx, tx, r.Identity.TenantID, "credential", r.Identity.ConnectionID, v, old)
	})
}
func (s *Store) BeginRefresh(ctx context.Context, i credential.RefreshIntent) error {
	if i.Identity.TenantID == "" || i.Identity.ConnectionID == "" || i.Identity.CredentialID == "" || i.Identity.AccountID == "" || i.Identity.Provider == "" || i.Identity.Version < 1 || i.Identity.Version == int64(^uint64(0)>>1) || i.AttemptID == "" || i.Fence == "" || !i.LeaseUntil.After(time.Now()) {
		return credential.ErrConflict
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedCredential
		v, e := s.durableGet(ctx, tx, i.Identity.TenantID, "credential", i.Identity.ConnectionID, &row)
		if e != nil {
			return e
		}
		if row.Record.Identity != i.Identity {
			return credential.ErrConflict
		}
		if row.Intent != nil || row.RefreshStatus == "pending" || row.RefreshStatus == "reauth_required" || row.Record.Status == "reauth_required" || row.Record.Status == "revoked" || row.Record.Status != "" && row.Record.Status != "active" {
			return credential.ErrReauthRequired
		}
		row.Intent = &i
		row.RefreshStatus = "pending"
		return s.durablePut(ctx, tx, i.Identity.TenantID, "credential", i.Identity.ConnectionID, v, row)
	})
}
func refreshMatches(row storedCredential, i credential.RefreshIntent) bool {
	return row.Record.Identity == i.Identity && row.Intent != nil && row.Intent.Identity == i.Identity && row.Intent.AttemptID == i.AttemptID && row.Intent.Fence == i.Fence && row.RefreshStatus == "pending"
}
func (s *Store) CommitRefresh(ctx context.Context, i credential.RefreshIntent, r credential.Record) error {
	if i.Identity.TenantID == "" || i.Identity.ConnectionID == "" || i.Identity.CredentialID == "" || i.Identity.AccountID == "" || i.Identity.Provider == "" || i.Identity.Version < 1 || i.Identity.Version == int64(^uint64(0)>>1) || r.Status != "active" {
		return credential.ErrConflict
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedCredential
		v, e := s.durableGet(ctx, tx, i.Identity.TenantID, "credential", i.Identity.ConnectionID, &row)
		if e != nil {
			return e
		}
		next := i.Identity
		next.Version++
		if !refreshMatches(row, i) || row.Record.Status != "" && row.Record.Status != "active" || r.Identity != next || !row.Intent.LeaseUntil.After(time.Now()) {
			return credential.ErrConflict
		}
		return s.durablePut(ctx, tx, i.Identity.TenantID, "credential", i.Identity.ConnectionID, v, storedCredential{Record: r, RefreshStatus: "completed"})
	})
}
func (s *Store) FailRefresh(ctx context.Context, i credential.RefreshIntent, reason string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var row storedCredential
		v, e := s.durableGet(ctx, tx, i.Identity.TenantID, "credential", i.Identity.ConnectionID, &row)
		if e != nil {
			return e
		}
		if !refreshMatches(row, i) {
			return credential.ErrConflict
		}
		row.RefreshStatus = "reauth_required"
		row.Record.Status = "reauth_required"
		return s.durablePut(ctx, tx, i.Identity.TenantID, "credential", i.Identity.ConnectionID, v, row)
	})
}
func (s *Store) SaveOAuthLogin(ctx context.Context, l credential.LoginSession) error {
	if l.TenantID == "" || l.StateHash == "" || l.AdminSessionID == "" || l.Connector == "" || !l.ExpiresAt.After(time.Now()) {
		return credential.ErrConflict
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		return s.durablePut(ctx, tx, l.TenantID, "credential_login", l.StateHash, 0, l)
	})
}
func (s *Store) ConsumeOAuthLogin(ctx context.Context, hash, tenant, session, connector string, now time.Time) (out credential.LoginSession, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var l credential.LoginSession
		_, e := s.durableGet(ctx, tx, tenant, "credential_login", hash, &l)
		if e != nil {
			return e
		}
		if l.TenantID != tenant || l.StateHash != hash || l.AdminSessionID != session || l.Connector != connector || !l.ExpiresAt.After(now) {
			return credential.ErrConflict
		}
		if e = s.durableDelete(ctx, tx, tenant, "credential_login", hash); e != nil {
			return e
		}
		out = l
		return nil
	})
	return
}

var _ credential.Repository = (*Store)(nil)
var _ credential.LoginRepository = (*Store)(nil)
var _ credential.RewrapRepository = (*Store)(nil)
