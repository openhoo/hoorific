package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
	"io"
	"sort"
)

type sseDecoder struct {
	d              *framing.SSEDecoder
	pending        []core.Event
	started, ended bool
	id, model      string
	blocks         map[int]string
	tools          map[int]string
	closed         map[int]bool
	usage          *core.Usage
}

func eventObject(data string) (object, error) {
	if data == "[DONE]" {
		return nil, unsupported("stream.done")
	}
	var o object
	if e := strict([]byte(data), &o); e != nil {
		return nil, e
	}
	return o, nil
}
func (d *sseDecoder) queue(e core.Event) { d.pending = append(d.pending, e) }
func (d *sseDecoder) handle(ev framing.SSEEvent) error {
	if ev.Event == "error" {
		var eo object
		if err := strict([]byte(ev.Data), &eo); err != nil {
			return err
		}
		if nested, ok := eo["error"]; ok {
			ne, err := obj(nested)
			if err != nil {
				return err
			}
			eo = ne
		}
		if err := fields(eo, "type", "code", "message", "param"); err != nil {
			return err
		}
		var ge core.GatewayError
		_ = get(eo, "code", &ge.Code, false)
		_ = get(eo, "message", &ge.Message, false)
		_ = get(eo, "param", &ge.Param, false)
		if ge.Message == "" {
			ge.Message = "Responses stream error"
		}
		d.ended = true
		d.queue(core.StreamError{Error: ge})
		return nil
	}
	o, e := eventObject(ev.Data)
	if e != nil {
		return e
	}
	name := ev.Event
	if d.ended && name != "response.usage" {
		return unsupported("stream.after_terminal")
	}
	switch name {
	case "response.created":
		if d.started {
			return unsupported("stream.response.created")
		}
		var r object
		r, e = obj(o["response"])
		if e != nil {
			return e
		}
		d.id, e = str(r, "id")
		if e != nil {
			return e
		}
		d.model, _ = str(r, "model")
		d.started = true
		d.queue(core.Start{ID: d.id, Model: d.model})
	case "response.in_progress":
		if !d.started {
			return unsupported("stream.order")
		}
	case "response.output_item.added":
		if !d.started {
			return unsupported("stream.order")
		}
		var ix int
		if e = get(o, "output_index", &ix, true); e != nil || ix < 0 {
			return unsupported("output_index")
		}
		var item object
		item, e = obj(o["item"])
		if e != nil {
			return e
		}
		t := typ(item)
		var id, name, call, args string
		_ = get(item, "id", &id, false)
		_ = get(item, "name", &name, false)
		_ = get(item, "call_id", &call, false)
		_ = get(item, "arguments", &args, false)
		switch t {
		case "message":
			if _, ok := d.blocks[ix]; ok {
				return unsupported("output_index")
			}
			d.blocks[ix] = id
			d.queue(core.BlockStart{Index: core.Index{Block: ix}, Kind: "text", ID: id})
		case "function_call":
			if _, ok := d.tools[ix]; ok {
				return unsupported("output_index")
			}
			if call == "" {
				call = id
			}
			d.tools[ix] = call
			d.queue(core.ToolCallStart{Index: core.Index{Block: ix}, ID: call, Name: name})
			if args != "" {
				d.queue(core.ToolArgumentsDelta{Index: core.Index{Block: ix}, Fragment: args})
			}
		default:
			return unsupported("output_item.type")
		}
	case "response.content_part.added":
		var ix, ci int
		if e = get(o, "output_index", &ix, true); e != nil {
			return e
		}
		if e = get(o, "content_index", &ci, true); e != nil {
			return e
		}
		if ix < 0 || ci < 0 {
			return unsupported("content_index")
		}
		var part object
		part, e = obj(o["part"])
		if e != nil {
			return e
		}
		if typ(part) != "output_text" {
			return unsupported("content_part.type")
		}
	case "response.output_text.delta":
		var ix, ci int
		if e = get(o, "output_index", &ix, true); e != nil {
			return e
		}
		if e = get(o, "content_index", &ci, false); e != nil {
			return e
		}
		if ix < 0 || ci < 0 {
			return unsupported("output_index")
		}
		if _, ok := d.blocks[ix]; !ok || d.closed[ix] {
			return unsupported("stream.text")
		}
		var s string
		if e = get(o, "delta", &s, true); e != nil {
			return e
		}
		d.queue(core.TextDelta{Index: core.Index{Block: ix, Tool: ci}, Text: s})
	case "response.function_call_arguments.delta":
		var ix int
		if e = get(o, "output_index", &ix, true); e != nil {
			return e
		}
		if ix < 0 {
			return unsupported("output_index")
		}
		if _, ok := d.tools[ix]; !ok || d.closed[ix] {
			return unsupported("stream.arguments")
		}
		var s string
		if e = get(o, "delta", &s, true); e != nil {
			return e
		}
		d.queue(core.ToolArgumentsDelta{Index: core.Index{Block: ix}, Fragment: s})
	case "response.output_text.done", "response.function_call_arguments.done", "response.content_part.done":
		if !d.started {
			return unsupported("stream.order")
		}
	case "response.output_item.done":
		var ix int
		if e = get(o, "output_index", &ix, true); e != nil {
			return e
		}
		if ix < 0 || d.closed[ix] {
			return unsupported("stream.block_end")
		}
		if _, ok := d.blocks[ix]; !ok {
			if _, ok := d.tools[ix]; !ok {
				return unsupported("stream.block_end")
			}
		}
		d.closed[ix] = true
		d.queue(core.BlockEnd{Index: core.Index{Block: ix}})
	case "response.usage":
		if !d.started {
			return unsupported("stream.order")
		}
		var u wireUsageResult
		if e := strict([]byte(ev.Data), &u); e != nil {
			return e
		}
		x, e := decodeUsage(&u)
		if e != nil {
			return e
		}
		if d.usage == nil {
			d.usage = x
			if x != nil {
				d.queue(*x)
			}
		}
	case "response.completed", "response.incomplete":
		if !d.started {
			return unsupported("stream.order")
		}
		var r object
		r, e = obj(o["response"])
		if e != nil {
			return e
		}
		var id, model string
		_ = get(r, "id", &id, false)
		_ = get(r, "model", &model, false)
		if id != "" && id != d.id {
			return unsupported("response.id")
		}
		if model != "" && model != d.model {
			return unsupported("response.model")
		}
		if ub, ok := r["usage"]; ok && d.usage == nil {
			var u wireUsageResult
			if e := strict(ub, &u); e != nil {
				return e
			}
			d.usage, e = decodeUsage(&u)
			if e != nil {
				return e
			}
			if d.usage != nil {
				d.queue(*d.usage)
			}
		}
		status, reason := "completed", "stop"
		if len(d.tools) > 0 {
			reason = "tool_calls"
		}
		if name == "response.incomplete" {
			status = "incomplete"
			if ib, ok := r["incomplete_details"]; ok {
				var in object
				in, e = obj(ib)
				if e != nil {
					return e
				}
				_ = get(in, "reason", &reason, false)
				if reason == "max_output_tokens" {
					reason = "length"
				}
			}
		}
		d.queue(core.Finish{Status: status, Reason: reason})
		d.ended = true
	default:
		return unsupported("stream.event")
	}
	return nil
}
func (d *sseDecoder) Next(ctx context.Context) (core.Event, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if len(d.pending) > 0 {
		x := d.pending[0]
		d.pending = d.pending[1:]
		return x, nil
	}
	for {
		f, e := d.d.Next(ctx)
		if e != nil {
			if e == io.EOF && !d.ended {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, e
		}
		if e = d.handle(f); e != nil {
			return nil, e
		}
		if len(d.pending) > 0 {
			x := d.pending[0]
			d.pending = d.pending[1:]
			return x, nil
		}
	}
}

type sseEncoder struct {
	w              io.Writer
	started, ended bool
	id, model      string
	usage          *core.Usage
	blocks         map[int]core.ContentBlock
	closed         map[int]bool
	seq            int64
}

func (e *sseEncoder) item(ix int) any {
	b := e.blocks[ix]
	if b.Kind == "text" {
		return map[string]any{"id": b.ID, "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": b.Text}}}
	}
	return map[string]any{"id": b.ID, "type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": b.Arguments}
}
func (e *sseEncoder) send(name string, v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		m = map[string]any{"error": v}
	}
	m["type"] = name
	e.seq++
	m["sequence_number"] = e.seq
	b, _ := json.Marshal(m)
	return framing.WriteSSE(e.w, framing.SSEEvent{Event: name, Data: string(b)})
}
func (e *sseEncoder) Write(ctx context.Context, v core.Event) error {
	if x := ctx.Err(); x != nil {
		return x
	}
	if e.ended {
		return unsupported("stream.after_terminal")
	}
	switch x := v.(type) {
	case core.Start:
		if e.started {
			return unsupported("stream.start")
		}
		e.started = true
		e.id = x.ID
		e.model = x.Model
		return e.send("response.created", map[string]any{"response": map[string]any{"id": x.ID, "model": x.Model, "status": "in_progress"}})
	case core.BlockStart:
		if !e.started || x.Index.Block < 0 {
			return unsupported("stream.order")
		}
		if _, ok := e.blocks[x.Index.Block]; ok {
			return unsupported("stream.block_start")
		}
		if x.Kind != "text" && x.Kind != "tool_call" {
			return unsupported("stream.block")
		}
		e.blocks[x.Index.Block] = core.ContentBlock{Kind: x.Kind, ID: x.ID, Name: x.Name}
		if x.Kind == "text" {
			return e.send("response.output_item.added", map[string]any{"output_index": x.Index.Block, "item": map[string]any{"id": x.ID, "type": "message", "role": "assistant", "content": []any{}}})
		}
		return e.send("response.output_item.added", map[string]any{"output_index": x.Index.Block, "item": map[string]any{"id": x.ID, "type": "function_call", "call_id": x.ID, "name": x.Name, "arguments": ""}})
	case core.TextDelta:
		b, ok := e.blocks[x.Index.Block]
		if !ok || b.Kind != "text" || e.closed[x.Index.Block] || x.Index.Block < 0 {
			return unsupported("stream.text")
		}
		b.Text += x.Text
		e.blocks[x.Index.Block] = b
		return e.send("response.output_text.delta", map[string]any{"output_index": x.Index.Block, "content_index": x.Index.Tool, "delta": x.Text})
	case core.ToolCallStart:
		if !e.started || x.Index.Block < 0 {
			return unsupported("stream.order")
		}
		if _, ok := e.blocks[x.Index.Block]; ok {
			return unsupported("stream.tool_start")
		}
		e.blocks[x.Index.Block] = core.ContentBlock{Kind: "tool_call", ID: x.ID, Name: x.Name}
		return e.send("response.output_item.added", map[string]any{"output_index": x.Index.Block, "item": map[string]any{"id": x.ID, "type": "function_call", "call_id": x.ID, "name": x.Name, "arguments": ""}})
	case core.ToolArgumentsDelta:
		b, ok := e.blocks[x.Index.Block]
		if !ok || b.Kind != "tool_call" || e.closed[x.Index.Block] || x.Index.Block < 0 {
			return unsupported("stream.arguments")
		}
		b.Arguments += x.Fragment
		e.blocks[x.Index.Block] = b
		return e.send("response.function_call_arguments.delta", map[string]any{"output_index": x.Index.Block, "delta": x.Fragment})
	case core.BlockEnd:
		if _, ok := e.blocks[x.Index.Block]; !ok || e.closed[x.Index.Block] {
			return unsupported("stream.block_end")
		}
		e.closed[x.Index.Block] = true
		return e.send("response.output_item.done", map[string]any{"output_index": x.Index.Block, "item": e.item(x.Index.Block)})
	case core.Usage:
		if !e.started {
			return unsupported("stream.order")
		}
		if x.Input != nil && *x.Input < 0 || x.Output != nil && *x.Output < 0 || x.Total != nil && *x.Total < 0 {
			return unsupported("usage")
		}
		e.usage = &x
		return nil
	case core.Finish:
		if !e.started || x.Cancellation != "" {
			return unsupported("finish")
		}
		status := x.Status
		if status == "" {
			status = "completed"
		}
		if status != "completed" && status != "incomplete" {
			return unsupported("finish.status")
		}
		reason := x.Reason
		if reason == "" {
			reason = "stop"
			for _, b := range e.blocks {
				if b.Kind == "tool_call" {
					reason = "tool_calls"
					break
				}
			}
		}
		if status == "completed" && (reason != "stop" && reason != "stop_sequence" && reason != "tool_calls") {
			return unsupported("finish.reason")
		}
		r := map[string]any{"id": e.id, "model": e.model, "status": status, "output": []any{}}
		keys := make([]int, 0, len(e.blocks))
		for k := range e.blocks {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			r["output"] = append(r["output"].([]any), e.item(k))
		}
		if e.usage != nil {
			r["usage"] = encodeUsage(e.usage)
		}
		name := "response.completed"
		if status == "incomplete" {
			if reason == "length" {
				reason = "max_output_tokens"
			}
			if reason != "max_output_tokens" && reason != "content_filter" && reason != "error" {
				return unsupported("finish.reason")
			}
			r["incomplete_details"] = map[string]any{"reason": reason}
			name = "response.incomplete"
		}
		e.ended = true
		return e.send(name, map[string]any{"response": r})
	case core.StreamError:
		e.ended = true
		return e.send("error", map[string]any{"error": x.Error})
	default:
		return fmt.Errorf("unsupported stream event %T", v)
	}
}
func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	return &sseDecoder{d: framing.NewSSEDecoder(r, 0), blocks: map[int]string{}, tools: map[int]string{}, closed: map[int]bool{}}, nil
}
func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	return &sseEncoder{w: w, blocks: map[int]core.ContentBlock{}, closed: map[int]bool{}}, nil
}

var _ core.StreamCodec = (*Codec)(nil)
