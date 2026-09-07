// Package claudesubscription binds the separately authenticated Claude subscription Messages transport.
package claudesubscription

import (
	"context"
	"hoorific/internal/core"
	"hoorific/internal/provider/subscription"
)

type Connector struct{ *subscription.Connector }

func New() *Connector {
	return &Connector{subscription.New("claude-subscription", []subscription.Route{{Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1/messages", Action: "messages.create", Operation: "generate", ModelLocation: "body", Framing: "sse-named"}, Protocol: "anthropic-messages", Variant: "claude-subscription"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1/messages/count_tokens", Action: "messages.count_tokens", Operation: "count_tokens", ModelLocation: "body", Framing: "json"}, Protocol: "anthropic-messages", Variant: "claude-subscription"}})}
}
func (c *Connector) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	return c.Connector.Bind(ctx, t, o)
}

var _ core.Connector = (*Connector)(nil)
