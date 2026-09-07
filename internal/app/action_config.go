package app

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"strings"
)

func (a *Actions) configAction(ctx context.Context, p core.Principal, action string, version int64, data json.RawMessage) (json.RawMessage, error) {
	if action == "export" {
		return a.deps.Store.Execute(ctx, p, "config", "", "export", version, json.RawMessage(`{}`))
	}
	if action != "diff" && action != "apply" {
		return nil, actionError("unsupported_operation", 404, "Configuration action is not supported")
	}
	var in struct {
		ExpectedRevision int64           `json:"expected_revision"`
		Config           json.RawMessage `json:"config"`
		Prune            bool            `json:"prune"`
	}
	if len(data) == 0 || json.Unmarshal(data, &in) != nil || len(in.Config) == 0 {
		return nil, actionError("invalid_configuration", 400, "Configuration candidate is required")
	}
	if containsConfigSecret(in.Config) {
		return nil, actionError("invalid_configuration", 400, "Configuration may contain references but never secret values")
	}
	if action == "apply" && in.ExpectedRevision == 0 && version > 0 {
		in.ExpectedRevision = version
	}
	b, e := json.Marshal(in)
	if e != nil {
		return nil, e
	}
	return a.deps.Store.Execute(ctx, p, "config", "", action, version, b)
}

func containsConfigSecret(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return true
	}
	var walk func(any) bool
	walk = func(x any) bool {
		switch y := x.(type) {
		case map[string]any:
			for k, v := range y {
				n := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "-", ""), "_", ""))
				switch n {
				case "secret", "password", "privatekey", "accesstoken", "refreshtoken", "clientsecret", "apikey":
					return true
				}
				if walk(v) {
					return true
				}
			}
		case []any:
			for _, v := range y {
				if walk(v) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}
