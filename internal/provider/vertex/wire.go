package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"hoorific/internal/core"
)

const vertexAnthropicVersion = "vertex-2023-10-16"

// AdaptRequest translates the portable Anthropic Messages body to Vertex's
// rawPredict contract. Vertex owns the model in the URL, while the body needs
// the Vertex-specific Anthropic version marker.
func (*Connector) AdaptRequest(ctx context.Context, _ core.Target, binding core.Binding, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.Codec.Protocol != core.Protocol("anthropic-messages") {
		return body, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("vertex anthropic request must be a JSON object")
	}
	if _, ok := object["anthropic_version"]; !ok {
		object["anthropic_version"] = json.RawMessage(`"` + vertexAnthropicVersion + `"`)
	}
	delete(object, "model")
	adapted, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return adapted, nil
}

// AdaptResponse leaves Vertex's rawPredict Anthropic response in the standard
// Messages shape. Native provider routes bypass wire adapters in the gateway.
func (*Connector) AdaptResponse(ctx context.Context, _ core.Binding, _ *http.Response) error {
	return ctx.Err()
}

var _ core.WireAdapter = (*Connector)(nil)
