package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hoorific/internal/resource"
	"sort"
	"sync"
	"time"
)

func jobID(j resource.Job) string {
	return durableKey(j.TenantID, j.ConnectionID, j.AccountID, j.ResourceID)
}
func jobAttemptID(metadata []byte) string {
	var identity struct{ AttemptID string }
	if json.Unmarshal(metadata, &identity) != nil {
		return ""
	}
	return identity.AttemptID
}
func (s *Store) PutJob(ctx context.Context, j resource.Job) error {
	if j.TenantID == "" || j.ConnectionID == "" || j.AccountID == "" || j.ResourceID == "" {
		return errors.New("invalid job scope")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error { return s.durablePut(ctx, tx, j.TenantID, "resource_job", jobID(j), 0, j) })
}
func (s *Store) GetJob(ctx context.Context, tenant, connection, resourceID string) (out resource.Job, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		q := "SELECT tenant_id,id,version,data FROM durable_records WHERE tenant_id=? AND kind=?"
		if s.Dialect == "postgres" {
			q += " FOR UPDATE"
		}
		rows, e := tx.QueryContext(ctx, s.Query(q), tenant, "resource_job")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var t, id, data string
			var v int64
			if e = rows.Scan(&t, &id, &v, &data); e != nil {
				return e
			}
			var j resource.Job
			if e = json.Unmarshal([]byte(data), &j); e != nil {
				return e
			}
			if j.ConnectionID == connection && j.ResourceID == resourceID {
				out = j
				return nil
			}
		}
		return sql.ErrNoRows
	})
	return
}

// jobClock is sampled after WithTx has acquired the database writer lock.
// sqlNow is second-granularity, which is insufficient for lease expiry.
func (s *Store) jobClock(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	if s.Dialect == "postgres" {
		var now time.Time
		err := tx.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now)
		return now.UTC(), err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT strftime('%Y-%m-%dT%H:%M:%fZ','now')").Scan(&raw); err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, raw)
}

// ClaimJobs uses the durable_records primary key as a bounded, round-robin
// cursor. Eligibility lives in JSON, so a full-table due-time sort would make
// LIMIT misleading and could starve rows behind a large inactive prefix.
var jobCursorMu sync.Mutex
var jobCursors = make(map[*Store]struct{ tenant, id string })

