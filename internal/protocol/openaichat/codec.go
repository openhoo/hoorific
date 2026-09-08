package openaichat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"hoorific/internal/core"
)

type Codec struct{}

func New() *Codec { return &Codec{} }
func validCacheString(v string, max int) bool {
	return v != "" && len([]rune(v)) <= max && !strings.ContainsAny(v, "\x00\r\n")
}

func validCacheTTL(v string) bool {
	switch v {
	case "", "5m", "30m", "1h", "24h":
		return true
	default:
		return false
	}
}

func validateCacheControl(c *core.CacheControl, param string) error {
	if c == nil {
		return nil
	}
	if !validCacheString(c.Type, 32) {
		return unsupported(param + ".type")
	}
	switch c.Type {
	case "ephemeral":
	default:
		return unsupported(param + ".type")
	}
	if !validCacheTTL(c.TTL) {
		return unsupported(param + ".ttl")
	}
	return nil
}

func validatePromptCache(c *core.PromptCache) error {
	if c == nil {
		return nil
	}
	switch c.Protocol {
	case "openai-chat", "openrouter", "openai-responses":
	default:
		return unsupported("cache.protocol")
	}
	if c.Key != "" && !validCacheString(c.Key, 256) {
		return unsupported("prompt_cache_key")
	}
	if c.SessionID != "" {
		if c.Protocol != "openrouter" || !validCacheString(c.SessionID, 256) {
			return unsupported("session_id")
		}
	}
	switch c.Retention {
	case "", "in_memory", "24h":
	default:
		return unsupported("prompt_cache_retention")
	}
	switch c.Mode {
	case "", "implicit", "explicit":
	default:
		return unsupported("prompt_cache_options.mode")
	}
	if !validCacheTTL(c.TTL) {
		return unsupported("prompt_cache_options.ttl")
	}
	if c.CachedContent != "" {
		return unsupported("cache.cached_content")
	}
	if c.Control != nil {
		if err := validateCacheControl(c.Control, "cache.control"); err != nil {
			return err
		}
		return unsupported("cache.control")
	}
	return nil
}
func validateConversationCache(q core.Conversation) error {
	if err := validatePromptCache(q.Cache); err != nil {
		return err
	}
	proto := core.Protocol("")
	if q.Cache != nil {
		proto = q.Cache.Protocol
	}
	checkBlock := func(b core.ContentBlock, param string) error {
		if b.CacheControl != nil {
			if proto != "openrouter" {
				return unsupported(param + ".cache_control")
			}
			if err := validateCacheControl(b.CacheControl, param+".cache_control"); err != nil {
				return err
			}
		}
		if b.CacheBreakpoint {
			if proto != "openai-chat" && proto != "openrouter" && proto != "openai-responses" {
				return unsupported(param + ".prompt_cache_breakpoint")
			}
			if b.Kind != "text" {
				return unsupported(param + ".prompt_cache_breakpoint")
			}
		}
		return nil
	}
	for i, b := range q.System {
		if err := checkBlock(b, fmt.Sprintf("system[%d]", i)); err != nil {
			return err
		}
	}
	for i, m := range q.Messages {
		for j, b := range m.Content {
			if err := checkBlock(b, fmt.Sprintf("messages[%d].content[%d]", i, j)); err != nil {
				return err
			}
		}
	}
	for i, t := range q.Tools {
		if t.CacheControl != nil {
			if proto != "openrouter" {
				return unsupported(fmt.Sprintf("tools[%d].cache_control", i))
			}
			if err := validateCacheControl(t.CacheControl, fmt.Sprintf("tools[%d].cache_control", i)); err != nil {
				return err
			}
		}
		if t.CacheBreakpoint {
			return unsupported(fmt.Sprintf("tools[%d].prompt_cache_breakpoint", i))
		}
	}
	return nil
}

func cacheControlFromWire(v *cacheControl, param string) (*core.CacheControl, error) {
	if v == nil {
		return nil, nil
	}
	c := &core.CacheControl{Type: v.Type, TTL: v.TTL}
	if err := validateCacheControl(c, param); err != nil {
		return nil, err
	}
	return c, nil
}

func breakpointFromWire(v *cacheBreakpoint, param string) (bool, error) {
	if v == nil {
		return false, nil
	}
	if v.Mode != "explicit" {
		return false, unsupported(param + ".mode")
	}
	return true, nil
}

