package embedding

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"io"
	"math"
)

type openAIData struct {
	Object    string          `json:"object"`
	Embedding json.RawMessage `json:"embedding"`
	Index     int             `json:"index"`
}
type openAIUsageDetails struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
}
type openAIUsage struct {
	PromptTokens        *int64              `json:"prompt_tokens"`
	TotalTokens         *int64              `json:"total_tokens"`
	CompletionTokens    *int64              `json:"completion_tokens"`
	PromptTokensDetails *openAIUsageDetails `json:"prompt_tokens_details,omitempty"`
}
type openAIResult struct {
	Object string       `json:"object"`
	Data   []openAIData `json:"data"`
	Model  string       `json:"model"`
	Usage  *openAIUsage `json:"usage"`
}
type geminiEmbedding struct {
	Values []float64 `json:"values"`
}
type geminiResult struct {
	Embedding     geminiEmbedding `json:"embedding"`
	UsageMetadata *geminiUsage    `json:"usageMetadata"`
}
type geminiUsage struct {
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount,omitempty"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	ToolUsePromptTokenCount *int64 `json:"toolUsePromptTokenCount,omitempty"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         *int64 `json:"totalTokenCount"`
}
type geminiBatchResult struct {
	Embeddings    []geminiEmbedding `json:"embeddings"`
	UsageMetadata *geminiUsage      `json:"usageMetadata"`
}
type cohereV1Result struct {
	ID           string          `json:"id"`
	ResponseType string          `json:"response_type"`
	Embeddings   [][]float64     `json:"embeddings"`
	Meta         json.RawMessage `json:"meta"`
}
type cohereV2Embeddings struct {
	Float  [][]float64 `json:"float"`
	Int8   [][]int8    `json:"int8"`
	Uint8  [][]uint8   `json:"uint8"`
	Binary []string    `json:"binary"`
}
type cohereV2Result struct {
	ID         string             `json:"id"`
	Embeddings cohereV2Embeddings `json:"embeddings"`
	Meta       json.RawMessage    `json:"meta"`
}
type ollamaResult struct {
	Model              string      `json:"model"`
	Embeddings         [][]float64 `json:"embeddings"`
	TotalDuration      *int64      `json:"total_duration"`
	LoadDuration       *int64      `json:"load_duration"`
	PromptEvalCount    *int64      `json:"prompt_eval_count"`
	PromptEvalDuration *int64      `json:"prompt_eval_duration"`
}
type ollamaLegacyResult struct {
	Model              string    `json:"model"`
	Embedding          []float64 `json:"embedding"`
	TotalDuration      *int64    `json:"total_duration"`
	LoadDuration       *int64    `json:"load_duration"`
	PromptEvalCount    *int64    `json:"prompt_eval_count"`
	PromptEvalDuration *int64    `json:"prompt_eval_duration"`
}

func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
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
		var w openAIResult
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		return decodeOpenAI(w)
	case "gemini":
		var w geminiResult
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		u, err := geminiUsageCore(w.UsageMetadata)
		if err != nil {
			return nil, err
		}
		return makeFloatResult("", [][]float64{w.Embedding.Values}, u)
	case "gemini-batch":
		var w geminiBatchResult
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		u, err := geminiUsageCore(w.UsageMetadata)
		if err != nil {
			return nil, err
		}
		return makeFloatResult("", geminiValues(w.Embeddings), u)
	case "cohere-v1":
		var w cohereV1Result
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		u, err := cohereUsageCore(w.Meta)
		if err != nil {
			return nil, err
		}
		return makeFloatResult("", w.Embeddings, u)
	case "cohere-v2":
		var w cohereV2Result
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		if len(w.Embeddings.Int8) > 0 || len(w.Embeddings.Uint8) > 0 || len(w.Embeddings.Binary) > 0 {
			return nil, unsupported("embeddings", "only float vectors are supported")
		}
		u, err := cohereUsageCore(w.Meta)
		if err != nil {
			return nil, err
		}
		return makeFloatResult("", w.Embeddings.Float, u)
	case "ollama":
		var w ollamaResult
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		var u *core.Usage
		if w.PromptEvalCount != nil {
			u = &core.Usage{Input: w.PromptEvalCount, Source: "ollama"}
		}
		return makeFloatResult(w.Model, w.Embeddings, u)
	case "ollama-legacy":
		var w ollamaLegacyResult
		if err := strictDecode(data, &w); err != nil {
			return nil, err
		}
		var u *core.Usage
		if w.PromptEvalCount != nil {
			u = &core.Usage{Input: w.PromptEvalCount, Source: "ollama"}
		}
		return makeFloatResult(w.Model, [][]float64{w.Embedding}, u)
	}
	panic("unreachable")
}

