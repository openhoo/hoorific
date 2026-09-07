// Package codexsubscription binds the opt-in Codex subscription Responses transport.
package codexsubscription

import (
	"context"
	"hoorific/internal/core"
	"hoorific/internal/provider/subscription"
)

type Connector struct{ *subscription.Connector }

func New() *Connector {
	return &Connector{subscription.New("codex-subscription", []subscription.Route{{Endpoint: core.NativeEndpoint{Method: "POST", Path: "backend-api/codex/responses", Action: "responses", Operation: "generate", ModelLocation: "body", Framing: "sse-data"}, Protocol: "openai-responses", Variant: "codex-subscription"}})}
}
func (c *Connector) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	return c.Connector.Bind(ctx, t, o)
}

var _ core.Connector = (*Connector)(nil)
