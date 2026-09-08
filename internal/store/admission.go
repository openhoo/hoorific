package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/policy"
)

type attemptEnvelope struct {
	Plan                core.AttemptPlan     `json:"plan"`
	Submitted           core.AttemptPlan     `json:"submitted,omitempty"`
	SubjectID           string               `json:"subject_id,omitempty"`
	Outcome             *core.AttemptOutcome `json:"outcome,omitempty"`
	Held                []int64              `json:"held,omitempty"`
	ChargedCost         int64                `json:"charged_cost,omitempty"`
	ChargedCostKnown    bool                 `json:"charged_cost_known,omitempty"`
	Reconciled          bool                 `json:"reconciled,omitempty"`
	AcceptedEvidence    string               `json:"accepted_evidence,omitempty"`
	UpdatedAt           int64                `json:"updated_at,omitempty"`
	ConcurrencyReleased bool                 `json:"concurrency_released,omitempty"`
	Price               *core.PriceSchedule  `json:"price,omitempty"`
}

func allowanceAmount(a core.Allowance) int64 { return a.Reserve }

func decodeAttemptPlan(data string) (core.AttemptPlan, error) {
	var env attemptEnvelope
	if err := json.Unmarshal([]byte(data), &env); err == nil && env.Plan.AttemptID != "" {
		return env.Plan, nil
	}
	var p core.AttemptPlan
	err := json.Unmarshal([]byte(data), &p)
	return p, err
}

func validatePlan(p core.AttemptPlan) error {
	if p.TenantID == "" || p.RequestID == "" || p.AttemptID == "" {
		return errors.New("attempt identity is required")
	}
	if p.Deadline.IsZero() {
		return errors.New("attempt deadline is required")
	}
	for _, a := range p.Allowances {
		if a.ScopeKind == "" || a.ScopeID == "" || a.WindowID == "" || a.Kind == "" || a.Maximum < 0 || a.Reserve < 0 || a.Reserve > a.Maximum {
			return errors.New("invalid allowance")
		}
	}
	return nil
}

