package openaichat

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

// Wire structures deliberately contain only fields supported by this codec.
type request struct {
	Model               string          `json:"model"`
	Messages            []message       `json:"messages"`
	Tools               []wireTool      `json:"tools,omitempty"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	Stop                any             `json:"stop,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
}
type streamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}
type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
}
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
	File     *filePart `json:"file,omitempty"`
}
type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}
type filePart struct {
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}
type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}
type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}
type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireCallFunction `json:"function"`
}
type wireCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func rawString(b json.RawMessage) (string, error) {
	var s string
	if len(b) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return "", unsupported("content")
	}
	if e := strict(b, &s); e != nil {
		return "", e
	}
	return s, nil
}
func decodeParts(b json.RawMessage) ([]contentPart, error) {
	var p []contentPart
	if e := strict(b, &p); e != nil {
		return nil, e
	}
	return p, nil
}
func blockFromPart(p contentPart) (core.ContentBlock, error) {
	switch p.Type {
	case "text":
		if p.ImageURL != nil || p.File != nil {
			return core.ContentBlock{}, unsupported("content.text")
		}
		return core.ContentBlock{Kind: "text", Text: p.Text}, nil
	case "image_url":
		if p.ImageURL == nil || p.ImageURL.URL == "" || p.File != nil {
			return core.ContentBlock{}, unsupported("content.image_url")
		}
		if p.ImageURL.Detail != "" && p.ImageURL.Detail != "auto" {
			return core.ContentBlock{}, unsupported("content.image_url.detail")
		}
		return core.ContentBlock{Kind: "image", URL: p.ImageURL.URL}, nil
	case "file":
		if p.File == nil {
			return core.ContentBlock{}, unsupported("content.file")
		}
		if p.File.FileID != "" || p.File.FileData == "" {
			return core.ContentBlock{}, unsupported("content.file.file_id")
		}
		b := core.ContentBlock{Kind: "document", URL: p.File.FileData, Name: p.File.Filename}
		if strings.HasPrefix(p.File.FileData, "data:") {
			if i := strings.Index(p.File.FileData, ","); i > 5 {
				meta := p.File.FileData[5:i]
				if j := strings.Index(meta, ";"); j >= 0 {
					b.MIMEType = meta[:j]
				} else {
					b.MIMEType = meta
				}
				if d, e := base64.StdEncoding.DecodeString(p.File.FileData[i+1:]); e == nil {
					b.Data = d
				}
			}
		}
		return b, nil
	default:
		return core.ContentBlock{}, unsupported("content.type")
	}
}
func blocksFromContent(b json.RawMessage, allowNull bool) ([]core.ContentBlock, error) {
	if len(b) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		if allowNull {
			return nil, nil
		}
		return nil, unsupported("content")
	}
	if s, e := rawString(b); e == nil {
		return []core.ContentBlock{{Kind: "text", Text: s}}, nil
	}
	p, e := decodeParts(b)
	if e != nil {
		return nil, e
	}
	out := make([]core.ContentBlock, 0, len(p))
	for _, x := range p {
		v, e := blockFromPart(x)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func messageFromWire(m message) (core.Message, error) {
	if m.Role == "" {
		return core.Message{}, unsupported("messages.role")
	}
	if m.Name != "" {
		return core.Message{}, unsupported("messages.name")
	}
	switch m.Role {
	case "system":
		b, e := blocksFromContent(m.Content, false)
		if e != nil {
			return core.Message{}, e
		}
		return core.Message{Role: "system", Content: b}, nil
	case "user":
		b, e := blocksFromContent(m.Content, false)
		if e != nil {
			return core.Message{}, e
		}
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return core.Message{}, unsupported("messages.tool_calls")
		}
		return core.Message{Role: "user", Content: b}, nil
	case "assistant":
		b, e := blocksFromContent(m.Content, true)
		if e != nil {
			return core.Message{}, e
		}
		for _, tc := range m.ToolCalls {
			if tc.Type != "function" || tc.ID == "" || tc.Function.Name == "" {
				return core.Message{}, unsupported("messages.tool_calls")
			}
			if e := arguments(tc.Function.Arguments); e != nil {
				return core.Message{}, e
			}
			b = append(b, core.ContentBlock{Kind: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if len(b) == 0 {
			return core.Message{}, unsupported("messages.content")
		}
		return core.Message{Role: "assistant", Content: b}, nil
	case "tool":
		if m.ToolCallID == "" || len(m.ToolCalls) > 0 {
			return core.Message{}, unsupported("messages.tool_call_id")
		}
		b, e := blocksFromContent(m.Content, false)
		if e != nil {
			return core.Message{}, e
		}
		for i := range b {
			if b[i].Kind != "text" {
				return core.Message{}, unsupported("messages.content")
			}
			b[i].Kind = "tool_result"
			b[i].ID = m.ToolCallID
		}
		return core.Message{Role: "tool", Content: b}, nil
	default:
		return core.Message{}, unsupported("messages.role")
	}
}
func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	var q request
	if e := input(ctx, r, &q); e != nil {
		return nil, e
	}
	if q.Model == "" || len(q.Messages) == 0 {
		return nil, unsupported("model/messages")
	}
	if q.MaxTokens != nil && q.MaxCompletionTokens != nil {
		return nil, unsupported("max_tokens")
	}
	max := q.MaxTokens
	if max == nil {
		max = q.MaxCompletionTokens
	}
	if max != nil && *max < 0 {
		return nil, unsupported("max_tokens")
	}
	conv := core.Conversation{Model: q.Model, MaxOutputTokens: max, Stream: q.Stream}
	if q.StreamOptions != nil {
		if !q.Stream {
			return nil, unsupported("stream_options")
		}
		conv.StreamIncludeUsage = q.StreamOptions.IncludeUsage
	}
	seenNonSystem := false
	for _, m := range q.Messages {
		cm, e := messageFromWire(m)
		if e != nil {
			return nil, e
		}
		if cm.Role == "system" {
			if seenNonSystem {
				return nil, unsupported("messages.system_order")
			}
			conv.System = append(conv.System, cm.Content...)
		} else {
			seenNonSystem = true
			conv.Messages = append(conv.Messages, cm)
		}
	}
	for _, t := range q.Tools {
		if t.Type != "function" || t.Function.Name == "" {
			return nil, unsupported("tools")
		}
		if e := object(t.Function.Parameters); e != nil {
			return nil, e
		}
		if t.Function.Strict != nil && !*t.Function.Strict {
			return nil, unsupported("tools.function.strict")
		}
		conv.Tools = append(conv.Tools, core.Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: append([]byte(nil), t.Function.Parameters...)})
	}
	switch s := q.Stop.(type) {
	case nil:
	case string:
		conv.Stop = []string{s}
	case []any:
		for _, v := range s {
			x, ok := v.(string)
			if !ok {
				return nil, unsupported("stop")
			}
			conv.Stop = append(conv.Stop, x)
		}
	default:
		return nil, unsupported("stop")
	}
	if len(q.ResponseFormat) > 0 {
		mode, name, desc, schema, st, e := decodeStructured(q.ResponseFormat)
		if e != nil {
			return nil, e
		}
		conv.StructuredOutputMode = mode
		conv.StructuredOutputName = name
		conv.StructuredOutputDescription = desc
		conv.StructuredOutputStrict = st
		conv.StructuredOutput = schema
	}
	return conv, nil
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	return encodeConversation(ctx, p, w)
}

// Result wire representation.
type choice struct {
	Index        int             `json:"index"`
	Message      message         `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
type result struct {
	ID      string     `json:"id"`
	Object  string     `json:"object,omitempty"`
	Created int64      `json:"created,omitempty"`
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *wireUsage `json:"usage,omitempty"`
}
type wireUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`
}

func resultBlocks(m message) ([]core.ContentBlock, error) {
	if m.Role != "assistant" {
		return nil, unsupported("choices.message.role")
	}
	b, e := blocksFromContent(m.Content, true)
	if e != nil {
		return nil, e
	}
	for _, tc := range m.ToolCalls {
		if tc.Type != "function" || tc.ID == "" || tc.Function.Name == "" {
			return nil, unsupported("choices.message.tool_calls")
		}
		if e := arguments(tc.Function.Arguments); e != nil {
			return nil, e
		}
		b = append(b, core.ContentBlock{Kind: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return b, nil
}
func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var v result
	if e := input(ctx, r, &v); e != nil {
		return nil, e
	}
	if v.ID == "" || len(v.Choices) != 1 {
		return nil, unsupported("choices")
	}
	ch := v.Choices[0]
	if ch.Index != 0 || ch.FinishReason == nil || *ch.FinishReason == "" {
		return nil, unsupported("choices.finish_reason")
	}
	if len(ch.Logprobs) > 0 && !bytes.Equal(bytes.TrimSpace(ch.Logprobs), []byte("null")) {
		return nil, unsupported("choices.logprobs")
	}
	f, e := finishFromWire(*ch.FinishReason)
	if e != nil {
		return nil, e
	}
	b, e := resultBlocks(ch.Message)
	if e != nil {
		return nil, e
	}
	var u *core.Usage
	if v.Usage != nil {
		if v.Usage.PromptTokens != nil && *v.Usage.PromptTokens < 0 || v.Usage.CompletionTokens != nil && *v.Usage.CompletionTokens < 0 || v.Usage.TotalTokens != nil && *v.Usage.TotalTokens < 0 {
			return nil, unsupported("usage")
		}
		u = &core.Usage{Input: v.Usage.PromptTokens, Output: v.Usage.CompletionTokens, Total: v.Usage.TotalTokens, Source: "provider"}
	}
	return core.GenerationResult{ID: v.ID, Model: v.Model, Blocks: b, Finish: f, Usage: u}, nil
}
func finishFromWire(r string) (core.Finish, error) {
	switch r {
	case "stop", "stop_sequence", "tool_calls", "content_filter":
		return core.Finish{Status: "completed", Reason: r}, nil
	case "length":
		return core.Finish{Status: "incomplete", Reason: r}, nil
	default:
		return core.Finish{}, unsupported("finish_reason")
	}
}
func finishOK(f core.Finish) error {
	if f.Cancellation != "" || f.Reason == "" {
		return fmt.Errorf("finish cannot be represented by Chat Completions")
	}
	expected := "completed"
	if f.Reason == "length" {
		expected = "incomplete"
	}
	if f.Status != "" && f.Status != expected {
		return unsupported("finish.status")
	}
	switch f.Reason {
	case "stop", "stop_sequence", "tool_calls", "content_filter", "length":
		return nil
	}
	return unsupported("finish.reason")
}
func validateResponseFormat(b []byte) error {
	var m map[string]json.RawMessage
	if e := strict(b, &m); e != nil {
		return e
	}
	var typ string
	if x := m["type"]; len(x) > 0 {
		if e := strict(x, &typ); e != nil {
			return e
		}
	} else {
		return unsupported("response_format.type")
	}
	switch typ {
	case "text":
		if len(m) != 1 {
			return unsupported("response_format")
		}
	case "json_object":
		if len(m) != 1 {
			return unsupported("response_format")
		}
	case "json_schema":
		if len(m) != 2 || len(m["json_schema"]) == 0 {
			return unsupported("response_format.json_schema")
		}
		var s struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if e := strict(m["json_schema"], &s); e != nil {
			return e
		}
		if s.Name == "" || len(s.Schema) == 0 {
			return unsupported("response_format.json_schema")
		}
		if e := object(s.Schema); e != nil {
			return e
		}
	default:
		return unsupported("response_format.type")
	}
	return nil
}
func decodeStructured(b []byte) (string, string, string, []byte, *bool, error) {
	var m map[string]json.RawMessage
	if e := strict(b, &m); e != nil {
		return "", "", "", nil, nil, e
	}
	var typ string
	if e := strict(m["type"], &typ); e != nil {
		return "", "", "", nil, nil, e
	}
	switch typ {
	case "text", "json_object":
		if len(m) != 1 {
			return "", "", "", nil, nil, unsupported("response_format")
		}
		return typ, "", "", nil, nil, nil
	case "json_schema":
		if e := validateResponseFormat(b); e != nil {
			return "", "", "", nil, nil, e
		}
		var s struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if e := strict(m["json_schema"], &s); e != nil {
			return "", "", "", nil, nil, e
		}
		return typ, s.Name, s.Description, append([]byte(nil), s.Schema...), s.Strict, nil
	default:
		return "", "", "", nil, nil, unsupported("response_format.type")
	}
}
func encodeConversation(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	q, ok := p.(core.Conversation)
	if !ok {
		return unsupported("payload")
	}
	if q.Model == "" || len(q.Messages) == 0 {
		return unsupported("model/messages")
	}
	out := request{Model: q.Model, MaxTokens: q.MaxOutputTokens, Stream: q.Stream}
	if q.StreamIncludeUsage != nil {
		if !q.Stream {
			return unsupported("stream_options")
		}
		out.StreamOptions = &streamOptions{IncludeUsage: q.StreamIncludeUsage}
	}
	for _, b := range q.System {
		if b.Kind != "text" || !cleanBlock(b, "text") {
			return unsupported("system")
		}
	}
	if len(q.System) > 0 {
		out.Messages = append(out.Messages, message{Role: "system", Content: rawTextBlocks(q.System)})
	}
	for _, m := range q.Messages {
		wm, e := wireMessage(m)
		if e != nil {
			return e
		}
		out.Messages = append(out.Messages, wm)
	}
	for _, t := range q.Tools {
		if t.Name == "" {
			return unsupported("tools")
		}
		if e := object(t.Schema); e != nil {
			return e
		}
		out.Tools = append(out.Tools, wireTool{Type: "function", Function: wireFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
	}
	if len(q.Stop) > 0 {
		out.Stop = q.Stop
	}
	if len(q.StructuredOutput) > 0 || q.StructuredOutputMode != "" || q.StructuredOutputName != "" || q.StructuredOutputDescription != "" || q.StructuredOutputStrict != nil {
		mode := q.StructuredOutputMode
		if mode == "" {
			mode = "json_schema"
		}
		switch mode {
		case "text", "json_object":
			if len(q.StructuredOutput) > 0 || q.StructuredOutputName != "" || q.StructuredOutputDescription != "" || q.StructuredOutputStrict != nil {
				return unsupported("response_format")
			}
			out.ResponseFormat = mustJSON(map[string]string{"type": mode})
		case "json_schema":
			if q.StructuredOutputName == "" || len(q.StructuredOutput) == 0 {
				return unsupported("response_format.json_schema")
			}
			if e := object(q.StructuredOutput); e != nil {
				return e
			}
			inner := struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Schema      json.RawMessage `json:"schema"`
				Strict      *bool           `json:"strict,omitempty"`
			}{q.StructuredOutputName, q.StructuredOutputDescription, q.StructuredOutput, q.StructuredOutputStrict}
			out.ResponseFormat = mustJSON(struct {
				Type   string `json:"type"`
				Schema any    `json:"json_schema"`
			}{"json_schema", inner})
		default:
			return unsupported("response_format.type")
		}
	}
	return output(ctx, w, out)
}
func rawTextBlocks(b []core.ContentBlock) json.RawMessage {
	if len(b) == 1 {
		return json.RawMessage(mustJSON(b[0].Text))
	}
	p := make([]contentPart, 0, len(b))
	for _, x := range b {
		p = append(p, contentPart{Type: "text", Text: x.Text})
	}
	return mustJSON(p)
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func wireMessage(m core.Message) (message, error) {
	if m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
		return message{}, unsupported("messages.role")
	}
	wm := message{Role: m.Role}
	if m.Role == "tool" {
		if len(m.Content) == 0 {
			return message{}, unsupported("messages.content")
		}
		id := m.Content[0].ID
		if id == "" {
			return message{}, unsupported("messages.tool_call_id")
		}
		for _, b := range m.Content {
			if b.Kind != "tool_result" || b.ID != id || !cleanBlock(b, "tool_result") {
				return message{}, unsupported("messages.content")
			}
		}
		wm.ToolCallID = id
		wm.Content = rawTextBlocks(m.Content)
		return wm, nil
	}
	for _, b := range m.Content {
		switch b.Kind {
		case "text":
			if !cleanBlock(b, "text") {
				return message{}, unsupported("messages.content")
			}
			wm.Content = appendRaw(wm.Content, mustJSON(contentPart{Type: "text", Text: b.Text}))
		case "image":
			if !cleanBlock(b, "image") {
				return message{}, unsupported("messages.content")
			}
			wm.Content = appendRaw(wm.Content, mustJSON(contentPart{Type: "image_url", ImageURL: &imageURL{URL: b.URL}}))
		case "document":
			if !cleanBlock(b, "document") {
				return message{}, unsupported("messages.content")
			}
			wm.Content = appendRaw(wm.Content, mustJSON(contentPart{Type: "file", File: &filePart{FileData: b.URL, Filename: b.Name}}))
		case "tool_call":
			if m.Role != "assistant" || !cleanBlock(b, "tool_call") || b.ID == "" || b.Name == "" {
				return message{}, unsupported("messages.tool_calls")
			}
			if e := arguments(b.Arguments); e != nil {
				return message{}, e
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: b.ID, Type: "function", Function: wireCallFunction{Name: b.Name, Arguments: b.Arguments}})
		default:
			return message{}, unsupported("messages.content")
		}
	}
	if len(wm.Content) > 0 && m.Role == "assistant" {
		wm.Content = collapseParts(wm.Content)
	}
	if len(wm.Content) == 0 && len(wm.ToolCalls) == 0 {
		return message{}, unsupported("messages.content")
	}
	return wm, nil
}
func appendRaw(existing json.RawMessage, b []byte) json.RawMessage {
	var arr []json.RawMessage
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &arr)
	}
	arr = append(arr, b)
	return mustJSON(arr)
}
func collapseParts(b json.RawMessage) json.RawMessage {
	var a []json.RawMessage
	_ = json.Unmarshal(b, &a)
	if len(a) == 1 {
		var p contentPart
		if json.Unmarshal(a[0], &p) == nil && p.Type == "text" {
			return mustJSON(p.Text)
		}
	}
	return b
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	g, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("payload")
	}
	if e := finishOK(g.Finish); e != nil {
		return e
	}
	m := message{Role: "assistant"}
	for _, b := range g.Blocks {
		switch b.Kind {
		case "text":
			if !cleanBlock(b, "text") {
				return unsupported("blocks")
			}
			m.Content = appendRaw(m.Content, mustJSON(contentPart{Type: "text", Text: b.Text}))
		case "tool_call":
			if !cleanBlock(b, "tool_call") || b.ID == "" || b.Name == "" {
				return unsupported("blocks")
			}
			if e := arguments(b.Arguments); e != nil {
				return e
			}
			m.ToolCalls = append(m.ToolCalls, wireToolCall{ID: b.ID, Type: "function", Function: wireCallFunction{Name: b.Name, Arguments: b.Arguments}})
		default:
			return unsupported("blocks")
		}
	}
	if len(m.Content) > 0 {
		m.Content = collapseParts(m.Content)
	}
	if len(m.Content) == 0 && len(m.ToolCalls) == 0 {
		return unsupported("blocks")
	}
	v := result{ID: g.ID, Object: "chat.completion", Model: g.Model, Choices: []choice{{Index: 0, Message: m, FinishReason: &g.Finish.Reason}}}
	if g.Usage != nil {
		if g.Usage.Input != nil && *g.Usage.Input < 0 || g.Usage.Output != nil && *g.Usage.Output < 0 || g.Usage.Total != nil && *g.Usage.Total < 0 {
			return unsupported("usage")
		}
		v.Usage = &wireUsage{PromptTokens: g.Usage.Input, CompletionTokens: g.Usage.Output, TotalTokens: g.Usage.Total}
	}
	return output(ctx, w, v)
}

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