func (s *Store) ClaimJobs(ctx context.Context, owner string, _ time.Time, lease time.Duration, limit int) (out []resource.Job, err error) {
	if owner == "" || lease <= 0 || limit <= 0 {
		return nil, errors.New("invalid job lease")
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		now, e := s.jobClock(ctx, tx)
		if e != nil {
			return e
		}
		type candidate struct {
			tenant, id string
			version    int64
			job        resource.Job
		}
		jobCursorMu.Lock()
		cursor := jobCursors[s]
		jobCursorMu.Unlock()
		scanBudget := 1024
		if limit < 8 {
			scanBudget = limit * 16
		}
		if scanBudget < 128 {
			scanBudget = 128
		}
		scan := func(after struct{ tenant, id string }, n int) ([]candidate, struct{ tenant, id string }, int, error) {
			q := "SELECT tenant_id,id,version,data FROM durable_records WHERE kind=?"
			args := []any{"resource_job"}
			if after.tenant != "" {
				q += " AND (tenant_id>? OR (tenant_id=? AND id>?))"
				args = append(args, after.tenant, after.tenant, after.id)
			}
			q += " ORDER BY tenant_id,id LIMIT ?"
			args = append(args, n)
			if s.Dialect == "postgres" {
				q += " FOR UPDATE"
			}
			rows, e := tx.QueryContext(ctx, s.Query(q), args...)
			if e != nil {
				return nil, struct{ tenant, id string }{}, 0, e
			}
			var candidates []candidate
			last := after
			count := 0
			for rows.Next() {
				var c candidate
				var data string
				if e = rows.Scan(&c.tenant, &c.id, &c.version, &data); e != nil {
					rows.Close()
					return nil, last, count, e
				}
				count++
				last.tenant, last.id = c.tenant, c.id
				if e = json.Unmarshal([]byte(data), &c.job); e != nil {
					rows.Close()
					return nil, last, count, e
				}
				if c.job.Status == "outcome_unknown" ||
					(c.job.Status == "settled" || c.job.Status == "completed" || c.job.Status == "failed" || c.job.Status == "cancelled") && !c.job.SettlementPending ||
					c.job.NextPoll.After(now) || c.job.LeaseUntil.After(now) {
					continue
				}
				if c.job.TenantID != c.tenant || jobID(c.job) != c.id || c.job.Fence < 0 || c.job.Fence == 1<<63-1 {
					rows.Close()
					return nil, last, count, errors.New("invalid persisted job identity or fence")
				}
				candidates = append(candidates, c)
			}
			e = rows.Err()
			closeErr := rows.Close()
			if e != nil {
				return nil, last, count, e
			}
			if closeErr != nil {
				return nil, last, count, closeErr
			}
			return candidates, last, count, nil
		}
		candidates, last, scanned, e := scan(cursor, scanBudget)
		if e != nil {
			return e
		}
		if scanned < scanBudget && cursor.tenant != "" {
			wrapped, wrappedLast, wrappedScanned, e := scan(struct{ tenant, id string }{}, scanBudget-scanned)
			if e != nil {
				return e
			}
			candidates = append(candidates, wrapped...)
			if wrappedScanned > 0 {
				last = wrappedLast
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if !candidates[i].job.NextPoll.Equal(candidates[j].job.NextPoll) {
				return candidates[i].job.NextPoll.Before(candidates[j].job.NextPoll)
			}
			if !candidates[i].job.UpdatedAt.Equal(candidates[j].job.UpdatedAt) {
				return candidates[i].job.UpdatedAt.Before(candidates[j].job.UpdatedAt)
			}
			if candidates[i].tenant != candidates[j].tenant {
				return candidates[i].tenant < candidates[j].tenant
			}
			return candidates[i].id < candidates[j].id
		})
		if len(candidates) > limit {
			candidates = candidates[:limit]
		}
		jobCursorMu.Lock()
		jobCursors[s] = last
		jobCursorMu.Unlock()
		claimedAt, e := s.jobClock(ctx, tx)
		if e != nil {
			return e
		}
		for _, c := range candidates {
			j := c.job
			j.LeaseOwner = owner
			j.Fence++
			j.LeaseUntil = claimedAt.Add(lease)
			j.UpdatedAt = claimedAt
			if e = s.durablePut(ctx, tx, c.tenant, "resource_job", c.id, c.version, j); e != nil {
				return e
			}
			out = append(out, j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) UpdateJob(ctx context.Context, j resource.Job, owner string, fence int64) error {
	if owner == "" || fence <= 0 || j.Fence != fence {
		return errors.New("stale job lease")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var old resource.Job
		v, e := s.durableGet(ctx, tx, j.TenantID, "resource_job", jobID(j), &old)
		if e != nil {
			return e
		}
		now, e := s.jobClock(ctx, tx)
		if e != nil {
			return e
		}
		expiryClock := now
		// SQLite exposes milliseconds. Reject within the final clock tick rather
		// than accepting a lease that has already expired inside that tick.
		if s.Dialect != "postgres" {
			expiryClock = expiryClock.Add(time.Millisecond)
		}
		if old.LeaseOwner != owner || old.Fence != fence || !old.LeaseUntil.After(expiryClock) {
			return errors.New("stale job lease")
		}
		if j.TenantID != old.TenantID || j.ConnectionID != old.ConnectionID ||
			j.AccountID != old.AccountID || j.ResourceID != old.ResourceID ||
			j.RequestID != old.RequestID || j.Operation != old.Operation || !j.CreatedAt.Equal(old.CreatedAt) ||
			jobAttemptID(j.Metadata) != jobAttemptID(old.Metadata) {
			return errors.New("immutable job identity")
		}
		// The current owner may release a lease, but cannot transfer or extend it.
		if j.LeaseOwner == "" && j.LeaseUntil.IsZero() {
			j.LeaseOwner = ""
			j.LeaseUntil = time.Time{}
		} else {
			if j.LeaseOwner != old.LeaseOwner || !j.LeaseUntil.Equal(old.LeaseUntil) {
				return errors.New("invalid job lease mutation")
			}
			j.LeaseOwner = old.LeaseOwner
			j.LeaseUntil = old.LeaseUntil
		}
		j.Fence = old.Fence
		j.UpdatedAt = now
		return s.durablePut(ctx, tx, old.TenantID, "resource_job", jobID(old), v, j)
	})
}

func continuationID(c resource.Continuation) string {
	return durableKey(c.TenantID, c.ConnectionID, c.AccountID, c.ID)
}
func (s *Store) PutContinuation(ctx context.Context, c resource.Continuation) error {
	if c.TenantID == "" || c.ConnectionID == "" || c.AccountID == "" || c.ID == "" || !c.ExpiresAt.After(time.Now()) {
		return errors.New("invalid continuation")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		return s.durablePut(ctx, tx, c.TenantID, "resource_continuation", continuationID(c), 0, c)
	})
}
func (s *Store) GetContinuation(ctx context.Context, tenant, connection, id string) (out resource.Continuation, err error) {
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		q := "SELECT tenant_id,id,version,data FROM durable_records WHERE tenant_id=? AND kind=?"
		if s.Dialect == "postgres" {
			q += " FOR UPDATE"
		}
		rows, e := tx.QueryContext(ctx, s.Query(q), tenant, "resource_continuation")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var t, key, data string
			var v int64
			if e = rows.Scan(&t, &key, &v, &data); e != nil {
				return e
			}
			var c resource.Continuation
			if e = json.Unmarshal([]byte(data), &c); e != nil {
				return e
			}
			if c.ConnectionID == connection && c.ID == id {
				out = c
				return nil
			}
		}
		return sql.ErrNoRows
	})
	if err == nil && !out.ExpiresAt.After(time.Now()) {
		err = sql.ErrNoRows
	}
	return
}
func (s *Store) DeleteContinuation(ctx context.Context, tenant, connection, id string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		q := "SELECT tenant_id,id,version,data FROM durable_records WHERE tenant_id=? AND kind=?"
		if s.Dialect == "postgres" {
			q += " FOR UPDATE"
		}
		rows, e := tx.QueryContext(ctx, s.Query(q), tenant, "resource_continuation")
		if e != nil {
			return e
		}
		var key string
		for rows.Next() {
			var t, k, data string
			var v int64
			if e = rows.Scan(&t, &k, &v, &data); e != nil {
				rows.Close()
				return e
			}
			var c resource.Continuation
			if e = json.Unmarshal([]byte(data), &c); e != nil {
				rows.Close()
				return e
			}
			if c.ConnectionID == connection && c.ID == id {
				key = k
				break
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if key == "" {
			return sql.ErrNoRows
		}
		return s.durableDelete(ctx, tx, tenant, "resource_continuation", key)
	})
}

var _ resource.Store = (*Store)(nil)
