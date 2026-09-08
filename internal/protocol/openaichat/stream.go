package openaichat

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
	"io"
	"sort"
	"strings"
)

type chunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object,omitempty"`
	Created           int64         `json:"created,omitempty"`
	Model             string        `json:"model"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
	Choices           []chunkChoice `json:"choices"`
	Usage             *wireUsage    `json:"usage,omitempty"`
}
type chunkChoice struct {
	Index        int             `json:"index"`
	Delta        chunkDelta      `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
type chunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	ToolCalls []chunkToolCall `json:"tool_calls,omitempty"`
}
type chunkToolCall struct {
	Index    int           `json:"index"`
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function chunkFunction `json:"function"`
}
type chunkFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
type streamErrorPayload struct {
	Error struct {
		Type    string `json:"type,omitempty"`
		Code    string `json:"code,omitempty"`
		Message string `json:"message"`
		Param   string `json:"param,omitempty"`
	} `json:"error"`
}

type streamDecoder struct {
	s                                      *framing.SSEDecoder
	pending                                []core.Event
	started, finished, done, finishEmitted bool
	finishPending                          *core.Finish
	id, model                              string
	blocks                                 map[int]int
	text                                   map[int]core.Index
	tools                                  map[[2]int]core.Index
	toolIDs                                map[[2]int]string
	toolNames                              map[[2]int]string
}

