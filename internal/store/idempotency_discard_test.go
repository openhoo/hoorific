package store

import (
	"context"
	"testing"

	"hoorific/internal/core"
)

// TestDiscardIdempotencyFreesPendingClaim pins that a pre-intent rejection can
// free its pending claim for an immediate retry without touching rows that
// already reached a terminal state.
func TestDiscardIdempotencyFreesPendingClaim(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)

	req := idempotencyTestRequest("tenant-a", "subject-a", "discard-key", "discard-fingerprint", "discard-owner")
	if record, err := s.BeginIdempotency(ctx, req); err != nil || record.State != "new" {
		t.Fatalf("claim = %#v, %v", record, err)
	}
	if err := s.DiscardIdempotency(ctx, req); err != nil {
		t.Fatalf("discard failed: %v", err)
	}
	retry := idempotencyTestRequest("tenant-a", "subject-a", "discard-key", "discard-fingerprint", "discard-owner-2")
	if record, err := s.BeginIdempotency(ctx, retry); err != nil || record.State != "new" {
		t.Fatalf("freed key did not allow a fresh claim: %#v, %v", record, err)
	}

	// A fenced owner cannot discard someone else's live claim.
	foreign := req
	foreign.OwnerID = "wrong-owner"
	if err := s.DiscardIdempotency(ctx, foreign); err != nil {
		t.Fatalf("foreign discard errored: %v", err)
	}
	if record, err := s.BeginIdempotency(ctx, idempotencyTestRequest("tenant-a", "subject-a", "discard-key", "discard-fingerprint", "discard-owner-3")); err != nil || record.State != "pending" {
		t.Fatalf("foreign discard removed a live claim: %#v, %v", record, err)
	}

	// Terminal rows are never discarded.
	done := idempotencyTestRequest("tenant-a", "subject-a", "discard-done", "discard-done-fingerprint", "discard-done-owner")
	if record, err := s.BeginIdempotency(ctx, done); err != nil || record.State != "new" {
		t.Fatalf("done claim = %#v, %v", record, err)
	}
	if err := s.FinishIdempotency(ctx, done, &core.IdempotencyResponse{Status: 200, Body: []byte("ok")}); err != nil {
		t.Fatal(err)
	}
	if err := s.DiscardIdempotency(ctx, done); err != nil {
		t.Fatalf("complete discard errored: %v", err)
	}
	record, err := s.BeginIdempotency(ctx, idempotencyTestRequest("tenant-a", "subject-a", "discard-done", "discard-done-fingerprint", "discard-done-owner"))
	if err != nil || record.State != "complete" || record.Response == nil {
		t.Fatalf("complete row was discarded: %#v, %v", record, err)
	}
}
