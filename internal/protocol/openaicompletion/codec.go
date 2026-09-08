package openaicompletion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"hoorific/internal/core"
)

type Codec struct{}

func New() *Codec { return &Codec{} }
func unsupported(field string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: "unsupported completion field: " + field, Origin: "gateway"}
}
func malformed() error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Message: "invalid completion payload", Origin: "gateway"}
}

// scan rejects duplicate object members at every nesting level.
func scan(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok {
				return malformed()
			}
			if seen[s] {
				return unsupported(s)
			}
			seen[s] = true
			if e = scan(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := scan(d); e != nil {
				return e
			}
		}
	default:
		return malformed()
	}
	_, e = d.Token()
	return e
}
func decode(r io.Reader, v any) error {
	b, e := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return malformed()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e = scan(d); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return malformed()
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return unsupported("payload")
	}
	return nil
}
func object(r io.Reader) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if e := decode(r, &m); e != nil {
		return nil, e
	}
	if m == nil {
		return nil, malformed()
	}
	return m, nil
}
func fields(m map[string]json.RawMessage, allowed ...string) error {
	a := map[string]bool{}
	for _, s := range allowed {
		a[s] = true
	}
	for s := range m {
		if !a[s] {
			return unsupported(s)
		}
	}
	return nil
}
func value(m map[string]json.RawMessage, k string, v any) error {
	b, ok := m[k]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return malformed()
	}
	return decode(bytes.NewReader(b), v)
}

type usageDetails struct {
	Cached    *int64 `json:"cached_tokens,omitempty"`
	Reasoning *int64 `json:"reasoning_tokens,omitempty"`
}
type wireUsage struct {
	Input         *int64        `json:"prompt_tokens,omitempty"`
	Output        *int64        `json:"completion_tokens,omitempty"`
	Total         *int64        `json:"total_tokens,omitempty"`
	InputDetails  *usageDetails `json:"prompt_tokens_details,omitempty"`
	OutputDetails *usageDetails `json:"completion_tokens_details,omitempty"`
}

func (u *wireUsage) core() (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	for _, n := range []*int64{u.Input, u.Output, u.Total} {
		if n != nil && *n < 0 {
			return nil, malformed()
		}
	}
	var cached, reasoning *int64
	if u.InputDetails != nil {
		cached = u.InputDetails.Cached
	}
	if u.OutputDetails != nil {
		reasoning = u.OutputDetails.Reasoning
	}
	for _, n := range []*int64{cached, reasoning} {
		if n != nil && *n < 0 {
			return nil, malformed()
		}
	}
	if reasoning != nil && u.Output != nil && *reasoning > *u.Output {
		return nil, malformed()
	}
	if u.Input != nil && u.Output != nil && u.Total != nil && *u.Input != *u.Total-*u.Output {
		return nil, malformed()
	}
	return &core.Usage{Input: u.Input, Output: u.Output, Total: u.Total, CachedInput: cached, ReasoningOutput: reasoning, Source: "provider"}, nil
}

func usage(u *core.Usage) (*wireUsage, error) {
	if u == nil {
		return nil, nil
	}
	for _, n := range []*int64{
		u.Input, u.Output, u.Total, u.CachedInput, u.CacheWriteInput,
		u.CacheWrite5mInput, u.CacheWrite1hInput, u.ReasoningOutput, u.ToolInput,
	} {
		if n != nil && *n < 0 {
			return nil, malformed()
		}
	}
	if u.Input != nil && u.Output != nil && u.Total != nil && *u.Input != *u.Total-*u.Output {
		return nil, malformed()
	}
	if u.ReasoningOutput != nil && (u.Output == nil || *u.ReasoningOutput > *u.Output) {
		return nil, malformed()
	}
	out := &wireUsage{Input: u.Input, Output: u.Output, Total: u.Total}
	if u.CachedInput != nil {
		out.InputDetails = &usageDetails{Cached: u.CachedInput}
	}
	if u.ReasoningOutput != nil {
		out.OutputDetails = &usageDetails{Reasoning: u.ReasoningOutput}
	}
	if _, e := out.core(); e != nil {
		return nil, e
	}
	return out, nil
}
func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	m, e := object(r)
	if e != nil {
		return nil, e
	}
	if e = fields(m, "model", "prompt", "max_tokens", "stream", "n", "best_of", "logprobs", "echo"); e != nil {
		return nil, e
	}
	var q core.CompletionRequest
	var prompt json.RawMessage
	if e = value(m, "prompt", &prompt); e != nil {
		return nil, e
	}
	trimmed := bytes.TrimSpace(prompt)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return nil, unsupported("prompt")
	}
	if e = json.Unmarshal(trimmed, &q.Prompt); e != nil {
		return nil, malformed()
	}
	if e = value(m, "model", &q.Model); e != nil {
		return nil, e
	}
	if e = value(m, "max_tokens", &q.MaxOutputTokens); e != nil {
		return nil, e
	}
	if e = value(m, "stream", &q.Stream); e != nil {
		return nil, e
	}
	if _, ok := m["prompt"]; !ok {
		return nil, malformed()
	}
	for _, k := range []string{"n", "best_of"} {
		if _, ok := m[k]; ok {
			var n int
			if e = value(m, k, &n); e != nil {
				return nil, e
			}
			if n != 1 {
				return nil, unsupported(k)
			}
		}
	}
	if b, ok := m["logprobs"]; ok && !bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, unsupported("logprobs")
	}
	if _, ok := m["echo"]; ok {
		var echo bool
		if e = value(m, "echo", &echo); e != nil {
			return nil, e
		}
		if echo {
			return nil, unsupported("echo")
		}
	}
	if q.MaxOutputTokens != nil && *q.MaxOutputTokens < 0 {
		return nil, malformed()
	}
	return q, nil
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	var q core.CompletionRequest
	if w == nil {
		return unsupported("body")
	}
	switch v := p.(type) {
	case core.CompletionRequest:
		q = v
	case *core.CompletionRequest:
		if v == nil {
			return unsupported("payload")
		}
		q = *v
	default:
		return unsupported("payload")
	}
	if q.MaxOutputTokens != nil && *q.MaxOutputTokens < 0 {
		return malformed()
	}
	return json.NewEncoder(w).Encode(struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Max    *int64 `json:"max_tokens,omitempty"`
		Stream bool   `json:"stream,omitempty"`
	}{q.Model, q.Prompt, q.MaxOutputTokens, q.Stream})
}

