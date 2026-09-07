// Package antigravity binds the separately authenticated Antigravity Code Assist transport.
package antigravity

import (
	"context"
	"hoorific/internal/core"
	"hoorific/internal/provider/subscription"
)

type Connector struct{ *subscription.Connector }

func New() *Connector {
	return &Connector{subscription.New("antigravity", []subscription.Route{{Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:generateContent", Action: "antigravity.generateContent", Operation: "generate", ModelLocation: "body", Framing: "json"}, Protocol: "gemini-content", Variant: "antigravity"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:streamGenerateContent?alt=sse", Action: "antigravity.streamGenerateContent", Operation: "generate", ModelLocation: "body", Framing: "sse-data"}, Protocol: "gemini-content", Variant: "antigravity"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:countTokens", Action: "antigravity.countTokens", Operation: "count_tokens", ModelLocation: "body", Framing: "json"}, Protocol: "gemini-content", Variant: "antigravity"}})}
}
func (c *Connector) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	return c.Connector.Bind(ctx, t, o)
}

var _ core.Connector = (*Connector)(nil)
var _ core.StreamBinder = (*Connector)(nil)
