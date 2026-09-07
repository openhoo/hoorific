package store

import (
	"context"
	"encoding/json"
	"testing"

	"hoorific/internal/core"
)

func insertRegressionAttemptFixture(t *testing.T, s *Store, plan core.AttemptPlan, state string, held []int64, subject string) {
	t.Helper()
	ctx := context.Background()
	for i, allowance := range plan.Allowances {
		amount := int64(0)
		if i < len(held) {
			amount = held[i]
		}
		if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO allowances(tenant_id,scope_kind,scope_id,window_id,kind,reserved,version) VALUES (?,?,?,?,?,?,1)"), plan.TenantID, allowance.ScopeKind, allowance.ScopeID, allowance.WindowID, allowance.Kind, amount); err != nil {
			t.Fatal(err)
		}
	}
	envelope, err := json.Marshal(attemptEnvelope{Plan: plan, Held: held, SubjectID: subject})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, s.Query("INSERT INTO attempts(tenant_id,attempt_id,request_id,state,version,data) VALUES (?,?,?,?,?,?)"), plan.TenantID, plan.AttemptID, plan.RequestID, state, 1, string(envelope)); err != nil {
		t.Fatal(err)
	}
}

func regressionAttemptSnapshot(t *testing.T, s *Store, tenant, attempt string) (string, int64) {
	t.Helper()
	var state string
	var version int64
	if err := s.DB.QueryRowContext(context.Background(), s.Query("SELECT state,version FROM attempts WHERE tenant_id=? AND attempt_id=?"), tenant, attempt).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	return state, version
}

func regressionReserved(t *testing.T, s *Store, tenant, scopeKind, scopeID, windowID, kind string) int64 {
	t.Helper()
	var reserved int64
	if err := s.DB.QueryRowContext(context.Background(), s.Query("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?"), tenant, scopeKind, scopeID, windowID, kind).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	return reserved
}

func regressionLedgerCount(t *testing.T, s *Store, tenant, attempt string) int {
	t.Helper()
	var count int
	if err := s.DB.QueryRowContext(context.Background(), s.Query("SELECT COUNT(1) FROM usage_ledger WHERE tenant_id=? AND attempt_id=?"), tenant, attempt).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestFinalizeAttemptRejectsForbiddenTerminalReversals(t *testing.T) {
	cases := []struct {
		name, initial, next string
	}{
		{"settled to not_executed", "settled", "not_executed"},
		{"settled to outcome_unknown", "settled", "outcome_unknown"},
		{"settled to job_pending", "settled", "job_pending"},
		{"not_executed to settled", "not_executed", "settled"},
		{"not_executed to outcome_unknown", "not_executed", "outcome_unknown"},
		{"not_executed to job_pending", "not_executed", "job_pending"},
		{"outcome_unknown to not_executed", "outcome_unknown", "not_executed"},
		{"outcome_unknown to job_pending", "outcome_unknown", "job_pending"},
		{"job_pending to not_executed", "job_pending", "not_executed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newTenancyAuthTestStore(t)
			maximum := int64(100)
			hold := int64(1)
			if tc.initial == "settled" || tc.initial == "not_executed" {
				hold = 0
			}
			plan := core.AttemptPlan{
				TenantID:    "tenant",
				RequestID:   "request",
				AttemptID:   "attempt",
				MaximumCost: &maximum,
				Allowances: []core.Allowance{{
					ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "concurrency", Maximum: 1, Reserve: 1,
				}},
			}
			insertRegressionAttemptFixture(t, s, plan, tc.initial, []int64{hold}, "subject")
			beforeState, beforeVersion := regressionAttemptSnapshot(t, s, plan.TenantID, plan.AttemptID)
			if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{
				TenantID: plan.TenantID, RequestID: plan.RequestID, AttemptID: plan.AttemptID, State: tc.next,
			}); err == nil {
				t.Fatalf("forbidden transition %s -> %s was accepted", tc.initial, tc.next)
			}
			afterState, afterVersion := regressionAttemptSnapshot(t, s, plan.TenantID, plan.AttemptID)
			if afterState != beforeState || afterVersion != beforeVersion {
				t.Fatalf("rejected transition changed attempt from %s/%d to %s/%d", beforeState, beforeVersion, afterState, afterVersion)
			}
			if got := regressionReserved(t, s, plan.TenantID, "tenant", "tenant", "window", "concurrency"); got != hold {
				t.Fatalf("rejected transition changed hold from %d to %d", hold, got)
			}
			if got := regressionLedgerCount(t, s, plan.TenantID, plan.AttemptID); got != 0 {
				t.Fatalf("rejected transition wrote %d ledger rows", got)
			}
		})
	}
}