// Wire structures deliberately contain only fields supported by this codec.
type request struct {
	Model                string              `json:"model"`
	Messages             []message           `json:"messages"`
	Tools                []wireTool          `json:"tools,omitempty"`
	MaxTokens            *int64              `json:"max_tokens,omitempty"`
	MaxCompletionTokens  *int64              `json:"max_completion_tokens,omitempty"`
	Stream               bool                `json:"stream,omitempty"`
	StreamOptions        *streamOptions      `json:"stream_options,omitempty"`
	Stop                 any                 `json:"stop,omitempty"`
	ResponseFormat       json.RawMessage     `json:"response_format,omitempty"`
	PromptCacheKey       string              `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string              `json:"prompt_cache_retention,omitempty"`
	PromptCacheOptions   *promptCacheOptions `json:"prompt_cache_options,omitempty"`
	SessionID            string              `json:"session_id,omitempty"`
}
type streamOptions struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}
type promptCacheOptions struct {
	Mode string `json:"mode,omitempty"`
	TTL  string `json:"ttl,omitempty"`
}
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}
type cacheBreakpoint struct {
	Mode string `json:"mode"`
}
type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
}
type contentPart struct {
	Type                  string           `json:"type"`
	Text                  string           `json:"text,omitempty"`
	ImageURL              *imageURL        `json:"image_url,omitempty"`
	File                  *filePart        `json:"file,omitempty"`
	CacheControl          *cacheControl    `json:"cache_control,omitempty"`
	PromptCacheBreakpoint *cacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
}
type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}
type filePart struct {
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}
type wireTool struct {
	Type         string        `json:"type"`
	Function     wireFunction  `json:"function"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}
