// Package vertex provides explicit Vertex AI publisher and endpoint bindings.
package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
)

type Option func(*Connector)

func WithHTTPClient(h *http.Client) Option {
	return func(c *Connector) {
		if h != nil {
			c.client = h
		}
	}
}
func WithCredential(l core.CredentialLease) Option { return func(c *Connector) { c.credential = l } }

type Connector struct {
	id         string
	client     *http.Client
	credential core.CredentialLease
}

func New(opts ...Option) *Connector {
	c := &Connector{id: "vertex", client: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}
func (c *Connector) Descriptor() core.ConnectorDescriptor {
	return core.ConnectorDescriptor{ID: c.id, Protocols: []core.Protocol{"gemini-content", "anthropic-messages", "native"}, Operations: []core.Operation{"generate", "embed", "native", "video", "batch", "prediction", "response.resource", "model.list", "count_tokens", "realtime"}}
}
func (c *Connector) Endpoints() []core.NativeEndpoint {
	return []core.NativeEndpoint{
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent", Action: "models.generateContent", Operation: "generate", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:streamGenerateContent?alt=sse", Action: "models.streamGenerateContent", Operation: "generate", ModelLocation: "path", Framing: "sse-data"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:countTokens", Action: "models.countTokens", Operation: "count_tokens", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:predict", Action: "models.embed", Operation: "embed", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/anthropic/models/{model}:rawPredict", Action: "anthropic.rawPredict", Operation: "generate", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/anthropic/models/{model}:streamRawPredict", Action: "anthropic.streamRawPredict", Operation: "generate", ModelLocation: "path", Framing: "sse-named"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:predict", Action: "models.predict", Operation: "prediction", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:rawPredict", Action: "models.rawPredict", Operation: "native", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:streamRawPredict", Action: "models.streamRawPredict", Operation: "native", ModelLocation: "path", Framing: "ndjson"},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:predictLongRunning", Action: "models.predictLongRunning", Operation: "video", ModelLocation: "path", Framing: "json", Stateful: true},
		{Method: "GET", Path: "/v1/projects/{project}/locations/{location}/publishers/google/models", Action: "models.list", Operation: "model.list", Framing: "json"},
		{Method: "GET", Path: "/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent", Action: "live.bidiGenerateContent", Operation: "realtime", ModelLocation: "none", Framing: "websocket", Stateful: true},
		{Method: "POST", Path: "/v1/projects/{project}/locations/{location}/batchPredictionJobs", Action: "batchPredictionJobs.create", Operation: "batch", Framing: "json", Stateful: true},
		{Method: "GET", Path: "/v1/projects/{project}/locations/{location}/operations/{id}", Action: "operations.get", Operation: "response.resource", Framing: "json", Stateful: true, ResourceIDField: "id"},
	}
}
func (c *Connector) EndpointsFor(conn core.Connection) ([]core.NativeEndpoint, error) {
	publisher := conn.Settings["publisher"]
	if publisher == "" {
		publisher = "google"
	}
	if publisher != "google" && publisher != "anthropic" {
		return nil, fmt.Errorf("unsupported configured Vertex publisher %q", publisher)
	}
	var endpoints []core.NativeEndpoint
	for _, e := range c.Endpoints() {
		anthropic := strings.Contains(e.Path, "/publishers/anthropic/")
		if (publisher == "anthropic") != anthropic {
			continue
		}
		if strings.Contains(e.Path, "{project}") && strings.TrimSpace(conn.Project) == "" {
			continue
		}
		if strings.Contains(e.Path, "{location}") && strings.TrimSpace(conn.Region) == "" {
			continue
		}
		endpoints = append(endpoints, e)
	}
	return endpoints, nil
}
func (c *Connector) DescriptorFor(conn core.Connection) (core.ConnectorDescriptor, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.ConnectorDescriptor{}, err
	}
	d := core.ConnectorDescriptor{ID: c.id}
	seen := map[core.Operation]bool{}
	hasGoogle, hasAnthropic, hasNative := false, false, false
	for _, e := range endpoints {
		if !seen[e.Operation] {
			d.Operations = append(d.Operations, e.Operation)
			seen[e.Operation] = true
		}
		if strings.Contains(e.Path, "/publishers/anthropic/") {
			hasAnthropic = true
		} else {
			hasGoogle = true
		}
		hasNative = hasNative || e.Operation == "native"
	}
	if hasGoogle {
		d.Protocols = append(d.Protocols, "gemini-content")
	}
	if hasAnthropic {
		d.Protocols = append(d.Protocols, "anthropic-messages")
	}
	if hasNative {
		d.Protocols = append(d.Protocols, "native")
	}
	return d, nil
}

