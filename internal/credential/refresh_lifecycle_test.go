package credential

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"hoorific/internal/core"
)

type lifecycleTestRepository struct {
	mu          sync.Mutex
	record      Record
	intent      *RefreshIntent
	pending     bool
	usableLoads int
}

func (r *lifecycleTestRepository) LoadCredential(_ context.Context, tenant, id string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Identity.TenantID != tenant || r.record.Identity.ConnectionID != id {
		return Record{}, ErrConflict
	}
	return r.record, nil
}

func (r *lifecycleTestRepository) LoadUsableCredential(_ context.Context, tenant, id string) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usableLoads++
	if r.record.Identity.TenantID != tenant || r.record.Identity.ConnectionID != id {
		return Record{}, ErrConflict
	}
	if r.pending {
		return Record{}, ErrReauthRequired
	}
	return r.record, nil
}

func (r *lifecycleTestRepository) PutCredential(_ context.Context, next Record, expected int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Identity.Version != expected || next.Identity.Version != expected+1 {
		return ErrConflict
	}
	if next.Identity.TenantID != r.record.Identity.TenantID || next.Identity.ConnectionID != r.record.Identity.ConnectionID || next.Identity.CredentialID != r.record.Identity.CredentialID || next.Identity.AccountID != r.record.Identity.AccountID || next.Identity.Provider != r.record.Identity.Provider {
		return ErrConflict
	}
	r.record = next
	r.pending = false
	r.intent = nil
	return nil
}

func (r *lifecycleTestRepository) BeginRefresh(_ context.Context, intent RefreshIntent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending || intent.Identity != r.record.Identity {
		return ErrConflict
	}
	r.pending = true
	r.intent = &intent
	return nil
}

func (r *lifecycleTestRepository) CommitRefresh(_ context.Context, intent RefreshIntent, next Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pending || r.intent == nil || *r.intent != intent || intent.Identity != r.record.Identity || next.Identity.Version != r.record.Identity.Version+1 {
		return ErrConflict
	}
	r.record = next
	r.pending = false
	r.intent = nil
	return nil
}

func (r *lifecycleTestRepository) FailRefresh(_ context.Context, intent RefreshIntent, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pending || r.intent == nil || *r.intent != intent {
		return ErrConflict
	}
	r.pending = false
	r.intent = nil
	return nil
}

func lifecycleTestIdentity() Identity {
	return Identity{TenantID: "tenant-a", ConnectionID: "connection-a", CredentialID: "credential-a", AccountID: "account-a", Provider: "provider-a", Version: 1}
}

func lifecycleTestKeyring() StaticKeyring {
	return StaticKeyring{ID: "key-a", Key: []byte("01234567890123456789012345678901")}
}

func lifecycleTestRecord(t *testing.T, keys Keyring, id Identity, token OAuthToken) Record {
	t.Helper()
	envelope, err := Seal(keys, id, Secret{OAuth: &token})
	if err != nil {
		t.Fatal(err)
	}
	return Record{Identity: id, Envelope: envelope, Status: "active", RotatedAt: time.Now().UTC()}
}

func TestRefreshFenceBlocksOldLeaseAndAcceptsFreshReauthentication(t *testing.T) {
	ctx := context.Background()
	keys := lifecycleTestKeyring()
	id := lifecycleTestIdentity()
	repo := &lifecycleTestRepository{record: lifecycleTestRecord(t, keys, id, OAuthToken{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(time.Hour),
	})}
	manager, err := NewManager(keys, repo)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewRefreshingSource(manager, repo, func(context.Context, core.Connection) (*OAuthClient, error) {
		t.Fatal("refresh client factory must not run while a refresh fence is pending")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := RefreshIntent{Identity: id, AttemptID: "attempt-a", Fence: "fence-a", LeaseUntil: time.Now().Add(time.Minute)}
	if err := repo.BeginRefresh(ctx, intent); err != nil {
		t.Fatal(err)
	}
	_, err = source.Lease(ctx, core.Connection{TenantID: id.TenantID, ID: id.ConnectionID, Connector: id.Provider, AccountID: id.AccountID})
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("pending refresh lease error = %v, want %v", err, ErrReauthRequired)
	}
	if repo.usableLoads == 0 {
		t.Fatal("refreshing source did not use the atomic usable-credential loader")
	}

	fresh := OAuthToken{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := manager.Put(ctx, id, Secret{OAuth: &fresh}); err != nil {
		t.Fatalf("fresh reauthentication replacement failed: %v", err)
	}
	if repo.record.Identity.Version != 2 {
		t.Fatalf("fresh replacement version = %d, want 2", repo.record.Identity.Version)
	}

	lateID := id
	lateID.Version++
	late := lifecycleTestRecord(t, keys, lateID, OAuthToken{AccessToken: "late-refresh", RefreshToken: "late-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)})
	if err := repo.CommitRefresh(ctx, intent, late); !errors.Is(err, ErrConflict) {
		t.Fatalf("late refresh commit error = %v, want %v", err, ErrConflict)
	}
	opened, err := Open(keys, repo.record.Identity, repo.record.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if opened.OAuth == nil || opened.OAuth.AccessToken != fresh.AccessToken {
		t.Fatalf("late refresh replaced fresh credential: %#v", opened)
	}
}

func TestRefreshRejectsTokenExpiredAtRefresherClock(t *testing.T) {
	ctx := context.Background()
	keys := lifecycleTestKeyring()
	id := lifecycleTestIdentity()
	now := time.Date(2040, time.January, 2, 3, 4, 5, 0, time.UTC)
	repo := &lifecycleTestRepository{record: lifecycleTestRecord(t, keys, id, OAuthToken{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    now.Add(time.Second),
	})}
	manager, err := NewManager(keys, repo)
	if err != nil {
		t.Fatal(err)
	}
	refresher, err := NewRefresher(manager, repo)
	if err != nil {
		t.Fatal(err)
	}
	refresher.now = func() time.Time { return now }
	_, err = refresher.Refresh(ctx, id, func(context.Context, OAuthToken) (OAuthToken, error) {
		return OAuthToken{AccessToken: "expired-at-clock", RefreshToken: "new-refresh", TokenType: "Bearer", ExpiresAt: now.Add(-time.Second)}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "provider returned expired token") {
		t.Fatalf("expired provider token error = %v", err)
	}
	if repo.record.Identity.Version != id.Version || repo.pending {
		t.Fatalf("invalid refresh changed durable state: version=%d pending=%t", repo.record.Identity.Version, repo.pending)
	}
}

var _ Repository = (*lifecycleTestRepository)(nil)
