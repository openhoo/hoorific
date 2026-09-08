package cohere

import (
	"context"
	"hoorific/internal/core"
	"io"
)

type wireResult struct {
	ID           string              `json:"id"`
	Model        string              `json:"model,omitempty"`
	FinishReason string              `json:"finish_reason"`
	Message      wireResponseMessage `json:"message"`
	Usage        *wireUsage          `json:"usage"`
}
type wireResponseMessage struct {
	Role      string        `json:"role"`
	Content   []wireContent `json:"content,omitempty"`
	ToolCalls []wireCall    `json:"tool_calls,omitempty"`
}
type wireUsage struct {
	Tokens *wireTokenUsage `json:"tokens"`
	Billed *wireTokenUsage `json:"billed_units"`
}
type wireTokenUsage struct {
	Input  *int64 `json:"input_tokens"`
	Output *int64 `json:"output_tokens"`
}

func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var x wireResult
	if err := readJSON(ctx, r, &x); err != nil {
		return nil, err
	}
	if x.ID == "" {
		return nil, invalid("id", "is required")
	}
	if x.Message.Role != "" && x.Message.Role != "assistant" {
		return nil, invalid("message.role", "must be assistant")
	}
	status, reason, err := finishStatus(x.FinishReason)
	if err != nil {
		return nil, err
	}
	out := core.GenerationResult{ID: x.ID, Model: x.Model, Finish: core.Finish{Status: status, Reason: reason}}
	for i, b := range x.Message.Content {
		switch b.Type {
		case "text":
			out.Blocks = append(out.Blocks, core.ContentBlock{Kind: "text", Text: b.Text})
		case "thinking":
			return nil, unsupported(at("message.content", i) + ".thinking")
		default:
			return nil, unsupported(at("message.content", i) + ".type")
		}
	}
	for i, c := range x.Message.ToolCalls {
		if c.Type != "function" || c.ID == "" || c.Function.Name == "" {
			return nil, invalid(at("message.tool_calls", i), "invalid function call")
		}
		if err := objectJSON([]byte(c.Function.Arguments), at("message.tool_calls", i)+".function.arguments"); err != nil {
			return nil, err
		}
		out.Blocks = append(out.Blocks, core.ContentBlock{Kind: "tool_call", ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	if x.Usage != nil {
		u := x.Usage.Tokens
		if u == nil {
			u = x.Usage.Billed
		}
		if u != nil {
			out.Usage = &core.Usage{Source: "cohere"}
			if u.Input != nil {
				v := *u.Input
				out.Usage.Input = &v
			}
			if u.Output != nil {
				v := *u.Output
				out.Usage.Output = &v
			}
			if u.Input != nil && u.Output != nil {
				v := *u.Input + *u.Output
				out.Usage.Total = &v
			}
		}
	}
	return out, nil
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	r, ok := p.(core.GenerationResult)
	if !ok {
		if rp, yes := p.(*core.GenerationResult); yes && rp != nil {
			r = *rp
			ok = true
		}
	}
	if !ok {
		return invalid("result", "expected core.GenerationResult")
	}
	if r.Finish.Cancellation != "" {
		return unsupported("finish.cancellation")
	}
	reason, err := finishReason(r.Finish.Status, r.Finish.Reason)
	if err != nil {
		return err
	}
	x := wireResult{ID: r.ID, Model: r.Model, FinishReason: reason, Message: wireResponseMessage{Role: "assistant"}}
	for i, b := range r.Blocks {
		if err := cleanBlock(b, b.Kind); err != nil {
			return invalid(at("blocks", i), err.Error())
		}
		switch b.Kind {
		case "text":
			x.Message.Content = append(x.Message.Content, wireContent{Type: "text", Text: b.Text})
		case "tool_call":
			x.Message.ToolCalls = append(x.Message.ToolCalls, wireCall{ID: b.ID, Type: "function", Function: wireFunction{Name: b.Name, Arguments: b.Arguments}})
		default:
			return unsupported(at("blocks", i) + ".kind")
		}
	}
	if r.Usage != nil {
		for _, n := range []*int64{
			r.Usage.Input, r.Usage.Output, r.Usage.Total, r.Usage.CachedInput,
			r.Usage.CacheWriteInput, r.Usage.CacheWrite5mInput, r.Usage.CacheWrite1hInput,
			r.Usage.ReasoningOutput, r.Usage.ToolInput,
		} {
			if n != nil && *n < 0 {
				return invalid("usage", "counts must be non-negative")
			}
		}
		if r.Usage.Input != nil && r.Usage.Output != nil && r.Usage.Total != nil && *r.Usage.Total != *r.Usage.Input+*r.Usage.Output {
			return invalid("usage.total", "does not match input plus output")
		}
		x.Usage = &wireUsage{Tokens: &wireTokenUsage{}}
		if r.Usage.Input != nil {
			v := *r.Usage.Input
			x.Usage.Tokens.Input = &v
		}
		if r.Usage.Output != nil {
			v := *r.Usage.Output
			x.Usage.Tokens.Output = &v
		}
	}
	return writeJSON(ctx, w, x)
}
