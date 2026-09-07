package app

import (
	"context"
	"hoorific/internal/core"
	"net/http"
)

func (a *connectionAdapter) AdaptRequest(ctx context.Context, t core.Target, b core.Binding, raw []byte) ([]byte, error) {
	c, e := a.instance(targetConnection(t))
	if e != nil {
		return nil, e
	}
	if adapter, ok := c.(core.WireAdapter); ok {
		return adapter.AdaptRequest(ctx, t, b, raw)
	}
	return raw, nil
}
func (a *connectionAdapter) AdaptResponse(ctx context.Context, b core.Binding, response *http.Response) error {
	if adapter, ok := a.base.(core.WireAdapter); ok {
		return adapter.AdaptResponse(ctx, b, response)
	}
	return nil
}
func (a *connectionAdapter) ExecuteDuplex(ctx context.Context, c core.Connection, model string, lease core.CredentialLease, client *http.Client, input <-chan []byte, output func([]byte) error) error {
	v, e := a.instance(c)
	if e != nil {
		return e
	}
	if duplex, ok := v.(core.StreamDuplex); ok {
		return duplex.ExecuteDuplex(ctx, c, model, lease, client, input, output)
	}
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "connector has no duplex transport"}
}
