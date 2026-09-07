// Package compatible contains explicit manifests for providers exposing an
// OpenAI-shaped (or explicitly Anthropic-shaped) inference API.
// Provider sources: OpenRouter https://openrouter.ai/docs/api-reference/overview,
// Groq https://console.groq.com/docs/openai, Together
// https://docs.together.ai/docs/inference/openai-compatibility, Fireworks
// https://docs.fireworks.ai/api-reference/post-chatcompletions, DeepSeek
// https://api-docs.deepseek.com/guides/anthropic_api, Mistral
// https://docs.mistral.ai/api, xAI https://docs.x.ai/developers/rest-api-reference,
// and Cerebras https://inference-docs.cerebras.ai/resources/openai.
package compatible

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

type Connector struct {
	*endpoint.Connector
	dialect string
	routes  []endpoint.Route
}

const (
	OpenRouter        = "openrouter"
	Groq              = "groq"
	Together          = "together"
	Fireworks         = "fireworks"
	DeepSeek          = "deepseek"
	DeepSeekAnthropic = "deepseek-anthropic"
	Mistral           = "mistral"
	XAI               = "xai"
	Cerebras          = "cerebras"
)
const (
	OpenRouterBaseURL        = "https://openrouter.ai/api/v1"
	GroqBaseURL              = "https://api.groq.com/openai/v1"
	TogetherBaseURL          = "https://api.together.ai/v1"
	FireworksBaseURL         = "https://api.fireworks.ai/inference/v1"
	DeepSeekBaseURL          = "https://api.deepseek.com/v1"
	DeepSeekAnthropicBaseURL = "https://api.deepseek.com/anthropic"
	MistralBaseURL           = "https://api.mistral.ai/v1"
	XAIBaseURL               = "https://api.x.ai/v1"
	CerebrasBaseURL          = "https://api.cerebras.ai/v1"
)

func New(id, base string, routes []endpoint.Route, opts ...endpoint.Option) *Connector {
	for i := range routes {
		switch {
		case strings.Contains(routes[i].Path, "{id}"):
			routes[i].ResourceIDField = "id"
		case strings.Contains(routes[i].Path, "{model_id}"):
			routes[i].ResourceIDField = "model_id"
		case strings.Contains(routes[i].Path, "{model}"):
			routes[i].ResourceIDField = "model"
		}
	}
	discovery := make([]endpoint.Option, 0, 1+len(opts))
	for _, r := range routes {
		if r.Operation == "model.list" {
			discovery = append(discovery, endpoint.WithDiscoveryPath("models"))
			break
		}
	}
	discovery = append(discovery, opts...)
	return &Connector{Connector: endpoint.New(id, base, routes, discovery...), dialect: id, routes: append([]endpoint.Route(nil), routes...)}
}

func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	if (c.dialect == Groq || c.dialect == Mistral) && op == "complete" {
		return core.Binding{}, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "Legacy completions are unsupported", Origin: "gateway"}
	}
	var conn core.Connection
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
	for _, r := range c.routes {
		if r.Operation != op || (action != "" && action != r.Action) {
			continue
		}
		if wire != "" && wire != string(r.Protocol) {
			continue
		}
		if action == "" && r.Stateful && r.ModelLocation == "none" {
			continue
		}
		return c.BindEndpoint(ctx, conn, r.NativeEndpoint, params)
	}
	return core.Binding{}, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "No endpoint matches the configured protocol and operation", Origin: "gateway"}
}

func route(method, path, action string, op core.Operation, protocol core.Protocol, framing core.Framing, location string, stateful bool, variant string) endpoint.Route {
	r := endpoint.E(method, path, action, op, protocol, framing, location, stateful)
	r.Variant = variant
	return r
}

func openAIRoutes(base string) []endpoint.Route {
	_ = base
	return []endpoint.Route{
		route("POST", "chat/completions", "chat.completions", "generate", "openai-chat", "sse-data", "body", false, ""),
		route("POST", "completions", "completions", "complete", "openai-completion", "sse-data", "body", false, ""),
		route("POST", "embeddings", "embeddings.create", "embed", "openai-chat", "json", "body", false, ""),
		route("GET", "models", "models.list", "model.list", "openai-chat", "json", "none", false, ""),
		route("GET", "models/{model}", "models.retrieve", "model.list", "openai-chat", "json", "none", false, ""),
	}
}

