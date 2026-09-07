package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"hoorific/internal/core"
)

func TestAdministrativeProblemPreservesCodeWithoutLeakingCause(t *testing.T) {
	tests := []struct {
		name   string
		cause  error
		status int
		code   string
	}{
		{"credential rejection", core.GatewayError{Code: "invalid_credential", HTTPStatus: 400, Message: "Invalid credential"}, 400, "invalid_credential"},
		{"stale version", core.GatewayError{Code: "version_conflict", HTTPStatus: 409, Message: "Credential changed"}, 409, "version_conflict"},
		{"unsafe gateway failure", core.GatewayError{Code: "upstream_failed", HTTPStatus: 502, Message: "secret-provider-payload"}, 502, "upstream_failed"},
		{"unsafe generic failure", errors.New("secret-provider-payload"), 500, ""},
		{"typed service failure", huma.NewError(503, "secret-provider-payload"), 503, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			api := humago.New(mux, huma.DefaultConfig("problem regression", "1"))
			huma.Register(api, huma.Operation{OperationID: "problem", Method: "GET", Path: "/problem"}, func(context.Context, *struct{}) (*struct{}, error) { return nil, runtimeRepositoryError(test.cause) })
			documentProblemCodes(api)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest("GET", "/problem", nil))
			if recorder.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.status, recorder.Body.String())
			}
			var body struct {
				Code   string `json:"code"`
				Status int    `json:"status"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != test.code || body.Status != test.status {
				t.Fatalf("problem=%+v", body)
			}
			if strings.Contains(recorder.Body.String(), "secret-provider-payload") {
				t.Fatal("problem response disclosed private cause")
			}
		})
	}
}
