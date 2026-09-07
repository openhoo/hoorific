package embedding

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"io"
	"strings"
)

type openAIRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	EncodingFormat string `json:"encoding_format,omitempty"`
}
type geminiPart struct {
	Text string `json:"text"`
}
type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}
type geminiRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	OutputDimensionality *int          `json:"outputDimensionality,omitempty"`
}
type geminiSingleRequest struct {
	Content              geminiContent `json:"content"`
	OutputDimensionality *int          `json:"outputDimensionality,omitempty"`
}
type geminiBatchRequest struct {
	Requests []geminiRequest `json:"requests"`
}
type cohereV1Request struct {
	Model           string   `json:"model"`
	Texts           []string `json:"texts"`
	OutputDimension *int     `json:"output_dimension,omitempty"`
}
type cohereV2Request struct {
	Model           string   `json:"model"`
	Texts           []string `json:"texts"`
	OutputDimension *int     `json:"output_dimension,omitempty"`
}
type ollamaRequest struct {
	Model      string `json:"model"`
	Input      any    `json:"input"`
	Dimensions *int   `json:"dimensions,omitempty"`
}
type ollamaLegacyRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, invalid("body", "nil reader")
	}
	variant, err := c.selected()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	switch variant {
	case "openai":
		var w struct {
			Model          string          `json:"model"`
			Input          json.RawMessage `json:"input"`
			Dimensions     *int            `json:"dimensions"`
			EncodingFormat string          `json:"encoding_format"`
		}
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if len(w.Input) == 0 {
			return nil, missing("input")
		}
		var one string
		var many []string
		if json.Unmarshal(w.Input, &one) == nil {
			many = []string{one}
		} else if err := json.Unmarshal(w.Input, &many); err != nil {
			return nil, invalid("input", "must be string or string array")
		}
		if err := validateStrings(many); err != nil {
			return nil, err
		}
		if w.EncodingFormat != "" && w.EncodingFormat != "float" && w.EncodingFormat != "base64" {
			return nil, unsupported("encoding_format", "unsupported encoding")
		}
		return core.EmbeddingRequest{Model: w.Model, Inputs: many, Dimensions: w.Dimensions, Encoding: defaultEncoding(w.EncodingFormat)}, nil
	case "gemini":
		var w geminiSingleRequest
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if len(w.Content.Parts) != 1 || w.Content.Parts[0].Text == "" {
			return nil, invalid("content", "exactly one text part is required")
		}
		return core.EmbeddingRequest{Inputs: []string{w.Content.Parts[0].Text}, Dimensions: w.OutputDimensionality, Encoding: "float"}, nil
	case "gemini-batch":
		var w geminiBatchRequest
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if len(w.Requests) == 0 {
			return nil, invalid("requests", "must not be empty")
		}
		out := core.EmbeddingRequest{Encoding: "float", Inputs: make([]string, len(w.Requests))}
		for i, q := range w.Requests {
			if len(q.Content.Parts) != 1 || q.Content.Parts[0].Text == "" {
				return nil, invalid("requests", "each request requires one text part")
			}
			if i == 0 {
				out.Model = normalizeGeminiModel(q.Model)
				out.Dimensions = q.OutputDimensionality
			} else if normalizeGeminiModel(q.Model) != out.Model || !sameInt(q.OutputDimensionality, out.Dimensions) {
				return nil, invalid("requests", "model and dimensions must be consistent")
			}
			out.Inputs[i] = q.Content.Parts[0].Text
		}
		return out, nil
	case "cohere-v1":
		var w cohereV1Request
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if err := validateStrings(w.Texts); err != nil {
			return nil, err
		}
		return core.EmbeddingRequest{Model: w.Model, Inputs: w.Texts, Dimensions: w.OutputDimension, Encoding: "float"}, nil
	case "cohere-v2":
		var w cohereV2Request
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if err := validateStrings(w.Texts); err != nil {
			return nil, err
		}
		return core.EmbeddingRequest{Model: w.Model, Inputs: w.Texts, Dimensions: w.OutputDimension, Encoding: "float"}, nil
	case "ollama":
		var w struct {
			Model      string          `json:"model"`
			Input      json.RawMessage `json:"input"`
			Dimensions *int            `json:"dimensions"`
		}
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		var one string
		var many []string
		if json.Unmarshal(w.Input, &one) == nil {
			many = []string{one}
		} else if err := json.Unmarshal(w.Input, &many); err != nil {
			return nil, invalid("input", "must be string or string array")
		}
		if err := validateStrings(many); err != nil {
			return nil, err
		}
		return core.EmbeddingRequest{Model: w.Model, Inputs: many, Dimensions: w.Dimensions, Encoding: "float"}, nil
	case "ollama-legacy":
		var w ollamaLegacyRequest
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if w.Prompt == "" {
			return nil, invalid("prompt", "must not be empty")
		}
		return core.EmbeddingRequest{Model: w.Model, Inputs: []string{w.Prompt}, Encoding: "float"}, nil
	}
	panic("unreachable")
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	variant, err := c.selected()
	if err != nil {
		return err
	}
	if w == nil {
		return invalid("body", "nil writer")
	}
	var req core.EmbeddingRequest
	switch x := p.(type) {
	case core.EmbeddingRequest:
		req = x
	case *core.EmbeddingRequest:
		if x == nil {
			return invalid("payload", "expected core.EmbeddingRequest")
		}
		req = *x
	default:
		return invalid("payload", "expected core.EmbeddingRequest")
	}
	if err := validateRequest(req, variant); err != nil {
		return err
	}
	var out any
	switch variant {
	case "openai":
		in := any(req.Inputs)
		if len(req.Inputs) == 1 {
			in = req.Inputs[0]
		}
		out = openAIRequest{Model: req.Model, Input: in, Dimensions: req.Dimensions, EncodingFormat: omitDefaultEncoding(req.Encoding)}
	case "gemini":
		out = geminiSingleRequest{Content: geminiContent{Parts: []geminiPart{{Text: req.Inputs[0]}}}, OutputDimensionality: req.Dimensions}
	case "gemini-batch":
		qs := make([]geminiRequest, len(req.Inputs))
		for i, s := range req.Inputs {
			qs[i] = geminiRequest{Model: geminiModel(req.Model), Content: geminiContent{Parts: []geminiPart{{Text: s}}}, OutputDimensionality: req.Dimensions}
		}
		out = geminiBatchRequest{Requests: qs}
	case "cohere-v1":
		out = cohereV1Request{Model: req.Model, Texts: req.Inputs, OutputDimension: req.Dimensions}
	case "cohere-v2":
		out = cohereV2Request{Model: req.Model, Texts: req.Inputs, OutputDimension: req.Dimensions}
	case "ollama":
		in := any(req.Inputs)
		if len(req.Inputs) == 1 {
			in = req.Inputs[0]
		}
		out = ollamaRequest{Model: req.Model, Input: in, Dimensions: req.Dimensions}
	case "ollama-legacy":
		out = ollamaLegacyRequest{Model: req.Model, Prompt: req.Inputs[0]}
	}
	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}

