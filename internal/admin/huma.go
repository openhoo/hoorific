package admin

import (
	"context"
	"errors"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"net/http"
	"reflect"
)

type humaResourceInput struct {
	ID   string `path:"id"`
	Kind string `path:"kind"`
	Body resourceInput
}
type humaResourcePatch struct {
	ID      string `path:"id"`
	Body    resourceInput
	IfMatch string `header:"If-Match"`
}
type humaResourceOutput struct{ Body coreResource }
type coreResource struct {
	ID       string         `json:"id"`
	TenantID string         `json:"tenant_id"`
	Kind     string         `json:"kind"`
	Version  int64          `json:"version"`
	Data     map[string]any `json:"data"`
}
type humaPageOutput struct {
	Items      []coreResource `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}
type humaActionInput struct {
	ID   string `path:"id"`
	Body map[string]any
}
type humaActionOutput struct{ Body map[string]any }
type humaAuthInput struct {
	Code string `json:"code"`
}
type humaConfigInput struct {
	ExpectedRevision int64          `json:"expected_revision"`
	Config           map[string]any `json:"config,omitempty"`
	Prune            bool           `json:"prune,omitempty"`
}

func unavailable() error { return errors.New("authenticated admin facade owns this operation") }
func buildHumaAPI() huma.API {
	api := humago.New(http.NewServeMux(), huma.DefaultConfig("Hoorific administrative API", "1.0.0"))
	registerTypedSchemas(api)
	registerHumaAuxSchema(api)
	dummy := &Server{}
	dummy.registerHumaAuth(api)
	dummy.registerHumaRuntime(api)
	dummy.registerHumaActions(api)
	dummy.registerAdditionalHumaActions(api)
	dummy.registerHumaReconcile(api)
	documentProblemCodes(api)
	return api
}
func registerTypedSchemas(api huma.API) {
	r := api.OpenAPI().Components.Schemas
	for _, t := range []reflect.Type{reflect.TypeOf(TenantData{}), reflect.TypeOf(OperatorData{}), reflect.TypeOf(RoleBindingData{}), reflect.TypeOf(ConnectionData{}), reflect.TypeOf(CredentialMetadataData{}), reflect.TypeOf(APIKeyGrantData{}), reflect.TypeOf(APIKeyMetadataData{}), reflect.TypeOf(APIKeyIssuedData{}), reflect.TypeOf(ModelData{}), reflect.TypeOf(AliasData{}), reflect.TypeOf(RoutePolicyData{}), reflect.TypeOf(PolicyLimitData{}), reflect.TypeOf(UsageLedgerData{}), reflect.TypeOf(AuditEventData{}), reflect.TypeOf(UpstreamOperationData{}), reflect.TypeOf(AdmissionData{}), reflect.TypeOf(ReconciliationData{}), reflect.TypeOf(AccountPoolData{}), reflect.TypeOf(OAuthSessionData{})} {
		huma.SchemaFromType(r, t)
	}
}
func registerHumaAuxSchema(api huma.API) {
	for _, x := range []struct{ path, method, id string }{{"/admin/api/v1/auth/bootstrap", "POST", "bootstrap"}, {"/admin/api/v1/auth/login", "GET", "login"}, {"/admin/api/v1/auth/callback", "GET", "callback"}, {"/admin/api/v1/auth/logout", "POST", "logout"}, {"/admin/api/v1/config/export", "GET", "exportConfig"}, {"/admin/api/v1/config/diff", "POST", "diffConfig"}, {"/admin/api/v1/config/apply", "POST", "applyConfig"}} {
		huma.Register(api, huma.Operation{OperationID: x.id, Method: x.method, Path: x.path}, func(context.Context, *humaAuthInput) (*humaActionOutput, error) { return nil, unavailable() })
	}
}
