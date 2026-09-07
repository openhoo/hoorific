package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/resource"
)

type nativeContinuationDispatch struct {
	URL     string
	Binding core.Binding
}
type nativeContinuationKey struct{}
type nativeContinuationMetadata struct {
	Binding           core.Binding
	Action            string
	Operation         core.Operation
	Method            string
	OriginalAction    string
	ConnectionVersion int64
}

func (g *Gateway) continuationURL(connectionID, id string) (string, error) {
	base, err := url.Parse(g.deps.PublicURL)
	if err != nil || base.Host == "" || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return "", failure("unavailable", 503, "configured inference URL is unavailable")
	}
	escaped := strings.TrimRight(base.EscapedPath(), "/") + "/connect/" + url.PathEscape(connectionID) + "/continuations/" + url.PathEscape(id)
	base.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return "", err
	}
	base.RawPath = escaped
	return base.String(), nil
}

// A descriptor authorizes the destination, not reuse of the source account.
// Keep the egress restrictions while dropping provider identity and secrets.
func continuationConnection(source core.Connection, target string) core.Connection {
	settings := make(map[string]string)
	for _, key := range []string{"allow_private", "allowed_cidrs", "allow_same_origin_redirect"} {
		if value, ok := source.Settings[key]; ok {
			settings[key] = value
		}
	}
	return core.Connection{BaseURL: target, Settings: settings}
}
func continuationID(tenant, connection, account, raw string) string {
	return resource.IDForScopedContinuation(tenant, connection, account, raw)
}

func (g *Gateway) sealContinuation(ctx context.Context, p core.Principal, t selected, b core.Binding, raw string, uploadID, method, action string, origins []string) (string, error) {
	if g.deps.Resources == nil || g.deps.Keys == nil {
		return "", failure("unavailable", 503, "continuation persistence unavailable")
	}
	idBytes := make([]byte, 24)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	nonce := fmt.Sprintf("%x", idBytes)
	id := continuationID(p.TenantID, t.connection.ID, t.connection.AccountID, nonce)
	publicURL, err := g.continuationURL(t.connection.ID, id)
	if err != nil {
		return "", err
	}
	keyID, key, err := g.deps.Keys.Current()
	if err != nil {
		return "", failure("unavailable", 503, "continuation encryption unavailable")
	}
	if method == "" || action == "" {
		return "", failure("unsupported_operation", 502, "continuation action is not declared")
	}
	c := resource.Continuation{ID: id, TenantID: p.TenantID, ConnectionID: t.connection.ID, AccountID: t.connection.AccountID, ExpiresAt: time.Now().Add(10 * time.Minute), UpstreamURL: raw, UploadID: uploadID, KeyID: keyID}
	operation := b.Codec.Operation
	if t.endpoint != nil && t.endpoint.Operation != "" {
		operation = t.endpoint.Operation
	}
	originalAction := ""
	if t.endpoint != nil {
		originalAction = t.endpoint.Action
	}
	if originalAction == "" {
		originalAction = action
	}
	c.Metadata, _ = json.Marshal(nativeContinuationMetadata{Binding: b, Action: action, Operation: operation, Method: method, OriginalAction: originalAction, ConnectionVersion: t.connection.Version})
	if err = resource.SealContinuation(key, &c, origins); err != nil {
		return "", failure("unsupported_operation", 502, "upstream continuation URL was not allowed")
	}
	if err = g.deps.Resources.PutContinuation(ctx, c); err != nil {
		return "", failure("unavailable", 503, "continuation persistence failed")
	}
	return publicURL, nil
}

