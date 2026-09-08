package openaicompletion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
)

type streamResponse struct {
	ID      string         `json:"id,omitempty"`
	Object  string         `json:"object,omitempty"`
	Created *int64         `json:"created,omitempty"`
	Model   string         `json:"model,omitempty"`
	Choices []streamChoice `json:"choices"`
	Usage   *wireUsage     `json:"usage,omitempty"`
}
type streamChoice struct {
	Text     string          `json:"text,omitempty"`
	Index    int             `json:"index"`
	Logprobs json.RawMessage `json:"logprobs,omitempty"`
	Finish   json.RawMessage `json:"finish_reason"`
}

type streamDecoder struct {
	d                           *framing.SSEDecoder
	pending                     []core.Event
	started, finished, terminal bool
	id, model                   string
}

func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if c == nil {
		return nil, unsupported("codec")
	}
	return &streamDecoder{d: framing.NewSSEDecoder(r, framing.DefaultMaxEventBytes)}, nil
}
func (s *streamDecoder) Next(ctx context.Context) (core.Event, error) {
	if len(s.pending) > 0 {
		e := s.pending[0]
		s.pending = s.pending[1:]
		return e, nil
	}
	if s.terminal {
		return core.Event(nil), io.EOF
	}
	f, e := s.d.Next(ctx)
	if e != nil {
		if e == io.EOF && !s.terminal {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, e
	}
	if f.Event != "" && f.Event != "message" {
		return nil, unsupported("event")
	}
	if f.Data == "[DONE]" {
		if s.finished == false {
			return nil, io.ErrUnexpectedEOF
		}
		s.terminal = true
		return nil, io.EOF
	}
	var v streamResponse
	if e = decode(bytes.NewReader([]byte(f.Data)), &v); e != nil {
		return nil, e
	}
	if v.Usage != nil && len(v.Choices) == 0 {
		u, e := v.Usage.core()
		if e != nil {
			return nil, e
		}
		return *u, nil
	}
	if s.finished {
		return nil, io.ErrUnexpectedEOF
	}
	if len(v.Choices) != 1 {
		return nil, unsupported("choices")
	}
	ch := v.Choices[0]
	if ch.Index != 0 {
		return nil, unsupported("choices.index")
	}
	if len(ch.Logprobs) > 0 && !bytes.Equal(bytes.TrimSpace(ch.Logprobs), []byte("null")) {
		return nil, unsupported("logprobs")
	}
	if v.ID != "" {
		s.id = v.ID
	}
	if v.Model != "" {
		s.model = v.Model
	}
	if !s.started {
		s.started = true
		s.pending = append(s.pending, core.Start{ID: s.id, Model: s.model}, core.BlockStart{Index: core.Index{}, Kind: "text"})
	}
	if len(ch.Text) > 0 {
		s.pending = append(s.pending, core.TextDelta{Index: core.Index{}, Text: ch.Text})
	}
	if len(ch.Finish) > 0 && !bytes.Equal(bytes.TrimSpace(ch.Finish), []byte("null")) {
		var reason string
		if json.Unmarshal(ch.Finish, &reason) != nil {
			return nil, malformed()
		}
		f, e := finishFromWire(reason)
		if e != nil {
			return nil, e
		}
		s.finished = true
		s.pending = append(s.pending, core.BlockEnd{Index: core.Index{}}, f)
	}
	if len(s.pending) > 0 {
		e := s.pending[0]
		s.pending = s.pending[1:]
		return e, nil
	}
	return s.Next(ctx)
}

type streamEncoder struct {
	w                    io.Writer
	started, ended, done bool
	id, model            string
}

func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if c == nil {
		return nil, unsupported("codec")
	}
	if w == nil {
		return nil, unsupported("body")
	}
	return &streamEncoder{w: w}, nil
}
func (e *streamEncoder) Write(ctx context.Context, ev core.Event) error {
	if er := ctx.Err(); er != nil {
		return er
	}
	if e.done {
		return io.ErrClosedPipe
	}
	switch v := ev.(type) {
	case core.Start:
		if e.started {
			return unsupported("start")
		}
		e.started = true
		e.id = either(v.ID, e.id)
		e.model = either(v.Model, e.model)
	case core.BlockStart:
		if v.Index != (core.Index{}) || v.Kind != "text" {
			return unsupported("block")
		}
	case core.TextDelta:
		if v.Index != (core.Index{}) {
			return unsupported("text.index")
		}
		if !e.started {
			return malformed()
		}
		return e.chunk(v.Text, nil, nil)
	case core.Usage:
		u, er := usage(&v)
		if er != nil {
			return er
		}
		return e.chunk("", nil, u)
	case core.BlockEnd:
		if v.Index != (core.Index{}) {
			return unsupported("block.index")
		}
	case core.Finish:
		if e.ended {
			return unsupported("finish")
		}
		reason, er := finishToWire(v)
		if er != nil {
			return er
		}
		e.ended = true
		if er = e.chunk("", &reason, nil); er != nil {
			return er
		}
		if er = framing.WriteSSE(e.w, framing.SSEEvent{Data: "[DONE]"}); er != nil {
			return er
		}
		e.done = true
	default:
		return unsupported("event")
	}
	return nil
}
func (e *streamEncoder) chunk(text string, reason *string, u *wireUsage) error {
	v := streamResponse{ID: e.id, Model: e.model, Usage: u}
	ch := streamChoice{Text: text, Index: 0}
	if reason != nil {
		b, _ := json.Marshal(*reason)
		ch.Finish = b
	} else {
		ch.Finish = []byte("null")
	}
	v.Choices = []streamChoice{ch}
	if u != nil {
		v.Choices = []streamChoice{}
	}
	b, er := json.Marshal(v)
	if er != nil {
		return er
	}
	return framing.WriteSSE(e.w, framing.SSEEvent{Data: string(b)})
}
func either(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var _ core.StreamCodec = (*Codec)(nil)
