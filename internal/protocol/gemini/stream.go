package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
)

type streamDecoder struct {
	d                           *framing.SSEDecoder
	pending                     []core.Event
	started, finished, terminal bool
	toolSeen                    bool
	id, model                   string
	usage                       *core.Usage
	active                      map[core.Index]string
	textIndex                   core.Index
	textOpen                    bool
	nextBlock, nextTool         int
}
type streamError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status,omitempty"`
	} `json:"error"`
}

const maxStreamArguments = 16 << 20

func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if c == nil {
		return nil, unsupported("codec")
	}
	if r == nil {
		return nil, errors.New("nil event stream reader")
	}
	return &streamDecoder{d: framing.NewSSEDecoder(r, framing.DefaultMaxEventBytes), active: map[core.Index]string{}}, nil
}
func (s *streamDecoder) Next(ctx context.Context) (core.Event, error) {
	if e, ok := s.pendingEvent(); ok {
		return e, nil
	}
	f, e := s.d.Next(ctx)
	if e != nil {
		if e == io.EOF {
			if !s.finished {
				return nil, io.ErrUnexpectedEOF
			}
			s.terminal = true
			return nil, io.EOF
		}
		return nil, e
	}
	if s.finished {
		return nil, invalid("stream data after finish")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.Data), &envelope); err != nil && f.Data != "" {
		return nil, invalid("stream data")
	}
	if f.Event == "error" || envelope["error"] != nil {
		var se streamError
		if e := decode([]byte(f.Data), &se); e != nil {
			return nil, e
		}
		if se.Error.Code <= 0 || se.Error.Message == "" {
			return nil, invalid("stream error")
		}
		s.finished = true
		s.terminal = true
		return core.StreamError{Error: core.GatewayError{Code: strconv.Itoa(se.Error.Code), HTTPStatus: se.Error.Code, Message: se.Error.Message, Origin: "provider"}}, nil
	}
	if f.Event != "" && f.Event != "message" {
		return nil, unsupported("stream event")
	}
	if f.Data == "" {
		return s.Next(ctx)
	}
	var v response
	if e = decodeResponseData([]byte(f.Data), &v); e != nil {
		return nil, e
	}
	if v.ID != "" {
		s.id = v.ID
	}
	if v.Model != "" {
		s.model = v.Model
	}
	if !s.started {
		s.started = true
		s.pending = append(s.pending, core.Start{ID: s.id, Model: s.model})
	}
	if v.Usage != nil {
		u, e := usageCore(v.Usage)
		if e != nil {
			return nil, e
		}
		if s.usage != nil && !sameUsage(s.usage, u) {
			return nil, invalid("duplicate usageMetadata")
		}
		s.usage = u
	}
	if len(v.Candidates) > 1 {
		return nil, unsupported("multiple candidates")
	}
	if len(v.Candidates) == 1 {
		c := v.Candidates[0]
		if c.Index != nil && *c.Index != 0 {
			return nil, unsupported("candidate index")
		}
		if c.Content != nil && c.Content.Role != "" && c.Content.Role != "model" {
			return nil, invalid("candidate role")
		}
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				n := 0
				if p.Text != nil {
					n++
				}
				if p.Call != nil {
					n++
				}
				if p.Response != nil {
					n++
				}
				if n != 1 {
					return nil, invalid("parts union")
				}
				switch {
				case p.Text != nil:
					if !s.textOpen {
						s.textIndex = core.Index{Block: s.nextBlock}
						s.nextBlock++
						s.active[s.textIndex] = "text"
						s.textOpen = true
						s.pending = append(s.pending, core.BlockStart{Index: s.textIndex, Kind: "text"})
					}
					s.pending = append(s.pending, core.TextDelta{Index: s.textIndex, Text: *p.Text})
				case p.Call != nil:
					if p.Call.Name == "" {
						return nil, invalid("functionCall.name")
					}
					if len(p.Call.Args) > 0 {
						if e := object(p.Call.Args, "args"); e != nil {
							return nil, e
						}
					}
					if s.textOpen {
						s.pending = append(s.pending, core.BlockEnd{Index: s.textIndex})
						delete(s.active, s.textIndex)
						s.textOpen = false
					}
					ix := core.Index{Block: s.nextBlock, Tool: s.nextTool}
					s.nextBlock++
					s.nextTool++
					s.toolSeen = true
					s.active[ix] = "tool_call"
					s.pending = append(s.pending, core.ToolCallStart{Index: ix, ID: p.Call.ID, Name: p.Call.Name})
					a := string(p.Call.Args)
					if a == "" {
						a = "{}"
					}
					s.pending = append(s.pending, core.ToolArgumentsDelta{Index: ix, Fragment: a}, core.BlockEnd{Index: ix})
					delete(s.active, ix)
				case p.Response != nil:
					return nil, unsupported("stream functionResponse")
				}
			}
		}
		if c.Finish != "" {
			fin, err := finish(c.Finish, s.toolSeen)
			if err != nil {
				return nil, err
			}
			return s.finishStream(fin)
		}
		if blocked(c.SafetyRatings) {
			return s.finishStream(core.Finish{Reason: "content_filter", Status: "content_filter"})
		}
	} else if v.PromptFeedback != nil {
		fin, err := promptFinish(v.PromptFeedback)
		if err != nil {
			return nil, err
		}
		return s.finishStream(fin)
	}
	if e, ok := s.pendingEvent(); ok {
		return e, nil
	}
	return s.Next(ctx)
}
func sameCount(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameUsage(a, b *core.Usage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return sameCount(a.Input, b.Input) &&
		sameCount(a.Output, b.Output) &&
		sameCount(a.Total, b.Total) &&
		sameCount(a.CachedInput, b.CachedInput) &&
		sameCount(a.ReasoningOutput, b.ReasoningOutput) &&
		sameCount(a.ToolInput, b.ToolInput) &&
		sameCount(a.CacheWriteInput, b.CacheWriteInput) &&
		sameCount(a.CacheWrite5mInput, b.CacheWrite5mInput) &&
		sameCount(a.CacheWrite1hInput, b.CacheWrite1hInput) &&
		a.Source == b.Source
}

func (s *streamDecoder) finishStream(fin core.Finish) (core.Event, error) {
	for ix := range s.active {
		s.pending = append(s.pending, core.BlockEnd{Index: ix})
		delete(s.active, ix)
	}
	s.textOpen = false
	if s.usage != nil {
		s.pending = append(s.pending, *s.usage)
		s.usage = nil
	}
	s.pending = append(s.pending, fin)
	s.finished = true
	e, ok := s.pendingEvent()
	if !ok {
		return nil, invalid("stream finish")
	}
	return e, nil
}

func (s *streamDecoder) pendingEvent() (core.Event, bool) {
	if len(s.pending) == 0 {
		return nil, false
	}
	e := s.pending[0]
	s.pending = s.pending[1:]
	return e, true
}
func checkStreamIndex(ix core.Index) error {
	if ix.Choice != 0 || ix.Block < 0 || ix.Tool < 0 {
		return unsupported("stream index")
	}
	return nil
}

type encBlock struct {
	kind, id, name string
	args           string
}
type streamEncoder struct {
	w                 io.Writer
	started, finished bool
	id, model         string
	blocks            map[core.Index]*encBlock
}

func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if c == nil {
		return nil, unsupported("codec")
	}
	if w == nil {
		return nil, errors.New("nil event stream writer")
	}
	return &streamEncoder{w: w, blocks: map[core.Index]*encBlock{}}, nil
}
func (s *streamEncoder) emit(ctx context.Context, v response) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return framing.WriteSSE(s.w, framing.SSEEvent{Data: string(b)})
}
func (s *streamEncoder) Write(ctx context.Context, ev core.Event) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if s.finished {
		return io.ErrClosedPipe
	}
	switch v := ev.(type) {
	case core.Start:
		if s.started {
			return invalid("duplicate stream start")
		}
		s.started = true
		s.id = v.ID
		s.model = v.Model
	case core.BlockStart:
		if !s.started {
			return invalid("block before start")
		}
		if e := checkStreamIndex(v.Index); e != nil {
			return e
		}
		if _, ok := s.blocks[v.Index]; ok {
			return invalid("duplicate block start")
		}
		if v.Kind != "text" && v.Kind != "tool_call" {
			return unsupported("stream block kind")
		}
		s.blocks[v.Index] = &encBlock{kind: v.Kind, id: v.ID, name: v.Name}
	case core.TextDelta:
		if e := checkStreamIndex(v.Index); e != nil {
			return e
		}
		b, ok := s.blocks[v.Index]
		if !ok || b.kind != "text" {
			return invalid("text block")
		}
		if !s.started {
			return invalid("text before start")
		}
		p := part{}
		t := v.Text
		p.Text = &t
		return s.emit(ctx, response{ID: s.id, Model: s.model, Candidates: []candidate{{Content: &content{Role: "model", Parts: []part{p}}}}})
	case core.ToolCallStart:
		if e := checkStreamIndex(v.Index); e != nil {
			return e
		}
		if !s.started {
			return invalid("tool call before start")
		}
		b, ok := s.blocks[v.Index]
		if !ok {
			if v.Name == "" {
				return invalid("tool call name")
			}
			b = &encBlock{kind: "tool_call", id: v.ID, name: v.Name}
			s.blocks[v.Index] = b
		}
		if b.kind != "tool_call" {
			return invalid("tool call block")
		}
		if b.id != "" && v.ID != b.id {
			return invalid("tool call ID")
		}
		if b.name != "" && v.Name != b.name {
			return invalid("tool call name")
		}
		b.id = v.ID
		b.name = v.Name
		if b.name == "" {
			return invalid("tool call name")
		}
	case core.ToolArgumentsDelta:
		if e := checkStreamIndex(v.Index); e != nil {
			return e
		}
		b, ok := s.blocks[v.Index]
		if !ok || b.kind != "tool_call" {
			return invalid("tool argument block")
		}
		if len(b.args)+len(v.Fragment) > maxStreamArguments {
			return invalid("tool arguments size")
		}
		b.args += v.Fragment
	case core.BlockEnd:
		if e := checkStreamIndex(v.Index); e != nil {
			return e
		}
		b, ok := s.blocks[v.Index]
		if !ok {
			return invalid("unknown block")
		}
		if b.kind == "tool_call" {
			a := b.args
			if a == "" {
				a = "{}"
			}
			if e := object([]byte(a), "tool arguments"); e != nil {
				return e
			}
			p := part{Call: &functionCall{ID: b.id, Name: b.name, Args: json.RawMessage(a)}}
			if e := s.emit(ctx, response{ID: s.id, Model: s.model, Candidates: []candidate{{Content: &content{Role: "model", Parts: []part{p}}}}}); e != nil {
				return e
			}
		}
		delete(s.blocks, v.Index)
	case core.Usage:
		if !s.started {
			return invalid("usage before start")
		}
		u, e := usageWire(&v)
		if e != nil {
			return e
		}
		return s.emit(ctx, response{ID: s.id, Model: s.model, Usage: u})
	case core.Finish:
		if !s.started {
			return invalid("finish before start")
		}
		if len(s.blocks) > 0 {
			return invalid("finish with open blocks")
		}
		reason, e := finishReason(v)
		if e != nil {
			return e
		}
		if e := s.emit(ctx, response{ID: s.id, Model: s.model, Candidates: []candidate{{Finish: reason}}}); e != nil {
			return e
		}
		s.finished = true
	case core.StreamError:
		if v.Error.Code == "" || v.Error.Message == "" {
			return invalid("stream error")
		}
		code := v.Error.HTTPStatus
		if code <= 0 {
			code = 500
		}
		se := streamError{}
		se.Error.Code = code
		se.Error.Message = v.Error.Message
		se.Error.Status = v.Error.Code
		b, e := json.Marshal(se)
		if e != nil {
			return e
		}
		if e = framing.WriteSSE(s.w, framing.SSEEvent{Data: string(b)}); e != nil {
			return e
		}
		s.finished = true
	default:
		return unsupported("stream event")
	}
	return nil
}

var _ core.StreamCodec = (*Codec)(nil)
