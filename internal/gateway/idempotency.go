package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"hoorific/internal/core"
)

const idempotencyResponseLimit = 32 << 20

type idempotencyState struct {
	mu    sync.Mutex
	phase string
}

func (s *idempotencyState) mark(phase string) {
	if s == nil || phase == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == "unknown" {
		return
	}
	if phase == "unknown" || s.phase == "" || phase == "terminal" {
		s.phase = phase
	}
}

func (s *idempotencyState) value() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

type idempotencyCaptureWriter struct {
	http.ResponseWriter
	mu         sync.Mutex
	status     int
	wrote      bool
	body       []byte
	oversized  bool
	writeError bool
}

func newIdempotencyCaptureWriter(w http.ResponseWriter) *idempotencyCaptureWriter {
	return &idempotencyCaptureWriter{ResponseWriter: w}
}

func (w *idempotencyCaptureWriter) WriteHeader(status int) {
	w.mu.Lock()
	if !w.wrote {
		w.status = status
		w.wrote = true
	}
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}

func (w *idempotencyCaptureWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	wrote := w.wrote
	w.mu.Unlock()
	if !wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.mu.Lock()
	if n > 0 {
		if len(w.body) <= idempotencyResponseLimit-n {
			w.body = append(w.body, data[:n]...)
		} else {
			w.oversized = true
		}
	}
	if err != nil || n != len(data) {
		w.writeError = true
	}
	w.mu.Unlock()
	return n, err
}

func (w *idempotencyCaptureWriter) Flush() {
	w.mu.Lock()
	wrote := w.wrote
	w.mu.Unlock()
	if !wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *idempotencyCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *idempotencyCaptureWriter) snapshot() (core.IdempotencyResponse, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wrote || w.status < 100 || w.status > 999 || w.oversized || w.writeError {
		return core.IdempotencyResponse{}, false
	}
	return core.IdempotencyResponse{Status: w.status, Header: replayHeaders(w.Header()), Body: append([]byte(nil), w.body...)}, true
}

func replayHeaders(in http.Header) http.Header {
	out := make(http.Header)
	for key, values := range in {
		lower := strings.ToLower(key)
		if isSensitiveReplayHeader(lower) || isHopHeader(lower) {
			continue
		}
		for _, value := range values {
			if len(value) <= 16<<10 {
				out.Add(key, value)
			}
		}
	}
	return out
}

func isSensitiveReplayHeader(name string) bool {
	return name == "authorization" || name == "proxy-authorization" || name == "cookie" || name == "set-cookie" ||
		name == "x-api-key" || name == "x-goog-api-key" || strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "credential")
}

