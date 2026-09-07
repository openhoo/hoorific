package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"hoorific/internal/core"
)

func invalid(field, message string) error {
	if field != "" {
		message = field + ": " + message
	}
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Param: field, Message: message, Origin: "gateway"}
}

func invalidResponse(field, message string) error {
	if field != "" {
		message = field + ": " + message
	}
	return core.GatewayError{Code: "invalid_response", HTTPStatus: 400, Param: field, Message: message, Origin: "gateway"}
}

func parseJSON(ctx context.Context, r io.Reader) (any, error) {
	if r == nil {
		return nil, invalid("body", "nil reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	value, err := parseValue(ctx, dec)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if token, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, invalid("body", fmt.Sprintf("trailing JSON value %v", token))
		}
		return nil, invalid("body", "trailing JSON")
	}
	return value, nil
}

func parseValue(ctx context.Context, dec *json.Decoder) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := dec.Token()
	if err != nil {
		return nil, invalid("body", "malformed JSON: "+err.Error())
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			result := make(map[string]any)
			seen := make(map[string]struct{})
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, invalid("body", "malformed object")
				}
				name, ok := key.(string)
				if !ok {
					return nil, invalid("body", "object key is not a string")
				}
				if _, ok := seen[name]; ok {
					return nil, invalid(name, "duplicate field")
				}
				seen[name] = struct{}{}
				child, err := parseValue(ctx, dec)
				if err != nil {
					return nil, err
				}
				result[name] = child
			}
			if _, err := dec.Token(); err != nil {
				return nil, invalid("body", "unterminated object")
			}
			return result, nil
		case '[':
			result := make([]any, 0)
			for dec.More() {
				child, err := parseValue(ctx, dec)
				if err != nil {
					return nil, err
				}
				result = append(result, child)
			}
			if _, err := dec.Token(); err != nil {
				return nil, invalid("body", "unterminated array")
			}
			return result, nil
		default:
			return nil, invalid("body", "unexpected delimiter")
		}
	case string, bool, nil, json.Number:
		return value, nil
	default:
		return nil, invalid("body", "unsupported JSON value")
	}
}

func asObject(value any, response bool) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		if response {
			return nil, invalidResponse("body", "must be a JSON object")
		}
		return nil, invalid("body", "must be a JSON object")
	}
	return object, nil
}

func requiredString(object map[string]any, name string, response bool) (string, error) {
	value, ok := object[name]
	if !ok {
		return "", fieldError(name, "required field", response)
	}
	text, ok := value.(string)
	if !ok {
		return "", fieldError(name, "must be a string", response)
	}
	return text, nil
}

func fieldError(field, message string, response bool) error {
	if response {
		return invalidResponse(field, message)
	}
	return invalid(field, message)
}
