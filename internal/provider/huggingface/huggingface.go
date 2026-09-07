// Package huggingface binds explicitly configured Hugging Face inference tasks.
// Sources: https://huggingface.co/docs/inference-providers/providers/hf-inference
// https://huggingface.co/docs/inference-endpoints/guides/test_endpoint
// https://github.com/huggingface/huggingface_hub/blob/main/src/huggingface_hub/inference/_providers/hf_inference.py
package huggingface

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

// Connection settings: hf.task selects the inference task; hf.provider selects the
// provider. hf.endpoint, when present, is an exact relative inference path under
// the operator's BaseURL (the empty path is valid for dedicated endpoints).
// Other providers require this explicit endpoint. Conversational endpoints must
// speak OpenAI chat completions; other tasks retain their native payloads.
// Model IDs are never interpreted as URLs.
type Connector struct {
	*endpoint.Connector
	routes []endpoint.Route
}

type task struct {
	name      string
	operation core.Operation
}

var tasks = []task{
	{"automatic-speech-recognition", "generate"}, {"audio-classification", "generate"},
	{"feature-extraction", "embed"}, {"sentence-similarity", "generate"},
	{"fill-mask", "generate"}, {"image-classification", "generate"},
	{"image-segmentation", "generate"}, {"object-detection", "generate"},
	{"question-answering", "generate"}, {"summarization", "generate"},
	{"table-question-answering", "generate"}, {"text-classification", "generate"},
	{"text-generation", "generate"}, {"text-to-image", "generate"},
	{"text-to-speech", "generate"}, {"token-classification", "generate"},
	{"translation", "generate"}, {"zero-shot-classification", "generate"},
	{"conversational", "generate"}, {"text-ranking", "rerank"},
	{"custom", "generate"},
}

func New(opts ...endpoint.Option) *Connector {
	routes := make([]endpoint.Route, 0, len(tasks))
	for _, t := range tasks {
		path := "hf-inference/models/{owner}/{model}"
		if t.name == "feature-extraction" || t.name == "sentence-similarity" {
			path += "/pipeline/" + t.name
		}
		if t.name == "conversational" {
			path += "/v1/chat/completions"
		}
		r := endpoint.E("POST", path, "inference."+t.name, t.operation, "huggingface", "raw", "path", false)
		r.Variant = "native:" + t.name
		if t.name == "conversational" {
			r.Protocol = "openai-chat"
			r.Variant = ""
			r.Framing = "json"
			r.ModelLocation = "body"
		}
		routes = append(routes, r)
	}
	return &Connector{Connector: endpoint.New("huggingface", "https://router.huggingface.co", routes, opts...), routes: routes}
}

func (c *Connector) EndpointsFor(conn core.Connection) ([]core.NativeEndpoint, error) {
	task := conn.Settings["hf.task"]
	if task == "" { return nil, invalid("hf.task must explicitly select the inference task") }
	for _, route := range c.routes {
		if route.Action == "inference."+task {
			return []core.NativeEndpoint{route.NativeEndpoint}, nil
		}
	}
	return nil, invalid("Configured inference task is not supported")
}

func (c *Connector) DescriptorFor(conn core.Connection) (core.ConnectorDescriptor, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil { return core.ConnectorDescriptor{}, err }
	route, err := c.selected(conn, endpoints[0].Operation)
	if err != nil { return core.ConnectorDescriptor{}, err }
	return core.ConnectorDescriptor{ID:"huggingface", Protocols:[]core.Protocol{route.Protocol}, Operations:[]core.Operation{route.Operation}}, nil
}

func invalid(message string) error {
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: message, Origin: "gateway"}
}

func (c *Connector) selected(conn core.Connection, op core.Operation) (endpoint.Route, error) {
	name := conn.Settings["hf.task"]
	if name == "" {
		return endpoint.Route{}, invalid("hf.task must explicitly select the inference task")
	}
	for _, r := range c.routes {
		if r.Action == "inference."+name && r.Operation == op {
			return r, nil
		}
	}
	return endpoint.Route{}, invalid("Configured task does not support this operation")
}

func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	t, ok := target.(core.ModelCall)
	if !ok {
		return core.Binding{}, invalid("Hugging Face inference requires a model target")
	}
	approved := false
	for _, operation := range t.Model.Operations {
		if operation == op {
			approved = true
		}
	}
	if !approved {
		return core.Binding{}, invalid("Model manifest does not approve this operation")
	}
	r, err := c.selected(t.Connection, op)
	if err != nil {
		return core.Binding{}, err
	}
	return c.BindEndpoint(ctx, t.Connection, r.NativeEndpoint, map[string]string{"model": t.Model.ID})
}

