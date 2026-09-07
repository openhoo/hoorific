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
