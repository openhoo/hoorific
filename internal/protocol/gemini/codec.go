// Package gemini translates the text and function subset of the Gemini API.
// Model selection and streaming are transport concerns (URL path and method).
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"strings"

	"hoorific/internal/core"
)

type Codec struct{}

func New() *Codec { return &Codec{} }

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
var _ core.StreamCodec = (*Codec)(nil)

func unsupported(field string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: "Gemini semantics cannot be represented: " + field, Origin: "gateway"}
}
func invalid(field string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Param: field, Message: "Invalid Gemini value: " + field, Origin: "gateway"}
}

// checkJSON walks tokens before unmarshalling, including arbitrary argument and
// schema objects. This also rejects trailing values and excessive nesting.
func checkJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 128 {
			return invalid("JSON nesting")
		}
		t, e := d.Token()
		if e != nil {
			return invalid("JSON")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return invalid("JSON")
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return invalid("duplicate JSON key")
				}
				seen[k] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return invalid("JSON")
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return invalid("JSON")
			}
		default:
			return invalid("JSON")
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return invalid("trailing JSON")
	}
	return nil
}

var rawType = reflect.TypeOf(json.RawMessage{})

// Exact JSON tag matching prevents encoding/json's case-insensitive field aliases.
func fields(data json.RawMessage, typ reflect.Type, path string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == rawType {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return invalid(path)
		}
		allowed := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			allowed[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		for k, v := range obj {
			t, ok := allowed[k]
			if !ok {
				return unsupported(path + "." + k)
			}
			if e := fields(v, t, path+"."+k); e != nil {
				return e
			}
		}
	case reflect.Slice:
		var list []json.RawMessage
		if json.Unmarshal(data, &list) != nil {
			return invalid(path)
		}
		for _, v := range list {
			if e := fields(v, typ.Elem(), path+"[]"); e != nil {
				return e
			}
		}
	}
	return nil
}
func decode(data []byte, out any) error {
	if e := checkJSON(data); e != nil {
		return e
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return invalid("body")
	}
	if e := fields(data, reflect.TypeOf(out).Elem(), "body"); e != nil {
		return e
	}
	if e := json.Unmarshal(data, out); e != nil {
		return invalid("JSON field type")
	}
	return nil
}
func readJSON(ctx context.Context, r io.Reader, out any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := io.ReadAll(io.LimitReader(r, 16<<20+1))
	if e != nil {
		return e
	}
	if len(b) > 16<<20 {
		return invalid("body size")
	}
	return decode(b, out)
}
func writeJSON(ctx context.Context, w io.Writer, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return json.NewEncoder(w).Encode(v)
}
func mediaKind(mime string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/") {
		return "image"
	}
	return "document"
}
func object(b []byte, field string) error {
	if e := checkJSON(b); e != nil {
		return e
	}
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return invalid(field)
	}
	return nil
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}
type functionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}
type inlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}
type fileData struct {
	MIMEType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}