type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}
type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireCallFunction `json:"function"`
}
type wireCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func rawString(b json.RawMessage) (string, error) {
	var s string
	if len(b) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return "", unsupported("content")
	}
	if e := strict(b, &s); e != nil {
		return "", e
	}
	return s, nil
}
func decodeParts(b json.RawMessage) ([]contentPart, error) {
	var p []contentPart
	if e := strict(b, &p); e != nil {
		return nil, e
	}
	return p, nil
}
func blockFromPart(p contentPart) (core.ContentBlock, error) {
	cache, err := cacheControlFromWire(p.CacheControl, "content.cache_control")
	if err != nil {
		return core.ContentBlock{}, err
	}
	breakpoint, err := breakpointFromWire(p.PromptCacheBreakpoint, "content.prompt_cache_breakpoint")
	if err != nil {
		return core.ContentBlock{}, err
	}
	switch p.Type {
	case "text":
		if p.ImageURL != nil || p.File != nil {
			return core.ContentBlock{}, unsupported("content.text")
		}
		return core.ContentBlock{Kind: "text", Text: p.Text, CacheControl: cache, CacheBreakpoint: breakpoint}, nil
	case "image_url":
		if p.ImageURL == nil || p.ImageURL.URL == "" || p.File != nil {
			return core.ContentBlock{}, unsupported("content.image_url")
		}
		if p.ImageURL.Detail != "" && p.ImageURL.Detail != "auto" {
			return core.ContentBlock{}, unsupported("content.image_url.detail")
		}
		if breakpoint {
			return core.ContentBlock{}, unsupported("content.prompt_cache_breakpoint")
		}
		return core.ContentBlock{Kind: "image", URL: p.ImageURL.URL, CacheControl: cache}, nil
	case "file":
		if p.File == nil {
			return core.ContentBlock{}, unsupported("content.file")
		}
		if p.File.FileID != "" || p.File.FileData == "" {
			return core.ContentBlock{}, unsupported("content.file.file_id")
		}
		if breakpoint {
			return core.ContentBlock{}, unsupported("content.prompt_cache_breakpoint")
		}
		b := core.ContentBlock{Kind: "document", URL: p.File.FileData, Name: p.File.Filename, CacheControl: cache}
		if strings.HasPrefix(p.File.FileData, "data:") {
			if i := strings.Index(p.File.FileData, ","); i > 5 {
				meta := p.File.FileData[5:i]
				if j := strings.Index(meta, ";"); j >= 0 {
					b.MIMEType = meta[:j]
				} else {
					b.MIMEType = meta
				}
				if d, e := base64.StdEncoding.DecodeString(p.File.FileData[i+1:]); e == nil {
					b.Data = d
				}
			}
		}
		return b, nil
	default:
		return core.ContentBlock{}, unsupported("content.type")
	}
}
func blocksFromContent(b json.RawMessage, allowNull bool) ([]core.ContentBlock, error) {
	if len(b) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		if allowNull {
			return nil, nil
		}
		return nil, unsupported("content")
	}
	if s, e := rawString(b); e == nil {
		return []core.ContentBlock{{Kind: "text", Text: s}}, nil
	}
	p, e := decodeParts(b)
	if e != nil {
		return nil, e
	}
	out := make([]core.ContentBlock, 0, len(p))
	for _, x := range p {
		v, e := blockFromPart(x)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func messageFromWire(m message) (core.Message, error) {
	if m.Role == "" {
		return core.Message{}, unsupported("messages.role")
	}
	if m.Name != "" {
		return core.Message{}, unsupported("messages.name")
	}
	switch m.Role {
	case "system":
		b, e := blocksFromContent(m.Content, false)
		if e != nil {
			return core.Message{}, e
		}
		return core.Message{Role: "system", Content: b}, nil
	case "user":
		b, e := blocksFromContent(m.Content, false)
		if e != nil {
			return core.Message{}, e
		}
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return core.Message{}, unsupported("messages.tool_calls")
		}
		return core.Message{Role: "user", Content: b}, nil
	case "assistant":
		b, e := blocksFromContent(m.Content, true)
		if e != nil {
			return core.Message{}, e
		}
		for _, tc := range m.ToolCalls {
			if tc.Type != "function" || tc.ID == "" || tc.Function.Name == "" {
				return core.Message{}, unsupported("messages.tool_calls")
			}
			if e := arguments(tc.Function.Arguments); e != nil {
				return core.Message{}, e
			}
			b = append(b, core.ContentBlock{Kind: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if len(b) == 0 {
			return core.Message{}, unsupported("messages.content")
		}
		return core.Message{Role: "assistant", Content: b}, nil
	case "tool":
		if m.ToolCallID == "" || len(m.ToolCalls) > 0 {
			return core.Message{}, unsupported("messages.tool_call_id")
		}
		b, e := blocksFromContent(m.Content, false)
		if e != nil {
			return core.Message{}, e
		}
		for i := range b {
			if b[i].Kind != "text" {
				return core.Message{}, unsupported("messages.content")
			}
			b[i].Kind = "tool_result"
			b[i].ID = m.ToolCallID
		}
		return core.Message{Role: "tool", Content: b}, nil
	default:
		return core.Message{}, unsupported("messages.role")
	}
}
func promptCacheFromWire(q request) (*core.PromptCache, error) {
	if q.PromptCacheKey == "" && q.PromptCacheRetention == "" && q.PromptCacheOptions == nil && q.SessionID == "" {
		return nil, nil
	}
	protocol := core.Protocol("openai-chat")
	if q.SessionID != "" {
		protocol = "openrouter"
	}
	c := &core.PromptCache{
		Protocol:  protocol,
		Key:       q.PromptCacheKey,
		Retention: q.PromptCacheRetention,
		SessionID: q.SessionID,
	}
	if q.PromptCacheOptions != nil {
		c.Mode = q.PromptCacheOptions.Mode
		c.TTL = q.PromptCacheOptions.TTL
	}
	if err := validatePromptCache(c); err != nil {
		return nil, err
	}
	return c, nil
}

func markDecodedCache(conv *core.Conversation) {
	var hasControl, hasBreakpoint bool
	for _, b := range conv.System {
		hasControl = hasControl || b.CacheControl != nil
		hasBreakpoint = hasBreakpoint || b.CacheBreakpoint
	}
	for _, m := range conv.Messages {
		for _, b := range m.Content {
			hasControl = hasControl || b.CacheControl != nil
			hasBreakpoint = hasBreakpoint || b.CacheBreakpoint
		}
	}
	for _, t := range conv.Tools {
		hasControl = hasControl || t.CacheControl != nil
	}
	if !hasControl && !hasBreakpoint {
		return
	}
	if conv.Cache == nil {
		conv.Cache = &core.PromptCache{}
	}
	if hasControl {
		conv.Cache.Protocol = "openrouter"
	} else if conv.Cache.Protocol == "" {
		conv.Cache.Protocol = "openai-chat"
	}
}
func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	var q request
	if e := input(ctx, r, &q); e != nil {
		return nil, e
	}
	if q.Model == "" || len(q.Messages) == 0 {
		return nil, unsupported("model/messages")
	}
	if q.MaxTokens != nil && q.MaxCompletionTokens != nil {
		return nil, unsupported("max_tokens")
	}
	max := q.MaxTokens
	if max == nil {
		max = q.MaxCompletionTokens
	}
	if max != nil && *max < 0 {
		return nil, unsupported("max_tokens")
	}
	cache, e := promptCacheFromWire(q)
	if e != nil {
		return nil, e
	}
	conv := core.Conversation{Model: q.Model, MaxOutputTokens: max, Stream: q.Stream, Cache: cache}
	if q.StreamOptions != nil {
		if !q.Stream {
			return nil, unsupported("stream_options")
		}
		conv.StreamIncludeUsage = q.StreamOptions.IncludeUsage
	}
	seenNonSystem := false
	for _, m := range q.Messages {
		cm, e := messageFromWire(m)
		if e != nil {
			return nil, e
		}
		if cm.Role == "system" {
			if seenNonSystem {
				return nil, unsupported("messages.system_order")
			}
			conv.System = append(conv.System, cm.Content...)
		} else {
			seenNonSystem = true
			conv.Messages = append(conv.Messages, cm)
		}
	}
	for _, t := range q.Tools {
		if t.Type != "function" || t.Function.Name == "" {
			return nil, unsupported("tools")
		}
		if e := object(t.Function.Parameters); e != nil {
			return nil, e
		}
		if t.Function.Strict != nil && !*t.Function.Strict {
			return nil, unsupported("tools.function.strict")
		}
		cache, e := cacheControlFromWire(t.CacheControl, "tools.cache_control")
		if e != nil {
			return nil, e
		}
		conv.Tools = append(conv.Tools, core.Tool{
			Name: t.Function.Name, Description: t.Function.Description,
			Schema: append([]byte(nil), t.Function.Parameters...), CacheControl: cache,
		})
	}
	switch s := q.Stop.(type) {
	case nil:
	case string:
		conv.Stop = []string{s}
	case []any:
		for _, v := range s {
			x, ok := v.(string)
			if !ok {
				return nil, unsupported("stop")
			}
			conv.Stop = append(conv.Stop, x)
		}
	default:
		return nil, unsupported("stop")
	}
	if len(q.ResponseFormat) > 0 {
		mode, name, desc, schema, st, e := decodeStructured(q.ResponseFormat)
		if e != nil {
			return nil, e
		}
		conv.StructuredOutputMode = mode
		conv.StructuredOutputName = name
		conv.StructuredOutputDescription = desc
		conv.StructuredOutputStrict = st
		conv.StructuredOutput = schema
	}
	markDecodedCache(&conv)
	if e := validateConversationCache(conv); e != nil {
		return nil, e
	}
	return conv, nil
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	return encodeConversation(ctx, p, w)
}

type choice struct {
	Index        int             `json:"index"`
	Message      message         `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}
type result struct {
	ID      string     `json:"id"`
	Object  string     `json:"object,omitempty"`
	Created int64      `json:"created,omitempty"`
	Model   string     `json:"model"`
	Choices []choice   `json:"choices"`
	Usage   *wireUsage `json:"usage,omitempty"`
}
type wireUsage struct {
	PromptTokens            *int64                   `json:"prompt_tokens,omitempty"`
	CompletionTokens        *int64                   `json:"completion_tokens,omitempty"`
	TotalTokens             *int64                   `json:"total_tokens,omitempty"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}
type promptTokensDetails struct {
	CachedTokens        *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens    *int64 `json:"cache_write_tokens,omitempty"`
	CacheReadTokens     *int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	AudioTokens         *int64 `json:"audio_tokens,omitempty"`
}
type completionTokensDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
	ReasoningOutput *int64 `json:"reasoning_output_tokens,omitempty"`
	AudioTokens     *int64 `json:"audio_tokens,omitempty"`
}

func resultBlocks(m message) ([]core.ContentBlock, error) {
	if m.Role != "assistant" {
		return nil, unsupported("choices.message.role")
	}
	b, e := blocksFromContent(m.Content, true)
	if e != nil {
		return nil, e
	}
	for _, tc := range m.ToolCalls {
		if tc.Type != "function" || tc.ID == "" || tc.Function.Name == "" {
			return nil, unsupported("choices.message.tool_calls")
		}
		if e := arguments(tc.Function.Arguments); e != nil {
			return nil, e
		}
		b = append(b, core.ContentBlock{Kind: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return b, nil
}
func validUsageValue(v *int64) bool { return v == nil || *v >= 0 }

func decodeUsage(u *wireUsage) (*core.Usage, error) {
	if u == nil {
		return nil, nil
	}
	values := []*int64{u.PromptTokens, u.CompletionTokens, u.TotalTokens}
	if u.PromptTokensDetails != nil {
		values = append(values,
			u.PromptTokensDetails.CachedTokens,
			u.PromptTokensDetails.CacheWriteTokens,
			u.PromptTokensDetails.CacheReadTokens,
			u.PromptTokensDetails.CacheCreationTokens,
			u.PromptTokensDetails.AudioTokens,
		)
	}
	if u.CompletionTokensDetails != nil {
		values = append(values,
			u.CompletionTokensDetails.ReasoningTokens,
			u.CompletionTokensDetails.ReasoningOutput,
			u.CompletionTokensDetails.AudioTokens,
		)
	}
	for _, v := range values {
		if !validUsageValue(v) {
			return nil, unsupported("usage")
		}
	}
	out := &core.Usage{Input: u.PromptTokens, Output: u.CompletionTokens, Total: u.TotalTokens, Source: "provider"}
	if d := u.PromptTokensDetails; d != nil {
		out.CachedInput = d.CachedTokens
		out.CacheWriteInput = d.CacheWriteTokens
		if out.CacheWriteInput == nil {
			out.CacheWriteInput = d.CacheCreationTokens
		}
		if out.CachedInput == nil {
			out.CachedInput = d.CacheReadTokens
		}
	}
	if d := u.CompletionTokensDetails; d != nil {
		out.ReasoningOutput = d.ReasoningTokens
		if out.ReasoningOutput == nil {
			out.ReasoningOutput = d.ReasoningOutput
		}
	}
	return out, nil
}

func cacheWriteAggregate(u *core.Usage) (*int64, error) {
	if u.CacheWriteInput != nil {
		return u.CacheWriteInput, nil
	}
	var total int64
	var present bool
	const maxInt64 = int64(^uint64(0) >> 1)
	for _, part := range []*int64{u.CacheWrite5mInput, u.CacheWrite1hInput} {
		if part == nil {
			continue
		}
		if total > maxInt64-*part {
			return nil, unsupported("usage")
		}
		total += *part
		present = true
	}
	if !present {
		return nil, nil
	}
	return &total, nil
}

func encodeUsage(u *core.Usage) (*wireUsage, error) {
	if u == nil {
		return nil, nil
	}
	values := []*int64{u.Input, u.Output, u.Total, u.CachedInput, u.CacheWriteInput, u.CacheWrite5mInput, u.CacheWrite1hInput, u.ReasoningOutput, u.ToolInput}
	for _, v := range values {
		if !validUsageValue(v) {
			return nil, unsupported("usage")
		}
	}
	write, err := cacheWriteAggregate(u)
	if err != nil {
		return nil, err
	}
	out := &wireUsage{PromptTokens: u.Input, CompletionTokens: u.Output, TotalTokens: u.Total}
	if u.CachedInput != nil || write != nil {
		out.PromptTokensDetails = &promptTokensDetails{CachedTokens: u.CachedInput, CacheWriteTokens: write}
	}
	if u.ReasoningOutput != nil {
		out.CompletionTokensDetails = &completionTokensDetails{ReasoningTokens: u.ReasoningOutput}
	}
	return out, nil
}

func (c *Codec) DecodeResult(ctx context.Context, r io.Reader) (core.ResultPayload, error) {
	var v result
	if e := input(ctx, r, &v); e != nil {
		return nil, e
	}
	if v.ID == "" || len(v.Choices) != 1 {
		return nil, unsupported("choices")
	}
	ch := v.Choices[0]
	if ch.Index != 0 || ch.FinishReason == nil || *ch.FinishReason == "" {
		return nil, unsupported("choices.finish_reason")
	}
	if len(ch.Logprobs) > 0 && !bytes.Equal(bytes.TrimSpace(ch.Logprobs), []byte("null")) {
		return nil, unsupported("choices.logprobs")
	}
	f, e := finishFromWire(*ch.FinishReason)
	if e != nil {
		return nil, e
	}
	b, e := resultBlocks(ch.Message)
	if e != nil {
		return nil, e
	}
	var u *core.Usage
	if v.Usage != nil {
		u, e = decodeUsage(v.Usage)
		if e != nil {
			return nil, e
		}
	}
	return core.GenerationResult{ID: v.ID, Model: v.Model, Blocks: b, Finish: f, Usage: u}, nil
}
func finishFromWire(r string) (core.Finish, error) {
	switch r {
	case "stop", "stop_sequence", "tool_calls", "content_filter":
		return core.Finish{Status: "completed", Reason: r}, nil
	case "length":
		return core.Finish{Status: "incomplete", Reason: r}, nil
	default:
		return core.Finish{}, unsupported("finish_reason")
	}
}
func finishOK(f core.Finish) error {
	if f.Cancellation != "" || f.Reason == "" {
		return fmt.Errorf("finish cannot be represented by Chat Completions")
	}
	expected := "completed"
	if f.Reason == "length" {
		expected = "incomplete"
	}
	if f.Status != "" && f.Status != expected {
		return unsupported("finish.status")
	}
	switch f.Reason {
	case "stop", "stop_sequence", "tool_calls", "content_filter", "length":
		return nil
	}
	return unsupported("finish.reason")
}
func validateResponseFormat(b []byte) error {
	var m map[string]json.RawMessage
	if e := strict(b, &m); e != nil {
		return e
	}
	var typ string
	if x := m["type"]; len(x) > 0 {
		if e := strict(x, &typ); e != nil {
			return e
		}
	} else {
		return unsupported("response_format.type")
	}
	switch typ {
	case "text":
		if len(m) != 1 {
			return unsupported("response_format")
		}
	case "json_object":
		if len(m) != 1 {
			return unsupported("response_format")
		}
	case "json_schema":
		if len(m) != 2 || len(m["json_schema"]) == 0 {
			return unsupported("response_format.json_schema")
		}
		var s struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if e := strict(m["json_schema"], &s); e != nil {
			return e
		}
		if s.Name == "" || len(s.Schema) == 0 {
			return unsupported("response_format.json_schema")
		}
		if e := object(s.Schema); e != nil {
			return e
		}
	default:
		return unsupported("response_format.type")
	}
	return nil
}
func decodeStructured(b []byte) (string, string, string, []byte, *bool, error) {
	var m map[string]json.RawMessage
	if e := strict(b, &m); e != nil {
		return "", "", "", nil, nil, e
	}
	var typ string
	if e := strict(m["type"], &typ); e != nil {
		return "", "", "", nil, nil, e
	}
	switch typ {
	case "text", "json_object":
		if len(m) != 1 {
			return "", "", "", nil, nil, unsupported("response_format")
		}
		return typ, "", "", nil, nil, nil
	case "json_schema":
		if e := validateResponseFormat(b); e != nil {
			return "", "", "", nil, nil, e
		}
		var s struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if e := strict(m["json_schema"], &s); e != nil {
			return "", "", "", nil, nil, e
		}
		return typ, s.Name, s.Description, append([]byte(nil), s.Schema...), s.Strict, nil
	default:
		return "", "", "", nil, nil, unsupported("response_format.type")
	}
}
func wireCacheControlFromCore(c *core.CacheControl, param string) (*cacheControl, error) {
	if c == nil {
		return nil, nil
	}
	if err := validateCacheControl(c, param); err != nil {
		return nil, err
	}
	return &cacheControl{Type: c.Type, TTL: c.TTL}, nil
}

func wirePartFromBlock(b core.ContentBlock, typ string) (contentPart, error) {
	cc, err := wireCacheControlFromCore(b.CacheControl, "content.cache_control")
	if err != nil {
		return contentPart{}, err
	}
	bp := (*cacheBreakpoint)(nil)
	if b.CacheBreakpoint {
		bp = &cacheBreakpoint{Mode: "explicit"}
	}
	part := contentPart{Type: typ, CacheControl: cc, PromptCacheBreakpoint: bp}
	switch typ {
	case "text":
		if !cleanBlock(b, "text") {
			return contentPart{}, unsupported("messages.content")
		}
		part.Text = b.Text
	case "image_url":
		if !cleanBlock(b, "image") || b.CacheBreakpoint {
			return contentPart{}, unsupported("messages.content")
		}
		part.ImageURL = &imageURL{URL: b.URL}
	case "file":
		if !cleanBlock(b, "document") || b.CacheBreakpoint {
			return contentPart{}, unsupported("messages.content")
		}
		part.File = &filePart{FileData: b.URL, Filename: b.Name}
	default:
		return contentPart{}, unsupported("messages.content")
	}
	return part, nil
}

func encodeConversation(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	q, ok := p.(core.Conversation)
	if !ok {
		return unsupported("payload")
	}
	if q.Model == "" || len(q.Messages) == 0 {
		return unsupported("model/messages")
	}
	if err := validateConversationCache(q); err != nil {
		return err
	}
	out := request{Model: q.Model, MaxTokens: q.MaxOutputTokens, Stream: q.Stream}
	if q.Cache != nil {
		out.PromptCacheKey = q.Cache.Key
		out.PromptCacheRetention = q.Cache.Retention
		out.SessionID = q.Cache.SessionID
		if q.Cache.Mode != "" || q.Cache.TTL != "" {
			out.PromptCacheOptions = &promptCacheOptions{Mode: q.Cache.Mode, TTL: q.Cache.TTL}
		}
	}
	if q.StreamIncludeUsage != nil {
		if !q.Stream {
			return unsupported("stream_options")
		}
		out.StreamOptions = &streamOptions{IncludeUsage: q.StreamIncludeUsage}
	}
	for _, b := range q.System {
		if b.Kind != "text" {
			return unsupported("system")
		}
	}
	if len(q.System) > 0 {
		content, err := rawTextBlocks(q.System)
		if err != nil {
			return err
		}
		out.Messages = append(out.Messages, message{Role: "system", Content: content})
	}
	for _, m := range q.Messages {
		wm, e := wireMessage(m)
		if e != nil {
			return e
		}
		out.Messages = append(out.Messages, wm)
	}
	for _, t := range q.Tools {
		if t.Name == "" || t.CacheBreakpoint {
			return unsupported("tools")
		}
		if e := object(t.Schema); e != nil {
			return e
		}
		cc, e := wireCacheControlFromCore(t.CacheControl, "tools.cache_control")
		if e != nil {
			return e
		}
		out.Tools = append(out.Tools, wireTool{
			Type:         "function",
			Function:     wireFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema},
			CacheControl: cc,
		})
	}
	if len(q.Stop) > 0 {
		out.Stop = q.Stop
	}
	if len(q.StructuredOutput) > 0 || q.StructuredOutputMode != "" || q.StructuredOutputName != "" || q.StructuredOutputDescription != "" || q.StructuredOutputStrict != nil {
		mode := q.StructuredOutputMode
		if mode == "" {
			mode = "json_schema"
		}
		switch mode {
		case "text", "json_object":
			if len(q.StructuredOutput) > 0 || q.StructuredOutputName != "" || q.StructuredOutputDescription != "" || q.StructuredOutputStrict != nil {
				return unsupported("response_format")
			}
			out.ResponseFormat = mustJSON(map[string]string{"type": mode})
		case "json_schema":
			if q.StructuredOutputName == "" || len(q.StructuredOutput) == 0 {
				return unsupported("response_format.json_schema")
			}
			if e := object(q.StructuredOutput); e != nil {
				return e
			}
			inner := struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Schema      json.RawMessage `json:"schema"`
				Strict      *bool           `json:"strict,omitempty"`
			}{q.StructuredOutputName, q.StructuredOutputDescription, q.StructuredOutput, q.StructuredOutputStrict}
			out.ResponseFormat = mustJSON(struct {
				Type   string `json:"type"`
				Schema any    `json:"json_schema"`
			}{"json_schema", inner})
		default:
			return unsupported("response_format.type")
		}
	}
	return output(ctx, w, out)
}

func rawTextBlocks(b []core.ContentBlock) (json.RawMessage, error) {
	if len(b) == 1 && b[0].Kind == "text" && b[0].CacheControl == nil && !b[0].CacheBreakpoint {
		return json.RawMessage(mustJSON(b[0].Text)), nil
	}
	p := make([]contentPart, 0, len(b))
	for _, x := range b {
		if x.Kind == "tool_result" {
			if !cleanBlock(x, "tool_result") {
				return nil, unsupported("messages.content")
			}
			cc, err := wireCacheControlFromCore(x.CacheControl, "content.cache_control")
			if err != nil {
				return nil, err
			}
			bp := (*cacheBreakpoint)(nil)
			if x.CacheBreakpoint {
				bp = &cacheBreakpoint{Mode: "explicit"}
			}
			p = append(p, contentPart{Type: "text", Text: x.Text, CacheControl: cc, PromptCacheBreakpoint: bp})
			continue
		}
		part, err := wirePartFromBlock(x, "text")
		if err != nil {
			return nil, err
		}
		p = append(p, part)
	}
	return mustJSON(p), nil
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func wireMessage(m core.Message) (message, error) {
	if m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
		return message{}, unsupported("messages.role")
	}
	wm := message{Role: m.Role}
	if m.Role == "tool" {
		if len(m.Content) == 0 {
			return message{}, unsupported("messages.content")
		}
		id := m.Content[0].ID
		if id == "" {
			return message{}, unsupported("messages.tool_call_id")
		}
		for _, b := range m.Content {
			if b.Kind != "tool_result" || b.ID != id || !cleanBlock(b, "tool_result") {
				return message{}, unsupported("messages.content")
			}
		}
		wm.ToolCallID = id
		content, err := rawTextBlocks(m.Content)
		if err != nil {
			return message{}, err
		}
		wm.Content = content
		return wm, nil
	}
	for _, b := range m.Content {
		switch b.Kind {
		case "text":
			part, err := wirePartFromBlock(b, "text")
			if err != nil {
				return message{}, err
			}
			wm.Content = appendRaw(wm.Content, mustJSON(part))
		case "image":
			part, err := wirePartFromBlock(b, "image_url")
			if err != nil {
				return message{}, err
			}
			wm.Content = appendRaw(wm.Content, mustJSON(part))
		case "document":
			part, err := wirePartFromBlock(b, "file")
			if err != nil {
				return message{}, err
			}
			wm.Content = appendRaw(wm.Content, mustJSON(part))
		case "tool_call":
			if m.Role != "assistant" || !cleanBlock(b, "tool_call") || b.ID == "" || b.Name == "" || b.CacheControl != nil || b.CacheBreakpoint {
				return message{}, unsupported("messages.tool_calls")
			}
			if e := arguments(b.Arguments); e != nil {
				return message{}, e
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{ID: b.ID, Type: "function", Function: wireCallFunction{Name: b.Name, Arguments: b.Arguments}})
		default:
			return message{}, unsupported("messages.content")
		}
	}
	if len(wm.Content) > 0 && m.Role == "assistant" {
		wm.Content = collapseParts(wm.Content)
	}
	if len(wm.Content) == 0 && len(wm.ToolCalls) == 0 {
		return message{}, unsupported("messages.content")
	}
	return wm, nil
}
func appendRaw(existing json.RawMessage, b []byte) json.RawMessage {
	var arr []json.RawMessage
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &arr)
	}
	arr = append(arr, b)
	return mustJSON(arr)
}
func collapseParts(b json.RawMessage) json.RawMessage {
	var a []json.RawMessage
	_ = json.Unmarshal(b, &a)
	if len(a) == 1 {
		var p contentPart
		if json.Unmarshal(a[0], &p) == nil && p.Type == "text" && p.CacheControl == nil && p.PromptCacheBreakpoint == nil {
			return mustJSON(p.Text)
		}
	}
	return b
}
func (c *Codec) EncodeResult(ctx context.Context, p core.ResultPayload, w io.Writer) error {
	g, ok := p.(core.GenerationResult)
	if !ok {
		return unsupported("payload")
	}
	if e := finishOK(g.Finish); e != nil {
		return e
	}
	m := message{Role: "assistant"}
	for _, b := range g.Blocks {
		switch b.Kind {
		case "text":
			part, err := wirePartFromBlock(b, "text")
			if err != nil {
				return err
			}
			m.Content = appendRaw(m.Content, mustJSON(part))
		case "tool_call":
			if !cleanBlock(b, "tool_call") || b.ID == "" || b.Name == "" || b.CacheControl != nil || b.CacheBreakpoint {
				return unsupported("blocks")
			}
			if e := arguments(b.Arguments); e != nil {
				return e
			}
			m.ToolCalls = append(m.ToolCalls, wireToolCall{ID: b.ID, Type: "function", Function: wireCallFunction{Name: b.Name, Arguments: b.Arguments}})
		default:
			return unsupported("blocks")
		}
	}
	if len(m.Content) > 0 {
		m.Content = collapseParts(m.Content)
	}
	if len(m.Content) == 0 && len(m.ToolCalls) == 0 {
		return unsupported("blocks")
	}
	v := result{ID: g.ID, Object: "chat.completion", Model: g.Model, Choices: []choice{{Index: 0, Message: m, FinishReason: &g.Finish.Reason}}}
	if g.Usage != nil {
		usage, err := encodeUsage(g.Usage)
		if err != nil {
			return err
		}
		v.Usage = usage
	}
	return output(ctx, w, v)
}

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
