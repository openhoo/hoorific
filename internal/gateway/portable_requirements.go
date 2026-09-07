package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/routing"
)

type portablePayloadKey struct{}

func (g *Gateway) portableRequirements(ctx context.Context, x route, raw []byte, fields map[string]json.RawMessage) (context.Context, routing.Requirements, error) {
	req := routing.Requirements{Operation: x.operation}
	if x.native {
		return ctx, req, nil
	}
	switch x.operation {
	case "image.generate", "image.edit", "image.variation", "audio.speech", "audio.transcribe", "audio.translate":
		if err := mediaScope(x.operation, fields); err != nil {
			return ctx, req, err
		}
	}
	entry, ok := g.deps.Codecs[core.CodecKey{Protocol: x.protocol, Operation: x.operation}]
	if !ok || entry.Request == nil {
		return ctx, req, nil
	}
	payload, err := entry.Request.DecodeRequest(ctx, bytes.NewReader(raw))
	if err != nil {
		return ctx, req, err
	}
	// Gemini selects streaming in the URL action, not a JSON stream member.
	// Carry that transport choice into the translated upstream conversation.
	if strings.Contains(x.path, ":streamGenerateContent") {
		if conversation, ok := payload.(core.Conversation); ok {
			conversation.Stream = true
			payload = conversation
		}
	}
	ctx = context.WithValue(ctx, portablePayloadKey{}, payload)
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
			req.Features = append(req.Features, "streaming")
		}
	case core.EmbeddingRequest:
		req.InputModalities = []string{"text"}
	case core.RerankRequest:
		req.InputModalities = []string{"text"}
	}
	if b := fields["parallel_tool_calls"]; bytes.Equal(bytes.TrimSpace(b), []byte("true")) {
		add(&req.Features, "parallel_tools")
	}
	if strings.Contains(x.path, ":streamGenerateContent") {
		add(&req.Features, "streaming")
	}
	return ctx, req, nil
}

func affinityKey(p core.Principal, alias string) string {
	identity := p.KeyID
	if identity == "" {
		identity = p.SessionID
	}
	return p.TenantID + "\x00" + identity + "\x00" + alias
}