func NewPreset(name string, opts ...endpoint.Option) (*Connector, error) {
	var id, base string
	var routes []endpoint.Route
	switch strings.ToLower(name) {
	case OpenRouter:
		id, base, routes = OpenRouter, OpenRouterBaseURL, openAIRoutes("")
	case Groq:
		id, base, routes = Groq, GroqBaseURL, openAIRoutes("")
	case Together:
		id, base, routes = Together, TogetherBaseURL, openAIRoutes("")
	case Fireworks:
		id, base, routes = Fireworks, FireworksBaseURL, []endpoint.Route{
			route("POST", "chat/completions", "chat.completions", "generate", "openai-chat", "sse-data", "body", false, ""),
			route("POST", "completions", "completions", "complete", "openai-completion", "sse-data", "body", false, ""),
			route("POST", "responses", "responses.create", "generate", "openai-responses", "sse-data", "body", false, ""),
			route("POST", "embeddings", "embeddings.create", "embed", "openai-chat", "json", "body", false, ""),
			route("POST", "rerank", "rerank.create", "rerank", "fireworks", "json", "body", false, ""),
			route("GET", "models", "models.list", "model.list", "openai-chat", "json", "none", false, ""),
			route("GET", "models/{model}", "models.retrieve", "model.list", "openai-chat", "json", "none", false, ""),
		}
	case DeepSeek:
		id, base, routes = DeepSeek, DeepSeekBaseURL, openAIRoutes("")
	case DeepSeekAnthropic:
		id, base, routes = DeepSeekAnthropic, DeepSeekAnthropicBaseURL, []endpoint.Route{
			route("POST", "messages", "messages.create", "generate", "anthropic-messages", "sse-data", "body", false, ""),
			route("POST", "messages/count_tokens", "messages.count_tokens", "count_tokens", "anthropic-messages", "json", "body", false, "count_tokens"),
		}
	case Mistral:
		id, base, routes = Mistral, MistralBaseURL, openAIRoutes("")
	case XAI:
		id, base, routes = XAI, XAIBaseURL, openAIRoutes("")
	case Cerebras:
		id, base, routes = Cerebras, CerebrasBaseURL, []endpoint.Route{
			route("POST", "chat/completions", "chat.completions", "generate", "openai-chat", "sse-data", "body", false, ""),
			route("GET", "models", "models.list", "model.list", "openai-chat", "json", "none", false, ""),
			route("GET", "models/{model}", "models.retrieve", "model.list", "openai-chat", "json", "none", false, ""),
		}
	default:

		return nil, fmt.Errorf("unknown compatible provider %q", name)
	}
	return New(id, base, routes, opts...), nil
}

func OpenRouterConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(OpenRouter, opts...)
	return c
}
func GroqConnector(opts ...endpoint.Option) *Connector { c, _ := NewPreset(Groq, opts...); return c }
func TogetherConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(Together, opts...)
	return c
}
func FireworksConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(Fireworks, opts...)
	return c
}
func DeepSeekConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(DeepSeek, opts...)
	return c
}
func DeepSeekAnthropicConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(DeepSeekAnthropic, opts...)
	return c
}
func MistralConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(Mistral, opts...)
	return c
}
func XAIConnector(opts ...endpoint.Option) *Connector { c, _ := NewPreset(XAI, opts...); return c }
func CerebrasConnector(opts ...endpoint.Option) *Connector {
	c, _ := NewPreset(Cerebras, opts...)
	return c
}

func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	if c.dialect == Groq && op == "complete" || c.dialect == Mistral && op == "complete" {
		return core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "legacy text completions are not supported by this provider", Origin: "gateway"}
	}
	if err := c.Connector.Inspect(ctx, target, op, body); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	switch c.dialect {
	case Cerebras:
		if raw, ok := fields["n"]; ok {
			var n int
			if json.Unmarshal(raw, &n) == nil && n > 1 {
				return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Message: "Cerebras supports only n=1", Param: "n", Origin: "gateway"}
			}
		}
		if hasExternalImage(fields) {
			return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Message: "Cerebras accepts only base64 image data URIs", Param: "image_url", Origin: "gateway"}
		}
	case DeepSeek, DeepSeekAnthropic:
		if containsThinkingReplay(fields) {
			return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Message: "DeepSeek thinking replay is not supported by this connector", Param: "reasoning_content", Origin: "gateway"}
		}
	}
	return nil
}
func NewOpenRouter(opts ...endpoint.Option) *Connector { return OpenRouterConnector(opts...) }
func NewGroq(opts ...endpoint.Option) *Connector       { return GroqConnector(opts...) }
func NewTogether(opts ...endpoint.Option) *Connector   { return TogetherConnector(opts...) }
func NewFireworks(opts ...endpoint.Option) *Connector  { return FireworksConnector(opts...) }
func NewDeepSeek(opts ...endpoint.Option) *Connector   { return DeepSeekConnector(opts...) }
func NewDeepSeekAnthropic(opts ...endpoint.Option) *Connector {
	return DeepSeekAnthropicConnector(opts...)
}
func NewMistral(opts ...endpoint.Option) *Connector  { return MistralConnector(opts...) }
func NewXAI(opts ...endpoint.Option) *Connector      { return XAIConnector(opts...) }
func NewCerebras(opts ...endpoint.Option) *Connector { return CerebrasConnector(opts...) }

// Preset is an alias for NewPreset for callers that model connectors as a
// named catalog rather than selecting a constructor directly.
func Preset(name string, opts ...endpoint.Option) (*Connector, error) {
	return NewPreset(name, opts...)
}

func hasExternalImage(fields map[string]json.RawMessage) bool {
	var scan func(any, bool) bool
	scan = func(v any, imageContext bool) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				nextImage := imageContext || k == "image_url" || k == "image"
				if nextImage && k == "url" {
					if s, ok := child.(string); ok && strings.HasPrefix(s, "http") {
						return true
					}
				}
				if scan(child, nextImage) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if scan(child, imageContext) {
					return true
				}
			}
		}
		return false
	}
	var v any
	for _, raw := range fields {
		if json.Unmarshal(raw, &v) == nil && scan(v, false) {
			return true
		}
	}
	return false
}

func containsThinkingReplay(fields map[string]json.RawMessage) bool {
	for key, raw := range fields {
		if key == "reasoning_content" {
			return true
		}
		var v any
		if json.Unmarshal(raw, &v) == nil {
			b, _ := json.Marshal(v)
			if bytes.Contains(b, []byte(`"reasoning_content"`)) {
				return true
			}
		}
	}
	return false
}
