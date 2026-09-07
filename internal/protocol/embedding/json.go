package embedding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"io"
)

// strictDecode rejects duplicate keys at every nesting level, unknown struct
// fields, and any non-whitespace after the top-level JSON value.
func strictDecode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(d); err != nil {
		return malformed(err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return malformed(fmt.Errorf("trailing JSON"))
		}
		return malformed(err)
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return malformed(err)
	}
	if err := d.Decode(&extra); err != io.EOF {
		return malformed(fmt.Errorf("trailing JSON"))
	}
	return nil
}

func scanValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch x := t.(type) {
	case json.Delim:
		if x == '{' {
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				if !ok {
					return fmt.Errorf("object key is not string")
				}
				if seen[k] {
					return fmt.Errorf("duplicate field %q", k)
				}
				seen[k] = true
				if err := scanValue(d); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		if x == '[' {
			for d.More() {
				if err := scanValue(d); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
	}
	return nil
}

func malformed(err error) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Message: "malformed embedding JSON", Origin: "gateway"}
}
