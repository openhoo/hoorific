package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOperationalLedgerProjectionIsolationAndCursor(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	ownerA := bootstrapTenant(t, s, "tenant-a", "owner-a")
	ownerB := bootstrapTenant(t, s, "tenant-b", "owner-b")
	now := time.Now().Unix()
	for _, row := range []struct {
		effect, attempt string
		amount          int64
	}{
		{"effect-a", "attempt-a", 7},
		{"effect-b", "attempt-b", 8},
	} {
		payload, err := json.Marshal(map[string]any{
			"TenantID": ownerA.TenantID, "AttemptID": row.attempt,
			"ActualCost": row.amount, "Usage": map[string]any{"Input": int64(1), "Output": int64(2), "Total": int64(3), "Source": "provider"},
			"secret": "must-not-be-projected",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.ExecContext(ctx, s.Query("INSERT INTO usage_ledger(tenant_id,effect_id,attempt_id,effect_kind,amount,data,created_at) VALUES(?,?,?,?,?,?,?)"), ownerA.TenantID, row.effect, row.attempt, "charge", row.amount, string(payload), now); err != nil {
			t.Fatal(err)
		}
	}

	page, err := s.List(ctx, ownerA, "usage_ledger", "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("expected bounded operational page and cursor, got %+v, %v", page, err)
	}
	if strings.Contains(string(page.Items[0].Data), "must-not-be-projected") {
		t.Fatal("operational projection leaked an unallowlisted payload field")
	}
	if _, err = s.List(ctx, ownerB, "usage_ledger", page.NextCursor, 1); err == nil {
		t.Fatal("cursor signed for one tenant must not work for another tenant")
	}
	second, err := s.List(ctx, ownerA, "usage_ledger", page.NextCursor, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "effect-b" {
		t.Fatalf("expected next operational page, got %+v, %v", second, err)
	}
	if _, err = s.Get(ctx, ownerB, "usage_ledger", "effect-a"); err == nil {
		t.Fatal("cross-tenant operational get must not expose a row")
	}
}

func TestOperationalLedgerMalformedRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant-a", "owner-a")
	if _, err := s.DB.ExecContext(ctx, s.Query("INSERT INTO usage_ledger(tenant_id,effect_id,attempt_id,effect_kind,amount,data,created_at) VALUES(?,?,?,?,?,?,?)"), owner.TenantID, "bad-effect", "bad-attempt", "charge", int64(1), `{"TenantID":"tenant-a","AttemptID":`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	_, err := s.List(ctx, owner, "usage_ledger", "", 10)
	if err == nil || !strings.Contains(err.Error(), "invalid operational record") {
		t.Fatalf("malformed stored record must fail safely, got %v", err)
	}
	if strings.Contains(err.Error(), "TenantID") || strings.Contains(err.Error(), "secret") {
		t.Fatal("malformed-record error exposed stored payload")
	}
}