func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	switch call := target.(type) {
	case core.ConnectionResourceCall:
		endpoints, err := c.EndpointsFor(call.Connection)
		if err != nil {
			return core.Binding{}, err
		}
		switch {
		case op == "batch" && call.Action == "batchPredictionJobs.create",
			op == "model.list" && call.Action == "models.list":
			if call.ResourceID != "" {
				return core.Binding{}, fmt.Errorf("vertex action %q does not accept a resource ID", call.Action)
			}
		case op == "response.resource" && call.Action == "operations.get":
			if call.ResourceID == "" {
				return core.Binding{}, fmt.Errorf("vertex action %q requires a resource ID", call.Action)
			}
		default:
			return core.Binding{}, fmt.Errorf("unsupported vertex resource action %q for operation %q", call.Action, op)
		}
		for _, e := range endpoints {
			if e.Operation == op && e.Action == call.Action {
				return c.BindEndpoint(ctx, call.Connection, e, map[string]string{"id": call.ResourceID})
			}
		}
	case core.ModelCall:
		if op == "native" {
			return core.Binding{}, fmt.Errorf("native operation requires a pinned endpoint action")
		}
		endpoints, err := c.EndpointsFor(call.Connection)
		if err != nil {
			return core.Binding{}, err
		}
		for _, e := range endpoints {
			if e.Operation == op && !strings.Contains(e.Action, "stream") &&
				(e.ModelLocation == "path" || e.Operation == "realtime") {
				return c.BindEndpoint(ctx, call.Connection, e, map[string]string{"model": call.Model.ID})
			}
		}
	}
	return core.Binding{}, fmt.Errorf("unsupported vertex operation %q for target or connection", op)
}
func vertexResponsePolicy(action string) core.NativeResponsePolicy {
	switch action {
	case "models.predictLongRunning":
		// The returned operation name is the documented Vertex polling
		// identity. The operations.get route is the only registered poll
		// operation; no batch polling route is inferred here.
		return core.NativeResponsePolicy{
			IDField: "name", StatusField: "done",
			PollEndpoint: "v1/{operation}", PollMethod: http.MethodGet,
			PollAction: "operations.get", PollOperation: "response.resource",
			Async: true, TerminalStatuses: []string{"true"},
		}
	case "batchPredictionJobs.create":
		// BatchPredictionJob responses expose name/state, but this inventory
		// intentionally has no batch get route, so it cannot claim polling.
		return core.NativeResponsePolicy{IDField: "name", StatusField: "state"}
	case "operations.get":
		return core.NativeResponsePolicy{IDField: "name", StatusField: "done"}
	default:
		return core.NativeResponsePolicy{}
	}
}

