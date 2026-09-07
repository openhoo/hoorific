// Package kimisubscription binds the separately authenticated Kimi Code
// subscription transports. Kimi Code is distinct from the public Moonshot
// platform: its device OAuth account uses the coding service endpoints and
// must not be represented as an API-key connector.
//
// Source contracts:
//   - https://www.kimi.com/code/docs/en/kimi-code/error-reference.html
//   - https://www.kimi.com/code/docs/en/kimi-code-cli/configuration/providers.html
//   - https://github.com/MoonshotAI/kimi-cli/blob/main/src/kimi_cli/auth/oauth.py
package kimisubscription

import (
	"context"

	"hoorific/internal/core"
	"hoorific/internal/provider/subscription"
)

const (
	// Kimi Code publishes separate SDK bases for the two supported wire
	// dialects. They are deliberately not inferred by the connector.
	OpenAIBaseURL           = "https://api.kimi.com/coding/v1"
	AnthropicBaseURL        = "https://api.kimi.com/coding/"
	OAuthHost               = "https://auth.kimi.com"
	DeviceAuthorizationPath = "/api/oauth/device_authorization"
	TokenPath               = "/api/oauth/token"
)

type Connector struct{ *subscription.Connector }

func New() *Connector {
	return &Connector{subscription.New("kimi-subscription", []subscription.Route{
		{
			Endpoint: core.NativeEndpoint{
				Method: "POST", Path: "chat/completions", Action: "chat.completions",
				Operation: "generate", ModelLocation: "body", Framing: "sse-data",
			},
			Protocol: core.Protocol("openai-chat"), Variant: "kimi-code",
		},
		{
			Endpoint: core.NativeEndpoint{
				// Anthropic SDKs append /v1/messages to this configured base.
				Method: "POST", Path: "v1/messages", Action: "messages.create",
				Operation: "generate", ModelLocation: "body", Framing: "sse-named",
			},
			Protocol: core.Protocol("anthropic-messages"), Variant: "kimi-code",
		},
	})}
}

func (c *Connector) Bind(ctx context.Context, target core.Target, operation core.Operation) (core.Binding, error) {
	return c.Connector.Bind(ctx, target, operation)
}

var _ core.Connector = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
