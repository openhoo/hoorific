package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hoorific/internal/core"
)

func idempotencyTestRequest(tenant, subject, key, fingerprint, owner string) core.IdempotencyRequest {
	return core.IdempotencyRequest{
		TenantID:    tenant,
		SubjectID:   subject,
		Key:         key,
		Fingerprint: fingerprint,
		OwnerID:     owner,
		ExpiresAt:   time.Now().UTC().Add(2 * time.Hour),
	}
}

func TestIdempotencySchemaMigratesSequentiallyFromV2(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	if _, err := s.DB.ExecContext(ctx, "DROP INDEX IF EXISTS idempotency_records_expiry"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "DROP TABLE idempotency_records"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE schema_version SET version=2 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.DB.QueryRowContext(ctx, "SELECT version FROM schema_version WHERE id=1").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("schema version = %d, want 3", version)
	}
	var tableCount int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='idempotency_records'").Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 1 {
		t.Fatalf("idempotency table count = %d, want 1", tableCount)
	}
}
func TestIdempotencySchemaMigratesSequentiallyFromV1(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	if _, err := s.DB.ExecContext(ctx, "DROP INDEX IF EXISTS idempotency_records_expiry"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "DROP TABLE idempotency_records"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "DROP INDEX IF EXISTS attempts_tenant_request"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE schema_version SET version=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.DB.QueryRowContext(ctx, "SELECT version FROM schema_version WHERE id=1").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("schema version = %d, want 3", version)
	}
	var indexCount int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='attempts_tenant_request'").Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("v2 migration index count = %d, want 1", indexCount)
	}
}

func TestIdempotencyConcurrentClaimHasOneOwner(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	base := idempotencyTestRequest("tenant-a", "subject-a", "same-key", "same-fingerprint", "")
	const workers = 16
	type result struct {
		state string
		err   error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			req := base
			req.OwnerID = fmt.Sprintf("owner-%032d", i)
			record, err := s.BeginIdempotency(ctx, req)
			results <- result{state: record.State, err: err}
		}(i)
	}
	wg.Wait()
	close(results)
	newClaims := 0
	pendingClaims := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent claim failed: %v", result.err)
		}
		switch result.state {
		case "new":
			newClaims++
		case "pending":
			pendingClaims++
		default:
			t.Fatalf("unexpected concurrent claim state %q", result.state)
		}
	}
	if newClaims != 1 || pendingClaims != workers-1 {
		t.Fatalf("claims new=%d pending=%d, want 1/%d", newClaims, pendingClaims, workers-1)
	}
}

