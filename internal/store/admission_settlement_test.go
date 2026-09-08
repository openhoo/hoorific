package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"hoorific/internal/core"
)

func TestFinalizeAttemptJobPendingSettlesEachDimensionOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	writeStoreTestKeyring(t, key, make([]byte, 32))
	var c core.BootstrapConfig
	c.SchemaVersion = 1
	c.Mode = "standalone"
	c.DataDir = dir
	c.Listeners.Inference = ":0"
	c.Listeners.Management = "127.0.0.1:0"
	c.Storage.SQLite.Path = filepath.Join(dir, "state.db")
	c.Encryption.KeyFile = key
	s, err := Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	plan := core.AttemptPlan{TenantID: "tenant", RequestID: "request", AttemptID: "attempt", Allowances: []core.Allowance{
		{ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "concurrency", Maximum: 1, Reserve: 1},
		{ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "jobs", Maximum: 1, Reserve: 1},
		{ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "tokens", Maximum: 100, Reserve: 100},
		{ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "cost", Maximum: 100, Reserve: 100},
	}}
	envelope, _ := json.Marshal(attemptEnvelope{Plan: plan})
	for _, a := range plan.Allowances {
		if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO allowances(tenant_id,scope_kind,scope_id,window_id,kind,reserved,version) VALUES (?,?,?,?,?,?,1)"), "tenant", a.ScopeKind, a.ScopeID, a.WindowID, a.Kind, a.Reserve); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO attempts(tenant_id,attempt_id,request_id,state,version,data) VALUES (?,?,?,?,?,?)"), "tenant", plan.AttemptID, plan.RequestID, "dispatch_intent", 1, string(envelope)); err != nil {
		t.Fatal(err)
	}

	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: "tenant", AttemptID: plan.AttemptID, State: "job_pending"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: "tenant", AttemptID: plan.AttemptID, State: "job_pending"}); err != nil {
		t.Fatal(err)
	}
	if got := reservedForTest(t, s, "concurrency"); got != 0 {
		t.Fatalf("concurrency released %d times; got reserved=%d", 1-got, got)
	}
	if got := reservedForTest(t, s, "jobs"); got != 1 {
		t.Fatalf("pending job reservation=%d, want 1", got)
	}

	usage := int64(40)
	cost := int64(30)
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: "tenant", AttemptID: plan.AttemptID, State: "settled", Usage: &core.Usage{Total: &usage}, ActualCost: &cost}); err != nil {
		t.Fatal(err)
	}
	if got := reservedForTest(t, s, "tokens"); got != 40 {
		t.Fatalf("token reservation=%d, want 40", got)
	}
	if got := reservedForTest(t, s, "cost"); got != 30 {
		t.Fatalf("cost reservation=%d, want 30", got)
	}
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: "tenant", AttemptID: plan.AttemptID, State: "settled", Usage: &core.Usage{Total: &usage}, ActualCost: &cost}); err != nil {
		t.Fatal(err)
	}
	if got := reservedForTest(t, s, "jobs"); got != 0 {
		t.Fatalf("settled job reservation after repeated finalization=%d, want 0", got)
	}
	var charges int
	if err := s.DB.QueryRowContext(ctx, s.Query("SELECT COUNT(1) FROM usage_ledger WHERE tenant_id=? AND attempt_id=? AND effect_kind=?"), "tenant", plan.AttemptID, "charge").Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if charges != 1 {
		t.Fatalf("settlement charge effects=%d, want 1", charges)
	}

	unknown := plan
	unknown.AttemptID = "unknown"
	unknown.RequestID = "request-unknown"
	unknown.Allowances = []core.Allowance{{ScopeKind: "tenant", ScopeID: "tenant", WindowID: "unknown-window", Kind: "cost", Maximum: 100, Reserve: 100}}
	raw, _ := json.Marshal(attemptEnvelope{Plan: unknown})
	if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO allowances(tenant_id,scope_kind,scope_id,window_id,kind,reserved,version) VALUES (?,?,?,?,?,?,1) ON CONFLICT DO NOTHING"), "tenant", "tenant", "tenant", "unknown-window", "cost", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO attempts(tenant_id,attempt_id,request_id,state,version,data) VALUES (?,?,?,?,?,?)"), "tenant", unknown.AttemptID, unknown.RequestID, "dispatch_intent", 1, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: "tenant", AttemptID: unknown.AttemptID, State: "outcome_unknown"}); err != nil {
		t.Fatal(err)
	}
	var held int64
	if err := s.DB.QueryRowContext(ctx, s.Query("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?"), "tenant", "tenant", "tenant", "unknown-window", "cost").Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 100 {
		t.Fatalf("unknown hold=%d, want 100", held)
	}
}

func reservedForTest(t *testing.T, s *Store, kind string) int64 {
	t.Helper()
	var got int64
	if err := s.DB.QueryRow("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?", "tenant", "tenant", "tenant", "window", kind).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}
