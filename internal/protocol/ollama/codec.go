// Package ollama translates the Ollama chat protocol without transport concerns.
package ollama

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
)

const maxBytes = 1 << 20

type Codec struct{}

func New() *Codec { return &Codec{} }

var (
	_ core.RequestCodec = (*Codec)(nil)
	_ core.ResultCodec  = (*Codec)(nil)
	_ core.StreamCodec  = (*Codec)(nil)
)

func unsupported(field string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: "Ollama cannot represent this feature", Origin: "gateway"}
}
func invalid(field string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Param: field, Message: "Invalid Ollama chat payload", Origin: "gateway"}
}

// unique walks every JSON object, including objects nested in arrays.
func unique(d *json.Decoder, depth int) error {
	if depth > 128 {
		return invalid("json")
	}
	t, e := d.Token()
	if e != nil {
		return invalid("json")
	}
	x, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch x {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return invalid("json")
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return invalid("json")
			}
			seen[s] = true
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return invalid("json")
		}
	case '[':
		for d.More() {
			if e := unique(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return invalid("json")
		}
	default:
		return invalid("json")
	}
	return nil
}
func parse(raw []byte, out any) error {
	if len(raw) > maxBytes {
		return invalid("json")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if e := unique(d, 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return invalid("json")
	}
	if e := json.Unmarshal(raw, out); e != nil {
		return invalid("json")
	}
	return nil
}
func readBody(ctx context.Context, r io.Reader) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if r == nil {
		return nil, invalid("reader")
	}
	b, e := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	if len(b) > maxBytes {
		return nil, invalid("body")
	}
	return b, nil
}
func emit(ctx context.Context, w io.Writer, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if w == nil {
		return invalid("writer")
	}
	b, e := json.Marshal(v)
	if e != nil {
		return invalid("json")
	}
	b = append(b, '\n')
	if len(b) > maxBytes {
		return invalid("body")
	}
	n, e := w.Write(b)
	if e == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return e
}
func fields(m map[string]json.RawMessage, allowed ...string) error {
	a := map[string]bool{}
	for _, k := range allowed {
		a[k] = true
	}
	for k := range m {
		if !a[k] {
			return unsupported(k)
		}
	}
	return nil
}
func has(m map[string]json.RawMessage, k string) bool { _, ok := m[k]; return ok }
func rawValue(m map[string]json.RawMessage, k string, out any) error {
	b, ok := m[k]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return invalid(k)
	}
	return parse(b, out)
}
func jsonObject(raw []byte, field string) error {
	var m map[string]json.RawMessage
	if e := parse(raw, &m); e != nil {
		return e
	}
	if m == nil {
		return invalid(field)
	}
	return nil
}
func validateCall(raw []byte) error {
	var c map[string]json.RawMessage
	if e := parse(raw, &c); e != nil || c == nil {
		return invalid("tool_call")
	}
	if e := fields(c, "function"); e != nil {
		return e
	}
	var f map[string]json.RawMessage
	if e := rawValue(c, "function", &f); e != nil {
		return e
	}
	if e := fields(f, "name", "arguments"); e != nil {
		return e
	}
	var name string
	if e := rawValue(f, "name", &name); e != nil || name == "" {
		return invalid("tool_call.name")
	}
	var args json.RawMessage
	if e := rawValue(f, "arguments", &args); e != nil {
		return e
	}
	return jsonObject(args, "tool_call.arguments")
}
func validateMessage(raw []byte) error {
	var m map[string]json.RawMessage
	if e := parse(raw, &m); e != nil || m == nil {
		return invalid("message")
	}
	if e := fields(m, "role", "content", "images", "tool_calls", "tool_name"); e != nil {
		return e
	}
	if b, ok := m["tool_calls"]; ok {
		var calls []json.RawMessage
		if e := parse(b, &calls); e != nil {
			return e
		}
		for _, c := range calls {
			if e := validateCall(c); e != nil {
				return e
			}
		}
	}
	return nil
}

