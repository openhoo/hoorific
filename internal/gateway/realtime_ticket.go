package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/realtime"
)

// serveRealtimeTicket issues a one-use, short-lived browser ticket. The caller
// must have already authenticated the gateway principal; this endpoint never
// accepts an upstream credential or returns one.
func (g *Gateway) serveRealtimeTicket(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if g.deps.Tickets == nil {
		writeError(w, "native", failure("unsupported_operation", 404, "realtime browser tickets are unavailable"))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, "native", failure("unsupported_operation", 405, "ticket issuance requires POST"))
		return
	}
	if !p.Realtime || !p.NativeAccount {
		writeError(w, "native", failure("forbidden", 403, "realtime and native account grants are required"))
		return
	}
	if r.Body == nil || r.ContentLength > 16<<10 {
		writeError(w, "native", failure("invalid_request", 400, "ticket request is too large"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeError(w, "native", failure("invalid_request", 400, "invalid ticket request"))
		return
	}
	fields, err := strictObject(raw)
	if err != nil {
		writeError(w, "native", failure("invalid_request", 400, "invalid ticket request"))
		return
	}
	for key := range fields {
		if key != "connection_id" && key != "model" {
			writeError(w, "native", failure("unsupported_feature", 400, "unknown ticket field"))
			return
		}
	}
	var req struct {
		ConnectionID string `json:"connection_id"`
		Model        string `json:"model"`
	}
	if b := fields["connection_id"]; b == nil || json.Unmarshal(b, &req.ConnectionID) != nil {
		writeError(w, "native", failure("invalid_request", 400, "connection_id must be a string"))
		return
	}
	if b := fields["model"]; b == nil || json.Unmarshal(b, &req.Model) != nil {
		writeError(w, "native", failure("invalid_request", 400, "model must be a string"))
		return
	}
	req.ConnectionID = strings.TrimSpace(req.ConnectionID)
	req.Model = strings.TrimSpace(req.Model)
	if req.ConnectionID == "" || req.Model == "" {
		writeError(w, "native", failure("invalid_request", 400, "connection_id and model are required"))
		return
	}
	if !contains(p.Connections, req.ConnectionID) {
		writeError(w, "native", failure("forbidden", 403, "connection access is not granted"))
		return
	}
	snap, err := g.deps.Snapshots.Snapshot(r.Context())
	if err != nil {
		writeError(w, "native", failure("unavailable", 503, "configuration unavailable"))
		return
	}
	c, ok := snap.Connections[req.ConnectionID]
	if !ok || c.TenantID != p.TenantID || c.Settings["disabled"] == "true" || !c.Dedicated {
		writeError(w, "native", failure("forbidden", 403, "realtime requires a dedicated enabled connection"))
		return
	}
	m, ok := snap.Models[req.Model]
	if !ok || m.ConnectionID != c.ID || !contains(m.Operations, core.Operation("realtime")) {
		writeError(w, "native", failure("model_not_found", 404, "realtime model is not enabled on this connection"))
		return
	}
	token, err := realtime.IssueBoundTicket(r.Context(), g.deps.Tickets, p, c.ID, m.ID, time.Now())
	if err != nil {
		writeError(w, "native", failure("unavailable", 503, "realtime ticket could not be issued"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"ticket": token, "expires_in": 30, "connection_id": c.ID, "model": m.ID})
}

func realtimeTicketRoute(r *http.Request, x route) bool {
	if r == nil || !x.native || x.connection == "" || r.Method != http.MethodGet ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	// The ticket is a browser-session credential, not a general native
	// credential. Keep the pre-consumption check tied to the documented
	// realtime handshake paths; serveNativeSession performs the
	// connection-specific endpoint check once the ticket principal is known.
	switch x.protocol {
	case "openai-chat":
		return x.path == "realtime" || x.path == "realtime/translations"
	case "gemini-content":
		return x.path == "ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	case "native":
		// Vertex's native facade uses the same documented BidiGenerateContent
		// handshake but has a provider-specific path prefix.
		return strings.HasPrefix(x.path, "ws/") && strings.HasSuffix(x.path, "BidiGenerateContent")
	default:
		return false
	}
}

type realtimeTicketAuthKey struct{}

// authTicket authenticates a browser WebSocket using its ticket as the sole
// bearer credential. ConsumeTicket atomically binds route scope and marks the
// ticket used; the returned principal is then revalidated by the SQL planner.
func (g *Gateway) authTicket(r *http.Request, x route) (core.Principal, error) {
	if !realtimeTicketRoute(r, x) {
		return core.Principal{}, failure("unauthorized", 401, "realtime ticket requires a native realtime WebSocket")
	}
	if g.deps.Tickets == nil {
		return core.Principal{}, failure("unsupported_operation", 404, "realtime browser tickets are unavailable")
	}
	values := r.URL.Query()["ticket"]
	if len(values) != 1 || values[0] == "" {
		return core.Principal{}, failure("unauthorized", 401, "realtime ticket required")
	}
	for _, h := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		if len(r.Header.Values(h)) > 0 {
			return core.Principal{}, failure("unauthorized", 401, "realtime ticket cannot be combined with an API key")
		}
	}
	if len(r.URL.Query()["key"]) > 0 {
		return core.Principal{}, failure("unauthorized", 401, "realtime ticket cannot be combined with an API key")
	}
	connection, model := x.connection, x.model
	if connection == "" {
		connection = r.URL.Query().Get("connection_id")
	}
	if model == "" {
		model = r.URL.Query().Get("model")
	}
	if connection == "" || model == "" {
		return core.Principal{}, failure("invalid_request", 400, "realtime ticket route requires connection and model")
	}
	q := r.URL.Query()
	q.Del("ticket")
	q.Del("key")
	r.URL.RawQuery = q.Encode()
	ticket, err := realtime.ConsumeTicket(r.Context(), g.deps.Tickets, values[0], time.Now(), connection, model)
	if err != nil {
		return core.Principal{}, failure("unauthorized", 401, "invalid or expired realtime ticket")
	}
	p := ticket.Principal
	if p.TenantID == "" || (p.KeyID == "" && p.SessionID == "") || !p.Realtime || !p.NativeAccount {
		return core.Principal{}, failure("unauthorized", 401, "invalid realtime ticket principal")
	}
	if err = realtime.AuthorizeTicket(ticket, p.TenantID, connection, model, "realtime"); err != nil {
		return core.Principal{}, failure("unauthorized", 401, "invalid realtime ticket scope")
	}
	return p, nil
}
