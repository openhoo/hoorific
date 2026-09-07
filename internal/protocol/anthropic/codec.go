package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"hoorific/internal/core"
)

type Codec struct{}

func New() *Codec { return &Codec{} }

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)

func (Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var a requestWire
	if err := readJSON(r, &a); err != nil {
		return nil, err
	}
	return conversationFromWire(a)
}
func (Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, ok := p.(core.Conversation)
	if !ok {
		return unsupported("request")
	}
	a, err := conversationToWire(c)
	if err != nil {
		return err
	}
	return emitJSON(w, a)
}
func (Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var a resultWire
	if err := readJSON(r, &a); err != nil {
		return nil, err
	}
	return resultFromWire(a)
}
func (Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("result")
	}
	a, err := resultToWire(g)
	if err != nil {
		return err
	}
	return emitJSON(w, a)
}
func (Codec) NewDecoder(r io.Reader) (core.EventDecoder, error) {
	if r == nil {
		return nil, unsupported("stream.reader")
	}
	return newStreamDecoder(r), nil
}
func (Codec) NewEncoder(w io.Writer) (core.EventEncoder, error) {
	if w == nil {
		return nil, unsupported("stream.writer")
	}
	return newStreamEncoder(w), nil
}

// Wire types are deliberately closed; strict() rejects provider extensions that
// the core cannot retain.
type requestWire struct {
	Model        string            `json:"model"`
	MaxTokens    *int64            `json:"max_tokens"`
	Messages     []messageWire     `json:"messages"`
	System       json.RawMessage   `json:"system,omitempty"`
	Tools        []toolWire        `json:"tools,omitempty"`
	Stop         []string          `json:"stop_sequences,omitempty"`
	Stream       bool              `json:"stream,omitempty"`
	OutputConfig *outputConfigWire `json:"output_config,omitempty"`
}
type outputConfigWire struct {
	Format outputFormatWire `json:"format"`
}
type outputFormatWire struct {
	Type        string          `json:"type"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}
type messageWire struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type toolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type blockWire struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	Source    *sourceWire     `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}
type sourceWire struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