func TestIdempotencyReplayIsScopedEncryptedAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	cfg := s.Config
	req := idempotencyTestRequest("tenant-a", "subject-a", "model-response-key", "request-fingerprint", "owner-aaaaaaaaaaaaaaaa")
	record, err := s.BeginIdempotency(ctx, req)
	if err != nil || record.State != "new" {
		t.Fatalf("initial claim = %#v, %v", record, err)
	}
	response := &core.IdempotencyResponse{
		Status: 201,
		Header: http.Header{"Content-Type": {"application/json"}, "X-Duplicate": {"one", "two"}},
		Body:   []byte("model-secret-response"),
	}
	if err := s.FinishIdempotency(ctx, req, response); err != nil {
		t.Fatal(err)
	}

	keyHash := idempotencyHash("key", req.Key)
	var state, storedFingerprint, owner, keyID string
	var nonce, ciphertext []byte
	if err := s.DB.QueryRowContext(ctx, "SELECT state,fingerprint_hash,owner_id,key_id,nonce,ciphertext FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=?", req.TenantID, req.SubjectID, keyHash).Scan(&state, &storedFingerprint, &owner, &keyID, &nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || owner != req.OwnerID || keyID == "" || len(nonce) == 0 || len(ciphertext) == 0 {
		t.Fatalf("stored row lost completion metadata: state=%q owner=%q key_id=%q nonce=%d ciphertext=%d", state, owner, keyID, len(nonce), len(ciphertext))
	}
	if storedFingerprint == req.Fingerprint || strings.Contains(storedFingerprint, req.Fingerprint) || bytes.Contains(ciphertext, response.Body) {
		t.Fatal("plaintext key fingerprint or response leaked into encrypted row")
	}
	var rawKeyRows int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE key_hash=? OR fingerprint_hash=?", req.Key, req.Fingerprint).Scan(&rawKeyRows); err != nil {
		t.Fatal(err)
	}
	if rawKeyRows != 0 {
		t.Fatal("raw client key or fingerprint was persisted")
	}

	replayReq := req
	replayReq.OwnerID = "owner-bbbbbbbbbbbbbbbb"
	replay, err := s.BeginIdempotency(ctx, replayReq)
	if err != nil || replay.State != "complete" || replay.Response == nil {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if replay.Response.Status != response.Status || !bytes.Equal(replay.Response.Body, response.Body) || len(replay.Response.Header["X-Duplicate"]) != 2 || replay.Response.Header["X-Duplicate"][1] != "two" {
		t.Fatalf("replayed response changed: %#v", replay.Response)
	}
	for _, scoped := range []core.IdempotencyRequest{
		{TenantID: "tenant-b", SubjectID: req.SubjectID, Key: req.Key, Fingerprint: req.Fingerprint, OwnerID: "owner-cccccccccccccccc", ExpiresAt: req.ExpiresAt},
		{TenantID: req.TenantID, SubjectID: "subject-b", Key: req.Key, Fingerprint: req.Fingerprint, OwnerID: "owner-dddddddddddddddd", ExpiresAt: req.ExpiresAt},
	} {
		isolated, e := s.BeginIdempotency(ctx, scoped)
		if e != nil || isolated.State != "new" {
			t.Fatalf("cross-scope claim = %#v, %v", isolated, e)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	replayAfterRestart, err := reopened.BeginIdempotency(ctx, replayReq)
	if err != nil || replayAfterRestart.State != "complete" || replayAfterRestart.Response == nil || !bytes.Equal(replayAfterRestart.Response.Body, response.Body) {
		t.Fatalf("restart replay = %#v, %v", replayAfterRestart, err)
	}
}
func TestIdempotencyKeyringRotationUsesPurposeAndLoadsOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	oldKey := bytes.Repeat([]byte{'o'}, 32)
	newKey := bytes.Repeat([]byte{'n'}, 32)
	writeStoreKeyring(t, keyPath, "old", map[string][]byte{"old": oldKey})
	var cfg core.BootstrapConfig
	cfg.SchemaVersion = 1
	cfg.Mode = "standalone"
	cfg.DataDir = dir
	cfg.Listeners.Inference = ":0"
	cfg.Listeners.Management = "127.0.0.1:0"
	cfg.Storage.SQLite.Path = filepath.Join(dir, "state.db")
	cfg.Encryption.KeyFile = keyPath
	first, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(ctx); err != nil {
		first.Close()
		t.Fatal(err)
	}
	req := idempotencyTestRequest("tenant-a", "subject-a", "rotation-key", "rotation-fingerprint", "owner-old")
	if record, err := first.BeginIdempotency(ctx, req); err != nil || record.State != "new" {
		first.Close()
		t.Fatalf("initial claim = %#v, %v", record, err)
	}
	if err := first.FinishIdempotency(ctx, req, &core.IdempotencyResponse{Status: 200, Body: []byte("rotated")}); err != nil {
		first.Close()
		t.Fatal(err)
	}
	keyHash := idempotencyHash("key", req.Key)
	fingerprintHash := idempotencyHash("fingerprint", req.Fingerprint)
	var keyID string
	var nonce, ciphertext []byte
	if err := first.DB.QueryRowContext(ctx, "SELECT key_id,nonce,ciphertext FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=?", req.TenantID, req.SubjectID, keyHash).Scan(&keyID, &nonce, &ciphertext); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if keyID != "old" {
		first.Close()
		t.Fatalf("stored key id = %q, want deployment key id old", keyID)
	}
	block, err := aes.NewCipher(oldKey)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	rawAEAD, err := cipher.NewGCM(block)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if _, err := rawAEAD.Open(nil, nonce, ciphertext, idempotencyAAD(req.TenantID, req.SubjectID, keyHash, fingerprintHash)); err == nil {
		first.Close()
		t.Fatal("idempotency ciphertext decrypted with raw deployment key")
	}
	if err := os.WriteFile(keyPath, []byte("not a keyring"), 0600); err != nil {
		first.Close()
		t.Fatal(err)
	}
	replay, err := first.BeginIdempotency(ctx, req)
	if err != nil || replay.State != "complete" || replay.Response == nil {
		first.Close()
		t.Fatalf("replay after key-file replacement = %#v, %v", replay, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	writeStoreKeyring(t, keyPath, "new", map[string][]byte{"new": newKey, "old": oldKey})
	second, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	rotatedReplay, err := second.BeginIdempotency(ctx, req)
	if err != nil || rotatedReplay.State != "complete" || rotatedReplay.Response == nil {
		t.Fatalf("replay after key rotation = %#v, %v", rotatedReplay, err)
	}
	req2 := idempotencyTestRequest("tenant-a", "subject-a", "rotation-key-2", "rotation-fingerprint-2", "owner-new")
	if record, err := second.BeginIdempotency(ctx, req2); err != nil || record.State != "new" {
		t.Fatalf("rotated claim = %#v, %v", record, err)
	}
	if err := second.FinishIdempotency(ctx, req2, &core.IdempotencyResponse{Status: 200, Body: []byte("new-key")}); err != nil {
		t.Fatal(err)
	}
	var rotatedKeyID string
	if err := second.DB.QueryRowContext(ctx, "SELECT key_id FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=?", req2.TenantID, req2.SubjectID, idempotencyHash("key", req2.Key)).Scan(&rotatedKeyID); err != nil {
		t.Fatal(err)
	}
	if rotatedKeyID != "new" {
		t.Fatalf("rotated stored key id = %q, want new", rotatedKeyID)
	}
}

func TestIdempotencyPendingExpiryCannotReclaimAndCompletionStartsRetention(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	req := idempotencyTestRequest("tenant-a", "subject-a", "expiry-key", "expiry-fingerprint", "expiry-owner")
	if record, err := s.BeginIdempotency(ctx, req); err != nil || record.State != "new" {
		t.Fatalf("initial claim = %#v, %v", record, err)
	}
	keyHash := idempotencyHash("key", req.Key)
	if _, err := s.DB.ExecContext(ctx, "UPDATE idempotency_records SET expires_at=? WHERE tenant_id=? AND subject_id=? AND key_hash=?", time.Now().Unix()-1, req.TenantID, req.SubjectID, keyHash); err != nil {
		t.Fatal(err)
	}
	expiredRequest := req
	expiredRequest.OwnerID = "expiry-retry-owner"
	expiredRequest.ExpiresAt = time.Now().Add(-time.Minute)
	pending, err := s.BeginIdempotency(ctx, expiredRequest)
	if err != nil || pending.State != "pending" {
		t.Fatalf("expired pending claim = %#v, %v", pending, err)
	}
	if err := s.FinishIdempotency(ctx, req, &core.IdempotencyResponse{Status: 200, Body: []byte("completed-after-delay")}); err != nil {
		t.Fatal(err)
	}
	var state string
	var expiresAt, updatedAt int64
	if err := s.DB.QueryRowContext(ctx, "SELECT state,expires_at,updated_at FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=?", req.TenantID, req.SubjectID, keyHash).Scan(&state, &expiresAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || expiresAt != updatedAt+int64(idempotencyMaxRetention/time.Second) {
		t.Fatalf("completion retention state=%q expires=%d updated=%d", state, expiresAt, updatedAt)
	}
	replay, err := s.BeginIdempotency(ctx, expiredRequest)
	if err != nil || replay.State != "complete" || replay.Response == nil {
		t.Fatalf("expired request replay = %#v, %v", replay, err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE idempotency_records SET expires_at=? WHERE tenant_id=? AND subject_id=? AND key_hash=?", time.Now().Unix()-1, req.TenantID, req.SubjectID, keyHash); err != nil {
		t.Fatal(err)
	}
	fresh := req
	fresh.Fingerprint = "expiry-fingerprint-after-retention"
	fresh.OwnerID = "fresh-owner"
	fresh.ExpiresAt = time.Now().Add(time.Hour)
	if record, err := s.BeginIdempotency(ctx, fresh); err != nil || record.State != "new" {
		t.Fatalf("post-retention claim = %#v, %v", record, err)
	}
}

func TestOpenRejectsRawDeploymentKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{'r'}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg core.BootstrapConfig
	cfg.SchemaVersion = 1
	cfg.Mode = "standalone"
	cfg.DataDir = dir
	cfg.Listeners.Inference = ":0"
	cfg.Listeners.Management = "127.0.0.1:0"
	cfg.Storage.SQLite.Path = filepath.Join(dir, "state.db")
	cfg.Encryption.KeyFile = keyPath
	if _, err := Open(ctx, cfg); err == nil {
		t.Fatal("raw deployment key unexpectedly accepted")
	}
}

func TestIdempotencyFingerprintMismatchAndOwnerFenceTombstone(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	req := idempotencyTestRequest("tenant-a", "subject-a", "fenced-key", "fingerprint-a", "owner-aaaaaaaaaaaaaaaa")
	if record, err := s.BeginIdempotency(ctx, req); err != nil || record.State != "new" {
		t.Fatalf("initial claim = %#v, %v", record, err)
	}
	mismatch := req
	mismatch.Fingerprint = "fingerprint-b"
	if _, err := s.BeginIdempotency(ctx, mismatch); !errors.Is(err, core.ErrIdempotencyFingerprintConflict) {
		t.Fatalf("fingerprint mismatch error = %v", err)
	}
	wrongOwner := req
	wrongOwner.OwnerID = "owner-bbbbbbbbbbbbbbbb"
	if err := s.FinishIdempotency(ctx, wrongOwner, nil); !errors.Is(err, core.ErrIdempotencyOwnerFenced) {
		t.Fatalf("wrong owner finish error = %v", err)
	}
	if err := s.FinishIdempotency(ctx, req, nil); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"owner-cccccccccccccccc", req.OwnerID} {
		replayReq := req
		replayReq.OwnerID = owner
		record, err := s.BeginIdempotency(ctx, replayReq)
		if err != nil || record.State != "unreplayable" || record.Response != nil {
			t.Fatalf("tombstone state = %#v, %v", record, err)
		}
	}
	if err := s.FinishIdempotency(ctx, req, &core.IdempotencyResponse{Status: 200, Body: []byte("late")}); !errors.Is(err, core.ErrIdempotencyUnreplayable) {
		t.Fatalf("late completion error = %v", err)
	}
}

func TestIdempotencyRejectsUnboundedInputs(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	valid := idempotencyTestRequest("tenant-a", "subject-a", "bounded-key", "bounded-fingerprint", "owner-aaaaaaaaaaaaaaaa")
	for name, req := range map[string]core.IdempotencyRequest{
		"missing tenant":     {SubjectID: valid.SubjectID, Key: valid.Key, Fingerprint: valid.Fingerprint, OwnerID: valid.OwnerID, ExpiresAt: valid.ExpiresAt},
		"expired":            {TenantID: valid.TenantID, SubjectID: valid.SubjectID, Key: valid.Key, Fingerprint: valid.Fingerprint, OwnerID: valid.OwnerID, ExpiresAt: time.Now().Add(-time.Minute)},
		"oversized key":      {TenantID: valid.TenantID, SubjectID: valid.SubjectID, Key: strings.Repeat("k", idempotencyMaxKeyBytes+1), Fingerprint: valid.Fingerprint, OwnerID: valid.OwnerID, ExpiresAt: valid.ExpiresAt},
		"oversized identity": {TenantID: valid.TenantID, SubjectID: strings.Repeat("s", idempotencyMaxCoordinateBytes+1), Key: valid.Key, Fingerprint: valid.Fingerprint, OwnerID: valid.OwnerID, ExpiresAt: valid.ExpiresAt},
	} {
		if _, err := s.BeginIdempotency(ctx, req); !errors.Is(err, core.ErrIdempotencyInvalid) {
			t.Errorf("%s error = %v", name, err)
		}
	}
	if record, err := s.BeginIdempotency(ctx, valid); err != nil || record.State != "new" {
		t.Fatalf("valid claim = %#v, %v", record, err)
	}
	if err := s.FinishIdempotency(ctx, valid, &core.IdempotencyResponse{Status: 200, Body: bytes.Repeat([]byte{'x'}, idempotencyMaxBodyBytes+1)}); !errors.Is(err, core.ErrIdempotencyInvalid) {
		t.Fatalf("oversized body error = %v", err)
	}
	headers := make(http.Header, idempotencyMaxHeaderCount+1)
	for i := range idempotencyMaxHeaderCount + 1 {
		headers[fmt.Sprintf("X-Test-%d", i)] = []string{"value"}
	}
	if err := s.FinishIdempotency(ctx, valid, &core.IdempotencyResponse{Status: 200, Header: headers}); !errors.Is(err, core.ErrIdempotencyInvalid) {
		t.Fatalf("oversized headers error = %v", err)
	}
}

func TestIdempotencyCleanupIsBoundedAndLeavesTombstones(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	type claim struct {
		tenant, subject, key, fingerprint, owner string
	}
	claims := make([]claim, idempotencyCleanupBatch+1)
	for i := range claims {
		claims[i] = claim{
			tenant:      "tenant-a",
			subject:     "subject-a",
			key:         fmt.Sprintf("cleanup-key-%03d", i),
			fingerprint: fmt.Sprintf("cleanup-fingerprint-%03d", i),
			owner:       fmt.Sprintf("cleanup-owner-%032d", i),
		}
		req := idempotencyTestRequest(claims[i].tenant, claims[i].subject, claims[i].key, claims[i].fingerprint, claims[i].owner)
		if record, err := s.BeginIdempotency(ctx, req); err != nil || record.State != "new" {
			t.Fatalf("claim %d = %#v, %v", i, record, err)
		}
		if err := s.FinishIdempotency(ctx, req, &core.IdempotencyResponse{Status: 200, Body: []byte("done")}); err != nil {
			t.Fatal(err)
		}
	}
	pending := idempotencyTestRequest("tenant-a", "subject-a", "cleanup-pending", "cleanup-pending-fingerprint", "cleanup-pending-owner-aaaaaaaa")
	if record, err := s.BeginIdempotency(ctx, pending); err != nil || record.State != "new" {
		t.Fatalf("pending claim = %#v, %v", record, err)
	}
	if err := s.FinishIdempotency(ctx, pending, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(ctx, func(tx *sql.Tx) error {
		now, err := sqlNow(ctx, tx, s.Dialect)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.Query("UPDATE idempotency_records SET expires_at=? WHERE state='complete'"), now-1); err != nil {
			return err
		}
		return s.cleanupIdempotencyTx(ctx, tx, now)
	}); err != nil {
		t.Fatal(err)
	}
	var complete int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE state='complete'").Scan(&complete); err != nil {
		t.Fatal(err)
	}
	if complete != 1 {
		t.Fatalf("complete rows after one cleanup = %d, want 1", complete)
	}
	var unreplayable int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE state='unreplayable'").Scan(&unreplayable); err != nil {
		t.Fatal(err)
	}
	if unreplayable != 1 {
		t.Fatalf("tombstone rows after cleanup = %d, want 1", unreplayable)
	}
}

func TestIdempotencyMigrationUsesConfiguredDatabaseAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	writeStoreTestKeyring(t, keyPath, bytes.Repeat([]byte{'k'}, 32))
	var cfg core.BootstrapConfig
	cfg.SchemaVersion = 1
	cfg.Mode = "standalone"
	cfg.DataDir = dir
	cfg.Listeners.Inference = ":0"
	cfg.Listeners.Management = "127.0.0.1:0"
	cfg.Storage.SQLite.Path = filepath.Join(dir, "state.db")
	cfg.Encryption.KeyFile = keyPath
	first, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Migrate(ctx); err != nil {
		first.Close()
		t.Fatal(err)
	}
	req := idempotencyTestRequest("tenant-a", "subject-a", "restart-key", "restart-fingerprint", "restart-owner-aaaaaaaa")
	if _, err := first.BeginIdempotency(ctx, req); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	record, err := second.BeginIdempotency(ctx, req)
	if err != nil || record.State != "pending" {
		t.Fatalf("restart pending claim = %#v, %v", record, err)
	}
}
