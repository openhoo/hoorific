package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/credential"
	"hoorific/internal/resource"
)

func TestOAuthSessionProjectionIsSafeScopedAndPaged(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	ownerA := bootstrapTenant(t, s, "tenant-a", "owner-a")
	ownerB := bootstrapTenant(t, s, "tenant-b", "owner-b")
	expires := time.Now().Add(time.Hour)
	for _, session := range []credential.LoginSession{
		{
			StateHash: "state-hash-a", TenantID: ownerA.TenantID, AdminSessionID: "admin-session-a", Connector: "fixture", ConnectionID: "conn-a", AccountID: "account-a", Nonce: "nonce-a", Verifier: "pkce-a", VerifierEnvelope: credential.Envelope{KeyID: "secret-key-a", Ciphertext: []byte("cipher-a")}, RedirectURI: "https://admin.example/callback", ConnectionVersion: 1, ExpiresAt: expires,
		},
		{
			StateHash: "state-hash-b", TenantID: ownerA.TenantID, AdminSessionID: "admin-session-b", Connector: "fixture", ConnectionID: "conn-b", AccountID: "account-b", Nonce: "nonce-b", Verifier: "pkce-b", VerifierEnvelope: credential.Envelope{KeyID: "secret-key-b", Ciphertext: []byte("cipher-b")}, RedirectURI: "https://admin.example/callback", ConnectionVersion: 1, ExpiresAt: expires,
		},
		{
			StateHash: "state-hash-other", TenantID: ownerB.TenantID, AdminSessionID: "admin-session-other", Connector: "fixture", ConnectionID: "conn-other", AccountID: "account-other", Nonce: "nonce-other", Verifier: "pkce-other", VerifierEnvelope: credential.Envelope{KeyID: "secret-key-other", Ciphertext: []byte("cipher-other")}, RedirectURI: "https://admin.example/callback", ConnectionVersion: 1, ExpiresAt: expires,
		},
	} {
		if err := s.SaveOAuthLogin(ctx, session); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.List(ctx, ownerA, "oauth_sessions", "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("expected one safe OAuth projection and a cursor, got %+v, %v", page, err)
	}
	var data admin.OAuthSessionData
	if err := json.Unmarshal(page.Items[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Connector != "fixture" || data.Status != "pending" || data.ExpiresAt == "" {
		t.Fatalf("unexpected OAuth projection: %+v", data)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(page.Items[0].Data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["connector"] == nil || fields["status"] == nil || fields["expires_at"] == nil {
		t.Fatalf("OAuth projection has unsafe or unexpected fields: %s", page.Items[0].Data)
	}
	response := string(page.Items[0].Data) + page.NextCursor
	for _, secret := range []string{"state-hash-a", "state-hash-b", "nonce-a", "pkce-a", "cipher-a", "secret-key-a", "admin-session-a"} {
		if strings.Contains(response, secret) {
			t.Fatalf("OAuth projection leaked %q: %s", secret, response)
		}
	}
	parts := strings.Split(page.NextCursor, ".")
	if len(parts) != 2 {
		t.Fatalf("malformed cursor %q", page.NextCursor)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || strings.Contains(string(payload), "state-hash") {
		t.Fatalf("OAuth cursor disclosed durable state: %s", payload)
	}

	next, err := s.List(ctx, ownerA, "oauth_sessions", page.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("expected second tenant-scoped OAuth projection, got %+v, %v", next, err)
	}
	got, err := s.Get(ctx, ownerA, "oauth_sessions", page.Items[0].ID)
	if err != nil || got.ID != page.Items[0].ID {
		t.Fatalf("OAuth get by safe projection ID failed: %+v, %v", got, err)
	}
	if _, err = s.List(ctx, ownerB, "oauth_sessions", page.NextCursor, 1); err == nil {
		t.Fatal("OAuth cursor must not cross tenant scope")
	}
	if _, err = s.Get(ctx, ownerB, "oauth_sessions", page.Items[0].ID); err == nil {
		t.Fatal("OAuth get must not cross tenant scope")
	}
}

func TestUpstreamOperationProjectionUsesJobIdentityAndRejectsAmbiguousGet(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant", "owner")
	now := time.Now()
	for _, job := range []resource.Job{
		{TenantID: owner.TenantID, ConnectionID: "conn-a", AccountID: "account-a", ResourceID: "operation-a", Operation: "batch", Status: "completed", RequestID: "request-a", CreatedAt: now, UpdatedAt: now},
		{TenantID: owner.TenantID, ConnectionID: "conn-b", AccountID: "account-b", ResourceID: "operation-b", Operation: "batch", Status: "failed", RequestID: "request-b", CreatedAt: now, UpdatedAt: now},
		{TenantID: owner.TenantID, ConnectionID: "conn-c", AccountID: "account-c", ResourceID: "duplicate", Operation: "batch", Status: "job_pending", RequestID: "request-c", CreatedAt: now, UpdatedAt: now},
		{TenantID: owner.TenantID, ConnectionID: "conn-d", AccountID: "account-d", ResourceID: "duplicate", Operation: "batch", Status: "cancelled", RequestID: "request-d", CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.PutJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(ctx, owner, "upstream_operations", "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("expected paged upstream operation projection, got %+v, %v", page, err)
	}
	var data admin.UpstreamOperationData
	if err := json.Unmarshal(page.Items[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if page.Items[0].ID != data.ResourceID || page.Items[0].Version < 1 || data.Status == "" || data.Operation == "" {
		t.Fatalf("projection omitted action-compatible identity/state: %+v data=%+v", page.Items[0], data)
	}
	if _, err := s.Get(ctx, owner, "upstream_operations", "duplicate"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("ambiguous provider operation ID must fail closed, got %v", err)
	}
}
