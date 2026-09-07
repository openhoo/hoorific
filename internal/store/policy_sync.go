package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"time"
)

func (s *Store) syncPolicyLimit(ctx context.Context, tx *sql.Tx, p core.Principal, m core.Mutation) error {
	if m.Kind != "policy_limits" {
		return nil
	}
	var oldRaw string
	_ = tx.QueryRowContext(ctx, s.Query("SELECT data FROM resources WHERE tenant_id=? AND kind=? AND id=?"), p.TenantID, m.Kind, m.ID).Scan(&oldRaw)
	var old PolicyLimitData
	if oldRaw != "" {
		_ = json.Unmarshal([]byte(oldRaw), &old)
	}
	if old.Scope != "" {
		if _, e := tx.ExecContext(ctx, s.Query("DELETE FROM policy_limits WHERE tenant_id=? AND scope_kind=? AND scope_id=?"), p.TenantID, old.Scope, old.ScopeID); e != nil {
			return e
		}
	}
	if m.Delete {
		return nil
	}
	var next PolicyLimitData
	if e := json.Unmarshal(m.Data, &next); e != nil {
		return e
	}
	if next.Scope == "" || next.ScopeID == "" {
		return fmt.Errorf("policy scope is required")
	}
	now, e := sqlNow(ctx, tx, s.Dialect)
	if e != nil {
		return e
	}
	type dim struct {
		kind    string
		maximum int64
		window  string
	}
	dims := []dim{{"requests", next.RequestsPerMinute, "60"}, {"tokens", next.TokensPerMinute, "60"}, {"concurrency", int64(next.Concurrency), "total"}, {"jobs", int64(next.OutstandingJobs), "total"}}
	cw := next.CostWindow
	if cw == "" {
		cw = "total"
	}
	dims = append(dims, dim{"cost", next.MaxCost, cw})
	for _, d := range dims {
		if d.maximum <= 0 {
			continue
		}
		wid := currentWindowID(now, d.window)
		if d.window == "total" {
			wid = "total"
		}
		starts, ends := windowBounds(now, d.window)
		if d.window != "total" {
			if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO quota_windows(tenant_id,id,starts,ends,version) VALUES(?,?,?,?,1) ON CONFLICT(tenant_id,id) DO UPDATE SET starts=excluded.starts,ends=excluded.ends"), p.TenantID, wid, starts, ends); e != nil {
				return e
			}
		}
		reserve := int64(1)
		if d.kind == "cost" {
			reserve = d.maximum
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO policy_limits(tenant_id,scope_kind,scope_id,window_id,kind,maximum,reserve,version) VALUES(?,?,?,?,?,?,?,1) ON CONFLICT(tenant_id,scope_kind,scope_id,window_id,kind) DO UPDATE SET maximum=excluded.maximum,reserve=excluded.reserve,version=policy_limits.version+1"), p.TenantID, next.Scope, next.ScopeID, wid, d.kind, d.maximum, reserve); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO allowances(tenant_id,scope_kind,scope_id,window_id,kind,reserved,version) VALUES(?,?,?,?,?,?,1) ON CONFLICT(tenant_id,scope_kind,scope_id,window_id,kind) DO NOTHING"), p.TenantID, next.Scope, next.ScopeID, wid, d.kind, 0); e != nil {
			return e
		}
	}
	return nil
}
func windowBounds(now int64, kind string) (int64, int64) {
	switch kind {
	case "60":
		start := now / 60 * 60
		return start, start + 60
	case "daily":
		t := time.Unix(now, 0).UTC()
		start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix()
		return start, start + 86400
	case "monthly":
		t := time.Unix(now, 0).UTC()
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
		return start, time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	return 0, 0
}
