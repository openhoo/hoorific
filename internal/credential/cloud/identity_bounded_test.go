//go:build unix

// Behavioral regression for the WIF identity token read: bounded regular-file
// semantics enforced before any open or read.
package cloud

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func wifFor(t *testing.T, tokenFile string) *WIF {
	t.Helper()
	w, err := NewWIF(WIFConfig{FederationRuleID: "rule", OrganizationID: "org", ServiceAccountID: "sa", IdentityTokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWIFIdentityTokenMustBeBoundedRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file semantics")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "token.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := wifFor(t, fifo).accessToken(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("expected regular-file rejection without blocking, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO identity token blocked instead of being rejected before open")
	}
	big := filepath.Join(dir, "big.token")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4<<20 + 1); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	if _, err := wifFor(t, big).accessToken(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("expected invalid-size rejection, got: %v", err)
	}
	empty := filepath.Join(dir, "empty.token")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wifFor(t, empty).accessToken(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("expected invalid-size rejection for empty token, got: %v", err)
	}
}