func (s *Store) BeginAttempt(ctx context.Context, p core.AttemptPlan) (permit core.AttemptPermit, err error) {
	if err = validatePlan(p); err != nil {
		return permit, err
	}
	submitted := p
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		now, e := sqlNow(ctx, tx, s.Dialect)
		if e != nil {
			return e
		}
		if p.Deadline.Unix() <= now {
			return problem("deadline_exceeded", 408, "attempt deadline has expired")
		}
		authoritative, e := s.derivePlanTx(ctx, tx, p)
		if e != nil {
			return e
		}
		p = authoritative
		var subjectID string
		if principal, ok := core.PrincipalFromContext(ctx); ok {
			subjectID = principal.SubjectID
		}
		var keyRev int64
		var revoked int
		if p.KeyID != "" {
			e = tx.QueryRowContext(ctx, s.Query("SELECT version,revoked FROM api_keys WHERE tenant_id=? AND id=?"), p.TenantID, p.KeyID).Scan(&keyRev, &revoked)
			if e != nil || revoked != 0 || p.KeyRevision != 0 && keyRev != p.KeyRevision {
				return problem("authentication_required", 401, "key was rotated or revoked")
			}
		}
		var cfg int64
		if e = tx.QueryRowContext(ctx, s.Query("SELECT revision FROM config_state WHERE id=1")).Scan(&cfg); e != nil {
			return fmt.Errorf("read config revision: %w", e)
		}
		if p.ConfigRevision != 0 && cfg != p.ConfigRevision {
			return problem("configuration_stale", 409, "configuration changed; retry planning")
		}
		var state, data string
		var version int64
		e = tx.QueryRowContext(ctx, s.Query("SELECT state,data,version FROM attempts WHERE tenant_id=? AND attempt_id=?"), p.TenantID, p.AttemptID).Scan(&state, &data, &version)
		if e == nil {
			var old attemptEnvelope
			if json.Unmarshal([]byte(data), &old) != nil {
				return problem("invalid_configuration", 500, "stored attempt envelope is invalid")
			}
			original := old.Submitted
			if original.AttemptID == "" {
				original = old.Plan
			}
			if !sameAttempt(original, submitted) {
				return problem("conflict", 409, "attempt identity is already bound to another plan")
			}
			if state == "dispatch_intent" || state == "accepted" {
				permit = core.AttemptPermit{TenantID: p.TenantID, RequestID: p.RequestID, AttemptID: p.AttemptID, ConfigRevision: old.Plan.ConfigRevision}
				return nil
			}
			return problem("attempt_already_settled", 409, "attempt already finalized")
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var requestCount int
		if e = tx.QueryRowContext(ctx, s.Query("SELECT COUNT(1) FROM attempts WHERE tenant_id=? AND request_id=?"), p.TenantID, p.RequestID).Scan(&requestCount); e != nil {
			return e
		}
		seenRequest := requestCount > 0
		held := make([]int64, len(p.Allowances))
		for i, a := range p.Allowances {
			amount := allowanceAmount(a)
			if a.Kind == "cost" {
				if p.MaximumCost == nil {
					return problem("unsupported_policy", 400, "cost policy requires a bounded model price")
				}
				amount = *p.MaximumCost
			} else if a.Kind == "tokens" {
				if p.InputBound == nil || p.OutputBound == nil {
					return problem("unsupported_policy", 400, "token policy requires bounded input and output")
				}
				var sumErr error
				amount, sumErr = policy.CheckedAdd(*p.InputBound, *p.OutputBound)
				if sumErr != nil {
					return problem("unsupported_policy", 400, "token bound is not representable")
				}
			} else if a.Kind == "requests" || a.Kind == "concurrency" || a.Kind == "jobs" {
				amount = 1
			}
			if amount == 0 || (seenRequest && a.Kind == "requests") {
				continue
			}
			var policyMax int64
			e = tx.QueryRowContext(ctx, s.Query("SELECT maximum FROM policy_limits WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=?"), p.TenantID, a.ScopeKind, a.ScopeID, a.WindowID, a.Kind).Scan(&policyMax)
			if errors.Is(e, sql.ErrNoRows) || e != nil {
				if errors.Is(e, sql.ErrNoRows) {
					return problem("configuration_stale", 409, "allowance policy is not configured")
				}
				return e
			}
			if a.Maximum <= 0 || policyMax <= 0 || a.Maximum > policyMax || amount > policyMax {
				return problem("quota_exceeded", 429, "required allowance exceeds current policy")
			}
			var starts, ends int64
			if a.WindowID != "total" {
				if e = tx.QueryRowContext(ctx, s.Query("SELECT starts,ends FROM quota_windows WHERE tenant_id=? AND id=?"), p.TenantID, a.WindowID).Scan(&starts, &ends); e != nil || now < starts || now >= ends {
					return problem("quota_exceeded", 429, "allowance window is expired")
				}
			}
			q := s.Query("UPDATE allowances SET reserved=reserved+?,version=version+1 WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=? AND reserved+?<=?")
			r, e := tx.ExecContext(ctx, q, amount, p.TenantID, a.ScopeKind, a.ScopeID, a.WindowID, a.Kind, amount, a.Maximum)
			if e != nil {
				return e
			}
			n, _ := r.RowsAffected()
			if n != 1 {
				return problem("quota_exceeded", 429, "allowance exceeded")
			}
			held[i] = amount
		}
		// Pin the authoritative schedule with the admission, not a later
		// mutable model snapshot or caller-supplied settlement price.
		var pricing struct {
			Price *core.PriceSchedule `json:"price"`
		}
		if p.ModelID != "" {
			var data string
			if e = tx.QueryRowContext(ctx, s.Query("SELECT data FROM resources WHERE tenant_id=? AND kind='models' AND id=?"), p.TenantID, p.ModelID).Scan(&data); e != nil {
				return e
			}
			if e = json.Unmarshal([]byte(data), &pricing); e != nil {
				return e
			}
			if pricing.Price != nil && pricing.Price.Version != p.PriceVersion {
				return problem("configuration_stale", 409, "price version changed")
			}
		}
		env := attemptEnvelope{Plan: p, Submitted: submitted, SubjectID: subjectID, Held: held, UpdatedAt: now, Price: clonePrice(pricing.Price)}
		b, _ := json.Marshal(env)
		q := s.Query("INSERT INTO attempts(tenant_id,attempt_id,request_id,state,version,deadline,updated_at,data) VALUES (?,?,?,?,?,?,?,?)")
		if _, e = tx.ExecContext(ctx, q, p.TenantID, p.AttemptID, p.RequestID, "dispatch_intent", cfg, p.Deadline.Unix(), now, string(b)); e != nil {
			return e
		}
		permit = core.AttemptPermit{TenantID: p.TenantID, RequestID: p.RequestID, AttemptID: p.AttemptID, ConfigRevision: cfg}
		return nil
	})
	return permit, err
}

