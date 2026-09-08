// Package claudesubscription binds the separately authenticated Claude subscription Messages transport.
package claudesubscription

import (
	"context"
	"net/http"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/subscription"
)

const (
	APIVersion = "2023-06-01"
	OAuthBeta  = "oauth-2025-04-20"
)

type Connector struct{ *subscription.Connector }

func New() *Connector {
	routes := []subscription.Route{
		{
			Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1/messages", Action: "messages.create", Operation: "generate", ModelLocation: "body", Framing: "sse-named"},
			Protocol: "anthropic-messages", Variant: "claude-subscription",
		},
		{
			Endpoint: core.NativeEndpoint{Method: "POST", Path: "v1/messages/count_tokens", Action: "messages.count_tokens", Operation: "count_tokens", ModelLocation: "body", Framing: "json"},
			Protocol: "anthropic-messages", Variant: "claude-subscription",
		},
	}
	for i := range routes {
		routes[i].Headers = http.Header{"Anthropic-Version": []string{APIVersion}}
		routes[i].AllowedRequestHeaders = []string{"anthropic-beta"}
		routes[i].ValidateRequestHeaders = validateRequestHeaders
	}
	return &Connector{subscription.New("claude-subscription", routes)}
}
func (c *Connector) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	return c.Connector.Bind(ctx, t, o)
}

func validateRequestHeaders(headers http.Header) error {
	for _, value := range headers.Values("anthropic-beta") {
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return invalidHeader("anthropic-beta", "Claude subscription anthropic-beta must contain capability values")
		}
		for _, token := range strings.Split(value, ",") {
			if !validBetaToken(strings.TrimSpace(token)) {
				return invalidHeader("anthropic-beta", "Claude subscription anthropic-beta contains an invalid capability value")
			}
		}
	}
	return nil
}

func validBetaToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		default:
			return false
		}
	}
	return true
}

func invalidHeader(name, message string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: http.StatusBadRequest, Message: message, Param: name, Origin: "gateway"}
}

var _ core.Connector = (*Connector)(nil)
