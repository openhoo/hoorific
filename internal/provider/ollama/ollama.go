// Package ollama inventories Ollama's native API and its documented OpenAI
// compatibility surface. Management endpoints (create, copy, pull, push,
// delete, ps, version) are intentionally absent.
// Sources: https://docs.ollama.com/api/introduction,
// https://docs.ollama.com/api/chat, https://docs.ollama.com/api/generate,
// https://docs.ollama.com/api/embed, https://docs.ollama.com/api/tags,
// https://docs.ollama.com/api-reference/show-model-details and
// https://docs.ollama.com/api/openai-compatibility.
package ollama

import (
	"context"
	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

const DefaultBaseURL = "http://localhost:11434"

type Connector struct{ *endpoint.Connector }

func native(method, path, action string, op core.Operation, framing core.Framing, location string, stateful bool) endpoint.Route {
	r := endpoint.E(method, path, action, op, "ollama", framing, location, stateful)
	r.Variant = ""
	return r
}
func openai(method, path, action string, op core.Operation, framing core.Framing, location string) endpoint.Route {
	r := endpoint.E(method, path, action, op, "openai-chat", framing, location, false)
	r.Variant = ""
	if path == "v1/completions" {
		r.Protocol = "openai-completion"
	}
	if path == "v1/responses" {
		r.Protocol = "openai-responses"
	}
	return r
}

func routes() []endpoint.Route {
	routes := []endpoint.Route{
		native("POST", "api/chat", "chat", "generate", "ndjson", "body", false),
		native("POST", "api/generate", "generate", "complete", "ndjson", "body", false),
		native("POST", "api/embed", "embed", "embed", "json", "body", false),
		native("GET", "api/tags", "tags", "model.list", "json", "none", false),
		native("POST", "api/show", "show", "model.resource", "json", "body", false),
		openai("POST", "v1/chat/completions", "chat.completions", "generate", "sse-data", "body"),
		openai("POST", "v1/completions", "completions", "complete", "sse-data", "body"),
		openai("POST", "v1/embeddings", "embeddings", "embed", "json", "body"),
		openai("POST", "v1/responses", "responses.create", "generate", "sse-data", "body"),
		openai("GET", "v1/models", "models.list", "model.list", "json", "none"),
		openai("GET", "v1/models/{model}", "models.retrieve", "model.list", "json", "none"),
	}
	routes[10].ResourceIDField = "model"
	return routes
}

func New(opts ...endpoint.Option) *Connector {
	return NewWithBase(DefaultBaseURL, opts...)
}
func NewWithBase(base string, opts ...endpoint.Option) *Connector {
	all := append([]endpoint.Option{endpoint.WithDiscoveryPath("api/tags")}, opts...)
	return &Connector{Connector: endpoint.New("ollama", base, routes(), all...)}
}

func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	// Native and OpenAI-compatible requests use different codecs, but the
	// endpoint inventory remains the source of truth for operation approval.
	return c.Connector.Inspect(ctx, target, op, body)
}

func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	conn := core.Connection{}
	params := map[string]string{}
	action := ""
	switch t := target.(type) {
	case core.ModelCall:
		conn, params["model"] = t.Connection, t.Model.ID
		approved := false
		for _, candidate := range t.Model.Operations {
			if candidate == op {
				approved = true
				break
			}
		}
		if !approved {
			return core.Binding{}, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "Model manifest does not approve this operation", Origin: "gateway"}
		}
	case core.ConnectionResourceCall:
		conn, action = t.Connection, t.Action
		params["id"], params["resource_id"] = t.ResourceID, t.ResourceID
	default:
		return core.Binding{}, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "Unknown target", Origin: "gateway"}
	}
	wire := conn.Settings["wire_protocol"]
	if wire == "" {
		wire = conn.Settings["protocol"]
	}
	if wire == "openai" || wire == "openai-compatible" {
		wire = "openai-chat"
	}
	for _, r := range routes() {
		if r.Operation != op || (action != "" && action != r.Action) {
			continue
		}
		if wire != "" && wire != string(r.Protocol) {
			continue
		}
		return c.BindEndpoint(ctx, conn, r.NativeEndpoint, params)
	}
	return core.Binding{}, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "Endpoint action is not supported", Origin: "gateway"}
}

var _ core.EndpointInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
