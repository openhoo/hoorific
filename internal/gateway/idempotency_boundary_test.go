package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	"hoorific/internal/core"
)

// TestIdempotencyReplayPreservesResponseBoundary pins that the replay path
// restores the sanitized response boundary exactly as captured. The replay
// writer clears every header before restoring the cached set, so boundary
// headers captured from a sanitized relay must survive, while credential
// headers must never reappear.
func TestIdempotencyReplayPreservesResponseBoundary(t *testing.T) {
	capture := newIdempotencyCaptureWriter(httptest.NewRecorder())
	capture.Header().Set("Content-Type", "text/plain; charset=utf-8")
	capture.Header().Set("X-Content-Type-Options", "nosniff")
	capture.Header().Set("Content-Security-Policy", "sandbox")
	capture.Header().Set("X-Upstream-Custom", "yes")
	capture.Header().Set("Authorization", "Bearer upstream-secret")
	capture.WriteHeader(200)
	if _, err := capture.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("capture write failed: %v", err)
	}
	snapshot, ok := capture.snapshot()
	if !ok {
		t.Fatal("capture did not produce a replayable snapshot")
	}

	replay := httptest.NewRecorder()
	if err := writeIdempotencyReplay(replay, &snapshot); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if got := replay.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("replay lost neutralized content type: %q", got)
	}
	if got := replay.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("replay lost nosniff boundary: %q", got)
	}
	if got := replay.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("replay lost sandboxing CSP: %q", got)
	}
	if got := replay.Header().Get("X-Upstream-Custom"); got != "yes" {
		t.Errorf("replay dropped ordinary upstream header: %q", got)
	}
	if got := replay.Header().Get("Authorization"); got != "" {
		t.Errorf("replay restored credential header: %q", got)
	}
	if got := replay.Body.String(); got != `{"ok":true}` {
		t.Errorf("replay body mismatch: %q", got)
	}
}

type fakeIdempotencyStore struct {
	finishCalls    int
	discardCalls   int
	finishResponse *core.IdempotencyResponse
}

func (s *fakeIdempotencyStore) BeginIdempotency(context.Context, core.IdempotencyRequest) (core.IdempotencyRecord, error) {
	return core.IdempotencyRecord{State: "new"}, nil
}

func (s *fakeIdempotencyStore) FinishIdempotency(_ context.Context, _ core.IdempotencyRequest, response *core.IdempotencyResponse) error {
	s.finishCalls++
	s.finishResponse = response
	return nil
}

func (s *fakeIdempotencyStore) DiscardIdempotency(context.Context, core.IdempotencyRequest) error {
	s.discardCalls++
	return nil
}

// TestIdempotencyFinishDiscardsPreIntentRejection pins that a request rejected
// before any upstream intent frees its key instead of leaving a permanent
// unreplayable tombstone, while unmarked and unknown outcomes keep tombstoning.
func TestIdempotencyFinishDiscardsPreIntentRejection(t *testing.T) {
	store := &fakeIdempotencyStore{}
	execution := &idempotencyExecution{store: store, req: core.IdempotencyRequest{Key: "k"}, state: &idempotencyState{}, write: newIdempotencyCaptureWriter(httptest.NewRecorder())}
	execution.state.mark("not_dispatched")
	execution.finish(context.Background())
	if store.discardCalls != 1 || store.finishCalls != 0 {
		t.Fatalf("pre-intent rejection: discard=%d finish=%d", store.discardCalls, store.finishCalls)
	}

	unmarked := &idempotencyExecution{store: store, req: core.IdempotencyRequest{Key: "k2"}, state: &idempotencyState{}, write: newIdempotencyCaptureWriter(httptest.NewRecorder())}
	unmarked.finish(context.Background())
	if store.finishCalls != 1 || store.finishResponse != nil || store.discardCalls != 1 {
		t.Fatalf("unmarked outcome must keep tombstone: finish=%d response=%v discard=%d", store.finishCalls, store.finishResponse, store.discardCalls)
	}

	unknown := &idempotencyExecution{store: store, req: core.IdempotencyRequest{Key: "k3"}, state: &idempotencyState{}, write: newIdempotencyCaptureWriter(httptest.NewRecorder())}
	unknown.state.mark("unknown")
	unknown.finish(context.Background())
	if store.finishCalls != 2 || store.finishResponse != nil || store.discardCalls != 1 {
		t.Fatalf("unknown outcome must keep tombstone: finish=%d response=%v discard=%d", store.finishCalls, store.finishResponse, store.discardCalls)
	}

	terminal := &idempotencyExecution{store: store, req: core.IdempotencyRequest{Key: "k4"}, state: &idempotencyState{}, write: newIdempotencyCaptureWriter(httptest.NewRecorder())}
	terminal.state.mark("not_dispatched")
	terminal.state.mark("terminal")
	terminal.write.WriteHeader(200)
	if _, err := terminal.write.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("terminal capture write failed: %v", err)
	}
	terminal.finish(context.Background())
	if store.finishCalls != 3 || store.finishResponse == nil || store.finishResponse.Status != 200 || store.discardCalls != 1 {
		t.Fatalf("terminal outcome must record replay: finish=%d response=%v discard=%d", store.finishCalls, store.finishResponse, store.discardCalls)
	}
}