// BindStream selects the OpenAI chat wire framing only for a streaming
// conversational request. The inventory remains the task endpoint;
// nonconversational tasks and non-streaming calls retain their native framing.
func (c *Connector) BindStream(ctx context.Context, target core.Target, op core.Operation, stream bool) (core.Binding, error) {
	binding, err := c.Bind(ctx, target, op)
	if err != nil || !stream {
		return binding, err
	}
	t, ok := target.(core.ModelCall)
	if !ok || t.Connection.Settings["hf.task"] != "conversational" {
		return binding, nil
	}
	binding.Framing = "sse-data"
	return binding, nil
}

func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, params map[string]string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	r, err := c.selected(conn, e.Operation)
	if err != nil {
		return core.Binding{}, err
	}
	if e != r.NativeEndpoint {
		return core.Binding{}, invalid("Endpoint does not match the configured task")
	}
	provider := conn.Settings["hf.provider"]
	if provider == "" {
		return core.Binding{}, invalid("hf.provider must be explicitly configured")
	}
	explicit, configured := conn.Settings["hf.endpoint"]
	if conn.Dedicated || provider != "hf-inference" || configured {
		if conn.BaseURL == "" || !configured {
			return core.Binding{}, invalid("Dedicated and provider-specific inference require BaseURL and hf.endpoint")
		}
		u, err := url.Parse(conn.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return core.Binding{}, invalid("Inference BaseURL must be an HTTPS endpoint without credentials, query or fragment")
		}
		if err := endpoint.ValidateRelative(explicit); err != nil {
			return core.Binding{}, err
		}
		if strings.ContainsAny(explicit, "?{}") {
			return core.Binding{}, invalid("hf.endpoint must be a concrete path without query parameters")
		}
		location := "none"
		if r.ModelLocation == "body" {
			location = "body"
		}
		return core.Binding{Codec: core.CodecKey{Protocol: r.Protocol, Variant: r.Variant, Operation: e.Operation}, Endpoint: explicit, Method: e.Method, ModelLocation: location, Framing: e.Framing}, nil
	}
	if conn.Settings["hf.task"] == "custom" {
		return core.Binding{}, invalid("Custom handlers require an explicitly configured endpoint")
	}
	// HF Hub identifiers may be unnamespaced or owner/model; validate each segment.
	parts := strings.Split(params["model"], "/")
	if len(parts) < 1 || len(parts) > 2 {
		return core.Binding{}, invalid("Invalid Hugging Face model identifier")
	}
	for i, part := range parts {
		escaped, err := endpoint.Segment(part)
		if err != nil {
			return core.Binding{}, err
		}
		parts[i] = escaped
	}
	path := "hf-inference/models/" + strings.Join(parts, "/")
	switch conn.Settings["hf.task"] {
	case "feature-extraction", "sentence-similarity":
		path += "/pipeline/" + conn.Settings["hf.task"]
	case "conversational":
		path += "/v1/chat/completions"
	}
	return core.Binding{Codec: core.CodecKey{Protocol: r.Protocol, Variant: r.Variant, Operation: e.Operation}, Endpoint: path, Method: e.Method, ModelLocation: r.ModelLocation, Framing: e.Framing}, nil
}

// Inspect checks routing/manifest boundaries, not a fabricated common schema.
// Native binary audio/image inputs, arrays, custom handler fields, and task
// parameters remain byte-for-byte native; provider schema validation is upstream.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	_, err := c.Bind(ctx, target, op)
	return err
}

// Discover deliberately does not query a generic model catalog on the router:
// catalog membership does not establish a task/provider/endpoint binding.
// An explicitly configured discovery path and injected HTTP client can supply
// candidates through the shared helper; these have no approved operations.
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	if conn.Settings["hf.task"] == "" {
		return nil, fmt.Errorf("huggingface discovery requires configured hf.task")
	}
	provider := conn.Settings["hf.provider"]
	if provider == "" {
		return nil, fmt.Errorf("huggingface discovery requires configured hf.provider")
	}
	if (conn.Dedicated || provider != "hf-inference") && (conn.BaseURL == "" || conn.Settings["hf.endpoint"] == "") {
		return nil, fmt.Errorf("huggingface discovery requires configured endpoint for dedicated/provider-specific inference")
	}
	return c.Connector.Discover(ctx, conn)
}

var _ core.Connector = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.StreamBinder = (*Connector)(nil)
var _ core.ConnectionInventory = (*Connector)(nil)
var _ core.ConnectionDescriptor = (*Connector)(nil)
