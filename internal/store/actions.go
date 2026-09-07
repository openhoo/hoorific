package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hoorific/internal/admin"
	"hoorific/internal/core"
	"strings"
	"time"
)

// Execute implements the state-changing admin action surface whose effects are
// wholly durable. Network operations (test/discover/OAuth exchange/job polling)
// remain explicit runtime injections rather than being faked by storage.
func (s *Store) Execute(ctx context.Context, p core.Principal, kind, id, action string, expected int64, data json.RawMessage) (json.RawMessage, error) {
	switch kind {
	case "api_keys":
		switch action {
		case "issue":
			r, t, e := s.IssueKey(ctx, p, id, data)
			if e != nil {
				return nil, e
			}
			return json.Marshal(struct {
				Resource core.Resource `json:"resource"`
				Token    string        `json:"token"`
			}{r, t})
		case "rotate":
			r, t, e := s.RotateKey(ctx, p, id, expected)
			if e != nil {
				return nil, e
			}
			return json.Marshal(struct {
				Resource core.Resource `json:"resource"`
				Token    string        `json:"token"`
			}{r, t})
		case "revoke":
			r, e := s.Mutate(ctx, p, core.Mutation{Kind: kind, ID: id, ExpectedVersion: expected, Delete: true})
			if e != nil {
				return nil, e
			}
			return json.Marshal(r)
		}
	case "config":
		var in struct {
			ExpectedRevision int64           `json:"expected_revision"`
			Config           json.RawMessage `json:"config"`
			Prune            bool            `json:"prune"`
		}
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, err
		}
		switch action {
		case "apply":
			rev, e := s.ApplyConfig(ctx, p, in.ExpectedRevision, in.Config, in.Prune)
			if e != nil {
				return nil, e
			}
			return json.Marshal(map[string]any{"revision": rev})
		case "export":
			return s.ExportConfig(ctx, p)
		case "diff":
			current, e := s.ExportConfig(ctx, p)
			if e != nil {
				return nil, e
			}
			return json.Marshal(map[string]json.RawMessage{"current": current, "candidate": in.Config})
		}
	default:
		if action == "update" || action == "create" {
			r, e := s.Mutate(ctx, p, core.Mutation{Kind: kind, ID: id, ExpectedVersion: expected, Data: data})
			if e != nil {
				return nil, e
			}
			return json.Marshal(r)
		}
	}
	return nil, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 404, Message: fmt.Sprintf("unsupported action %s/%s", kind, action)}
}

