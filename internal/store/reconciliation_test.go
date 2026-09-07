package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hoorific/internal/core"
)

func TestReconcileUnknownConservativeIdempotencyAndEvidenceDelta(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
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

	maximum := int64(100)
	plan := core.AttemptPlan{TenantID: "tenant", RequestID: "request", AttemptID: "attempt", MaximumCost: &maximum,
		Allowances: []core.Allowance{{ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "cost", Maximum: maximum, Reserve: maximum}}}
	envelope, _ := json.Marshal(attemptEnvelope{Plan: plan, Held: []int64{maximum}})
	if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO allowances(tenant_id,scope_kind,scope_id,window_id,kind,reserved,version) VALUES (?,?,?,?,?,?,1)"), "tenant", "tenant", "tenant", "window", "cost", maximum); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO attempts(tenant_id,attempt_id,request_id,state,version,data) VALUES (?,?,?,?,?,?)"), "tenant", plan.AttemptID, plan.RequestID, "outcome_unknown", 1, string(envelope)); err != nil {
		t.Fatal(err)
	}

	p := bootstrapTenant(t, s, "tenant", "subject")
	callerCost := int64(1)
	conservative := core.Reconciliation{ReconciliationID: "rec-1", Mode: "charge_reserved_maximum", Reason: "provider unavailable", Cost: &callerCost}
	first, err := s.Reconcile(ctx, p, plan.AttemptID, 1, conservative)
	if err != nil {
		t.Fatal(err)
	}
	var firstData struct {
		State            string `json:"state"`
		ChargedCost      int64  `json:"charged_cost"`
		AdmissionVersion int64  `json:"admission_version"`
	}
	if err := json.Unmarshal(first.Data, &firstData); err != nil {
		t.Fatal(err)
	}
	if firstData.State != "outcome_unknown" || firstData.ChargedCost != maximum || firstData.AdmissionVersion != 2 {
		t.Fatalf("unexpected reconciliation result: %+v", firstData)
	}

	var held int64
	if err := s.DB.QueryRowContext(ctx, s.Query("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?"), "tenant", "tenant", "tenant", "window", "cost").Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != maximum {
		t.Fatalf("conservative reconciliation used caller cost: held=%d", held)
	}
	retry, err := s.Reconcile(ctx, p, plan.AttemptID, 1, conservative)
	if err != nil {
		t.Fatal(err)
	}
	if string(retry.Data) != string(first.Data) {
		t.Fatal("identical reconciliation retry changed the durable result")
	}
	altered := conservative
	altered.Reason = "different explanation"
	if _, err := s.Reconcile(ctx, p, plan.AttemptID, 999, altered); err == nil {
		t.Fatal("altered reconciliation ID was accepted")
	}

	actual := int64(70)
	evidence := core.Reconciliation{ReconciliationID: "rec-2", Mode: "provider_evidence", Reason: "provider response received", SourceReference: "provider-event-1", Cost: &actual}
	corrected, err := s.Reconcile(ctx, p, plan.AttemptID, 2, evidence)
	if err != nil {
		t.Fatal(err)
	}
	var correctedData struct {
		State       string `json:"state"`
		ChargedCost int64  `json:"charged_cost"`
		Delta       int64  `json:"delta"`
	}
	if err := json.Unmarshal(corrected.Data, &correctedData); err != nil {
		t.Fatal(err)
	}
	if correctedData.State != "outcome_unknown" || correctedData.ChargedCost != actual || correctedData.Delta != actual-maximum {
		t.Fatalf("unexpected evidence correction: %+v", correctedData)
	}
	if err := s.DB.QueryRowContext(ctx, s.Query("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?"), "tenant", "tenant", "tenant", "window", "cost").Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != actual {
		t.Fatalf("evidence correction did not adjust held cost: %d", held)
	}
}
