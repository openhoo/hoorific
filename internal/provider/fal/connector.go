// Package fal binds explicitly configured fal model endpoints without translating
// their model-specific input or output schemas.
package fal

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

const (
	InferenceBaseURL = "https://fal.run"
	QueueBaseURL     = "https://queue.fal.run"
	EndpointSetting  = "endpoint"
)

// Connector requires Connection.Settings["endpoint"] (owner/app[/subpath])
// and an explicit Connection.BaseURL. Direct and queue calls use separate
// connections because core.Binding deliberately cannot change the origin.
// Model IDs and request parameters never choose the configured endpoint.
type Connector struct {
	*endpoint.Connector
}

func New(opts ...endpoint.Option) *Connector {
	return &Connector{Connector: endpoint.New("fal", QueueBaseURL, routes(), opts...)}
}

// EndpointsFor removes the ambiguous POST route before gateway matching.
// Direct and queue connections have separate, explicitly configured origins.
func (c *Connector) EndpointsFor(conn core.Connection) ([]core.NativeEndpoint, error) {
	origin, err := connectionOrigin(conn)
	if err != nil {
		return nil, err
	}
	full, app, err := configuredPaths(conn)
	if err != nil {
		return nil, err
	}
	direct := origin == "fal.run"
	var endpoints []core.NativeEndpoint
	for _, e := range c.Endpoints() {
		if (e.Action == "inference") != direct {
			continue
		}
		configured := full
		pattern := e.Path
		if e.Action != "inference" && e.Action != "queue.submit" {
			configured = app
			if suffix := strings.Index(pattern, "/requests/"); suffix >= 0 {
				pattern = pattern[:suffix]
			}
		}
		parts, actual := strings.Split(pattern, "/"), strings.Split(configured, "/")
		if len(parts) != len(actual) {
			continue
		}
		matches := true
		for i, part := range parts {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				continue
			}
			if part != actual[i] {
				matches = false
				break
			}
		}
		if matches {
			endpoints = append(endpoints, e)
		}
	}
	return endpoints, nil
}

// Official queue methods and framing:
// https://fal.ai/docs/documentation/model-apis/inference/queue
// The SDK strips the inference subpath for ALL lifecycle operations and uses
// GET requests/{request_id}, NOT requests/{request_id}/response. The latter
// appears in documentation response examples but is not the SDK result route:
// https://github.com/fal-ai/fal-js/blob/main/libs/client/src/queue.ts
// https://github.com/fal-ai/fal-js/blob/main/libs/client/src/utils.ts
// Direct inference: https://fal.ai/docs/documentation/model-apis/inference
func routes() []endpoint.Route {
	r := make([]endpoint.Route, 0, 18)
	for n := 2; n <= 8; n++ {
		parts := make([]string, n)
		parts[0], parts[1] = "{owner}", "{app}"
		for i := 2; i < n; i++ {
			parts[i] = "{path" + strconv.Itoa(i-2) + "}"
		}
		endpointPath := strings.Join(parts, "/")
		r = append(r,
			endpoint.E(http.MethodPost, endpointPath, "inference", "native", "native", "json", "none", false),
			endpoint.E(http.MethodPost, endpointPath, "queue.submit", "native", "native", "json", "none", true),
		)
	}
	for _, namespace := range []string{"", "workflows/", "comfy/"} {
		app := namespace + "{owner}/{app}"
		r = append(r,
			endpoint.E(http.MethodGet, app+"/requests/{id}/status", "queue.status", "native", "native", "json", "none", true),
			endpoint.E(http.MethodGet, app+"/requests/{id}/status/stream", "queue.status-stream", "native", "native", "sse", "none", true),
			endpoint.E(http.MethodGet, app+"/requests/{id}", "queue.result", "native", "native", "json", "none", true),
			endpoint.E(http.MethodPut, app+"/requests/{id}/cancel", "queue.cancel", "native", "native", "json", "none", true),
		)
	}
	for i := range r {
		r[i].Variant = "fal"
		r[i].Response = responsePolicy(r[i].Action)
		if strings.Contains(r[i].Path, "/requests/{id}") {
			r[i].ResourceIDField = "id"
		}
	}
	return r
}

func failure(code, message string) error {
	return core.GatewayError{Code: code, HTTPStatus: http.StatusBadRequest, Message: message, Origin: "gateway"}
}

