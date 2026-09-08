package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"hoorific/internal/core"
	"io"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
)

type AliasData struct {
	Targets     []core.RouteTarget `json:"targets,omitempty"`
	ModelIDs    []string           `json:"model_ids,omitempty"`
	Description string             `json:"description,omitempty"`
	Enabled     bool               `json:"enabled"`
}
type RouteTargetData struct {
	ConnectionID string `json:"connection_id"`
	ModelID      string `json:"model_id"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
	Region       string `json:"region,omitempty"`
}
type RoutePolicyData struct {
	Alias         string            `json:"alias"`
	Targets       []RouteTargetData `json:"targets"`
	Residency     []string          `json:"residency,omitempty"`
	Fallback      bool              `json:"fallback"`
	AccountPoolID string            `json:"account_pool_id,omitempty"`
	Affinity      bool              `json:"affinity"`
}
type PolicyLimitData struct {
	Scope             string `json:"scope"`
	ScopeID           string `json:"scope_id"`
	RequestsPerMinute int64  `json:"requests_per_minute,omitempty"`
	TokensPerMinute   int64  `json:"tokens_per_minute,omitempty"`
	MaxCost           int64  `json:"max_cost,omitempty"`
	Concurrency       int    `json:"concurrency,omitempty"`
	CostWindow        string `json:"cost_window,omitempty"`
	OutstandingJobs   int    `json:"outstanding_jobs,omitempty"`
}
type AccountData struct {
	Connector  string   `json:"connector,omitempty"`
	Name       string   `json:"name,omitempty"`
	Provider   string   `json:"provider,omitempty"`
	AccountIDs []string `json:"account_ids,omitempty"`
}
type PriceData struct {
	ModelID                   string `json:"model_id"`
	Currency                  string `json:"currency"`
	InputPerMillion           int64  `json:"input_per_million"`
	OutputPerMillion          int64  `json:"output_per_million"`
	CachedInputPerMillion     *int64 `json:"cached_input_per_million,omitempty"`
	CacheWriteInputPerMillion *int64 `json:"cache_write_input_per_million,omitempty"`
	CacheWrite5mPerMillion    *int64 `json:"cache_write_5m_per_million,omitempty"`
	CacheWrite1hPerMillion    *int64 `json:"cache_write_1h_per_million,omitempty"`
}
type ConnectionData struct {
	Connector string            `json:"connector"`
	AccountID string            `json:"account_id"`
	BaseURL   string            `json:"base_url"`
	Region    string            `json:"region,omitempty"`
	Project   string            `json:"project,omitempty"`
	Dedicated bool              `json:"dedicated"`
	Enabled   bool              `json:"enabled"`
	Settings  map[string]string `json:"settings,omitempty"`
}
type ModelData struct {
	ConnectionID     string              `json:"connection_id"`
	UpstreamID       string              `json:"upstream_id"`
	Operations       []string            `json:"operations"`
	Features         map[string]string   `json:"features,omitempty"`
	InputModalities  []string            `json:"input_modalities,omitempty"`
	OutputModalities []string            `json:"output_modalities,omitempty"`
	ContextLimit     *int64              `json:"context_limit,omitempty"`
	OutputLimit      *int64              `json:"output_limit,omitempty"`
	Provenance       string              `json:"provenance,omitempty"`
	Price            *core.PriceSchedule `json:"price,omitempty"`
	Enabled          bool                `json:"enabled"`
}
type KeyData struct {
	Name          string           `json:"name"`
	Permissions   []string         `json:"permissions"`
	Aliases       []string         `json:"aliases"`
	Connections   []string         `json:"connections"`
	Operations    []core.Operation `json:"operations"`
	Portable      bool             `json:"portable"`
	NativeAccount bool             `json:"native_account"`
	Realtime      bool             `json:"realtime"`
	Role          string           `json:"role"`
	Version       int64            `json:"version,omitempty"`
}

func strictDecode(data []byte, dst any) error {
	if dst == nil || len(data) == 0 || len(data) > 1<<20 {
		return errors.New("invalid resource payload")
	}
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 || trim[0] != '{' {
		return errors.New("resource must be an object")
	}
	d := json.NewDecoder(bytes.NewReader(trim))
	if err := uniqueJSON(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing resource JSON")
	}
	d = json.NewDecoder(bytes.NewReader(trim))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	return nil
}
func uniqueJSON(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok {
				return errors.New("invalid name")
			}
			s = strings.ToLower(s)
			if seen[s] {
				return errors.New("duplicate name")
			}
			seen[s] = true
			if e = uniqueJSON(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := uniqueJSON(d); e != nil {
				return e
			}
		}
	default:
		return errors.New("invalid delimiter")
	}
	_, e = d.Token()
	return e
}
func validRef(s string) bool {
	return s != "" && len(s) <= 512 && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) })
}
func validBaseURL(u *url.URL, settings map[string]string) bool {
	if u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if settings["allow_private"] != "true" || strings.TrimSpace(settings["allowed_cidrs"]) == "" {
		return false
	}
	// A private-HTTP opt-in must not admit public addresses through a broad
	// prefix. Hostnames are resolved only by the egress resolver at dial time.
	privateRanges := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("::1/128"),
	}
	var allowed []netip.Prefix
	for _, raw := range strings.Split(settings["allowed_cidrs"], ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return false
		}
		prefix = prefix.Masked()
		private := false
		for _, parent := range privateRanges {
			if parent.Addr().BitLen() == prefix.Addr().BitLen() && parent.Bits() <= prefix.Bits() && parent.Contains(prefix.Addr()) {
				private = true
				break
			}
		}
		if !private {
			return false
		}
		allowed = append(allowed, prefix)
	}
	if address, err := netip.ParseAddr(host); err == nil {
		for _, prefix := range allowed {
			if prefix.Contains(address) {
				return true
			}
		}
		return false
	}
	return true
}
func validNames(xs []string, wild bool) bool {
	seen := map[string]bool{}
	for _, s := range xs {
		if !validRef(s) || seen[s] || (!wild && s == "*") {
			return false
		}
		seen[s] = true
	}
	return true
}
func validOperations(xs []string) bool {
	seen := map[string]bool{}
	for _, s := range xs {
		if !validRef(s) || seen[s] || strings.ContainsAny(s, " /\\\t\n") {
			return false
		}
		seen[s] = true
	}
	return true
}
func validKeyOperations(xs []core.Operation) bool {
	allowed := map[core.Operation]struct{}{
		"generate": {}, "complete": {}, "embed": {}, "rerank": {}, "count_tokens": {},
		"image.generate": {}, "image.edit": {}, "image.variation": {},
		"audio.speech": {}, "audio.transcribe": {}, "audio.translate": {},
		"video": {}, "realtime": {}, "realtime.ticket": {}, "file": {}, "upload": {},
		"batch": {}, "response.resource": {}, "conversation.resource": {}, "prediction": {},
		"model.list": {}, "native": {},
		"chat.resource": {}, "moderation": {}, "model.resource": {},
		"audio.voice": {}, "audio.voice_consent": {}, "video.extension": {}, "video.character": {},
		"prediction.create": {}, "prediction.get": {}, "prediction.list": {}, "prediction.cancel": {},
		"model.get": {}, "model.create": {}, "model.version.get": {}, "model.version.list": {},
		"deployment.get": {}, "deployment.list": {}, "deployment.create": {},
	}
	seen := map[core.Operation]bool{}
	for _, operation := range xs {
		if _, ok := allowed[operation]; !ok || seen[operation] {
			return false
		}
		seen[operation] = true
	}
	return true
}
func validPriceRate(rate *int64) bool {
	return rate == nil || *rate >= 0
}

func validPriceSchedule(price *core.PriceSchedule) bool {
	return price != nil &&
		validRef(price.Version) &&
		validPriceRate(price.InputPerMillion) &&
		validPriceRate(price.OutputPerMillion) &&
		validPriceRate(price.CachedInputPerMillion) &&
		validPriceRate(price.CacheWriteInputPerMillion) &&
		validPriceRate(price.CacheWrite5mPerMillion) &&
		validPriceRate(price.CacheWrite1hPerMillion) &&
		validPriceRate(price.MaximumUnitCost)
}
func validateResource(kind string, data json.RawMessage) (json.RawMessage, error) {
	bad := func() (json.RawMessage, error) {
		return nil, problem("invalid_resource", 400, "invalid "+kind+" resource")
	}
	var v any
	switch kind {
	case "connections":
		var x ConnectionData
		if strictDecode(data, &x) != nil || !validRef(x.Connector) || x.AccountID != "" && !validRef(x.AccountID) {
			return bad()
		}
		if x.BaseURL != "" {
			u, e := url.Parse(x.BaseURL)
			if e != nil || !validBaseURL(u, x.Settings) {
				return bad()
			}
		}
		v = x
	case "models":
		var x ModelData
		if strictDecode(data, &x) != nil || !validRef(x.ConnectionID) || !validRef(x.UpstreamID) || len(x.Operations) == 0 || !validOperations(x.Operations) || x.ContextLimit != nil && *x.ContextLimit < 0 || x.OutputLimit != nil && *x.OutputLimit < 0 || !validNames(x.InputModalities, false) || !validNames(x.OutputModalities, false) {
			return bad()
		}
		for k, v := range x.Features {
			if !validRef(k) || !validRef(v) || v != "supported" && v != "unsupported" && v != "unknown" {
				return bad()
			}
		}
		if x.Price != nil {
			if !validPriceSchedule(x.Price) {
				return bad()
			}
		}
		v = x
	case "aliases":
		var x AliasData
		if strictDecode(data, &x) != nil || len(x.Targets) == 0 {
			return bad()
		}
		seen := map[[2]string]bool{}
		for _, t := range x.Targets {
			k := [2]string{t.ConnectionID, t.ModelID}
			if !validRef(t.ConnectionID) || !validRef(t.ModelID) || t.Priority < 0 || t.Weight <= 0 || seen[k] {
				return bad()
			}
			seen[k] = true
		}
		v = x
	case "accounts":
		var x AccountData
		if strictDecode(data, &x) != nil || x.Connector == "" && x.Provider == "" || x.Connector != "" && !validRef(x.Connector) || x.Provider != "" && !validRef(x.Provider) {
			return bad()
		}
		if x.Name == "" && len(x.AccountIDs) == 0 {
			return bad()
		}
		v = x
	case "prices":
		var x PriceData
		if strictDecode(data, &x) != nil || !validRef(x.ModelID) || len(x.Currency) != 3 || strings.ToUpper(x.Currency) != x.Currency || x.InputPerMillion < 0 || x.OutputPerMillion < 0 ||
			!validPriceRate(x.CachedInputPerMillion) || !validPriceRate(x.CacheWriteInputPerMillion) || !validPriceRate(x.CacheWrite5mPerMillion) || !validPriceRate(x.CacheWrite1hPerMillion) {
			return bad()
		}
		for _, r := range x.Currency {
			if r < 'A' || r > 'Z' {
				return bad()
			}
		}
		v = x
	case "model_aliases":
		var x AliasData
		if strictDecode(data, &x) != nil || !validNames(x.ModelIDs, false) {
			return bad()
		}
		v = x
	case "route_policies":
		var x RoutePolicyData
		if strictDecode(data, &x) != nil || !validRef(x.Alias) || len(x.Targets) == 0 || !validNames(x.Residency, false) || x.AccountPoolID != "" && !validRef(x.AccountPoolID) {
			return bad()
		}
		for _, t := range x.Targets {
			if !validRef(t.ConnectionID) || !validRef(t.ModelID) || t.Priority < 0 || t.Weight <= 0 || t.Region != "" && !validRef(t.Region) {
				return bad()
			}
		}
		v = x
	case "policy_limits":
		var x PolicyLimitData
		if strictDecode(data, &x) != nil || !validRef(x.Scope) || !validRef(x.ScopeID) || x.RequestsPerMinute < 0 || x.TokensPerMinute < 0 || x.MaxCost < 0 || x.Concurrency < 0 || x.OutstandingJobs < 0 || x.CostWindow != "" && x.CostWindow != "total" && x.CostWindow != "daily" && x.CostWindow != "monthly" {
			return bad()
		}
		v = x
	case "account_pools":
		var x AccountData
		if strictDecode(data, &x) != nil || !validRef(x.Provider) || !validNames(x.AccountIDs, false) {
			return bad()
		}
		v = x
	case "api_keys":
		var x KeyData
		if strictDecode(data, &x) != nil || len(x.Permissions) == 0 || !validNames(x.Permissions, true) || !validNames(x.Aliases, true) || !validNames(x.Connections, true) || !validKeyOperations(x.Operations) {
			return bad()
		}
		switch x.Role {
		case "owner", "admin", "operator", "viewer":
		default:
			return bad()
		}
		v = x
	default:
		return bad()
	}
	out, e := json.Marshal(v)
	if e != nil {
		return bad()
	}
	return out, nil
}