type choice struct {
	Text     string          `json:"text"`
	Index    int             `json:"index"`
	Logprobs json.RawMessage `json:"logprobs,omitempty"`
	Finish   *string         `json:"finish_reason"`
}
type response struct {
	ID      string     `json:"id"`
	Object  string     `json:"object,omitempty"`
	Created *int64     `json:"created,omitempty"`
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *wireUsage `json:"usage,omitempty"`
}

func validChoice(ch choice) error {
	if ch.Index != 0 {
		return unsupported("choices.index")
	}
	if len(ch.Logprobs) > 0 && !bytes.Equal(bytes.TrimSpace(ch.Logprobs), []byte("null")) {
		return unsupported("logprobs")
	}
	return nil
}
func finishFromWire(reason string) (core.Finish, error) {
	switch reason {
	case "stop":
		return core.Finish{Status: "completed", Reason: "stop"}, nil
	case "length":
		return core.Finish{Status: "incomplete", Reason: "length"}, nil
	case "tool_calls", "content_filter":
		return core.Finish{Status: "completed", Reason: reason}, nil
	case "error":
		return core.Finish{Status: "error", Reason: "error"}, nil
	default:
		return core.Finish{}, unsupported("finish_reason")
	}
}
func finishToWire(f core.Finish) (string, error) {
	if f.Cancellation != "" {
		return "", unsupported("finish.cancellation")
	}
	reason := f.Reason
	if reason == "" {
		switch f.Status {
		case "completed", "":
			reason = "stop"
		case "incomplete":
			reason = "length"
		case "error":
			reason = "error"
		default:
			return "", unsupported("finish.status")
		}
	}
	switch reason {
	case "stop", "tool_calls", "content_filter":
		if f.Status != "" && f.Status != "completed" {
			return "", unsupported("finish.status")
		}
	case "length":
		if f.Status != "" && f.Status != "incomplete" {
			return "", unsupported("finish.status")
		}
	case "error":
		if f.Status != "" && f.Status != "error" {
			return "", unsupported("finish.status")
		}
	default:
		return "", unsupported("finish.reason")
	}
	return reason, nil
}
func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	var v response
	if e := decode(r, &v); e != nil {
		return nil, e
	}
	if len(v.Choices) != 1 {
		return nil, unsupported("choices")
	}
	ch := v.Choices[0]
	if e := validChoice(ch); e != nil {
		return nil, e
	}
	if ch.Finish == nil {
		return nil, malformed()
	}
	f, e := finishFromWire(*ch.Finish)
	if e != nil {
		return nil, e
	}
	u, e := v.Usage.core()
	if e != nil {
		return nil, e
	}
	return core.CompletionResult{ID: v.ID, Model: v.Model, Text: ch.Text, Finish: f, Usage: u}, nil
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	var v core.CompletionResult
	if w == nil {
		return unsupported("body")
	}
	switch x := p.(type) {
	case core.CompletionResult:
		v = x
	case *core.CompletionResult:
		if x == nil {
			return unsupported("payload")
		}
		v = *x
	default:
		return unsupported("payload")
	}
	reason, e := finishToWire(v.Finish)
	if e != nil {
		return e
	}
	u, e := usage(v.Usage)
	if e != nil {
		return e
	}
	return json.NewEncoder(w).Encode(response{ID: v.ID, Object: "text_completion", Model: v.Model, Choices: []choice{{Text: v.Text, Finish: &reason}}, Usage: u})
}

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
