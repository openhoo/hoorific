package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

// readOperationalResources projects committed operational facts, never the
// generic resources or admissions shadow tables. Immutable events use resource
// version 1; attempts and reconciliations retain their stored row version.
func (s *Store) readOperationalResources(ctx context.Context, p core.Principal, kind, id, cursor string, limit int, single bool) (core.ResourcePage, error) {
	if durableOperationalKind(kind) {
		return s.readDurableOperationalResources(ctx, p, kind, id, cursor, limit, single)
	}
	out := core.ResourcePage{Items: []core.Resource{}}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return out, storeError("invalid_limit", 400)
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, err := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
		if err != nil {
			return err
		}
		p = current
		if err = authorize(p, kind, false); err != nil {
			return err
		}
		after, err := s.tenancyCursor(p, kind, cursor, limit)
		if err != nil {
			return err
		}
		var query, column string
		switch kind {
		case "usage_ledger":
			query = "SELECT effect_id,attempt_id,effect_kind,amount,data,created_at FROM usage_ledger WHERE tenant_id=?"
			column = "effect_id"
		case "audit_events":
			query = "SELECT id,subject_id,kind,resource_id,action,version,created_at FROM audit_events WHERE tenant_id=?"
			column = "id"
		case "admissions":
			query = "SELECT attempt_id,request_id,state,version,deadline,updated_at,data FROM attempts WHERE tenant_id=?"
			column = "attempt_id"
		case "reconciliations":
			query = "SELECT reconciliation_id,admission_id,version,payload FROM reconciliations WHERE tenant_id=?"
			column = "reconciliation_id"
		default:
			return storeError("invalid_kind", 400)
		}
		args := []any{p.TenantID}
		if single {
			query += " AND " + column + "=? LIMIT 1"
			args = append(args, id)
		} else {
			query += " AND " + column + ">? ORDER BY " + column + " LIMIT ?"
			args = append(args, after, limit+1)
		}
		rows, err := tx.QueryContext(ctx, s.Query(query), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanOperationalResource(rows, p.TenantID, kind)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, r)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if single && len(out.Items) == 0 {
			return storeError("not_found", 404)
		}
		if len(out.Items) > limit {
			out.Items = out.Items[:limit]
			out.NextCursor, err = s.encodeCursor(resourceCursor{p.TenantID, p.SubjectID, p.Role, kind, out.Items[limit-1].ID, limit, time.Now().Add(15 * time.Minute).Unix()})
		}
		return err
	})
	if err != nil {
		// Never return a partial page if a later record is malformed.
		return core.ResourcePage{Items: []core.Resource{}}, err
	}
	return out, nil
}

func invalidOperationalRecord() error {
	return storeError("invalid_operational_record", 500)
}

// Select exact, case-sensitive keys before typed decoding. Unknown properties
// (including provider evidence, cancellation text and credentials) never enter
// the DTO. Decoder errors are deliberately not returned to callers.
type operationalObject map[string]json.RawMessage

func operationalDecode(raw []byte) (operationalObject, error) {
	var object operationalObject
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, invalidOperationalRecord()
	}
	return object, nil
}

func (o operationalObject) field(name string, dst any, required bool) error {
	raw, ok := o[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if required {
			return invalidOperationalRecord()
		}
		return nil
	}
	if json.Unmarshal(raw, dst) != nil {
		return invalidOperationalRecord()
	}
	return nil
}

func operationalUsage(raw json.RawMessage) (*core.Usage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	o, err := operationalDecode(raw)
	if err != nil {
		return nil, err
	}
	u := &core.Usage{}
	for _, field := range []struct {
		name string
		dst  **int64
	}{{"Input", &u.Input}, {"Output", &u.Output}, {"Total", &u.Total}} {
		if err := o.field(field.name, field.dst, false); err != nil {
			return nil, err
		}
		if *field.dst != nil && **field.dst < 0 {
			return nil, invalidOperationalRecord()
		}
	}
	// Source is an enumerated provenance label, not provider evidence.
	if err := o.field("Source", &u.Source, false); err != nil {
		return nil, err
	}
	return u, nil
}

func operationalTime(seconds int64) (string, error) {
	t := time.Unix(seconds, 0).UTC()
	if seconds <= 0 || t.Year() < 1 || t.Year() > 9999 {
		return "", invalidOperationalRecord()
	}
	return t.Format(time.RFC3339), nil
}

