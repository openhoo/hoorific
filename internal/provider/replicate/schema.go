package replicate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

// VersionSchema keeps Replicate's native OpenAPI document intact. In
// particular, $ref, oneOf, nullable values, and provider-specific outputs are
// not projected into the gateway's portable request schema.
type VersionSchema struct {
	Model         core.Model
	OpenAPISchema json.RawMessage
}

// DiscoverVersion is the explicit, trusted discovery boundary for Replicate
// version metadata. The HTTP client and credential source are supplied by the
// caller; no ambient credentials or model-name catalog lookup is used.
// Replicate documents this response at:
// https://replicate.com/docs/reference/http#models.versions.get
func DiscoverVersion(ctx context.Context, conn core.Connection, modelVersion string, client *http.Client, credentials core.CredentialSource) (VersionSchema, error) {
	if client == nil {
		return VersionSchema{}, core.GatewayError{Code: "configuration_stale", HTTPStatus: 400, Message: "Replicate schema discovery requires an injected HTTP client", Origin: "gateway"}
	}
	owner, name, version, err := modelParts(modelVersion)
	if err != nil || version == "" {
		if err != nil {
			return VersionSchema{}, err
		}
		return VersionSchema{}, invalid("Replicate version discovery requires owner/name:version")
	}
	base := conn.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return VersionSchema{}, invalid("Replicate discovery base URL must be an HTTPS origin")
	}
	if u.Path != "" && u.Path != "/" {
		return VersionSchema{}, invalid("Replicate discovery base URL must not contain a path")
	}
	u.Path = "/v1/models/" + mustSegment(owner) + "/" + mustSegment(name) + "/versions/" + mustSegment(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return VersionSchema{}, err
	}
	if credentials == nil {
		return VersionSchema{}, invalid("Replicate schema discovery requires an injected credential source")
	}
	lease, err := credentials.Lease(ctx, conn)
	if err != nil {
		return VersionSchema{}, err
	}
	if lease == nil {
		return VersionSchema{}, invalid("Replicate credential lease is unavailable")
	}
	authErr := lease.Authorize(ctx, req)
	closeErr := core.CloseCredentialLease(lease)
	if authErr != nil {
		return VersionSchema{}, authErr
	}
	if closeErr != nil {
		return VersionSchema{}, closeErr
	}
	resp, err := client.Do(req)
	if err != nil {
		return VersionSchema{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return VersionSchema{}, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: "Replicate version discovery failed", Origin: "upstream", Retryable: resp.StatusCode >= 500}
	}
	var raw struct {
		ID            string          `json:"id"`
		OpenAPISchema json.RawMessage `json:"openapi_schema"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&raw); err != nil {
		return VersionSchema{}, invalid("Replicate version metadata is invalid JSON")
	}
	if len(raw.OpenAPISchema) == 0 || string(raw.OpenAPISchema) == "null" {
		return VersionSchema{}, invalid("Replicate version has no openapi_schema")
	}
	return VersionSchema{Model: core.Model{ID: modelVersion, ConnectionID: conn.ID, Operations: []core.Operation{PredictionCreate}, Features: map[string]core.Support{}, Provenance: "replicate_version_discovery"}, OpenAPISchema: append(json.RawMessage(nil), raw.OpenAPISchema...)}, nil
}

func mustSegment(s string) string { escaped, _ := endpoint.Segment(s); return escaped }

// ValidateStreamURL accepts only Replicate's documented API stream origin or
// the configured API origin. Returned media URLs and arbitrary webhook URLs
// are not valid stream transports and must not receive gateway credentials.
func ValidateStreamURL(conn core.Connection, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("Replicate stream URL must be an HTTPS URL without credentials, query or fragment")
	}
	base := conn.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	b, err := url.Parse(base)
	if err != nil || b.Scheme != "https" || b.Host == "" || b.User != nil || b.RawQuery != "" || b.Fragment != "" {
		return "", invalid("Replicate base URL is invalid")
	}
	host := strings.ToLower(u.Hostname())
	baseHost := strings.ToLower(b.Hostname())
	if host != "api.replicate.com" && host != "stream.replicate.com" && host != "streaming.api.replicate.com" && host != baseHost {
		return "", invalid("Replicate stream URL host is not trusted")
	}
	return u.String(), nil
}
