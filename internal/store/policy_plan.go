package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hoorific/internal/core"
	"hoorific/internal/policy"
	"strings"
	"time"
)

func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == "*" || v == want {
			return true
		}
	}
	return false
}
func hasOperation(values []core.Operation, want core.Operation) bool {
	for _, v := range values {
		if v == "*" || v == want {
			return true
		}
	}
	return false
}
func permitted(p core.Principal, alias, connection string, op core.Operation) bool {
	if p.Role == "admin" {
		return true
	}
	if alias != "" && !hasString(p.Aliases, alias) && !hasString(p.Permissions, "alias:"+alias) && !hasString(p.Permissions, "alias/"+alias) {
		return false
	}
	if connection != "" && !hasString(p.Connections, connection) && !hasString(p.Permissions, "connection:"+connection) && !hasString(p.Permissions, "connection/"+connection) && !hasString(p.Permissions, "connections:*") {
		return false
	}
	return hasOperation(p.Operations, op) || hasString(p.Permissions, "operation:"+string(op)) || hasString(p.Permissions, "operation/"+string(op))
}
func hasOperationStrings(values []string, want core.Operation) bool {
	for _, v := range values {
		if v == "*" || v == string(want) {
			return true
		}
	}
	return false
}
func planIdentity(p core.Principal, in core.AttemptPlan) error {
	if p.TenantID == "" || (p.KeyID == "" && p.SessionID == "") {
		return problem("authentication_required", 401, "authenticated key or admin session is required")
	}
	if p.KeyID != "" && p.KeyRevision <= 0 {
		return problem("authentication_required", 401, "authenticated key is required")
	}
	if !validRef(in.RequestID) || !validRef(in.AttemptID) || in.Deadline.IsZero() {
		return problem("invalid_attempt", 400, "attempt identity and deadline are required")
	}
	if in.TenantID != "" && in.TenantID != p.TenantID || in.KeyID != "" && in.KeyID != p.KeyID {
		return problem("forbidden", 403, "attempt binding is immutable")
	}
	return nil
}
func (s *Store) PlanAttempt(ctx context.Context, p core.Principal, snap core.RuntimeSnapshot, target core.Target, op core.Operation, body []byte, in core.AttemptPlan) (out core.AttemptPlan, err error) {
	if !validRef(string(op)) {
		return out, problem("invalid_target", 400, "operation is required")
	}
	if err = planIdentity(p, in); err != nil {
		return out, err
	}
	if snap.Revision < 0 {
		return out, problem("invalid_configuration", 500, "negative configuration revision")
	}
	if pm, ok := target.(*core.ModelCall); ok {
		if pm == nil {
			return out, problem("invalid_target", 400, "nil model target")
		}
		target = *pm
	}
	if pc, ok := target.(*core.ConnectionResourceCall); ok {
		if pc == nil {
			return out, problem("invalid_target", 400, "nil connection target")
		}
		target = *pc
	}
	out = in
	out.TenantID = p.TenantID
	out.KeyID = p.KeyID
	out.KeyRevision = p.KeyRevision
	out.ConfigRevision = snap.Revision
	out.Allowances = nil
	out.MaximumCost = nil
	out.PriceVersion = ""
	out.Operation = op
	var alias, connection, model, account string
	switch t := target.(type) {
	case core.ModelCall:
		connection = t.Connection.ID
		model = t.Model.CatalogID
		alias = t.Alias
		if !validRef(connection) || !validRef(model) || !validRef(t.Model.ID) || t.Connection.TenantID != p.TenantID || t.Model.ConnectionID != connection || t.Model.CatalogID != model {
			return out, problem("invalid_target", 400, "model target references are invalid")
		}
		snapConn, ok := snap.Connections[connection]
		if !ok || snapConn.TenantID != p.TenantID || t.Connection.Version != 0 && t.Connection.Version != snapConn.Version || t.Connection.AccountID != snapConn.AccountID {
			return out, problem("invalid_target", 409, "connection snapshot is stale")
		}
		snapModel, ok := snap.Models[model]
		if !ok || snapModel.ConnectionID != connection {
			return out, problem("invalid_target", 404, "model is not configured")
		}
		if alias != "" {
			targets, ok := snap.Aliases[alias]
			if !ok {
				return out, problem("invalid_target", 404, "alias is not configured")
			}
			matched := false
			for _, rt := range targets {
				if rt.ConnectionID == connection && rt.ModelID == model {
					matched = true
					break
				}
			}
			if !matched {
				return out, problem("invalid_target", 400, "alias target is not configured")
			}
		}
		if !hasOperation(snapModel.Operations, op) {
			return out, problem("operation_not_supported", 400, "model does not support operation")
		}
		out.Alias = alias
		account = snapConn.AccountID
		if err = boundModel(&out, snapModel, body); err != nil {
			return out, err
		}
	case core.ConnectionResourceCall:
		connection = t.Connection.ID
		if !validRef(connection) || t.Connection.TenantID != p.TenantID {
			return out, problem("invalid_target", 400, "connection target is invalid")
		}
		snapConn, ok := snap.Connections[connection]
		if !ok || snapConn.TenantID != p.TenantID || t.Connection.Version != 0 && t.Connection.Version != snapConn.Version || t.Connection.AccountID != snapConn.AccountID {
			return out, problem("invalid_target", 409, "connection snapshot is stale")
		}
		account = snapConn.AccountID
	default:
		return out, problem("invalid_target", 400, "unsupported target")
	}
	if in.ConnectionID != "" && in.ConnectionID != connection || in.ModelID != "" && in.ModelID != model || in.AccountID != "" && in.AccountID != account {
		return out, problem("forbidden", 403, "attempt target binding is immutable")
	}
	nativeRealtime := alias == "" && string(op) == "realtime" && p.NativeAccount && p.Realtime
	if nativeRealtime {
		if !hasString(p.Connections, connection) && !hasString(p.Permissions, "connection:"+connection) && !hasString(p.Permissions, "connection/"+connection) && !hasString(p.Permissions, "connections:*") {
			return out, problem("forbidden", 403, "native realtime connection grant is required")
		}
	} else if !permitted(p, alias, connection, op) {
		return out, problem("forbidden", 403, "key is not permitted for target operation")
	}
	out.ConnectionID = connection
	out.ModelID = model
	out.AccountID = account
	return out, nil
}
func (s *Store) derivePlan(ctx context.Context, p core.AttemptPlan) (core.AttemptPlan, error) {
	return p, nil
}
func boundModel(out *core.AttemptPlan, m core.Model, body []byte) error {
	var meta struct {
		// Caller token counts are observations, not trustworthy admission bounds.
		MaxTokens       *int64 `json:"max_tokens"`
		MaxOutputTokens *int64 `json:"max_output_tokens"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &meta); err != nil {
			return problem("invalid_request", 400, "request metadata is invalid")
		}
	}
	input := m.ContextLimit
	output := meta.MaxOutputTokens
	if output == nil {
		output = meta.MaxTokens
	}
	if m.OutputLimit == nil {
		output = nil
	} else if output == nil || *output > *m.OutputLimit {
		output = m.OutputLimit
	}
	if input != nil && *input < 0 || output != nil && *output < 0 {
		return problem("unsupported_policy", 400, "negative token bound")
	}
	if input != nil {
		v := *input
		out.InputBound = &v
	}
	if output != nil {
		v := *output
		out.OutputBound = &v
	}
	// These are independent conservative maxima, not an exact token count.
	// Reserve the full approved input context plus bounded output; never
	// subtract output from context without an explicit combined-window contract.
	if m.Price != nil {
		out.PriceVersion = m.Price.Version
		if input != nil && output != nil && m.Price.InputPerMillion != nil && m.Price.OutputPerMillion != nil {
			cost, err := policy.EstimateCost(*input, *output, *m.Price.InputPerMillion, *m.Price.OutputPerMillion, 1000000)
			if err != nil {
				return problem("unsupported_policy", 400, "cost bound is not representable")
			}
			out.MaximumCost = &cost
		} else if m.Price.MaximumUnitCost != nil && m.Price.UnitOperation == out.Operation {
			v := *m.Price.MaximumUnitCost
			out.MaximumCost = &v
		}
	}
	return nil
}

func (s *Store) derivePlanTx(ctx context.Context, tx *sql.Tx, p core.AttemptPlan) (core.AttemptPlan, error) {
	if tx == nil {
		return core.AttemptPlan{}, errors.New("nil transaction")
	}
	now, err := sqlNow(ctx, tx, s.Dialect)
	if err != nil {
		return core.AttemptPlan{}, err
	}
	if err = validatePlan(p); err != nil {
		return core.AttemptPlan{}, err
	}
	if p.TenantID == "" {
		return core.AttemptPlan{}, problem("authentication_required", 401, "tenant is required")
	}
	var e error
	if p.KeyID == "" {
		principal, ok := core.PrincipalFromContext(ctx)
		if !ok || principal.TenantID != p.TenantID || principal.SessionID == "" || principal.SubjectID == "" {
			return core.AttemptPlan{}, problem("authentication_required", 401, "admin session is required")
		}
		var sess storedSession
		_, _, e = s.durableGetAny(ctx, tx, "admin_session", principal.SessionID, &sess)
		if e != nil || sess.ExpiresAt.Unix() <= now || sess.Principal.TenantID != p.TenantID || sess.Principal.SubjectID != principal.SubjectID {
			return core.AttemptPlan{}, problem("authentication_required", 401, "admin session is invalid")
		}
		var current core.Principal
		current, e = s.resolveMembershipTx(ctx, tx, p.TenantID, principal.SubjectID)
		if e != nil {
			return core.AttemptPlan{}, problem("authentication_required", 401, "admin session is invalid")
		}
		switch current.Role {
		case "owner", "admin", "operator":
		default:
			return core.AttemptPlan{}, problem("forbidden", 403, "admin role cannot execute playground")
		}
	} else {
		activeKey, e := s.resolveActiveKeyTx(ctx, tx, p.TenantID, p.KeyID, p.KeyRevision)
		if e != nil {
			return core.AttemptPlan{}, problem("authentication_required", 401, "key is not active")
		}
		if activeKey.Role != "admin" && !hasString(activeKey.Connections, p.ConnectionID) && !hasString(activeKey.Permissions, "connection:"+p.ConnectionID) && !hasString(activeKey.Permissions, "connection/"+p.ConnectionID) && !hasString(activeKey.Permissions, "connections:*") && !hasString(activeKey.Permissions, "*") {
			return core.AttemptPlan{}, problem("forbidden", 403, "key is not permitted for connection")
		}
	}
	var cfg int64
	if e = tx.QueryRowContext(ctx, s.Query("SELECT revision FROM config_state WHERE id=1")).Scan(&cfg); e != nil {
		return core.AttemptPlan{}, e
	}
	if cfg < 0 || p.ConfigRevision < 0 || cfg != p.ConfigRevision {
		return core.AttemptPlan{}, problem("configuration_stale", 409, "configuration changed; retry planning")
	}
	if !validRef(p.ConnectionID) {
		return core.AttemptPlan{}, problem("invalid_target", 400, "connection reference is required")
	}
	var connData string
	var connVersion int64
	e = tx.QueryRowContext(ctx, s.Query("SELECT version,data FROM resources WHERE tenant_id=? AND kind='connections' AND id=?"), p.TenantID, p.ConnectionID).Scan(&connVersion, &connData)
	if e != nil {
		return core.AttemptPlan{}, problem("invalid_target", 404, "connection is not configured")
	}
	var cx ConnectionData
	if json.Unmarshal([]byte(connData), &cx) != nil || !cx.Enabled || cx.Connector == "" || p.AccountID != cx.AccountID {
		return core.AttemptPlan{}, problem("configuration_stale", 409, "connection binding invalid")
	}
	if p.ModelID != "" {
		var modelData string
		if e = tx.QueryRowContext(ctx, s.Query("SELECT data FROM resources WHERE tenant_id=? AND kind='models' AND id=?"), p.TenantID, p.ModelID).Scan(&modelData); e != nil {
			return core.AttemptPlan{}, problem("invalid_target", 404, "model is not configured")
		}
		var mx ModelData
		var caps struct {
			ContextLimit *int64              `json:"context_limit"`
			OutputLimit  *int64              `json:"output_limit"`
			Price        *core.PriceSchedule `json:"price"`
		}
		if json.Unmarshal([]byte(modelData), &mx) != nil || json.Unmarshal([]byte(modelData), &caps) != nil || !mx.Enabled || mx.UpstreamID == "" || mx.ConnectionID != p.ConnectionID {
			return core.AttemptPlan{}, problem("invalid_configuration", 500, "stored model reference is invalid")
		}
		if !hasOperationStrings(mx.Operations, p.Operation) {
			return core.AttemptPlan{}, problem("unsupported_operation", 400, "model operation is disabled")
		}
		if p.InputBound != nil && (caps.ContextLimit == nil || *caps.ContextLimit < 0 || *p.InputBound != *caps.ContextLimit) {
			return core.AttemptPlan{}, problem("configuration_stale", 409, "approved input context bound changed")
		}
		if p.OutputBound != nil && (caps.OutputLimit == nil || *caps.OutputLimit < 0 || *p.OutputBound < 0 || *p.OutputBound > *caps.OutputLimit) {
			return core.AttemptPlan{}, problem("configuration_stale", 409, "approved output bound changed")
		}
		if caps.Price != nil && caps.Price.InputPerMillion != nil && caps.Price.OutputPerMillion != nil && p.InputBound != nil && p.OutputBound != nil {
			want, e := policy.EstimateCost(*p.InputBound, *p.OutputBound, *caps.Price.InputPerMillion, *caps.Price.OutputPerMillion, 1000000)
			if e != nil || p.MaximumCost == nil || *p.MaximumCost != want || caps.Price.Version != p.PriceVersion {
				return core.AttemptPlan{}, problem("configuration_stale", 409, "cost bound changed")
			}
		}
		if (p.InputBound == nil || p.OutputBound == nil) && (caps.Price == nil || caps.Price.MaximumUnitCost == nil || caps.Price.UnitOperation != p.Operation) {
			p.MaximumCost = nil
		}
	}
	return s.resolveAllowances(ctx, tx, p, now, true)
}

func (s *Store) resolveAllowances(ctx context.Context, tx *sql.Tx, p core.AttemptPlan, now int64, create bool) (core.AttemptPlan, error) {
	rows, err := tx.QueryContext(ctx, s.Query(`SELECT scope_kind,scope_id,window_id,kind,maximum,reserve FROM policy_limits WHERE tenant_id=? ORDER BY scope_kind,scope_id,window_id,kind`), p.TenantID)
	if err != nil {
		return core.AttemptPlan{}, err
	}
	defer rows.Close()
	out := p
	out.Allowances = nil
	for rows.Next() {
		var scope, id, wid, kind string
		var maximum, reserve int64
		if err = rows.Scan(&scope, &id, &wid, &kind, &maximum, &reserve); err != nil {
			return core.AttemptPlan{}, err
		}
		if !policyScopeMatches(scope, id, p) || maximum <= 0 {
			continue
		}
		active := wid == "total"
		if !active {
			var starts, ends int64
			if e := tx.QueryRowContext(ctx, s.Query("SELECT starts,ends FROM quota_windows WHERE tenant_id=? AND id=?"), p.TenantID, wid).Scan(&starts, &ends); e == nil {
				active = starts <= now && now < ends
			} else if strings.HasPrefix(wid, "60:") {
				var start int64
				if _, e = fmt.Sscan(strings.TrimPrefix(wid, "60:"), &start); e == nil {
					active = start <= now && now < start+60
				}
			}
		}
		if !active && create {
			prefix := wid
			if i := strings.IndexByte(prefix, ':'); i >= 0 {
				prefix = prefix[:i]
			}
			nw := currentWindowID(now, prefix)
			if nw != "" {
				starts, ends := windowBounds(now, prefix)
				if _, err = tx.ExecContext(ctx, s.Query("INSERT INTO quota_windows(tenant_id,id,starts,ends,version) VALUES(?,?,?,?,1) ON CONFLICT(tenant_id,id) DO UPDATE SET starts=excluded.starts,ends=excluded.ends"), p.TenantID, nw, starts, ends); err != nil {
					return core.AttemptPlan{}, err
				}
				if _, err = tx.ExecContext(ctx, s.Query("INSERT INTO policy_limits(tenant_id,scope_kind,scope_id,window_id,kind,maximum,reserve,version) VALUES(?,?,?,?,?,?,?,1) ON CONFLICT(tenant_id,scope_kind,scope_id,window_id,kind) DO NOTHING"), p.TenantID, scope, id, nw, kind, maximum, reserve); err != nil {
					return core.AttemptPlan{}, err
				}
				if _, err = tx.ExecContext(ctx, s.Query("INSERT INTO allowances(tenant_id,scope_kind,scope_id,window_id,kind,reserved,version) VALUES(?,?,?,?,?,?,1) ON CONFLICT(tenant_id,scope_kind,scope_id,window_id,kind) DO NOTHING"), p.TenantID, scope, id, nw, kind, 0); err != nil {
					return core.AttemptPlan{}, err
				}
				wid = nw
				active = true
			}
		}
		if active {
			required := int64(1)
			switch kind {
			case "tokens":
				if p.InputBound == nil || p.OutputBound == nil {
					return core.AttemptPlan{}, problem("unsupported_policy", 400, "token policy requires bounded input and output")
				}
				required = *p.InputBound + *p.OutputBound
			case "cost":
				if p.MaximumCost == nil {
					return core.AttemptPlan{}, problem("unsupported_policy", 400, "cost policy requires bounded price")
				}
				required = *p.MaximumCost
			}
			out.Allowances = append(out.Allowances, core.Allowance{ScopeKind: scope, ScopeID: id, WindowID: wid, Kind: kind, Maximum: maximum, Reserve: required})
		}
	}
	if err = rows.Err(); err != nil {
		return core.AttemptPlan{}, err
	}
	return out, nil
}
func currentWindowID(now int64, prefix string) string {
	switch prefix {
	case "60":
		return fmt.Sprintf("60:%d", now/60*60)
	case "daily":
		t := time.Unix(now, 0).UTC()
		return fmt.Sprintf("daily:%04d-%03d", t.Year(), t.YearDay())
	case "monthly":
		t := time.Unix(now, 0).UTC()
		return fmt.Sprintf("monthly:%04d-%02d", t.Year(), t.Month())
	}
	return ""
}
func policyScopeMatches(scope, id string, p core.AttemptPlan) bool {
	switch scope {
	case "tenant":
		return id == p.TenantID
	case "key":
		return id == p.KeyID
	case "connection":
		return id == p.ConnectionID
	case "account":
		return id == p.AccountID
	case "model":
		return id == p.ModelID
	}
	return false
}