func decodeOpenAI(w openAIResult) (core.ResultPayload, error) {
	out := core.EmbeddingResult{Model: w.Model}
	if w.Usage != nil && (w.Usage.PromptTokens != nil || w.Usage.CompletionTokens != nil || w.Usage.TotalTokens != nil || w.Usage.PromptTokensDetails != nil) {
		var cached *int64
		if w.Usage.PromptTokensDetails != nil {
			cached = w.Usage.PromptTokensDetails.CachedTokens
		}
		out.Usage = &core.Usage{Input: w.Usage.PromptTokens, Output: w.Usage.CompletionTokens, Total: w.Usage.TotalTokens, CachedInput: cached, Source: "openai"}
	}
	if len(w.Data) == 0 {
		return out, nil
	}
	mode := ""
	for _, d := range w.Data {
		if len(d.Embedding) == 0 {
			return nil, invalid("embedding", "missing vector")
		}
		var nums []float64
		if json.Unmarshal(d.Embedding, &nums) == nil && nums != nil {
			if mode == "base64" {
				return nil, invalid("data", "mixed vector representations")
			}
			mode = "float"
			out.Embeddings = append(out.Embeddings, core.Embedding{Index: d.Index, Vector: nums})
		} else {
			var s string
			if json.Unmarshal(d.Embedding, &s) != nil {
				return nil, invalid("embedding", "must be float array or base64 string")
			}
			if mode == "float" {
				return nil, invalid("data", "mixed vector representations")
			}
			mode = "base64"
			if _, err := decodeBase64Vector(s); err != nil {
				return nil, invalid("embedding", "invalid base64 vector")
			}
			out.Embeddings = append(out.Embeddings, core.Embedding{Index: d.Index, Encoded: s})
		}
	}
	out.Encoding = mode
	dims, err := checkDimensions(out.Embeddings, mode)
	if err != nil {
		return nil, err
	}
	out.Dimensions = dims
	return out, nil
}

func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return invalid("body", "nil writer")
	}
	v, err := c.selected()
	if err != nil {
		return err
	}
	var r core.EmbeddingResult
	var ok bool
	switch x := p.(type) {
	case core.EmbeddingResult:
		r = x
		ok = true
	case *core.EmbeddingResult:
		if x != nil {
			r = *x
			ok = true
		}
	}
	if !ok {
		return invalid("payload", "expected core.EmbeddingResult")
	}
	if err := validateResult(r, v); err != nil {
		return err
	}
	var out any
	switch v {
	case "openai":
		data := make([]openAIData, len(r.Embeddings))
		for i, e := range r.Embeddings {
			raw, _ := json.Marshal(e.Vector)
			if r.Encoding == "base64" {
				raw, _ = json.Marshal(e.Encoded)
			}
			data[i] = openAIData{Object: "embedding", Embedding: raw, Index: e.Index}
		}
		var u *openAIUsage
		if r.Usage != nil {
			u = &openAIUsage{PromptTokens: r.Usage.Input, CompletionTokens: r.Usage.Output, TotalTokens: r.Usage.Total}
			if r.Usage.CachedInput != nil {
				u.PromptTokensDetails = &openAIUsageDetails{CachedTokens: r.Usage.CachedInput}
			}
		}
		out = openAIResult{Object: "list", Data: data, Model: r.Model, Usage: u}
	case "gemini":
		out = geminiResult{Embedding: geminiEmbedding{Values: r.Embeddings[0].Vector}, UsageMetadata: geminiUsageWire(r.Usage)}
	case "gemini-batch":
		es := make([]geminiEmbedding, len(r.Embeddings))
		for i, e := range r.Embeddings {
			es[i] = geminiEmbedding{Values: e.Vector}
		}
		out = geminiBatchResult{Embeddings: es, UsageMetadata: geminiUsageWire(r.Usage)}
	case "cohere-v1":
		out = cohereV1Result{Embeddings: vectors(r.Embeddings), Meta: cohereUsageWire(r.Usage)}
	case "cohere-v2":
		out = cohereV2Result{Embeddings: cohereV2Embeddings{Float: vectors(r.Embeddings)}, Meta: cohereUsageWire(r.Usage)}
	case "ollama":
		var u *int64
		if r.Usage != nil {
			u = r.Usage.Input
		}
		out = ollamaResult{Model: r.Model, Embeddings: vectors(r.Embeddings), PromptEvalCount: u}
	case "ollama-legacy":
		var u *int64
		if r.Usage != nil {
			u = r.Usage.Input
		}
		out = ollamaLegacyResult{Model: r.Model, Embedding: r.Embeddings[0].Vector, PromptEvalCount: u}
	}

	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func cohereUsageWire(u *core.Usage) json.RawMessage {
	if u == nil {
		return nil
	}
	type tokenUsage struct {
		Input  *int64 `json:"input_tokens,omitempty"`
		Output *int64 `json:"output_tokens,omitempty"`
	}
	wire := struct {
		Tokens *tokenUsage `json:"tokens,omitempty"`
	}{Tokens: &tokenUsage{Input: u.Input, Output: u.Output}}
	b, err := json.Marshal(wire)
	if err != nil {
		return nil
	}
	return b
}