func conversationToWire(c core.Conversation) (requestWire, error) {
	if err := required(c.Model, "model"); err != nil {
		return requestWire{}, err
	}
	max := c.MaxOutputTokens
	if max == nil {
		v := int64(1024)
		max = &v
	}
	if *max <= 0 {
		return requestWire{}, unsupported("max_output_tokens")
	}
	a := requestWire{Model: c.Model, MaxTokens: max, Stream: c.Stream}
	a.Stop = append([]string(nil), c.Stop...)
	if len(c.System) > 0 {
		var err error
		a.System, err = blocksToJSON(c.System, "system")
		if err != nil {
			return a, err
		}
	}
	for i, m := range c.Messages {
		role := m.Role
		if role == "tool" {
			role = "user"
		}
		if role != "user" && role != "assistant" {
			return a, unsupported(field("messages.role", i))
		}
		for _, b := range m.Content {
			if m.Role == "tool" && b.Kind != "tool_result" {
				return a, unsupported(field("messages", i))
			}
		}
		b, err := blocksToJSON(m.Content, field("messages", i))
		if err != nil {
			return a, err
		}
		a.Messages = append(a.Messages, messageWire{Role: role, Content: b})
	}
	if c.StructuredOutput != nil {
		if c.StructuredOutputMode != "" && c.StructuredOutputMode != "json_schema" {
			return a, unsupported("structured_output.mode")
		}
		if object(c.StructuredOutput) != nil {
			return a, unsupported("structured_output")
		}
		a.OutputConfig = &outputConfigWire{Format: outputFormatWire{Type: "json_schema", Schema: json.RawMessage(c.StructuredOutput), Name: c.StructuredOutputName, Description: c.StructuredOutputDescription, Strict: c.StructuredOutputStrict}}
	} else if c.StructuredOutputName != "" || c.StructuredOutputDescription != "" || c.StructuredOutputMode != "" || c.StructuredOutputStrict != nil {
		return a, unsupported("structured_output")
	}
	return a, nil
}
func conversationFromWire(a requestWire) (core.RequestPayload, error) {
	if err := required(a.Model, "model"); err != nil {
		return nil, err
	}
	if a.MaxTokens == nil || *a.MaxTokens <= 0 {
		return nil, unsupported("max_tokens")
	}
	if len(a.Messages) == 0 {
		return nil, unsupported("messages")
	}
	c := core.Conversation{Model: a.Model, MaxOutputTokens: a.MaxTokens, Stream: a.Stream, Stop: append([]string(nil), a.Stop...)}
	if len(a.System) > 0 {
		b, err := blocksFromJSON(a.System, "system")
		if err != nil {
			return nil, err
		}
		c.System = b
	}
	for i, m := range a.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, unsupported(field("messages.role", i))
		}
		b, err := blocksFromJSON(m.Content, field("messages", i))
		if err != nil {
			return nil, err
		}
		role := m.Role
		if role == "user" && len(b) > 0 {
			allTool := true
			for _, z := range b {
				if z.Kind != "tool_result" {
					allTool = false
					break
				}
			}
			if allTool {
				role = "tool"
			}
		}
		c.Messages = append(c.Messages, core.Message{Role: role, Content: b})
	}
	for i, t := range a.Tools {
		if err := required(t.Name, field("tools.name", i)); err != nil {
			return nil, err
		}
		if len(t.InputSchema) == 0 || object(t.InputSchema) != nil {
			return nil, unsupported(field("tools.input_schema", i))
		}
		c.Tools = append(c.Tools, core.Tool{Name: t.Name, Description: t.Description, Schema: append([]byte(nil), t.InputSchema...)})
	}
	if a.OutputConfig != nil {
		if a.OutputConfig.Format.Type != "json_schema" || len(a.OutputConfig.Format.Schema) == 0 || object(a.OutputConfig.Format.Schema) != nil {
			return nil, unsupported("output_config.format")
		}
		c.StructuredOutput = append([]byte(nil), a.OutputConfig.Format.Schema...)
		c.StructuredOutputName = a.OutputConfig.Format.Name
		c.StructuredOutputDescription = a.OutputConfig.Format.Description
		c.StructuredOutputMode = "json_schema"
		c.StructuredOutputStrict = a.OutputConfig.Format.Strict
	}
	return c, nil
}
func blocksToJSON(in []core.ContentBlock, path string) (json.RawMessage, error) {
	out := make([]blockWire, 0, len(in))
	for i, b := range in {
		x, err := blockToWire(b, field(path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	v, e := json.Marshal(out)
	return v, e
}
func blocksFromJSON(raw json.RawMessage, path string) ([]core.ContentBlock, error) {
	var s string
	if err := strict(raw, &s); err == nil {
		return []core.ContentBlock{{Kind: "text", Text: s}}, nil
	}
	var blocks []json.RawMessage
	if err := strict(raw, &blocks); err != nil {
		return nil, err
	}
	if blocks == nil {
		return nil, unsupported(path)
	}
	out := make([]core.ContentBlock, 0, len(blocks))
	for i, r := range blocks {
		x, e := blockFromRaw(r, field(path, i))
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, nil
}
func blockType(raw json.RawMessage) (string, error) {
	var m map[string]json.RawMessage
	if err := strict(raw, &m); err != nil {
		return "", err
	}
	var t string
	if v, ok := m["type"]; !ok || strict(v, &t) != nil {
		return "", unsupported("type")
	}
	return t, nil
}
func blockToWire(b core.ContentBlock, path string) (blockWire, error) {
	switch b.Kind {
	case "text":
		if b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 || b.ID != "" || b.Name != "" || b.Arguments != "" {
			return blockWire{}, unsupported(path)
		}
		return blockWire{Type: "text", Text: &b.Text}, nil
	case "image", "document":
		if b.ID != "" || b.Name != "" || b.Arguments != "" {
			return blockWire{}, unsupported(path)
		}
		s := sourceWire{}
		if b.URL != "" {
			if len(b.Data) > 0 {
				return blockWire{}, unsupported(path + ".source")
			}
			s = sourceWire{Type: "url", URL: b.URL}
		} else if len(b.Data) > 0 {
			if b.MIMEType == "" {
				return blockWire{}, unsupported(path + ".mime_type")
			}
			s = sourceWire{Type: "base64", MediaType: b.MIMEType, Data: base64.StdEncoding.EncodeToString(b.Data)}
		} else {
			return blockWire{}, unsupported(path + ".source")
		}
		return blockWire{Type: b.Kind, Source: &s}, nil
	case "tool_call":
		if b.ID == "" || b.Name == "" || len(b.Arguments) == 0 || b.Text != "" || b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 {
			return blockWire{}, unsupported(path)
		}
		if err := object([]byte(b.Arguments)); err != nil {
			return blockWire{}, unsupported(path + ".input")
		}
		return blockWire{Type: "tool_use", ID: b.ID, Name: b.Name, Input: json.RawMessage(b.Arguments)}, nil
	case "tool_result":
		if b.ID == "" || b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 || b.Name != "" || b.Arguments != "" {
			return blockWire{}, unsupported(path + ".tool_use_id")
		}
		content := json.RawMessage(nil)
		if b.Text != "" {
			content, _ = json.Marshal(b.Text)
		} else {
			content, _ = json.Marshal([]blockWire{{Type: "text", Text: &b.Text}})
		}
		return blockWire{Type: "tool_result", ToolUseID: b.ID, Content: content}, nil
	default:
		return blockWire{}, unsupported(path + ".kind")
	}
}
func blockFromRaw(raw json.RawMessage, path string) (core.ContentBlock, error) {
	typ, err := blockType(raw)
	if err != nil {
		return core.ContentBlock{}, err
	}
	if typ == "" {
		return core.ContentBlock{}, unsupported(path + ".type")
	}
	switch typ {
	case "text":
		var x struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := strict(raw, &x); err != nil {
			return core.ContentBlock{}, err
		}
		if x.Text == nil {
			return core.ContentBlock{}, unsupported(path + ".text")
		}
		return core.ContentBlock{Kind: "text", Text: *x.Text}, nil
	case "image", "document":
		var x struct {
			Type   string     `json:"type"`
			Source sourceWire `json:"source"`
		}
		if err := strict(raw, &x); err != nil {
			return core.ContentBlock{}, err
		}
		if x.Source.Type == "url" && x.Source.URL != "" {
			return core.ContentBlock{Kind: typ, URL: x.Source.URL}, nil
		}
		if x.Source.Type == "base64" && x.Source.Data != "" && x.Source.MediaType != "" {
			data, e := base64.StdEncoding.DecodeString(x.Source.Data)
			if e != nil {
				return core.ContentBlock{}, unsupported(path + ".source.data")
			}
			return core.ContentBlock{Kind: typ, MIMEType: x.Source.MediaType, Data: data}, nil
		}
		return core.ContentBlock{}, unsupported(path + ".source")
	case "tool_use":
		var x struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := strict(raw, &x); err != nil {
			return core.ContentBlock{}, err
		}
		if x.ID == "" || x.Name == "" || len(x.Input) == 0 {
			return core.ContentBlock{}, unsupported(path)
		}
		if err := object(x.Input); err != nil {
			return core.ContentBlock{}, err
		}
		return core.ContentBlock{Kind: "tool_call", ID: x.ID, Name: x.Name, Arguments: string(x.Input)}, nil
	case "tool_result":
		var x struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := strict(raw, &x); err != nil {
			return core.ContentBlock{}, err
		}
		if x.ToolUseID == "" {
			return core.ContentBlock{}, unsupported(path)
		}
		text, err := toolResultText(x.Content, path+".content")
		if err != nil {
			return core.ContentBlock{}, err
		}
		return core.ContentBlock{Kind: "tool_result", ID: x.ToolUseID, Text: text}, nil
	default:
		return core.ContentBlock{}, unsupported(path + ".type")
	}
}
func toolResultText(raw json.RawMessage, path string) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if strict(raw, &s) == nil {
		return s, nil
	}
	var bs []json.RawMessage
	if err := strict(raw, &bs); err != nil {
		return "", err
	}
	var out strings.Builder
	for i, r := range bs {
		b, e := blockFromRaw(r, field(path, i))
		if e != nil {
			return "", e
		}
		if b.Kind != "text" {
			return "", unsupported(field(path, i))
		}
		out.WriteString(b.Text)
	}
	return out.String(), nil
}