// configuredPaths accepts unescaped path components only, then escapes once.
// Namespaced workflows/comfy applications have three root components, as in
// the official SDK parser. Empty components and traversal are never normalized.
func configuredPaths(conn core.Connection) (full, app string, err error) {
	parts := strings.Split(conn.Settings[EndpointSetting], "/")
	root := 2
	if parts[0] == "workflows" || parts[0] == "comfy" {
		root = 3
	}
	if len(parts) < root {
		return "", "", failure("configuration_stale", "fal requires an explicit endpoint in owner/app[/subpath] form")
	}
	for i, part := range parts {
		for _, ch := range part {
			if ch <= ' ' || ch == 127 {
				return "", "", failure("configuration_stale", "fal endpoint contains whitespace or control characters")
			}
		}
		parts[i], err = endpoint.Segment(part)
		if err != nil {
			return "", "", failure("configuration_stale", "fal endpoint contains an unsafe path component")
		}
	}
	return strings.Join(parts, "/"), strings.Join(parts[:root], "/"), nil
}

func connectionOrigin(conn core.Connection) (string, error) {
	u, err := url.Parse(conn.BaseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", failure("configuration_stale", "fal requires an explicit HTTPS origin without a path, credentials, query or fragment")
	}
	switch u.Host {
	case "fal.run", "queue.fal.run":
		return u.Host, nil
	default:
		return "", failure("configuration_stale", "fal origin must be fal.run or queue.fal.run")
	}
}

func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	if op != "native" {
		return core.Binding{}, failure("unsupported_operation", "fal exposes only endpoint-specific native operations")
	}
	var conn core.Connection
	var action, id string
	switch call := target.(type) {
	case core.ModelCall:
		approved := false
		for _, allowed := range call.Model.Operations {
			approved = approved || allowed == op
		}
		if !approved {
			return core.Binding{}, failure("unsupported_operation", "Model manifest does not approve native inference")
		}
		conn = call.Connection
		origin, err := connectionOrigin(conn)
		if err != nil {
			return core.Binding{}, err
		}
		action = "inference"
		if origin == "queue.fal.run" {
			action = "queue.submit"
		}
	case core.ConnectionResourceCall:
		conn, action, id = call.Connection, call.Action, call.ResourceID
	default:
		return core.Binding{}, failure("unsupported_operation", "Unknown fal target")
	}
	full, _, err := configuredPaths(conn)
	if err != nil {
		return core.Binding{}, err
	}
	for _, e := range c.Endpoints() {
		if e.Action != action {
			continue
		}
		if (action == "inference" || action == "queue.submit") &&
			len(strings.Split(strings.Trim(e.Path, "/"), "/")) != len(strings.Split(strings.Trim(full, "/"), "/")) {
			continue
		}
		return c.BindEndpoint(ctx, conn, e, map[string]string{"request_id": id})
	}
	return core.Binding{}, failure("unsupported_operation", "An explicit fal endpoint action is required")
}

func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, params map[string]string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	listed := false
	for _, allowed := range c.Endpoints() {
		listed = listed || allowed == e
	}
	if !listed {
		return core.Binding{}, failure("unsupported_operation", "Endpoint is not in the fal inventory")
	}
	full, app, err := configuredPaths(conn)
	if err != nil {
		return core.Binding{}, err
	}
	origin, err := connectionOrigin(conn)
	if err != nil {
		return core.Binding{}, err
	}
	if (e.Action == "inference") != (origin == "fal.run") {
		return core.Binding{}, failure("configuration_stale", "fal action does not match the configured direct or queue origin")
	}
	expected := app
	if e.Action == "inference" || e.Action == "queue.submit" {
		expected = full
	}
	patternForConfig := e.Path
	if suffix := strings.Index(patternForConfig, "/requests/"); suffix >= 0 {
		patternForConfig = patternForConfig[:suffix]
	}
	hasRouteParams := false
	for _, part := range strings.Split(strings.Trim(patternForConfig, "/"), "/") {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") && params[strings.Trim(part, "{}")] != "" {
			hasRouteParams = true
			break
		}
	}
	if hasRouteParams {
		if err := matchConfiguredPattern(patternForConfig, expected, params); err != nil {
			return core.Binding{}, err
		}
	}
	var path string
	if e.Action == "inference" || e.Action == "queue.submit" {
		path = full
	} else {
		suffix := strings.Index(e.Path, "/requests/")
		if suffix < 0 {
			return core.Binding{}, failure("unsupported_operation", "Malformed fal queue lifecycle route")
		}
		path = app + e.Path[suffix:]
		id := params["id"]
		if id == "" {
			id = params["request_id"]
		}
		escaped, err := endpoint.Segment(id)
		if err != nil {
			return core.Binding{}, err
		}
		path = strings.Replace(path, "{id}", escaped, 1)
		path = strings.Replace(path, "{request_id}", escaped, 1)
	}
	query := url.Values{}
	if e.Action == "queue.status" || e.Action == "queue.status-stream" {
		if logs := params["logs"]; logs != "" {
			if logs != "0" && logs != "1" {
				return core.Binding{}, failure("unsupported_feature", "fal logs must be 0 or 1")
			}
			query.Set("logs", logs)
		}
	}
	if e.Action == "queue.submit" && params["fal_webhook"] != "" {
		query.Set("fal_webhook", params["fal_webhook"])
	}
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	if err := endpoint.ValidateRelative(path); err != nil {
		return core.Binding{}, err
	}
	policy := responsePolicy(e.Action)
	if e.Action == "queue.submit" {
		policy.PollEndpoint = app + "/requests/{id}/status"
	}
	return core.Binding{
		Codec:    core.CodecKey{Protocol: "native", Variant: "fal", Operation: "native"},
		Endpoint: path, Method: e.Method, ModelLocation: "none", Framing: e.Framing,
		ReplaySafe:            e.Method == http.MethodGet,
		CancellationSupported: e.Action == "queue.cancel",
		Response:              policy,
	}, nil
}

