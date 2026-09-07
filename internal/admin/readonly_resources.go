package admin

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

// Operational records are projections of durable state, never generic CRUD.
func registerReadOnlyResource[T any](api huma.API, s *Server, kind, permission string) {
	huma.Register(api, huma.Operation{OperationID: "list_" + kind, Method: "GET", Path: "/admin/api/v1/" + kind}, func(ctx context.Context, in *humaCollectionInput) (*typedResourcePageOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, permission)
		if e != nil {
			return nil, e
		}
		limit := in.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 200 {
			return nil, huma.Error400BadRequest("limit must be between 1 and 200")
		}
		page, e := s.deps.Repository.List(ctx, p, kind, in.Cursor, limit)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		out := typedResourcePage[T]{Items: []typedResource[T]{}, NextCursor: page.NextCursor}
		for _, r := range page.Items {
			item, e := decodeTypedResource[T](sanitizeResource(r))
			if e != nil {
				return nil, e
			}
			out.Items = append(out.Items, item)
		}
		return &typedResourcePageOutput[T]{Body: out}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "get_" + kind, Method: "GET", Path: "/admin/api/v1/" + kind + "/{id}"}, func(ctx context.Context, in *typedResourceGet) (*typedResourceOutput[T], error) {
		p, e := s.runtimePrincipalWith(ctx, permission)
		if e != nil {
			return nil, e
		}
		resource, e := s.deps.Repository.Get(ctx, p, kind, in.ID)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		out, e := decodeTypedResource[T](sanitizeResource(resource))
		if e != nil {
			return nil, e
		}
		return &typedResourceOutput[T]{Body: out}, nil
	})
}
