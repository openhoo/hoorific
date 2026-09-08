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

type cachePricingRequirements struct {
	read        bool
	write       bool
	breakpoints int
	ttl         map[string]bool
	otherTTL    bool
	openAITTL   string
}

func (r *cachePricingRequirements) addWriteTTL(ttl string) {
	r.write = true
	if ttl == "" {
		return
	}
	if r.ttl == nil {
		r.ttl = map[string]bool{}
	}
	switch ttl {
	case "5m", "1h":
		r.ttl[ttl] = true
	default:
		r.otherTTL = true
	}
}

func cacheStringField(m map[string]any, key string) string {
	value, ok := m[key]
	if !ok {
		return ""
	}
	var out string
	if json.Unmarshal(mustJSON(value), &out) != nil {
		return ""
	}
	return out
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// inspectCacheBlocks follows only content-block containers. Cache-looking
// fields in text, tool arguments, schemas, or other opaque properties are not
// request directives and must not change billing admission.
func inspectCacheBlocks(value any, req *cachePricingRequirements, breakpoints bool) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			inspectCacheBlocks(item, req, breakpoints)
		}
	case map[string]any:
		kind, hasKind := value["type"].(string)
		_, hasPoint := value["cachePoint"]
		semantic := hasPoint
		if hasKind {
			switch kind {
			case "text", "thinking", "image", "document", "tool_use", "tool_result",
				"input_text", "input_image", "input_file":
				semantic = true
			}
		}
		if !semantic {
			return
		}
		if control, ok := value["cache_control"]; ok {
			if object, ok := control.(map[string]any); ok {
				req.addWriteTTL(cacheStringField(object, "ttl"))
			} else {
				req.addWriteTTL("")
			}
		}
		if point, ok := value["cachePoint"]; ok {
			if object, ok := point.(map[string]any); ok {
				req.addWriteTTL(cacheStringField(object, "ttl"))
			} else {
				req.addWriteTTL("")
			}
		}
		if breakpoints && (kind == "text" || kind == "input_text") {
			if _, ok := value["prompt_cache_breakpoint"]; ok {
				req.breakpoints++
			}
		}
		// Only the validated Anthropic tool-result block has a nested
		// semantic content container. Do not recurse arbitrary content maps.
		if kind == "tool_result" {
			if child, ok := value["content"]; ok {
				inspectCacheBlocks(child, req, breakpoints)
			}
		}
	}
}

// inspectCacheContainers follows message/content containers but not their
// arbitrary properties. This keeps detection semantic-path based while
// supporting OpenAI input/messages and Gemini contents/systemInstruction.
func inspectCacheContainers(value any, req *cachePricingRequirements) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			inspectCacheContainers(item, req)
		}
	case map[string]any:
		if child, ok := value["content"]; ok {
			inspectCacheBlocks(child, req, true)
		}
		if child, ok := value["parts"]; ok {
			inspectCacheBlocks(child, req, false)
		}
	}
}

func inspectCacheSystem(value any, req *cachePricingRequirements) {
	if object, ok := value.(map[string]any); ok {
		if child, ok := object["parts"]; ok {
			inspectCacheBlocks(child, req, false)
			return
		}
	}
	// Anthropic's top-level system is itself a block list.
	inspectCacheBlocks(value, req, true)
}

func inspectCacheTools(value any, req *cachePricingRequirements) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			inspectCacheTools(item, req)
		}
	case map[string]any:
		if control, ok := value["cache_control"]; ok {
			if object, ok := control.(map[string]any); ok {
				req.addWriteTTL(cacheStringField(object, "ttl"))
			} else {
				req.addWriteTTL("")
			}
		}
		if point, ok := value["cachePoint"]; ok {
			if object, ok := point.(map[string]any); ok {
				req.addWriteTTL(cacheStringField(object, "ttl"))
			} else {
				req.addWriteTTL("")
			}
		}
	}
}

func inspectCacheJSON(value any, req *cachePricingRequirements, root bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	if !root {
		inspectCacheContainers(object, req)
		return
	}
	// These are root-owned cache identities/options. Do not search for them
	// recursively: an arbitrary property named cachedContent is just data.
	if name, ok := object["cachedContent"].(string); ok && name != "" {
		req.read = true
	}
	if control, ok := object["cache_control"]; ok {
		if object, ok := control.(map[string]any); ok {
			req.addWriteTTL(cacheStringField(object, "ttl"))
		} else {
			req.addWriteTTL("")
		}
	}
	if key, ok := object["prompt_cache_key"].(string); ok && key != "" {
		req.read = true
	}
	if retention, ok := object["prompt_cache_retention"].(string); ok && retention != "" {
		req.read = true
	}
	if options, ok := object["prompt_cache_options"].(map[string]any); ok {
		mode := cacheStringField(options, "mode")
		ttl := cacheStringField(options, "ttl")
		if mode != "" {
			req.read = true
		}
		// An explicit mode/TTL asks the provider to create or retain a
		// cache, so it needs a write rate rather than a guessed base rate.
		if mode == "explicit" || ttl != "" {
			req.addWriteTTL(ttl)
		}
		if ttl != "" {
			req.openAITTL = ttl
		}
	}
	for _, key := range []string{"messages", "input", "contents"} {
		if child, ok := object[key]; ok {
			inspectCacheContainers(child, req)
		}
	}
	for _, key := range []string{"system", "systemInstruction"} {
		if child, ok := object[key]; ok {
			inspectCacheSystem(child, req)
		}
	}
	if child, ok := object["content"]; ok {
		inspectCacheBlocks(child, req, true)
	}
	if child, ok := object["tools"]; ok {
		inspectCacheTools(child, req)
	}
	if config, ok := object["toolConfig"].(map[string]any); ok {
		if child, ok := config["tools"]; ok {
			inspectCacheTools(child, req)
		}
	}
	if req.breakpoints > 0 {
		req.addWriteTTL(req.openAITTL)
	}
}

