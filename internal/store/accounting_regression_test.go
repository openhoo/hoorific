package store

import (
	"context"
	"testing"

	"hoorific/internal/core"
)

func TestFinalizeAttemptRecordsObservedCostAboveReservation(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	maximum := int64(100)
	hold := int64(10)
	plan := core.AttemptPlan{
		TenantID:    "tenant",
		RequestID:   "request",
		AttemptID:   "attempt",
		MaximumCost: &maximum,
		Allowances: []core.Allowance{{
			ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window",
			Kind: "cost", Maximum: maximum, Reserve: hold,
		}},
	}
	insertRegressionAttemptFixture(t, s, plan, "accepted", []int64{hold}, "subject")
	actual := int64(50)
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{
		TenantID: plan.TenantID, RequestID: plan.RequestID, AttemptID: plan.AttemptID,
		State: "settled", ActualCost: &actual,
	}); err != nil {
		t.Fatal(err)
	}
	if got := regressionReserved(t, s, plan.TenantID, "tenant", "tenant", "window", "cost"); got != actual {
		t.Fatalf("incurred cost counter=%d, want %d", got, actual)
	}
	var amount int64
	if err := s.DB.QueryRowContext(ctx, s.Query("SELECT amount FROM usage_ledger WHERE tenant_id=? AND attempt_id=? AND effect_kind='charge'"), plan.TenantID, plan.AttemptID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != actual {
		t.Fatalf("ledger charge=%d, want %d", amount, actual)
	}
}

func TestFinalizeAttemptMissingCostRemainsReconcilable(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	maximum := int64(100)
	hold := int64(10)
	plan := core.AttemptPlan{
		TenantID:    "tenant",
		RequestID:   "request",
		AttemptID:   "attempt",
		MaximumCost: &maximum,
		Allowances: []core.Allowance{{
			ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window",
			Kind: "cost", Maximum: maximum, Reserve: hold,
		}},
	}
	insertRegressionAttemptFixture(t, s, plan, "accepted", []int64{hold}, "subject")
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{
		TenantID: plan.TenantID, RequestID: plan.RequestID, AttemptID: plan.AttemptID,
		State: "settled",
	}); err != nil {
		t.Fatal(err)
	}
	state, version := regressionAttemptSnapshot(t, s, plan.TenantID, plan.AttemptID)
	if state != "outcome_unknown" || version != 2 {
		t.Fatalf("missing-cost state=%s/%d, want outcome_unknown/2", state, version)
	}
	if got := regressionReserved(t, s, plan.TenantID, "tenant", "tenant", "window", "cost"); got != hold {
		t.Fatalf("missing-cost hold=%d, want %d", got, hold)
	}
	if got := regressionLedgerCount(t, s, plan.TenantID, plan.AttemptID); got != 0 {
		t.Fatalf("missing-cost ledger rows=%d, want 0", got)
	}

	principal := bootstrapTenant(t, s, plan.TenantID, "subject")
	recovered := int64(7)
	if _, err := s.Reconcile(ctx, principal, plan.AttemptID, version, core.Reconciliation{
		ReconciliationID: "recovery",
		Mode:             "provider_evidence",
		Reason:           "provider usage recovered",
		SourceReference:  "provider-event",
		Cost:             &recovered,
	}); err != nil {
		t.Fatal(err)
	}
	if got := regressionReserved(t, s, plan.TenantID, "tenant", "tenant", "window", "cost"); got != recovered {
		t.Fatalf("recovered cost hold=%d, want %d", got, recovered)
	}
}
func TestFinalizeAttemptOverrunAddsToSharedAllowance(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	maximum := int64(100)
	hold := int64(10)
	plan := core.AttemptPlan{
		TenantID:    "tenant",
		RequestID:   "request-shared",
		AttemptID:   "attempt-shared",
		MaximumCost: &maximum,
		Allowances: []core.Allowance{{
			ScopeKind: "tenant", ScopeID: "tenant", WindowID: "window",
			Kind: "cost", Maximum: maximum, Reserve: hold,
		}},
	}
	insertRegressionAttemptFixture(t, s, plan, "accepted", []int64{hold}, "subject")
	// Simulate another admitted attempt holding 15 units in the same shared
	// counter. Settling this attempt at 50 must add its 40-unit overrun.
	if _, err := s.DB.ExecContext(ctx, s.Query("UPDATE allowances SET reserved=? WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?"), 25, "tenant", "tenant", "tenant", "window", "cost"); err != nil {
		t.Fatal(err)
	}
	actual := int64(50)
	if err := s.FinalizeAttempt(ctx, core.AttemptOutcome{
		TenantID: plan.TenantID, RequestID: plan.RequestID, AttemptID: plan.AttemptID,
		State: "settled", ActualCost: &actual,
	}); err != nil {
		t.Fatal(err)
	}
	if got := regressionReserved(t, s, plan.TenantID, "tenant", "tenant", "window", "cost"); got != 65 {
		t.Fatalf("shared incurred cost counter=%d, want 65", got)
	}
}

func TestBoundModelExplicitCacheWriteWithoutRateLeavesCostUnknown(t *testing.T) {
	maxTokens := int64(8)
	contextLimit := int64(32)
	outputLimit := int64(16)
	inputRate := int64(10)
	outputRate := int64(20)
	unitCost := int64(999)
	out := core.AttemptPlan{Operation: "generate"}
	err := boundModel(&out, core.Model{
		ContextLimit: &contextLimit,
		OutputLimit:  &outputLimit,
		Price: &core.PriceSchedule{
			Version:          "v1",
			InputPerMillion:  &inputRate,
			OutputPerMillion: &outputRate,
			MaximumUnitCost:  &unitCost,
			UnitOperation:    "generate",
		},
	}, []byte(`{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.InputBound == nil || out.OutputBound == nil || *out.OutputBound != maxTokens {
		t.Fatalf("token bounds were not retained: %#v/%#v", out.InputBound, out.OutputBound)
	}
	if out.MaximumCost != nil {
		t.Fatalf("explicit cache write received guessed cost bound %d", *out.MaximumCost)
	}
}

func TestCachePricingIgnoresOpaqueCacheLookingProperties(t *testing.T) {
	contextLimit := int64(32)
	outputLimit := int64(16)
	inputRate := int64(10)
	outputRate := int64(20)
	cachedRate := int64(2)
	writeRate := int64(3)
	out := core.AttemptPlan{Operation: "generate"}
	err := boundModel(&out, core.Model{
		ContextLimit: &contextLimit,
		OutputLimit:  &outputLimit,
		Price: &core.PriceSchedule{
			Version:                "v1",
			InputPerMillion:        &inputRate,
			OutputPerMillion:       &outputRate,
			CachedInputPerMillion:  &cachedRate,
			CacheWrite5mPerMillion: &writeRate,
		},
	}, []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"cache_control is just text","metadata":{"cache_control":{"type":"ephemeral"}}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"cachePoint":{"type":"default"}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.MaximumCost == nil {
		t.Fatal("opaque cache-looking properties incorrectly removed ordinary cost bound")
	}
}
