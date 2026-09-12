//go:build unix

// Behavioral regression for the IdentitySource lease path: bounded
// regular-file credential reads, tenant-isolated cache keys, and context
// checks. Companion evidence: .artifacts/exploratory-credential-runtime-proof.json
package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"hoorific/internal/core"
)

type identitySentinelLease struct{ tag string }

func (s identitySentinelLease) Authorize(context.Context, *http.Request) error {
	return errors.New("sentinel:" + s.tag)
}

type identityStubStored struct{ lease core.CredentialLease }

func (s identityStubStored) Lease(context.Context, core.Connection) (core.CredentialLease, error) {
	if s.lease == nil {
		return nil, errors.New("stub stored source has no lease")
	}
	return s.lease, nil
}

func identityCloudConn(tenant, id string, version int64, file string) core.Connection {
	return core.Connection{TenantID: tenant, ID: id, Version: version,
		Settings: map[string]string{"auth_mode": "google_service_account", "credential_file": file}}
}

// A FIFO, character device, or directory credential_file must be rejected by
// the stat gate before any open: pre-fix, the FIFO open blocked while holding
// the global IdentitySource mutex and ignored context cancellation.
func TestIdentitySourceCredentialFileMustBeRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file semantics")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "cred.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewIdentitySource(identityStubStored{})
	done := make(chan error, 1)
	go func() {
		_, err := src.Lease(context.Background(), identityCloudConn("t", "c", 1, fifo))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("expected regular-file rejection without blocking, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO credential_file blocked instead of being rejected before open")
	}
	// /dev/zero must be rejected by the same gate without ever being read.
	if _, err := src.Lease(context.Background(), identityCloudConn("t", "c", 1, "/dev/zero")); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected regular-file rejection for /dev/zero, got: %v", err)
	}
	if _, err := src.Lease(context.Background(), identityCloudConn("t", "c", 1, dir)); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected regular-file rejection for directory, got: %v", err)
	}
}

// Oversized regular files must be rejected by size; a small valid-size file
// must still flow through the bounded read to the credential parser.
func TestIdentitySourceCredentialFileSizeLimit(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4<<20 + 1); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	src := NewIdentitySource(identityStubStored{})
	if _, err := src.Lease(context.Background(), identityCloudConn("t", "big", 1, big)); err == nil || !strings.Contains(err.Error(), "no larger than") {
		t.Fatalf("expected size-limit rejection, got: %v", err)
	}
	small := filepath.Join(dir, "small.json")
	if err := os.WriteFile(small, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Lease(context.Background(), identityCloudConn("t", "small", 1, small)); err == nil || strings.Contains(err.Error(), "no larger than") {
		t.Fatalf("bounded read should still reach the credential parser, got: %v", err)
	}
}

// Cache keys must not collide across tenant/connection tuples when IDs
// contain "/" (validRef permits it): a/b + c and a + b/c are distinct leases.
func TestIdentitySourceLeaseCacheIsolatesTenantAndConnection(t *testing.T) {
	src := NewIdentitySource(identityStubStored{})
	src.mu.Lock()
	src.leases["a/b\x00c"] = cachedLease{version: 7, lease: identitySentinelLease{tag: "tenant=a/b,conn=c"}}
	src.mu.Unlock()
	missing := filepath.Join(t.TempDir(), "missing.json")
	if l, err := src.Lease(context.Background(), identityCloudConn("a", "b/c", 7, missing)); err == nil {
		t.Fatalf("colliding tuple served a cross-tenant lease: %T", l)
	}
	l, err := src.Lease(context.Background(), identityCloudConn("a/b", "c", 7, missing))
	if err != nil {
		t.Fatalf("owning tuple lost its cache hit: %v", err)
	}
	if s, ok := l.(identitySentinelLease); !ok || s.tag != "tenant=a/b,conn=c" {
		t.Fatalf("unexpected lease %T", l)
	}
}

// Cancellation must be observed before any credential-file work.
func TestIdentitySourceLeaseHonorsCancelledContext(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "cred.json")
	if err := os.WriteFile(ok, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewIdentitySource(identityStubStored{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Lease(ctx, identityCloudConn("t", "c", 1, ok)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// The api_key fast path must still delegate to the stored source untouched.
func TestIdentitySourceAPIKeyPathUnchanged(t *testing.T) {
	src := NewIdentitySource(identityStubStored{lease: identitySentinelLease{tag: "stored"}})
	c := core.Connection{TenantID: "t", ID: "c", Version: 1, Settings: map[string]string{"auth_mode": "api_key"}}
	l, err := src.Lease(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := l.(identitySentinelLease); !ok || s.tag != "stored" {
		t.Fatalf("api_key mode must still delegate to the stored source, got %T", l)
	}
}
