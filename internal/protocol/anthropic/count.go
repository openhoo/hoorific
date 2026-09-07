package anthropic

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"io"
)

type CountTokensCodec struct{}

func NewCountTokens() *CountTokensCodec { return &CountTokensCodec{} }

var _ core.RequestCodec = (*CountTokensCodec)(nil)
var _ core.ResultCodec = (*CountTokensCodec)(nil)

func (CountTokensCodec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var a countRequestWire
	if err := readJSON(r, &a); err != nil {
		return nil, err
	}
	if a.MaxTokens != nil {
		return nil, unsupported("max_tokens")
	}
	if a.Stream {
		return nil, unsupported("stream")
	}
	if len(a.Stop) > 0 {
		return nil, unsupported("stop_sequences")
	}
	x := requestWire{Model: a.Model, Messages: a.Messages, System: a.System, Tools: a.Tools}
	one := int64(1)
	x.MaxTokens = &one
	p, e := conversationFromWire(x)
	if e != nil {
		return nil, e
	}
	c := p.(core.Conversation)
	c.MaxOutputTokens = nil
	return core.CountTokensRequest{Conversation: c}, nil
}
func (CountTokensCodec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q, ok := p.(core.CountTokensRequest)
	if !ok {
		return unsupported("request")
	}
	c := q.Conversation
	if c.MaxOutputTokens != nil {
		return unsupported("max_output_tokens")
	}
	if c.Stream {
		return unsupported("stream")
	}
	if len(c.Stop) > 0 {
		return unsupported("stop_sequences")
	}
	if c.StructuredOutput != nil || c.StructuredOutputName != "" || c.StructuredOutputDescription != "" || c.StructuredOutputMode != "" || c.StructuredOutputStrict != nil {
		return unsupported("structured_output")
	}
	one := int64(1)
	c.MaxOutputTokens = &one
	a, e := conversationToWire(c)
	if e != nil {
		return e
	}
	b := countRequestWire{Model: a.Model, Messages: a.Messages, System: a.System, Tools: a.Tools}
	return emitJSON(w, b)
}
func (CountTokensCodec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var a countResultWire
	if err := readJSON(r, &a); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := strict(rawObject(a), &m); err != nil {
		return nil, err
	}
	if _, ok := m["input_tokens"]; !ok {
		return nil, unsupported("input_tokens")
	}
	if a.Input != nil && *a.Input < 0 {
		return nil, unsupported("input_tokens")
	}
	return core.CountTokensResult{InputTokens: a.Input, Source: "anthropic"}, nil
}
func (CountTokensCodec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q, ok := p.(core.CountTokensResult)
	if !ok {
		return unsupported("result")
	}
	if q.InputTokens != nil && *q.InputTokens < 0 {
		return unsupported("input_tokens")
	}
	return emitJSON(w, countResultWire{Input: q.InputTokens})
}

type countRequestWire struct {
	Model     string          `json:"model"`
	MaxTokens *int64          `json:"max_tokens,omitempty"`
	Messages  []messageWire   `json:"messages"`
	System    json.RawMessage `json:"system,omitempty"`
	Tools     []toolWire      `json:"tools,omitempty"`
	Stop      []string        `json:"stop_sequences,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
}
type countResultWire struct {
	Input *int64 `json:"input_tokens"`
}

func rawObject(a countResultWire) []byte { b, _ := json.Marshal(a); return b }
