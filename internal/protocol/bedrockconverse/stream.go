package bedrockconverse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	es "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"hoorific/internal/core"
	"io"
)

type decoder struct {
	r                                  io.Reader
	d                                  *es.Decoder
	payload                            []byte
	pending                            []core.Event
	pendingFinish                      *core.Finish
	args                               map[core.Index]*bytes.Buffer
	started, stopped, failed, metadata bool
	block                              map[core.Index]string
	nextBlock                          int
}
type encoder struct {
	w                          io.Writer
	e                          *es.Encoder
	args                       map[core.Index]*bytes.Buffer
	pendingUsage               *usage
	started, stopped, metadata bool
	block                      map[core.Index]string
	nextBlock                  int
}

func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if r == nil {
		return nil, errors.New("nil event stream reader")
	}
	return &decoder{r: r, d: es.NewDecoder(), args: map[core.Index]*bytes.Buffer{}, block: map[core.Index]string{}}, nil
}
func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if w == nil {
		return nil, errors.New("nil event stream writer")
	}
	return &encoder{w: w, e: es.NewEncoder(), args: map[core.Index]*bytes.Buffer{}, block: map[core.Index]string{}}, nil
}
func eventName(m es.Message) (string, error) {
	v := m.Headers.Get(":event-type")
	if v == nil {
		return "", invalid("event-type")
	}
	s, ok := v.(es.StringValue)
	if !ok {
		return "", invalid("event-type")
	}
	return string(s), nil
}
func messageType(m es.Message) (string, error) {
	v := m.Headers.Get(":message-type")
	if v == nil {
		return "event", nil
	}
	s, ok := v.(es.StringValue)
	if !ok {
		return "", invalid("message-type")
	}
	return string(s), nil
}
func decodeJSON(b []byte, v any) error { return strict(b, v) }
func appendArg(b *bytes.Buffer, s string) error {
	if b == nil || b.Len()+len(s) > maxJSON {
		return invalid("tool.input")
	}
	_, e := b.WriteString(s)
	return e
}

type msgStart struct {
	Role string `json:"role"`
}
type cbStart struct {
	ContentBlockIndex *int `json:"contentBlockIndex"`
	Start             struct {
		ToolUse *struct {
			ToolUseId string `json:"toolUseId"`
			Name      string `json:"name"`
		} `json:"toolUse,omitempty"`
	} `json:"start"`
}
type cbDelta struct {
	ContentBlockIndex *int `json:"contentBlockIndex"`
	Delta             struct {
		Text    *string `json:"text,omitempty"`
		ToolUse *struct {
			Input string `json:"input"`
		} `json:"toolUse,omitempty"`
	} `json:"delta"`
}
type cbStop struct {
	ContentBlockIndex *int `json:"contentBlockIndex"`
}
type msgStop struct {
	StopReason string `json:"stopReason"`
}
type streamMeta struct {
	Usage   *usage   `json:"usage,omitempty"`
	Metrics *metrics `json:"metrics,omitempty"`
}
type exception struct {
	Message string `json:"Message"`
	Code    string `json:"__type"`
}

