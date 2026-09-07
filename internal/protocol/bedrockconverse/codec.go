package bedrockconverse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"hoorific/internal/core"
)

type Codec struct{}

func New() *Codec { return &Codec{} }

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
var _ core.StreamCodec = (*Codec)(nil)

const maxJSON = 16 << 20

func unsupported(field string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: "unsupported Converse field", Origin: "gateway"}
}
func invalid(field string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Param: field, Message: "invalid Converse payload", Origin: "gateway"}
}

// Walk tokens before unmarshalling so duplicate keys are rejected even inside opaque tool JSON.
func unique(d *json.Decoder, depth int) error {
	if depth > 128 {
		return invalid("json")
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return invalid("duplicate_key")
			}
			seen[s] = true
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := unique(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return invalid("json")
	}
	_, e = d.Token()
	return e
}
func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := unique(d, 0); e != nil {
		return invalid("json")
	}
	if _, e := d.Token(); e != io.EOF {
		return invalid("json")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		if strings.HasPrefix(e.Error(), "json: unknown field ") {
			return unsupported(strings.Trim(strings.TrimPrefix(e.Error(), "json: unknown field "), "`\""))
		}
		return invalid("json")
	}
	return nil
}
func load(ctx context.Context, r io.Reader, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := io.ReadAll(io.LimitReader(r, maxJSON+1))
	if e != nil {
		return e
	}
	if len(b) > maxJSON {
		return invalid("body")
	}
	return strict(b, v)
}
func save(ctx context.Context, w io.Writer, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return json.NewEncoder(w).Encode(v)
}
func object(b []byte) error {
	var v map[string]json.RawMessage
	if e := strict(b, &v); e != nil {
		return e
	}
	if v == nil {
		return invalid("json_object")
	}
	return nil
}

type imageContent struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}
type documentContent struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}
type block struct {
	Text       *string          `json:"text,omitempty"`
	Image      *imageContent    `json:"image,omitempty"`
	Document   *documentContent `json:"document,omitempty"`
	ToolUse    *toolUse         `json:"toolUse,omitempty"`
	ToolResult *toolResult      `json:"toolResult,omitempty"`
}
type toolUse struct {
	ID    string          `json:"toolUseId"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}
type toolResult struct {
	ID      string          `json:"toolUseId"`
	Content []resultContent `json:"content"`
}
type resultContent struct {
	Text *string         `json:"text,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
}
type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}
type specification struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema struct {
		JSON json.RawMessage `json:"json"`
	} `json:"inputSchema"`
}
type tool struct {
	Spec specification `json:"toolSpec"`
}
type toolConfig struct {
	Tools []tool `json:"tools"`
}
type inference struct {
	Max  *int64   `json:"maxTokens,omitempty"`
	Stop []string `json:"stopSequences,omitempty"`
}
type request struct {
	System    []block     `json:"system,omitempty"`
	Messages  []message   `json:"messages"`
	Tools     *toolConfig `json:"toolConfig,omitempty"`
	Inference *inference  `json:"inferenceConfig,omitempty"`
}
type usage struct {
	Input  *int64 `json:"inputTokens,omitempty"`
	Output *int64 `json:"outputTokens,omitempty"`
	Total  *int64 `json:"totalTokens,omitempty"`
}
type metrics struct {
	Latency *int64 `json:"latencyMs,omitempty"`
}
type response struct {
	Output struct {
		Message message `json:"message"`
	} `json:"output"`
	Stop    string   `json:"stopReason"`
	Usage   *usage   `json:"usage,omitempty"`
	Metrics *metrics `json:"metrics,omitempty"`
}