func sameAttempt(a, b core.AttemptPlan) bool {
	return a.TenantID == b.TenantID && a.RequestID == b.RequestID && a.AttemptID == b.AttemptID && a.KeyID == b.KeyID && a.ConnectionID == b.ConnectionID && a.AccountID == b.AccountID && a.ModelID == b.ModelID && a.KeyRevision == b.KeyRevision && a.ConfigRevision == b.ConfigRevision && a.Deadline.Equal(b.Deadline)
}

func (s *Store) loadAttempt(ctx context.Context, tx *sql.Tx, tenant, id string) (attemptEnvelope, string, int64, error) {
	var env attemptEnvelope
	var state, data string
	var version int64
	err := tx.QueryRowContext(ctx, s.Query("SELECT state,data,version FROM attempts WHERE tenant_id=? AND attempt_id=?"), tenant, id).Scan(&state, &data, &version)
	if err != nil {
		return env, "", 0, err
	}
	if err = json.Unmarshal([]byte(data), &env); err != nil {
		return env, "", 0, err
	}
	if !env.ChargedCostKnown && (env.ChargedCost != 0 || env.Outcome != nil && env.Outcome.ActualCost != nil) {
		env.ChargedCostKnown = true
	}
	// Settlement belongs to the immutable admission, even if its key or
	// operator has since been revoked. Never use payload coordinates to
	// redirect ledger or hold mutations to another admission.
	if env.Plan.TenantID != tenant || env.Plan.AttemptID != id || env.Plan.RequestID == "" {
		return env, "", 0, problem("conflict", 409, "stored attempt identity differs")
	}
	if len(env.Held) < len(env.Plan.Allowances) {
		held := make([]int64, len(env.Plan.Allowances))
		copy(held, env.Held)
		for i := len(env.Held); i < len(env.Plan.Allowances); i++ {
			held[i] = allowanceAmount(env.Plan.Allowances[i])
		}
		env.Held = held
	}
	return env, state, version, nil
}

func (s *Store) saveAttempt(ctx context.Context, tx *sql.Tx, env attemptEnvelope, state string, version int64) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, s.Query("UPDATE attempts SET state=?,version=version+1,deadline=?,updated_at=?,data=? WHERE tenant_id=? AND attempt_id=? AND version=?"), state, env.Plan.Deadline.Unix(), env.UpdatedAt, string(b), env.Plan.TenantID, env.Plan.AttemptID, version)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return errors.New("attempt changed during finalization")
	}
	return nil
}

func (s *Store) adjustHold(ctx context.Context, tx *sql.Tx, env *attemptEnvelope, index int, target int64) error {
	return s.adjustHoldMode(ctx, tx, env, index, target, false)
}

// adjustIncurredHold records observed spend or usage even when it exceeds the
// configured policy maximum. The overrun is real accounting evidence; future
// admissions will fail the normal maximum guard while the counter remains
// truthful.
func (s *Store) adjustIncurredHold(ctx context.Context, tx *sql.Tx, env *attemptEnvelope, index int, target int64) error {
	return s.adjustHoldMode(ctx, tx, env, index, target, true)
}

