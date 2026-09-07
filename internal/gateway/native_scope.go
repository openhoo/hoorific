package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/routing"
)

// nativeScopeQueryKey carries the original parsed query, including repeated
// values. A flattened model string cannot prove absence of query overrides.
type nativeScopeQueryKey struct{}

// constrainedNative validates without translating or replacing the native body.
// Only exact portable wire contracts classify all semantic fields; a registered
// opaque/native codec or a provider wrapper is not sufficient evidence.
func (g *Gateway) constrainedNative(ctx context.Context, s core.RuntimeSnapshot, p core.Principal, x route, c core.Connection, connector core.Connector, ep core.NativeEndpoint, params map[string]string, raw []byte, fields map[string]json.RawMessage) (selected, error) {
	deny := func() (selected, error) {
		return selected{}, failure("forbidden", http.StatusForbidden, "native request requires an explicit account grant")
	}
	if c.ID != x.connection || c.TenantID != p.TenantID || c.Settings["disabled"] == "true" || !contains(p.Connections, c.ID) || !contains(p.Operations, ep.Operation) {
		return deny()
	}
	if ep.Method != http.MethodPost || ep.ResourceIDField != "" {
		return deny()
	}
	switch ep.Operation {
	case "generate", "complete", "embed", "rerank", "count_tokens":
	default:
		return deny()
	}
	if _, ok := ctx.Value(requestBodyKey{}).(*capturedBody); ok {
		return deny()
	}
	binder, ok := connector.(core.EndpointBinder)
	if !ok {
		return deny()
	}
	binding, err := binder.BindEndpoint(ctx, c, ep, params)
	if err != nil {
		return selected{}, failure("unsupported_operation", 400, "native endpoint cannot be validated")
	}
	if binding.Method != ep.Method || binding.ModelLocation != ep.ModelLocation || binding.Codec.Operation != ep.Operation || binding.Realtime != nil || binding.Response.Async || len(binding.Response.ContinuationFields) > 0 || len(binding.Response.ContinuationHeaders) > 0 {
		return deny()
	}
	switch binding.Framing {
	case "json", "sse-data", "sse-named", "ndjson", "aws-eventstream":
	default:
		return deny()
	}
	switch binding.Codec.Protocol {
	case "openai-chat", "openai-responses", "openai-completion", "anthropic-messages", "gemini-content", "bedrock-converse", "cohere-v2", "ollama":
	default:
		return deny()
	}
	if ep.Stateful && !(binding.Codec.Protocol == "openai-responses" && ep.Operation == "generate" && ep.ModelLocation == "body") {
		return deny()
	}
	entry, ok := g.deps.Codecs[binding.Codec]
	if !ok || entry.Request == nil {
		return deny()
	}
	query, ok := ctx.Value(nativeScopeQueryKey{}).(url.Values)
	if !ok {
		return deny()
	}
	for name, values := range query {
		if len(values) != 1 {
			return deny()
		}
		switch name {
		case "key": // Ingress authentication consumes this; dispatch removes it.
		case "model":
			if ep.ModelLocation != "query" {
				return deny()
			}
		case "alt":
			if binding.Codec.Protocol != "gemini-content" || values[0] != "sse" || binding.Framing != "sse-data" {
				return deny()
			}
		default:
			return deny()
		}
	}
	// Capture already rejects duplicate JSON keys at every depth. Requiring the
	// original object excludes multipart metadata and absent/uninspectable bodies.
	if len(raw) == 0 || fields == nil {
		return deny()
	}
	var bodyModel string
	if value, present := fields["model"]; present {
		if json.Unmarshal(value, &bodyModel) != nil || bodyModel == "" {
			return deny()
		}
	}
	modelID := ""
	switch ep.ModelLocation {
	case "body":
		modelID = bodyModel
		if len(params) != 0 {
			return deny()
		}
	case "path":
		// Connection-owned path variables may be checked exactly; all other
		// variables would let a caller choose a project, deployment, or resource.
		modelID = params["model"]
		if modelID == "" || bodyModel != "" {
			return deny()
		}
		for name, value := range params {
			switch name {
			case "model":
			case "project":
				if c.Project == "" || value != c.Project {
					return deny()
				}
			case "location":
				if c.Region == "" || value != c.Region {
					return deny()
				}
			default:
				return deny()
			}
		}
	case "query":
		modelID = query.Get("model")
		if len(params) != 0 || bodyModel != "" {
			return deny()
		}
	default:
		return deny()
	}
	if modelID == "" || (x.model != "" && x.model != modelID) {
		return deny()
	}
	scope := route{protocol: binding.Codec.Protocol, operation: ep.Operation, path: x.path}
	if err = portableScope(scope, fields); err != nil {
		return deny()
	}
	// References are forbidden even when null: they must not acquire semantics
	// through an upstream default or a future API revision.
	for _, name := range []string{"previous_response_id", "conversation", "file_id", "batch_id"} {
		if _, present := fields[name]; present {
			return deny()
		}
	}
	if value, present := fields["background"]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
		return deny()
	}
	payload, err := entry.Request.DecodeRequest(ctx, bytes.NewReader(raw))
	if err != nil {
		return selected{}, err
	}
	var decodedModel string
	switch v := payload.(type) {
	case core.Conversation:
		decodedModel = v.Model
	case core.CountTokensRequest:
		decodedModel = v.Conversation.Model
	case core.CompletionRequest:
		decodedModel = v.Model
	case core.EmbeddingRequest:
		decodedModel = v.Model
	case core.RerankRequest:
		decodedModel = v.Model
	default:
		return deny()
	}
	if decodedModel != "" && decodedModel != modelID {
		return deny()
	}
	requirements := nativeRequirements(x, payload, fields, ep.Operation, binding.Framing)
	requirements.Features = append(requirements.Features, binding.RequiredFeatures...)
	if binding.Framing != "json" && !contains(requirements.Features, "streaming") {
		requirements.Features = append(requirements.Features, "streaming")
	}
	// Restrict the routing snapshot before invoking shared eligibility rules. A
	// normal routing lottery could otherwise pick another model or conceal this
	// exact model behind an alias with fallback disabled.
	for _, alias := range p.Aliases {
		for _, rt := range s.Aliases[alias] {
			if rt.ConnectionID != c.ID || rt.Weight <= 0 {
				continue
			}
			model, exists := s.Models[rt.ModelID]
			if !exists || model.ID != modelID || model.ConnectionID != c.ID {
				continue
			}
			pinned := s
			pinned.Aliases = map[string][]core.RouteTarget{alias: {rt}}
			if _, err = routing.Order(ctx, pinned, alias, requirements, nil); err != nil {
				if ctx.Err() != nil {
					return selected{}, ctx.Err()
				}
				continue
			}
			target := core.ModelCall{Connection: c, Model: model, Alias: alias}
			return selected{connection: c, model: model, target: target, connector: connector, endpoint: &ep, params: params}, nil
		}
	}
	return selected{}, failure("forbidden", http.StatusForbidden, "native model is not permitted by an eligible model alias")
}

