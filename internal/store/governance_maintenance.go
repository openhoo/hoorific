package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const (
	attemptRetentionSeconds         = int64(30 * 24 * time.Hour / time.Second)
	usageRetentionSeconds           = int64(365 * 24 * time.Hour / time.Second)
	auditRetentionSeconds           = int64(90 * 24 * time.Hour / time.Second)
	attemptCancellationGraceSeconds = int64(30)
)

func (s *Store) RunMaintenance(ctx context.Context) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		now, e := sqlNow(ctx, tx, s.Dialect)
		if e != nil {
			return e
		}
		cutoff := now - attemptCancellationGraceSeconds
		released := "json_extract(data,'$.concurrency_released') IS NOT 1"
		if s.Dialect == "postgres" {
			released = "COALESCE((data::jsonb ->> 'concurrency_released'),'false') <> 'true'"
		}
		q := "SELECT tenant_id,attempt_id,state,version,data FROM attempts WHERE deadline>0 AND deadline<=? AND ((state IN ('dispatch_intent','accepted')) OR (state='outcome_unknown' AND " + released + ")) ORDER BY deadline,tenant_id,attempt_id LIMIT 256"
		rows, e := tx.QueryContext(ctx, s.Query(q), cutoff)
		if e != nil {
			return e
		}
		type row struct {
			tenant, id, state, data string
			version                 int64
		}
		var pending []row
		for rows.Next() {
			var r row
			if e = rows.Scan(&r.tenant, &r.id, &r.state, &r.version, &r.data); e != nil {
				rows.Close()
				return e
			}
			pending = append(pending, r)
		}
		if e = rows.Close(); e != nil {
			return e
		}
		for _, r := range pending {
			var env attemptEnvelope
			if e = json.Unmarshal([]byte(r.data), &env); e != nil {
				return e
			}
			for i, a := range env.Plan.Allowances {
				if a.Kind == "concurrency" && i < len(env.Held) && env.Held[i] > 0 {
					if e = s.adjustHold(ctx, tx, &env, i, 0); e != nil {
						return e
					}
				}
			}
			env.ConcurrencyReleased = true
			if r.state != "outcome_unknown" {
				r.state = "outcome_unknown"
			}
			env.UpdatedAt = now
			if e = s.saveAttempt(ctx, tx, env, r.state, r.version); e != nil {
				return e
			}
		}
		settled, e := tx.QueryContext(ctx, s.Query("SELECT tenant_id,attempt_id,data FROM attempts WHERE state='settled' ORDER BY updated_at LIMIT 256"))
		if e != nil {
			return e
		}
		for settled.Next() {
			var tenant, id, data string
			if e = settled.Scan(&tenant, &id, &data); e != nil {
				settled.Close()
				return e
			}
			var env attemptEnvelope
			if json.Unmarshal([]byte(data), &env) == nil && env.UpdatedAt > 0 && env.UpdatedAt < now-attemptRetentionSeconds {
				if _, e = tx.ExecContext(ctx, s.Query("DELETE FROM attempts WHERE tenant_id=? AND attempt_id=? AND state='settled'"), tenant, id); e != nil {
					settled.Close()
					return e
				}
			}
		}
		if e = settled.Close(); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("DELETE FROM usage_ledger WHERE created_at<? AND effect_kind NOT LIKE 'reconciliation:%'"), now-usageRetentionSeconds); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("DELETE FROM audit_events WHERE created_at<?"), now-auditRetentionSeconds); e != nil {
			return e
		}
		if e = s.cleanupIdempotencyTx(ctx, tx, now); e != nil {
			return e
		}
		return nil
	})
}
