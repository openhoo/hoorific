package cohere

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
	"io"
)

type streamDecoder struct {
	s                 *framing.SSEDecoder
	started, finished bool
	open              map[int]string
	args              map[int]string
	pending           []core.Event
	model             string
	id                string
	pendingFinish     *core.Finish
}
type streamEncoder struct {
	w                 io.Writer
	started, finished bool
	open              map[int]string
	args              map[int]string
	usage             *core.Usage
}

func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if r == nil {
		return nil, invalid("stream", "nil reader")
	}
	return &streamDecoder{s: framing.NewSSEDecoder(r, 0), open: map[int]string{}, args: map[int]string{}}, nil
}
func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if w == nil {
		return nil, invalid("stream", "nil writer")
	}
	return &streamEncoder{w: w, open: map[int]string{}, args: map[int]string{}}, nil
}
func (d *streamDecoder) Next(ctx context.Context) (core.Event, error) {
	if len(d.pending) > 0 {
		e := d.pending[0]
		d.pending = d.pending[1:]
		return e, nil
	}
	if d.pendingFinish != nil {
		f := *d.pendingFinish
		d.pendingFinish = nil
		return f, nil
	}
	if d.finished {
		return nil, io.EOF
	}
	e, err := d.s.Next(ctx)
	if err != nil {
		if err == io.EOF {
			return nil, invalid("stream", "ended before message-end")
		}
		return nil, err
	}
	if e.Event == "" && e.Data == "" {
		return d.Next(ctx)
	}
	var raw map[string]json.RawMessage
	if err := strict([]byte(e.Data), &raw, "stream.data"); err != nil {
		return nil, err
	}
	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil || typ == "" {
		return nil, invalid("stream.type", "is required")
	}
	if e.Event != "" && e.Event != typ {
		return nil, invalid("stream.event", "event name does not match typed event")
	}
	switch typ {
	case "message-start":
		if d.started {
			return nil, invalid("stream", "duplicate message-start")
		}
		var x struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Delta struct {
				Message struct {
					Role string `json:"role"`
				} `json:"message"`
			} `json:"delta"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Delta.Message.Role != "" && x.Delta.Message.Role != "assistant" {
			return nil, invalid("message-start.role", "must be assistant")
		}
		d.started = true
		d.id = x.ID
		return core.Start{ID: x.ID}, nil
	case "content-start":
		if !d.started {
			return nil, invalid("stream", "content-start before message-start")
		}
		var x struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			Delta struct {
				Message struct {
					Content struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			} `json:"delta"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, invalid("stream.index", "must be non-negative")
		}
		if x.Delta.Message.Content.Type != "text" {
			return nil, unsupported("stream.content.type")
		}
		if _, ok := d.open[x.Index]; ok {
			return nil, invalid("stream.index", "content index already open")
		}
		d.open[x.Index] = "text"
		if x.Delta.Message.Content.Text != "" {
			d.pending = append(d.pending, core.TextDelta{Index: core.Index{Block: x.Index}, Text: x.Delta.Message.Content.Text})
		}
		return core.BlockStart{Index: core.Index{Block: x.Index}, Kind: "text"}, nil
	case "content-delta":
		if !d.started {
			return nil, invalid("stream", "content-delta before message-start")
		}
		var x struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			Delta struct {
				Message struct {
					Content struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			} `json:"delta"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, invalid("stream.index", "must be non-negative")
		}
		if x.Delta.Message.Content.Type != "" && x.Delta.Message.Content.Type != "text" {
			return nil, unsupported("stream.content.type")
		}
		if d.open[x.Index] != "text" {
			return nil, invalid("stream.index", "text delta without content-start")
		}
		return core.TextDelta{Index: core.Index{Block: x.Index}, Text: x.Delta.Message.Content.Text}, nil
	case "content-end":
		if !d.started {
			return nil, invalid("stream", "content-end before message-start")
		}
		var x struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, invalid("stream.index", "must be non-negative")
		}
		if d.open[x.Index] != "text" {
			return nil, invalid("stream.index", "content-end without content-start")
		}
		delete(d.open, x.Index)
		return core.BlockEnd{Index: core.Index{Block: x.Index}}, nil
	case "tool-call-start":
		if !d.started {
			return nil, invalid("stream", "tool-call-start before message-start")
		}
		var x struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			Delta struct {
				Message struct {
					ToolCalls struct {
						ID       string       `json:"id"`
						Type     string       `json:"type"`
						Function wireFunction `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"delta"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, invalid("stream.index", "must be non-negative")
		}
		if x.Delta.Message.ToolCalls.Type != "function" || x.Delta.Message.ToolCalls.ID == "" || x.Delta.Message.ToolCalls.Function.Name == "" {
			return nil, invalid("tool-call-start", "invalid function call")
		}
		if _, ok := d.open[x.Index]; ok {
			return nil, invalid("stream.index", "tool index already open")
		}
		d.open[x.Index] = "tool"
		d.args[x.Index] = x.Delta.Message.ToolCalls.Function.Arguments
		if len(d.args[x.Index]) > maxBody {
			return nil, invalid("stream.tool.arguments", "exceeds limit")
		}
		if x.Delta.Message.ToolCalls.Function.Arguments != "" {
			d.pending = append(d.pending, core.ToolArgumentsDelta{Index: core.Index{Block: x.Index, Tool: x.Index}, Fragment: x.Delta.Message.ToolCalls.Function.Arguments})
		}
		return core.ToolCallStart{Index: core.Index{Block: x.Index, Tool: x.Index}, ID: x.Delta.Message.ToolCalls.ID, Name: x.Delta.Message.ToolCalls.Function.Name}, nil
	case "tool-call-delta":
		if !d.started {
			return nil, invalid("stream", "tool-call-delta before message-start")
		}
		var x struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			Delta struct {
				Message struct {
					ToolCalls struct {
						Function wireFunction `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"delta"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, invalid("stream.index", "must be non-negative")
		}
		if x.Delta.Message.ToolCalls.Function.Name != "" {
			return nil, unsupported("stream.tool.name")
		}
		if d.open[x.Index] != "tool" {
			return nil, invalid("stream.index", "tool delta without tool-call-start")
		}
		d.args[x.Index] += x.Delta.Message.ToolCalls.Function.Arguments
		if len(d.args[x.Index]) > maxBody {
			return nil, invalid("stream.tool.arguments", "exceeds limit")
		}
		return core.ToolArgumentsDelta{Index: core.Index{Block: x.Index, Tool: x.Index}, Fragment: x.Delta.Message.ToolCalls.Function.Arguments}, nil
	case "tool-call-end":
		if !d.started {
			return nil, invalid("stream", "tool-call-end before message-start")
		}
		var x struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, invalid("stream.index", "must be non-negative")
		}
		if d.open[x.Index] != "tool" {
			return nil, invalid("stream.index", "tool-call-end without tool-call-start")
		}
		if err := objectJSON([]byte(d.args[x.Index]), at("stream.tool", x.Index)+".arguments"); err != nil {
			return nil, err
		}
		delete(d.open, x.Index)
		delete(d.args, x.Index)
		return core.BlockEnd{Index: core.Index{Block: x.Index, Tool: x.Index}}, nil
	case "message-end":
		if !d.started {
			return nil, invalid("stream", "message-end before message-start")
		}
		if len(d.open) != 0 {
			return nil, invalid("stream", "message-end with open content")
		}
		var x struct {
			Type  string `json:"type"`
			Delta struct {
				FinishReason string     `json:"finish_reason"`
				Usage        *wireUsage `json:"usage"`
			} `json:"delta"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		status, reason, err := finishStatus(x.Delta.FinishReason)
		if err != nil {
			return nil, err
		}
		d.finished = true
		f := core.Finish{Status: status, Reason: reason}
		if x.Delta.Usage != nil {
			v, e := usageValue(x.Delta.Usage)
			if e != nil {
				return nil, e
			}
			d.pendingFinish = &f
			return v, nil
		}
		return f, nil
	case "error":
		var x struct {
			Type  string `json:"type"`
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := decodeKnown(raw, &x); err != nil {
			return nil, err
		}
		d.finished = true
		return core.StreamError{Error: core.GatewayError{Code: x.Error.Code, Message: x.Error.Message, HTTPStatus: 502, Origin: "cohere"}}, nil
	default:
		return nil, unsupported("stream event " + typ)
	}
}
func decodeKnown(raw map[string]json.RawMessage, out any) error {
	b, _ := json.Marshal(raw)
	return strict(b, out, "stream.data")
}
func validIndex(i core.Index) error {
	if i.Choice != 0 || i.Block < 0 || i.Tool < 0 {
		return invalid("stream.index", "choice and indexes must be non-negative; choice must be zero")
	}
	return nil
}
func validUsage(u core.Usage) error {
	if u.Input != nil && *u.Input < 0 || u.Output != nil && *u.Output < 0 || u.Total != nil && *u.Total < 0 {
		return invalid("usage", "counts must be non-negative")
	}
	if u.Total != nil && (u.Input == nil || u.Output == nil || *u.Total != *u.Input+*u.Output) {
		return unsupported("usage.total")
	}
	return nil
}
func usageValue(u *wireUsage) (core.Usage, error) {
	var v core.Usage
	v.Source = "cohere"
	if u == nil {
		return v, nil
	}
	x := u.Tokens
	if x == nil {
		x = u.Billed
	}
	if x != nil {
		if x.Input != nil {
			n := *x.Input
			v.Input = &n
		}
		if x.Output != nil {
			n := *x.Output
			v.Output = &n
		}
	}
	return v, validUsage(v)
}
func (s *streamEncoder) Write(ctx context.Context, ev core.Event) error { return s.write(ctx, ev) }
func (s *streamEncoder) write(ctx context.Context, ev core.Event) error {
	var typ string
	var body map[string]any
	switch x := ev.(type) {
	case core.Start:
		if x.Model != "" {
			return unsupported("start.model")
		}
		if s.started {
			return invalid("stream", "duplicate start")
		}
		s.started = true
		typ = "message-start"
		body = map[string]any{"id": x.ID, "type": typ, "delta": map[string]any{"message": map[string]any{"role": "assistant"}}}
	case core.BlockStart:
		if err := validIndex(x.Index); err != nil {
			return err
		}
		if !s.started {
			return invalid("stream", "start required")
		}
		if x.Kind == "tool_call" {
			if x.ID == "" || x.Name == "" {
				return invalid("tool_call", "id and name are required")
			}
			if _, ok := s.open[x.Index.Block]; ok {
				return invalid("stream.index", "already open")
			}
			s.open[x.Index.Block] = "tool"
			s.args[x.Index.Block] = ""
			typ = "tool-call-start"
			body = map[string]any{"type": typ, "index": x.Index.Block, "delta": map[string]any{"message": map[string]any{"tool_calls": map[string]any{"id": x.ID, "type": "function", "function": map[string]any{"name": x.Name, "arguments": ""}}}}}
			break
		}
		if x.Kind != "text" {
			return unsupported("stream block " + x.Kind)
		}
		if _, ok := s.open[x.Index.Block]; ok {
			return invalid("stream.index", "already open")
		}
		s.open[x.Index.Block] = "text"
		typ = "content-start"
		body = map[string]any{"type": typ, "index": x.Index.Block, "delta": map[string]any{"message": map[string]any{"content": map[string]any{"type": "text", "text": ""}}}}
	case core.TextDelta:
		if err := validIndex(x.Index); err != nil {
			return err
		}
		if s.open[x.Index.Block] != "text" {
			return invalid("stream.index", "text delta without start")
		}
		typ = "content-delta"
		body = map[string]any{"type": typ, "index": x.Index.Block, "delta": map[string]any{"message": map[string]any{"content": map[string]any{"type": "text", "text": x.Text}}}}
	case core.ToolCallStart:
		if err := validIndex(x.Index); err != nil {
			return err
		}
		if !s.started {
			return invalid("stream", "start required")
		}
		if _, ok := s.open[x.Index.Block]; ok {
			return invalid("stream.index", "already open")
		}
		s.open[x.Index.Block] = "tool"
		s.args[x.Index.Block] = ""
		typ = "tool-call-start"
		body = map[string]any{"type": typ, "index": x.Index.Block, "delta": map[string]any{"message": map[string]any{"tool_calls": map[string]any{"id": x.ID, "type": "function", "function": map[string]any{"name": x.Name, "arguments": ""}}}}}
	case core.ToolArgumentsDelta:
		if err := validIndex(x.Index); err != nil {
			return err
		}
		if s.open[x.Index.Block] != "tool" {
			return invalid("stream.index", "tool delta without start")
		}
		s.args[x.Index.Block] += x.Fragment
		if len(s.args[x.Index.Block]) > maxBody {
			return invalid("stream.tool.arguments", "exceeds limit")
		}
		typ = "tool-call-delta"
		body = map[string]any{"type": typ, "index": x.Index.Block, "delta": map[string]any{"message": map[string]any{"tool_calls": map[string]any{"function": map[string]any{"arguments": x.Fragment}}}}}
	case core.BlockEnd:
		if err := validIndex(x.Index); err != nil {
			return err
		}
		kind := s.open[x.Index.Block]
		if kind == "" {
			return invalid("stream.index", "end without start")
		}
		if kind == "tool" {
			if err := objectJSON([]byte(s.args[x.Index.Block]), at("stream.tool", x.Index.Block)+".arguments"); err != nil {
				return err
			}
			delete(s.args, x.Index.Block)
		}
		delete(s.open, x.Index.Block)
		if kind == "tool" {
			typ = "tool-call-end"
		} else {
			typ = "content-end"
		}
		body = map[string]any{"type": typ, "index": x.Index.Block}
	case core.Usage:
		u := x
		if err := validUsage(u); err != nil {
			return err
		}
		s.usage = &u
		return nil
	case core.Finish:
		if !s.started || len(s.open) > 0 {
			return invalid("stream", "finish before blocks close")
		}
		reason, err := finishReason(x.Status, x.Reason)
		if err != nil {
			return err
		}
		s.finished = true
		typ = "message-end"
		delta := map[string]any{"finish_reason": reason}
		if s.usage != nil {
			u := map[string]any{}
			if s.usage.Input != nil {
				u["input_tokens"] = *s.usage.Input
			}
			if s.usage.Output != nil {
				u["output_tokens"] = *s.usage.Output
			}
			delta["usage"] = map[string]any{"tokens": u}
		}
		body = map[string]any{"type": typ, "delta": delta}
	case core.StreamError:
		s.finished = true
		typ = "error"
		body = map[string]any{"type": typ, "error": map[string]any{"code": x.Error.Code, "message": x.Error.Message}}
	default:
		return unsupported("stream event")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return framing.WriteSSE(s.w, framing.SSEEvent{Event: typ, Data: string(b)})
}
