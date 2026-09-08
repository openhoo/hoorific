package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"hoorific/internal/core"
)

const maxBody = 16 << 20

func unsupported(param string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: param, Message: "Unsupported or unrepresentable Chat Completions field: " + param, Origin: "gateway"}
}

// Check tokens before unmarshalling: DisallowUnknownFields alone accepts duplicate keys.
func unique(d *json.Decoder, depth int) error {
	if depth > 256 {
		return unsupported("json.depth")
	}
	t, e := d.Token()
	if e != nil {
		return unsupported("json")
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
				return unsupported("json")
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return unsupported("json.duplicate_key")
			}
			seen[key] = true
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e = unique(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return unsupported("json")
	}
	_, e = d.Token()
	if e != nil {
		return unsupported("json")
	}
	return nil
}
func strict(b []byte, v any) error {
	if !utf8.Valid(b) {
		return unsupported("json.utf8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := unique(d, 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return unsupported("json.trailing")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return unsupported("json.fields")
	}
	return nil
}
func input(ctx context.Context, r io.Reader, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if r == nil {
		return unsupported("reader")
	}
	b, e := io.ReadAll(io.LimitReader(r, maxBody+1))
	if e != nil {
		return e
	}
	if len(b) > maxBody {
		return unsupported("body.size")
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	return strict(b, v)
}
func output(ctx context.Context, w io.Writer, v any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if w == nil {
		return unsupported("writer")
	}
	return json.NewEncoder(w).Encode(v)
}
func object(b []byte) error {
	var m map[string]json.RawMessage
	if e := strict(b, &m); e != nil {
		return e
	}
	if m == nil {
		return unsupported("schema")
	}
	return nil
}
func arguments(s string) error { return object([]byte(s)) }
func cleanBlock(b core.ContentBlock, kind string) bool {
	switch kind {
	case "text":
		return b.URL == "" && b.MIMEType == "" && b.ID == "" && b.Name == "" && b.Arguments == "" && len(b.Data) == 0
	case "tool_call":
		return b.Text == "" && b.URL == "" && b.MIMEType == "" && len(b.Data) == 0
	case "tool_result":
		return b.URL == "" && b.MIMEType == "" && b.Arguments == "" && len(b.Data) == 0
	case "image":
		return b.Text == "" && b.MIMEType == "" && b.ID == "" && b.Name == "" && b.Arguments == "" && len(b.Data) == 0 && b.URL != ""
	case "document":
		return b.Text == "" && b.Arguments == "" && b.ID == "" && b.URL != "" && (len(b.Data) == 0 || strings.HasPrefix(b.URL, "data:"))
	}
	return false
}
