package transport

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type eofOnCloseBody struct {
	closed chan struct{}
}

func (b *eofOnCloseBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *eofOnCloseBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestIdleReaderReportsDeadlineWhenIdleCloseReturnsEOF(t *testing.T) {
	body := &eofOnCloseBody{closed: make(chan struct{})}
	reader := NewIdleReader(context.Background(), body, 5*time.Millisecond)
	defer reader.Close()

	done := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		n, err := reader.Read(make([]byte, 1))
		done <- struct {
			n   int
			err error
		}{n: n, err: err}
	}()

	select {
	case result := <-done:
		if result.n != 0 {
			t.Fatalf("read bytes = %d, want 0", result.n)
		}
		if !errors.Is(result.err, context.DeadlineExceeded) {
			t.Fatalf("read error = %v, want context deadline exceeded", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle reader did not return after its underlying body was closed")
	}
}