func decodeBlocks(bs []block, role string) ([]core.ContentBlock, error) {
	out := make([]core.ContentBlock, 0, len(bs))
	for _, b := range bs {
		n := 0
		if b.Text != nil {
			n++
		}
		if b.Image != nil {
			n++
		}
		if b.Document != nil {
			n++
		}
		if b.ToolUse != nil {
			n++
		}
		if b.ToolResult != nil {
			n++
		}
		if n != 1 {
			return nil, invalid("content")
		}
		switch {
		case b.Text != nil:
			out = append(out, core.ContentBlock{Kind: "text", Text: *b.Text})
		case b.Image != nil:
			if role == "system" {
				return nil, unsupported("system.image")
			}
			if b.Image.Source.Bytes == "" {
				return nil, invalid("image.source")
			}
			raw, e := base64.StdEncoding.DecodeString(b.Image.Source.Bytes)
			if e != nil {
				return nil, invalid("image.bytes")
			}
			out = append(out, core.ContentBlock{Kind: "image", MIMEType: "image/" + b.Image.Format, Data: raw})
		case b.Document != nil:
			if role == "system" {
				return nil, unsupported("system.document")
			}
			if b.Document.Source.Bytes == "" || b.Document.Name == "" {
				return nil, invalid("document.source")
			}
			raw, e := base64.StdEncoding.DecodeString(b.Document.Source.Bytes)
			if e != nil {
				return nil, invalid("document.bytes")
			}
			out = append(out, core.ContentBlock{Kind: "document", MIMEType: "application/" + b.Document.Format, Name: b.Document.Name, Data: raw})
		case b.ToolUse != nil:
			t := b.ToolUse
			if role != "assistant" || t.ID == "" || t.Name == "" {
				return nil, invalid("toolUse")
			}
			if e := object(t.Input); e != nil {
				return nil, e
			}
			out = append(out, core.ContentBlock{Kind: "tool_call", ID: t.ID, Name: t.Name, Arguments: string(t.Input)})
		case b.ToolResult != nil:
			t := b.ToolResult
			if role != "user" || t.ID == "" || len(t.Content) != 1 {
				return nil, unsupported("toolResult.content")
			}
			c := t.Content[0]
			if c.Text == nil || len(c.JSON) != 0 {
				return nil, unsupported("toolResult.content.json")
			}
			out = append(out, core.ContentBlock{Kind: "tool_result", ID: t.ID, Text: *c.Text})
		}
	}
	return out, nil
}
func encodeBlocks(bs []core.ContentBlock, role string) ([]block, error) {
	out := make([]block, 0, len(bs))
	for _, b := range bs {
		if b.URL != "" {
			return nil, unsupported("content.url")
		}
		switch b.Kind {
		case "text":
			if b.ID != "" || b.Name != "" || b.Arguments != "" || b.MIMEType != "" || len(b.Data) != 0 {
				return nil, unsupported("text")
			}
			s := b.Text
			out = append(out, block{Text: &s})
		case "image":
			if role == "system" {
				return nil, unsupported("system.image")
			}
			if b.Text != "" || len(b.Data) == 0 || b.MIMEType == "" || b.ID != "" || b.Name != "" || b.Arguments != "" {
				return nil, invalid("image")
			}
			const p = "image/"
			if !strings.HasPrefix(b.MIMEType, p) {
				return nil, unsupported("image.mimeType")
			}
			x := imageContent{Format: strings.TrimPrefix(b.MIMEType, p)}
			x.Source.Bytes = base64.StdEncoding.EncodeToString(b.Data)
			out = append(out, block{Image: &x})
		case "document":
			if role == "system" {
				return nil, unsupported("system.document")
			}
			if b.Text != "" || len(b.Data) == 0 || b.MIMEType == "" || b.Name == "" || b.ID != "" || b.Arguments != "" {
				return nil, invalid("document")
			}
			const p = "application/"
			if !strings.HasPrefix(b.MIMEType, p) {
				return nil, unsupported("document.mimeType")
			}
			x := documentContent{Format: strings.TrimPrefix(b.MIMEType, p), Name: b.Name}
			x.Source.Bytes = base64.StdEncoding.EncodeToString(b.Data)
			out = append(out, block{Document: &x})
		case "tool_call":
			if role != "assistant" || b.ID == "" || b.Name == "" || b.Text != "" || b.MIMEType != "" || len(b.Data) != 0 {
				return nil, invalid("tool_call")
			}
			if e := object([]byte(b.Arguments)); e != nil {
				return nil, e
			}
			out = append(out, block{ToolUse: &toolUse{ID: b.ID, Name: b.Name, Input: json.RawMessage(b.Arguments)}})
		case "tool_result":
			if role != "user" || b.ID == "" || b.Name != "" || b.Arguments != "" || b.MIMEType != "" || len(b.Data) != 0 {
				return nil, unsupported("tool_result")
			}
			s := b.Text
			out = append(out, block{ToolResult: &toolResult{ID: b.ID, Content: []resultContent{{Text: &s}}}})
		default:
			return nil, unsupported("content." + b.Kind)
		}
	}
	return out, nil
}
func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	var n request
	if e := load(ctx, r, &n); e != nil {
		return nil, e
	}
	v := core.Conversation{}
	var e error
	if v.System, e = decodeBlocks(n.System, "system"); e != nil {
		return nil, e
	}
	if len(n.Messages) == 0 {
		return nil, invalid("messages")
	}
	for _, m := range n.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, invalid("role")
		}
		bs, e := decodeBlocks(m.Content, m.Role)
		if e != nil {
			return nil, e
		}
		if len(bs) == 0 {
			return nil, invalid("content")
		}
		v.Messages = append(v.Messages, core.Message{Role: m.Role, Content: bs})
	}
	if n.Inference != nil {
		v.MaxOutputTokens = n.Inference.Max
		v.Stop = n.Inference.Stop
		if v.MaxOutputTokens != nil && *v.MaxOutputTokens <= 0 {
			return nil, invalid("maxTokens")
		}
	}
	if n.Tools != nil {
		for _, t := range n.Tools.Tools {
			s := t.Spec
			if s.Name == "" {
				return nil, invalid("toolSpec.name")
			}
			if e := object(s.InputSchema.JSON); e != nil {
				return nil, e
			}
			v.Tools = append(v.Tools, core.Tool{Name: s.Name, Description: s.Description, Schema: s.InputSchema.JSON})
		}
	}
	return v, nil
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	v, ok := p.(core.Conversation)
	if !ok {
		return unsupported("payload")
	}
	if len(v.StructuredOutput) != 0 || v.StructuredOutputName != "" || v.StructuredOutputDescription != "" || v.StructuredOutputMode != "" || v.StructuredOutputStrict != nil {
		return unsupported("structured_output")
	}
	n := request{}
	var e error
	if n.System, e = encodeBlocks(v.System, "system"); e != nil {
		return e
	}
	if len(v.Messages) == 0 {
		return invalid("messages")
	}
	for _, m := range v.Messages {
		role := m.Role
		if role == "tool" {
			role = "user"
			if len(m.Content) == 0 {
				return invalid("tool.content")
			}
			for _, b := range m.Content {
				if b.Kind != "tool_result" {
					return unsupported("tool.role")
				}
			}
		} else if role != "user" && role != "assistant" {
			return unsupported("role")
		}
		bs, e := encodeBlocks(m.Content, role)
		if e != nil {
			return e
		}
		if len(bs) == 0 {
			return invalid("content")
		}
		n.Messages = append(n.Messages, message{Role: role, Content: bs})
	}
	if v.MaxOutputTokens != nil || len(v.Stop) > 0 {
		if v.MaxOutputTokens != nil && *v.MaxOutputTokens <= 0 {
			return invalid("maxTokens")
		}
		n.Inference = &inference{Max: v.MaxOutputTokens, Stop: v.Stop}
	}
	if len(v.Tools) > 0 {
		n.Tools = &toolConfig{}
		for _, t := range v.Tools {
			if t.Name == "" {
				return invalid("tool.name")
			}
			if e := object(t.Schema); e != nil {
				return e
			}
			s := specification{Name: t.Name, Description: t.Description}
			s.InputSchema.JSON = t.Schema
			n.Tools.Tools = append(n.Tools.Tools, tool{Spec: s})
		}
	}
	return save(ctx, w, n)
}
func finish(reason string) (core.Finish, error) {
	s := "completed"
	canonical := reason
	switch reason {
	case "end_turn":
		canonical = "stop"
	case "stop_sequence":
		canonical = "stop_sequence"
	case "tool_use":
		canonical = "tool_calls"
	case "max_tokens":
		canonical = "length"
	case "guardrail_intervened", "content_filtered":
		canonical = "content_filter"
	default:
		return core.Finish{}, unsupported("stopReason")
	}
	if canonical == "tool_calls" {
		s = "tool_calls"
	}
	if canonical == "length" {
		s = "length"
	}
	if canonical == "content_filter" {
		s = "content_filter"
	}
	return core.Finish{Status: s, Reason: canonical}, nil
}
func stop(f core.Finish) (string, error) {
	if f.Cancellation != "" || f.Reason == "error" {
		return "", unsupported("cancellation")
	}
	switch f.Reason {
	case "stop", "":
		if f.Reason == "stop" {
			return "end_turn", nil
		}
	case "stop_sequence":
		return "stop_sequence", nil
	case "tool_calls":
		return "tool_use", nil
	case "length":
		return "max_tokens", nil
	case "content_filter":
		return "content_filtered", nil
	}
	switch f.Status {
	case "", "completed":
		return "end_turn", nil
	case "tool_calls":
		return "tool_use", nil
	case "length":
		return "max_tokens", nil
	case "content_filter":
		return "content_filtered", nil
	}
	return "", unsupported("finish")
}
func decodeUsage(u *usage) (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	for _, n := range []*int64{u.Input, u.Output, u.Total} {
		if n != nil && *n < 0 {
			return nil, invalid("usage")
		}
	}
	return &core.Usage{Input: u.Input, Output: u.Output, Total: u.Total, Source: "provider"}, nil
}
func encodeUsage(u *core.Usage) (*usage, error) {
	if u == nil {
		return nil, nil
	}
	n := &usage{Input: u.Input, Output: u.Output, Total: u.Total}
	_, e := decodeUsage(n)
	return n, e
}
func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var n response
	if e := load(ctx, r, &n); e != nil {
		return nil, e
	}
	if n.Output.Message.Role != "assistant" {
		return nil, invalid("role")
	}
	bs, e := decodeBlocks(n.Output.Message.Content, "assistant")
	if e != nil {
		return nil, e
	}
	f, e := finish(n.Stop)
	if e != nil {
		return nil, e
	}
	u, e := decodeUsage(n.Usage)
	if e != nil {
		return nil, e
	}
	return core.GenerationResult{Blocks: bs, Finish: f, Usage: u}, nil
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	v, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("payload")
	}
	if v.ID != "" || v.Model != "" {
		return unsupported("result.identity")
	}
	n := response{}
	var e error
	n.Output.Message.Role = "assistant"
	if n.Output.Message.Content, e = encodeBlocks(v.Blocks, "assistant"); e != nil {
		return e
	}
	if n.Stop, e = stop(v.Finish); e != nil {
		return e
	}
	if n.Usage, e = encodeUsage(v.Usage); e != nil {
		return e
	}
	return save(ctx, w, n)
}
func order(s string) error { return fmt.Errorf("bedrock converse stream: %s", s) }
