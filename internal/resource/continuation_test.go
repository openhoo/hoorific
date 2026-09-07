package resource_test

import (
	"bytes"
	"testing"
	"time"

	"hoorific/internal/resource"
)

func TestContinuationRejectsChangedDispatchMetadata(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	now := time.Now()
	c := resource.Continuation{ID: "opaque", TenantID: "tenant", ConnectionID: "connection", AccountID: "account", UpstreamURL: "https://provider.example/session?token=opaque", ExpiresAt: now.Add(time.Hour), Metadata: []byte(`{"method":"GET"}`)}
	expected := c.UpstreamURL
	if err := resource.SealContinuation(key, &c, []string{"https://provider.example"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := resource.OpenContinuation(key, c, now)
	if err != nil || got != expected {
		t.Fatalf("valid continuation failed: %v", err)
	}
	c.Metadata = []byte(`{"method":"DELETE"}`)
	if _, _, err = resource.OpenContinuation(key, c, now); err == nil {
		t.Fatal("altered dispatch metadata retained valid authorization")
	}
}
