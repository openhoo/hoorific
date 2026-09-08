// Package azureopenai provides explicit Azure OpenAI data-plane bindings.
package azureopenai

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
	legacy     bool
	id         string
	client     *http.Client
	credential core.CredentialLease
}

func New(opts ...Option) *Connector {
	c := &Connector{id: "azure-openai", client: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}
func NewLegacy(opts ...Option) *Connector { c := New(opts...); c.legacy = true; return c }
func (c *Connector) Descriptor() core.ConnectorDescriptor {
	return descriptorForEndpoints(c.id, c.Endpoints())
}

func descriptorForEndpoints(id string, endpoints []core.NativeEndpoint) core.ConnectorDescriptor {
	d := core.ConnectorDescriptor{ID: id}
	if len(endpoints) == 0 {
		return d
	}
	hasCompletion, hasResponses := false, false
	seen := make(map[core.Operation]bool)
	for _, e := range endpoints {
		if !seen[e.Operation] {
			d.Operations = append(d.Operations, e.Operation)
			seen[e.Operation] = true
		}
		hasCompletion = hasCompletion || e.Operation == "complete"
		hasResponses = hasResponses || strings.HasPrefix(e.Action, "responses.") || e.Operation == "conversation.resource"
	}
	d.Protocols = []core.Protocol{"openai-chat"}
	if hasResponses {
		d.Protocols = append(d.Protocols, "openai-responses")
	}
	if hasCompletion {
		d.Protocols = append(d.Protocols, "openai-completion")
	}
	d.Protocols = append(d.Protocols, "native")
	return d
}

func azureResponsePolicy(action string) core.NativeResponsePolicy {
	switch action {
	case "responses.create", "files.create":
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status"}
	case "responses.get":
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status", Async: true}
	default:
		return core.NativeResponsePolicy{}
	}
}

func (c *Connector) DescriptorFor(conn core.Connection) (core.ConnectorDescriptor, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.ConnectorDescriptor{}, err
	}
	return descriptorForEndpoints(c.id, endpoints), nil
}

