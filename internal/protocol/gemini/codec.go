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
	Text             *string           `json:"text,omitempty"`
	Inline           *inlineData       `json:"inlineData,omitempty"`
	File             *fileData         `json:"fileData,omitempty"`
	Call             *functionCall     `json:"functionCall,omitempty"`
	Response         *functionResponse `json:"functionResponse,omitempty"`
	Thought          *bool             `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	ExecutableCode   json.RawMessage   `json:"executableCode,omitempty"`
	CodeExecution    json.RawMessage   `json:"codeExecutionResult,omitempty"`
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
	Model         string    `json:"-"`
	Contents      []content `json:"contents"`
	System        *content  `json:"systemInstruction,omitempty"`
	Tools         []tool    `json:"tools,omitempty"`
	Config        *config   `json:"generationConfig,omitempty"`
	CachedContent *string   `json:"cachedContent,omitempty"`
}

// validateResponseJSON keeps client requests strict while allowing the
// documented response-only metadata Gemini may add around semantic content.
// checkJSON has already rejected duplicate keys and trailing values.
func validateResponseJSON(data []byte) error {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return invalid("body")
	}
	for k, raw := range root {
		switch k {
		case "candidates":
			var xs []json.RawMessage
			if json.Unmarshal(raw, &xs) != nil {
				return invalid("candidates")
			}
			for _, x := range xs {
				if err := validateCandidateJSON(x); err != nil {
					return err
				}
			}
		case "promptFeedback":
			if err := validatePromptFeedbackJSON(raw); err != nil {
				return err
			}
		case "usageMetadata":
			if err := validateUsageJSON(raw); err != nil {
				return err
			}
		case "modelVersion", "responseId":
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return invalid("response." + k)
			}
		case "modelStatus":
			if err := object(raw, "response.modelStatus"); err != nil {
				return err
			}
		default:
			return unsupported("response." + k)
		}
	}
	return nil
}

func validateObjectFields(raw []byte, path string, allowed map[string]bool) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, invalid(path)
	}
	for k := range m {
		if !allowed[k] {
			return nil, unsupported(path + "." + k)
		}
	}
	return m, nil
}

func validatePromptFeedbackJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "promptFeedback", map[string]bool{"blockReason": true, "safetyRatings": true})
	if err != nil {
		return err
	}
	if v, ok := m["blockReason"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("promptFeedback.blockReason")
		}
	}
	if v, ok := m["safetyRatings"]; ok {
		if err := validateSafetyRatings(v, "promptFeedback.safetyRatings"); err != nil {
			return err
		}
	}
	return nil
}

func validateSafetyRatings(raw []byte, path string) error {
	var xs []json.RawMessage
	if json.Unmarshal(raw, &xs) != nil {
		return invalid(path)
	}
	for _, x := range xs {
		m, err := validateObjectFields(x, path+"[]", map[string]bool{
			"category": true, "probability": true, "probabilityScore": true,
			"severity": true, "severityScore": true, "blocked": true,
		})
		if err != nil {
			return err
		}
		for _, key := range []string{"category", "probability", "severity"} {
			if v, ok := m[key]; ok {
				var s string
				if json.Unmarshal(v, &s) != nil {
					return invalid(path + "." + key)
				}
			}
		}
		if v, ok := m["blocked"]; ok {
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return invalid(path + ".blocked")
			}
		}
	}
	return nil
}

func validateCandidateJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "candidate", map[string]bool{
		"content": true, "finishReason": true, "safetyRatings": true,
		"citationMetadata": true, "tokenCount": true, "groundingAttributions": true,
		"groundingMetadata": true, "finishMessage": true, "index": true,
		"avgLogprobs": true, "logprobsResult": true,
	})
	if err != nil {
		return err
	}
	if v, ok := m["content"]; ok {
		if err := validateResponseContentJSON(v); err != nil {
			return err
		}
	}
	if v, ok := m["finishReason"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("candidate.finishReason")
		}
	}
	if v, ok := m["finishMessage"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("candidate.finishMessage")
		}
	}
	if v, ok := m["safetyRatings"]; ok {
		if err := validateSafetyRatings(v, "candidate.safetyRatings"); err != nil {
			return err
		}
	}
	return nil
}

func validateResponseContentJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "candidate.content", map[string]bool{"role": true, "parts": true})
	if err != nil {
		return err
	}
	if v, ok := m["role"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("candidate.content.role")
		}
	}
	v, ok := m["parts"]
	if !ok {
		return invalid("candidate.content.parts")
	}
	var xs []json.RawMessage
	if json.Unmarshal(v, &xs) != nil {
		return invalid("candidate.content.parts")
	}
	for _, x := range xs {
		if err := validateResponsePartJSON(x); err != nil {
			return err
		}
	}
	return nil
}

func validateFunctionCallJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "candidate.content.part.functionCall", map[string]bool{"id": true, "name": true, "args": true})
	if err != nil {
		return err
	}
	if v, ok := m["name"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil || s == "" {
			return invalid("functionCall.name")
		}
	} else {
		return invalid("functionCall.name")
	}
	if v, ok := m["args"]; ok {
		if err := object(v, "functionCall.args"); err != nil {
			return err
		}
	}
	return nil
}

func validateFunctionResponseJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "candidate.content.part.functionResponse", map[string]bool{"id": true, "name": true, "response": true})
	if err != nil {
		return err
	}
	if v, ok := m["name"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil || s == "" {
			return invalid("functionResponse.name")
		}
	} else {
		return invalid("functionResponse.name")
	}
	if v, ok := m["response"]; ok {
		if err := object(v, "functionResponse.response"); err != nil {
			return err
		}
	} else {
		return invalid("functionResponse.response")
	}
	return nil
}

func validateResponsePartJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "candidate.content.part", map[string]bool{
		"thought": true, "thoughtSignature": true, "partMetadata": true,
		"mediaResolution": true, "mediaProcessing": true, "text": true,
		"inlineData": true, "fileData": true, "functionCall": true,
		"functionResponse": true, "executableCode": true, "codeExecutionResult": true,
		"videoMetadata": true,
	})
	if err != nil {
		return err
	}
	if v, ok := m["thought"]; ok {
		var b bool
		if json.Unmarshal(v, &b) != nil {
			return invalid("candidate.content.part.thought")
		}
		if b {
			return unsupported("candidate.content.part.thought")
		}
	}
	if v, ok := m["thoughtSignature"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("candidate.content.part.thoughtSignature")
		}
		if s != "" {
			return unsupported("candidate.content.part.thoughtSignature")
		}
	}
	for _, key := range []string{"partMetadata", "mediaResolution", "videoMetadata"} {
		if v, ok := m[key]; ok {
			if err := object(v, "candidate.content.part."+key); err != nil {
				return err
			}
		}
	}
	if v, ok := m["functionCall"]; ok {
		if err := validateFunctionCallJSON(v); err != nil {
			return err
		}
	}
	if v, ok := m["functionResponse"]; ok {
		if err := validateFunctionResponseJSON(v); err != nil {
			return err
		}
	}
	for _, key := range []string{"inlineData", "fileData"} {
		if v, ok := m[key]; ok {
			var allowed map[string]bool
			if key == "inlineData" {
				allowed = map[string]bool{"mimeType": true, "data": true}
			} else {
				allowed = map[string]bool{"mimeType": true, "fileUri": true}
			}
			if _, err := validateObjectFields(v, "candidate.content.part."+key, allowed); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"executableCode", "codeExecutionResult"} {
		if _, ok := m[key]; ok {
			return unsupported("candidate.content.part." + key)
		}
	}
	if v, ok := m["mediaProcessing"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("candidate.content.part.mediaProcessing")
		}
	}
	if v, ok := m["text"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("candidate.content.part.text")
		}
	}
	return nil
}

func validateUsageJSON(raw []byte) error {
	m, err := validateObjectFields(raw, "usageMetadata", map[string]bool{
		"promptTokenCount": true, "cachedContentTokenCount": true,
		"candidatesTokenCount": true, "toolUsePromptTokenCount": true,
		"thoughtsTokenCount": true, "totalTokenCount": true,
		"promptTokensDetails": true, "cacheTokensDetails": true,
		"candidatesTokensDetails": true, "toolUsePromptTokensDetails": true,
		"serviceTier": true,
	})
	if err != nil {
		return err
	}
	for _, key := range []string{"promptTokenCount", "cachedContentTokenCount", "candidatesTokenCount", "toolUsePromptTokenCount", "thoughtsTokenCount", "totalTokenCount"} {
		if v, ok := m[key]; ok {
			var n int64
			if json.Unmarshal(v, &n) != nil {
				return invalid("usageMetadata." + key)
			}
		}
	}
	if v, ok := m["serviceTier"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return invalid("usageMetadata.serviceTier")
		}
	}
	for _, key := range []string{"promptTokensDetails", "cacheTokensDetails", "candidatesTokensDetails", "toolUsePromptTokensDetails"} {
		if v, ok := m[key]; ok {
			var xs []json.RawMessage
			if json.Unmarshal(v, &xs) != nil {
				return invalid("usageMetadata." + key)
			}
			for _, x := range xs {
				if err := object(x, "usageMetadata."+key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeResponseData(data []byte, out any) error {
	if e := checkJSON(data); e != nil {
		return e
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return invalid("body")
	}
	if e := validateResponseJSON(data); e != nil {
		return e
	}
	if e := json.Unmarshal(data, out); e != nil {
		return invalid("JSON field type")
	}
	return nil
}

func readResponseJSON(ctx context.Context, r io.Reader, out any) error {
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
	return decodeResponseData(b, out)
}

func decodeParts(parts []part) ([]core.ContentBlock, error) {
	out := make([]core.ContentBlock, 0, len(parts))
	for _, p := range parts {
		if p.Thought != nil || p.ThoughtSignature != "" || len(p.ExecutableCode) > 0 || len(p.CodeExecution) > 0 {
			return nil, unsupported("thought or code-execution part")
		}
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
		if b.CacheControl != nil || b.CacheBreakpoint {
			return nil, unsupported("content cache control")
		}
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

func validCachedContent(name string) bool {
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 2 && parts[0] == "cachedContents":
		return safeResourceSegment(parts[1])
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "cachedContents":
		return safeResourceSegment(parts[1]) && safeResourceSegment(parts[3]) && safeResourceSegment(parts[5])
	default:
		return false
	}
}

func safeResourceSegment(s string) bool {
	return s != "" && !strings.ContainsAny(s, "{}:")
}

func fromRequest(w request) (core.Conversation, error) {
	c := core.Conversation{Model: w.Model}
	if w.CachedContent != nil {
		if !validCachedContent(*w.CachedContent) {
			return c, invalid("cachedContent")
		}
		c.Cache = &core.PromptCache{Protocol: core.Protocol("gemini-content"), CachedContent: *w.CachedContent}
	}
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
func cacheRequest(c *core.PromptCache) (*string, error) {
	if c == nil {
		return nil, nil
	}
	if c.Protocol != "" && c.Protocol != core.Protocol("gemini-content") {
		return nil, unsupported("cache.protocol")
	}
	if c.CachedContent == "" || !validCachedContent(c.CachedContent) {
		return nil, invalid("cache.cachedContent")
	}
	if c.Key != "" || c.Retention != "" || c.Mode != "" || c.TTL != "" || c.SessionID != "" || c.Control != nil {
		return nil, unsupported("cache")
	}
	name := c.CachedContent
	return &name, nil
}

func toRequest(c core.Conversation) (request, error) {
	w := request{Model: c.Model}
	cached, e := cacheRequest(c.Cache)
	if e != nil {
		return w, e
	}
	w.CachedContent = cached
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
			if f.CacheControl != nil || f.CacheBreakpoint {
				return w, unsupported("tool cache control")
			}
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

type safetyRating struct {
	Category         string   `json:"category,omitempty"`
	Probability      string   `json:"probability,omitempty"`
	ProbabilityScore *float64 `json:"probabilityScore,omitempty"`
	Severity         string   `json:"severity,omitempty"`
	SeverityScore    *float64 `json:"severityScore,omitempty"`
	Blocked          *bool    `json:"blocked,omitempty"`
}
type promptFeedback struct {
	BlockReason   string         `json:"blockReason,omitempty"`
	SafetyRatings []safetyRating `json:"safetyRatings,omitempty"`
}
type usage struct {
	Input       *int64 `json:"promptTokenCount,omitempty"`
	CachedInput *int64 `json:"cachedContentTokenCount,omitempty"`
	Output      *int64 `json:"candidatesTokenCount,omitempty"`
	ToolInput   *int64 `json:"toolUsePromptTokenCount,omitempty"`
	Thoughts    *int64 `json:"thoughtsTokenCount,omitempty"`
	Total       *int64 `json:"totalTokenCount,omitempty"`
}
type candidate struct {
	Content       *content       `json:"content,omitempty"`
	Finish        string         `json:"finishReason,omitempty"`
	Index         *int           `json:"index,omitempty"`
	SafetyRatings []safetyRating `json:"safetyRatings,omitempty"`
	FinishMessage string         `json:"finishMessage,omitempty"`
}
type response struct {
	Candidates     []candidate     `json:"candidates,omitempty"`
	PromptFeedback *promptFeedback `json:"promptFeedback,omitempty"`
	Usage          *usage          `json:"usageMetadata,omitempty"`
	Model          string          `json:"modelVersion,omitempty"`
	ID             string          `json:"responseId,omitempty"`
	ModelStatus    json.RawMessage `json:"modelStatus,omitempty"`
}

func addCounts(a, b *int64) (*int64, error) {
	if a == nil && b == nil {
		return nil, nil
	}
	var x int64
	if a != nil {
		x = *a
	}
	if b != nil {
		const maxInt64 = int64(^uint64(0) >> 1)
		if *b > 0 && x > maxInt64-*b {
			return nil, invalid("usageMetadata")
		}
		x += *b
	}
	return &x, nil
}

func validUsage(u *usage) error {
	if u == nil {
		return nil
	}
	for _, n := range []*int64{u.Input, u.CachedInput, u.Output, u.ToolInput, u.Thoughts, u.Total} {
		if n != nil && *n < 0 {
			return invalid("usageMetadata")
		}
	}
	if u.Input != nil && u.Output != nil && u.Total != nil {
		out, e := addCounts(u.Output, u.Thoughts)
		if e != nil {
			return e
		}
		sum, e := addCounts(u.Input, out)
		if e != nil || sum == nil || *sum != *u.Total {
			return invalid("usageMetadata.totalTokenCount")
		}
	}
	return nil
}

func usageCore(u *usage) (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	if e := validUsage(u); e != nil {
		return nil, e
	}
	output, e := addCounts(u.Output, u.Thoughts)
	if e != nil {
		return nil, e
	}
	return &core.Usage{
		Input:           u.Input,
		Output:          output,
		Total:           u.Total,
		CachedInput:     u.CachedInput,
		ReasoningOutput: u.Thoughts,
		ToolInput:       u.ToolInput,
		Source:          "provider",
	}, nil
}

func blocked(ratings []safetyRating) bool {
	for _, rating := range ratings {
		if rating.Blocked != nil && *rating.Blocked {
			return true
		}
	}
	return false
}

func promptFinish(p *promptFeedback) (core.Finish, error) {
	if p == nil || p.BlockReason == "" {
		return core.Finish{}, invalid("promptFeedback.blockReason")
	}
	switch p.BlockReason {
	case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY":
		return core.Finish{Reason: "content_filter", Status: "content_filter"}, nil
	case "OTHER":
		return core.Finish{Reason: "error", Status: "error"}, nil
	case "BLOCK_REASON_UNSPECIFIED":
		return core.Finish{}, unsupported("promptFeedback.blockReason")
	default:
		return core.Finish{}, unsupported("promptFeedback.blockReason." + p.BlockReason)
	}
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
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "ESCALATION":
		reason = "content_filter"
		status = "content_filter"
	case "OTHER", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS", "MALFORMED_RESPONSE", "MISSING_THOUGHT_SIGNATURE":
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
	var e error
	r.Usage, e = usageCore(w.Usage)
	if e != nil {
		return r, e
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
		r.Finish, e = finish(c.Finish, tools)
		if e != nil {
			return r, e
		}
		if r.Finish.Reason == "" && blocked(c.SafetyRatings) {
			r.Finish = core.Finish{Reason: "content_filter", Status: "content_filter"}
		}
	} else if w.PromptFeedback != nil {
		r.Finish, e = promptFinish(w.PromptFeedback)
		if e != nil {
			return r, e
		}
	} else if terminal {
		return r, invalid("missing candidates")
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
func usageWire(u *core.Usage) (*usage, error) {
	if u == nil {
		return nil, nil
	}
	candidates := u.Output
	if u.ReasoningOutput != nil {
		if u.Output == nil || *u.ReasoningOutput > *u.Output {
			return nil, invalid("usage.reasoning_output")
		}
		n := *u.Output - *u.ReasoningOutput
		candidates = &n
	}
	out := &usage{
		Input:       u.Input,
		CachedInput: u.CachedInput,
		Output:      candidates,
		ToolInput:   u.ToolInput,
		Thoughts:    u.ReasoningOutput,
		Total:       u.Total,
	}
	if e := validUsage(out); e != nil {
		return nil, e
	}
	return out, nil
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
	w.Usage, e = usageWire(r.Usage)
	if e != nil {
		return w, e
	}
	return w, nil
}
func (*Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var w response
	if e := readResponseJSON(ctx, r, &w); e != nil {
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
	Model         string    `json:"model"`
	Contents      []content `json:"contents"`
	System        *content  `json:"systemInstruction,omitempty"`
	Tools         []tool    `json:"tools,omitempty"`
	Config        *config   `json:"generationConfig,omitempty"`
	CachedContent *string   `json:"cachedContent,omitempty"`
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
		v = request{Model: w.Generate.Model, Contents: w.Generate.Contents, System: w.Generate.System, Tools: w.Generate.Tools, Config: w.Generate.Config, CachedContent: w.Generate.CachedContent}
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
	if v.System == nil && len(v.Tools) == 0 && v.Config == nil && v.CachedContent == nil {
		return writeJSON(ctx, w, countRequest{Contents: v.Contents})
	}
	g := countGenerate{Model: v.Model, Contents: v.Contents, System: v.System, Tools: v.Tools, Config: v.Config, CachedContent: v.CachedContent}
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
