package anthropic

import (
	"context"
	"encoding/json"
	"io"

	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
)

type streamDecoder struct {
	s                           *framing.SSEDecoder
	started, terminal, finished bool
	blocks                      map[int]string
	usageSeen                   bool
	pendingFinish               *core.Finish
	pendingUsage                *core.Usage
	usage                       usageWire
}

func newStreamDecoder(r io.Reader) *streamDecoder {
	return &streamDecoder{s: framing.NewSSEDecoder(r, framing.DefaultMaxEventBytes), blocks: map[int]string{}}
}
func (d *streamDecoder) Next(ctx context.Context) (core.Event, error) {
	if d.pendingUsage != nil {
		u := *d.pendingUsage
		d.pendingUsage = nil
		return u, nil
	}
	if d.pendingFinish != nil {
		f := *d.pendingFinish
		d.pendingFinish = nil
		return f, nil
	}
	if d.terminal {
		return nil, io.EOF
	}
	e, err := d.s.Next(ctx)
	if err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if e.Event == "" {
		return nil, unsupported("sse.event")
	}
	if len(e.Data) == 0 {
		return nil, unsupported("sse.data")
	}
	switch e.Event {
	case "message_start":
		if d.started {
			return nil, unsupported("message_start")
		}
		var x struct {
			Type    string     `json:"type"`
			Message resultWire `json:"message"`
		}
		if err := strict([]byte(e.Data), &x); err != nil {
			return nil, err
		}
		if x.Type != "message_start" || x.Message.ID == "" || x.Message.Model == "" {
			return nil, unsupported("message_start")
		}
		d.started = true
		if x.Message.Usage != nil {
			if x.Message.Usage.Input != nil && *x.Message.Usage.Input < 0 || x.Message.Usage.Output != nil && *x.Message.Usage.Output < 0 {
				return nil, unsupported("usage")
			}
			d.usage = *x.Message.Usage
		}
		return core.Start{ID: x.Message.ID, Model: x.Message.Model}, nil
	case "content_block_start":
		if !d.started || d.finished {
			return nil, unsupported("content_block_start")
		}
		var x struct {
			Type    string    `json:"type"`
			Index   int       `json:"index"`
			Content blockWire `json:"content_block"`
		}
		if err := strict([]byte(e.Data), &x); err != nil {
			return nil, err
		}
		if x.Index < 0 {
			return nil, unsupported("index")
		}
		kind := x.Content.Type
		switch kind {
		case "text":
			kind = "text"
		case "tool_use":
			kind = "tool_call"
		default:
			return nil, unsupported("content_block.type")
		}
		if _, ok := d.blocks[x.Index]; ok {
			return nil, unsupported("index")
		}
		d.blocks[x.Index] = kind
		idx := core.Index{Block: x.Index}
		if kind == "tool_call" {
			if x.Content.ID == "" || x.Content.Name == "" {
				return nil, unsupported("content_block")
			}
			return core.ToolCallStart{Index: idx, ID: x.Content.ID, Name: x.Content.Name}, nil
		}
		return core.BlockStart{Index: idx, Kind: kind, ID: x.Content.ID, Name: x.Content.Name}, nil
	case "content_block_delta":
		if !d.started || d.finished {
			return nil, unsupported("content_block_delta")
		}
		var x struct {
			Type  string          `json:"type"`
			Index int             `json:"index"`
			Delta json.RawMessage `json:"delta"`
		}
		if err := strict([]byte(e.Data), &x); err != nil {
			return nil, err
		}
		kind, ok := d.blocks[x.Index]
		if !ok {
			return nil, unsupported("index")
		}
		var dh struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Partial string `json:"partial_json"`
		}
		if err := strict(x.Delta, &dh); err != nil {
			return nil, err
		}
		switch dh.Type {
		case "text_delta":
			if kind != "text" {
				return nil, unsupported("delta.type")
			}
			return core.TextDelta{Index: core.Index{Block: x.Index}, Text: dh.Text}, nil
		case "input_json_delta":
			if kind != "tool_call" {
				return nil, unsupported("delta.type")
			}
			return core.ToolArgumentsDelta{Index: core.Index{Block: x.Index}, Fragment: dh.Partial}, nil
		default:
			return nil, unsupported("delta.type")
		}
	case "content_block_stop":
		if !d.started || d.finished {
			return nil, unsupported("content_block_stop")
		}
		var x struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := strict([]byte(e.Data), &x); err != nil {
			return nil, err
		}
		if _, ok := d.blocks[x.Index]; !ok {
			return nil, unsupported("index")
		}
		delete(d.blocks, x.Index)
		return core.BlockEnd{Index: core.Index{Block: x.Index}}, nil
	case "message_delta":
		if !d.started {
			return nil, unsupported("message_delta")
		}
		var x struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason   *string `json:"stop_reason"`
				StopSequence *string `json:"stop_sequence"`
			} `json:"delta"`
			Usage *usageWire `json:"usage"`
		}
		if err := strict([]byte(e.Data), &x); err != nil {
			return nil, err
		}
		var fin *core.Finish
		if x.Delta.StopReason != nil {
			if d.finished {
				return nil, unsupported("finish")
			}
			reason, status, er := reasonFromWire(*x.Delta.StopReason)
			if er != nil {
				return nil, er
			}
			if x.Delta.StopSequence != nil && reason != "stop_sequence" {
				return nil, unsupported("stop_sequence")
			}
			f := core.Finish{Status: status, Reason: reason}
			d.finished = true
			fin = &f
		}
		if x.Usage != nil {
			if x.Usage.Input != nil && *x.Usage.Input < 0 || x.Usage.Output != nil && *x.Usage.Output < 0 {
				return nil, unsupported("usage")
			}
			d.usageSeen = true
			if fin != nil {
				d.pendingFinish = fin
			}
			if x.Usage.Input != nil {
				d.usage.Input = x.Usage.Input
			}
			if x.Usage.Output != nil {
				d.usage.Output = x.Usage.Output
			}
			var total *int64
			if d.usage.Input != nil && d.usage.Output != nil {
				if *d.usage.Input > int64(^uint64(0)>>1)-*d.usage.Output {
					return nil, unsupported("usage")
				}
				n := *d.usage.Input + *d.usage.Output
				total = &n
			}
			return core.Usage{Input: d.usage.Input, Output: d.usage.Output, Total: total, Source: "anthropic"}, nil
		}
		if fin != nil {
			return *fin, nil
		}
		return nil, unsupported("message_delta")
	case "message_stop":
		if !d.started {
			return nil, unsupported("message_stop")
		}
		d.terminal = true
		if !d.finished {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, io.EOF
	case "error":
		var x struct {
			Type  string            `json:"type"`
			Error core.GatewayError `json:"error"`
		}
		if err := strict([]byte(e.Data), &x); err != nil {
			return nil, err
		}
		if x.Type != "error" {
			return nil, unsupported("error.type")
		}
		d.terminal = true
		return core.StreamError{Error: x.Error}, nil
	default:
		return nil, unsupported("sse.event")
	}
}