// Ollama's wire representation. Its tool call format has no call IDs, so
// canonical calls carrying IDs are rejected rather than silently rewritten.
type optionsWire struct {
	NumPredict *int64   `json:"num_predict,omitempty"`
	Stop       []string `json:"stop,omitempty"`
}
type callFunctionWire struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type functionToolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}
type toolCallWire struct {
	Function callFunctionWire `json:"function"`
}
type messageWire struct {
	Role      string         `json:"role"`
	Content   string         `json:"content,omitempty"`
	Images    []string       `json:"images,omitempty"`
	ToolCalls []toolCallWire `json:"tool_calls,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
}
type responseWire struct {
	Model              string      `json:"model"`
	CreatedAt          string      `json:"created_at,omitempty"`
	Message            messageWire `json:"message"`
	Done               bool        `json:"done"`
	DoneReason         string      `json:"done_reason,omitempty"`
	TotalDuration      *int64      `json:"total_duration,omitempty"`
	LoadDuration       *int64      `json:"load_duration,omitempty"`
	PromptEvalDuration *int64      `json:"prompt_eval_duration,omitempty"`
	EvalDuration       *int64      `json:"eval_duration,omitempty"`
	PromptEvalCount    *int64      `json:"prompt_eval_count,omitempty"`
	EvalCount          *int64      `json:"eval_count,omitempty"`
	Error              string      `json:"error,omitempty"`
}
type toolWire struct {
	Type     string           `json:"type"`
	Function functionToolWire `json:"function"`
}
type requestWire struct {
	Model    string          `json:"model"`
	Messages []messageWire   `json:"messages"`
	Tools    []toolWire      `json:"tools,omitempty"`
	Stream   bool            `json:"stream,omitempty"`
	Options  *optionsWire    `json:"options,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"`
}

func encodeBlock(b core.ContentBlock) (text, image string, call *toolCallWire, e error) {
	switch b.Kind {
	case "text":
		if b.URL != "" || b.MIMEType != "" || b.ID != "" || b.Name != "" || b.Arguments != "" || len(b.Data) > 0 {
			return "", "", nil, unsupported("content")
		}
		return b.Text, "", nil, nil
	case "image":
		if b.URL != "" || b.Text != "" || b.ID != "" || b.Name != "" || b.Arguments != "" || len(b.Data) == 0 {
			return "", "", nil, unsupported("image")
		}
		return "", base64.StdEncoding.EncodeToString(b.Data), nil, nil
	case "tool_call":
		if b.ID != "" || b.Text != "" || b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 || b.Name == "" {
			return "", "", nil, unsupported("tool_call")
		}
		if e = jsonObject([]byte(b.Arguments), "tool_call.arguments"); e != nil {
			return "", "", nil, e
		}
		return "", "", &toolCallWire{Function: callFunctionWire{Name: b.Name, Arguments: json.RawMessage(b.Arguments)}}, nil
	case "tool_result":
		if b.ID != "" || b.URL != "" || b.MIMEType != "" || b.Name != "" || b.Arguments != "" || len(b.Data) > 0 {
			return "", "", nil, unsupported("tool_result")
		}
		return b.Text, "", nil, nil
	default:
		return "", "", nil, unsupported(b.Kind)
	}
}
func encodeMessages(c core.Conversation) ([]messageWire, error) {
	// tool results use Ollama's tool role; the protocol has no call-id member.
	out := make([]messageWire, 0, len(c.Messages))
	for _, m := range c.Messages {
		if m.Role == "" {
			return nil, invalid("messages.role")
		}
		x := messageWire{Role: m.Role}
		for _, b := range m.Content {
			if b.Kind == "tool_result" && m.Role != "tool" {
				return nil, unsupported("tool_result.role")
			}
			t, i, call, e := encodeBlock(b)
			if e != nil {
				return nil, e
			}
			x.Content += t
			if i != "" {
				x.Images = append(x.Images, i)
			}
			if b.Kind == "tool_result" && b.Name != "" {
				x.ToolName = b.Name
			}
			if call != nil {
				if m.Role != "assistant" {
					return nil, unsupported("tool_call.role")
				}
				x.ToolCalls = append(x.ToolCalls, *call)
			}
		}
		out = append(out, x)
	}
	return out, nil
}

func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	b, e := readBody(ctx, r)
	if e != nil {
		return nil, e
	}
	var top map[string]json.RawMessage
	if e = parse(b, &top); e != nil || top == nil {
		return nil, invalid("request")
	}
	if e = fields(top, "model", "messages", "tools", "stream", "options", "format"); e != nil {
		return nil, e
	}
	var q requestWire
	if e = parse(b, &q); e != nil {
		return nil, e
	}
	if q.Model == "" || len(q.Messages) == 0 {
		return nil, invalid("request")
	}
	var rawMessages []json.RawMessage
	if e = rawValue(top, "messages", &rawMessages); e != nil {
		return nil, e
	}
	for _, raw := range rawMessages {
		if e = validateMessage(raw); e != nil {
			return nil, e
		}
	}
	var cvt = core.Conversation{Model: q.Model, Stream: q.Stream}
	for i, m := range q.Messages {
		if m.Role == "" {
			return nil, invalid(fmt.Sprintf("messages[%d].role", i))
		}
		var blocks []core.ContentBlock
		if m.Content != "" {
			blocks = append(blocks, core.ContentBlock{Kind: "text", Text: m.Content})
		}
		for _, im := range m.Images {
			d, de := base64.StdEncoding.DecodeString(im)
			if de != nil {
				return nil, invalid("images")
			}
			blocks = append(blocks, core.ContentBlock{Kind: "image", Data: d, MIMEType: "image/*"})
		}
		for _, call := range m.ToolCalls {
			if call.Function.Name == "" || len(call.Function.Arguments) == 0 {
				return nil, invalid("tool_calls")
			}
			if e = jsonObject(call.Function.Arguments, "tool_calls.arguments"); e != nil {
				return nil, e
			}
			blocks = append(blocks, core.ContentBlock{Kind: "tool_call", Name: call.Function.Name, Arguments: string(call.Function.Arguments)})
		}
		if m.Role == "tool" {
			for j := range blocks {
				if blocks[j].Kind == "text" {
					blocks[j].Kind = "tool_result"
					blocks[j].ID = ""
				}
			}
		}
		if m.Role == "tool" && m.ToolName != "" {
			for j := range blocks {
				if blocks[j].Kind == "tool_result" {
					blocks[j].Name = m.ToolName
				}
			}
		}
		cvt.Messages = append(cvt.Messages, core.Message{Role: m.Role, Content: blocks})
	}
	if raw, ok := top["tools"]; ok {
		var rawTools []json.RawMessage
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, invalid("tools")
		}
		if e = parse(raw, &rawTools); e != nil {
			return nil, e
		}
		for i, rawTool := range rawTools {
			var t toolWire
			if e = parse(rawTool, &t); e != nil {
				return nil, e
			}
			var tm map[string]json.RawMessage
			if e = parse(rawTool, &tm); e != nil {
				return nil, e
			}
			if e = fields(tm, "type", "function"); e != nil {
				return nil, e
			}
			if t.Type != "function" {
				return nil, unsupported("tools.type")
			}
			var fm map[string]json.RawMessage
			fnRaw := tm["function"]
			if e = parse(fnRaw, &fm); e != nil {
				return nil, e
			}
			if e = fields(fm, "name", "description", "parameters"); e != nil {
				return nil, e
			}
			if t.Function.Name == "" || len(t.Function.Parameters) == 0 {
				return nil, invalid(fmt.Sprintf("tools[%d].function", i))
			}
			if e = jsonObject(t.Function.Parameters, "tools.parameters"); e != nil {
				return nil, e
			}
			cvt.Tools = append(cvt.Tools, core.Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: append([]byte(nil), t.Function.Parameters...)})
		}
	}
	if raw, ok := top["options"]; ok {
		var om map[string]json.RawMessage
		if e = parse(raw, &om); e != nil {
			return nil, e
		}
		if om == nil {
			return nil, invalid("options")
		}
	}
	if q.Options != nil {
		if q.Options.NumPredict != nil {
			if *q.Options.NumPredict < 0 {
				return nil, invalid("options.num_predict")
			}
			cvt.MaxOutputTokens = q.Options.NumPredict
		}
		cvt.Stop = append([]string(nil), q.Options.Stop...)
	}
	if len(q.Format) > 0 {
		if bytes.Equal(bytes.TrimSpace(q.Format), []byte(`"json"`)) {
			cvt.StructuredOutputMode = "json_object"
		} else if e = jsonObject(q.Format, "format"); e != nil {
			return nil, e
		} else {
			cvt.StructuredOutputMode = "json_schema"
			cvt.StructuredOutput = append([]byte(nil), q.Format...)
		}
	}
	return cvt, nil
}

func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	q, ok := p.(core.Conversation)
	if e := ctx.Err(); e != nil {
		return e
	}
	if !ok {
		return unsupported("payload")
	}
	if q.Model == "" || len(q.Messages) == 0 {
		return invalid("request")
	}
	if q.MaxOutputTokens != nil && *q.MaxOutputTokens < 0 {
		return invalid("max_output_tokens")
	}
	msgs, e := encodeMessages(q)
	if e != nil {
		return e
	}
	o := requestWire{Model: q.Model, Messages: msgs, Stream: q.Stream}
	for _, t := range q.Tools {
		if t.Name == "" || len(t.Schema) == 0 {
			return invalid("tools")
		}
		if e = jsonObject(t.Schema, "tools.schema"); e != nil {
			return e
		}
		o.Tools = append(o.Tools, toolWire{Type: "function", Function: functionToolWire{Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Schema)}})
	}
	if q.MaxOutputTokens != nil || len(q.Stop) > 0 {
		o.Options = &optionsWire{NumPredict: q.MaxOutputTokens, Stop: q.Stop}
	}
	if q.StructuredOutputName != "" || q.StructuredOutputDescription != "" || q.StructuredOutputStrict != nil {
		return unsupported("structured_output.metadata")
	}
	switch q.StructuredOutputMode {
	case "":
		if len(q.StructuredOutput) > 0 {
			if e = jsonObject(q.StructuredOutput, "format"); e != nil {
				return e
			}
			o.Format = json.RawMessage(q.StructuredOutput)
		}
	case "json_object":
		if len(q.StructuredOutput) > 0 {
			return unsupported("structured_output")
		}
		o.Format = json.RawMessage(`"json"`)
	case "json_schema":
		if len(q.StructuredOutput) == 0 {
			return invalid("structured_output")
		}
		if e = jsonObject(q.StructuredOutput, "format"); e != nil {
			return e
		}
		o.Format = json.RawMessage(q.StructuredOutput)
	default:
		return unsupported("structured_output.mode")
	}
	return emit(ctx, w, o)
}
func validateResponse(m map[string]json.RawMessage) error {
	if raw, ok := m["message"]; ok {
		return validateMessage(raw)
	}
	if m["error"] == nil {
		return invalid("message")
	}
	return nil
}
func normalizeReason(reason string) (string, error) {
	if reason == "" {
		return "stop", nil
	}
	switch reason {
	case "stop", "length", "tool_calls", "content_filter", "error", "stop_sequence":
		return reason, nil
	}
	return "", unsupported("done_reason")
}

func responseResult(v responseWire) (core.GenerationResult, error) {
	if v.Error != "" {
		return core.GenerationResult{}, core.GatewayError{Code: "provider_error", HTTPStatus: 502, Message: "Ollama returned an error", Origin: "ollama"}
	}
	if !v.Done {
		return core.GenerationResult{}, invalid("done")
	}
	if v.PromptEvalCount != nil && *v.PromptEvalCount < 0 || v.EvalCount != nil && *v.EvalCount < 0 {
		return core.GenerationResult{}, invalid("usage")
	}
	var b []core.ContentBlock
	if v.Message.Content != "" {
		kind := "text"
		if v.Message.Role == "tool" {
			kind = "tool_result"
		}
		b = append(b, core.ContentBlock{Kind: kind, Text: v.Message.Content, Name: v.Message.ToolName})
	}
	for _, c := range v.Message.ToolCalls {
		if c.Function.Name == "" || len(c.Function.Arguments) == 0 {
			return core.GenerationResult{}, invalid("message.tool_calls")
		}
		if e := jsonObject(c.Function.Arguments, "message.tool_calls.arguments"); e != nil {
			return core.GenerationResult{}, e
		}
		b = append(b, core.ContentBlock{Kind: "tool_call", Name: c.Function.Name, Arguments: string(c.Function.Arguments)})
	}
	for _, im := range v.Message.Images {
		d, e := base64.StdEncoding.DecodeString(im)
		if e != nil {
			return core.GenerationResult{}, invalid("message.images")
		}
		b = append(b, core.ContentBlock{Kind: "image", Data: d, MIMEType: "image/*"})
	}
	var u *core.Usage
	if v.PromptEvalCount != nil || v.EvalCount != nil {
		u = &core.Usage{Input: v.PromptEvalCount, Output: v.EvalCount, Source: "provider"}
		if v.PromptEvalCount != nil && v.EvalCount != nil {
			x := *v.PromptEvalCount + *v.EvalCount
			u.Total = &x
		}
	}
	reason, e := normalizeReason(v.DoneReason)
	if e != nil {
		return core.GenerationResult{}, e
	}
	return core.GenerationResult{Model: v.Model, Blocks: b, Finish: core.Finish{Status: "completed", Reason: reason}, Usage: u}, nil
}
func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	b, e := readBody(ctx, r)
	if e != nil {
		return nil, e
	}
	var m map[string]json.RawMessage
	if e = parse(b, &m); e != nil || m == nil {
		return nil, invalid("result")
	}
	if e = fields(m, "model", "created_at", "message", "done", "done_reason", "total_duration", "load_duration", "prompt_eval_duration", "eval_duration", "prompt_eval_count", "eval_count", "error"); e != nil {
		return nil, e
	}
	if e = validateResponse(m); e != nil {
		return nil, e
	}
	var v responseWire
	if e = parse(b, &v); e != nil {
		return nil, e
	}
	if !has(m, "done") {
		return nil, invalid("done")
	}
	return responseResult(v)
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	v, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("payload")
	}
	if v.Finish.Status != "" && v.Finish.Status != "completed" || v.Finish.Cancellation != "" {
		return unsupported("finish")
	}
	reason, e := normalizeReason(v.Finish.Reason)
	if e != nil {
		return e
	}
	o := responseWire{Model: v.Model, Done: true, DoneReason: reason, Message: messageWire{Role: "assistant"}}
	for _, b := range v.Blocks {
		switch b.Kind {
		case "text":
			if b.URL != "" || b.MIMEType != "" || b.ID != "" || b.Name != "" || b.Arguments != "" || len(b.Data) > 0 {
				return unsupported("content")
			}
			o.Message.Content += b.Text
		case "image":
			if b.URL != "" || b.Text != "" || b.ID != "" || b.Name != "" || b.Arguments != "" || len(b.Data) == 0 {
				return unsupported("image")
			}
			o.Message.Images = append(o.Message.Images, base64.StdEncoding.EncodeToString(b.Data))
		case "tool_call":
			if b.ID != "" || b.Name == "" || b.Text != "" || b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 {
				return unsupported("tool_call")
			}
			if e := jsonObject([]byte(b.Arguments), "tool_call.arguments"); e != nil {
				return e
			}
			o.Message.ToolCalls = append(o.Message.ToolCalls, toolCallWire{Function: callFunctionWire{Name: b.Name, Arguments: json.RawMessage(b.Arguments)}})
		case "tool_result":
			if b.ID != "" || b.URL != "" || b.MIMEType != "" || b.Arguments != "" || len(b.Data) > 0 {
				return unsupported("tool_result")
			}
			o.Message.Role = "tool"
			o.Message.Content += b.Text
			o.Message.ToolName = b.Name
		default:
			return unsupported("content")
		}
	}
	if v.Usage != nil {
		if v.Usage.Total != nil && (v.Usage.Input == nil || v.Usage.Output == nil) {
			return unsupported("usage.total")
		}
		o.PromptEvalCount, o.EvalCount = v.Usage.Input, v.Usage.Output
	}
	return emit(ctx, w, o)
}

type streamDecoder struct {
	d                                       *framing.NDJSONDecoder
	terminal, started, checked, textStarted bool
	model                                   string
	pending                                 []core.Event
}

func (c *Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if c == nil || r == nil {
		return nil, invalid("decoder")
	}
	return &streamDecoder{d: framing.NewNDJSONDecoder(r, framing.DefaultMaxEventBytes)}, nil
}
func (s *streamDecoder) Next(ctx context.Context) (core.Event, error) {
	if s.terminal {
		if s.checked {
			return nil, io.EOF
		}
		s.checked = true
		if _, e := s.d.Next(ctx); e == io.EOF {
			return nil, io.EOF
		} else if e != nil {
			return nil, e
		}
		return nil, invalid("stream.after_terminal")
	}
	b, e := s.d.Next(ctx)
	if e != nil {
		if e == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, e
	}
	var m map[string]json.RawMessage
	if e = parse(b, &m); e != nil || m == nil {
		return nil, invalid("stream")
	}
	if e = fields(m, "model", "created_at", "message", "done", "done_reason", "total_duration", "load_duration", "prompt_eval_duration", "eval_duration", "prompt_eval_count", "eval_count", "error"); e != nil {
		return nil, e
	}
	if e = validateResponse(m); e != nil {
		return nil, e
	}
	var v responseWire
	if e = parse(b, &v); e != nil {
		return nil, e
	}
	if !has(m, "done") {
		return nil, invalid("done")
	}
	if v.Error != "" {
		s.terminal = true
		return core.StreamError{Error: core.GatewayError{Code: "provider_error", HTTPStatus: 502, Message: "Ollama returned an error", Origin: "ollama"}}, nil
	}
	if !s.started {
		s.started = true
		s.model = v.Model
		s.pending = append(s.pending, core.Start{Model: s.model})
	}
	if v.Done {
		r, e := responseResult(v)
		if e != nil {
			return nil, e
		}
		s.terminal = true
		for _, x := range r.Blocks {
			if x.Kind == "text" {
				s.pending = append(s.pending, core.BlockStart{Kind: "text", Index: core.Index{}}, core.TextDelta{Text: x.Text}, core.BlockEnd{Index: core.Index{}})
			} else {
				s.pending = append(s.pending, core.BlockStart{Kind: "tool_call", Index: core.Index{}}, core.ToolCallStart{Name: x.Name}, core.ToolArgumentsDelta{Fragment: x.Arguments}, core.BlockEnd{})
			}
		}
		if r.Usage != nil {
			s.pending = append(s.pending, *r.Usage)
		}
		s.pending = append(s.pending, r.Finish)
		if len(s.pending) == 0 {
			return nil, io.EOF
		}
		x := s.pending[0]
		s.pending = s.pending[1:]
		return x, nil
	}
	if v.Message.Content != "" {
		if !s.textStarted {
			s.textStarted = true
			s.pending = append(s.pending, core.BlockStart{Kind: "text", Index: core.Index{}})
		}
		if len(v.Message.ToolCalls) > 0 {
			for _, call := range v.Message.ToolCalls {
				if call.Function.Name == "" || len(call.Function.Arguments) == 0 {
					return nil, invalid("tool_calls")
				}
				if e = jsonObject(call.Function.Arguments, "tool_calls.arguments"); e != nil {
					return nil, e
				}
				s.pending = append(s.pending, core.ToolCallStart{Name: call.Function.Name}, core.ToolArgumentsDelta{Fragment: string(call.Function.Arguments)}, core.BlockEnd{})
			}
		}
		s.pending = append(s.pending, core.TextDelta{Text: v.Message.Content})
		x := s.pending[0]
		s.pending = s.pending[1:]
		return x, nil
	}
	if len(v.Message.ToolCalls) > 0 {
		for _, call := range v.Message.ToolCalls {
			if call.Function.Name == "" || len(call.Function.Arguments) == 0 {
				return nil, invalid("tool_calls")
			}
			if e = jsonObject(call.Function.Arguments, "tool_calls.arguments"); e != nil {
				return nil, e
			}
			s.pending = append(s.pending, core.BlockStart{Kind: "tool_call", Index: core.Index{}}, core.ToolCallStart{Name: call.Function.Name}, core.ToolArgumentsDelta{Fragment: string(call.Function.Arguments)}, core.BlockEnd{})
		}
		x := s.pending[0]
		s.pending = s.pending[1:]
		return x, nil
	}
	return s.Next(ctx)
}

type streamEncoder struct {
	w             io.Writer
	started, done bool
	callName      string
	callArgs      strings.Builder
}

func (c *Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if c == nil || w == nil {
		return nil, invalid("encoder")
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
	case core.BlockStart:
		if v.Index != (core.Index{}) || (v.Kind != "text" && v.Kind != "tool_call") {
			return unsupported("block")
		}
	case core.TextDelta:
		if v.Index != (core.Index{}) || !e.started {
			return unsupported("text")
		}
		return emit(ctx, e.w, responseWire{Message: messageWire{Role: "assistant", Content: v.Text}})
	case core.ToolCallStart:
		if v.Index != (core.Index{}) || v.Name == "" || e.callName != "" {
			return unsupported("tool_call")
		}
		e.callName = v.Name
		e.callArgs.Reset()
	case core.ToolArgumentsDelta:
		if v.Index != (core.Index{}) || e.callName == "" {
			return unsupported("tool_arguments")
		}
		e.callArgs.WriteString(v.Fragment)
	case core.BlockEnd:
		if v.Index != (core.Index{}) {
			return unsupported("block.index")
		}
		if e.callName != "" {
			if er := jsonObject([]byte(e.callArgs.String()), "tool_call.arguments"); er != nil {
				return er
			}
			er := emit(ctx, e.w, responseWire{Message: messageWire{Role: "assistant", ToolCalls: []toolCallWire{{Function: callFunctionWire{Name: e.callName, Arguments: json.RawMessage(e.callArgs.String())}}}}})
			e.callName = ""
			e.callArgs.Reset()
			return er
		}
	case core.Usage:
		if v.Source != "" && v.Source != "provider" {
			return unsupported("usage.source")
		}
		if v.Total != nil && (v.Input == nil || v.Output == nil) {
			return unsupported("usage.total")
		}
		if v.Input != nil && *v.Input < 0 || v.Output != nil && *v.Output < 0 {
			return invalid("usage")
		}
		return emit(ctx, e.w, responseWire{PromptEvalCount: v.Input, EvalCount: v.Output, Message: messageWire{Role: "assistant"}})
	case core.StreamError:
		e.done = true
		return emit(ctx, e.w, struct {
			Done  bool   `json:"done"`
			Error string `json:"error"`
		}{true, v.Error.Message})
	case core.Finish:
		if e.callName != "" || v.Cancellation != "" || (v.Status != "" && v.Status != "completed") || v.Reason == "" {
			return unsupported("finish")
		}
		reason, er := normalizeReason(v.Reason)
		if er != nil {
			return er
		}
		e.done = true
		return emit(ctx, e.w, responseWire{Done: true, DoneReason: reason, Message: messageWire{Role: "assistant"}})
	default:
		return unsupported("stream.event")
	}
	return nil
}