func nativeRequirements(x route, payload core.RequestPayload, fields map[string]json.RawMessage, operation core.Operation, framing core.Framing) routing.Requirements {
	req := routing.Requirements{Operation: operation}
	add := func(xs *[]string, s string) {
		if !contains(*xs, s) {
			*xs = append(*xs, s)
		}
	}
	blocks := func(bs []core.ContentBlock) {
		for _, b := range bs {
			switch b.Kind {
			case "text", "tool_result", "tool_call":
				add(&req.InputModalities, "text")
			case "image", "document", "audio", "video":
				add(&req.InputModalities, b.Kind)
			case "reasoning":
				add(&req.Features, "reasoning_blocks")
			case "citation":
				add(&req.Features, "citations")
			}
		}
	}
	conversation := func(c core.Conversation) {
		if len(c.Tools) > 0 {
			add(&req.Features, "custom_tools")
		}
		if len(c.StructuredOutput) > 0 || c.StructuredOutputMode != "" {
			add(&req.Features, "structured_output")
		}
		if c.Stream {
			add(&req.Features, "streaming")
		}
		req.OutputTokens = c.MaxOutputTokens
		blocks(c.System)
		for _, m := range c.Messages {
			blocks(m.Content)
		}
		add(&req.OutputModalities, "text")
	}
	switch p := payload.(type) {
	case core.Conversation:
		conversation(p)
	case core.CountTokensRequest:
		conversation(p.Conversation)
		req.OutputModalities = nil
	case core.CompletionRequest:
		req.InputModalities = []string{"text"}
		req.OutputModalities = []string{"text"}
		req.OutputTokens = p.MaxOutputTokens
		if p.Stream {
			add(&req.Features, "streaming")
		}
	case core.EmbeddingRequest:
		req.InputModalities = []string{"text"}
	case core.RerankRequest:
		req.InputModalities = []string{"text"}
	}
	if b, ok := fields["parallel_tool_calls"]; ok && bytes.Equal(bytes.TrimSpace(b), []byte("true")) {
		add(&req.Features, "parallel_tools")
	}
	if strings.Contains(x.path, ":streamGenerateContent") || framing != "json" {
		add(&req.Features, "streaming")
	}
	return req
}
