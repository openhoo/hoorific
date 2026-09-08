package replicate

import (
	"context"
	"net/http"
	"testing"

	"hoorific/internal/core"
)

func TestPredictionCreateValidatesControlsAndActions(t *testing.T) {
	connector := New()
	var endpoint core.NativeEndpoint
	for _, candidate := range connector.Endpoints() {
		if candidate.Action == "predictions.create" {
			endpoint = candidate
			break
		}
	}
	if endpoint.Action == "" {
		t.Fatal("prediction create endpoint is missing")
	}
	binding, err := connector.BindEndpoint(context.Background(), core.Connection{}, endpoint, nil)
	if err != nil {
		t.Fatalf("bind prediction create: %v", err)
	}
	if binding.Response.ResultAction != "predictions.get" || binding.Response.CancelAction != "predictions.cancel" {
		t.Fatalf("prediction actions are not explicit: %#v", binding.Response)
	}
	valid := http.Header{"Prefer": []string{"wait=60"}, "Cancel-After": []string{"1h30m"}}
	if err := binding.ValidateRequestHeaders(valid); err != nil {
		t.Fatalf("valid Replicate controls rejected: %v", err)
	}
	invalid := http.Header{"Prefer": []string{"wait=61"}}
	if err := binding.ValidateRequestHeaders(invalid); err == nil {
		t.Fatal("out-of-range Prefer was accepted")
	}
	invalid = http.Header{"Cancel-After": []string{"4s"}}
	if err := binding.ValidateRequestHeaders(invalid); err == nil {
		t.Fatal("below-minimum Cancel-After was accepted")
	}
	invalid = http.Header{"Cancel-After": []string{"10s", "24h"}}
	if err := binding.ValidateRequestHeaders(invalid); err == nil {
		t.Fatal("conflicting duplicate Cancel-After values were accepted")
	}
	invalid = http.Header{"Prefer": []string{"wait=10", "wait=60"}}
	if err := binding.ValidateRequestHeaders(invalid); err == nil {
		t.Fatal("duplicate Prefer values were accepted")
	}
	invalid = http.Header{"Cancel-After": []string{"30s", "30s"}}
	if err := binding.ValidateRequestHeaders(invalid); err == nil {
		t.Fatal("duplicate equal Cancel-After values were accepted")
	}
	invalid = http.Header{"Prefer": []string{"wait=5", "wait=5"}}
	if err := binding.ValidateRequestHeaders(invalid); err == nil {
		t.Fatal("duplicate equal Prefer values were accepted")
	}
}
