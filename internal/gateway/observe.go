package gateway

import (
	"context"
	"io"
	"net/http"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/protocol"
	"hoorific/internal/transport"
)

type observation struct {
	usage    *core.Usage
	terminal bool
	err      error
}
type teeBody struct {
	io.Reader
	closer io.Closer
}

func (t teeBody) Close() error { return t.closer.Close() }

// One observer per response, never one goroutine per token. The pipe applies
// backpressure; decoding failure switches to drain without changing native bytes.
func observeResponse(ctx context.Context, response *http.Response, entry protocol.Entry, stream bool) (func() observation, bool) {
	if stream && entry.Stream == nil || !stream && entry.Result == nil {
		return func() observation { return observation{} }, false
	}
	reader, writer := io.Pipe()
	done := make(chan observation, 1)
	original := response.Body
	response.Body = teeBody{Reader: io.TeeReader(original, writer), closer: original}
	go func() {
		var result observation
		if stream {
			decoder, e := entry.Stream.NewDecoder(reader)
			if e != nil {
				result.err = e
			} else {
				for {
					event, e := decoder.Next(ctx)
					if e != nil {
						if e != io.EOF {
							result.err = e
						}
						break
					}
					gatewayObserveEvent(ctx, event)
					switch v := event.(type) {
					case core.Usage:
						u := v
						result.usage = &u
					case core.Finish:
						result.terminal = true
					case core.StreamError:
						result.err = v.Error
					}
				}
			}
		}
		if !stream {
			payload, e := entry.Result.DecodeResult(ctx, io.LimitReader(reader, 32<<20))
			result.err = e
			if e == nil {
				gatewayObserveResult(ctx, payload)
				result.usage = resultUsage(payload)
				result.terminal = true
			}
		}
		_, _ = io.Copy(io.Discard, reader)
		_ = reader.Close()
		done <- result
	}()
	return func() observation { _ = writer.Close(); return <-done }, true
}
func relayNative(ctx context.Context, w http.ResponseWriter, response *http.Response, entry protocol.Entry, stream bool) (transport.RelayResult, observation, error) {
	finish, observed := observeResponse(ctx, response, entry, stream)
	result, err := transport.RelayHTTP(ctx, w, response, transport.RelayOptions{BufferSize: 32 << 10, MaxObservedBytes: eventLimit(ctx), ReadIdle: 120 * time.Second, WriteTimeout: 30 * time.Second})
	seen := finish()
	if !observed && !stream {
		seen.terminal = err == nil
	}
	return result, seen, err
}
