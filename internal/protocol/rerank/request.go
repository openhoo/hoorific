package rerank

import (
	"context"
	"encoding/json"
	"io"

	"hoorific/internal/core"
)

func decodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	value, err := parseJSON(ctx, r)
	if err != nil {
		return nil, err
	}
	object, err := asObject(value, false)
	if err != nil {
		return nil, err
	}
	for name := range object {
		switch name {
		case "model", "query", "documents", "top_n":
		case "return_documents", "max_chunks_per_doc", "max_tokens_per_doc", "priority":
			return nil, unsupported(name)
		default:
			return nil, invalid(name, "unknown field")
		}
	}
	model, err := requiredString(object, "model", false)
	if err != nil {
		return nil, err
	}
	query, err := requiredString(object, "query", false)
	if err != nil {
		return nil, err
	}
	documentsValue, ok := object["documents"]
	if !ok {
		return nil, invalid("documents", "required field")
	}
	documentValues, ok := documentsValue.([]any)
	if !ok {
		return nil, invalid("documents", "must be an array of strings")
	}
	documents := make([]string, len(documentValues))
	for i, value := range documentValues {
		text, ok := value.(string)
		if !ok {
			return nil, invalid("documents", "document "+itoa(i)+" is not a string")
		}
		documents[i] = text
	}
	var topN *int
	if value, ok := object["top_n"]; ok {
		number, ok := value.(json.Number)
		if !ok {
			return nil, invalid("top_n", "must be an integer")
		}
		parsed, err := number.Int64()
		if err != nil || int64(int(parsed)) != parsed {
			return nil, invalid("top_n", "must be an integer representable by this gateway")
		}
		n := int(parsed)
		topN = &n
	}
	return core.RerankRequest{Model: model, Query: query, Documents: documents, TopN: topN}, nil
}

func encodeRequest(ctx context.Context, payload core.RequestPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return invalid("body", "nil writer")
	}
	request, ok := payload.(core.RerankRequest)
	if !ok {
		if pointer, pointerOK := payload.(*core.RerankRequest); pointerOK && pointer != nil {
			request, ok = *pointer, true
		}
	}
	if !ok {
		return invalid("payload", "must be core.RerankRequest")
	}

	body := struct {
		Model     string   `json:"model"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
		TopN      *int     `json:"top_n,omitempty"`
	}{request.Model, request.Query, request.Documents, request.TopN}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = w.Write(encoded)
	return err
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [24]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
