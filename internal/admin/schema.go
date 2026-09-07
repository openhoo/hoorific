package admin

import "github.com/danielgtaylor/huma/v2"

// API returns the registered Huma API used by schema tooling and management composition.
func (s *Server) API() huma.API { return s.api }