func cohereUsageCore(raw json.RawMessage) (*core.Usage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil || meta == nil {
		return nil, invalid("meta", "must be an object")
	}
	for _, key := range []string{"tokens", "billed_units"} {
		var section map[string]json.RawMessage
		value, ok := meta[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, &section); err != nil || section == nil {
			return nil, invalid("meta."+key, "must be an object")
		}
		input, hasInput, err := cohereCount(section, "input_tokens")
		if err != nil {
			return nil, err
		}
		output, hasOutput, err := cohereCount(section, "output_tokens")
		if err != nil {
			return nil, err
		}
		if !hasInput && !hasOutput {
			continue
		}
		u := &core.Usage{Source: "cohere"}
		if hasInput {
			u.Input = input
		}
		if hasOutput {
			u.Output = output
		}
		if hasInput && hasOutput {
			const maxInt64 = int64(^uint64(0) >> 1)
			if *input > maxInt64-*output {
				return nil, invalid("meta", "token count overflow")
			}
			total := *input + *output
			u.Total = &total
		}
		return u, nil
	}
	return nil, nil
}

func cohereCount(section map[string]json.RawMessage, key string) (*int64, bool, error) {
	raw, ok := section[key]
	if !ok {
		return nil, false, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return nil, true, invalid("meta."+key, "must be a non-negative integer")
	}
	return &n, true, nil
}

func validUsage(u *core.Usage) error {
	if u == nil {
		return nil
	}
	for _, n := range []*int64{
		u.Input, u.Output, u.Total, u.CachedInput, u.CacheWriteInput,
		u.CacheWrite5mInput, u.CacheWrite1hInput, u.ReasoningOutput, u.ToolInput,
	} {
		if n != nil && *n < 0 {
			return invalid("usage", "counts must be non-negative")
		}
	}
	if u.Input != nil && u.CachedInput != nil && *u.CachedInput > *u.Input {
		return invalid("usage.cached_input", "exceeds input")
	}
	if u.Input != nil && u.ToolInput != nil && *u.ToolInput > *u.Input {
		return invalid("usage.tool_input", "exceeds input")
	}
	if u.Output != nil && u.ReasoningOutput != nil && *u.ReasoningOutput > *u.Output {
		return invalid("usage.reasoning_output", "exceeds output")
	}
	if u.Input != nil && u.Output != nil && u.Total != nil && *u.Total != *u.Input+*u.Output {
		return invalid("usage.total", "does not match input plus output")
	}
	return nil
}