func (c *Connector) BindEndpoint(_ context.Context, conn core.Connection, e core.NativeEndpoint, vars map[string]string) (core.Binding, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.Binding{}, err
	}
	registered := false
	for _, candidate := range endpoints {
		if candidate == e {
			registered = true
			break
		}
	}
	if !registered {
		return core.Binding{}, fmt.Errorf("vertex endpoint action %q is not registered for this connection", e.Action)
	}
	if e.ResourceIDField == "" && (strings.TrimSpace(vars["id"]) != "" || strings.TrimSpace(vars["resource_id"]) != "") {
		return core.Binding{}, fmt.Errorf("vertex %s does not accept a resource ID", e.Action)
	}
	p := e.Path
	values := map[string]string{"project": conn.Project, "location": conn.Region}
	for k, v := range vars {
		values[k] = v
	}
	if values["id"] == "" {
		values["id"] = values["resource_id"]
	}
	for _, k := range []string{"project", "location", "model", "id"} {
		if strings.Contains(p, "{"+k+"}") {
			v := values[k]
			if v == "" {
				return core.Binding{}, fmt.Errorf("vertex %s is required", k)
			}
			if strings.ContainsAny(v, "/\\?#") {
				return core.Binding{}, fmt.Errorf("invalid vertex %s", k)
			}
			p = strings.ReplaceAll(p, "{"+k+"}", url.PathEscape(v))
		}
	}
	if strings.Contains(p, "{") {
		return core.Binding{}, fmt.Errorf("unbound vertex path variable")
	}
	protocol := core.Protocol("gemini-content")
	variant := ""
	if strings.Contains(e.Path, "/publishers/anthropic/") {
		protocol = core.Protocol("anthropic-messages")
		variant = "vertex-anthropic"
	}
	realtime := (*core.RealtimePolicy)(nil)
	if e.Framing == "websocket" {
		realtime = &core.RealtimePolicy{Protocol: "gemini", MaxSessionSeconds: 3600, AllowBinary: true, RequirePayloadEnforcement: true}
	}
	return core.Binding{Codec: core.CodecKey{Protocol: protocol, Variant: variant, Operation: e.Operation}, Endpoint: p, Method: e.Method, ModelLocation: e.ModelLocation, Framing: e.Framing, ReplaySafe: false, CancellationSupported: true, Response: vertexResponsePolicy(e.Action), Realtime: realtime}, nil
}
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return nil, err
	}
	if conn.Settings["publisher"] == "anthropic" {
		return nil, fmt.Errorf("Vertex partner Anthropic model discovery is unsupported")
	}
	if conn.BaseURL == "" || conn.Project == "" || conn.Region == "" {
		return nil, fmt.Errorf("vertex base URL, project and region are required")
	}
	var ep core.NativeEndpoint
	for _, e := range endpoints {
		if e.Operation == "model.list" {
			ep = e
			break
		}
	}
	b, e := c.BindEndpoint(ctx, conn, ep, nil)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(conn.BaseURL, "/")+b.Endpoint, nil)
	if e != nil {
		return nil, fmt.Errorf("vertex discovery request: %w", e)
	}
	if c.credential != nil {
		if e = c.credential.Authorize(ctx, req); e != nil {
			return nil, e
		}
	}
	resp, e := c.client.Do(req)
	if e != nil {
		return nil, fmt.Errorf("vertex discovery failed: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vertex discovery returned status %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
		PublisherModels []struct {
			Name string `json:"name"`
		} `json:"publisherModels"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body); e != nil {
		return nil, fmt.Errorf("vertex discovery response: %w", e)
	}
	out := make([]core.Model, 0, len(body.Models)+len(body.PublisherModels))
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, core.Model{ID: name, ConnectionID: conn.ID, Features: map[string]core.Support{}})
	}
	for _, m := range body.Models {
		add(m.Name)
	}
	for _, m := range body.PublisherModels {
		add(m.Name)
	}
	return out, nil
}
func (c *Connector) Inspect(context.Context, core.Target, core.Operation, []byte) error { return nil }
func (c *Connector) BindStream(ctx context.Context, target core.Target, op core.Operation, stream bool) (core.Binding, error) {
	if !stream {
		return c.Bind(ctx, target, op)
	}
	call, ok := target.(core.ModelCall)
	if !ok {
		return core.Binding{}, fmt.Errorf("vertex stream operation requires model call")
	}
	endpoints, err := c.EndpointsFor(call.Connection)
	if err != nil {
		return core.Binding{}, err
	}
	for _, e := range endpoints {
		if e.Operation != op {
			continue
		}
		if (op == "native" && e.Action == "models.streamRawPredict") ||
			(op == "generate" && (e.Action == "models.streamGenerateContent" || e.Action == "anthropic.streamRawPredict")) {
			return c.BindEndpoint(ctx, call.Connection, e, map[string]string{"model": call.Model.ID})
		}
	}
	return core.Binding{}, fmt.Errorf("vertex streaming is unsupported for operation %q", op)
}
