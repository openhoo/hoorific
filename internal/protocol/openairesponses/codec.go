package openairesponses

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"hoorific/internal/core"
	"io"
	"strings"
)

type Codec struct{}

func New() *Codec { return &Codec{} }

type wireInput struct {
	Role      string        `json:"role,omitempty"`
	Content   []wireContent `json:"content,omitempty"`
	Type      string        `json:"type,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	ID        string        `json:"id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	Output    any           `json:"output,omitempty"`
}
type wireContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}
type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}
type wireText struct {
	Format *wireFormat `json:"format,omitempty"`
}
type wireFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}
type requestWire struct {
	Model  string      `json:"model"`
	Store  *bool       `json:"store"`
	Input  []wireInput `json:"input"`
	Max    *int64      `json:"max_output_tokens,omitempty"`
	Stream bool        `json:"stream,omitempty"`
	Tools  []wireTool  `json:"tools,omitempty"`
	Text   *wireText   `json:"text,omitempty"`
}

func contentToWire(b core.ContentBlock) (wireContent, error) {
	switch b.Kind {
	case "text":
		return wireContent{Type: "input_text", Text: b.Text}, nil
	case "image":
		if b.URL != "" {
			return wireContent{Type: "input_image", ImageURL: b.URL}, nil
		}
		if len(b.Data) > 0 {
			return wireContent{Type: "input_image", ImageURL: "data:" + b.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(b.Data)}, nil
		}
		return wireContent{}, unsupported("input.image")
	case "document":
		if b.URL != "" {
			return wireContent{Type: "input_file", FileURL: b.URL}, nil
		}
		if len(b.Data) > 0 {
			return wireContent{Type: "input_file", FileData: "data:" + b.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(b.Data), Filename: b.Name}, nil
		}
		return wireContent{}, unsupported("input.document")
	default:
		return wireContent{}, unsupported("input." + b.Kind)
	}
}
func messageToWire(m core.Message) ([]wireInput, error) {
	if m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "tool" {
		return nil, unsupported("input.role")
	}
	var out []wireInput
	var regular []wireContent
	for _, b := range m.Content {
		switch b.Kind {
		case "tool_call":
			if len(regular) > 0 || len(m.Content) != 1 {
				return nil, unsupported("input.tool_call.order")
			}
			out = append(out, wireInput{Type: "function_call", CallID: b.ID, Name: b.Name, Arguments: b.Arguments})
		case "tool_result":
			if len(regular) > 0 || len(m.Content) != 1 {
				return nil, unsupported("input.tool_result.order")
			}
			var text string
			for _, c := range m.Content {
				if c.Kind != "tool_result" {
					continue
				}
				text += c.Text
			}
			out = append(out, wireInput{Type: "function_call_output", CallID: b.ID, Output: text})
		default:
			c, e := contentToWire(b)
			if e != nil {
				return nil, e
			}
			regular = append(regular, c)
		}
	}
	if len(regular) > 0 {
		out = append(out, wireInput{Role: m.Role, Content: regular})
	}
	return out, nil
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	q, ok := p.(core.Conversation)
	if !ok {
		return unsupported("payload")
	}
	if q.Model == "" {
		return unsupported("model")
	}
	rw := requestWire{Model: q.Model, Store: new(bool), Max: q.MaxOutputTokens, Stream: q.Stream}
	*rw.Store = false
	if len(q.Stop) > 0 {
		return unsupported("stop")
	}
	for _, b := range q.System {
		xs, e := messageToWire(core.Message{Role: "system", Content: []core.ContentBlock{b}})
		if e != nil {
			return e
		}
		rw.Input = append(rw.Input, xs...)
	}
	for _, m := range q.Messages {
		xs, e := messageToWire(m)
		if e != nil {
			return e
		}
		rw.Input = append(rw.Input, xs...)
	}
	for _, t := range q.Tools {
		var schema json.RawMessage = t.Schema
		if len(schema) == 0 {
			schema = []byte("{}")
		}
		var schemaValue any
		if e := strict(schema, &schemaValue); e != nil {
			return unsupported("tools.parameters")
		}
		rw.Tools = append(rw.Tools, wireTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schema})
	}
	if len(q.StructuredOutput) > 0 || q.StructuredOutputMode != "" || q.StructuredOutputName != "" || q.StructuredOutputDescription != "" || q.StructuredOutputStrict != nil {
		name := q.StructuredOutputName
		mode := q.StructuredOutputMode
		if mode == "" {
			mode = "json_schema"
		}
		if mode != "json_schema" && mode != "json_object" {
			return unsupported("text.format.mode")
		}
		if mode == "json_schema" && name == "" {
			return unsupported("text.format.name")
		}
		f := &wireFormat{Type: mode, Name: name, Description: q.StructuredOutputDescription}
		if q.StructuredOutputStrict != nil {
			v := *q.StructuredOutputStrict
			f.Strict = &v
		}
		if mode == "json_schema" {
			if len(q.StructuredOutput) == 0 {
				return unsupported("text.format.schema")
			}
			var schema any
			if e := strict(q.StructuredOutput, &schema); e != nil {
				return unsupported("text.format.schema")
			}
			f.Schema = q.StructuredOutput
		}
		rw.Text = &wireText{Format: f}
	}
	return encode(w, rw)
}
func parseContent(v object) (core.ContentBlock, error) {
	t, e := str(v, "type")
	if e != nil {
		return core.ContentBlock{}, e
	}
	switch t {
	case "input_text", "output_text":
		if e := fields(v, "type", "text"); e != nil {
			return core.ContentBlock{}, e
		}
		var s string
		if e = get(v, "text", &s, true); e != nil {
			return core.ContentBlock{}, e
		}
		return core.ContentBlock{Kind: "text", Text: s}, nil
	case "input_image":
		if e := fields(v, "type", "image_url"); e != nil {
			return core.ContentBlock{}, e
		}
		var s string
		if e = get(v, "image_url", &s, true); e != nil {
			return core.ContentBlock{}, e
		}
		return core.ContentBlock{Kind: "image", URL: s}, nil
	case "input_file":
		if e := fields(v, "type", "file_url", "file_data", "filename"); e != nil {
			return core.ContentBlock{}, e
		}
		var id string
		if e = get(v, "file_url", &id, false); e == nil && id != "" {
			return core.ContentBlock{Kind: "document", URL: id}, nil
		}
		var fd, fn string
		_ = get(v, "file_data", &fd, false)
		_ = get(v, "filename", &fn, false)
		if strings.HasPrefix(fd, "data:") {
			semi := strings.Index(fd, ";base64,")
			if semi > 5 {
				data, e := base64.StdEncoding.DecodeString(fd[semi+8:])
				if e == nil {
					return core.ContentBlock{Kind: "document", Name: fn, MIMEType: fd[5:semi], Data: data}, nil
				}
			}
		}
		return core.ContentBlock{}, unsupported("input_file")
	default:
		return core.ContentBlock{}, unsupported("input.content.type")
	}
}
func parseInput(b json.RawMessage) ([]core.Message, error) {
	var arr []json.RawMessage
	if e := strict(b, &arr); e == nil {
		var out []core.Message
		for _, x := range arr {
			o, e := obj(x)
			if e != nil {
				return nil, e
			}
			t := typ(o)
			switch t {
			case "function_call":
				if e := fields(o, "type", "id", "call_id", "name", "arguments", "status"); e != nil {
					return nil, e
				}
				var id, name, args string
				if e = get(o, "call_id", &id, true); e != nil {
					return nil, e
				}
				if e = get(o, "name", &name, true); e != nil {
					return nil, e
				}
				if e = get(o, "arguments", &args, true); e != nil {
					return nil, e
				}
				out = append(out, core.Message{Role: "assistant", Content: []core.ContentBlock{{Kind: "tool_call", ID: id, Name: name, Arguments: args}}})
			case "function_call_output":
				if e := fields(o, "type", "id", "call_id", "output"); e != nil {
					return nil, e
				}
				var id, outText string
				if e = get(o, "call_id", &id, true); e != nil {
					return nil, e
				}
				if e = get(o, "output", &outText, true); e != nil {
					return nil, e
				}
				out = append(out, core.Message{Role: "tool", Content: []core.ContentBlock{{Kind: "tool_result", ID: id, Text: outText}}})
			default:
				if e := fields(o, "role", "content", "type"); e != nil {
					return nil, e
				}
				if t != "" && t != "message" {
					return nil, unsupported("input.type")
				}
				role, e := str(o, "role")
				if e != nil {
					return nil, e
				}
				if role != "user" && role != "assistant" && role != "system" {
					return nil, unsupported("input.role")
				}
				var cs []json.RawMessage
				if e = get(o, "content", &cs, true); e != nil {
					return nil, e
				}
				m := core.Message{Role: role}
				for _, cb := range cs {
					co, e := obj(cb)
					if e != nil {
						return nil, e
					}
					b, e := parseContent(co)
					if e != nil {
						return nil, e
					}
					m.Content = append(m.Content, b)
				}
				out = append(out, m)
			}
		}
		return out, nil
	}
	var s string
	if e := json.Unmarshal(b, &s); e != nil {
		return nil, unsupported("input")
	}
	return []core.Message{{Role: "user", Content: []core.ContentBlock{{Kind: "text", Text: s}}}}, nil
}
func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	var m object
	if e := load(r, &m); e != nil {
		return nil, e
	}
	if e := fields(m, "model", "store", "input", "max_output_tokens", "stream", "tools", "text"); e != nil {
		return nil, e
	}
	var q core.Conversation
	var e error
	q.Model, e = str(m, "model")
	if e != nil {
		return nil, e
	}
	sb, ok := m["store"]
	if !ok {
		return nil, unsupported("store")
	}
	var store bool
	if e = json.Unmarshal(sb, &store); e != nil || store {
		return nil, unsupported("store")
	}
	in, ok := m["input"]
	if !ok {
		return nil, unsupported("input")
	}
	msgs, e := parseInput(in)
	if e != nil {
		return nil, e
	}
	for _, msg := range msgs {
		if msg.Role == "system" {
			q.System = append(q.System, msg.Content...)
		} else {
			q.Messages = append(q.Messages, msg)
		}
	}
	if b := m["max_output_tokens"]; b != nil {
		if e = json.Unmarshal(b, &q.MaxOutputTokens); e != nil || q.MaxOutputTokens == nil || *q.MaxOutputTokens < 0 {
			return nil, unsupported("max_output_tokens")
		}
	}
	if b := m["stream"]; b != nil {
		if e = json.Unmarshal(b, &q.Stream); e != nil {
			return nil, unsupported("stream")
		}
	}
	if b := m["tools"]; b != nil {
		var ts []object
		if e = json.Unmarshal(b, &ts); e != nil {
			return nil, unsupported("tools")
		}
		for _, x := range ts {
			if e := fields(x, "type", "name", "description", "parameters"); e != nil {
				return nil, e
			}
			if typ(x) != "function" {
				return nil, unsupported("tools.type")
			}
			var name, desc string
			_ = get(x, "name", &name, true)
			_ = get(x, "description", &desc, false)
			var schema json.RawMessage
			if e = get(x, "parameters", &schema, true); e != nil {
				return nil, e
			}
			q.Tools = append(q.Tools, core.Tool{Name: name, Description: desc, Schema: schema})
		}
	}
	if b := m["text"]; b != nil {
		var tx object
		var e error
		tx, e = obj(b)
		if e != nil {
			return nil, e
		}
		if e = fields(tx, "format"); e != nil {
			return nil, e
		}
		fb, ok := tx["format"]
		if !ok {
			return nil, unsupported("text.format")
		}
		var f object
		f, e = obj(fb)
		if e != nil {
			return nil, e
		}
		if e = fields(f, "type", "name", "description", "schema", "strict"); e != nil {
			return nil, e
		}
		mode := typ(f)
		if mode != "json_schema" && mode != "json_object" {
			return nil, unsupported("text.format.type")
		}
		q.StructuredOutputMode = mode
		_ = get(f, "name", &q.StructuredOutputName, false)
		_ = get(f, "description", &q.StructuredOutputDescription, false)
		var strictMode bool
		if _, ok := f["strict"]; ok {
			if e = get(f, "strict", &strictMode, true); e != nil {
				return nil, e
			}
			q.StructuredOutputStrict = &strictMode
		}
		if mode == "json_schema" {
			var schema json.RawMessage
			if e = get(f, "schema", &schema, true); e != nil {
				return nil, e
			}
			if e := strict(schema, &map[string]any{}); e != nil {
				return nil, e
			}
			q.StructuredOutput = schema
		}
	}
	return q, nil
}

var _ core.RequestCodec = (*Codec)(nil)