func (s *Store) adjustHoldMode(ctx context.Context, tx *sql.Tx, env *attemptEnvelope, index int, target int64, allowOverrun bool) error {
	if index < 0 || index >= len(env.Plan.Allowances) || target < 0 {
		return errors.New("invalid hold adjustment")
	}
	current := env.Held[index]
	if target == current {
		return nil
	}
	a := env.Plan.Allowances[index]
	if target < current {
		delta := current - target
		// The row is shared by every concurrent attempt. Subtract only this
		// admission's prior hold, and require the row to contain that amount;
		// clamping to zero could erase another attempt's reservation.
		r, err := tx.ExecContext(ctx, s.Query("UPDATE allowances SET reserved=reserved-?,version=version+1 WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=? AND reserved>=?"), delta, env.Plan.TenantID, a.ScopeKind, a.ScopeID, a.WindowID, a.Kind, delta)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return errors.New("allowance changed during settlement")
		}
	} else {
		delta := target - current
		if allowOverrun {
			// Actual spend is shared accounting state. Add this admission's
			// overrun instead of assigning an absolute total, otherwise a
			// concurrent hold would be silently discarded. The predicate
			// avoids overflowing the signed SQL integer before the update.
			maxInt := int64(^uint64(0) >> 1)
			r, err := tx.ExecContext(ctx, s.Query("UPDATE allowances SET reserved=reserved+?,version=version+1 WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=? AND reserved<=?"), delta, env.Plan.TenantID, a.ScopeKind, a.ScopeID, a.WindowID, a.Kind, maxInt-delta)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n != 1 {
				return errors.New("allowance changed or overflows during settlement")
			}
		} else {
			// Admission-time adjustments remain bounded by the configured
			// maximum, while using a subtraction predicate to avoid SQL
			// integer overflow in reserved+delta.
			if a.Maximum < delta {
				return problem("quota_exceeded", 429, "allowance adjustment exceeded")
			}
			r, err := tx.ExecContext(ctx, s.Query("UPDATE allowances SET reserved=reserved+?,version=version+1 WHERE tenant_id=? AND scope_kind=? AND scope_id=? AND window_id=? AND kind=? AND reserved<=?"), delta, env.Plan.TenantID, a.ScopeKind, a.ScopeID, a.WindowID, a.Kind, a.Maximum-delta)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n != 1 {
				return problem("quota_exceeded", 429, "allowance adjustment exceeded")
			}
		}
	}
	env.Held[index] = target
	return nil
}
func normalizeUsage(input *core.Usage) (usage *core.Usage, valid, totalKnown bool) {
	if input == nil {
		return nil, true, false
	}
	copy := *input
	for _, value := range []*int64{
		copy.Input, copy.Output, copy.Total, copy.CachedInput,
		copy.CacheWriteInput, copy.CacheWrite5mInput, copy.CacheWrite1hInput,
		copy.ReasoningOutput, copy.ToolInput,
	} {
		if value != nil && *value < 0 {
			return nil, false, false
		}
	}
	totalKnown = copy.Total != nil
	if copy.Input != nil && copy.Output != nil {
		total, err := policy.CheckedAdd(*copy.Input, *copy.Output)
		if err != nil {
			totalKnown = false
		} else if copy.Total == nil {
			copy.Total = &total
			totalKnown = true
		} else {
			totalKnown = *copy.Total == total
		}
	}
	return &copy, true, totalKnown
}

