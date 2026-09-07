package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"

	"hoorific/internal/core"
	"hoorific/internal/transport"
)

type requestBodyKey struct{}
type capturedBody struct {
	body        core.Body
	contentType string
	multipart   *multipartBody
}
type multipartBody struct {
	parts    []transport.MultipartPart
	boundary string
	once     sync.Once
}

func (b *multipartBody) Replayable() bool { return true }
func (b *multipartBody) Close() error {
	var first error
	b.once.Do(func() {
		for _, p := range b.parts {
			if p.Body != nil {
				if e := p.Body.Close(); e != nil && first == nil {
					first = e
				}
			}
		}
	})
	return first
}
func (b *multipartBody) Open(ctx context.Context) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()); _ = writer.CloseWithError(ctx.Err()) })
	go func() {
		defer stop()
		mw := multipart.NewWriter(writer)
		if e := mw.SetBoundary(b.boundary); e != nil {
			_ = writer.CloseWithError(e)
			return
		}
		for _, p := range b.parts {
			h := make(textproto.MIMEHeader)
			disposition := fmt.Sprintf("form-data; name=%q", p.Name)
			if p.Filename != "" {
				disposition += fmt.Sprintf("; filename=%q", p.Filename)
			}
			h.Set("Content-Disposition", disposition)
			if p.ContentType != "" {
				h.Set("Content-Type", p.ContentType)
			}
			part, e := mw.CreatePart(h)
			if e != nil {
				_ = writer.CloseWithError(e)
				return
			}
			if p.Body == nil {
				_, e = part.Write(p.Value)
			} else {
				var source io.ReadCloser
				source, e = p.Body.Open(ctx)
				if e == nil {
					_, e = io.Copy(part, source)
					_ = source.Close()
				}
			}
			if e != nil {
				_ = writer.CloseWithError(e)
				return
			}
		}
		if e := mw.Close(); e != nil {
			_ = writer.CloseWithError(e)
			return
		}
		_ = writer.Close()
	}()
	return reader, nil
}
func (g *Gateway) capture(ctx context.Context, w http.ResponseWriter, r *http.Request, p core.Principal, id string) ([]byte, map[string]json.RawMessage, *capturedBody, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		if g.deps.Spool == nil {
			return nil, nil, nil, failure("unavailable", 503, "request body spooling unavailable")
		}
		limit := bodyLimit(ctx, true)
		bounded := http.MaxBytesReader(w, r.Body, limit)
		parts, e := transport.CaptureMultipart(ctx, p.TenantID, id, ct, bounded, transport.MultipartConfig{Spool: g.deps.Spool})
		if e != nil {
			return nil, nil, nil, failure("invalid_request", 400, "invalid or oversized multipart body")
		}
		// Multipart parsing stops at the final boundary; enforce the whole-body
		// ceiling on any buffered epilogue and unread trailing bytes as well.
		if _, e = io.Copy(io.Discard, bounded); e != nil {
			for _, part := range parts {
				if part.Body != nil {
					_ = part.Body.Close()
				}
			}
			return nil, nil, nil, failure("invalid_request", 413, "request body limit exceeded")
		}
		b := &multipartBody{parts: parts, boundary: "hoorific-" + id}
		fields := map[string]json.RawMessage{}
		for _, part := range parts {
			if part.Body != nil {
				continue
			}
			if _, duplicate := fields[part.Name]; duplicate {
				_ = b.Close()
				return nil, nil, nil, failure("invalid_request", 400, "duplicate multipart metadata")
			}
			switch part.Name {
			case "stream", "n", "max_tokens", "max_output_tokens", "store", "background":
				if !json.Valid(part.Value) {
					_ = b.Close()
					return nil, nil, nil, failure("invalid_request", 400, "invalid multipart control value")
				}
				fields[part.Name] = append(json.RawMessage(nil), part.Value...)
			default:
				fields[part.Name], _ = json.Marshal(string(part.Value))
			}
		}
		raw, e := json.Marshal(fields)
		return raw, fields, &capturedBody{body: b, multipart: b, contentType: "multipart/form-data; boundary=" + b.boundary}, e
	}
	if ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		if g.deps.Spool == nil {
			return nil, nil, nil, failure("unavailable", 503, "request body spooling unavailable")
		}
		limit := bodyLimit(ctx, true)
		b, e := g.deps.Spool.Capture(ctx, p.TenantID, id, http.MaxBytesReader(w, r.Body, limit), limit)
		if e != nil {
			return nil, nil, nil, e
		}
		return nil, map[string]json.RawMessage{}, &capturedBody{body: b, contentType: ct}, nil
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit(ctx, false)))
	if e != nil {
		return nil, nil, nil, failure("invalid_request", 413, "request body exceeds JSON limit")
	}
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		fields, e = strictObject(raw)
		if e != nil {
			return nil, nil, nil, failure("invalid_request", 400, e.Error())
		}
	}
	return raw, fields, nil, nil
}
func mediaScope(op core.Operation, fields map[string]json.RawMessage) error {
	allowed := map[string]bool{"model": true, "stream": true}
	var names []string
	switch op {
	case "image.generate":
		names = []string{"prompt", "n", "size", "quality", "response_format", "style", "background", "output_format", "output_compression", "moderation"}
	case "image.edit":
		names = []string{"prompt", "n", "size", "quality", "response_format", "input_fidelity", "background", "output_format", "output_compression", "image", "mask"}
	case "image.variation":
		names = []string{"n", "size", "response_format", "image"}
	case "audio.speech":
		names = []string{"input", "voice", "response_format", "speed", "instructions"}
	case "audio.transcribe":
		names = []string{"file", "language", "prompt", "response_format", "temperature", "timestamp_granularities[]", "chunking_strategy"}
	case "audio.translate":
		names = []string{"file", "prompt", "response_format", "temperature"}
	default:
		return failure("unsupported_operation", 400, "operation codec unavailable")
	}
	for _, n := range names {
		allowed[n] = true
	}
	for key := range fields {
		if !allowed[key] {
			e := failure("unsupported_feature", 400, "unclassified portable media field")
			e.Param = key
			return e
		}
	}
	return nil
}
