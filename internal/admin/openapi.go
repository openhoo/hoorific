package admin

import "encoding/json"

// OpenAPISpec returns the OpenAPI document produced by the registered Huma operations.
// It imports no application composition or embedded console assets.
func OpenAPISpec() map[string]any {
	api := buildHumaAPI()
	b, e := json.Marshal(api.OpenAPI())
	if e != nil {
		return map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Hoorific administrative API", "version": "1.0.0"}}
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return map[string]any{}
	}
	return out
}