func validateResult(r core.EmbeddingResult, v string) error {
	if r.Encoding == "" {
		r.Encoding = "float"
	}
	if r.Encoding != "float" && r.Encoding != "base64" {
		return unsupported("encoding", "unsupported encoding")
	}
	if v != "openai" && r.Encoding != "float" {
		return unsupported("encoding", "variant only supports float vectors")
	}
	if v == "gemini" && len(r.Embeddings) != 1 {
		return invalid("embeddings", "single result requires one embedding")
	}
	if v == "ollama-legacy" && len(r.Embeddings) != 1 {
		return invalid("embeddings", "legacy result requires one embedding")
	}
	if err := validUsage(r.Usage); err != nil {
		return err
	}
	if r.Usage != nil && r.Usage.Source != "" && !sourceAllowed(r.Usage.Source, v) {
		return unsupported("usage.source", "usage source cannot be represented")
	}
	for i, e := range r.Embeddings {
		if r.Encoding == "base64" {
			if e.Vector != nil || e.Encoded == "" {
				return invalid("embeddings", "base64 results require encoded vectors only")
			}
			b, err := decodeBase64Vector(e.Encoded)
			if err != nil || len(b)/4 != r.Dimensions {
				return invalid("embeddings", "invalid base64 vector or dimensions")
			}
		} else if e.Encoded != "" || e.Vector == nil {
			return invalid("embeddings", "float results require float vectors only")
		}
		if v != "openai" && e.Index != i {
			return invalid("index", "positional variant requires sequential indexes")
		}
	}
	if (v == "gemini" || v == "gemini-batch") && r.Usage != nil && r.Usage.Source != "" && r.Usage.Source != "gemini" {
		return unsupported("usage.source", "usage source cannot be represented")
	}
	return nil
}
func sourceAllowed(s, v string) bool {
	return s == v || ((v == "gemini" || v == "gemini-batch") && s == "gemini")
}
func vectors(es []core.Embedding) [][]float64 {
	out := make([][]float64, len(es))
	for i, e := range es {
		out[i] = e.Vector
	}
	return out
}
func geminiValues(es []geminiEmbedding) [][]float64 {
	out := make([][]float64, len(es))
	for i, e := range es {
		out[i] = e.Values
	}
	return out
}
func makeFloatResult(model string, vs [][]float64, u *core.Usage) (core.ResultPayload, error) {
	es := make([]core.Embedding, len(vs))
	for i, v := range vs {
		es[i] = core.Embedding{Index: i, Vector: v}
	}
	dims, err := checkDimensions(es, "float")
	if err != nil {
		return nil, err
	}
	return core.EmbeddingResult{Model: model, Encoding: "float", Dimensions: dims, Embeddings: es, Usage: u}, nil
}
func checkDimensions(es []core.Embedding, mode string) (int, error) {
	if len(es) == 0 {
		return 0, nil
	}
	d := -1
	for _, e := range es {
		n := len(e.Vector)
		if mode == "base64" {
			b, _ := decodeBase64Vector(e.Encoded)
			n = len(b) / 4
		}
		if d < 0 {
			d = n
		} else if d != n {
			return 0, invalid("dimensions", "vectors have inconsistent dimensions")
		}
	}
	return d, nil
}
func decodeBase64Vector(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b)%4 != 0 {
		return nil, fmt.Errorf("invalid float32 base64")
	}
	return b, nil
}
func geminiUsageCore(u *geminiUsage) (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	for _, n := range []*int64{
		u.PromptTokenCount, u.CachedContentTokenCount, u.CandidatesTokenCount,
		u.ToolUsePromptTokenCount, u.ThoughtsTokenCount, u.TotalTokenCount,
	} {
		if n != nil && *n < 0 {
			return nil, invalid("usageMetadata", "counts must be non-negative")
		}
	}
	if u.PromptTokenCount != nil && u.CachedContentTokenCount != nil && *u.CachedContentTokenCount > *u.PromptTokenCount {
		return nil, invalid("usageMetadata.cachedContentTokenCount", "exceeds promptTokenCount")
	}
	if u.PromptTokenCount != nil && u.ToolUsePromptTokenCount != nil && *u.ToolUsePromptTokenCount > *u.PromptTokenCount {
		return nil, invalid("usageMetadata.toolUsePromptTokenCount", "exceeds promptTokenCount")
	}
	output, err := sumCounts(u.CandidatesTokenCount, u.ThoughtsTokenCount)
	if err != nil {
		return nil, err
	}
	if u.PromptTokenCount != nil && output != nil && u.TotalTokenCount != nil {
		total, err := sumCounts(u.PromptTokenCount, output)
		if err != nil {
			return nil, err
		}
		if total == nil || *total != *u.TotalTokenCount {
			return nil, invalid("usageMetadata.totalTokenCount", "does not match input plus output")
		}
	}
	return &core.Usage{
		Input: u.PromptTokenCount, CachedInput: u.CachedContentTokenCount,
		Output: output, ReasoningOutput: u.ThoughtsTokenCount,
		ToolInput: u.ToolUsePromptTokenCount, Total: u.TotalTokenCount, Source: "gemini",
	}, nil
}

func sumCounts(a, b *int64) (*int64, error) {
	if a == nil {
		return b, nil
	}
	if b == nil {
		return a, nil
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if *a > maxInt64-*b {
		return nil, invalid("usageMetadata", "token count overflow")
	}
	n := *a + *b
	return &n, nil
}

func geminiUsageWire(u *core.Usage) *geminiUsage {
	if u == nil {
		return nil
	}
	candidates := u.Output
	if u.ReasoningOutput != nil {
		if u.Output == nil || *u.ReasoningOutput > *u.Output {
			return nil
		}
		n := *u.Output - *u.ReasoningOutput
		candidates = &n
	}
	return &geminiUsage{
		PromptTokenCount: u.Input, CachedContentTokenCount: u.CachedInput,
		CandidatesTokenCount: candidates, ToolUsePromptTokenCount: u.ToolInput,
		ThoughtsTokenCount: u.ReasoningOutput, TotalTokenCount: u.Total,
	}
}

var _ = binary.LittleEndian
var _ = math.Float32bits