func (d *decoder) Next(ctx context.Context) (core.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.failed {
		return nil, io.EOF
	}
	if len(d.pending) > 0 {
		ev := d.pending[0]
		d.pending = d.pending[1:]
		return ev, nil
	}
	m, e := d.d.Decode(d.r, d.payload[:0])
	if e != nil {
		if e == io.EOF {
			if d.pendingFinish != nil {
				f := *d.pendingFinish
				d.pendingFinish = nil
				return f, nil
			}
			if d.stopped {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		return nil, e
	}
	d.payload = m.Payload
	typ, e := messageType(m)
	if e != nil {
		return nil, e
	}
	if typ == "exception" {
		var x exception
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		if x.Code == "" {
			if v := m.Headers.Get(":exception-type"); v != nil {
				if s, ok := v.(es.StringValue); ok {
					x.Code = string(s)
				}
			}
		}
		if x.Code == "" {
			x.Code = "provider_exception"
		}
		d.stopped = true
		d.failed = true
		return core.StreamError{Error: core.GatewayError{Code: x.Code, HTTPStatus: 500, Message: x.Message, Origin: "provider"}}, nil
	}
	if typ != "event" {
		return nil, unsupported("message-type")
	}
	name, e := eventName(m)
	if e != nil {
		return nil, e
	}
	if d.stopped && name != "metadata" {
		return nil, order("event after message stop")
	}
	switch name {
	case "messageStart":
		if d.started || d.stopped {
			return nil, order("duplicate message start")
		}
		var x msgStart
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		if x.Role != "assistant" {
			return nil, invalid("role")
		}
		d.started = true
		return core.Start{}, nil
	case "contentBlockStart":
		if !d.started || d.stopped {
			return nil, order("content block start")
		}
		var x cbStart
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		if x.ContentBlockIndex == nil || *x.ContentBlockIndex != d.nextBlock {
			return nil, order("non-sequential block index")
		}
		idx := core.Index{Block: *x.ContentBlockIndex}
		d.nextBlock++
		if x.Start.ToolUse != nil {
			id := x.Start.ToolUse.ToolUseId
			if id == "" || x.Start.ToolUse.Name == "" {
				return nil, invalid("toolUse")
			}
			d.block[idx] = "tool_call"
			d.args[idx] = bytes.NewBuffer(nil)
			return core.BlockStart{Index: idx, Kind: "tool_call", ID: id, Name: x.Start.ToolUse.Name}, nil
		}
		d.block[idx] = "text"
		return core.BlockStart{Index: idx, Kind: "text"}, nil
	case "contentBlockDelta":
		if !d.started || d.stopped {
			return nil, order("content block delta")
		}
		var x cbDelta
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		if x.ContentBlockIndex == nil {
			return nil, invalid("contentBlockIndex")
		}
		if x.Delta.Text != nil && x.Delta.ToolUse != nil {
			return nil, invalid("delta")
		}
		idx := core.Index{Block: *x.ContentBlockIndex}
		if d.block[idx] == "" {
			if idx.Block != d.nextBlock || x.Delta.Text == nil {
				return nil, order("delta without block")
			}
			d.block[idx] = "text"
			d.nextBlock++
			d.pending = append(d.pending, core.TextDelta{Index: idx, Text: *x.Delta.Text})
			return core.BlockStart{Index: idx, Kind: "text"}, nil
		}
		if x.Delta.Text != nil {
			if d.block[idx] != "text" {
				return nil, order("text/tool block mismatch")
			}
			return core.TextDelta{Index: idx, Text: *x.Delta.Text}, nil
		}
		if x.Delta.ToolUse != nil {
			if d.block[idx] != "tool_call" {
				return nil, order("tool delta without tool")
			}
			if err := appendArg(d.args[idx], x.Delta.ToolUse.Input); err != nil {
				return nil, err
			}
			return core.ToolArgumentsDelta{Index: idx, Fragment: x.Delta.ToolUse.Input}, nil
		}
		return nil, unsupported("contentBlockDelta")
	case "contentBlockStop":
		if !d.started || d.stopped {
			return nil, order("content block stop")
		}
		var x cbStop
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		if x.ContentBlockIndex == nil {
			return nil, invalid("contentBlockIndex")
		}
		idx := core.Index{Block: *x.ContentBlockIndex}
		if d.block[idx] == "" {
			return nil, order("stop without block")
		}
		if d.block[idx] == "tool_call" {
			if e := object(d.args[idx].Bytes()); e != nil {
				return nil, e
			}
		}
		delete(d.block, idx)
		delete(d.args, idx)
		return core.BlockEnd{Index: idx}, nil
	case "messageStop":
		if !d.started || d.stopped || len(d.block) != 0 {
			return nil, order("message stop")
		}
		var x msgStop
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		f, e := finish(x.StopReason)
		if e != nil {
			return nil, e
		}
		d.stopped = true
		d.pendingFinish = &f
		return d.Next(ctx)
	case "metadata":
		if !d.stopped || d.metadata {
			return nil, order("metadata")
		}
		var x streamMeta
		if e = decodeJSON(m.Payload, &x); e != nil {
			return nil, e
		}
		d.metadata = true
		u, e := decodeUsage(x.Usage)
		if e != nil {
			return nil, e
		}
		if u == nil {
			return nil, unsupported("metadata.usage")
		}
		if d.pendingFinish != nil {
			d.pending = append(d.pending, *d.pendingFinish)
			d.pendingFinish = nil
		}
		return *u, nil
	default:
		return nil, unsupported("event." + name)
	}
}
func (e *encoder) write(ctx context.Context, name string, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return e.e.Encode(e.w, es.Message{Headers: es.Headers{{Name: ":message-type", Value: es.StringValue("event")}, {Name: ":event-type", Value: es.StringValue(name)}, {Name: ":content-type", Value: es.StringValue("application/json")}}, Payload: b})
}
func (e *encoder) writeException(ctx context.Context, g core.GatewayError) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := g.Code
	if name == "" {
		name = "GatewayError"
	}
	b, err := json.Marshal(exception{Message: g.Message})
	if err != nil {
		return err
	}
	e.stopped = true
	return e.e.Encode(e.w, es.Message{Headers: es.Headers{{Name: ":message-type", Value: es.StringValue("exception")}, {Name: ":exception-type", Value: es.StringValue(name)}, {Name: ":content-type", Value: es.StringValue("application/json")}}, Payload: b})
}
func (e *encoder) Write(ctx context.Context, ev core.Event) error {
	if ev == nil {
		return errors.New("nil event")
	}
	if e.stopped {
		return order("event after terminal")
	}
	switch x := ev.(type) {
	case core.Start:
		if e.started {
			return order("duplicate message start")
		}
		if x.ID != "" || x.Model != "" {
			return unsupported("start.identity")
		}
		e.started = true
		return e.write(ctx, "messageStart", msgStart{Role: "assistant"})
	case core.BlockStart:
		if !e.started {
			return order("block start")
		}
		if x.Index.Block != e.nextBlock || x.Index.Choice != 0 {
			return order("non-sequential block index")
		}
		if x.Kind != "tool_call" && x.Kind != "text" {
			return unsupported("block kind")
		}
		if x.Kind == "tool_call" && (x.ID == "" || x.Name == "") {
			return invalid("toolUse")
		}
		e.block[x.Index] = x.Kind
		e.nextBlock++
		if x.Kind == "tool_call" {
			e.args[x.Index] = bytes.NewBuffer(nil)
			return e.write(ctx, "contentBlockStart", map[string]any{"contentBlockIndex": x.Index.Block, "start": map[string]any{"toolUse": map[string]string{"toolUseId": x.ID, "name": x.Name}}})
		}
		return e.write(ctx, "contentBlockStart", map[string]any{"contentBlockIndex": x.Index.Block, "start": map[string]any{}})
	case core.ToolCallStart:
		return e.Write(ctx, core.BlockStart{Index: x.Index, Kind: "tool_call", ID: x.ID, Name: x.Name})
	case core.TextDelta:
		if !e.started || x.Index.Block < 0 || x.Index.Block >= e.nextBlock || e.block[x.Index] != "text" {
			return order("text delta index")
		}
		return e.write(ctx, "contentBlockDelta", map[string]any{"contentBlockIndex": x.Index.Block, "delta": map[string]string{"text": x.Text}})
	case core.ToolArgumentsDelta:
		if !e.started || e.block[x.Index] != "tool_call" {
			return order("tool delta")
		}
		if err := appendArg(e.args[x.Index], x.Fragment); err != nil {
			return err
		}
		return e.write(ctx, "contentBlockDelta", map[string]any{"contentBlockIndex": x.Index.Block, "delta": map[string]map[string]string{"toolUse": {"input": x.Fragment}}})
	case core.BlockEnd:
		if !e.started || e.block[x.Index] == "" {
			return order("block stop")
		}
		if e.block[x.Index] == "tool_call" {
			if err := object(e.args[x.Index].Bytes()); err != nil {
				return err
			}
		}
		delete(e.block, x.Index)
		delete(e.args, x.Index)
		return e.write(ctx, "contentBlockStop", cbStop{ContentBlockIndex: &x.Index.Block})
	case core.Usage:
		if !e.started || e.pendingUsage != nil {
			return order("usage ordering")
		}
		u, err := encodeUsage(&x)
		if err != nil {
			return err
		}
		e.pendingUsage = u
		return nil
	case core.Finish:
		if !e.started || len(e.block) != 0 {
			return order("message stop")
		}
		reason, err := stop(x)
		if err != nil {
			return err
		}
		e.stopped = true
		if err = e.write(ctx, "messageStop", msgStop{StopReason: reason}); err != nil {
			return err
		}
		if e.pendingUsage != nil {
			e.metadata = true
			return e.write(ctx, "metadata", streamMeta{Usage: e.pendingUsage})
		}
		return nil
	case core.StreamError:
		e2 := x.Error
		if e2.Code == "" {
			e2.Code = "provider_exception"
		}
		return e.writeException(ctx, e2)
	default:
		return fmt.Errorf("bedrock converse stream: unsupported event %T", ev)
	}
}
