package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"hoorific/internal/core"
)

type duplexConnectorStub struct {
	calls int
}

func (s *duplexConnectorStub) Descriptor() core.ConnectorDescriptor {
	return core.ConnectorDescriptor{ID: "duplex-test"}
}

func (*duplexConnectorStub) Bind(context.Context, core.Target, core.Operation) (core.Binding, error) {
	return core.Binding{}, nil
}

func (s *duplexConnectorStub) ExecuteDuplex(context.Context, core.Connection, string, core.CredentialLease, *http.Client, <-chan []byte, func([]byte) error) error {
	s.calls++
	return nil
}

type duplexLeaseStub struct{}

func (duplexLeaseStub) Authorize(context.Context, *http.Request) error { return nil }

func TestExecuteDuplexRejectsFirstFrameErrorsBeforeExecutor(t *testing.T) {
	for _, body := range []string{"", "partial-eventstream-frame"} {
		t.Run(body, func(t *testing.T) {
			connector := &duplexConnectorStub{}
			request := httptest.NewRequest(http.MethodPost, "http://gateway", bytes.NewBufferString(body))
			request.Header.Set("Content-Type", "application/vnd.amazon.eventstream")
			gateway := &Gateway{}
			outcome, err := gateway.executeDuplex(
				context.Background(),
				httptest.NewRecorder(),
				request,
				core.Principal{Realtime: true},
				selected{connection: core.Connection{ID: "connection"}, model: core.Model{ID: "model"}, connector: connector},
				core.Binding{Framing: "h2-eventstream-duplex"},
				core.AttemptPlan{},
				core.AttemptPermit{TenantID: "tenant", RequestID: "request", AttemptID: "attempt"},
				duplexLeaseStub{},
				&http.Client{},
				"request",
			)
			if err == nil {
				t.Fatal("malformed first frame unexpectedly executed")
			}
			if outcome.State != "not_executed" {
				t.Fatalf("outcome state = %q, want not_executed", outcome.State)
			}
			if connector.calls != 0 {
				t.Fatalf("duplex executor called %d times, want 0", connector.calls)
			}
		})
	}
}

func TestRealtimePreDispatchValidationIsNotExecuted(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://gateway", nil)
	gateway := &Gateway{}
	outcome, err := gateway.executeWebRTC(context.Background(), httptest.NewRecorder(), request, core.Principal{}, selected{}, core.Binding{}, core.AttemptPlan{}, core.AttemptPermit{AttemptID: "attempt"}, nil, nil, "request")
	if err == nil {
		t.Fatal("invalid WebRTC setup unexpectedly succeeded")
	}
	if outcome.State != "not_executed" {
		t.Fatalf("WebRTC outcome state = %q, want not_executed", outcome.State)
	}
}