type part struct {
	Text     *string           `json:"text,omitempty"`
	Inline   *inlineData       `json:"inlineData,omitempty"`
	File     *fileData         `json:"fileData,omitempty"`
	Call     *functionCall     `json:"functionCall,omitempty"`
	Response *functionResponse `json:"functionResponse,omitempty"`
}
type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}
type declaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	JSONSchema  json.RawMessage `json:"parametersJsonSchema,omitempty"`
}
type tool struct {
	Functions []declaration `json:"functionDeclarations"`
}
type config struct {
	Max                *int64          `json:"maxOutputTokens,omitempty"`
	Stops              []string        `json:"stopSequences,omitempty"`
	ResponseMimeType   string          `json:"responseMimeType,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
}
type request struct {
	Model    string    `json:"-"`
	Contents []content `json:"contents"`
	System   *content  `json:"systemInstruction,omitempty"`
	Tools    []tool    `json:"tools,omitempty"`
	Config   *config   `json:"generationConfig,omitempty"`
}

func decodeParts(parts []part) ([]core.ContentBlock, error) {
	out := make([]core.ContentBlock, 0, len(parts))
	for _, p := range parts {
		n := 0
		if p.Text != nil {
			n++
		}
		if p.Inline != nil {
			n++
		}
		if p.File != nil {
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
		b := core.ContentBlock{}
		switch {
		case p.Text != nil:
			b.Kind = "text"
			b.Text = *p.Text
		case p.Inline != nil:
			if p.Inline.MIMEType == "" || p.Inline.Data == "" {
				return nil, invalid("inlineData")
			}
			raw, e := base64.StdEncoding.DecodeString(p.Inline.Data)
			if e != nil {
				return nil, invalid("inlineData.data")
			}
			b.Kind = mediaKind(p.Inline.MIMEType)
			b.MIMEType = p.Inline.MIMEType
			b.Data = raw
		case p.File != nil:
			if p.File.MIMEType == "" || p.File.FileURI == "" {
				return nil, invalid("fileData")
			}
			b.Kind = mediaKind(p.File.MIMEType)
			b.MIMEType = p.File.MIMEType
			b.URL = p.File.FileURI
		case p.Call != nil:
			if p.Call.Name == "" {
				return nil, invalid("functionCall.name")
			}
			b.Kind = "tool_call"
			b.ID = p.Call.ID
			b.Name = p.Call.Name
			b.Arguments = string(p.Call.Args)
			if b.Arguments == "" {
				b.Arguments = "{}"
			}
			if e := object([]byte(b.Arguments), "args"); e != nil {
				return nil, e
			}
		case p.Response != nil:
			if p.Response.Name == "" {
				return nil, invalid("functionResponse.name")
			}
			if e := object(p.Response.Response, "response"); e != nil {
				return nil, e
			}
			b.Kind = "tool_result"
			b.ID = p.Response.ID
			b.Name = p.Response.Name
			b.Text = string(p.Response.Response)
		}
		out = append(out, b)
	}
	return out, nil
}
func encodeParts(blocks []core.ContentBlock, names map[string]string) ([]part, error) {
	out := make([]part, 0, len(blocks))
	for _, b := range blocks {
		p := part{}
		switch b.Kind {
		case "text":
			if b.ID != "" || b.Name != "" || b.Arguments != "" || b.URL != "" || b.MIMEType != "" || len(b.Data) > 0 {
				return nil, unsupported("text metadata")
			}
			text := b.Text
			p.Text = &text
		case "image", "document":
			if b.ID != "" || b.Name != "" || b.Arguments != "" || len(b.Data) == 0 && b.URL == "" || b.MIMEType == "" {
				return nil, invalid("content media")
			}
			if len(b.Data) > 0 {
				p.Inline = &inlineData{MIMEType: b.MIMEType, Data: base64.StdEncoding.EncodeToString(b.Data)}
			} else {
				p.File = &fileData{MIMEType: b.MIMEType, FileURI: b.URL}
			}
		case "tool_call":
			if b.Name == "" || b.Text != "" {
				return nil, invalid("tool_call")
			}
			args := b.Arguments
			if args == "" {
				args = "{}"
			}
			if e := object([]byte(args), "arguments"); e != nil {
				return nil, e
			}
			p.Call = &functionCall{ID: b.ID, Name: b.Name, Args: json.RawMessage(args)}
			if b.ID != "" {
				if old, ok := names[b.ID]; ok && old != b.Name {
					return nil, invalid("conflicting call ID")
				}
				names[b.ID] = b.Name
			}
		case "tool_result":
			name := b.Name
			if name == "" {
				name = names[b.ID]
			}
			if name == "" {
				return nil, unsupported("tool_result requires function name or known call ID")
			}
			if b.Arguments != "" {
				return nil, unsupported("tool_result.arguments")
			}
			if e := object([]byte(b.Text), "tool_result.text"); e != nil {
				return nil, e
			}
			p.Response = &functionResponse{ID: b.ID, Name: name, Response: json.RawMessage(b.Text)}
		default:
			return nil, unsupported("content." + b.Kind)
		}
		out = append(out, p)
	}
	return out, nil
}

// Native Schema uses uppercase type enums and is not JSON Schema. Translate a
// deliberately small common subset; reject constraints with different semantics.
func nativeSchema(data json.RawMessage) (json.RawMessage, error) {
	var value any
	if e := json.Unmarshal(data, &value); e != nil {
		return nil, invalid("parameters")
	}
	var convert func(any) error
	convert = func(v any) error {
		m, ok := v.(map[string]any)
		if !ok {
			return invalid("parameters")
		}
		for k, v := range m {
			switch k {
			case "type":
				s, ok := v.(string)
				if !ok {
					return invalid("schema.type")
				}
				switch s {
				case "OBJECT", "ARRAY", "STRING", "NUMBER", "INTEGER", "BOOLEAN":
					m[k] = strings.ToLower(s)
				default:
					return unsupported("schema.type")
				}
			case "properties":
				props, ok := v.(map[string]any)
				if !ok {
					return invalid("schema.properties")
				}
				for _, p := range props {
					if e := convert(p); e != nil {
						return e
					}
				}
			case "items":
				if e := convert(v); e != nil {
					return e
				}
			case "description", "title":
				if _, ok := v.(string); !ok {
					return invalid("schema." + k)
				}
			case "required", "enum":
				a, ok := v.([]any)
				if !ok {
					return invalid("schema." + k)
				}
				for _, x := range a {
					if _, ok := x.(string); !ok {
						return invalid("schema." + k)
					}
				}
			default:
				return unsupported("schema." + k)
			}
		}
		return nil
	}
	if e := convert(value); e != nil {
		return nil, e
	}
	return json.Marshal(value)
}
func fromRequest(w request) (core.Conversation, error) {
	c := core.Conversation{Model: w.Model}
	if len(w.Contents) == 0 {
		return c, invalid("contents")
	}
	if w.System != nil {
		if w.System.Role != "" && w.System.Role != "system" {
			return c, unsupported("systemInstruction.role")
		}
		b, e := decodeParts(w.System.Parts)
		if e != nil {
			return c, e
		}
		for _, x := range b {
			if x.Kind != "text" {
				return c, unsupported("systemInstruction.parts")
			}
		}
		c.System = b
	}
	for _, m := range w.Contents {
		role := m.Role
		if role == "" {
			role = "user"
		}
		if role == "model" {
			role = "assistant"
		}
		if role != "user" && role != "assistant" {
			return c, unsupported("contents.role")
		}
		if len(m.Parts) == 0 {
			return c, invalid("parts")
		}
		b, e := decodeParts(m.Parts)
		if e != nil {
			return c, e
		}
		for _, x := range b {
			if x.Kind == "tool_call" && role != "assistant" || x.Kind == "tool_result" && role != "user" {
				return c, invalid("part role")
			}
		}
		c.Messages = append(c.Messages, core.Message{Role: role, Content: b})
	}
	for _, t := range w.Tools {
		if len(t.Functions) == 0 {
			return c, invalid("functionDeclarations")
		}
		for _, f := range t.Functions {
			if f.Name == "" {
				return c, invalid("function name")
			}
			schema := f.JSONSchema
			if len(f.Parameters) > 0 {
				if len(schema) > 0 {
					return c, invalid("parameters union")
				}
				var e error
				schema, e = nativeSchema(f.Parameters)
				if e != nil {
					return c, e
				}
			}
			if len(schema) > 0 {
				if e := object(schema, "parametersJsonSchema"); e != nil {
					return c, e
				}
			}
			c.Tools = append(c.Tools, core.Tool{Name: f.Name, Description: f.Description, Schema: schema})
		}
	}
	if w.Config != nil {
		c.MaxOutputTokens = w.Config.Max
		c.Stop = w.Config.Stops
		if w.Config.ResponseMimeType != "" && w.Config.ResponseMimeType != "application/json" {
			return c, unsupported("responseMimeType")
		}
		if len(w.Config.ResponseJSONSchema) > 0 {
			if e := object(w.Config.ResponseJSONSchema, "responseJsonSchema"); e != nil {
				return c, e
			}
			c.StructuredOutput = append([]byte(nil), w.Config.ResponseJSONSchema...)
			c.StructuredOutputMode = "json_schema"
		} else if w.Config.ResponseMimeType == "application/json" {
			c.StructuredOutputMode = "json_object"
		}
	}
	if e := limits(c); e != nil {
		return c, e
	}
	return c, nil
}
func limits(c core.Conversation) error {
	if c.MaxOutputTokens != nil && *c.MaxOutputTokens <= 0 {
		return invalid("maxOutputTokens")
	}
	if len(c.Stop) > 5 {
		return invalid("stopSequences")
	}
	for _, s := range c.Stop {
		if s == "" {
			return invalid("stopSequences")
		}
	}
	return nil
}
func toRequest(c core.Conversation) (request, error) {
	w := request{Model: c.Model}
	if c.StructuredOutputName != "" || c.StructuredOutputDescription != "" || c.StructuredOutputStrict != nil {
		return w, unsupported("structured output metadata")
	}
	switch c.StructuredOutputMode {
	case "":
		if len(c.StructuredOutput) > 0 {
			if e := object(c.StructuredOutput, "structured_output"); e != nil {
				return w, e
			}
			w.Config = &config{ResponseMimeType: "application/json", ResponseJSONSchema: append([]byte(nil), c.StructuredOutput...)}
		}
	case "json_object":
		if len(c.StructuredOutput) > 0 {
			return w, unsupported("structured_output")
		}
		w.Config = &config{ResponseMimeType: "application/json"}
	case "json_schema":
		if len(c.StructuredOutput) == 0 {
			return w, invalid("structured_output")
		}
		if e := object(c.StructuredOutput, "structured_output"); e != nil {
			return w, e
		}
		w.Config = &config{ResponseMimeType: "application/json", ResponseJSONSchema: append([]byte(nil), c.StructuredOutput...)}
	default:
		return w, unsupported("structured output mode")
	}
	if e := limits(c); e != nil {
		return w, e
	}
	if len(c.Messages) == 0 {
		return w, invalid("messages")
	}
	names := map[string]string{}
	if len(c.System) > 0 {
		for _, b := range c.System {
			if b.Kind != "text" {
				return w, unsupported("system content")
			}
		}
		p, e := encodeParts(c.System, names)
		if e != nil {
			return w, e
		}
		w.System = &content{Parts: p}
	}
	for _, m := range c.Messages {
		role := m.Role
		switch role {
		case "assistant":
			role = "model"
		case "user":
		case "tool":
			role = "user"
		default:
			return w, unsupported("message.role")
		}
		if len(m.Content) == 0 {
			return w, invalid("message.content")
		}
		for _, b := range m.Content {
			if b.Kind == "tool_call" && role != "model" || b.Kind == "tool_result" && role != "user" || m.Role == "tool" && b.Kind != "tool_result" {
				return w, invalid("part role")
			}
		}
		p, e := encodeParts(m.Content, names)
		if e != nil {
			return w, e
		}
		w.Contents = append(w.Contents, content{Role: role, Parts: p})
	}
	if len(c.Tools) > 0 {
		t := tool{}
		for _, f := range c.Tools {
			if f.Name == "" {
				return w, invalid("tool.name")
			}
			if len(f.Schema) > 0 {
				if e := object(f.Schema, "tool.schema"); e != nil {
					return w, e
				}
			}
			t.Functions = append(t.Functions, declaration{Name: f.Name, Description: f.Description, JSONSchema: f.Schema})
		}
		w.Tools = []tool{t}
	}
	if c.MaxOutputTokens != nil || len(c.Stop) > 0 {
		if w.Config == nil {
			w.Config = &config{}
		}
		w.Config.Max = c.MaxOutputTokens
		w.Config.Stops = c.Stop
	}
	return w, nil
}
func (*Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	var w request
	if e := readJSON(ctx, r, &w); e != nil {
		return nil, e
	}
	c, e := fromRequest(w)
	return c, e
}
func (*Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	c, ok := p.(core.Conversation)
	if !ok {
		return unsupported("request payload")
	}
	v, e := toRequest(c)
	if e != nil {
		return e
	}
	v.Model = ""
	return writeJSON(ctx, w, v)
}

type usage struct {
	Input  *int64 `json:"promptTokenCount,omitempty"`
	Output *int64 `json:"candidatesTokenCount,omitempty"`
	Total  *int64 `json:"totalTokenCount,omitempty"`
}
type candidate struct {
	Content *content `json:"content,omitempty"`
	Finish  string   `json:"finishReason,omitempty"`
	Index   *int     `json:"index,omitempty"`
}
type response struct {
	Candidates []candidate `json:"candidates,omitempty"`
	Usage      *usage      `json:"usageMetadata,omitempty"`
	Model      string      `json:"modelVersion,omitempty"`
	ID         string      `json:"responseId,omitempty"`
}

func validUsage(u *usage) error {
	if u != nil {
		for _, n := range []*int64{u.Input, u.Output, u.Total} {
			if n != nil && *n < 0 {
				return invalid("usageMetadata")
			}
		}
	}
	return nil
}
func finish(native string, hasTools bool) (core.Finish, error) {
	var reason, status string
	switch native {
	case "":
		return core.Finish{}, nil
	case "STOP":
		reason = "stop"
		status = "completed"
		if hasTools {
			reason = "tool_calls"
		}
	case "MAX_TOKENS":
		reason = "length"
		status = "length"
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		reason = "content_filter"
		status = "content_filter"
	case "OTHER", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "MALFORMED_RESPONSE":
		reason = "error"
		status = "error"
	default:
		return core.Finish{}, unsupported("finishReason." + native)
	}
	return core.Finish{Reason: reason, Status: status}, nil
}
func fromResponse(w response, terminal bool) (core.GenerationResult, error) {
	r := core.GenerationResult{ID: w.ID, Model: w.Model}
	if len(w.Candidates) > 1 {
		return r, unsupported("multiple candidates")
	}
	if e := validUsage(w.Usage); e != nil {
		return r, e
	}
	if w.Usage != nil {
		r.Usage = &core.Usage{Input: w.Usage.Input, Output: w.Usage.Output, Total: w.Usage.Total, Source: "provider"}
	}
	if len(w.Candidates) == 1 {
		c := w.Candidates[0]
		if c.Index != nil && *c.Index != 0 {
			return r, unsupported("candidate index")
		}
		if c.Content != nil {
			if c.Content.Role != "" && c.Content.Role != "model" {
				return r, invalid("candidate role")
			}
			var e error
			r.Blocks, e = decodeParts(c.Content.Parts)
			if e != nil {
				return r, e
			}
		}
		tools := false
		for _, b := range r.Blocks {
			if b.Kind == "tool_result" {
				return r, unsupported("result functionResponse")
			}
			tools = tools || b.Kind == "tool_call"
		}
		var e error
		r.Finish, e = finish(c.Finish, tools)
		if e != nil {
			return r, e
		}
	}
	if terminal && r.Finish.Reason == "" {
		return r, invalid("missing finishReason")
	}
	return r, nil
}
func finishReason(f core.Finish) (string, error) {
	if f.Cancellation != "" {
		return "", unsupported("finish cancellation")
	}
	switch f.Reason {
	case "stop", "stop_sequence":
		if f.Status != "" && f.Status != "completed" && f.Status != "stop" {
			return "", unsupported("finish status/reason")
		}
		return "STOP", nil
	case "tool_calls":
		if f.Status != "" && f.Status != "completed" && f.Status != "tool_calls" {
			return "", unsupported("finish status/reason")
		}
		return "STOP", nil
	case "length":
		if f.Status != "" && f.Status != "length" {
			return "", unsupported("finish status/reason")
		}
		return "MAX_TOKENS", nil
	case "content_filter":
		if f.Status != "" && f.Status != "content_filter" {
			return "", unsupported("finish status/reason")
		}
		return "SAFETY", nil
	case "error":
		if f.Status != "" && f.Status != "error" {
			return "", unsupported("finish status/reason")
		}
		return "OTHER", nil
	case "":
	default:
		return "", unsupported("finish.reason")
	}
	switch f.Status {
	case "completed", "stop":
		return "STOP", nil
	case "tool_calls":
		return "STOP", nil
	case "length":
		return "MAX_TOKENS", nil
	case "content_filter":
		return "SAFETY", nil
	case "error":
		return "OTHER", nil
	}
	return "", unsupported("finish")
}
func toResponse(r core.GenerationResult, terminal bool) (response, error) {
	w := response{ID: r.ID, Model: r.Model}
	for _, b := range r.Blocks {
		if b.Kind != "text" && b.Kind != "tool_call" {
			return w, unsupported("result content")
		}
	}
	p, e := encodeParts(r.Blocks, map[string]string{})
	if e != nil {
		return w, e
	}
	c := candidate{}
	if len(p) > 0 {
		c.Content = &content{Role: "model", Parts: p}
	}
	if terminal {
		c.Finish, e = finishReason(r.Finish)
		if e != nil {
			return w, e
		}
	}
	if len(p) > 0 || c.Finish != "" {
		w.Candidates = []candidate{c}
	}
	if r.Usage != nil {
		w.Usage = &usage{Input: r.Usage.Input, Output: r.Usage.Output, Total: r.Usage.Total}
		if e := validUsage(w.Usage); e != nil {
			return w, e
		}
	}
	return w, nil
}
func (*Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var w response
	if e := readJSON(ctx, r, &w); e != nil {
		return nil, e
	}
	v, e := fromResponse(w, true)
	return v, e
}
func (*Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	r, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("result payload")
	}
	v, e := toResponse(r, true)
	if e != nil {
		return e
	}
	return writeJSON(ctx, w, v)
}

type CountTokensCodec struct{}

func NewCountTokens() *CountTokensCodec { return &CountTokensCodec{} }

var _ core.RequestCodec = (*CountTokensCodec)(nil)
var _ core.ResultCodec = (*CountTokensCodec)(nil)

type countGenerate struct {
	Model    string    `json:"model"`
	Contents []content `json:"contents"`
	System   *content  `json:"systemInstruction,omitempty"`
	Tools    []tool    `json:"tools,omitempty"`
	Config   *config   `json:"generationConfig,omitempty"`
}
type countRequest struct {
	Contents []content      `json:"contents,omitempty"`
	Generate *countGenerate `json:"generateContentRequest,omitempty"`
}
type countResult struct {
	Total *int64 `json:"totalTokens"`
}

func (*CountTokensCodec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	var w countRequest
	if e := readJSON(ctx, r, &w); e != nil {
		return nil, e
	}
	v := request{Contents: w.Contents}
	if w.Generate != nil {
		if w.Contents != nil {
			return nil, invalid("count request union")
		}
		v = request{Model: w.Generate.Model, Contents: w.Generate.Contents, System: w.Generate.System, Tools: w.Generate.Tools, Config: w.Generate.Config}
	}
	c, e := fromRequest(v)
	if e != nil {
		return nil, e
	}
	return core.CountTokensRequest{Conversation: c}, nil
}
func (*CountTokensCodec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	c, ok := p.(core.CountTokensRequest)
	if !ok {
		return unsupported("count request payload")
	}
	if c.Conversation.Stream {
		return unsupported("count stream")
	}
	v, e := toRequest(c.Conversation)
	if e != nil {
		return e
	}
	if v.System == nil && len(v.Tools) == 0 && v.Config == nil {
		return writeJSON(ctx, w, countRequest{Contents: v.Contents})
	}
	g := countGenerate{Model: v.Model, Contents: v.Contents, System: v.System, Tools: v.Tools, Config: v.Config}
	if g.Model == "" {
		return invalid("count generateContentRequest.model")
	}
	return writeJSON(ctx, w, countRequest{Generate: &g})
}
func (*CountTokensCodec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var w countResult
	if e := readJSON(ctx, r, &w); e != nil {
		return nil, e
	}
	if w.Total != nil && *w.Total < 0 {
		return nil, invalid("totalTokens")
	}
	return core.CountTokensResult{InputTokens: w.Total, Source: "provider"}, nil
}
func (*CountTokensCodec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	r, ok := p.(core.CountTokensResult)
	if !ok {
		return unsupported("count result payload")
	}
	if r.InputTokens != nil && *r.InputTokens < 0 {
		return invalid("totalTokens")
	}
	return writeJSON(ctx, w, countResult{Total: r.InputTokens})
}

// Gemini streams use the framing package directly; no provider body is echoed.