func isHopHeader(name string) bool {
	switch name {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func writeIdempotencyReplay(w http.ResponseWriter, response *core.IdempotencyResponse) error {
	if response == nil || response.Status < 100 || response.Status > 999 || len(response.Body) > idempotencyResponseLimit {
		return errors.New("invalid idempotency replay")
	}
	for key := range w.Header() {
		w.Header().Del(key)
	}
	for key, values := range replayHeaders(response.Header) {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.Status)
	_, err := w.Write(response.Body)
	return err
}

func idempotencyError(err error) error {
	switch {
	case errors.Is(err, core.ErrIdempotencyFingerprintConflict):
		return failure("idempotency_conflict", http.StatusConflict, "idempotency key was used with a different request")
	case errors.Is(err, core.ErrIdempotencyPending), errors.Is(err, core.ErrIdempotencyUnreplayable):
		e := failure("idempotency_in_progress", http.StatusConflict, "idempotency request is already in progress or unreplayable")
		e.RetryAfter = "1"
		return e
	case errors.Is(err, core.ErrIdempotencyInvalid):
		return failure("invalid_request", http.StatusBadRequest, "invalid idempotency request")
	default:
		return failure("unavailable", http.StatusServiceUnavailable, "idempotency persistence unavailable")
	}
}

type idempotencyExecution struct {
	store core.IdempotencyStore
	req   core.IdempotencyRequest
	state *idempotencyState
	write *idempotencyCaptureWriter
}

type idempotencyDiscarder interface {
	DiscardIdempotency(ctx context.Context, req core.IdempotencyRequest) error
}

func (e *idempotencyExecution) finish(ctx context.Context) {
	if e == nil || e.store == nil || e.write == nil {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e.state.value() == "not_dispatched" {
		// The request was rejected before any upstream intent (plan or
		// admission policy). Nothing ambiguous executed, so the key is freed
		// for a retry instead of leaving a permanent unreplayable tombstone.
		if d, ok := e.store.(idempotencyDiscarder); ok {
			_ = d.DiscardIdempotency(finishCtx, e.req)
			return
		}
	}
	var response *core.IdempotencyResponse
	if e.state.value() == "terminal" {
		if captured, ok := e.write.snapshot(); ok {
			response = &captured
		}
	}
	if err := e.store.FinishIdempotency(finishCtx, e.req, response); err != nil {
		// A lost completion is intentionally left pending/unreplayable by the
		// durable store. Never attempt a second provider dispatch here.
		return
	}
}

func fingerprintRequest(ctx context.Context, r *http.Request, raw []byte, captured *capturedBody) (string, error) {
	h := sha256.New()
	_, _ = io.WriteString(h, "hoorific/idempotency-fingerprint/v2\x00")
	const (
		fingerprintString byte = iota + 1
		fingerprintBytes
		fingerprintCount
		fingerprintDigest
	)
	frame := func(kind byte, data []byte) {
		_, _ = h.Write([]byte{kind})
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(data)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(data)
	}
	frameString := func(value string) { frame(fingerprintString, []byte(value)) }
	frameBytes := func(value []byte) { frame(fingerprintBytes, value) }
	frameCount := func(count int) {
		var value [8]byte
		binary.BigEndian.PutUint64(value[:], uint64(count))
		frame(fingerprintCount, value[:])
	}
	frameBody := func(reader io.Reader) error {
		digest := sha256.New()
		n, err := io.Copy(digest, reader)
		if err != nil {
			return err
		}
		var value [8 + sha256.Size]byte
		binary.BigEndian.PutUint64(value[:8], uint64(n))
		copy(value[8:], digest.Sum(nil))
		frame(fingerprintDigest, value[:])
		return nil
	}
	frameString(r.Method)
	frameString(r.URL.EscapedPath())
	frameString(r.URL.Query().Encode())
	headerValues := make(map[string][]string, len(r.Header))
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if lower == "authorization" || lower == "x-api-key" || lower == "x-goog-api-key" || lower == "cookie" || lower == "idempotency-key" || lower == "x-request-id" {
			continue
		}
		headerValues[lower] = append(headerValues[lower], values...)
	}
	keys := make([]string, 0, len(headerValues))
	for key := range headerValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	frameCount(len(keys))
	for _, key := range keys {
		values := headerValues[key]
		sort.Strings(values)
		frameString(key)
		frameCount(len(values))
		for _, value := range values {
			frameString(value)
		}
	}
	frameString("body")
	if captured == nil {
		frameString("raw")
		frameBytes(raw)
	} else if captured.multipart != nil {
		frameString("multipart")
		frameCount(len(captured.multipart.parts))
		for _, part := range captured.multipart.parts {
			frameString(part.Name)
			frameString(part.Filename)
			frameString(part.ContentType)
			if part.Body == nil {
				frameString("field")
				frameBytes(part.Value)
				continue
			}
			frameString("file")
			reader, err := part.Body.Open(ctx)
			if err != nil {
				return "", err
			}
			copyErr := frameBody(reader)
			_ = reader.Close()
			if copyErr != nil {
				return "", copyErr
			}
		}
	} else {
		frameString("binary")
		reader, err := captured.body.Open(ctx)
		if err != nil {
			return "", err
		}
		copyErr := frameBody(reader)
		_ = reader.Close()
		if copyErr != nil {
			return "", copyErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func idempotencyKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Header.Get("Idempotency-Key")
}

func idempotencySubject(p core.Principal) string {
	if p.SessionID != "" {
		return "session:" + p.SessionID
	}
	if p.KeyID != "" {
		return "key:" + p.KeyID
	}
	if p.SubjectID == "" {
		return ""
	}
	return "subject:" + p.SubjectID
}

func markIdempotency(ctx context.Context, phase string) {
	if state, ok := ctx.Value(idempotencyStateKey{}).(*idempotencyState); ok {
		state.mark(phase)
	}
}

type idempotencyStateKey struct{}

func (g *Gateway) reauthorizeIdempotency(ctx context.Context, r *http.Request, x route, p core.Principal) (core.Principal, error) {
	if p.KeyID != "" {
		token, err := extractKey(r, x)
		if err != nil {
			return core.Principal{}, err
		}
		current, err := g.deps.Auth.AuthenticateKey(ctx, token)
		if err != nil {
			return core.Principal{}, failure("unauthorized", http.StatusUnauthorized, "invalid gateway credential")
		}
		return current, nil
	}
	if p.SessionID != "" {
		return g.sessionGrants(ctx, p)
	}
	if p.SubjectID == "" {
		return core.Principal{}, failure("unauthorized", http.StatusUnauthorized, "authenticated identity required")
	}
	return p, nil
}

func (g *Gateway) beginIdempotency(ctx context.Context, r *http.Request, p core.Principal, raw []byte, captured *capturedBody) (*idempotencyExecution, *core.IdempotencyResponse, error) {
	key := idempotencyKey(r)
	if key == "" {
		return nil, nil, nil
	}
	if g.deps.Idempotency == nil {
		return nil, nil, failure("unavailable", http.StatusServiceUnavailable, "idempotency persistence unavailable")
	}
	subject := idempotencySubject(p)
	if subject == "" || p.TenantID == "" {
		return nil, nil, failure("unauthorized", http.StatusUnauthorized, "authenticated identity required for idempotency")
	}
	fingerprint, err := fingerprintRequest(ctx, r, raw, captured)
	if err != nil {
		return nil, nil, failure("invalid_request", http.StatusBadRequest, "request body could not be fingerprinted")
	}
	req := core.IdempotencyRequest{TenantID: p.TenantID, SubjectID: subject, Key: key, Fingerprint: fingerprint, OwnerID: requestID(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	record, err := g.deps.Idempotency.BeginIdempotency(ctx, req)
	if err != nil {
		return nil, nil, idempotencyError(err)
	}
	if record.State == "complete" {
		if record.Response == nil {
			return nil, nil, idempotencyError(core.ErrIdempotencyUnreplayable)
		}
		return nil, record.Response, nil
	}
	if record.State != "new" {
		e := failure("idempotency_in_progress", http.StatusConflict, "idempotency request is already in progress or unreplayable")
		e.RetryAfter = "1"
		return nil, nil, e
	}
	return &idempotencyExecution{store: g.deps.Idempotency, req: req, state: &idempotencyState{}}, nil, nil
}
