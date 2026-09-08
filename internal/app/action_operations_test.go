package app

import (
	"encoding/json"
	"testing"

	"hoorific/internal/core"
)

func TestCancellationReceiptUsesExplicitResponsePolicy(t *testing.T) {
	if got := cancellationJobStatus("job_pending", core.NativeResponsePolicy{}, json.RawMessage(`{}`)); got != "cancelled" {
		t.Fatalf("empty JSON cancellation receipt status = %q, want cancelled", got)
	}
	if got := cancellationJobStatus("job_pending", core.NativeResponsePolicy{StatusField: "status"}, json.RawMessage(`{}`)); got != "job_pending" {
		t.Fatalf("status-less cancellation without empty-receipt policy = %q, want job_pending", got)
	}
	if got := cancellationJobStatus("job_pending", core.NativeResponsePolicy{StatusField: "status"}, json.RawMessage(`{"status":"CANCELLED"}`)); got != "cancelled" {
		t.Fatalf("explicit cancellation status = %q, want cancelled", got)
	}
	if got := cancellationJobStatus("succeeded", core.NativeResponsePolicy{}, json.RawMessage(`{}`)); got != "succeeded" {
		t.Fatalf("terminal job status was overwritten by cancellation receipt: %q", got)
	}
}