func cachePricingRequired(body []byte) cachePricingRequirements {
	var value any
	if len(body) == 0 || json.Unmarshal(body, &value) != nil {
		return cachePricingRequirements{}
	}
	req := cachePricingRequirements{}
	inspectCacheJSON(value, &req, true)
	return req
}

func cachePricingAvailable(price *core.PriceSchedule, req cachePricingRequirements) bool {
	if !req.read && !req.write {
		return true
	}
	if price == nil {
		return false
	}
	if req.read && price.CachedInputPerMillion == nil {
		return false
	}
	if !req.write {
		return true
	}
	if req.otherTTL && price.CacheWriteInputPerMillion == nil {
		return false
	}
	if len(req.ttl) == 0 {
		return price.CacheWriteInputPerMillion != nil
	}
	for ttl := range req.ttl {
		switch ttl {
		case "5m":
			if price.CacheWrite5mPerMillion == nil && price.CacheWriteInputPerMillion == nil {
				return false
			}
		case "1h":
			if price.CacheWrite1hPerMillion == nil && price.CacheWriteInputPerMillion == nil {
				return false
			}
		}
	}
	return true
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
	// Input is inclusive of ordinary, cache-read and cache-write tokens. The
	// bound uses the highest configured input rate so cache writes cannot exceed
	// a base-only reservation. Explicit cache directives must first have a
	// usable category rate; otherwise the cost bound stays unknown instead of
	// falling back to a base or unit rate.
	cacheReq := cachePricingRequired(body)
	cacheRatesAvailable := cachePricingAvailable(m.Price, cacheReq)
	if m.Price != nil {
		out.PriceVersion = m.Price.Version
		if cacheRatesAvailable && input != nil && output != nil {
			cost, err := policy.EstimateBound(*input, *output, m.Price)
			if err == nil {
				out.MaximumCost = &cost
			} else if !errors.Is(err, policy.ErrUnknownCost) {
				return problem("unsupported_policy", 400, "cost bound is not representable")
			}
		}
		if cacheRatesAvailable && out.MaximumCost == nil && m.Price.MaximumUnitCost != nil && m.Price.UnitOperation == out.Operation {
			v := *m.Price.MaximumUnitCost
			out.MaximumCost = &v
		}
	}
	return nil
}

func planMaximumCost(input, output *int64, price *core.PriceSchedule, operation core.Operation) (*int64, error) {
	if price == nil {
		return nil, nil
	}
	if input != nil && output != nil {
		cost, err := policy.EstimateBound(*input, *output, price)
		if err == nil {
			return &cost, nil
		}
		if !errors.Is(err, policy.ErrUnknownCost) {
			return nil, err
		}
	}
	if price.MaximumUnitCost != nil && price.UnitOperation == operation {
		v := *price.MaximumUnitCost
		return &v, nil
	}
	return nil, nil
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
		if caps.Price != nil {
			if caps.Price.Version != p.PriceVersion {
				return core.AttemptPlan{}, problem("configuration_stale", 409, "price version changed")
			}
			// A nil maximum is an intentional unknown bound (for example,
			// an explicit cache directive whose category rate is not
			// configured). There is no request body at this authoritative
			// transaction boundary, so do not recompute a base/unit fallback
			// and resurrect a bound that planning deliberately withheld.
			if p.MaximumCost != nil {
				want, e := planMaximumCost(p.InputBound, p.OutputBound, caps.Price, p.Operation)
				if e != nil {
					return core.AttemptPlan{}, problem("invalid_configuration", 500, "stored price is invalid")
				}
				if want == nil || *p.MaximumCost != *want {
					return core.AttemptPlan{}, problem("configuration_stale", 409, "cost bound changed")
				}
			}
		} else if p.MaximumCost != nil || p.PriceVersion != "" {
			return core.AttemptPlan{}, problem("configuration_stale", 409, "price bound changed")
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
				var e error
				required, e = policy.CheckedAdd(*p.InputBound, *p.OutputBound)
				if e != nil {
					return core.AttemptPlan{}, problem("unsupported_policy", 400, "token bound is not representable")
				}
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
