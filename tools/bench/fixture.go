package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	fixtureContentBytes = 1024
	fixtureChunkBytes   = 8
	fixtureBodyLimit    = 1 << 20
)

var fixtureNoncePattern = regexp.MustCompile(`^BENCH:[0-9a-fA-F]+:`)

var fixtureActiveStalls atomic.Int64

// startFixture binds a real loopback HTTP server. The returned URL is its origin,
// without /v1. closeFunc is concurrent-safe and waits at most two seconds for
// graceful shutdown before closing connections. Its signature cannot return
// asynchronous Serve, request transport, or shutdown errors.
func startFixture(ctx context.Context, listen string) (baseURL string, closeFunc func(), err error) {
	if ctx == nil {
		return "", nil, errors.New("fixture: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	address, err := fixtureAddress(listen)
	if err != nil {
		return "", nil, err
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return "", nil, fmt.Errorf("fixture listen: %w", err)
	}
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !bound.IP.IsLoopback() {
		_ = listener.Close()
		return "", nil, errors.New("fixture: listener is not loopback")
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return "", nil, err
	}
	serverContext, cancel := context.WithCancel(ctx)
	server := &http.Server{
		Handler:           http.HandlerFunc(fixtureHandler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext: func(net.Listener) context.Context {
			return serverContext
		},
		// net/http otherwise logs internal transport failures to stderr.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	var once sync.Once
	closeFunc = func() {
		once.Do(func() {
			cancel()
			shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer shutdownCancel()
			if err := server.Shutdown(shutdownContext); err != nil {
				_ = server.Close()
			}
		})
	}
	go func() {
		// An unexpected Serve exit also cancels all active request contexts.
		_ = server.Serve(listener)
		cancel()
	}()
	go func() {
		<-serverContext.Done()
		closeFunc()
	}()
	return "http://" + listener.Addr().String(), closeFunc, nil
}

// Hostnames other than localhost are deliberately rejected: no DNS lookup can
// change a validated loopback address into an externally reachable binding.
func fixtureAddress(listen string) (string, error) {
	if listen == "" {
		listen = "127.0.0.1:18089"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("fixture address: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("fixture: listen host must be a loopback IP or localhost")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return "", errors.New("fixture: port must be between 0 and 65535")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(portNumber)), nil
}

type fixtureRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string  `json:"role"`
		Content *string `json:"content"`
	} `json:"messages"`
}

func fixtureHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/messages" {
		fixtureAnthropicHandler(w, r)
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		fixtureError(w, http.StatusNotFound, "unknown fixture endpoint")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fixtureError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if value := r.Header.Get("Content-Type"); value != "" {
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil || mediaType != "application/json" {
			fixtureError(w, http.StatusUnsupportedMediaType, "application/json required")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, fixtureBodyLimit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		var limitError *http.MaxBytesError
		if errors.As(err, &limitError) {
			fixtureError(w, http.StatusRequestEntityTooLarge, "request exceeds 1 MiB")
		} else {
			var timeoutError net.Error
			if errors.As(err, &timeoutError) && timeoutError.Timeout() {
				fixtureError(w, http.StatusRequestTimeout, "request body timed out")
			} else {
				fixtureError(w, http.StatusBadRequest, "cannot read request body")
			}
		}
		return
	}
	var request fixtureRequest
	if err := json.Unmarshal(body, &request); err != nil {
		fixtureError(w, http.StatusBadRequest, "invalid chat completion JSON")
		return
	}
	content, err := fixtureContent(request)
	if err != nil {
		fixtureError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if request.Stream {
		// After SSE headers are committed, transport errors can only terminate
		// the stream; writing a JSON error would corrupt the SSE response.
		if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Content != nil && strings.Contains(*request.Messages[len(request.Messages)-1].Content, ":STALL:") {
			_ = fixtureStall(r.Context(), w, request.Model, content)
			return
		}
		_ = fixtureStream(r.Context(), w, request.Model, content)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-bench", "object": "chat.completion", "created": 0,
		"model": request.Model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]string{"role": "assistant", "content": content},
		}},
		"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 128, "total_tokens": 129},
	})
}

func fixtureContent(request fixtureRequest) (string, error) {
	if strings.TrimSpace(request.Model) == "" || len(request.Messages) == 0 {
		return "", errors.New("model and nonempty messages are required")
	}
	for _, message := range request.Messages {
		if message.Content == nil {
			return "", errors.New("every message content must be a string")
		}
	}
	// Prefer the final prompt; the first message supports callers that place
	// their correlation nonce in the initial instruction instead.
	nonce := fixtureNoncePattern.FindString(*request.Messages[len(request.Messages)-1].Content)
	if nonce == "" {
		nonce = fixtureNoncePattern.FindString(*request.Messages[0].Content)
	}
	if nonce == "" {
		return "", errors.New("first or last message must contain BENCH:<hex>:")
	}
	if len(nonce) > fixtureContentBytes {
		return "", errors.New("nonce prefix exceeds 1024 bytes")
	}
	return nonce + strings.Repeat("x", fixtureContentBytes-len(nonce)), nil
}
func fixtureAnthropicHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fixtureError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, fixtureBodyLimit)
	defer r.Body.Close()
	var request struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model == "" || len(request.Messages) == 0 {
		fixtureError(w, http.StatusBadRequest, "invalid messages request")
		return
	}
	var prompt string
	if err := json.Unmarshal(request.Messages[len(request.Messages)-1].Content, &prompt); err != nil {
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(request.Messages[len(request.Messages)-1].Content, &blocks) != nil || len(blocks) == 0 {
			fixtureError(w, http.StatusBadRequest, "message content must be text")
			return
		}
		var text strings.Builder
		for _, block := range blocks {
			text.WriteString(block.Text)
		}
		prompt = text.String()
	}
	nonce := fixtureNoncePattern.FindString(prompt)
	if !fixtureNoncePattern.MatchString(prompt) {
		fixtureError(w, http.StatusBadRequest, "first or last message must contain BENCH:<hex>:")
		return
	}
	if len(nonce) > fixtureContentBytes {
		fixtureError(w, http.StatusBadRequest, "nonce prefix exceeds 1024 bytes")
		return
	}
	content := nonce + strings.Repeat("x", fixtureContentBytes-len(nonce))
	if request.Stream && strings.Contains(prompt, ":STALL:") {
		fixtureActiveStalls.Add(1)
		defer fixtureActiveStalls.Add(-1)
	}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg-bench", "type": "message", "role": "assistant", "model": request.Model, "content": []any{map[string]any{"type": "text", "text": content}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 128}})
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		fixtureError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	writeEvent := func(name string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := writeEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-bench", "type": "message", "role": "assistant", "model": request.Model, "content": []any{}, "usage": map[string]int{"input_tokens": 1}}}); err != nil {
		return
	}
	if err := writeEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}}); err != nil {
		return
	}
	for offset := 0; offset < fixtureContentBytes; offset += fixtureChunkBytes {
		if offset > 0 {
			timer := time.NewTimer(time.Millisecond)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if err := writeEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": content[offset : offset+fixtureChunkBytes]}}); err != nil {
			return
		}
		if offset == 0 && strings.Contains(prompt, ":STALL:") {
			<-r.Context().Done()
			return
		}
	}
	_ = writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	_ = writeEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 128}})
	_ = writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func fixtureError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message, "type": "invalid_request_error", "code": status,
		},
	})
}