func (g *Gateway) rewriteNativeHeaders(ctx context.Context, response *http.Response, p core.Principal, t selected, b core.Binding) error {
	if len(b.Response.ContinuationHeaders) == 0 {
		return nil
	}
	for _, name := range b.Response.ContinuationHeaders {
		values := response.Header.Values(name)
		if len(values) == 0 {
			continue
		}
		method, action := b.Response.ContinuationMethods[name], b.Response.ContinuationActions[name]
		if method == "" || action == "" {
			return failure("unsupported_operation", 502, "native continuation descriptor is incomplete")
		}
		for i, raw := range values {
			trimmed := strings.TrimSpace(raw)
			u, err := url.Parse(trimmed)
			if err != nil || u.Scheme == "" || u.Host == "" {
				continue
			}
			mapped, err := g.sealContinuation(ctx, p, t, b, trimmed, "", method, action, b.Response.ContinuationOrigins)
			if err != nil {
				return err
			}
			values[i] = mapped
		}
		response.Header.Del(name)
		for _, v := range values {
			response.Header.Add(name, v)
		}
	}
	return nil
}

func skipJSONSpace(data []byte, pos int) int {
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		default:
			return pos
		}
	}
	return pos
}
func jsonStringToken(data []byte, pos int) (int, string, error) {
	pos = skipJSONSpace(data, pos)
	if pos >= len(data) || data[pos] != '"' {
		return pos, "", fmt.Errorf("JSON string expected")
	}
	dec := json.NewDecoder(bytes.NewReader(data[pos:]))
	var value string
	if err := dec.Decode(&value); err != nil {
		return pos, "", err
	}
	return pos + int(dec.InputOffset()), value, nil
}
func skipJSONValue(data []byte, pos int) (int, error) {
	pos = skipJSONSpace(data, pos)
	if pos >= len(data) {
		return pos, ioErrUnexpectedEOF
	}
	switch data[pos] {
	case '"':
		end, _, err := jsonStringToken(data, pos)
		return end, err
	case '{':
		pos++
		pos = skipJSONSpace(data, pos)
		if pos < len(data) && data[pos] == '}' {
			return pos + 1, nil
		}
		for {
			end, _, err := jsonStringToken(data, pos)
			if err != nil {
				return pos, err
			}
			pos = skipJSONSpace(data, end)
			if pos >= len(data) || data[pos] != ':' {
				return pos, fmt.Errorf("JSON colon expected")
			}
			end, err = skipJSONValue(data, pos+1)
			if err != nil {
				return pos, err
			}
			pos = skipJSONSpace(data, end)
			if pos < len(data) && data[pos] == '}' {
				return pos + 1, nil
			}
			if pos >= len(data) || data[pos] != ',' {
				return pos, fmt.Errorf("JSON object separator expected")
			}
			pos = skipJSONSpace(data, pos+1)
		}
	case '[':
		pos++
		pos = skipJSONSpace(data, pos)
		if pos < len(data) && data[pos] == ']' {
			return pos + 1, nil
		}
		for {
			end, err := skipJSONValue(data, pos)
			if err != nil {
				return pos, err
			}
			pos = skipJSONSpace(data, end)
			if pos < len(data) && data[pos] == ']' {
				return pos + 1, nil
			}
			if pos >= len(data) || data[pos] != ',' {
				return pos, fmt.Errorf("JSON array separator expected")
			}
			pos = skipJSONSpace(data, pos+1)
		}
	default:
		for pos < len(data) && !strings.ContainsRune(" \t\r\n,]}", rune(data[pos])) {
			pos++
		}
		return pos, nil
	}
}

var ioErrUnexpectedEOF = fmt.Errorf("unexpected JSON end")