func (c *Connector) EndpointsFor(conn core.Connection) ([]core.NativeEndpoint, error) {
	configured := *c
	if mode := strings.TrimSpace(conn.Settings["api_mode"]); mode != "" && mode != "legacy" && mode != "modern" {
		return nil, fmt.Errorf("unsupported azure api_mode %q", mode)
	}
	configured.legacy = c.legacy || conn.Settings["api_mode"] == "legacy"
	endpoints := configured.Endpoints()
	available := make([]core.NativeEndpoint, 0, len(endpoints))
	for _, e := range endpoints {
		ready := true
		for _, key := range []string{"deployment", "api_version"} {
			if !strings.Contains(e.Path, "{"+key+"}") {
				continue
			}
			value := conn.Settings[key]
			if strings.TrimSpace(value) == "" {
				ready = false
				continue
			}
			if strings.ContainsAny(value, "/\\?#{}") {
				return nil, fmt.Errorf("invalid azure %s", key)
			}
		}
		if ready {
			available = append(available, e)
		}
	}
	return available, nil
}
func (c *Connector) Endpoints() []core.NativeEndpoint {
	routes := []core.NativeEndpoint{
		{Method: "POST", Path: "/openai/v1/chat/completions", Action: "chat.completions", Operation: "generate", ModelLocation: "body", Framing: "json"},
		{Method: "POST", Path: "/openai/v1/completions", Action: "completions.create", Operation: "complete", ModelLocation: "body", Framing: "json"},
		{Method: "POST", Path: "/openai/v1/responses", Action: "responses.create", Operation: "generate", ModelLocation: "body", Framing: "json", Stateful: true},
		{Method: "POST", Path: "/openai/v1/embeddings", Action: "embeddings.create", Operation: "embed", ModelLocation: "body", Framing: "json"},
		{Method: "POST", Path: "/openai/v1/images/generations", Action: "images.generate", Operation: "image.generate", ModelLocation: "body", Framing: "json"},
		{Method: "POST", Path: "/openai/v1/audio/speech", Action: "audio.speech", Operation: "audio.speech", ModelLocation: "body", Framing: "json"},
		{Method: "POST", Path: "/openai/v1/audio/transcriptions", Action: "audio.transcriptions", Operation: "audio.transcribe", ModelLocation: "body", Framing: "json"},
		{Method: "POST", Path: "/openai/v1/audio/translations", Action: "audio.translations", Operation: "audio.translate", ModelLocation: "body", Framing: "json"},
		{Method: "GET", Path: "/openai/v1/models", Action: "models.list", Operation: "model.list", Framing: "json"},
		{Method: "GET", Path: "/openai/v1/files", Action: "files.list", Operation: "file", Framing: "json", Stateful: true},
		{Method: "POST", Path: "/openai/v1/files", Action: "files.create", Operation: "upload", Framing: "json", Stateful: true},
		{Method: "POST", Path: "/openai/v1/batches", Action: "batches.create", Operation: "batch", Framing: "json", Stateful: true},
		{Method: "GET", Path: "/openai/v1/responses/{id}", Action: "responses.get", Operation: "response.resource", Framing: "json", Stateful: true, ResourceIDField: "id"},
		{Method: "GET", Path: "/openai/v1/conversations/{id}", Action: "conversations.get", Operation: "conversation.resource", Framing: "json", Stateful: true, ResourceIDField: "id"},
	}
	if !c.legacy {
		return routes
	}
	legacy := make([]core.NativeEndpoint, 0, len(routes))
	for _, r := range routes {
		if r.Action == "responses.create" || r.Action == "responses.get" || r.Action == "conversations.get" || r.Action == "files.list" || r.Action == "files.create" || r.Action == "batches.create" {
			continue
		}
		if r.Action == "chat.completions" || r.Action == "completions.create" || r.Action == "embeddings.create" || r.Action == "images.generate" || r.Action == "audio.speech" || r.Action == "audio.transcriptions" || r.Action == "audio.translations" {
			r.Path = strings.Replace(r.Path, "/openai/v1/", "/openai/deployments/{deployment}/", 1)
		} else {
			r.Path = strings.Replace(r.Path, "/openai/v1/", "/openai/", 1)
		}
		r.Path += "?api-version={api_version}"
		legacy = append(legacy, r)
	}
	return legacy
}
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	var conn core.Connection
	var action, resourceID, model string
	switch call := target.(type) {
	case core.ModelCall:
		conn, model = call.Connection, call.Model.ID
		if strings.TrimSpace(model) == "" {
			return core.Binding{}, fmt.Errorf("azure model is required")
		}
		switch op {
		case "generate":
			action = "chat.completions"
		case "complete":
			action = "completions.create"
		case "embed":
			action = "embeddings.create"
		case "image.generate":
			action = "images.generate"
		case "audio.speech":
			action = "audio.speech"
		case "audio.transcribe":
			action = "audio.transcriptions"
		case "audio.translate":
			action = "audio.translations"
		default:
			return core.Binding{}, fmt.Errorf("azure operation %q requires a resource call", op)
		}
	case core.ConnectionResourceCall:
		conn, action, resourceID = call.Connection, call.Action, call.ResourceID
		if action == "" {
			return core.Binding{}, fmt.Errorf("azure resource action is required")
		}
	default:
		return core.Binding{}, fmt.Errorf("unsupported azure target for operation %q", op)
	}
	if strings.TrimSpace(conn.BaseURL) == "" {
		return core.Binding{}, fmt.Errorf("azure base URL is required")
	}
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.Binding{}, err
	}
	for _, e := range endpoints {
		if e.Operation != op || e.Action != action {
			continue
		}
		if resourceID != "" && e.ResourceIDField == "" {
			return core.Binding{}, fmt.Errorf("azure %s does not accept a resource ID", action)
		}
		vars := map[string]string{"model": model}
		if e.ResourceIDField != "" {
			if strings.TrimSpace(resourceID) == "" {
				return core.Binding{}, fmt.Errorf("azure %s requires a resource ID", action)
			}
			vars[e.ResourceIDField] = resourceID
		}
		return c.BindEndpoint(ctx, conn, e, vars)
	}
	return core.Binding{}, fmt.Errorf("unsupported azure operation %q action %q for connection", op, action)
}
func (c *Connector) BindEndpoint(_ context.Context, conn core.Connection, e core.NativeEndpoint, vars map[string]string) (core.Binding, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.Binding{}, err
	}
	found := false
	for _, registered := range endpoints {
		if registered.Method == e.Method && registered.Path == e.Path && registered.Action == e.Action && registered.Operation == e.Operation {
			e = registered
			found = true
			break
		}
	}
	if !found {
		return core.Binding{}, fmt.Errorf("azure endpoint %q is unavailable for connection", e.Action)
	}
	if e.ResourceIDField == "" && (strings.TrimSpace(vars["id"]) != "" || strings.TrimSpace(vars["resource_id"]) != "") {
		return core.Binding{}, fmt.Errorf("azure %s does not accept a resource ID", e.Action)
	}
	if e.ResourceIDField != "" && strings.TrimSpace(vars[e.ResourceIDField]) == "" {
		return core.Binding{}, fmt.Errorf("azure %s requires a resource ID", e.Action)
	}
	p := e.Path
	for k, v := range vars {
		if k == "deployment" || k == "api_version" || !strings.Contains(p, "{"+k+"}") {
			continue
		}
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "/\\?#{}") {
			return core.Binding{}, fmt.Errorf("invalid azure path variable %q", k)
		}
		p = strings.ReplaceAll(p, "{"+k+"}", url.PathEscape(v))
	}
	for _, k := range []string{"deployment", "api_version"} {
		if strings.Contains(p, "{"+k+"}") {
			v := conn.Settings[k]
			if v == "" {
				return core.Binding{}, fmt.Errorf("azure %s is required", k)
			}
			if strings.ContainsAny(v, "/\\?#") {
				return core.Binding{}, fmt.Errorf("invalid azure %s", k)
			}
			escaped := url.PathEscape(v)
			if k == "api_version" {
				escaped = url.QueryEscape(v)
			}
			p = strings.ReplaceAll(p, "{"+k+"}", escaped)
		}
	}
	if strings.Contains(p, "{") {
		return core.Binding{}, fmt.Errorf("unbound azure path variable")
	}
	protocol := core.Protocol("openai-chat")
	if e.Operation == "complete" {
		protocol = "openai-completion"
	}
	if e.Action == "responses.create" || e.Operation == "response.resource" || e.Operation == "conversation.resource" {
		protocol = "openai-responses"
	}
	return core.Binding{
		Codec:    core.CodecKey{Protocol: protocol, Variant: "", Operation: e.Operation},
		Endpoint: p, Method: e.Method, ModelLocation: e.ModelLocation, Framing: e.Framing,
		ReplaySafe: e.Method == http.MethodGet, CancellationSupported: true,
		Response: azureResponsePolicy(e.Action),
	}, nil
}
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	binding, e := c.Bind(ctx, core.ConnectionResourceCall{Connection: conn, Action: "models.list"}, "model.list")
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, binding.Method, strings.TrimRight(conn.BaseURL, "/")+binding.Endpoint, nil)
	if e != nil {
		return nil, fmt.Errorf("azure discovery request: %w", e)
	}
	if c.credential != nil {
		if e = c.credential.Authorize(ctx, req); e != nil {
			return nil, e
		}
	}
	resp, e := c.client.Do(req)
	if e != nil {
		return nil, fmt.Errorf("azure discovery failed: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("azure discovery returned status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); e != nil {
		return nil, fmt.Errorf("azure discovery response: %w", e)
	}
	out := make([]core.Model, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, core.Model{ID: m.ID, ConnectionID: conn.ID, Features: map[string]core.Support{}})
		}
	}
	return out, nil
}
func (c *Connector) Inspect(_ context.Context, _ core.Target, _ core.Operation, body []byte) error {
	return rejectForeignCacheDirectives(body)
}

