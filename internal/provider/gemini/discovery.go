package gemini

import (
	"context"
	"net/url"

	"hoorific/internal/core"
)

// Discover follows Gemini's documented nextPageToken cursor. The endpoint
// helper performs the explicit connection-base validation, credential lease,
// response closure, bounded decode, and upstream status handling per page.
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	if conn.BaseURL == "" {
		return nil, core.GatewayError{Code: "configuration_stale", HTTPStatus: 400, Message: "Model discovery requires an explicit connection base URL", Origin: "gateway"}
	}
	path := "v1beta/models"
	seen := map[string]bool{}
	models := make([]core.Model, 0)
	for range 10000 {
		page, err := c.Connector.DiscoverPage(ctx, conn, path)
		if err != nil {
			return nil, err
		}
		models = append(models, page.Models...)
		if page.NextPageToken == "" {
			if page.HasMore {
				return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: "Gemini model discovery advertised another page without nextPageToken", Origin: "upstream"}
			}
			return models, nil
		}
		if seen[page.NextPageToken] {
			return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: "Gemini model discovery returned a repeated nextPageToken", Origin: "upstream"}
		}
		seen[page.NextPageToken] = true
		path = "v1beta/models?pageToken=" + url.QueryEscape(page.NextPageToken)
	}
	return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: "Gemini model discovery exceeded the pagination limit", Origin: "upstream"}
}