func fixtureStall(ctx context.Context, w http.ResponseWriter, model, content string) error {
	fixtureActiveStalls.Add(1)
	defer fixtureActiveStalls.Add(-1)
	if _, ok := w.(http.Flusher); !ok {
		return errors.New("fixture: response writer cannot flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	if err := fixtureEvent(w, controller, model, content[:fixtureChunkBytes], false); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}
func fixtureStream(ctx context.Context, w http.ResponseWriter, model, content string) error {
	if _, ok := w.(http.Flusher); !ok {
		fixtureError(w, http.StatusInternalServerError, "streaming unsupported")
		return errors.New("fixture: response writer cannot flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	for offset := 0; offset < fixtureContentBytes; offset += fixtureChunkBytes {
		if offset > 0 {
			timer := time.NewTimer(time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fixtureEvent(w, controller, model, content[offset:offset+fixtureChunkBytes], false); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fixtureEvent(w, controller, model, "", true); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return controller.Flush()
}

func fixtureEvent(w io.Writer, controller *http.ResponseController, model, content string, finished bool) error {
	var finishReason any
	if finished {
		finishReason = "stop"
	}
	frame := map[string]any{
		"id": "chatcmpl-bench", "object": "chat.completion.chunk", "created": 0,
		"model": model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": finishReason,
			"delta": map[string]string{"content": content},
		}},
	}
	if finished {
		frame["usage"] = map[string]int{"prompt_tokens": 1, "completion_tokens": 128, "total_tokens": 129}
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return controller.Flush()
}
