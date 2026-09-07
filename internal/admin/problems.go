package admin

import (
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"hoorific/internal/core"
)

// codedProblem extends the standard problem response without copying the
// wrapped cause, which may carry provider payloads or credential material.
type codedProblem struct {
	huma.ErrorModel
	Code string `json:"code,omitempty"`
}

func gatewayProblem(problem core.GatewayError) huma.StatusError {
	detail := problem.Message
	if problem.HTTPStatus >= 500 {
		detail = "Administrative operation failed"
	}
	return &codedProblem{ErrorModel: huma.ErrorModel{Status: problem.HTTPStatus, Title: http.StatusText(problem.HTTPStatus), Detail: detail}, Code: problem.Code}
}

// Huma v2's ErrorModel has no extension map. The wire extension above is an
// optional field on the standard ErrorModel response schema used by all routes.
func documentProblemCodes(api huma.API) {
	registry := api.OpenAPI().Components.Schemas
	schema := registry.Schema(reflect.TypeOf(huma.ErrorModel{}), true, "ErrorModel")
	if schema.Ref != "" {
		schema = registry.SchemaFromRef(schema.Ref)
	}
	if schema.Properties == nil {
		schema.Properties = map[string]*huma.Schema{}
	}
	schema.Properties["code"] = &huma.Schema{Type: "string", Description: "Stable administrative error code, when the failure has a classified gateway or repository cause."}
}
