package openairesponses

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"io"
	"strings"
)

type Codec struct{}

func New() *Codec { return &Codec{} }

type cacheBreakpoint struct {
	Mode string `json:"mode"`
}
type promptCacheOptions struct {
	Mode string `json:"mode,omitempty"`
	TTL  string `json:"ttl,omitempty"`
}
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
	Type                  string           `json:"type"`
	Text                  string           `json:"text,omitempty"`
	ImageURL              string           `json:"image_url,omitempty"`
	FileURL               string           `json:"file_url,omitempty"`
	Filename              string           `json:"filename,omitempty"`
	FileData              string           `json:"file_data,omitempty"`
	PromptCacheBreakpoint *cacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
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
	Model                string              `json:"model"`
	Store                *bool               `json:"store"`
	Input                []wireInput         `json:"input"`
	Max                  *int64              `json:"max_output_tokens,omitempty"`
	Stream               bool                `json:"stream,omitempty"`
	Tools                []wireTool          `json:"tools,omitempty"`
	Text                 *wireText           `json:"text,omitempty"`
	PromptCacheKey       string              `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string              `json:"prompt_cache_retention,omitempty"`
	PromptCacheOptions   *promptCacheOptions `json:"prompt_cache_options,omitempty"`
}

func validCacheString(v string, max int) bool {
	return v != "" && len([]rune(v)) <= max && !strings.ContainsAny(v, "\x00\r\n")
}

func validCacheTTL(v string) bool {
	switch v {
	case "", "5m", "30m", "1h", "24h":
		return true
	default:
		return false
	}
}

func validatePromptCache(c *core.PromptCache) error {
	if c == nil {
		return nil
	}
	switch c.Protocol {
	case "", "openai-chat", "openai-responses":
	default:
		return unsupported("cache.protocol")
	}
	if c.Key != "" && !validCacheString(c.Key, 256) {
		return unsupported("prompt_cache_key")
	}
	if c.Retention != "" && c.Retention != "in_memory" && c.Retention != "24h" {
		return unsupported("prompt_cache_retention")
	}
	if c.Mode != "" && c.Mode != "implicit" && c.Mode != "explicit" {
		return unsupported("prompt_cache_options.mode")
	}
	if !validCacheTTL(c.TTL) {
		return unsupported("prompt_cache_options.ttl")
	}
	if c.SessionID != "" {
		return unsupported("session_id")
	}
	if c.Control != nil {
		return unsupported("cache.control")
	}
	if c.CachedContent != "" {
		return unsupported("cache.cached_content")
	}
	return nil
}

func validateResponsesCache(q core.Conversation) error {
	if err := validatePromptCache(q.Cache); err != nil {
		return err
	}
	check := func(b core.ContentBlock, param string) error {
		if b.CacheControl != nil {
			return unsupported(param + ".cache_control")
		}
		if b.CacheBreakpoint && b.Kind != "text" {
			return unsupported(param + ".prompt_cache_breakpoint")
		}
		return nil
	}
	for i, b := range q.System {
		if err := check(b, "system["+fmt.Sprint(i)+"]"); err != nil {
			return err
		}
	}
	for i, m := range q.Messages {
		for j, b := range m.Content {
			if err := check(b, "messages["+fmt.Sprint(i)+"].content["+fmt.Sprint(j)+"]"); err != nil {
				return err
			}
		}
	}
	for i, t := range q.Tools {
		if t.CacheControl != nil || t.CacheBreakpoint {
			return unsupported("tools[" + fmt.Sprint(i) + "].cache_control")
		}
	}
	return nil
}

func contentToWire(b core.ContentBlock) (wireContent, error) {
	if b.CacheControl != nil {
		return wireContent{}, unsupported("input.cache_control")
	}
	if b.CacheBreakpoint && b.Kind != "text" {
		return wireContent{}, unsupported("input.prompt_cache_breakpoint")
	}
	var breakpoint *cacheBreakpoint
	if b.CacheBreakpoint {
		breakpoint = &cacheBreakpoint{Mode: "explicit"}
	}
	switch b.Kind {
	case "text":
		return wireContent{Type: "input_text", Text: b.Text, PromptCacheBreakpoint: breakpoint}, nil
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
	if err := validateResponsesCache(q); err != nil {
		return err
	}
	rw := requestWire{Model: q.Model, Store: new(bool), Max: q.MaxOutputTokens, Stream: q.Stream}
	*rw.Store = false
	if q.Cache != nil {
		rw.PromptCacheKey = q.Cache.Key
		rw.PromptCacheRetention = q.Cache.Retention
		if q.Cache.Mode != "" || q.Cache.TTL != "" {
			rw.PromptCacheOptions = &promptCacheOptions{Mode: q.Cache.Mode, TTL: q.Cache.TTL}
		}
	}
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
func contentMetadata(v object) (bool, error) {
	if _, ok := v["cache_control"]; ok {
		return false, unsupported("input.cache_control")
	}
	raw, ok := v["prompt_cache_breakpoint"]
	if !ok {
		return false, nil
	}
	b, err := obj(raw)
	if err != nil {
		return false, err
	}
	if err := fields(b, "mode"); err != nil {
		return false, err
	}
	var mode string
	if err := get(b, "mode", &mode, true); err != nil {
		return false, err
	}
	if mode != "explicit" {
		return false, unsupported("input.prompt_cache_breakpoint.mode")
	}
	return true, nil
}

func parseContent(v object) (core.ContentBlock, error) {
	t, e := str(v, "type")
	if e != nil {
		return core.ContentBlock{}, e
	}
	switch t {
	case "input_text", "output_text":
		if e := fields(v, "type", "text", "prompt_cache_breakpoint", "cache_control"); e != nil {
			return core.ContentBlock{}, e
		}
		breakpoint, e := contentMetadata(v)
		if e != nil {
			return core.ContentBlock{}, e
		}
		var s string
		if e = get(v, "text", &s, true); e != nil {
			return core.ContentBlock{}, e
		}
		return core.ContentBlock{Kind: "text", Text: s, CacheBreakpoint: breakpoint}, nil
	case "input_image":
		if e := fields(v, "type", "image_url", "prompt_cache_breakpoint", "cache_control"); e != nil {
			return core.ContentBlock{}, e
		}
		breakpoint, e := contentMetadata(v)
		if e != nil {
			return core.ContentBlock{}, e
		}
		if breakpoint {
			return core.ContentBlock{}, unsupported("input.prompt_cache_breakpoint")
		}
		var s string
		if e = get(v, "image_url", &s, true); e != nil {
			return core.ContentBlock{}, e
		}
		return core.ContentBlock{Kind: "image", URL: s}, nil
	case "input_file":
		if e := fields(v, "type", "file_url", "file_data", "filename", "prompt_cache_breakpoint", "cache_control"); e != nil {
			return core.ContentBlock{}, e
		}
		breakpoint, e := contentMetadata(v)
		if e != nil {
			return core.ContentBlock{}, e
		}
		if breakpoint {
			return core.ContentBlock{}, unsupported("input.prompt_cache_breakpoint")
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
func promptCacheFromObject(m object) (*core.PromptCache, error) {
	if _, key := m["prompt_cache_key"]; !key {
		if _, retention := m["prompt_cache_retention"]; !retention {
			if _, options := m["prompt_cache_options"]; !options {
				return nil, nil
			}
		}
	}
	c := &core.PromptCache{Protocol: "openai-responses"}
	if _, ok := m["prompt_cache_key"]; ok {
		if err := get(m, "prompt_cache_key", &c.Key, false); err != nil {
			return nil, err
		}
		if !validCacheString(c.Key, 256) {
			return nil, unsupported("prompt_cache_key")
		}
	}
	if _, ok := m["prompt_cache_retention"]; ok {
		if err := get(m, "prompt_cache_retention", &c.Retention, false); err != nil {
			return nil, err
		}
		switch c.Retention {
		case "in_memory", "24h":
		default:
			return nil, unsupported("prompt_cache_retention")
		}
	}
	if raw, ok := m["prompt_cache_options"]; ok {
		var options object
		if err := json.Unmarshal(raw, &options); err != nil {
			return nil, unsupported("prompt_cache_options")
		}
		if err := fields(options, "mode", "ttl"); err != nil {
			return nil, err
		}
		if _, ok := options["mode"]; ok {
			if err := get(options, "mode", &c.Mode, false); err != nil {
				return nil, err
			}
			switch c.Mode {
			case "implicit", "explicit":
			default:
				return nil, unsupported("prompt_cache_options.mode")
			}
		}
		if _, ok := options["ttl"]; ok {
			if err := get(options, "ttl", &c.TTL, false); err != nil {
				return nil, err
			}
			if !validCacheTTL(c.TTL) || c.TTL == "" {
				return nil, unsupported("prompt_cache_options.ttl")
			}
		}
	}
	if err := validatePromptCache(c); err != nil {
		return nil, err
	}
	return c, nil
}

func markDecodedCache(q *core.Conversation) {
	for _, b := range q.System {
		if b.CacheBreakpoint {
			if q.Cache == nil {
				q.Cache = &core.PromptCache{Protocol: "openai-responses"}
			}
			return
		}
	}
	for _, m := range q.Messages {
		for _, b := range m.Content {
			if b.CacheBreakpoint {
				if q.Cache == nil {
					q.Cache = &core.PromptCache{Protocol: "openai-responses"}
				}
				return
			}
		}
	}
}

func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	var m object
	if e := load(r, &m); e != nil {
		return nil, e
	}
	if e := fields(m, "model", "store", "input", "max_output_tokens", "stream", "tools", "text", "prompt_cache_key", "prompt_cache_retention", "prompt_cache_options"); e != nil {
		return nil, e
	}
	var q core.Conversation
	var e error
	q.Model, e = str(m, "model")
	if e != nil {
		return nil, e
	}
	q.Cache, e = promptCacheFromObject(m)
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
	markDecodedCache(&q)
	if err := validateResponsesCache(q); err != nil {
		return nil, err
	}
	return q, nil
}

var _ core.RequestCodec = (*Codec)(nil)
