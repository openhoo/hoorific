package cohere

import (
	"context"
	"testing"

	"hoorific/internal/core"
)

func TestEmbedJobCancellationUsesBodyAndExplicitReceipt(t *testing.T) {
	connector := New()
	var create, cancel core.NativeEndpoint
	for _, endpoint := range connector.Endpoints() {
		switch endpoint.Action {
		case "embed_jobs.create":
			create = endpoint
		case "embed_jobs.cancel":
			cancel = endpoint
		}
	}
	if create.Action == "" || cancel.Action == "" {
		t.Fatal("embed-job create/cancel endpoints are missing")
	}
	createBinding, err := connector.BindEndpoint(context.Background(), core.Connection{}, create, nil)
	if err != nil {
		t.Fatalf("bind embed-job create: %v", err)
	}
	if createBinding.Response.ResultAction != "embed_jobs.retrieve" || createBinding.Response.CancelAction != "embed_jobs.cancel" {
		t.Fatalf("embed-job actions are not explicit: %#v", createBinding.Response)
	}
	cancelBinding, err := connector.BindEndpoint(context.Background(), core.Connection{}, cancel, map[string]string{"id": "job-123"})
	if err != nil {
		t.Fatalf("bind embed-job cancel: %v", err)
	}
	if cancelBinding.Method != "POST" || cancelBinding.ModelLocation != "none" || cancelBinding.DefaultBody != "{}" || !cancelBinding.CancellationSupported {
		t.Fatalf("cancel binding does not preserve Cohere request semantics: %#v", cancelBinding)
	}
	if cancelBinding.Response.StatusField != "" {
		t.Fatalf("empty JSON cancellation receipt unexpectedly requires a status field: %#v", cancelBinding.Response)
	}
}