func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if r == nil {
		return nil, unsupported("reader")
	}
	return &streamDecoder{s: framing.NewSSEDecoder(r, framing.DefaultMaxEventBytes), blocks: map[int]int{}, text: map[int]core.Index{}, tools: map[[2]int]core.Index{}, toolIDs: map[[2]int]string{}, toolNames: map[[2]int]string{}}, nil
}
func (d *streamDecoder) Next(ctx context.Context) (core.Event, error) {
	if len(d.pending) > 0 {
		e := d.pending[0]
		d.pending = d.pending[1:]
		return e, nil
	}
	if d.done {
		return nil, io.EOF
	}
	for {
		ev, e := d.s.Next(ctx)
		if e != nil {
			if e == io.EOF {
				if !d.finished {
					return nil, io.ErrUnexpectedEOF
				}
				if d.finishPending != nil && !d.finishEmitted {
					d.pending = append(d.pending, *d.finishPending)
					d.finishEmitted = true
					if len(d.pending) > 0 {
						v := d.pending[0]
						d.pending = d.pending[1:]
						return v, nil
					}
				}
				return nil, io.EOF
			}
			return nil, e
		}
		if ev.Data == "" {
			continue
		}
		if ev.Event == "error" {
			var er streamErrorPayload
			if e := strict([]byte(ev.Data), &er); e != nil {
				return nil, e
			}
			if er.Error.Message == "" {
				return nil, unsupported("stream.error")
			}
			d.done = true
			return core.StreamError{Error: core.GatewayError{Code: er.Error.Code, Message: er.Error.Message, Param: er.Error.Param, HTTPStatus: 400, Origin: "provider"}}, nil
		}
		if ev.Data == "[DONE]" {
			if !d.finished {
				return nil, io.ErrUnexpectedEOF
			}
			d.done = true
			if d.finishPending != nil && !d.finishEmitted {
				d.pending = append(d.pending, *d.finishPending)
				d.finishEmitted = true
			}
			if len(d.pending) > 0 {
				e := d.pending[0]
				d.pending = d.pending[1:]
				return e, nil
			}
			return nil, io.EOF
		}
		var x chunk
		if e := strict([]byte(ev.Data), &x); e != nil {
			return nil, e
		}
		if x.Object != "" && x.Object != "chat.completion.chunk" {
			return nil, unsupported("stream.object")
		}
		if x.ID != "" {
			if d.id != "" && d.id != x.ID {
				return nil, unsupported("stream.id")
			}
			d.id = x.ID
		}
		if x.Model != "" {
			if d.model != "" && d.model != x.Model {
				return nil, unsupported("stream.model")
			}
			d.model = x.Model
		}
		if !d.started {
			if d.id == "" || d.model == "" {
				return nil, unsupported("stream.identity")
			}
			d.started = true
			d.pending = append(d.pending, core.Start{ID: d.id, Model: d.model})
		}
		if d.finished && len(x.Choices) > 0 {
			return nil, unsupported("stream.after_finish")
		}
		for _, ch := range x.Choices {
			if e := d.choice(ch); e != nil {
				return nil, e
			}
		}
		if x.Usage != nil {
			u, err := decodeUsage(x.Usage)
			if err != nil {
				return nil, err
			}
			if !d.finished {
				return nil, unsupported("usage.before_finish")
			}
			d.pending = append(d.pending, *u)
			if d.finishPending != nil && !d.finishEmitted {
				d.pending = append(d.pending, *d.finishPending)
				d.finishEmitted = true
			}
		}
		if len(d.pending) > 0 {
			e := d.pending[0]
			d.pending = d.pending[1:]
			return e, nil
		}
	}
}
func (d *streamDecoder) choice(ch chunkChoice) error {
	if ch.Index != 0 {
		return unsupported("choices.index")
	}
	if len(ch.Logprobs) > 0 && !isNull(ch.Logprobs) {
		return unsupported("choices.logprobs")
	}
	ci := ch.Index
	if ch.Delta.Role != "" && ch.Delta.Role != "assistant" {
		return unsupported("delta.role")
	}
	if ch.Delta.Content != nil {
		if _, ok := d.text[ci]; !ok {
			idx := core.Index{Choice: ci, Block: d.blocks[ci]}
			d.blocks[ci]++
			d.text[ci] = idx
			d.pending = append(d.pending, core.BlockStart{Index: idx, Kind: "text"})
		}
		d.pending = append(d.pending, core.TextDelta{Index: d.text[ci], Text: *ch.Delta.Content})
	}
	for _, tc := range ch.Delta.ToolCalls {
		if tc.Index < 0 {
			return unsupported("tool_calls.index")
		}
		key := [2]int{ci, tc.Index}
		idx, ok := d.tools[key]
		if tc.Type != "" && tc.Type != "function" {
			return unsupported("tool_calls.type")
		}
		if !ok {
			if tc.ID == "" || tc.Function.Name == "" {
				return unsupported("tool_calls.start")
			}
			idx = core.Index{Choice: ci, Block: d.blocks[ci], Tool: tc.Index}
			d.blocks[ci]++
			d.tools[key] = idx
			d.toolIDs[key] = tc.ID
			d.toolNames[key] = tc.Function.Name
			d.pending = append(d.pending, core.ToolCallStart{Index: idx, ID: tc.ID, Name: tc.Function.Name})
		} else {
			if tc.ID != "" && tc.ID != d.toolIDs[key] || tc.Function.Name != "" && tc.Function.Name != d.toolNames[key] {
				return unsupported("tool_calls.identity")
			}
		}
		if tc.Function.Arguments != "" {
			d.pending = append(d.pending, core.ToolArgumentsDelta{Index: idx, Fragment: tc.Function.Arguments})
		}
	}
	if ch.FinishReason != nil {
		if d.finished || *ch.FinishReason == "" {
			return unsupported("finish")
		}
		f, e := finishFromWire(*ch.FinishReason)
		if e != nil {
			return e
		}
		for ci2, idx := range d.text {
			d.pending = append(d.pending, core.BlockEnd{Index: idx})
			delete(d.text, ci2)
		}
		keys := make([][2]int, 0, len(d.tools))
		for key := range d.tools {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		for _, key := range keys {
			idx := d.tools[key]
			d.pending = append(d.pending, core.BlockEnd{Index: idx})
			delete(d.tools, key)
		}
		d.finished = true
		d.finishPending = &f
	}
	return nil
}
func isNull(b []byte) bool { return strings.TrimSpace(string(b)) == "null" }

type streamEncoder struct {
	w                              io.Writer
	started, finished, usage, done bool
	id, model                      string
	pendingUsage                   *core.Usage
	text                           map[core.Index]bool
	tools                          map[core.Index]bool
}

func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if w == nil {
		return nil, unsupported("writer")
	}
	return &streamEncoder{w: w, text: map[core.Index]bool{}, tools: map[core.Index]bool{}}, nil
}
func (e *streamEncoder) emit(ctx context.Context, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, er := json.Marshal(v)
	if er != nil {
		return er
	}
	return framing.WriteSSE(e.w, framing.SSEEvent{Data: string(b)})
}
func (e *streamEncoder) Write(ctx context.Context, v core.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.done {
		return unsupported("stream.after_done")
	}
	switch x := v.(type) {
	case core.Start:
		if e.started {
			return unsupported("stream.start")
		}
		if x.ID == "" || x.Model == "" {
			return unsupported("stream.identity")
		}
		e.started = true
		e.id = x.ID
		e.model = x.Model
		return e.emit(ctx, chunk{ID: e.id, Object: "chat.completion.chunk", Model: e.model, Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Role: "assistant"}}}})
	case core.ToolCallStart:
		if !e.started || e.finished || x.Index.Choice != 0 || x.Index.Tool < 0 || e.tools[x.Index] || x.ID == "" || x.Name == "" {
			return unsupported("tool.start")
		}
		e.tools[x.Index] = true
		return e.emit(ctx, chunk{ID: e.id, Model: e.model, Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{ToolCalls: []chunkToolCall{{Index: x.Index.Tool, ID: x.ID, Type: "function", Function: chunkFunction{Name: x.Name}}}}}}})
	case core.BlockStart:
		if !e.started || e.finished || x.Index.Choice != 0 {
			return unsupported("block.start")
		}
		if x.Kind != "text" && x.Kind != "tool_call" {
			return unsupported("block.kind")
		}
		if x.Kind == "text" {
			if e.text[x.Index] || x.Index.Tool != 0 {
				return unsupported("block.index")
			}
			e.text[x.Index] = true
		}
		if x.Kind == "tool_call" {
			if e.tools[x.Index] || x.ID == "" || x.Name == "" {
				return unsupported("block.tool_call")
			}
			e.tools[x.Index] = true
			return e.emit(ctx, chunk{ID: e.id, Model: e.model, Choices: []chunkChoice{{Index: x.Index.Choice, Delta: chunkDelta{ToolCalls: []chunkToolCall{{Index: x.Index.Tool, ID: x.ID, Type: "function", Function: chunkFunction{Name: x.Name}}}}}}})
		}
		return nil
	case core.TextDelta:
		if !e.text[x.Index] {
			return unsupported("text.delta")
		}
		return e.emit(ctx, chunk{ID: e.id, Model: e.model, Choices: []chunkChoice{{Index: x.Index.Choice, Delta: chunkDelta{Content: &x.Text}}}})
	case core.ToolArgumentsDelta:
		if !e.tools[x.Index] {
			return unsupported("tool.delta")
		}
		return e.emit(ctx, chunk{ID: e.id, Model: e.model, Choices: []chunkChoice{{Index: x.Index.Choice, Delta: chunkDelta{ToolCalls: []chunkToolCall{{Index: x.Index.Tool, Function: chunkFunction{Arguments: x.Fragment}}}}}}})
	case core.BlockEnd:
		if e.text[x.Index] {
			delete(e.text, x.Index)
			return nil
		}
		if e.tools[x.Index] {
			delete(e.tools, x.Index)
			return nil
		}
		return unsupported("block.end")
	case core.Finish:
		if !e.started || e.finished || len(e.text) > 0 || len(e.tools) > 0 {
			return unsupported("finish")
		}
		if err := finishOK(x); err != nil {
			return err
		}
		e.finished = true
		if err := e.emit(ctx, chunk{ID: e.id, Object: "chat.completion.chunk", Model: e.model, Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: &x.Reason}}}); err != nil {
			return err
		}
		if e.pendingUsage != nil {
			usage, err := encodeUsage(e.pendingUsage)
			if err != nil {
				return err
			}
			if err := e.emit(ctx, chunk{ID: e.id, Object: "chat.completion.chunk", Model: e.model, Choices: []chunkChoice{}, Usage: usage}); err != nil {
				return err
			}
			e.pendingUsage = nil
		}
		if err := framing.WriteSSE(e.w, framing.SSEEvent{Data: "[DONE]"}); err != nil {
			return err
		}
		e.done = true
		return nil
	case core.StreamError:
		if x.Error.Message == "" {
			return unsupported("stream.error")
		}
		var er streamErrorPayload
		er.Error.Code = x.Error.Code
		er.Error.Message = x.Error.Message
		er.Error.Param = x.Error.Param
		e.done = true
		return framing.WriteSSE(e.w, framing.SSEEvent{Event: "error", Data: string(mustJSON(er))})
	case core.Usage:
		if !e.started || e.finished || e.usage {
			return unsupported("usage")
		}
		if x.Input != nil && *x.Input < 0 || x.Output != nil && *x.Output < 0 || x.Total != nil && *x.Total < 0 {
			return unsupported("usage")
		}
		u := x
		e.pendingUsage = &u
		e.usage = true
		return nil
	default:
		return unsupported("event")
	}
}

var _ core.StreamCodec = (*Codec)(nil)
