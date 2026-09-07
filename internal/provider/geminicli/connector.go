// Package geminicli implements the authenticated Google Code Assist transport
// used by the official Gemini CLI. It is not the Gemini API-key connector.
package geminicli

import (
	"context"
	"hoorific/internal/core"
	"hoorific/internal/provider/subscription"
)

type Connector struct{ *subscription.Connector }

func New() *Connector {
	return &Connector{subscription.New("gemini-cli", []subscription.Route{{Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:generateContent", Action: "codeassist.generateContent", Operation: "generate", ModelLocation: "body", Framing: "json"}, Protocol: "gemini-content", Variant: "code-assist"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:streamGenerateContent?alt=sse", Action: "codeassist.streamGenerateContent", Operation: "generate", ModelLocation: "body", Framing: "sse-data"}, Protocol: "gemini-content", Variant: "code-assist"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:countTokens", Action: "codeassist.countTokens", Operation: "count_tokens", ModelLocation: "body", Framing: "json"}, Protocol: "gemini-content", Variant: "code-assist"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:loadCodeAssist", Action: "codeassist.loadCodeAssist", Operation: "native", ModelLocation: "none", Framing: "json"}, Protocol: "native", Variant: "code-assist"}, {Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1internal:retrieveUserQuota", Action: "codeassist.retrieveUserQuota", Operation: "native", ModelLocation: "none", Framing: "json"}, Protocol: "native", Variant: "code-assist"}})}
}
func (c *Connector) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	return c.Connector.Bind(ctx, t, o)
}

var _ core.Connector = (*Connector)(nil)
var _ core.StreamBinder = (*Connector)(nil)
