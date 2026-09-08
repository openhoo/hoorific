package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type RelayOptions struct {
	BufferSize             int
	MaxObservedBytes       int64
	ReadIdle, WriteTimeout time.Duration
	Observe                func([]byte)
}
type RelayResult struct {
	Bytes                 int64
	ObservedBytes         int64
	Committed             bool
	Truncated             bool
	CancellationRequested bool
}

var relayBuffers = sync.Pool{New: func() any { return make([]byte, 32<<10) }}

// RelayHTTP copies an opaque upstream response without accumulating it. Observe receives a
// temporary bounded view valid only during the callback. A single timer closes the upstream body
// when the read-idle bound expires; no per-chunk goroutines or synthetic terminal events exist.
func RelayHTTP(ctx context.Context, w http.ResponseWriter, response *http.Response, opt RelayOptions) (RelayResult, error) {
	if response == nil || response.Body == nil {
		return RelayResult{}, errors.New("nil upstream response")
	}
	if opt.BufferSize <= 0 {
		opt.BufferSize = 32 << 10
	}
	if opt.BufferSize > 1<<20 {
		opt.BufferSize = 1 << 20
	}
	if opt.ReadIdle <= 0 {
		opt.ReadIdle = 120 * time.Second
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = 30 * time.Second
	}
	headers := response.Header.Clone()
	SanitizeResponseHeaders(headers)
	for k, v := range headers {
		for _, x := range v {
			w.Header().Add(k, x)
		}
	}
	w.WriteHeader(response.StatusCode)
	result := RelayResult{Committed: true}
	buf := relayBuffers.Get().([]byte)
	if cap(buf) < opt.BufferSize {
		buf = make([]byte, opt.BufferSize)
	}
	buf = buf[:opt.BufferSize]
	defer relayBuffers.Put(buf[:32<<10])
	var idleExpired atomic.Bool
	idle := time.AfterFunc(opt.ReadIdle, func() { idleExpired.Store(true); _ = response.Body.Close() })
	defer idle.Stop()
	stop := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
	defer stop()
	var observed int64
	controller := http.NewResponseController(w)
	for {
		if err := ctx.Err(); err != nil {
			result.CancellationRequested = true
			return result, err
		}
		idle.Reset(opt.ReadIdle)
		n, err := response.Body.Read(buf)
		idle.Stop()
		if n > 0 {
			if opt.Observe != nil && opt.MaxObservedBytes > observed {
				take := int64(n)
				if take > opt.MaxObservedBytes-observed {
					take = opt.MaxObservedBytes - observed
				}
				if take > 0 {
					opt.Observe(buf[:take])
					observed += take
					result.ObservedBytes = observed
				}
			}
			_ = controller.SetWriteDeadline(time.Now().Add(opt.WriteTimeout))
			wn, we := w.Write(buf[:n])
			result.Bytes += int64(wn)
			if we != nil || wn != n {
				_ = controller.SetWriteDeadline(time.Time{})
				result.CancellationRequested = true
				return result, we
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			result.Truncated = true
			if idleExpired.Load() || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				result.CancellationRequested = true
			}
			return result, err
		}
	}
}

type IdleReader struct {
	ctx     context.Context
	body    io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	stop    func() bool
	expired atomic.Bool
}

var ErrReadLimit = errors.New("response body exceeds read limit")

func NewIdleReader(ctx context.Context, body io.ReadCloser, idle time.Duration) *IdleReader {
	if idle <= 0 {
		idle = 120 * time.Second
	}
	reader := &IdleReader{ctx: ctx, body: body, idle: idle}
	reader.timer = time.AfterFunc(idle, func() {
		reader.expired.Store(true)
		_ = body.Close()
	})
	reader.stop = context.AfterFunc(ctx, func() { _ = body.Close() })
	return reader
}

func (r *IdleReader) Read(p []byte) (int, error) {
	if err := r.stopContext(); err != nil {
		return 0, err
	}
	r.timer.Reset(r.idle)
	n, err := r.body.Read(p)
	r.timer.Stop()
	if r.expired.Load() {
		// Preserve bytes returned by the underlying read, but never turn an
		// idle-close EOF into successful end-of-stream.
		return n, context.DeadlineExceeded
	}
	if n == 0 && err != nil {
		if contextErr := r.stopContext(); contextErr != nil {
			return 0, contextErr
		}
	}
	return n, err
}

func (r *IdleReader) stopContext() error {
	if r.ctx == nil {
		return nil
	}
	return r.ctx.Err()
}

func (r *IdleReader) Close() error {
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.stop != nil {
		r.stop()
	}
	if r.body == nil {
		return nil
	}
	return r.body.Close()
}

// ReadBounded drains at most maxBytes while retaining cancellable read-idle
// behavior. It is used for bounded acknowledgement/error bodies only.
func ReadBounded(ctx context.Context, body io.ReadCloser, maxBytes int64, idle time.Duration) ([]byte, error) {
	if body == nil || maxBytes <= 0 {
		return nil, errors.New("invalid bounded response read")
	}
	reader := NewIdleReader(ctx, body, idle)
	defer reader.Close()
	buf := make([]byte, 32<<10)
	out := make([]byte, 0, min(maxBytes, int64(cap(buf))))
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if int64(len(out))+int64(n) > maxBytes {
				return nil, ErrReadLimit
			}
			out = append(out, buf[:n]...)
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
	}
}
