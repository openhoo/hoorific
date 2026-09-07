package transport

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"hoorific/internal/core"
)

type MultipartPart struct {
	Name, Filename, ContentType string
	Value                       []byte
	Body                        core.Body
}
type MultipartConfig struct {
	MaxParts, MaxFieldBytes, MaxPartBytes int
	Spool                                 *Spool
}

// CaptureMultipart consumes every part before dispatch. File parts are encrypted/spooled;
// fields remain bounded metadata, allowing callers to inspect a model appearing last.
func CaptureMultipart(ctx context.Context, tenant, request, contentType string, r io.Reader, cfg MultipartConfig) ([]MultipartPart, error) {
	if cfg.MaxParts <= 0 {
		cfg.MaxParts = 64
	}
	if cfg.MaxFieldBytes <= 0 {
		cfg.MaxFieldBytes = 1 << 20
	}
	if cfg.MaxPartBytes <= 0 {
		cfg.MaxPartBytes = 1 << 30
	}
	if cfg.Spool == nil {
		return nil, errors.New("multipart spool is required")
	}
	med, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(med, "multipart/form-data") {
		return nil, errors.New("invalid multipart content type")
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, errors.New("multipart boundary missing")
	}
	mr := multipart.NewReader(r, boundary)
	parts := make([]MultipartPart, 0, cfg.MaxParts)
	closeParts := func() {
		for _, p := range parts {
			if p.Body != nil {
				_ = p.Body.Close()
			}
		}
	}
	for len(parts) < cfg.MaxParts {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			closeParts()
			return nil, e
		}
		if err := ctx.Err(); err != nil {
			part.Close()
			closeParts()
			return nil, err
		}
		name := part.FormName()
		if name == "" {
			part.Close()
			closeParts()
			return nil, errors.New("multipart part name missing")
		}
		entry := MultipartPart{Name: name, Filename: part.FileName(), ContentType: part.Header.Get("Content-Type")}
		if entry.Filename != "" {
			body, e := cfg.Spool.Capture(ctx, tenant, request+":"+name+":"+string(rune(len(parts))), part, int64(cfg.MaxPartBytes))
			part.Close()
			if e != nil {
				closeParts()
				return nil, e
			}
			entry.Body = body
		} else {
			data, e := io.ReadAll(io.LimitReader(part, int64(cfg.MaxFieldBytes)+1))
			part.Close()
			if e != nil {
				closeParts()
				return nil, e
			}
			if len(data) > cfg.MaxFieldBytes {
				closeParts()
				return nil, errors.New("multipart field exceeds limit")
			}
			entry.Value = data
		}
		parts = append(parts, entry)
	}
	if len(parts) >= cfg.MaxParts {
		closeParts()
		return nil, errors.New("multipart part count exceeds limit")
	}
	return parts, nil
}