// Reconcile records an immutable request and its result in the same transaction
// as the allowance correction, attempt version and audit event.
func (s *Store) Reconcile(ctx context.Context, p core.Principal, admission string, expected int64, r core.Reconciliation) (core.Resource, error) {
	if strings.TrimSpace(r.ReconciliationID) == "" || strings.TrimSpace(admission) == "" {
		return core.Resource{}, storeError("invalid_reconciliation", 400)
	}
	request, e := json.Marshal(struct {
		Admission      string              `json:"admission_id"`
		Expected       int64               `json:"expected_version"`
		Reconciliation core.Reconciliation `json:"reconciliation"`
	}{admission, expected, r})
	if e != nil {
		return core.Resource{}, e
	}
	var out core.Resource
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolvePrincipalTx(ctx, tx, p)
		if e != nil {
			return e
		}
		if e = authorize(current, "accounting", true); e != nil {
			return e
		}
		p = current
		var version int64
		var oldPayload, oldAdmission string
		e = tx.QueryRowContext(ctx, s.Query("SELECT version,admission_id,payload FROM reconciliations WHERE tenant_id=? AND reconciliation_id=?"), p.TenantID, r.ReconciliationID).Scan(&version, &oldAdmission, &oldPayload)
		if e == nil {
			var prior struct {
				Request json.RawMessage `json:"request"`
				Result  json.RawMessage `json:"result"`
			}
			if json.Unmarshal([]byte(oldPayload), &prior) != nil || oldAdmission != admission || !bytes.Equal(prior.Request, request) || len(prior.Result) == 0 {
				return storeError("conflict", 409)
			}
			out = core.Resource{TenantID: p.TenantID, Kind: "reconciliations", ID: r.ReconciliationID, Version: version, Data: prior.Result}
			return nil
		}
		if e != sql.ErrNoRows {
			return e
		}
		if expected <= 0 {
			return storeError("invalid_version", 400)
		}
		if strings.TrimSpace(r.Reason) == "" || (r.Mode != "provider_evidence" && r.Mode != "charge_reserved_maximum") {
			return storeError("invalid_reconciliation", 400)
		}
		if r.Cost != nil && *r.Cost < 0 {
			return storeError("invalid_reconciliation_cost", 400)
		}
		if r.Mode == "provider_evidence" && (strings.TrimSpace(r.SourceReference) == "" || (r.Usage == nil && r.Cost == nil)) {
			return storeError("reconciliation_evidence_required", 400)
		}
		if r.Usage != nil {
			for _, n := range []*int64{r.Usage.Input, r.Usage.Output, r.Usage.Total} {
				if n != nil && *n < 0 {
					return storeError("invalid_reconciliation_usage", 400)
				}
			}
			if r.Usage.Input != nil && r.Usage.Output != nil {
				if *r.Usage.Input > int64(^uint64(0)>>1)-*r.Usage.Output {
					return storeError("invalid_reconciliation_usage", 400)
				}
				if r.Usage.Total != nil && *r.Usage.Total != *r.Usage.Input+*r.Usage.Output {
					return storeError("invalid_reconciliation_usage", 400)
				}
			}
		}
		env, state, attemptVersion, e := s.loadAttempt(ctx, tx, p.TenantID, admission)
		if e == sql.ErrNoRows {
			return storeError("not_found", 404)
		}
		if e != nil {
			return e
		}
		if attemptVersion != expected {
			return storeError("version_conflict", 412)
		}
		if attemptVersion == int64(^uint64(0)>>1) {
			return storeError("version_exhausted", 409)
		}
		if state != "outcome_unknown" {
			return storeError("attempt_not_reconcilable", 409)
		}
		charge := env.ChargedCost
		if r.Mode == "provider_evidence" && r.Cost != nil {
			charge = *r.Cost
		} else if r.Mode == "charge_reserved_maximum" {
			if env.Plan.MaximumCost != nil {
				charge = *env.Plan.MaximumCost
			} else {
				found := false
				for _, a := range env.Plan.Allowances {
					if a.Kind == "cost" {
						amount := a.Maximum
						if !found || amount > charge {
							charge = amount
						}
						found = true
					}
				}
				if !found {
					return storeError("reconciliation_maximum_unavailable", 409)
				}
			}
		}
		if charge < 0 {
			return storeError("reconciliation_maximum_unavailable", 409)
		}
		var tokens *int64
		if r.Mode == "provider_evidence" && r.Usage != nil {
			tokens = r.Usage.Total
			if tokens == nil && r.Usage.Input != nil && r.Usage.Output != nil {
				total := *r.Usage.Input + *r.Usage.Output
				tokens = &total
			}
		}
		for i, a := range env.Plan.Allowances {
			switch a.Kind {
			case "cost":
				if e = s.adjustHold(ctx, tx, &env, i, charge); e != nil {
					return e
				}
			case "tokens":
				if tokens != nil {
					if e = s.adjustHold(ctx, tx, &env, i, *tokens); e != nil {
						return e
					}
				}
			case "concurrency":
				if e = s.adjustHold(ctx, tx, &env, i, 0); e != nil {
					return e
				}
			}
		}
		if env.ChargedCost < 0 {
			return storeError("invalid_accounting_state", 409)
		}
		delta := charge - env.ChargedCost
		result, e := json.Marshal(struct {
			core.Reconciliation
			Admission        string `json:"admission_id"`
			AdmissionVersion int64  `json:"admission_version"`
			State            string `json:"state"`
			Reconciled       bool   `json:"reconciled"`
			ChargedCost      int64  `json:"charged_cost"`
			Delta            int64  `json:"delta"`
		}{r, admission, attemptVersion + 1, state, true, charge, delta})
		if e != nil {
			return e
		}
		effectKind := "reconciliation:" + r.ReconciliationID
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO usage_ledger(tenant_id,effect_id,attempt_id,effect_kind,amount,data,created_at) VALUES (?,?,?,?,?,?,?)"), p.TenantID, effectKind, admission, effectKind, delta, string(result), time.Now().Unix()); e != nil {
			return e
		}
		env.Reconciled = true
		env.ChargedCost = charge
		env.UpdatedAt = time.Now().Unix()
		if r.Mode == "provider_evidence" {
			env.AcceptedEvidence = r.SourceReference
		}
		if env.Outcome == nil {
			env.Outcome = &core.AttemptOutcome{TenantID: p.TenantID, RequestID: env.Plan.RequestID, AttemptID: admission, State: state}
		}
		env.Outcome.ActualCost = &charge
		if r.Mode == "provider_evidence" && r.Usage != nil {
			env.Outcome.Usage = r.Usage
		}
		if e = s.saveAttempt(ctx, tx, env, state, attemptVersion); e != nil {
			return e
		}
		if e = s.auditTx(ctx, tx, p, "admissions", admission, "reconcile", attemptVersion+1); e != nil {
			return e
		}
		payload, e := json.Marshal(struct {
			Request json.RawMessage `json:"request"`
			Result  json.RawMessage `json:"result"`
		}{request, result})
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO reconciliations(tenant_id,reconciliation_id,admission_id,payload,version) VALUES (?,?,?,?,1)"), p.TenantID, r.ReconciliationID, admission, string(payload)); e != nil {
			return e
		}
		out = core.Resource{TenantID: p.TenantID, Kind: "reconciliations", ID: r.ReconciliationID, Version: 1, Data: result}
		return nil
	})
	if err != nil {
		return core.Resource{}, err
	}
	return out, nil
}

var _ admin.ActionService = (*Store)(nil)
var _ admin.Reconciler = (*Store)(nil)
