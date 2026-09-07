package realtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type RelayOptions struct {
	MaxFrameBytes                            int64
	IdleTimeout, MaxSession                  time.Duration
	ValidateClient, ValidateServer           func([]byte) error
	ValidateClientFrame, ValidateServerFrame func(websocket.MessageType, []byte) error
}
type RelayResult struct {
	ClientToServerFrames, ServerToClientFrames int64
	ClientBytes, ServerBytes                   int64
	CancellationRequested                      bool
	Err                                        error
}

// Relay forwards complete native frames in order; one reader/writer loop owns each direction.
// Closing a socket records cancellation requested, never proof that provider billing stopped.
func Relay(ctx context.Context, client, server *websocket.Conn, opt RelayOptions) RelayResult {
	if opt.MaxFrameBytes <= 0 {
		opt.MaxFrameBytes = 1 << 20
	}
	if opt.IdleTimeout <= 0 {
		opt.IdleTimeout = 120 * time.Second
	}
	if opt.MaxSession <= 0 {
		opt.MaxSession = 60 * time.Minute
	}
	client.SetReadLimit(opt.MaxFrameBytes)
	server.SetReadLimit(opt.MaxFrameBytes)
	runCtx, cancel := context.WithTimeout(ctx, opt.MaxSession)
	defer cancel()
	var mu sync.Mutex
	res := RelayResult{}
	errs := make(chan relayError, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(src, dst *websocket.Conn, validate func([]byte) error, validateFrame func(websocket.MessageType, []byte) error, fromClient bool) {
		defer wg.Done()
		for {
			readCtx, done := context.WithTimeout(runCtx, opt.IdleTimeout)
			typ, data, err := src.Read(readCtx)
			done()
			if err != nil {
				errs <- relayError{err, dst}
				return
			}
			if validateFrame != nil {
				if err = validateFrame(typ, data); err != nil {
					errs <- relayError{err, dst}
					return
				}
			} else if validate != nil {
				if err = validate(data); err != nil {
					errs <- relayError{err, dst}
					return
				}
			}
			writeCtx, done := context.WithTimeout(runCtx, opt.IdleTimeout)
			err = dst.Write(writeCtx, typ, data)
			done()
			if err != nil {
				errs <- relayError{err, dst}
				return
			}
			mu.Lock()
			if fromClient {
				res.ClientToServerFrames++
				res.ClientBytes += int64(len(data))
			} else {
				res.ServerToClientFrames++
				res.ServerBytes += int64(len(data))
			}
			mu.Unlock()
		}
	}
	go pump(client, server, opt.ValidateClient, opt.ValidateClientFrame, true)
	go pump(server, client, opt.ValidateServer, opt.ValidateServerFrame, false)
	failure := <-errs
	cancel()
	propagateClose(failure.peer, failure.err)
	_ = client.CloseNow()
	_ = server.CloseNow()
	wg.Wait()
	res.CancellationRequested = true
	if IsCleanClose(failure.err) {
		res.Err = nil
	} else {
		res.Err = failure.err
	}
	return res
}

type relayError struct {
	err  error
	peer *websocket.Conn
}

func propagateClose(peer *websocket.Conn, err error) {
	if peer == nil {
		return
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		_ = peer.Close(ce.Code, ce.Reason)
		return
	}
	if IsCleanClose(err) {
		_ = peer.Close(websocket.StatusNormalClosure, "")
		return
	}
	_ = peer.Close(websocket.StatusGoingAway, "")
}
func IsCleanClose(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || websocket.CloseStatus(err) == websocket.StatusNormalClosure || websocket.CloseStatus(err) == websocket.StatusGoingAway
}
