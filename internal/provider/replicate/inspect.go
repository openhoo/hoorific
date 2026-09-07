package replicate

import (
	"context"
	"encoding/json"

	"hoorific/internal/core"
)

// Inspect enforces only the Replicate model/version boundary. It intentionally
// does not validate, normalize, or rewrite the provider's native input object.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	call, ok := target.(core.ModelCall)
	if !ok {
		return nil
	}
	if op != PredictionCreate || !approved(call, op) {
		return invalid("Model operation is not approved")
	}
	if len(body) == 0 {
		return invalid("Replicate prediction body is required")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return invalid("Replicate prediction body must be a JSON object")
	}
	rawVersion := fields["version"]
	_, _, selected, err := modelParts(call.Model.ID)
	if err != nil {
		if !validVersion(call.Model.ID) {
			return err
		}
		selected = call.Model.ID
	}
	if selected != "" {
		if len(rawVersion) == 0 {
			return invalid("Version is required for a version-specific Replicate model")
		}
		var supplied string
		if json.Unmarshal(rawVersion, &supplied) != nil || supplied != selected {
			return invalid("Prediction version does not match the selected model version")
		}
	} else if len(rawVersion) > 0 {
		var supplied string
		if json.Unmarshal(rawVersion, &supplied) != nil || !validVersion(supplied) {
			return invalid("Replicate prediction version must be a 64-character hexadecimal ID")
		}
	}
	return nil
}
