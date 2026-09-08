package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"hoorific/internal/core"
)

func unsupported(path string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: path, Message: "Unsupported or invalid Anthropic field: " + path, Origin: "gateway"}
}

// strict checks every object, including objects embedded in tool schemas.
func validateJSON(data []byte) error {
	if !utf8.Valid(data) {
		return unsupported("json")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 256 {
			return unsupported("json.depth")
		}
		t, err := d.Token()
		if err != nil {
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
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		default:
			return unsupported("json")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return unsupported("json.trailing")
	}
	return nil
}

func strict(data []byte, dst any) error {
	if err := validateJSON(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return unsupported("json.fields")
	}
	return nil
}

// tolerant validates the response envelope's JSON shape and duplicate keys,
// while allowing newly documented provider metadata that this core does not
// represent. Callers still validate all semantic fields they consume.
func tolerant(data []byte, dst any) error {
	if err := validateJSON(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return unsupported("json.fields")
	}
	return nil
}
func readResponseJSON(r io.Reader, dst any) error {
	b, e := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if e != nil {
		return e
	}
	if len(b) > 32<<20 {
		return unsupported("body.size")
	}
	return tolerant(b, dst)
}
func object(data []byte) error {
	var v map[string]json.RawMessage
	if err := strict(data, &v); err != nil {
		return err
	}
	if v == nil {
		return unsupported("json.object")
	}
	return nil
}
func readJSON(r io.Reader, dst any) error {
	b, e := io.ReadAll(io.LimitReader(r, 32<<20+1))
	if e != nil {
		return e
	}
	if len(b) > 32<<20 {
		return unsupported("body.size")
	}
	return strict(b, dst)
}
func emitJSON(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return unsupported("json")
	}
	if e = strict(b, new(any)); e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func required(s, path string) error {
	if s == "" {
		return unsupported(path)
	}
	return nil
}
func field(base string, i int) string { return fmt.Sprintf("%s[%d]", base, i) }
