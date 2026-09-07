package openairesponses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"io"
)

type object map[string]json.RawMessage

func unsupported(p string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: p, Message: "Unsupported or invalid Responses field: " + p, Origin: "gateway"}
}

// walk checks every object, including objects inside opaque schemas.
func walk(d *json.Decoder, depth int) error {
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
			s, ok := k.(string)
			if !ok || seen[s] {
				return unsupported("json.duplicate_key")
			}
			seen[s] = true
			if e = walk(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := walk(d, depth+1); e != nil {
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
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := walk(d, 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return unsupported("json.trailing")
	}
	if e := json.Unmarshal(b, v); e != nil {
		return unsupported("json")
	}
	return nil
}
func load(r io.Reader, v any) error {
	b, e := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if e != nil {
		return e
	}
	if len(b) > 32<<20 {
		return unsupported("json.size")
	}
	return strict(b, v)
}
func fields(o object, allowed ...string) error {
	if o == nil {
		return unsupported("object")
	}
	for k := range o {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return unsupported(k)
		}
	}
	return nil
}
func get(o object, k string, v any, required bool) error {
	b, ok := o[k]
	if !ok {
		if required {
			return unsupported(k)
		}
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return unsupported(k)
	}
	if e := json.Unmarshal(b, v); e != nil {
		return unsupported(k)
	}
	return nil
}
func str(o object, k string) (string, error) { var s string; e := get(o, k, &s, true); return s, e }
func typ(o object) string                    { var s string; _ = get(o, "type", &s, false); return s }
func obj(b json.RawMessage) (object, error) {
	var o object
	e := strict(b, &o)
	if e == nil && o == nil {
		e = unsupported("object")
	}
	return o, e
}
func raw(v any) json.RawMessage           { b, _ := json.Marshal(v); return b }
func encode(w io.Writer, v any) error     { return json.NewEncoder(w).Encode(v) }
func indexID(prefix string, n int) string { return fmt.Sprintf("%s_%d", prefix, n) }
