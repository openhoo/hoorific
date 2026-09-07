package core

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type noErrorLease struct {
	closed bool
}

func (*noErrorLease) Authorize(context.Context, *http.Request) error { return nil }
func (l *noErrorLease) Close()                                       { l.closed = true }

type errorLease struct {
	closed bool
	err    error
}

func (*errorLease) Authorize(context.Context, *http.Request) error { return nil }
func (l *errorLease) Close() error {
	l.closed = true
	return l.err
}

func TestCloseCredentialLeaseSupportsOptionalClosers(t *testing.T) {
	if err := CloseCredentialLease(nil); err != nil {
		t.Fatalf("nil lease close: %v", err)
	}
	plain := &noErrorLease{}
	if err := CloseCredentialLease(plain); err != nil {
		t.Fatalf("no-error lease close: %v", err)
	}
	if !plain.closed {
		t.Fatal("no-error lease was not closed")
	}
	expected := errors.New("close failed")
	withError := &errorLease{err: expected}
	if err := CloseCredentialLease(withError); !errors.Is(err, expected) {
		t.Fatalf("close error = %v, want %v", err, expected)
	}
	if !withError.closed {
		t.Fatal("error lease was not closed")
	}
}
