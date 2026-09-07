package transport_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"hoorific/internal/transport"
)

func TestSpoolReplayAndMarkedOrphanCleanup(t *testing.T) {
	dir := t.TempDir()
	cfg := transport.SpoolConfig{Dir: dir, MaxTenantBytes: 1 << 20, MaxTotalBytes: 2 << 20}
	spool, err := transport.NewSpool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("private multipart content\n"), 4000)
	body, err := spool.Capture(context.Background(), "tenant", "request", bytes.NewReader(payload), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	reader, err := body.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("encrypted replay differs: %v", err)
	}
	unrelated := filepath.Join(dir, "hoorific-spool-unrelated.spool")
	if err = os.WriteFile(unrelated, []byte("not an owned spool"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := transport.NewSpool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.CleanupOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(unrelated); err != nil {
		t.Fatalf("unmarked file was removed: %v", err)
	}
	if reopened, err := body.Open(context.Background()); err == nil {
		reopened.Close()
		t.Fatal("marked orphan survived startup cleanup")
	}
}