func rewriteJSONPath(value json.RawMessage, path string, rewrite func(string) (string, error)) (json.RawMessage, bool, error) {
	parts := strings.Split(path, ".")
	if len(parts) == 0 || parts[0] == "" {
		return value, false, nil
	}
	var walk func([]byte, int, int) ([]byte, bool, error)
	walk = func(data []byte, pos, idx int) ([]byte, bool, error) {
		pos = skipJSONSpace(data, pos)
		if pos >= len(data) {
			return data, false, nil
		}
		if idx >= len(parts) {
			return data, false, nil
		}
		if data[pos] != '{' {
			return data, false, nil
		}
		start := pos
		pos++
		for {
			pos = skipJSONSpace(data, pos)
			if pos < len(data) && data[pos] == '}' {
				return data, false, nil
			}
			keyEnd, key, err := jsonStringToken(data, pos)
			if err != nil {
				return data, false, err
			}
			pos = skipJSONSpace(data, keyEnd)
			if pos >= len(data) || data[pos] != ':' {
				return data, false, fmt.Errorf("JSON colon expected")
			}
			child := skipJSONSpace(data, pos+1)
			childEnd, err := skipJSONValue(data, child)
			if err != nil {
				return data, false, err
			}
			if key == parts[idx] {
				if idx == len(parts)-1 {
					end, raw, err := jsonStringToken(data, child)
					if err == nil {
						mapped, err := rewrite(raw)
						if err != nil {
							return data, false, err
						}
						if mapped != raw {
							enc, _ := json.Marshal(mapped)
							out := make([]byte, 0, len(data)+len(enc)-end+child)
							out = append(out, data[:child]...)
							out = append(out, enc...)
							out = append(out, data[end:]...)
							return out, true, nil
						}
					}
				} else {
					next, changed, err := walk(data, child, idx+1)
					if err != nil {
						return data, false, err
					}
					if changed {
						return next, true, nil
					}
				}
			}
			pos = skipJSONSpace(data, childEnd)
			if pos < len(data) && data[pos] == '}' {
				return data, false, nil
			}
			if pos >= len(data) || data[pos] != ',' {
				return data, false, fmt.Errorf("JSON object separator expected")
			}
			pos = skipJSONSpace(data, pos+1)
			_ = start
		}
	}
	return walk(value, 0, 0)
}

func (g *Gateway) rewriteNativeFields(ctx context.Context, data []byte, p core.Principal, t selected, b core.Binding) ([]byte, bool, error) {
	if len(b.Response.ContinuationFields) == 0 {
		return data, false, nil
	}
	out := data
	changed := false
	for _, path := range b.Response.ContinuationFields {
		method, action := b.Response.ContinuationMethods[path], b.Response.ContinuationActions[path]
		if method == "" || action == "" {
			return nil, false, failure("unsupported_operation", 502, "native continuation descriptor is incomplete")
		}
		next, did, err := rewriteJSONPath(out, path, func(raw string) (string, error) {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || u.Scheme == "" || u.Host == "" {
				return raw, nil
			}
			uploadID := nativeString(data, b.Response.IDField)
			if uploadID == "" {
				if target, ok := t.target.(core.ConnectionResourceCall); ok {
					uploadID = target.ResourceID
				}
			}
			return g.sealContinuation(ctx, p, t, b, strings.TrimSpace(raw), uploadID, method, action, b.Response.ContinuationOrigins)
		})
		if err != nil {
			return nil, false, err
		}
		if did {
			out = next
			changed = true
		}
	}
	return out, changed, nil
}