func validateRequest(r core.EmbeddingRequest, v string) error {
	if len(r.Inputs) == 0 {
		return invalid("inputs", "must not be empty")
	}
	if err := validateStrings(r.Inputs); err != nil {
		return err
	}
	if r.Dimensions != nil && *r.Dimensions <= 0 {
		return invalid("dimensions", "must be positive")
	}
	enc := defaultEncoding(r.Encoding)
	switch v {
	case "openai":
		if enc != "float" && enc != "base64" {
			return unsupported("encoding", "unsupported encoding")
		}
	default:
		if enc != "float" {
			return unsupported("encoding", "variant only supports float vectors")
		}
		if v == "gemini" && len(r.Inputs) != 1 {
			return invalid("inputs", "single embedContent accepts one input")
		}
		if v == "ollama-legacy" && len(r.Inputs) != 1 {
			return invalid("inputs", "legacy endpoint accepts one input")
		}
		if v == "ollama-legacy" && r.Dimensions != nil {
			return unsupported("dimensions", "legacy endpoint cannot represent dimensions")
		}
	}
	return nil
}
func validateStrings(xs []string) error {
	for _, s := range xs {
		if s == "" {
			return invalid("inputs", "text must not be empty")
		}
	}
	return nil
}
func defaultEncoding(s string) string {
	if s == "" {
		return "float"
	}
	return s
}
func omitDefaultEncoding(s string) string {
	if s == "float" || s == "" {
		return ""
	}
	return s
}
func normalizeGeminiModel(s string) string { return strings.TrimPrefix(s, "models/") }
func geminiModel(s string) string {
	if strings.HasPrefix(s, "models/") {
		return s
	}
	return "models/" + s
}
func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func missing(f string) error { return invalid(f, "required field is missing") }
func invalid(f, m string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Param: f, Message: "embedding " + f + ": " + m, Origin: "gateway"}
}