func scanOperationalResource(rows *sql.Rows, tenant, kind string) (core.Resource, error) {
	r := core.Resource{TenantID: tenant, Kind: kind, Version: 1}
	var projection any
	switch kind {
	case "usage_ledger":
		var d admin.UsageLedgerData
		var amount sql.NullInt64
		var raw string
		var created int64
		if err := rows.Scan(&r.ID, &d.AttemptID, &d.Kind, &amount, &raw, &created); err != nil {
			return r, invalidOperationalRecord()
		}
		o, err := operationalDecode([]byte(raw))
		if err != nil || d.AttemptID == "" {
			return r, invalidOperationalRecord()
		}
		if amount.Valid {
			d.Cost = &amount.Int64
		}
		switch {
		case d.Kind == "charge":
			var attemptID, payloadTenant string
			if o.field("AttemptID", &attemptID, true) != nil || o.field("TenantID", &payloadTenant, true) != nil || attemptID != d.AttemptID || payloadTenant != tenant || (d.Cost != nil && *d.Cost < 0) {
				return r, invalidOperationalRecord()
			}
			d.Usage, err = operationalUsage(o["Usage"])
		case strings.HasPrefix(d.Kind, "reconciliation:"):
			result, e := projectOperationalReconciliation(o, strings.TrimPrefix(d.Kind, "reconciliation:"), d.AttemptID)
			if e != nil || (amount.Valid && (result.Delta == nil || *result.Delta != amount.Int64)) {
				return r, invalidOperationalRecord()
			}
			d.Usage = result.Usage
			err = nil
		default:
			return r, invalidOperationalRecord()
		}
		if err != nil {
			return r, err
		}
		d.CreatedAt, err = operationalTime(created)
		if err != nil {
			return r, err
		}
		projection = d
	case "audit_events":
		var d admin.AuditEventData
		var created int64
		if err := rows.Scan(&r.ID, &d.Actor, &d.Kind, &d.ResourceID, &d.Action, &d.ResourceVersion, &created); err != nil {
			return r, invalidOperationalRecord()
		}
		if d.Actor == "" || d.Kind == "" || d.ResourceID == "" || d.Action == "" || d.ResourceVersion < 0 {
			return r, invalidOperationalRecord()
		}
		var err error
		d.CreatedAt, err = operationalTime(created)
		if err != nil {
			return r, err
		}
		d.Summary = d.Kind + "/" + d.ResourceID
		projection = d
	case "admissions":
		var d admin.AdmissionData
		var raw string
		var deadline, updated int64
		if err := rows.Scan(&r.ID, &d.RequestID, &d.State, &r.Version, &deadline, &updated, &raw); err != nil {
			return r, invalidOperationalRecord()
		}
		envelope, err := operationalDecode([]byte(raw))
		if err != nil {
			return r, err
		}
		plan, err := operationalDecode(envelope["plan"])
		if err != nil {
			return r, err
		}
		var planTenant, requestID string
		if plan.field("AttemptID", &d.AttemptID, true) != nil || plan.field("TenantID", &planTenant, true) != nil || plan.field("RequestID", &requestID, true) != nil || plan.field("MaximumCost", &d.MaximumCost, false) != nil || envelope.field("reconciled", &d.Reconciled, false) != nil || d.AttemptID != r.ID || planTenant != tenant || requestID != d.RequestID || requestID == "" || (d.MaximumCost != nil && *d.MaximumCost < 0) {
			return r, invalidOperationalRecord()
		}
		switch d.State {
		case "dispatch_intent", "accepted", "settled", "not_executed", "outcome_unknown", "job_pending":
		default:
			return r, invalidOperationalRecord()
		}
		if rawOutcome := envelope["outcome"]; len(rawOutcome) > 0 && !bytes.Equal(bytes.TrimSpace(rawOutcome), []byte("null")) {
			outcome, err := operationalDecode(rawOutcome)
			if err != nil {
				return r, err
			}
			var attemptID, outcomeTenant string
			if outcome.field("AttemptID", &attemptID, true) != nil || outcome.field("TenantID", &outcomeTenant, true) != nil || outcome.field("ActualCost", &d.ActualCost, false) != nil || attemptID != r.ID || outcomeTenant != tenant || (d.ActualCost != nil && *d.ActualCost < 0) {
				return r, invalidOperationalRecord()
			}
			d.Usage, err = operationalUsage(outcome["Usage"])
			if err != nil {
				return r, err
			}
		}
		d.Deadline, err = operationalTime(deadline)
		if err != nil {
			return r, err
		}
		d.UpdatedAt, err = operationalTime(updated)
		if err != nil {
			return r, err
		}
		projection = d
	case "reconciliations":
		var admission, raw string
		if err := rows.Scan(&r.ID, &admission, &r.Version, &raw); err != nil {
			return r, invalidOperationalRecord()
		}
		envelope, err := operationalDecode([]byte(raw))
		if err != nil {
			return r, err
		}
		result, err := operationalDecode(envelope["result"])
		if err != nil {
			return r, err
		}
		projection, err = projectOperationalReconciliation(result, r.ID, admission)
		if err != nil {
			return r, err
		}
	default:
		return r, storeError("invalid_kind", 400)
	}
	if r.ID == "" || r.Version < 0 {
		return r, invalidOperationalRecord()
	}
	var err error
	r.Data, err = json.Marshal(projection)
	return r, err
}
func projectOperationalReconciliation(o operationalObject, id, admission string) (admin.ReconciliationData, error) {
	var d admin.ReconciliationData
	for _, field := range []struct {
		name string
		dst  any
	}{{"reconciliation_id", &d.ReconciliationID}, {"admission_id", &d.AdmissionID}, {"mode", &d.Mode}, {"reason", &d.Reason}, {"source_reference", &d.SourceReference}, {"admission_version", &d.AdmissionVersion}, {"state", &d.State}, {"reconciled", &d.Reconciled}, {"charged_cost", &d.ChargedCost}, {"delta", &d.Delta}} {
		required := field.name != "source_reference"
		if err := o.field(field.name, field.dst, required); err != nil {
			return d, err
		}
	}
	if id == "" || admission == "" || d.ReconciliationID != id || d.AdmissionID != admission || d.Reason == "" || d.AdmissionVersion <= 0 || d.State != "outcome_unknown" || !d.Reconciled || d.ChargedCost == nil || *d.ChargedCost < 0 || d.Delta == nil || (d.Mode != "provider_evidence" && d.Mode != "charge_reserved_maximum") || o.field("cost", &d.Cost, false) != nil || (d.Cost != nil && *d.Cost < 0) {
		return d, invalidOperationalRecord()
	}
	var err error
	d.Usage, err = operationalUsage(o["usage"])
	return d, err
}
