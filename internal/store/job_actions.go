package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hoorific/internal/core"
	"hoorific/internal/resource"
	"time"
)

// GetJobAction resolves an exposed provider resource only within the caller's
// tenant. The returned version is the durable CAS version for an admin action.
func (s *Store) GetJobAction(ctx context.Context, p core.Principal, id string) (resource.Job, int64, error) {
	var out resource.Job
	var version int64
	matches := 0
	if p.TenantID == "" || id == "" {
		return out, 0, errors.New("job identity required")
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, s.Query("SELECT version,data FROM durable_records WHERE tenant_id=? AND kind=?"), p.TenantID, "resource_job")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var data string
			var v int64
			if e = rows.Scan(&v, &data); e != nil {
				return e
			}
			var j resource.Job
			if e = json.Unmarshal([]byte(data), &j); e != nil {
				return e
			}
			if j.ResourceID == id {
				matches++
				version = v
				out = j
			}
		}
		if matches > 1 {
			return problem("conflict", 409, "ambiguous upstream resource ID")
		}
		if matches == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	return out, version, err
}

// ClaimJobAction advances the durable row before any provider side effect.
// Existing lease ownership or an If-Match mismatch fails closed.
func (s *Store) ClaimJobAction(ctx context.Context, p core.Principal, id string, expected int64) (resource.Job, int64, error) {
	j, v, e := s.GetJobAction(ctx, p, id)
	if e != nil {
		return j, v, e
	}
	if expected > 0 && v != expected {
		return j, v, problem("version_conflict", 412, "job changed")
	}
	if j.LeaseOwner != "" && j.LeaseUntil.After(time.Now()) {
		return j, v, problem("conflict", 409, "job is already leased")
	}
	owner := "admin:" + p.SubjectID
	e = s.WithTx(ctx, func(tx *sql.Tx) error {
		var oldData string
		var oldVersion int64
		key := durableKey(j.TenantID, j.ConnectionID, j.AccountID, j.ResourceID)
		e := tx.QueryRowContext(ctx, s.Query("SELECT version,data FROM durable_records WHERE tenant_id=? AND kind=? AND id=?"), p.TenantID, "resource_job", key).Scan(&oldVersion, &oldData)
		if e != nil {
			return e
		}
		if expected > 0 && oldVersion != expected {
			return problem("version_conflict", 412, "job changed")
		}
		j.LeaseOwner = owner
		j.Fence++
		j.LeaseUntil = time.Now().Add(30 * time.Second)
		j.UpdatedAt = time.Now()
		b, _ := json.Marshal(j)
		r, e := tx.ExecContext(ctx, s.Query("UPDATE durable_records SET version=version+1,data=? WHERE tenant_id=? AND kind=? AND id=? AND version=?"), string(b), p.TenantID, "resource_job", key, oldVersion)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return problem("version_conflict", 412, "job changed")
		}
		v = oldVersion + 1
		return nil
	})
	return j, v, e
}
