package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"hoorific/internal/catalog"
	"hoorific/internal/core"
	"hoorific/internal/routing"
)

const operationPayloadLimit = 1 << 20

func decodeOperationInput(data json.RawMessage, out any) error {
	if len(data) > operationPayloadLimit {
		return actionError("payload_too_large", 413, "Action payload exceeds limit")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = json.RawMessage(`{}`)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return actionError("invalid_request", 400, "Action data must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return actionError("invalid_request", 400, "Invalid action data")
	}
	if d.Decode(new(any)) != io.EOF {
		return actionError("invalid_request", 400, "Invalid action data")
	}
	return nil
}

type routeTargetInput struct {
	ConnectionID string `json:"connection_id"`
	ModelID      string `json:"model_id"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
	Region       string `json:"region"`
}

func (a *Actions) routeAction(ctx context.Context, p core.Principal, id string, data json.RawMessage) (json.RawMessage, error) {
	var in struct {
		Alias            string             `json:"alias"`
		Targets          []routeTargetInput `json:"targets"`
		Operation        core.Operation     `json:"operation"`
		Features         []string           `json:"features"`
		InputModalities  []string           `json:"input_modalities"`
		OutputModalities []string           `json:"output_modalities"`
		InputTokens      *int64             `json:"input_tokens"`
		OutputTokens     *int64             `json:"output_tokens"`
		Region           string             `json:"region"`
		Fallback         bool               `json:"fallback"`
		AccountPoolID    string             `json:"account_pool_id"`
		Affinity         bool               `json:"affinity"`
		Residency        []string           `json:"residency"`
		Description      string             `json:"description"`
		Enabled          bool               `json:"enabled"`
	}
	if err := decodeOperationInput(data, &in); err != nil {
		return nil, err
	}
	if in.Operation == "" {
		in.Operation = core.Operation("generate")
	}
	policy, policyErr := a.deps.Store.Get(ctx, p, "route_policies", id)
	alias := in.Alias
	if policyErr == nil {
		var stored struct {
			Alias string `json:"alias"`
		}
		if json.Unmarshal(policy.Data, &stored) == nil && alias == "" {
			alias = stored.Alias
		}
	} else {
		return nil, policyErr
	}
	if alias == "" {
		alias = id
	}
	snap, err := a.deps.Store.Snapshot(core.WithPrincipal(ctx, p))
	if err != nil {
		return nil, actionError("configuration_stale", 503, "Runtime snapshot is unavailable")
	}
	cat, err := catalog.Compile(snap)
	if err != nil {
		return nil, actionError("configuration_stale", 503, "Runtime catalog is invalid")
	}
	targets := cat.Targets(alias)
	if len(targets) == 0 && len(in.Targets) > 0 {
		converted := make([]core.RouteTarget, 0, len(in.Targets))
		for _, t := range in.Targets {
			converted = append(converted, core.RouteTarget{ConnectionID: t.ConnectionID, ModelID: t.ModelID, Priority: t.Priority, Weight: t.Weight, Region: t.Region})
		}
		snap.Aliases[alias] = converted
		cat, err = catalog.Compile(snap)
		if err != nil {
			return nil, actionError("configuration_stale", 503, "Route target graph is invalid")
		}
		targets = cat.Targets(alias)
	}
	if len(targets) == 0 {
		return nil, actionError("not_found", 404, "Stored route has no targets")
	}
	type explanation struct {
		Target     core.RouteTarget `json:"target"`
		Compatible bool             `json:"compatible"`
		Eligible   bool             `json:"eligible"`
		Reason     string           `json:"reason"`
	}
	out := make([]explanation, 0, len(targets))
	best := int(^uint(0) >> 1)
	req := routing.Requirements{Operation: in.Operation, Features: in.Features, InputModalities: in.InputModalities, OutputModalities: in.OutputModalities, InputTokens: in.InputTokens, OutputTokens: in.OutputTokens, Region: in.Region}
	for _, t := range targets {
		e := explanation{Target: t, Reason: "incompatible_requirements"}
		c, ok := cat.Connection(t.ConnectionID)
		if !ok || c.TenantID != p.TenantID {
			e.Reason = "connection_unavailable"
		} else if _, err := a.connector(c); err != nil {
			e.Reason = "connection_unavailable"
		} else {
			// Compile a one-target catalog and ask the production router to apply its
			// exact manifest, feature, modality, token, priority, and region checks.
			single := snap
			single.Aliases = map[string][]core.RouteTarget{alias: {t}}
			one, compileErr := catalog.Compile(single)
			if compileErr == nil {
				router, _ := routing.New(one, nil)
				if _, selectErr := router.Select(ctx, alias, req); selectErr == nil {
					e.Compatible = true
					e.Reason = "compatible"
					if t.Priority < best {
						best = t.Priority
					}
				}
			}
		}
		out = append(out, e)
	}
	for i := range out {
		if out[i].Compatible {
			out[i].Eligible = out[i].Target.Priority == best
			if !out[i].Eligible {
				out[i].Reason = "lower_priority"
			}
		}
	}
	outData, err := json.Marshal(map[string]any{"alias": alias, "revision": cat.Revision(), "targets": out, "selection": "weighted among compatible targets at best priority", "health": "not_checked", "dry_run": true})
	if err != nil {
		return nil, err
	}
	if len(outData) > operationPayloadLimit {
		return nil, actionError("payload_too_large", 413, "Route explanation exceeds limit")
	}
	return outData, nil
}

type persistedJobDescriptor struct {
	ResultAction string                    `json:"result_action"`
	CancelAction string                    `json:"cancel_action"`
	Params       map[string]string         `json:"params"`
	AttemptID    string                    `json:"AttemptID"`
	Policy       core.NativeResponsePolicy `json:"Policy"`
	Binding      core.Binding              `json:"Binding"`
}

func (a *Actions) jobAction(ctx context.Context, p core.Principal, id, action string, version int64, data json.RawMessage) (json.RawMessage, error) {
	if action != "result" && action != "cancel" {
		return nil, actionError("unsupported_operation", 404, "Job action is not supported")
	}
	if len(data) > operationPayloadLimit {
		return nil, actionError("payload_too_large", 413, "Action payload exceeds limit")
	}
	if a.deps.Store == nil {
		return nil, actionError("configuration_stale", 503, "Administrative storage unavailable")
	}
	job, storedVersion, err := a.deps.Store.GetJobAction(ctx, p, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, actionError("not_found", 404, "Job not found")
		}
		return nil, actionError("configuration_stale", 503, "Job storage is unavailable")
	}
	if storedVersion <= 0 || version <= 0 || storedVersion != version {
		return nil, actionError("version_conflict", 412, "Job changed; reload before acting")
	}
	if job.TenantID != p.TenantID || job.ConnectionID == "" || job.AccountID == "" || job.ResourceID != id {
		return nil, actionError("configuration_stale", 503, "Stored job identity is invalid")
	}
	conn, _, err := a.connection(ctx, p, job.ConnectionID)
	if err != nil {
		return nil, err
	}
	if conn.AccountID != job.AccountID {
		return nil, actionError("configuration_stale", 503, "Stored job connection mapping is stale")
	}
	connector, err := a.connector(conn)
	if err != nil {
		return nil, err
	}
	binder, ok := connector.(core.EndpointBinder)
	if !ok {
		return nil, actionError("unsupported_operation", 400, "Connector cannot bind native endpoints")
	}
	var descriptor persistedJobDescriptor
	if len(job.Metadata) == 0 || json.Unmarshal(job.Metadata, &descriptor) != nil {
		return nil, actionError("configuration_stale", 503, "Stored job endpoint descriptor is invalid")
	}
	endpointAction := descriptor.ResultAction
	if endpointAction == "" {
		for path, candidate := range descriptor.Policy.ContinuationActions {
			if strings.Contains(strings.ToLower(path), "result") || strings.Contains(strings.ToLower(path), "response") || strings.HasSuffix(candidate, ".result") {
				endpointAction = candidate
				break
			}
		}
	}
	if endpointAction == "" {
		endpointAction = descriptor.Policy.PollAction
	}
	if action == "cancel" {
		endpointAction = descriptor.CancelAction
		if endpointAction == "" {
			for path, candidate := range descriptor.Policy.ContinuationActions {
				if strings.Contains(strings.ToLower(path), "cancel") || strings.HasSuffix(candidate, ".cancel") || strings.HasSuffix(candidate, ".delete") {
					endpointAction = candidate
					break
				}
			}
		}
	}
	if endpointAction == "" || len(endpointAction) > 256 || strings.ContainsAny(endpointAction, "\x00\r\n") {
		return nil, actionError("unsupported_operation", 400, "Job has no explicit native action")
	}
	if job.Operation == "" {
		return nil, actionError("configuration_stale", 503, "Stored job operation is missing")
	}
	var endpoints []core.NativeEndpoint
	if inventory, ok := connector.(core.EndpointInventory); ok {
		endpoints = inventory.Endpoints()
	}
	if inventory, ok := connector.(core.ConnectionInventory); ok {
		endpoints, err = inventory.EndpointsFor(conn)
		if err != nil {
			return nil, err
		}
	}
	var ep core.NativeEndpoint
	found := false
	for _, candidate := range endpoints {
		if candidate.Action == endpointAction {
			ep = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, actionError("unsupported_operation", 400, "Connector inventory does not advertise this job action")
	}
	params := make(map[string]string, len(descriptor.Params)+3)
	for k, v := range descriptor.Params {
		params[k] = v
	}
	params["id"] = job.ResourceID
	params["resource_id"] = job.ResourceID
	if ep.ResourceIDField != "" {
		params[ep.ResourceIDField] = job.ResourceID
	}
	if a.deps.Client == nil {
		return nil, actionError("configuration_stale", 503, "Connection HTTP client unavailable")
	}
	claimed, fenceVersion, err := a.deps.Store.ClaimJobAction(ctx, p, id, version)
	if err != nil {
		return nil, err
	}
	owner := "admin:" + p.SubjectID
	release := func(status string) error {
		claimed.Status = status
		claimed.LeaseOwner = ""
		claimed.LeaseUntil = time.Time{}
		return a.deps.Store.UpdateJob(ctx, claimed, owner, claimed.Fence)
	}
	binding, err := binder.BindEndpoint(ctx, conn, ep, params)
	if err != nil {
		_ = release(job.Status)
		return nil, err
	}
	if action == "cancel" && !binding.CancellationSupported {
		_ = release(job.Status)
		return nil, actionError("unsupported_operation", 400, "Native endpoint is not cancellable")
	}
	if action == "result" && (strings.HasSuffix(endpointAction, ".cancel") || strings.HasSuffix(endpointAction, ".delete")) {
		_ = release(job.Status)
		return nil, actionError("unsupported_operation", 400, "Native endpoint is not a result action")
	}
	target, err := joinConnectionURL(conn.BaseURL, binding.Endpoint)
	if err != nil {
		_ = release(job.Status)
		return nil, err
	}
	client, err := a.deps.Client(conn)
	if err != nil {
		_ = release(job.Status)
		return nil, err
	}
	if client == nil {
		_ = release(job.Status)
		return nil, actionError("configuration_stale", 503, "Connection HTTP client unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, binding.Method, target, nil)
	if err != nil {
		_ = release(job.Status)
		return nil, err
	}
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	for k, values := range binding.Headers {
		req.Header[k] = append([]string(nil), values...)
	}
	if a.deps.Credentials == nil {
		_ = release(job.Status)
		return nil, actionError("connection_required", 400, "Connection credential source unavailable")
	}
	lease, err := a.deps.Credentials.Lease(ctx, conn)
	if err != nil {
		_ = release(job.Status)
		return nil, err
	}
	if lease == nil {
		_ = release(job.Status)
		return nil, actionError("connection_required", 400, "Connection credential lease unavailable")
	}
	authErr := lease.Authorize(ctx, req)
	closeErr := core.CloseCredentialLease(lease)
	if authErr != nil {
		_ = release(job.Status)
		return nil, authErr
	}
	if closeErr != nil {
		_ = release(job.Status)
		return nil, closeErr
	}
	resp, err := client.Do(req)
	if err != nil {
		_ = release(job.Status)
		return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: "Native job operation outcome is unknown", Origin: "upstream", Retryable: true}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, operationPayloadLimit+1))
	if readErr != nil || len(body) > operationPayloadLimit {
		_ = release(job.Status)
		return nil, actionError("upstream_outcome_unknown", 502, "Native job response is unavailable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = release(job.Status)
		return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: "Native job operation failed", Origin: "upstream", Retryable: resp.StatusCode >= 500}
	}
	status := job.Status
	if action == "cancel" {
		status = "cancellation_requested"
	}
	if err := release(status); err != nil {
		return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: "Native job operation completed but durable state is unknown", Origin: "gateway", Retryable: true}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte(`{}`)
	}
	var raw json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil, actionError("upstream_outcome_unknown", 502, "Native job returned invalid JSON")
	}
	return json.Marshal(map[string]any{"action": action, "status_code": resp.StatusCode, "data": raw, "version": fenceVersion + 1})
}
