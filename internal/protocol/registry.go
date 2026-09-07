// Package protocol contains the wire protocol adapters used by the gateway.
package protocol

import (
	"fmt"

	"hoorific/internal/core"
	"hoorific/internal/protocol/anthropic"
	"hoorific/internal/protocol/bedrockconverse"
	"hoorific/internal/protocol/cohere"
	"hoorific/internal/protocol/embedding"
	"hoorific/internal/protocol/gemini"
	"hoorific/internal/protocol/ollama"
	"hoorific/internal/protocol/openaichat"
	"hoorific/internal/protocol/openaicompletion"
	"hoorific/internal/protocol/openairesponses"
	"hoorific/internal/protocol/rerank"
)

// Entry holds the independent codecs for one exact protocol/variant/operation
// key. Stream is optional for unary operations; request and result are always
// required. No lookup performs protocol or provider fallback.
type Entry struct {
	Request core.RequestCodec
	Result  core.ResultCodec
	Stream  core.StreamCodec
}

type Registry struct{ entries map[core.CodecKey]Entry }

func NewRegistry(entries map[core.CodecKey]Entry) (Registry, error) {
	r := Registry{entries: make(map[core.CodecKey]Entry, len(entries))}
	for k, e := range entries {
		if k.Protocol == "" || k.Operation == "" || k.Variant == "__invalid__" {
			return Registry{}, fmt.Errorf("invalid codec key")
		}
		if e.Request == nil || e.Result == nil {
			return Registry{}, fmt.Errorf("codec %s/%s/%s missing request or result", k.Protocol, k.Variant, k.Operation)
		}
		if _, ok := r.entries[k]; ok {
			return Registry{}, fmt.Errorf("duplicate codec key %s/%s/%s", k.Protocol, k.Variant, k.Operation)
		}
		r.entries[k] = e
	}
	return r, nil
}
func (r Registry) Lookup(k core.CodecKey) (Entry, bool) { e, ok := r.entries[k]; return e, ok }
func (r Registry) Must(k core.CodecKey) Entry {
	e, ok := r.Lookup(k)
	if !ok {
		panic("codec key not registered: " + string(k.Protocol) + "/" + k.Variant + "/" + string(k.Operation))
	}
	return e
}

// Builtins returns all portable protocol entries. Native provider/resource
// bindings are deliberately not registered here: their descriptors select an
// exact key and may use these codecs or an adapter-local codec.
func Builtins() map[core.CodecKey]Entry {
	entries := map[core.CodecKey]Entry{}
	add := func(protocol core.Protocol, variant string, op core.Operation, request core.RequestCodec, result core.ResultCodec, stream core.StreamCodec) {
		entries[core.CodecKey{Protocol: protocol, Variant: variant, Operation: op}] = Entry{request, result, stream}
	}
	add("openai-chat", "", "generate", openaichat.New(), openaichat.New(), openaichat.New())
	add("openai-chat", "kimi-code", "generate", openaichat.New(), openaichat.New(), openaichat.New())
	add("openai-responses", "", "generate", openairesponses.New(), openairesponses.New(), openairesponses.New())
	add("openai-responses", "codex-subscription", "generate", openairesponses.New(), openairesponses.New(), openairesponses.New())
	add("openai-completion", "", "complete", openaicompletion.New(), openaicompletion.New(), openaicompletion.New())
	add("anthropic-messages", "", "generate", anthropic.New(), anthropic.New(), anthropic.New())
	add("anthropic-messages", "kimi-code", "generate", anthropic.New(), anthropic.New(), anthropic.New())
	add("anthropic-messages", "claude-subscription", "generate", anthropic.New(), anthropic.New(), anthropic.New())
	add("anthropic-messages", "", "count_tokens", anthropic.NewCountTokens(), anthropic.NewCountTokens(), nil)
	add("gemini-content", "", "generate", gemini.New(), gemini.New(), gemini.New())
	add("gemini-content", "code-assist", "generate", gemini.New(), gemini.New(), gemini.New())
	add("gemini-content", "antigravity", "generate", gemini.New(), gemini.New(), gemini.New())
	add("gemini-content", "", "count_tokens", gemini.NewCountTokens(), gemini.NewCountTokens(), nil)
	add("bedrock-converse", "", "generate", bedrockconverse.New(), bedrockconverse.New(), bedrockconverse.New())
	add("cohere-v2", "", "generate", cohere.New(), cohere.New(), cohere.New())
	add("ollama", "", "generate", ollama.New(), ollama.New(), ollama.New())
	add("openai-chat", "", "embed", embedding.New(), embedding.New(), nil)
	add("gemini-content", "", "embed", embedding.NewVariant("gemini"), embedding.NewVariant("gemini"), nil)
	add("cohere-v2", "", "embed", embedding.NewVariant("cohere-v2"), embedding.NewVariant("cohere-v2"), nil)
	add("ollama", "", "embed", embedding.NewVariant("ollama"), embedding.NewVariant("ollama"), nil)
	add("cohere-v2", "", "rerank", rerank.NewVariant("cohere-v2"), rerank.NewVariant("cohere-v2"), nil)
	return entries
}
func DefaultRegistry() Registry {
	r, err := NewRegistry(Builtins())
	if err != nil {
		panic(err)
	}
	return r
}
