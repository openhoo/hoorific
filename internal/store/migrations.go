package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrate creates the complete durable schema. It is deliberately separate
// from Open: serving never races DDL, and cluster operators run it once under
// the database migration lock.
func (s *Store) Migrate(ctx context.Context) error {
	if s.Dialect == "postgres" {
		lock, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}
		defer lock.Rollback()
		if _, err = lock.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(719240114)); err != nil {
			return err
		}
		if err = s.migrateTx(ctx, lock); err != nil {
			return err
		}
		return lock.Commit()
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error { return s.migrateTx(ctx, tx) })
}
func (s *Store) migrateTx(ctx context.Context, tx *sql.Tx) error {
	// SQLite accepts the PostgreSQL-compatible types used below. Dialect-specific
	// timestamp expressions are only used by request-path queries.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (id INTEGER PRIMARY KEY, version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(id,version) VALUES (1,1) ON CONFLICT (id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS config_state (id INTEGER PRIMARY KEY, revision BIGINT NOT NULL)`,
		`INSERT INTO config_state(id,revision) VALUES (1,0) ON CONFLICT (id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS tenants (id TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS operators (id TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS role_bindings (tenant_id TEXT NOT NULL, subject_id TEXT NOT NULL, role TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,subject_id))`,
		`CREATE TABLE IF NOT EXISTS identity_subjects (subject_id TEXT PRIMARY KEY, issuer TEXT NOT NULL, identity_subject TEXT NOT NULL, UNIQUE (issuer,identity_subject))`,
		`CREATE TABLE IF NOT EXISTS api_keys (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, verifier TEXT NOT NULL, data TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS resources (tenant_id TEXT NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,kind,id))`,
		`CREATE TABLE IF NOT EXISTS durable_records (tenant_id TEXT NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,kind,id))`,
		`CREATE TABLE IF NOT EXISTS credentials (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS models (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS model_aliases (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS route_policies (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS policy_limits (tenant_id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, window_id TEXT NOT NULL, kind TEXT NOT NULL, maximum BIGINT NOT NULL, reserve BIGINT NOT NULL DEFAULT 0, version BIGINT NOT NULL DEFAULT 1, PRIMARY KEY (tenant_id,scope_kind,scope_id,window_id,kind))`,
		`CREATE TABLE IF NOT EXISTS config_revisions (id BIGINT PRIMARY KEY, revision BIGINT NOT NULL, data TEXT NOT NULL DEFAULT '{}')`,
		`INSERT INTO config_revisions(id,revision,data) VALUES (1,0,'{}') ON CONFLICT (id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS admissions (tenant_id TEXT NOT NULL, admission_id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, state TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY (tenant_id,admission_id))`,
		`CREATE TABLE IF NOT EXISTS attempts (tenant_id TEXT NOT NULL, attempt_id TEXT NOT NULL, request_id TEXT NOT NULL, state TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, deadline BIGINT NOT NULL DEFAULT 0, updated_at BIGINT NOT NULL DEFAULT 0, data TEXT NOT NULL, PRIMARY KEY (tenant_id,attempt_id))`,
		`CREATE INDEX IF NOT EXISTS attempts_state_deadline ON attempts(state,deadline)`,
		`CREATE TABLE IF NOT EXISTS allowances (tenant_id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, window_id TEXT NOT NULL, kind TEXT NOT NULL, reserved BIGINT NOT NULL DEFAULT 0, version BIGINT NOT NULL DEFAULT 1, PRIMARY KEY (tenant_id,scope_kind,scope_id,window_id,kind))`,
		`CREATE TABLE IF NOT EXISTS quota_windows (tenant_id TEXT NOT NULL, id TEXT NOT NULL, starts BIGINT NOT NULL, ends BIGINT NOT NULL, version BIGINT NOT NULL DEFAULT 1, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS usage_ledger (tenant_id TEXT NOT NULL, effect_id TEXT NOT NULL, attempt_id TEXT NOT NULL, effect_kind TEXT NOT NULL, amount BIGINT, data TEXT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY (tenant_id,effect_id), UNIQUE (tenant_id,attempt_id,effect_kind))`,
		`CREATE TABLE IF NOT EXISTS usage_aggregates (tenant_id TEXT NOT NULL, day BIGINT NOT NULL, amount BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (tenant_id,day))`,
		`CREATE TABLE IF NOT EXISTS reconciliations (tenant_id TEXT NOT NULL, reconciliation_id TEXT NOT NULL, admission_id TEXT NOT NULL, payload TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, PRIMARY KEY (tenant_id,reconciliation_id))`,
		`CREATE TABLE IF NOT EXISTS audit_events (tenant_id TEXT NOT NULL, id TEXT NOT NULL, subject_id TEXT NOT NULL, kind TEXT NOT NULL, resource_id TEXT NOT NULL, action TEXT NOT NULL, version BIGINT NOT NULL, created_at BIGINT NOT NULL, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS upstream_operations (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS continuations (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS realtime_tickets (tenant_id TEXT NOT NULL, hash TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS oauth_sessions (tenant_id TEXT NOT NULL, hash TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS admin_sessions (tenant_id TEXT NOT NULL, hash TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS refresh_leases (tenant_id TEXT NOT NULL, credential_id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,credential_id))`,
		`CREATE TABLE IF NOT EXISTS refresh_attempts (tenant_id TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,id))`,
		`CREATE TABLE IF NOT EXISTS durable_records (tenant_id TEXT NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL, version BIGINT NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (tenant_id,kind,id))`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, s.Query(q)); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	if s.Dialect == "postgres" {
		for _, q := range []string{"ALTER TABLE attempts ADD COLUMN IF NOT EXISTS deadline BIGINT NOT NULL DEFAULT 0", "ALTER TABLE attempts ADD COLUMN IF NOT EXISTS updated_at BIGINT NOT NULL DEFAULT 0", "CREATE INDEX IF NOT EXISTS attempts_state_deadline ON attempts(state,deadline)"} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migration compatibility: %w", err)
			}
		}
	} else {
		for _, col := range []string{"deadline", "updated_at"} {
			var n int
			err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name=?", col).Scan(&n)
			if err != nil {
				return err
			}
			if n == 0 {
				if _, err = tx.ExecContext(ctx, "ALTER TABLE attempts ADD COLUMN "+col+" INTEGER NOT NULL DEFAULT 0"); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS attempts_state_deadline ON attempts(state,deadline)"); err != nil {
			return err
		}
	}
	return s.upgradeSchema(ctx, tx)
}

// upgradeSchema applies additive, versioned migrations under the same database
// migration transaction/lock. Bootstrap configuration schema_version is separate.
func (s *Store) upgradeSchema(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT version FROM schema_version WHERE id=1").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > SchemaVersion {
		return fmt.Errorf("cannot migrate schema version %d; supported through %d", version, SchemaVersion)
	}
	if version == 1 {
		// Request retry accounting must not scan all of a tenant's attempt
		// history for every admission. Keep the exact transactional COUNT.
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS attempts_tenant_request ON attempts(tenant_id,request_id)"); err != nil {
			return fmt.Errorf("schema migration 2: %w", err)
		}
		result, err := tx.ExecContext(ctx, "UPDATE schema_version SET version=2 WHERE id=1 AND version=1")
		if err != nil { return err }
		changed, err := result.RowsAffected()
		if err != nil { return err }
		if changed != 1 { return fmt.Errorf("schema version changed during migration 2") }
	}
	return nil
}