type streamEncoder struct {
	w                           io.Writer
	started, finished, terminal bool
	blocks                      map[int]string
	usage                       usageWire
}

func newStreamEncoder(w io.Writer) *streamEncoder {
	return &streamEncoder{w: w, blocks: map[int]string{}}
}
func (x *streamEncoder) write(name string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return framing.WriteSSE(x.w, framing.SSEEvent{Event: name, Data: string(b)})
}
func (x *streamEncoder) Write(ctx context.Context, e core.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.terminal {
		return unsupported("stream.terminal")
	}
	switch v := e.(type) {
	case core.Start:
		if x.started {
			return unsupported("start")
		}
		x.started = true
		// Required wire counters start at zero until a provider Usage event
		// supplies cumulative counts. These are presentation placeholders only;
		// no core Usage event or accounting evidence is synthesized here.
		zero := int64(0)
		x.usage = usageWire{Input: &zero, Output: &zero}
		return x.write("message_start", struct {
			Type    string     `json:"type"`
			Message resultWire `json:"message"`
		}{"message_start", resultWire{ID: v.ID, Type: "message", Role: "assistant", Model: v.Model, Content: []json.RawMessage{}, Usage: &usageWire{Input: &zero, Output: &zero}}})
	case core.BlockStart:
		if !x.started || x.finished {
			return unsupported("block_start")
		}
		if v.Index.Block < 0 {
			return unsupported("index")
		}
		if _, ok := x.blocks[v.Index.Block]; ok {
			return unsupported("index")
		}
		block := blockWire{Type: v.Kind, ID: v.ID, Name: v.Name}
		switch v.Kind {
		case "text":
			text := ""
			block.Text = &text
		case "tool_use":
			block.Input = json.RawMessage("{}")
		}
		x.blocks[v.Index.Block] = v.Kind
		return x.write("content_block_start", struct {
			Type    string    `json:"type"`
			Index   int       `json:"index"`
			Content blockWire `json:"content_block"`
		}{"content_block_start", v.Index.Block, block})
	case core.ToolCallStart:
		if !x.started || x.finished {
			return unsupported("tool_call_start")
		}
		if _, ok := x.blocks[v.Index.Block]; ok {
			return unsupported("index")
		}
		x.blocks[v.Index.Block] = "tool_call"
		return x.write("content_block_start", struct {
			Type    string    `json:"type"`
			Index   int       `json:"index"`
			Content blockWire `json:"content_block"`
		}{"content_block_start", v.Index.Block, blockWire{Type: "tool_use", ID: v.ID, Name: v.Name, Input: json.RawMessage("{}")}})
	case core.TextDelta:
		if x.blocks[v.Index.Block] != "text" {
			return unsupported("text_delta")
		}
		return x.write("content_block_delta", struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta any    `json:"delta"`
		}{"content_block_delta", v.Index.Block, struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text_delta", Text: v.Text}})
	case core.ToolArgumentsDelta:
		if x.blocks[v.Index.Block] != "tool_call" {
			return unsupported("input_json_delta")
		}
		return x.write("content_block_delta", struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta any    `json:"delta"`
		}{"content_block_delta", v.Index.Block, struct {
			Type    string `json:"type"`
			Partial string `json:"partial_json"`
		}{Type: "input_json_delta", Partial: v.Fragment}})
	case core.BlockEnd:
		if _, ok := x.blocks[v.Index.Block]; !ok {
			return unsupported("block_end")
		}
		delete(x.blocks, v.Index.Block)
		return x.write("content_block_stop", struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}{"content_block_stop", v.Index.Block})
	case core.Usage:
		if v.Input != nil && *v.Input < 0 || v.Output != nil && *v.Output < 0 {
			return unsupported("usage")
		}
		if v.Input != nil {
			x.usage.Input = v.Input
		}
		if v.Output != nil {
			x.usage.Output = v.Output
		}
		return x.write("message_delta", struct {
			Type  string    `json:"type"`
			Delta any       `json:"delta"`
			Usage usageWire `json:"usage"`
		}{"message_delta", struct{}{}, x.usage})
	case core.Finish:
		if !x.started || x.finished {
			return unsupported("finish")
		}
		if len(x.blocks) > 0 {
			return unsupported("finish.open_blocks")
		}
		reason := v.Reason
		if reason == "" {
			reason = "stop"
		}
		wire, status, er := reasonToWire(reason)
		if er != nil {
			return er
		}
		if v.Status != "" && v.Status != status {
			return unsupported("finish.status")
		}
		x.finished = true
		if err := x.write("message_delta", struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage usageWire `json:"usage"`
		}{"message_delta", struct {
			StopReason string `json:"stop_reason"`
		}{wire}, x.usage}); err != nil {
			return err
		}
		x.terminal = true
		return x.write("message_stop", struct {
			Type string `json:"type"`
		}{"message_stop"})
	case core.StreamError:
		x.terminal = true
		return x.write("error", struct {
			Type  string            `json:"type"`
			Error core.GatewayError `json:"error"`
		}{"error", v.Error})
	default:
		return unsupported("event")
	}
}