func (s *Store) FinalizeAttempt(ctx context.Context, o core.AttemptOutcome) error {
	if o.TenantID == "" || o.AttemptID == "" {
		return errors.New("attempt identity is required")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		env, state, version, err := s.loadAttempt(ctx, tx, o.TenantID, o.AttemptID)
		if err != nil {
			return err
		}
		if o.RequestID != "" && o.RequestID != env.Plan.RequestID {
			return problem("conflict", 409, "request identity differs")
		}
		next := o.State
		if next == "" {
			next = "settled"
		}
		if next != "settled" && next != "outcome_unknown" && next != "not_executed" && next != "job_pending" {
			return errors.New("invalid terminal attempt state")
		}
		switch state {
		case "settled", "not_executed":
			if state == next {
				return nil
			}
			return problem("attempt_already_settled", 409, "attempt already finalized")
		case "outcome_unknown":
			if next == "outcome_unknown" {
				return nil
			}
			return problem("attempt_already_settled", 409, "attempt already finalized")
		case "job_pending":
			if next == "job_pending" {
				return nil
			}
			if next != "settled" && next != "outcome_unknown" {
				return problem("attempt_already_settled", 409, "attempt already finalized")
			}
		case "dispatch_intent", "accepted":
		default:
			return errors.New("invalid attempt state")
		}

		usage, usageValid, totalKnown := normalizeUsage(o.Usage)
		o.Usage = usage
		if !usageValid {
			o.Usage = nil
		}
		if o.ActualCost != nil && *o.ActualCost < 0 {
			o.ActualCost = nil
			if next == "settled" {
				next = "outcome_unknown"
			}
		}
		if next == "settled" && o.ActualCost == nil && usageValid {
			if cost, e := policy.EstimateUsageCost(o.Usage, env.Price); e == nil {
				o.ActualCost = &cost
			}
		}
		costKnown := next != "not_executed" && o.ActualCost != nil
		needsCost, needsTokens := false, false
		for _, a := range env.Plan.Allowances {
			switch a.Kind {
			case "cost":
				needsCost = true
			case "tokens":
				needsTokens = true
			}
		}
		// A successful response without the evidence required by its held
		// dimensions remains reconcilable. Control-only requests can settle
		// without fabricated usage or cost.
		if next == "settled" && (needsCost && !costKnown || needsTokens && !totalKnown) {
			next = "outcome_unknown"
		}
		if next == "not_executed" {
			costKnown = false
		}
		for i, a := range env.Plan.Allowances {
			target := env.Held[i]
			switch {
			case next == "not_executed":
				target = 0
			case a.Kind == "concurrency" && (next == "settled" || next == "job_pending"):
				target = 0
			case a.Kind == "jobs" && next == "settled":
				target = 0
			case a.Kind == "tokens" && (next == "settled" || next == "outcome_unknown") && usageValid && totalKnown && o.Usage != nil && o.Usage.Total != nil:
				target = *o.Usage.Total
			case a.Kind == "cost" && (next == "settled" || next == "outcome_unknown") && costKnown:
				target = *o.ActualCost
			}
			if target > env.Held[i] {
				err = s.adjustIncurredHold(ctx, tx, &env, i, target)
			} else {
				err = s.adjustHold(ctx, tx, &env, i, target)
			}
			if err != nil {
				return err
			}
		}
		if (next == "settled" || next == "outcome_unknown") && costKnown && !env.ChargedCostKnown {
			effect := o.AttemptID + ":charge"
			payload, e := json.Marshal(o)
			if e != nil {
				return e
			}
			if _, err = tx.ExecContext(ctx, s.Query("INSERT INTO usage_ledger(tenant_id,effect_id,attempt_id,effect_kind,amount,data,created_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT (tenant_id,effect_id) DO NOTHING"), o.TenantID, effect, o.AttemptID, "charge", *o.ActualCost, string(payload), time.Now().Unix()); err != nil {
				return err
			}
			env.ChargedCost = *o.ActualCost
			env.ChargedCostKnown = true
		}
		o.State = next
		env.Outcome = &o
		env.UpdatedAt = time.Now().Unix()
		return s.saveAttempt(ctx, tx, env, next, version)
	})
}

func (s *Store) MarkAccepted(ctx context.Context, permit core.AttemptPermit, evidence string) error {
	if permit.TenantID == "" || permit.AttemptID == "" || evidence == "" {
		return errors.New("accepted evidence is required")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		env, state, version, err := s.loadAttempt(ctx, tx, permit.TenantID, permit.AttemptID)
		if err != nil {
			return err
		}
		if state == "accepted" {
			if env.AcceptedEvidence == evidence {
				return nil
			}
			return problem("conflict", 409, "accepted evidence differs")
		}
		if state != "dispatch_intent" {
			return errors.New("attempt is not dispatchable")
		}
		if permit.RequestID != "" && permit.RequestID != env.Plan.RequestID {
			return problem("conflict", 409, "request identity differs")
		}
		env.AcceptedEvidence = evidence
		env.UpdatedAt = time.Now().Unix()
		return s.saveAttempt(ctx, tx, env, "accepted", version)
	})
}

var _ core.AdmissionStore = (*Store)(nil)
