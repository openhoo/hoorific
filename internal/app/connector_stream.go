package app

import (
	"context"
	"hoorific/internal/core"
)

func (a *connectionAdapter) BindStream(ctx context.Context, t core.Target, op core.Operation, stream bool) (core.Binding, error) {
	c, e := a.instance(targetConnection(t))
	if e != nil {
		return core.Binding{}, e
	}
	if b, ok := c.(core.StreamBinder); ok {
		return b.BindStream(ctx, t, op, stream)
	}
	return c.Bind(ctx, t, op)
}
