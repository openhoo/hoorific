package openairesponses

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"io"
)

type wireUsageResult struct {
	Input  *int64 `json:"input_tokens"`
	Output *int64 `json:"output_tokens"`
	Total  *int64 `json:"total_tokens"`
}
type resultWire struct {
	ID         string            `json:"id"`
	Model      string            `json:"model"`
	Status     string            `json:"status"`
	Output     []json.RawMessage `json:"output"`
	Usage      *wireUsageResult  `json:"usage,omitempty"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
}

func decodeUsage(u *wireUsageResult) (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	for _, n := range []*int64{u.Input, u.Output, u.Total} {
		if n != nil && *n < 0 {
			return nil, unsupported("usage")
		}
	}
	return &core.Usage{Input: u.Input, Output: u.Output, Total: u.Total, Source: "provider"}, nil
}
func encodeUsage(u *core.Usage) *wireUsageResult {
	if u == nil {
		return nil
	}
	return &wireUsageResult{u.Input, u.Output, u.Total}
}
func outputBlock(v object) ([]core.ContentBlock, error) {
	t, e := str(v, "type")
	if e != nil {
		return nil, e
	}
	switch t {
	case "message":
		if e := fields(v, "type", "id", "role", "content", "status"); e != nil {
			return nil, e
		}
		var role string
		if e = get(v, "role", &role, true); e != nil {
			return nil, e
		}
		if role != "assistant" {
			return nil, unsupported("output.role")
		}
		var cs []json.RawMessage
		if e = get(v, "content", &cs, true); e != nil {
			return nil, e
		}
		var blocks []core.ContentBlock
		for _, x := range cs {
			o, e := obj(x)
			if e != nil {
				return nil, e
			}
			bs, e := parseOutputContent(o)
			if e != nil {
				return nil, e
			}
			blocks = append(blocks, bs...)
		}
		return blocks, nil
	case "function_call":
		if e := fields(v, "type", "id", "call_id", "name", "arguments", "status"); e != nil {
			return nil, e
		}
		var id, name, args string
		if e = get(v, "call_id", &id, true); e != nil {
			return nil, e
		}
		if e = get(v, "name", &name, true); e != nil {
			return nil, e
		}
		if e = get(v, "arguments", &args, true); e != nil {
			return nil, e
		}
		return []core.ContentBlock{{Kind: "tool_call", ID: id, Name: name, Arguments: args}}, nil
	case "reasoning":
		return nil, unsupported("output.reasoning")
	default:
		return nil, unsupported("output.type")
	}
}
func parseOutputContent(v object) ([]core.ContentBlock, error) {
	if e := fields(v, "type", "text", "annotations"); e != nil {
		return nil, e
	}
	if typ(v) != "output_text" {
		return nil, unsupported("output.content.type")
	}
	var s string
	if e := get(v, "text", &s, true); e != nil {
		return nil, e
	}
	if a, ok := v["annotations"]; ok {
		var aa []any
		if e := json.Unmarshal(a, &aa); e != nil {
			return nil, e
		}
		if len(aa) > 0 {
			return nil, unsupported("output.annotations")
		}
	}
	return []core.ContentBlock{{Kind: "text", Text: s}}, nil
}
func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	b, e := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if e != nil || len(b) > 32<<20 {
		return nil, unsupported("response.size")
	}
	var m object
	if e := strict(b, &m); e != nil {
		return nil, e
	}
	if e := fields(m, "id", "object", "created_at", "model", "status", "output", "usage", "incomplete_details", "error", "metadata", "parallel_tool_calls"); e != nil {
		return nil, e
	}
	var v resultWire
	if e := json.Unmarshal(b, &v); e != nil {
		return nil, unsupported("response")
	}
	if v.ID == "" || v.Status == "" {
		return nil, unsupported("response")
	}
	g := core.GenerationResult{ID: v.ID, Model: v.Model}
	for _, x := range v.Output {
		o, e := obj(x)
		if e != nil {
			return nil, e
		}
		bs, e := outputBlock(o)
		if e != nil {
			return nil, e
		}
		g.Blocks = append(g.Blocks, bs...)
	}
	switch v.Status {
	case "completed":
		g.Finish.Status = "completed"
		g.Finish.Reason = "stop"
		for _, b := range g.Blocks {
			if b.Kind == "tool_call" {
				g.Finish.Reason = "tool_calls"
				break
			}
		}
	case "incomplete":
		g.Finish.Status = "incomplete"
		if v.Incomplete == nil || v.Incomplete.Reason == "" {
			return nil, unsupported("incomplete_details.reason")
		}
		g.Finish.Reason = v.Incomplete.Reason
		if g.Finish.Reason == "max_output_tokens" {
			g.Finish.Reason = "length"
		}
	case "failed":
		return nil, unsupported("status")
	default:
		return nil, unsupported("status")
	}
	u, e := decodeUsage(v.Usage)
	if e != nil {
		return nil, e
	}
	g.Usage = u
	return g, nil
}
func resultOutput(b core.ContentBlock) json.RawMessage {
	if b.Kind == "text" {
		if b.URL != "" || b.MIMEType != "" || b.ID != "" || b.Name != "" || b.Arguments != "" || len(b.Data) > 0 {
			return nil
		}
		return raw(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": b.Text}}})
	}
	if b.Kind == "tool_call" {
		if b.Text != "" || b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 {
			return nil
		}
		return raw(map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": b.Arguments})
	}
	return nil
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	g, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("payload")
	}
	if g.ID == "" || g.Finish.Status == "" {
		return unsupported("response")
	}
	if g.Finish.Status != "completed" && g.Finish.Status != "incomplete" {
		return unsupported("finish.status")
	}
	if g.Finish.Cancellation != "" {
		return unsupported("finish.cancellation")
	}
	reason := g.Finish.Reason
	if g.Finish.Status == "completed" && reason == "" {
		reason = "stop"
		for _, b := range g.Blocks {
			if b.Kind == "tool_call" {
				reason = "tool_calls"
				break
			}
		}
	}
	if g.Finish.Status == "completed" && (reason != "stop" && reason != "stop_sequence" && reason != "tool_calls") {
		return unsupported("finish.reason")
	}
	v := resultWire{ID: g.ID, Model: g.Model, Status: g.Finish.Status, Usage: encodeUsage(g.Usage)}
	for _, b := range g.Blocks {
		x := resultOutput(b)
		if x == nil {
			return unsupported("output." + b.Kind)
		}
		v.Output = append(v.Output, x)
	}
	if g.Finish.Status == "incomplete" {
		if reason == "" {
			return unsupported("finish.reason")
		}
		if reason == "length" {
			reason = "max_output_tokens"
		}
		if reason != "max_output_tokens" && reason != "content_filter" && reason != "error" {
			return unsupported("finish.reason")
		}
		v.Incomplete = &struct {
			Reason string `json:"reason"`
		}{Reason: reason}
	}
	return encode(w, v)
}

var _ core.ResultCodec = (*Codec)(nil)
