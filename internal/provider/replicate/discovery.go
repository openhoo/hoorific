package replicate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
)

// Version preserves Replicate's complete OpenAPI Schema object. RawMessage is
// intentional: $ref targets and vendor extensions must not be normalized.
type Version struct {
	ID            string          `json:"id"`
	CreatedAt     string          `json:"created_at,omitempty"`
	OpenAPISchema json.RawMessage `json:"openapi_schema"`
}

type ModelRecord struct {
	ID            string     `json:"id,omitempty"`
	Owner         string     `json:"owner,omitempty"`
	Name          string     `json:"name,omitempty"`
	LatestVersion *Version   `json:"latest_version,omitempty"`
	Model         core.Model `json:"-"`
}

type modelPage struct {
	Next    json.RawMessage `json:"next"`
	Results []struct {
		ID            string   `json:"id"`
		Owner         string   `json:"owner"`
		Name          string   `json:"name"`
		LatestVersion *Version `json:"latest_version"`
	} `json:"results"`
}
type versionPage struct {
	Next    json.RawMessage `json:"next"`
	Results []Version       `json:"results"`
}

// Discover performs authenticated model listing and returns gateway manifests.
// Schema-bearing records are available from DiscoverModels; core.Model has no
// schema field and therefore never receives a lossy schema projection.
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	records, err := c.DiscoverModels(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make([]core.Model, 0, len(records))
	for _, r := range records {
		out = append(out, r.Model)
	}
	return out, nil
}

func (c *Connector) DiscoverModels(ctx context.Context, conn core.Connection) ([]ModelRecord, error) {
	var records []ModelRecord
	path := "v1/models"
	for page := 0; page < 100 && path != ""; page++ {
		var p modelPage
		next, err := c.getJSON(ctx, conn, path, &p)
		if err != nil {
			return nil, err
		}
		for _, raw := range p.Results {
			id := raw.ID
			if id == "" && raw.Owner != "" && raw.Name != "" {
				id = raw.Owner + "/" + raw.Name
			}
			if id == "" {
				continue
			}
			owner, name, _, err := modelParts(id)
			if err != nil {
				continue
			}
			records = append(records, ModelRecord{ID: id, Owner: owner, Name: name, LatestVersion: raw.LatestVersion, Model: core.Model{ID: id, ConnectionID: conn.ID, Operations: []core.Operation{PredictionCreate}, Features: map[string]core.Support{}, Provenance: "replicate"}})
		}
		path = next
	}
	return records, nil
}

func (c *Connector) DiscoverModel(ctx context.Context, conn core.Connection, owner, name string) (ModelRecord, error) {
	if _, _, _, err := modelParts(owner + "/" + name); err != nil {
		return ModelRecord{}, err
	}
	var raw struct {
		ID            string   `json:"id"`
		Owner         string   `json:"owner"`
		Name          string   `json:"name"`
		LatestVersion *Version `json:"latest_version"`
	}
	_, err := c.getJSON(ctx, conn, "v1/models/"+owner+"/"+name, &raw)
	if err != nil {
		return ModelRecord{}, err
	}
	id := raw.ID
	if id == "" {
		id = owner + "/" + name
	}
	return ModelRecord{ID: id, Owner: owner, Name: name, LatestVersion: raw.LatestVersion, Model: core.Model{ID: id, ConnectionID: conn.ID, Operations: []core.Operation{PredictionCreate}, Features: map[string]core.Support{}, Provenance: "replicate"}}, nil
}

func (c *Connector) DiscoverVersion(ctx context.Context, conn core.Connection, owner, name, id string) (Version, error) {
	if !validVersion(id) {
		return Version{}, invalid("Invalid Replicate version ID")
	}
	if _, _, _, err := modelParts(owner + "/" + name); err != nil {
		return Version{}, err
	}
	var v Version
	_, err := c.getJSON(ctx, conn, "v1/models/"+owner+"/"+name+"/versions/"+id, &v)
	return v, err
}

func (c *Connector) ListVersions(ctx context.Context, conn core.Connection, owner, name string) ([]Version, error) {
	if _, _, _, err := modelParts(owner + "/" + name); err != nil {
		return nil, err
	}
	var out []Version
	path := "v1/models/" + owner + "/" + name + "/versions"
	for page := 0; page < 100 && path != ""; page++ {
		var p versionPage
		next, err := c.getJSON(ctx, conn, path, &p)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Results...)
		path = next
	}
	return out, nil
}

func (c *Connector) getJSON(ctx context.Context, conn core.Connection, path string, dst any) (string, error) {
	client, creds := c.DiscoveryClient(), c.CredentialSource()
	if client == nil || creds == nil {
		return "", invalid("Replicate discovery requires explicitly injected HTTP client and credentials")
	}
	base := conn.BaseURL
	if base == "" {
		base = c.DefaultBaseURL()
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return "", invalid("Invalid Replicate discovery base URL")
	}
	u, err := resolveTrusted(baseURL, path)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	lease, err := creds.Lease(ctx, conn)
	if err != nil {
		return "", err
	}
	if lease == nil {
		return "", invalid("Replicate credential lease is unavailable")
	}
	authErr := lease.Authorize(ctx, req)
	closeErr := core.CloseCredentialLease(lease)
	if authErr != nil {
		return "", authErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: "Replicate discovery failed", Origin: "upstream", Retryable: resp.StatusCode >= 500}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(dst); err != nil {
		return "", fmt.Errorf("invalid Replicate discovery JSON: %w", err)
	}
	next, err := nextPath(baseURL, dst)
	if err != nil {
		return "", err
	}
	return next, nil
}

func resolveTrusted(base *url.URL, path string) (*url.URL, error) {
	p, err := url.Parse(path)
	if err != nil || p.Fragment != "" {
		return nil, invalid("Invalid Replicate discovery continuation")
	}
	if p.IsAbs() || p.Host != "" {
		if p.Scheme != base.Scheme || !strings.EqualFold(p.Host, base.Host) {
			return nil, invalid("Replicate discovery continuation left trusted origin")
		}
		return p, nil
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return nil, invalid("Invalid Replicate discovery path")
	}
	out := *base
	out.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(p.Path, "/")
	out.RawQuery = p.RawQuery
	return &out, nil
}

func nextPath(base *url.URL, value any) (string, error) {
	var raw json.RawMessage
	switch p := value.(type) {
	case *modelPage:
		raw = p.Next
	case *versionPage:
		raw = p.Next
	default:
		return "", nil
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return "", invalid("Replicate discovery continuation is invalid")
	}
	u, err := resolveTrusted(base, s)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
