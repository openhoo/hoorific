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
		case "id", "meta", "api_version", "billed_units", "documents":
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
	return core.RerankResult{Results: results}, nil
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
	if result.Usage != nil {
		return unsupportedResult("usage")
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