// Queue envelopes are documented at
// https://fal.ai/docs/documentation/model-apis/inference/queue.
// Results and direct inference are model-specific, not queue envelopes.
func responsePolicy(action string) core.NativeResponsePolicy {
	switch action {
	case "queue.submit":
		return core.NativeResponsePolicy{
			IDField: "request_id", StatusField: "status",
			ResultAction: "queue.result", CancelAction: "queue.cancel",
			PollAction: "queue.status", PollOperation: "native",
			PollEndpoint: "{owner}/{app}/requests/{id}/status", PollMethod: http.MethodGet,
			Async:               true,
			ContinuationFields:  []string{"status_url", "response_url", "cancel_url"},
			ContinuationMethods: map[string]string{"status_url": http.MethodGet, "response_url": http.MethodGet, "cancel_url": http.MethodPut},
			ContinuationActions: map[string]string{"status_url": "queue.status", "response_url": "queue.result", "cancel_url": "queue.cancel"},
			ContinuationOrigins: []string{"https://queue.fal.run"},
			TerminalStatuses:    []string{"COMPLETED", "FAILED", "CANCELLED"},
			FailureStatuses:     []string{"FAILED", "CANCELLED"},
		}
	case "queue.status":
		return core.NativeResponsePolicy{
			IDField: "request_id", StatusField: "status",
			ContinuationFields:  []string{"response_url"},
			ContinuationMethods: map[string]string{"response_url": http.MethodGet},
			ContinuationActions: map[string]string{"response_url": "queue.result"},
			ContinuationOrigins: []string{"https://queue.fal.run"},
			TerminalStatuses:    []string{"COMPLETED", "FAILED", "CANCELLED"},
			FailureStatuses:     []string{"FAILED", "CANCELLED"},
		}
	case "queue.status-stream":
		return core.NativeResponsePolicy{
			IDField: "request_id", StatusField: "status",
			TerminalStatuses: []string{"COMPLETED", "FAILED", "CANCELLED"},
			FailureStatuses:  []string{"FAILED", "CANCELLED"},
		}
	case "queue.cancel":
		return core.NativeResponsePolicy{StatusField: "status"}
	case "queue.result", "inference":
		return core.NativeResponsePolicy{}
	default:
		return core.NativeResponsePolicy{}
	}
}

func matchConfiguredPattern(pattern, expected string, params map[string]string) error {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	expectedParts := strings.Split(strings.Trim(expected, "/"), "/")
	if len(patternParts) != len(expectedParts) {
		return failure("configuration_stale", "fal request does not target the configured endpoint")
	}
	for i, part := range patternParts {
		if !strings.HasPrefix(part, "{") || !strings.HasSuffix(part, "}") {
			if part != expectedParts[i] {
				return failure("configuration_stale", "fal request does not target the configured endpoint")
			}
			continue
		}
		value, err := endpoint.Segment(params[strings.Trim(part, "{}")])
		if err != nil || value != expectedParts[i] {
			return failure("configuration_stale", "fal request does not target the configured endpoint")
		}
	}
	return nil
}

// Inspect checks binding/manifest policy only. Model-specific fields named
// model, tools, stream or response_format are not portable feature switches.
// No native request bytes are decoded, flattened or rewritten here.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	_, err := c.Bind(ctx, target, op)
	return err
}

// Discover never performs implicit network discovery or assumes that a gallery
// listing proves an endpoint's operations/modalities. Operators must supply the
// endpoint and model manifest explicitly. Discovery options remain accepted by
// New for API consistency, but no undocumented model-list endpoint is called.
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, failure("unsupported_operation", "fal requires explicitly configured endpoint manifests; automatic discovery is unavailable")
}

var _ core.Connector = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.ConnectionInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
var _ core.Discoverer = (*Connector)(nil)
