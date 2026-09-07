package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/core"
	"hoorific/internal/credential"
)

func newTenancyAuthTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	key := filepath.Join(dir, "master.key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg core.BootstrapConfig
	cfg.SchemaVersion = 1
	cfg.Mode = "standalone"
	cfg.DataDir = dir
	cfg.Listeners.Inference = ":0"
	cfg.Listeners.Management = "127.0.0.1:0"
	cfg.Storage.SQLite.Path = filepath.Join(dir, "state.db")
	cfg.Encryption.KeyFile = key
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func bootstrapTenant(t *testing.T, s *Store, tenant, subject string) core.Principal {
	t.Helper()
	ctx := context.Background()
	code, err := s.CreateBootstrap(ctx, tenant, subject)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ConsumeBootstrap(ctx, hashBootstrapTestCode(code))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func hashBootstrapTestCode(code string) string {
	sum := sha256SumForTest(code)
	return sum
}

func sha256SumForTest(value string) string {
	// Keep the test on the public bootstrap API while matching the verifier
	// accepted by ConsumeBootstrap.
	h := sha256.New()
	_, _ = h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

func operatorMutation(t *testing.T, subject, issuer, identity string, enabled bool) core.Mutation {
	t.Helper()
	data, err := json.Marshal(admin.OperatorData{Subject: subject, Issuer: issuer, IdentitySubject: identity, Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	return core.Mutation{Kind: "operators", ID: subject, Data: data}
}

func TestBootstrapSubjectReservationAndProtectedCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)

	first, err := s.CreateBootstrap(ctx, "tenant-a", "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBootstrap(ctx, "tenant-a", "owner-a"); err != nil {
		t.Fatalf("same unclaimed bootstrap reservation should be reusable: %v", err)
	}
	if _, err = s.CreateBootstrap(ctx, "tenant-a", "other-owner"); err == nil {
		t.Fatal("different subject must not reuse a tenant bootstrap reservation")
	}
	if _, err = s.CreateBootstrap(ctx, "tenant-b", "owner-a"); err == nil {
		t.Fatal("bootstrap subject must not be reserved by two tenants")
	}

	ownerA, err := s.ConsumeBootstrap(ctx, hashBootstrapTestCode(first))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveMembership(ctx, ownerA.TenantID, ownerA.SubjectID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeBootstrap(ctx, hashBootstrapTestCode(first)); err == nil {
		t.Fatal("consumed bootstrap must not be reusable")
	}
	if _, err = s.CreateBootstrap(ctx, "tenant-a", "owner-a"); err == nil {
		t.Fatal("claimed tenant must not issue another bootstrap")
	}

	ownerB := bootstrapTenant(t, s, "tenant-b", "owner-b")
	if ownerA.SubjectID == ownerB.SubjectID {
		t.Fatal("test owners must remain distinct")
	}
	if err = s.CreateSession(ctx, admin.Session{Hash: "owner-b-session", Principal: ownerB, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SelectSessionTenant(ctx, "owner-b-session", ownerA.TenantID); err == nil {
		t.Fatal("operator must not select a tenant where it has no membership")
	}

	victimData, err := json.Marshal(admin.OperatorData{Subject: ownerA.SubjectID, Issuer: "https://attacker.example", IdentitySubject: "attacker", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, ownerB, core.Mutation{Kind: "operators", ID: ownerA.SubjectID, Data: victimData}); err == nil {
		t.Fatal("tenant owner must not modify an operator owned by another tenant")
	}
	if _, err = s.ResolveIdentity(ctx, "https://attacker.example", "attacker"); err == nil {
		t.Fatal("forged issuer and subject must not resolve")
	}
	if _, err = s.AvailableTenants(ctx, core.Principal{TenantID: ownerB.TenantID, SubjectID: ownerA.SubjectID}); err == nil {
		t.Fatal("forged subject must not enumerate victim tenants")
	}

	if _, err = s.Mutate(ctx, ownerA, core.Mutation{Kind: "operators", ID: ownerA.SubjectID, ExpectedVersion: 1, Delete: true}); err == nil {
		t.Fatal("bootstrap owner deletion must be rejected")
	}
	if _, err = s.Mutate(ctx, ownerA, core.Mutation{Kind: "operators", ID: ownerA.SubjectID, ExpectedVersion: 1, Data: victimData}); err == nil {
		t.Fatal("ordinary bootstrap owner update must be rejected")
	}
	if _, err = s.Mutate(ctx, ownerB, core.Mutation{Kind: "role_bindings", ID: ownerA.SubjectID, ExpectedVersion: 1, Delete: true}); err == nil {
		t.Fatal("protected bootstrap owner binding deletion must be rejected")
	}
	emptyOperator, err := json.Marshal(admin.OperatorData{Subject: "empty-operator", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, ownerB, core.Mutation{Kind: "operators", ID: "empty-operator", Data: emptyOperator}); err == nil {
		t.Fatal("arbitrary empty-provenance operator creation must be rejected")
	}
}

func TestExplicitIdentityEnrollmentAndTenantSelectionBoundaries(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant-a", "owner-a")
	tenantData, err := json.Marshal(admin.TenantData{Name: "tenant-b", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, owner, core.Mutation{Kind: "tenants", ID: "tenant-b", Data: tenantData}); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateSession(ctx, admin.Session{Hash: "multi-tenant-session", Principal: owner, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectSessionTenant(ctx, "multi-tenant-session", "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Principal.TenantID != "tenant-b" || selected.Principal.SubjectID != owner.SubjectID {
		t.Fatalf("unexpected selected tenant principal: %+v", selected.Principal)
	}
	memberships, err := s.AvailableTenants(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 2 || memberships[0].TenantID != "tenant-a" || memberships[1].TenantID != "tenant-b" {
		t.Fatalf("unexpected available tenants: %+v", memberships)
	}
	other := bootstrapTenant(t, s, "tenant-c", "owner-c")
	if other.TenantID == owner.TenantID {
		t.Fatal("unrelated bootstrap tenant unexpectedly reused tenant")
	}
	if _, err = s.SelectSessionTenant(ctx, "multi-tenant-session", other.TenantID); err == nil {
		t.Fatal("session must not select an unrelated tenant")
	}
	if _, err := s.Mutate(ctx, owner, operatorMutation(t, owner.SubjectID, "https://issuer.example", "owner-a-oidc", true)); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveIdentity(ctx, "https://issuer.example", "owner-a-oidc")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.TenantID != owner.TenantID || resolved.SubjectID != owner.SubjectID || resolved.Role != "owner" {
		t.Fatalf("unexpected enrolled principal: %+v", resolved)
	}
	if _, err = s.ResolveIdentity(ctx, "https://issuer.example", owner.SubjectID); err == nil {
		t.Fatal("issuer subject mismatch must not resolve")
	}
	if _, err = s.ResolveMembership(ctx, owner.TenantID, owner.SubjectID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SelectSessionTenant(ctx, "missing-session", owner.TenantID); err == nil {
		t.Fatal("missing session must remain unavailable")
	}
}
func TestCredentialMetadataProjectionAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant-a", "owner-a")
	other := bootstrapTenant(t, s, "tenant-b", "owner-b")
	keys, err := credential.NewKeyring("current", make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := credential.NewManager(keys, s)
	if err != nil {
		t.Fatal(err)
	}
	identity := credential.Identity{TenantID: owner.TenantID, ConnectionID: "connection-a", CredentialID: "credential-a", AccountID: "account-a", Provider: "provider-a"}
	if _, err = manager.Put(ctx, identity, credential.Secret{APIKey: &credential.APIKey{Value: "super-secret", Header: "x-api-key"}}); err != nil {
		t.Fatal(err)
	}

	page, err := s.List(ctx, owner, "credentials", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected one credential metadata item, got %d", len(page.Items))
	}
	var metadata admin.CredentialMetadataData
	if err = json.Unmarshal(page.Items[0].Data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ConnectionID != identity.ConnectionID || metadata.CredentialID != identity.CredentialID || metadata.Provider != identity.Provider || metadata.AccountID != identity.AccountID || metadata.Version != 1 {
		t.Fatalf("unexpected credential metadata: %+v", metadata)
	}
	public := string(page.Items[0].Data)
	for _, secretField := range []string{"super-secret", "ciphertext", "nonce", "key_id"} {
		if strings.Contains(public, secretField) {
			t.Fatalf("credential metadata exposed %q: %s", secretField, public)
		}
	}
	if _, err = s.Get(ctx, owner, "credentials", identity.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, other, "credentials", identity.ConnectionID); err == nil {
		t.Fatal("wrong tenant must not retrieve credential metadata")
	}
	if _, err = s.Mutate(ctx, owner, core.Mutation{Kind: "credentials", ID: identity.ConnectionID, Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("generic credential mutation must be rejected")
	}
}

func TestRevocationInvalidatesActiveSessionAndToken(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant-a", "owner-a")
	operator := "operator-a"
	if _, err := s.Mutate(ctx, owner, operatorMutation(t, operator, "https://issuer.example", "operator-a-oidc", true)); err != nil {
		t.Fatal(err)
	}
	member, err := s.ResolveMembership(ctx, owner.TenantID, operator)
	if err != nil {
		t.Fatal(err)
	}
	sessionHash := "active-session"
	if err = s.CreateSession(ctx, admin.Session{Hash: sessionHash, Principal: member, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	token, err := s.IssueAdminToken(ctx, owner, operator, []string{"catalog:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, sessionHash); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveAdminToken(ctx, sha256SumForTest(token)); err != nil {
		t.Fatal(err)
	}

	if _, err = s.Mutate(ctx, owner, operatorMutation(t, operator, "https://issuer.example", "operator-a-oidc", false)); err == nil {
		t.Fatal("operator revocation must not be hidden by stale version")
	}
	if _, err = s.Mutate(ctx, owner, core.Mutation{Kind: "operators", ID: operator, ExpectedVersion: 1, Data: mustOperatorJSON(t, operator, "https://issuer.example", "operator-a-oidc", false)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, sessionHash); err == nil {
		t.Fatal("revoked operator session must be rejected")
	}
	if _, err = s.ResolveAdminToken(ctx, sha256SumForTest(token)); err == nil {
		t.Fatal("revoked operator token must be rejected")
	}
}

func mustOperatorJSON(t *testing.T, subject, issuer, identity string, enabled bool) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(admin.OperatorData{Subject: subject, Issuer: issuer, IdentitySubject: identity, Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
