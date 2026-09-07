package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"hoorific/internal/core"
)

func bindSessionModel(s core.RuntimeSnapshot, t selected, model string) (selected, error) {
	if model == "" {
		model = t.params["model"]
	}
	if model == "" {
		return t, failure("model_not_found", 404, "realtime requires an explicit approved model")
	}
	var selectedModel core.Model
	matches := 0
	if m, ok := s.Models[model]; ok && m.ConnectionID == t.connection.ID {
		selectedModel = m
		matches = 1
	} else {
		for _, m := range s.Models {
			if m.ConnectionID == t.connection.ID && m.ID == model {
				selectedModel = m
				matches++
			}
		}
	}
	if matches != 1 || !contains(selectedModel.Operations, core.Operation("realtime")) {
		return t, failure("model_not_found", 404, "realtime model is not approved")
	}
	t.model = selectedModel
	t.target = core.ModelCall{Connection: t.connection, Model: selectedModel}
	if t.params == nil {
		t.params = map[string]string{}
	}
	t.params["model"] = selectedModel.ID
	return t, nil
}
func (g *Gateway) serveNativeSession(w http.ResponseWriter, r *http.Request, p core.Principal, x route, id string) {
	if !x.native || !p.Realtime || !p.NativeAccount {
		writeError(w, x.protocol, failure("forbidden", 403, "native realtime grants are required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Minute)
	defer cancel()
	snapshot, err := g.deps.Snapshots.Snapshot(ctx)
	if err != nil {
		writeError(w, x.protocol, err)
		return
	}
	ctx = withTenantPolicy(ctx, snapshot.Policy)
	r = r.WithContext(ctx)
	w.Header().Add("Vary", "Origin")
	if !g.originAllowed(r, snapshot.Policy) {
		writeError(w, x.protocol, failure("forbidden", http.StatusForbidden, "origin is not allowed"))
		return
	}
	ctx = context.WithValue(ctx, nativeScopeQueryKey{}, r.URL.Query())
	r = r.WithContext(ctx)
	for refreshed := false; ; {
		if err = ctx.Err(); err != nil {
			writeError(w, x.protocol, err)
			return
		}
		// Session frames are not a JSON request body. Leave the duplex stream unread;
		// the model-only planning payload below is not original scope evidence.
		targets, err := g.targets(ctx, snapshot, p, x, r.Method, nil, nil)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
		if len(targets) != 1 || targets[0].endpoint == nil {
			writeError(w, x.protocol, failure("unsupported_operation", 400, "native session endpoint is unavailable"))
			return
		}
		target := targets[0]
		ticketAuth, _ := r.Context().Value(realtimeTicketAuthKey{}).(bool)
		if ticketAuth && (target.endpoint.Method != http.MethodGet || target.endpoint.Framing != "websocket" || target.endpoint.Operation != core.Operation("realtime")) {
			writeError(w, x.protocol, failure("unauthorized", 401, "realtime ticket is not valid for this endpoint"))
			return
		}
		if target.endpoint.Framing != "websocket" && target.endpoint.Framing != "h2-eventstream-duplex" {
			writeError(w, x.protocol, failure("unsupported_operation", 400, "endpoint does not support a native session"))
			return
		}
		target, err = bindSessionModel(snapshot, target, x.model)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
		attemptRoute := x
		attemptRoute.operation = target.endpoint.Operation
		fields := map[string]json.RawMessage{}
		fields["model"], _ = json.Marshal(target.model.ID)
		raw, _ := json.Marshal(fields)
		_, err = g.attempt(ctx, w, r, p, attemptRoute, snapshot, target, raw, fields, id)
		if err == nil {
			return
		}
		// Only this marker proves planning/admission stopped before durable intent.
		// Transport errors, acceptance failures and finalized sessions are never retried.
		if refreshed || !isPreIntentStale(err) {
			writeError(w, x.protocol, err)
			return
		}
		if err = ctx.Err(); err != nil {
			writeError(w, x.protocol, err)
			return
		}
		refreshed = true
		snapshot, err = g.refreshSnapshot(ctx)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
		ctx = withTenantPolicy(ctx, snapshot.Policy)
		r = r.WithContext(ctx)
		if !g.originAllowed(r, snapshot.Policy) {
			writeError(w, x.protocol, failure("forbidden", http.StatusForbidden, "origin is not allowed"))
			return
		}
	}
}