func rejectForeignCacheDirectives(body []byte) error {
	var root map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &root) != nil || root == nil {
		return nil
	}
	if _, ok := root["session_id"]; ok {
		return unsupportedCacheDirective("session_id")
	}
	if _, ok := root["cache_control"]; ok {
		return unsupportedCacheDirective("cache_control")
	}
	for _, key := range []string{"messages", "input"} {
		var items []json.RawMessage
		if json.Unmarshal(root[key], &items) != nil {
			continue
		}
		for _, item := range items {
			var object map[string]json.RawMessage
			if json.Unmarshal(item, &object) != nil {
				continue
			}
			if err := rejectContentCacheDirectives(object["content"]); err != nil {
				return err
			}
		}
	}
	var tools []json.RawMessage
	if json.Unmarshal(root["tools"], &tools) == nil {
		for _, tool := range tools {
			var object map[string]json.RawMessage
			if json.Unmarshal(tool, &object) != nil {
				continue
			}
			if _, ok := object["cache_control"]; ok {
				return unsupportedCacheDirective("cache_control")
			}
		}
	}
	return nil
}

func rejectContentCacheDirectives(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		if _, ok := object["cache_control"]; ok {
			return unsupportedCacheDirective("cache_control")
		}
		return nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	for _, part := range parts {
		if err := rejectContentCacheDirectives(part); err != nil {
			return err
		}
	}
	return nil
}

func unsupportedCacheDirective(param string) error {
	return core.GatewayError{
		Code:       "unsupported_feature",
		HTTPStatus: 400,
		Param:      param,
		Message:    "cache directive is not supported by the direct Azure OpenAI provider: " + param,
		Origin:     "gateway",
	}
}

// BindStream selects the stream-capable binding. Azure uses the same endpoint
// path as unary chat; the request codec controls the stream field.
func (c *Connector) BindStream(ctx context.Context, target core.Target, op core.Operation, _ bool) (core.Binding, error) {
	return c.Bind(ctx, target, op)
}