func TestFinalizeAttemptUnknownExactRepeatLeavesHoldUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	maximum := int64(100)
	plan := core.AttemptPlan{
		TenantID:    "tenant",
		RequestID:   "request",
		AttemptID:   "attempt",
		MaximumCost: &maximum,
		Allowances: []core.Allowance{{
			ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window", Kind: "cost", Maximum: maximum, Reserve: maximum,
		}},
	}
	insertRegressionAttemptFixture(t, s, plan, "outcome_unknown", []int64{maximum}, "subject")
	outcome := core.AttemptOutcome{TenantID: plan.TenantID, RequestID: plan.RequestID, AttemptID: plan.AttemptID, State: "outcome_unknown"}
	if err := s.FinalizeAttempt(ctx, outcome); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeAttempt(ctx, outcome); err != nil {
		t.Fatal(err)
	}
	state, version := regressionAttemptSnapshot(t, s, plan.TenantID, plan.AttemptID)
	if state != "outcome_unknown" || version != 1 {
		t.Fatalf("exact unknown retry changed attempt to %s/%d", state, version)
	}
	if got := regressionReserved(t, s, plan.TenantID, "tenant", "tenant", "window", "cost"); got != maximum {
		t.Fatalf("exact unknown retry changed held cost to %d", got)
	}
	if got := regressionLedgerCount(t, s, plan.TenantID, plan.AttemptID); got != 0 {
		t.Fatalf("unknown retry wrote %d ledger rows", got)
	}
}

