// Package gateway owns authorization, dispatch and finalization for all ingress protocols.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/credential"
	"hoorific/internal/protocol"
	"hoorific/internal/realtime"
	"hoorific/internal/resource"
	"hoorific/internal/routing"
	"hoorific/internal/transport"
)

type Dependencies struct {
	Auth        core.KeyAuthenticator
	Snapshots   core.SnapshotSource
	Admission   core.AdmissionStore
	Planner     core.AdmissionPlanner
	Credentials core.CredentialSource
	Idempotency core.IdempotencyStore
	Connectors  map[string]core.Connector
	Codecs      map[core.CodecKey]protocol.Entry
	Client      func(core.Connection) (*http.Client, error)
	Spool       *transport.Spool
	Resources   resource.Store
	Keys        credential.Keyring
	Tickets     realtime.TicketStore
	Metrics     *Metrics
	Hints       core.ScopedRoutingHints
	PublicURL   string
}
type Gateway struct {
	deps       Dependencies
	draining   atomic.Bool
	bodyMemory atomic.Int64
	uploads    chan struct{}
}

func New(d Dependencies) (*Gateway, error) {
	if d.Auth == nil || d.Snapshots == nil || d.Admission == nil || d.Planner == nil || d.Credentials == nil || d.Client == nil || d.Spool == nil {
		return nil, fmt.Errorf("gateway dependencies must be explicit")
	}
	return &Gateway{deps: d, uploads: make(chan struct{}, 16)}, nil
}
func (g *Gateway) Drain() { g.draining.Store(true) }
func requestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID()
	w.Header().Set("X-Request-ID", id)
	if r.Method == http.MethodOptions {
		origin, valid := canonicalOrigin(r.Header.Get("Origin"))
		if !valid {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		requested := r.Header.Get("Access-Control-Request-Headers")
		if strings.ContainsAny(requested, "\r\n") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		method := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
		if method != "" && method != "GET" && method != "POST" && method != "OPTIONS" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, hasPrincipal := core.PrincipalFromContext(r.Context())
		earlySnapshot := core.RuntimeSnapshot{}
		if hasPrincipal {
			var err error
			earlySnapshot, err = g.deps.Snapshots.Snapshot(r.Context())
			if err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if !g.originAllowed(r, earlySnapshot.Policy) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if requested != "" {
			w.Header().Set("Access-Control-Allow-Headers", requested)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	x, err := identify(r)
	if err != nil {
		writeError(w, x.protocol, err)
		return
	}
	ctx, requestTelemetry := gatewayStartRequest(r, x)
	defer gatewayEndRequest(ctx, requestTelemetry)
	r = r.WithContext(ctx)
	w, finishMetrics := g.deps.Metrics.Wrap(w, x.protocol)
	defer finishMetrics()
	if g.draining.Load() {
		writeError(w, x.protocol, failure("unavailable", 503, "gateway is draining"))
		return
	}
	if err = validateIngressHeaders(r); err != nil {
		writeError(w, x.protocol, err)
		return
	}
	p, ok := core.PrincipalFromContext(r.Context())
	ticketAuth := false
	if !ok {
		if r.URL.Query().Has("ticket") {
			p, err = g.authTicket(r, x)
			ticketAuth = err == nil
		} else {
			var token string
			token, err = extractKey(r, x)
			if err == nil {
				p, err = g.deps.Auth.AuthenticateKey(r.Context(), token)
			}
		}
		if err != nil {
			writeError(w, x.protocol, failure("unauthorized", 401, "invalid gateway credential"))
			return
		}
	}
	if p.KeyID == "" && p.SessionID != "" {
		p, err = g.sessionGrants(r.Context(), p)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
	}
	if idempotencyKey(r) != "" {
		p, err = g.reauthorizeIdempotency(r.Context(), r, x, p)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
	}
	stripCredentialQuery(r, x)
	r = r.WithContext(core.WithPrincipal(r.Context(), p))
	if ticketAuth {
		r = r.WithContext(context.WithValue(r.Context(), realtimeTicketAuthKey{}, true))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	snapshot, err := g.deps.Snapshots.Snapshot(ctx)
	if err != nil {
		writeError(w, x.protocol, failure("unavailable", 503, "configuration unavailable"))
		return
	}
	ctx = withTenantPolicy(ctx, snapshot.Policy)
	r = r.WithContext(ctx)
	if !g.originAllowed(r, snapshot.Policy) {
		writeError(w, x.protocol, failure("forbidden", http.StatusForbidden, "origin is not allowed"))
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	if x.operation == "realtime.ticket" {
		g.serveRealtimeTicket(w, r, p)
		return
	}
	if !x.native && !p.Portable {
		writeError(w, x.protocol, failure("forbidden", 403, "portable access is not granted"))
		return
	}
	if x.operation == "model.list" && !x.native {
		g.models(w, r, p, x)
		return
	}
	if x.native && !contains(p.Connections, x.connection) {
		writeError(w, x.protocol, failure("forbidden", 403, "connection access is not granted"))
		return
	}
	if idempotencyKey(r) != "" && ((r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")) || strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/vnd.amazon.eventstream")) {
		writeError(w, x.protocol, failure("unsupported_operation", http.StatusBadRequest, "idempotency is not supported for realtime or duplex sessions"))
		return
	}
	if (r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")) || strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/vnd.amazon.eventstream") {
		g.serveNativeSession(w, r, p, x, id)
		return
	}
	ctx = context.WithValue(ctx, nativeScopeQueryKey{}, r.URL.Query())
	release, e := g.admitBody(r)
	if e != nil {
		writeError(w, x.protocol, e)
		return
	}
	defer release()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(120 * time.Second))
	stopBody := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopBody()
	raw, fields, captured, err := g.capture(ctx, w, r, p, id)
	if err != nil {
		writeError(w, x.protocol, err)
		return
	}
	if captured != nil {
		defer captured.body.Close()
		ctx = context.WithValue(ctx, requestBodyKey{}, captured)
	}
	_ = controller.SetReadDeadline(time.Time{})
	if !x.native {
		if err = portableScope(x, fields); err != nil {
			writeError(w, x.protocol, err)
			return
		}
	}
	if x.model == "" {
		if v, ok := fields["model"]; ok {
			if json.Unmarshal(v, &x.model) != nil {
				writeError(w, x.protocol, failure("invalid_request", 400, "model must be a string"))
				return
			}
		}
	}
	gatewaySetRequestedModel(ctx, x.model)
	ctx, x.requirements, err = g.portableRequirements(ctx, x, raw, fields)
	if err != nil {
		writeError(w, x.protocol, err)
		return
	}
	if x.native && strings.HasPrefix(x.path, "continuations/") {
		c, ok := snapshot.Connections[x.connection]
		if !ok || c.TenantID != p.TenantID || c.Settings["disabled"] == "true" {
			writeError(w, x.protocol, failure("model_not_found", 404, "connection not found"))
			return
		}
		target, binding, continued, e := g.resolveContinuation(ctx, r, p, c, strings.TrimPrefix(x.path, "continuations/"))
		if e != nil {
			writeError(w, x.protocol, e)
			return
		}
		x.operation = binding.Codec.Operation
		if idempotencyKey(r) != "" {
			p, e = g.reauthorizeIdempotency(ctx, r, x, p)
			if e != nil {
				writeError(w, x.protocol, e)
				return
			}
			if c.TenantID != p.TenantID {
				writeError(w, x.protocol, failure("model_not_found", 404, "connection not found"))
				return
			}
			r = r.WithContext(core.WithPrincipal(r.Context(), p))
			target, binding, continued, e = g.resolveContinuation(ctx, r, p, c, strings.TrimPrefix(x.path, "continuations/"))
			if e != nil {
				writeError(w, x.protocol, e)
				return
			}
			x.operation = binding.Codec.Operation
			idem, replay, e := g.beginIdempotency(ctx, r, p, raw, captured)
			if e != nil {
				writeError(w, x.protocol, e)
				return
			}
			if replay != nil {
				replayErr := writeIdempotencyReplay(w, replay)
				if replayErr == nil && replay.Status >= 200 && replay.Status < 300 {
					gatewayMarkRequestSuccess(ctx)
				}
				return
			}
			idem.write = newIdempotencyCaptureWriter(w)
			defer idem.finish(ctx)
			w = idem.write
			continued = continued.WithContext(context.WithValue(continued.Context(), idempotencyStateKey{}, idem.state))
		}
		_, e = g.attempt(continued.Context(), w, continued, p, x, snapshot, target, raw, fields, id)
		if e != nil {
			writeError(w, x.protocol, e)
		}
		return
	}
	candidates, err := g.targets(ctx, snapshot, p, x, r.Method, raw, fields)
	if err != nil {
		writeError(w, x.protocol, err)
		return
	}
	if idempotencyKey(r) != "" {
		p, err = g.reauthorizeIdempotency(ctx, r, x, p)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
		ctx = core.WithPrincipal(ctx, p)
		r = r.WithContext(ctx)
		candidates, err = g.targets(ctx, snapshot, p, x, r.Method, raw, fields)
		if err != nil {
			writeError(w, x.protocol, err)
			return
		}
		idem, replay, e := g.beginIdempotency(ctx, r, p, raw, captured)
		if e != nil {
			writeError(w, x.protocol, e)
			return
		}
		if replay != nil {
			replayErr := writeIdempotencyReplay(w, replay)
			if replayErr == nil && replay.Status >= 200 && replay.Status < 300 {
				gatewayMarkRequestSuccess(ctx)
			}
			return
		}
		idem.write = newIdempotencyCaptureWriter(w)
		defer idem.finish(ctx)
		w = idem.write
		ctx = context.WithValue(ctx, idempotencyStateKey{}, idem.state)
		r = r.WithContext(ctx)
	}
	var last error
	replanned := false
	for attempt := 0; attempt < 2; {
		if len(candidates) <= attempt {
			break
		}
		t := candidates[attempt]
		if attempt > 0 {
			gatewayRecordRetry(ctx)
			g.deps.Metrics.Retry(x.protocol)
			fresh, e := g.deps.Snapshots.Snapshot(ctx)
			if e != nil {
				last = e
				break
			}
			freshTargets, e := g.targets(ctx, fresh, p, x, r.Method, raw, fields)
			if e != nil {
				last = e
				break
			}
			found := false
			for _, v := range freshTargets {
				if v.connection.ID == t.connection.ID && v.model.ID == t.model.ID {
					t = v
					snapshot = fresh
					found = true
					break
				}
			}
			if !found {
				last = failure("configuration_stale", 409, "fallback target is no longer eligible")
				break
			}
		}
		retry, e := g.attempt(ctx, w, r, p, x, snapshot, t, raw, fields, id)
		if isPreIntentStale(e) && !replanned {
			replanned = true
			if ctx.Err() != nil {
				last = ctx.Err()
				break
			}
			fresh, e := g.refreshSnapshot(ctx)
			if e != nil {
				last = e
				break
			}
			snapshot = fresh
			ctx = withTenantPolicy(ctx, snapshot.Policy)
			candidates, e = g.targets(ctx, snapshot, p, x, r.Method, raw, fields)
			if e != nil {
				last = e
				break
			}
			continue
		}
		attempt++
		if e == nil {
			return
		}
		last = e
		if !retry {
			break
		}
	}
	if last == nil {
		last = failure("model_not_found", 404, "no compatible target")
	}
	writeError(w, x.protocol, last)
}

type selected struct {
	connection core.Connection
	model      core.Model
	target     core.Target
	connector  core.Connector
	endpoint   *core.NativeEndpoint
	params     map[string]string
	route      core.RouteTarget
}

func (g *Gateway) targets(ctx context.Context, s core.RuntimeSnapshot, p core.Principal, x route, method string, raw []byte, fields map[string]json.RawMessage) ([]selected, error) {
	if x.native {
		c, ok := s.Connections[x.connection]
		if !ok || c.TenantID != p.TenantID || c.Settings["disabled"] == "true" {
			return nil, failure("model_not_found", 404, "connection not found")
		}
		connector, ok := g.deps.Connectors[c.Connector]
		if !ok {
			return nil, failure("unsupported_operation", 404, "connector is unavailable")
		}
		inventory, ok := connector.(core.EndpointInventory)
		if !ok {
			return nil, failure("unsupported_operation", 404, "connector has no native endpoints")
		}
		endpoints := inventory.Endpoints()
		if inv, ok := connector.(core.ConnectionInventory); ok {
			var err error
			endpoints, err = inv.EndpointsFor(c)
			if err != nil {
				return nil, err
			}
		}
		for _, ep := range endpoints {
			if ep.Method != method {
				continue
			}
			params, ok := matchEndpoint(ep.Path, x.path)
			if !ok {
				continue
			}
			if !contains(p.Operations, ep.Operation) {
				return nil, failure("forbidden", 403, "operation is not granted")
			}
			if !p.NativeAccount {
				target, err := g.constrainedNative(ctx, s, p, x, c, connector, ep, params, raw, fields)
				if err != nil {
					return nil, err
				}
				return []selected{target}, nil
			}
			if !c.Dedicated {
				return nil, failure("forbidden", 403, "native account endpoints require a dedicated connection")
			}
			resourceID := params[ep.ResourceIDField]
			if resourceID == "" {
				resourceID = params["id"]
			}
			target := core.ConnectionResourceCall{Connection: c, Action: ep.Action, ResourceID: resourceID}
			return []selected{{connection: c, target: target, connector: connector, endpoint: &ep, params: params}}, nil
		}
		return nil, failure("unsupported_operation", 404, "unknown native endpoint or action")
	}
	if !contains(p.Aliases, x.model) {
		return nil, failure("model_not_found", 404, "model alias not found")
	}
	if !contains(p.Operations, x.operation) {
		return nil, failure("forbidden", 403, "operation is not granted")
	}
	requirements := x.requirements
	requirements.Operation = x.operation
	if g.deps.Hints != nil && s.RoutePolicies[x.model].Affinity {
		scope := core.HealthScope{TenantID: p.TenantID, Revision: s.Revision}
		if scope.Valid() {
			if preferred, ok, e := g.deps.Hints.GetAffinityScoped(ctx, scope, affinityKey(p, x.model)); e == nil && ok {
				requirements.Preferred = &preferred
			}
		}
	}
	routes, err := routing.Order(ctx, s, x.model, requirements, g.deps.Hints)
	if err != nil {
		return nil, err
	}
	var result []selected
	for _, rt := range routes {
		if rt.Weight <= 0 {
			continue
		}
		c, ok := s.Connections[rt.ConnectionID]
		if !ok || c.TenantID != p.TenantID || c.Settings["disabled"] == "true" {
			continue
		}
		m, ok := s.Models[rt.ModelID]
		if !ok || m.ConnectionID != c.ID || !contains(m.Operations, x.operation) {
			continue
		}
		connector, ok := g.deps.Connectors[c.Connector]
		if !ok {
			continue
		}
		target := core.ModelCall{Connection: c, Model: m, Alias: x.model}
		result = append(result, selected{connection: c, model: m, target: target, connector: connector, route: rt})
	}
	if len(result) == 0 {
		return nil, failure("model_not_found", 404, "no compatible model is configured")
	}
	if conversation, ok := ctx.Value(portablePayloadKey{}).(core.Conversation); ok && conversation.Cache != nil && conversation.Cache.CachedContent != "" {
		first := result[0].connection
		for _, candidate := range result[1:] {
			c := candidate.connection
			if c.ID != first.ID || c.AccountID != first.AccountID || c.Project != first.Project || c.Region != first.Region {
				return nil, failure("unsupported_feature", 400, "cached content requires one connection, account, project, and region")
			}
		}
	}
	return result, nil
}
func (g *Gateway) attempt(ctx context.Context, w http.ResponseWriter, r *http.Request, p core.Principal, x route, s core.RuntimeSnapshot, t selected, raw []byte, fields map[string]json.RawMessage, id string) (retry bool, returnErr error) {
	var binding core.Binding
	var err error
	op := x.operation
	var stream bool
	var streamFieldPresent bool
	var suppressUsage bool
	if streamRaw, ok := fields["stream"]; ok {
		streamFieldPresent = true
		_ = json.Unmarshal(streamRaw, &stream)
	}
	stream = stream || strings.Contains(r.URL.Path, ":streamGenerateContent")
	if !streamFieldPresent && x.native && t.endpoint != nil && t.endpoint.Framing == "ndjson" {
		stream = true
	}
	continuation, isContinuation := ctx.Value(nativeContinuationKey{}).(nativeContinuationDispatch)
	if isContinuation {
		binding = continuation.Binding
		op = binding.Codec.Operation
	} else {
		if t.endpoint != nil {
			op = t.endpoint.Operation
			if b, ok := t.connector.(core.EndpointBinder); ok {
				binding, err = b.BindEndpoint(ctx, t.connection, *t.endpoint, t.params)
			} else {
				binding, err = t.connector.Bind(ctx, t.target, op)
			}
		} else if b, ok := t.connector.(core.StreamBinder); ok {
			binding, err = b.BindStream(ctx, t.target, op, stream)
		} else {
			binding, err = t.connector.Bind(ctx, t.target, op)
		}
		if err != nil {
			return false, err
		}
		if inspector, ok := t.connector.(core.ScopeInspector); ok {
			if err = inspector.Inspect(ctx, t.target, op, raw); err != nil {
				return false, err
			}
		}
	}
	if binding.Framing == "ndjson" && (binding.Codec.Protocol != "ollama" || !streamFieldPresent) {
		stream = true
	}
	if binding.ValidateRequestHeaders != nil {
		if err = binding.ValidateRequestHeaders(r.Header); err != nil {
			markIdempotency(ctx, "terminal")
			return false, err
		}
	}
	inputKey := core.CodecKey{Protocol: x.protocol, Operation: op}
	input, inputOK := g.deps.Codecs[inputKey]
	output, outputOK := g.deps.Codecs[binding.Codec]
	body := raw
	nativeWire := x.native || binding.Codec == inputKey
	if !x.native {
		var payload core.RequestPayload
		if !inputOK || input.Request == nil {
			if !nativeWire {
				return false, failure("unsupported_operation", 400, "media translation has no equivalent wire contract")
			}
			if err = mediaScope(op, fields); err != nil {
				return false, err
			}
		} else {
			payload, _ = ctx.Value(portablePayloadKey{}).(core.RequestPayload)
			if payload == nil {
				return false, failure("invalid_request", 400, "portable request was not decoded")
			}
			gatewaySetRequestPayload(ctx, payload)
			if conversation, ok := payload.(core.Conversation); ok && x.protocol == "openai-chat" {
				suppressUsage = conversation.StreamIncludeUsage == nil || !*conversation.StreamIncludeUsage
			}
			payload = withModel(payload, t.model.ID)
			// A portable OpenAI Chat stream must be translated when the caller
			// did not request usage. The gateway still forces usage upstream so
			// admission can settle from canonical provider metadata, but the
			// optional usage event must not leak into the client protocol.
			if stream && suppressUsage && binding.Codec.Protocol == "openai-chat" {
				nativeWire = false
			}
		}
		if nativeWire {
			if binding.ModelLocation == "body" || binding.ModelLocation == "" {
				copyFields := make(map[string]json.RawMessage, len(fields))
				for k, v := range fields {
					copyFields[k] = v
				}
				modelField := binding.ModelField
				if modelField == "" {
					modelField = "model"
				}
				if modelField != "model" {
					delete(copyFields, "model")
				}
				copyFields[modelField], _ = json.Marshal(t.model.ID)
				body, err = json.Marshal(copyFields)
			}
		} else {
			if !outputOK || output.Request == nil || output.Result == nil {
				return false, failure("unsupported_operation", 400, "upstream operation codec is unavailable")
			}
			var b bytes.Buffer
			err = output.Request.EncodeRequest(ctx, payload, &b)
			body = b.Bytes()
		}
		if err != nil {
			return false, err
		}
		if stream && !x.native && binding.Codec.Protocol == "openai-chat" {
			body, err = forceOpenAIStreamUsage(body)
			if err != nil {
				return false, failure("invalid_request", 400, "upstream stream usage option could not be encoded")
			}
		}
	}
	if !x.native {
		if adapter, ok := t.connector.(core.WireAdapter); ok {
			body, err = adapter.AdaptRequest(ctx, t.target, binding, body)
			if err != nil {
				return false, err
			}
		}
	}
	if len(body) == 0 && binding.DefaultBody != "" {
		body = []byte(binding.DefaultBody)
	}
	upstreamURL, err := joinEndpoint(t.connection.BaseURL, binding.Endpoint)
	if err != nil {
		return false, err
	}
	if isContinuation {
		upstreamURL = continuation.URL
	}
	crossOrigin := isContinuation && !sameOrigin(t.connection.BaseURL, continuation.URL)
	req, err := http.NewRequestWithContext(ctx, binding.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	defer func() {
		if req.Body != nil {
			_ = req.Body.Close()
		}
	}()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", r.Header.Get("Accept"))
	if captured, ok := ctx.Value(requestBodyKey{}).(*capturedBody); ok && binding.Framing != "h2-eventstream-duplex" {
		if !nativeWire {
			return false, failure("unsupported_feature", 400, "binary and multipart translation is not representable")
		}
		if !x.native && captured.multipart != nil {
			for i := range captured.multipart.parts {
				if captured.multipart.parts[i].Name == "model" {
					captured.multipart.parts[i].Value = []byte(t.model.ID)
				}
			}
		}
		reader, e := captured.body.Open(ctx)
		if e != nil {
			return false, e
		}
		req.Body = reader
		req.ContentLength = -1
		req.GetBody = func() (io.ReadCloser, error) { return captured.body.Open(ctx) }
		req.Header.Set("Content-Type", captured.contentType)
	}
	if !crossOrigin {
		for _, h := range binding.AllowedRequestHeaders {
			for _, v := range r.Header.Values(h) {
				req.Header.Add(h, v)
			}
		}
		for h, values := range binding.Headers {
			req.Header[h] = append([]string(nil), values...)
		}
	}
	transport.SanitizeRequest(req)
	if x.native && !isContinuation {
		q := req.URL.Query()
		for k, values := range r.URL.Query() {
			if k == "key" || k == "ticket" {
				continue
			}
			if _, fixed := q[k]; fixed {
				continue
			}
			q[k] = append([]string(nil), values...)
		}
		req.URL.RawQuery = q.Encode()
	}
	if binding.ModelLocation == "query" && t.model.ID != "" && !crossOrigin {
		q := req.URL.Query()
		q.Set("model", t.model.ID)
		req.URL.RawQuery = q.Encode()
	}
	var lease core.CredentialLease
	clientConnection := t.connection
	if crossOrigin {
		clientConnection = continuationConnection(t.connection, continuation.URL)
	}
	client, err := g.deps.Client(clientConnection)
	if err != nil {
		return false, err
	}
	if client == nil {
		return false, failure("unavailable", 503, "upstream HTTP client is unavailable")
	}
	if crossOrigin {
		isolated := *client
		isolated.Jar = nil
		isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &isolated
	}
	client = gatewayTraceClient(client)
	deadline, _ := ctx.Deadline()
	plan := core.AttemptPlan{TenantID: p.TenantID, RequestID: id, AttemptID: requestID(), KeyID: p.KeyID, KeyRevision: p.KeyRevision, ConfigRevision: s.Revision, ConnectionID: t.connection.ID, AccountID: t.connection.AccountID, ModelID: t.model.CatalogID, Deadline: deadline}
	plan, err = g.deps.Planner.PlanAttempt(ctx, p, s, t.target, op, body, plan)
	if err != nil {
		return false, markPreIntentStale(err)
	}
	permit, err := g.deps.Admission.BeginAttempt(ctx, plan)
	if err != nil {
		return false, markPreIntentStale(err)
	}
	ctx, attemptTelemetry := gatewayStartAttempt(ctx, x, binding, op, t.model.ID, t.connection.Connector, stream, gatewayIsRetry(ctx))
	req = req.WithContext(ctx)
	g.deps.Metrics.Attempt(binding.Codec.Protocol, op, t.connection.Connector)
	outcome := core.AttemptOutcome{TenantID: permit.TenantID, RequestID: permit.RequestID, AttemptID: permit.AttemptID, State: "outcome_unknown"}
	finalized := false
	defer func() {
		if !finalized {
			g.finalize(ctx, outcome)
		}
	}()
	defer func() { g.deps.Metrics.Outcome(x.protocol, outcome) }()
	defer func() { gatewayEndAttempt(ctx, attemptTelemetry, outcome, returnErr) }()
	if err = ctx.Err(); err != nil {
		outcome.State = "not_executed"
		return false, err
	}
	if !crossOrigin {
		lease, err = g.deps.Credentials.Lease(ctx, t.connection)
		if err != nil || lease == nil {
			closeCredentialLease(lease)
			outcome.State = "not_executed"
			return false, failure("unavailable", 503, "upstream credential unavailable")
		}
		defer closeCredentialLease(lease)
		if err = lease.Authorize(ctx, req); err != nil {
			outcome.State = "not_executed"
			return false, failure("unavailable", 503, "upstream authorization failed")
		}
	}
	injectGatewayTrace(ctx, req)
	if binding.Framing == "h2-eventstream-duplex" {
		outcome, err = g.executeDuplex(ctx, w, r, p, t, binding, plan, permit, lease, client, id)
		return false, err
	}
	if binding.Realtime != nil && binding.Realtime.DirectWebRTC {
		outcome, err = g.executeWebRTC(ctx, w, r, p, t, binding, plan, permit, req, client, id)
		return false, err
	}
	if binding.Framing == "websocket" {
		outcome, err = g.executeRealtime(ctx, w, r, p, t, binding, plan, permit, req, client, id)
		return false, err
	}
	gatewayMarkAttemptIssued(ctx)
	response, err := client.Do(req)
	if err != nil {
		g.recordUnknownNative(ctx, p, t, binding, permit, id)
		markIdempotency(ctx, "unknown")
		return false, failure("upstream_outcome_unknown", 502, "upstream execution outcome is unknown")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests && g.deps.Hints != nil && !x.native {
		scope := core.HealthScope{TenantID: p.TenantID, Revision: s.Revision}
		if scope.Valid() {
			_ = g.deps.Hints.SetScoped(ctx, scope, t.route, false, cooldownDuration(response.Header))
		}
	}
	if response.StatusCode == http.StatusTooManyRequests && !x.native {
		outcome.State = "not_executed"
		e := failure("quota_exceeded", http.StatusTooManyRequests, "upstream rejected the request without execution")
		e.RetryAfter = response.Header.Get("Retry-After")
		markIdempotency(ctx, "terminal")
		return true, e
	}
	if response.StatusCode >= 400 { // Native errors retain safe bytes and status; do not expose credentials or arbitrary HTML.
		data, e := transport.ReadBounded(ctx, response.Body, 1<<20, 120*time.Second)
		if e != nil {
			if x.native && response.StatusCode == http.StatusTooManyRequests {
				outcome.State = "not_executed"
				rejection := failure("upstream_error", http.StatusTooManyRequests, "upstream rejected the request")
				rejection.RetryAfter = response.Header.Get("Retry-After")
				markIdempotency(ctx, "terminal")
				return false, rejection
			}
			markIdempotency(ctx, "unknown")
			return false, failure("upstream_error", 502, "upstream error response was truncated")
		}
		if x.native && json.Valid(data) {
			copyResponseHeaders(w.Header(), response.Header)
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(response.StatusCode)
			if _, e = w.Write(data); e != nil {
				markIdempotency(ctx, "unknown")
				return false, nil
			}
			if response.StatusCode == http.StatusTooManyRequests {
				outcome.State = "not_executed"
				markIdempotency(ctx, "terminal")
			} else {
				markIdempotency(ctx, "unknown")
			}
			return false, nil
		}
		if x.native && response.StatusCode == http.StatusTooManyRequests {
			outcome.State = "not_executed"
			rejection := failure("upstream_error", http.StatusTooManyRequests, "upstream rejected the request")
			rejection.RetryAfter = response.Header.Get("Retry-After")
			markIdempotency(ctx, "terminal")
			return false, rejection
		}
		markIdempotency(ctx, "unknown")
		return false, failure("upstream_error", response.StatusCode, "upstream rejected the request")
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if err = g.deps.Admission.MarkAccepted(ctx, permit, fmt.Sprintf("http:%d", response.StatusCode)); err != nil {
			markIdempotency(ctx, "unknown")
			return false, failure("unavailable", 503, "acceptance persistence unavailable")
		}
	}
	if g.deps.Hints != nil && s.RoutePolicies[x.model].Affinity && !x.native {
		scope := core.HealthScope{TenantID: p.TenantID, Revision: s.Revision}
		if scope.Valid() {
			_ = g.deps.Hints.SetAffinityScoped(ctx, scope, affinityKey(p, x.model), t.route, 10*time.Minute)
		}
	}
	if !x.native {
		if adapter, ok := t.connector.(core.WireAdapter); ok {
			if err = adapter.AdaptResponse(ctx, binding, response); err != nil {
				return false, err
			}
		}
	}
	nativeState, err := g.prepareNativeResponse(ctx, r, response, p, t, binding, permit, id)
	if err != nil {
		markIdempotency(ctx, "unknown")
		return false, err
	}
	if nativeState.JobPending {
		outcome.State = "job_pending"
		finalized = true
		markIdempotency(ctx, "terminal")
	}
	if nativeWire {
		relayResult, seen, relayErr := relayNative(ctx, w, response, output, stream)
		if relayResult.CancellationRequested {
			outcome.Cancellation = "requested"
		}
		outcome.Usage = seen.usage
		if !nativeState.JobPending && relayErr == nil && seen.err == nil && seen.terminal {
			outcome.State = "settled"
			markIdempotency(ctx, "terminal")
		} else if relayErr != nil || seen.err != nil || relayResult.Truncated {
			markIdempotency(ctx, "unknown")
		}
		return false, nil
	}
	if stream {
		response.Body = transport.NewIdleReader(ctx, response.Body, 120*time.Second)
		if output.Stream == nil || input.Stream == nil {
			return false, failure("unsupported_operation", 400, "stream codec unavailable")
		}
		decoder, e := output.Stream.NewDecoder(response.Body)
		if e != nil {
			markIdempotency(ctx, "unknown")
			return false, e
		}
		encoder, e := input.Stream.NewEncoder(w)
		if e != nil {
			markIdempotency(ctx, "unknown")
			return false, e
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(response.StatusCode)
		terminal := false
		failed := false
		for {
			event, e := decoder.Next(ctx)
			if e != nil {
				if e != io.EOF || !terminal {
					failed = true
					markIdempotency(ctx, "unknown")
					_ = encoder.Write(ctx, core.StreamError{Error: failure("upstream_outcome_unknown", 502, "upstream stream was truncated")})
				}
				break
			}
			gatewayObserveEvent(ctx, event)
			if usage, ok := event.(core.Usage); ok {
				outcome.Usage = &usage
				if suppressUsage {
					continue
				}
			}
			if _, ok := event.(core.Finish); ok {
				terminal = true
			}
			if _, ok := event.(core.StreamError); ok {
				failed = true
				markIdempotency(ctx, "unknown")
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
			if e = encoder.Write(ctx, event); e != nil {
				outcome.Cancellation = "requested"
				markIdempotency(ctx, "unknown")
				return false, nil
			}
			_ = http.NewResponseController(w).Flush()
		}
		if terminal && !failed {
			outcome.State = "settled"
			markIdempotency(ctx, "terminal")
		}
		return false, nil
	}
	if output.Result == nil || input.Result == nil {
		markIdempotency(ctx, "unknown")
		return false, failure("unsupported_operation", 400, "result codec unavailable")
	}
	result, err := output.Result.DecodeResult(ctx, io.LimitReader(response.Body, 32<<20))
	if err != nil {
		markIdempotency(ctx, "unknown")
		return false, failure("upstream_outcome_unknown", 502, "upstream result could not be decoded")
	}
	gatewayObserveResult(ctx, result)
	var encoded bytes.Buffer
	if err = input.Result.EncodeResult(ctx, result, &encoded); err != nil {
		markIdempotency(ctx, "unknown")
		return false, err
	}
	outcome.Usage = resultUsage(result)
	outcome.State = "settled"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	if _, err = w.Write(encoded.Bytes()); err != nil {
		markIdempotency(ctx, "unknown")
		return false, nil
	}
	markIdempotency(ctx, "terminal")
	return false, nil
}
func withModel(p core.RequestPayload, model string) core.RequestPayload {
	switch v := p.(type) {
	case core.Conversation:
		v.Model = model
		return v
	case core.CompletionRequest:
		v.Model = model
		return v
	case core.EmbeddingRequest:
		v.Model = model
		return v
	case core.RerankRequest:
		v.Model = model
		return v
	case core.CountTokensRequest:
		v.Conversation.Model = model
		return v
	}
	return p
}
func resultUsage(p core.ResultPayload) *core.Usage {
	switch v := p.(type) {
	case core.GenerationResult:
		return v.Usage
	case core.CompletionResult:
		return v.Usage
	case core.EmbeddingResult:
		return v.Usage
	case core.RerankResult:
		return v.Usage
	}
	return nil
}
func forceOpenAIStreamUsage(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	var options map[string]json.RawMessage
	if raw := fields["stream_options"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &options); err != nil {
			return nil, err
		}
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	options["include_usage"] = json.RawMessage("true")
	encodedOptions, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	fields["stream_options"] = encodedOptions
	return json.Marshal(fields)
}
func joinEndpoint(base, path string) (string, error) {
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", failure("invalid_configuration", 503, "invalid upstream base URL")
	}
	if strings.Contains(path, "://") || strings.HasPrefix(path, "//") || strings.Contains(path, "#") {
		return "", failure("invalid_configuration", 503, "invalid upstream endpoint")
	}
	v, e := url.Parse("./" + strings.TrimPrefix(path, "/"))
	if e != nil || v.Host != "" {
		return "", failure("invalid_configuration", 503, "invalid upstream endpoint")
	}
	decoded := strings.TrimPrefix(v.Path, "./")
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." || part == "." {
			return "", failure("invalid_configuration", 503, "invalid upstream endpoint")
		}
	}
	escaped := strings.TrimSuffix(u.EscapedPath(), "/") + "/" + strings.TrimPrefix(strings.TrimPrefix(v.EscapedPath(), "./"), "/")
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(decoded, "/")
	u.RawPath = escaped
	u.RawQuery = v.RawQuery
	return u.String(), nil
}
func extractKey(r *http.Request, x route) (string, error) {
	var values []string
	for _, h := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
		for _, v := range r.Header.Values(h) {
			if h == "Authorization" {
				if !strings.HasPrefix(v, "Bearer ") {
					return "", failure("unauthorized", 401, "unsupported authorization scheme")
				}
				v = strings.TrimPrefix(v, "Bearer ")
			}
			if v != "" {
				values = append(values, v)
			}
		}
	}
	if x.protocol == "gemini-content" {
		for _, v := range r.URL.Query()["key"] {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return "", failure("unauthorized", 401, "gateway credential required")
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return "", failure("unauthorized", 401, "conflicting gateway credentials")
		}
	}
	return values[0], nil
}
func stripCredentialQuery(r *http.Request, x route) {
	if r == nil || x.protocol != "gemini-content" || r.URL == nil {
		return
	}
	query := r.URL.Query()
	if !query.Has("key") {
		return
	}
	query.Del("key")
	r.URL.RawQuery = query.Encode()
}
func copyResponseHeaders(dst, src http.Header) {
	for _, k := range []string{"Content-Type", "Content-Disposition", "Cache-Control", "Retry-After", "Request-Id", "X-Request-Id"} {
		if v := src.Get(k); v != "" && k != "X-Request-Id" {
			dst.Set(k, v)
		}
	}
}
func writeError(w http.ResponseWriter, p core.Protocol, err error) {
	e := failure("internal_error", 500, "request failed")
	var ge core.GatewayError
	if errors.As(err, &ge) {
		e = ge
	}
	if e.HTTPStatus < 400 {
		e.HTTPStatus = 500
	}
	w.Header().Set("X-Hoorific-Error-Code", e.Code)
	w.Header().Set("Content-Type", "application/json")
	if e.HTTPStatus == http.StatusTooManyRequests {
		if e.RetryAfter == "" {
			e.RetryAfter = "1"
		}
		w.Header().Set("Retry-After", e.RetryAfter)
	}
	w.WriteHeader(e.HTTPStatus)
	switch p {
	case "anthropic-messages":
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": e.Code, "message": e.Message}})
	case "gemini-content":
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": e.HTTPStatus, "message": e.Message, "status": e.Code}})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
	}
}
func (g *Gateway) models(w http.ResponseWriter, r *http.Request, p core.Principal, x route) {
	s, e := g.deps.Snapshots.Snapshot(r.Context())
	if e != nil {
		writeError(w, x.protocol, e)
		return
	}
	items := []map[string]any{}
	for _, alias := range p.Aliases {
		if _, ok := s.Aliases[alias]; ok {
			items = append(items, map[string]any{"id": alias, "object": "model", "owned_by": "gateway"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if x.protocol == "gemini-content" {
		models := []map[string]any{}
		for _, item := range items {
			models = append(models, map[string]any{"name": "models/" + item["id"].(string)})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"models": models}); err == nil {
			gatewayMarkRequestSuccess(r.Context())
		}
		return
	}
	if x.protocol == "anthropic-messages" {
		models := []map[string]any{}
		for _, item := range items {
			id := item["id"].(string)
			models = append(models, map[string]any{"id": id, "type": "model", "display_name": id, "created_at": "1970-01-01T00:00:00Z"})
		}
		var first, last any
		if len(models) > 0 {
			first = models[0]["id"]
			last = models[len(models)-1]["id"]
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": models, "has_more": false, "first_id": first, "last_id": last}); err == nil {
			gatewayMarkRequestSuccess(r.Context())
		}
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items}); err == nil {
		gatewayMarkRequestSuccess(r.Context())
	}
}
