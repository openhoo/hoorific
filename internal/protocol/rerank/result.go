package rerank

import (
	"context"
	"encoding/json"
	"io"
	"math"

	"hoorific/internal/core"
)

func decodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	value, err := parseJSON(ctx, r)
	if err != nil {
		return nil, err
	}
	object, err := asObject(value, true)
	if err != nil {
		return nil, err
	}
	for name := range object {
		switch name {
		case "results":
		case "id", "meta", "api_version", "billed_units":
			// Documented response metadata is optional and does not affect the
			// representable ranked-document result.
		case "documents":
			return nil, unsupportedResult(name)
		default:
			return nil, invalidResponse(name, "unknown field")
		}
	}
	values, ok := object["results"]
	if !ok {
		return nil, invalidResponse("results", "required field")
	}
	items, ok := values.([]any)
	if !ok {
		return nil, invalidResponse("results", "must be an array")
	}
	results := make([]core.RankedDocument, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, invalidResponse("results", "entry "+itoa(i)+" must be an object")
		}
		for name := range entry {
			switch name {
			case "index", "relevance_score":
			case "document":
				return nil, unsupportedResult("results.document")
			default:
				return nil, invalidResponse("results."+name, "unknown field")
			}
		}
		index, err := responseInt(entry, "index")
		if err != nil {
			return nil, err
		}
		scoreValue, ok := entry["relevance_score"]
		if !ok {
			return nil, invalidResponse("relevance_score", "required field")
		}
		scoreNumber, ok := scoreValue.(json.Number)
		if !ok {
			return nil, invalidResponse("relevance_score", "must be a number")
		}
		score, err := scoreNumber.Float64()
		if err != nil || math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, invalidResponse("relevance_score", "must be a finite number")
		}
		results[i] = core.RankedDocument{Index: index, Score: score}
	}
	usage, err := usageFromMetadata(object)
	if err != nil {
		return nil, err
	}
	return core.RerankResult{Results: results, Usage: usage}, nil
}

func usageFromMetadata(object map[string]any) (*core.Usage, error) {
	for _, name := range []string{"billed_units", "meta"} {
		if value, ok := object[name].(map[string]any); ok {
			if u, found, err := usageFromMap(value); found || err != nil {
				return u, err
			}
		}
	}
	return nil, nil
}

func usageFromMap(object map[string]any) (*core.Usage, bool, error) {
	if nested, ok := object["tokens"].(map[string]any); ok {
		if u, found, err := usageFromMap(nested); found || err != nil {
			return u, found, err
		}
	}
	input, inputFound, err := usageNumber(object, "input_tokens")
	if err != nil {
		return nil, true, err
	}
	output, outputFound, err := usageNumber(object, "output_tokens")
	if err != nil {
		return nil, true, err
	}
	if !inputFound && !outputFound {
		return nil, false, nil
	}
	u := &core.Usage{Source: "cohere"}
	if inputFound {
		u.Input = &input
	}
	if outputFound {
		u.Output = &output
	}
	if inputFound && outputFound {
		const maxInt64 = int64(^uint64(0) >> 1)
		if output > maxInt64-input {
			return nil, true, invalidResponse("usage", "token count overflow")
		}
		total := input + output
		u.Total = &total
	}
	return u, true, nil
}

func usageNumber(object map[string]any, name string) (int64, bool, error) {
	value, ok := object[name]
	if !ok {
		return 0, false, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, true, invalidResponse("usage."+name, "must be an integer")
	}
	n, err := number.Int64()
	if err != nil || n < 0 {
		return 0, true, invalidResponse("usage."+name, "must be a non-negative integer")
	}
	return n, true, nil
}
func encodeResult(ctx context.Context, payload core.ResultPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return invalidResponse("body", "nil writer")
	}
	result, ok := payload.(core.RerankResult)
	if !ok {
		if pointer, pointerOK := payload.(*core.RerankResult); pointerOK && pointer != nil {
			result, ok = *pointer, true
		}
	}
	if !ok {
		return invalidResponse("payload", "must be core.RerankResult")
	}
	type wireResult struct {
		Index int     `json:"index"`
		Score float64 `json:"relevance_score"`
	}
	items := make([]wireResult, len(result.Results))
	for i, item := range result.Results {
		if math.IsNaN(item.Score) || math.IsInf(item.Score, 0) {
			return invalidResponse("results."+itoa(i)+".relevance_score", "must be finite")
		}
		items[i] = wireResult{Index: item.Index, Score: item.Score}
	}
	encoded, err := json.Marshal(struct {
		Results []wireResult `json:"results"`
	}{items})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = w.Write(encoded)
	return err
}

func responseInt(object map[string]any, field string) (int, error) {
	value, ok := object[field]
	if !ok {
		return 0, invalidResponse(field, "required field")
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, invalidResponse(field, "must be an integer")
	}
	parsed, err := number.Int64()
	if err != nil || int64(int(parsed)) != parsed {
		return 0, invalidResponse(field, "must be an integer representable by this gateway")
	}
	return int(parsed), nil
}

func unsupportedResult(field string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: field, Message: "Rerank result field cannot be preserved", Origin: "gateway"}
}