func (g *Gateway) resolveContinuation(ctx context.Context, r *http.Request, p core.Principal, c core.Connection, id string) (selected, core.Binding, *http.Request, error) {
	var zero selected
	// Opaque resource controls bypass model target selection, not account
	// authorization. A continuation is never evidence of a native account grant.
	if !p.NativeAccount || c.TenantID != p.TenantID || !c.Dedicated || !contains(p.Connections, c.ID) {
		return zero, core.Binding{}, r, failure("forbidden", 403, "native continuation requires an explicit account grant")
	}
	if g.deps.Resources == nil || g.deps.Keys == nil {
		return zero, core.Binding{}, r, failure("unavailable", 503, "continuation persistence unavailable")
	}
	record, err := g.deps.Resources.GetContinuation(ctx, p.TenantID, c.ID, id)
	if err != nil {
		return zero, core.Binding{}, r, failure("unsupported_operation", 404, "continuation not found")
	}
	if err = resource.ValidateScope(record, p.TenantID, c.ID, c.AccountID, time.Now()); err != nil {
		return zero, core.Binding{}, r, failure("forbidden", 403, "continuation scope mismatch")
	}
	key, err := g.deps.Keys.ByID(record.KeyID)
	if err != nil {
		return zero, core.Binding{}, r, failure("unavailable", 503, "continuation encryption key unavailable")
	}
	upstream, _, err := resource.OpenContinuation(key, record, time.Now())
	if err != nil {
		return zero, core.Binding{}, r, failure("forbidden", 403, "continuation is invalid or expired")
	}
	var meta nativeContinuationMetadata
	if json.Unmarshal(record.Metadata, &meta) != nil || meta.Action == "" || meta.Operation == "" || meta.Method == "" || meta.OriginalAction == "" {
		return zero, core.Binding{}, r, failure("unsupported_operation", 404, "continuation descriptor is unavailable")
	}
	if meta.ConnectionVersion != 0 && meta.ConnectionVersion != c.Version {
		return zero, core.Binding{}, r, failure("configuration_stale", 409, "continuation descriptor is stale")
	}
	if !strings.EqualFold(r.Method, meta.Method) {
		return zero, core.Binding{}, r, failure("unsupported_operation", 405, "continuation method is not allowed")
	}
	connector, ok := g.deps.Connectors[c.Connector]
	if !ok {
		return zero, core.Binding{}, r, failure("unsupported_operation", 404, "connector is unavailable")
	}
	target := core.ConnectionResourceCall{Connection: c, Action: meta.Action, ResourceID: record.UploadID}
	binding := meta.Binding
	binding.Method = meta.Method
	var endpoints []core.NativeEndpoint
	if inventory, ok := connector.(core.EndpointInventory); ok {
		endpoints = inventory.Endpoints()
	}
	if inventory, ok := connector.(core.ConnectionInventory); ok {
		endpoints, err = inventory.EndpointsFor(c)
		if err != nil {
			return zero, core.Binding{}, r, err
		}
	}
	matched := false
	if record.UploadID != "" {
		if binder, ok := connector.(core.EndpointBinder); ok {
			for _, endpoint := range endpoints {
				if endpoint.Action != meta.Action || !strings.EqualFold(endpoint.Method, meta.Method) {
					continue
				}
				resourceID := record.UploadID
				params := map[string]string{
					"id": resourceID, "resource_id": resourceID,
					"request_id": resourceID, "operation": resourceID,
					"model": resourceID,
				}
				binding, err = binder.BindEndpoint(ctx, c, endpoint, params)
				if err != nil {
					return zero, core.Binding{}, r, err
				}
				matched = true
				break
			}
		}
	}
	if !matched {
		if meta.Action == meta.OriginalAction {
			return zero, core.Binding{}, r, failure("unsupported_operation", 404, "continuation action is unavailable")
		}
		// Some provider continuation URLs (for example streaming/result
		// controls) are opaque and intentionally absent from the inventory.
		// Never carry the CREATE response policy into those requests.
		binding.Response = core.NativeResponsePolicy{}
	}
	if binding.Response.Async {
		return zero, core.Binding{}, r, failure("unsupported_operation", 404, "continuation response contract is unavailable")
	}
	if !contains(p.Operations, binding.Codec.Operation) {
		return zero, core.Binding{}, r, failure("forbidden", 403, "continuation operation is not granted")
	}
	r = r.WithContext(context.WithValue(ctx, nativeContinuationKey{}, nativeContinuationDispatch{URL: upstream, Binding: binding}))
	return selected{connection: c, target: target, connector: connector}, binding, r, nil
}

func (g *Gateway) serveContinuation(ctx context.Context, r *http.Request, p core.Principal, t selected, b core.Binding, id string) (*http.Request, error) {
	resolved, _, next, err := g.resolveContinuation(ctx, r, p, t.connection, id)
	_ = resolved
	_ = b
	return next, err
}