func TestFinalizeAttemptSettlesAfterKeyRevocationAndChargesOnce(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant", "owner")
	regressionConfigureKeyAdmission(t, s, owner, owner.TenantID, 1)
	key, token := regressionIssueKey(t, s, owner, "inflight-key")
	if _, err := s.AuthenticateKey(ctx, token); err != nil {
		t.Fatalf("fresh API key must authenticate before admission: %v", err)
	}
	configRevision, err := s.ConfigRevision(ctx, owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	plan := regressionKeyAttemptPlan(owner.TenantID, key.ID, key.Version, configRevision, "inflight-request", "inflight-attempt")
	if _, err = s.BeginAttempt(ctx, plan); err != nil {
		t.Fatalf("valid key admission failed: %v", err)
	}

	if _, err = s.Mutate(ctx, owner, core.Mutation{Kind: "api_keys", ID: key.ID, ExpectedVersion: key.Version, Delete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateKey(ctx, token); err == nil {
		t.Fatal("revoked API key still authenticated")
	}
	currentRevision, err := s.ConfigRevision(ctx, owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	newPlan := regressionKeyAttemptPlan(owner.TenantID, key.ID, key.Version, currentRevision, "new-request", "new-attempt")
	if _, err = s.BeginAttempt(ctx, newPlan); err == nil {
		t.Fatal("revoked API key admitted new work")
	}

	cost := int64(7)
	outcome := core.AttemptOutcome{TenantID: plan.TenantID, RequestID: plan.RequestID, AttemptID: plan.AttemptID, State: "settled", ActualCost: &cost}
	if err = s.FinalizeAttempt(ctx, outcome); err != nil {
		t.Fatalf("in-flight admission must settle after key revocation: %v", err)
	}
	if err = s.FinalizeAttempt(ctx, outcome); err != nil {
		t.Fatalf("exact settlement retry must be idempotent: %v", err)
	}
	if got := regressionReserved(t, s, owner.TenantID, "tenant", owner.TenantID, "total", "concurrency"); got != 0 {
		t.Fatalf("settlement left concurrency reserved at %d", got)
	}
	if got := regressionLedgerCount(t, s, owner.TenantID, plan.AttemptID); got != 1 {
		t.Fatalf("settlement wrote %d charge rows, want one", got)
	}
	var amount int64
	if err = s.DB.QueryRowContext(ctx, s.Query("SELECT amount FROM usage_ledger WHERE tenant_id=? AND attempt_id=? AND effect_kind='charge'"), owner.TenantID, plan.AttemptID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != cost {
		t.Fatalf("settlement charge=%d, want %d", amount, cost)
	}
}

func TestFinalizeAttemptRejectsMismatchedPlanIdentityWithoutMutation(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant", "owner")
	regressionConfigureKeyAdmission(t, s, owner, owner.TenantID, 2)
	key, _ := regressionIssueKey(t, s, owner, "identity-key")
	configRevision, err := s.ConfigRevision(ctx, owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	planA := regressionKeyAttemptPlan(owner.TenantID, key.ID, key.Version, configRevision, "request-a", "attempt-a")
	planB := regressionKeyAttemptPlan(owner.TenantID, key.ID, key.Version, configRevision, "request-b", "attempt-b")
	if _, err = s.BeginAttempt(ctx, planA); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginAttempt(ctx, planB); err != nil {
		t.Fatal(err)
	}
	beforeState, beforeVersion := regressionAttemptSnapshot(t, s, planA.TenantID, planA.AttemptID)
	wrongCost := int64(3)
	if err = s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: planA.TenantID, RequestID: planB.RequestID, AttemptID: planA.AttemptID, State: "settled", ActualCost: &wrongCost}); err == nil {
		t.Fatal("settlement with another plan's request identity was accepted")
	}
	if err = s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: "other-tenant", RequestID: planA.RequestID, AttemptID: planA.AttemptID, State: "settled", ActualCost: &wrongCost}); err == nil {
		t.Fatal("settlement with another tenant identity was accepted")
	}
	afterState, afterVersion := regressionAttemptSnapshot(t, s, planA.TenantID, planA.AttemptID)
	if afterState != beforeState || afterVersion != beforeVersion {
		t.Fatalf("mismatched settlement changed attempt A from %s/%d to %s/%d", beforeState, beforeVersion, afterState, afterVersion)
	}
	if got := regressionReserved(t, s, owner.TenantID, "tenant", owner.TenantID, "total", "concurrency"); got != 2 {
		t.Fatalf("mismatched settlement changed shared hold to %d", got)
	}
	if got := regressionLedgerCount(t, s, owner.TenantID, planA.AttemptID); got != 0 {
		t.Fatalf("mismatched settlement wrote %d charge rows for A", got)
	}
	if got := regressionLedgerCount(t, s, owner.TenantID, planB.AttemptID); got != 0 {
		t.Fatalf("mismatched settlement wrote %d charge rows for B", got)
	}

	cost := int64(4)
	if err = s.FinalizeAttempt(ctx, core.AttemptOutcome{TenantID: planA.TenantID, RequestID: planA.RequestID, AttemptID: planA.AttemptID, State: "settled", ActualCost: &cost}); err != nil {
		t.Fatal(err)
	}
	if got := regressionReserved(t, s, owner.TenantID, "tenant", owner.TenantID, "total", "concurrency"); got != 1 {
		t.Fatalf("settling A changed B's hold; shared hold=%d", got)
	}
	if got := regressionLedgerCount(t, s, owner.TenantID, planA.AttemptID); got != 1 {
		t.Fatalf("valid settlement wrote %d charge rows for A", got)
	}
	if got := regressionLedgerCount(t, s, owner.TenantID, planB.AttemptID); got != 0 {
		t.Fatalf("valid settlement of A wrote %d charge rows for B", got)
	}
}
