package main

// Live qualification deliberately does not discover models at the provider, log
// in, import credentials, or use ambient credentials. Admin reads are local
// gateway reads. Connection capabilities are authenticated management data:
// their endpoint inventory is intersected with THIS RUNNER BUILD's production
// codecs/binders; unknown remote-only operations fail closed.
//
// Required CLI integration: --allow-paid --connections=id[,id]
// --live-gateway-url --live-management-url --live-inference-key-file
// --live-admin-cookie-file --live-csrf-file --live-cases-file
// --live-spend-ceiling-nanodollars. URLs are HTTPS origins (HTTP only loopback).
// Secret files contain raw values, not cookie jars or shell assignments, and
// must be regular, non-symlink files with no group/other permissions.
//
// Cases JSON (strict, version 1): {"version":1,"cases":[{"connection_id":"c",
// "operation":"generate","model_id":"catalog-id","action":"chat.create",
// "timeout_seconds":30,"request":{"model":"upstream-id","messages":[
// {"role":"user","content":"Say hello"}],"max_tokens":8}}]}.
// One case per advertised operation per selected connection, at most 64 cases.
// model_id is an explicit configured catalog resource ID, NEVER a deletable
// resource. action is an exact production endpoint descriptor action, NOT a URL.
// request is the operation's wire JSON, decoded AND re-encoded by its production
// codec. No arbitrary headers, paths, URLs, cleanup scripts, bound assertions,
// tool execution, resource references, multimodal fetching or retries are allowed.
// Supported execution is stateless model generate/complete/embed/rerank/count_tokens
// with registered production codecs, JSON or bounded streaming. Stateful, async,
// realtime, opaque native and resource operations fail as LOCAL implementation
// gaps before dispatch: no cancellation/cleanup success is invented for them.
// No resources are created by accepted cases; stream cancellation closes the
// same pinned HTTP request and is not claimed to prove provider cancellation.
// PriceSchedule supplies the conservative bound, never an operator estimate.
// An existing authenticated gateway hard-cost policy fence no greater than the
// approved ceiling is also required; this runner never creates or mutates it.
// The remote catalog/prices are re-read before each dispatch; post-reread policy
// mutation remains an evidence limitation, not a claimed transactional fence.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"hoorific/internal/app"
	"hoorific/internal/core"
	"hoorific/internal/policy"
	"hoorific/internal/protocol"
)

type liveOptions struct {
	GatewayURL, ManagementURL                                 string
	InferenceKeyFile, AdminCookieFile, CSRFFile, RequestsFile string
	SpendCeilingNano                                          int64
}

type liveCasesFile struct {
	Version int        `json:"version"`
	Cases   []liveCase `json:"cases"`
}
type liveCase struct {
	ConnectionID   string          `json:"connection_id"`
	Operation      core.Operation  `json:"operation"`
	ModelID        string          `json:"model_id"`
	Action         string          `json:"action"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	Request        json.RawMessage `json:"request"`
}
type liveConnectionData struct {
	Connector string            `json:"connector"`
	AccountID string            `json:"account_id"`
	BaseURL   string            `json:"base_url"`
	Region    string            `json:"region"`
	Project   string            `json:"project"`
	Dedicated bool              `json:"dedicated"`
	Enabled   bool              `json:"enabled"`
	Settings  map[string]string `json:"settings"`
}
type liveModelData struct {
	ConnectionID string              `json:"connection_id"`
	UpstreamID   string              `json:"upstream_id"`
	Operations   []core.Operation    `json:"operations"`
	ContextLimit *int64              `json:"context_limit"`
	OutputLimit  *int64              `json:"output_limit"`
	Price        *core.PriceSchedule `json:"price"`
	Enabled      bool                `json:"enabled"`
}
type liveResource[T any] struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Version  int64  `json:"version"`
	Data     T      `json:"data"`
}
type livePage[T any] struct {
	Items      []liveResource[T] `json:"items"`
	NextCursor string            `json:"next_cursor"`
}
type liveCapabilityDescriptor struct {
	ID           string           `json:"id"`
	Protocols    []core.Protocol  `json:"protocols"`
	Operations   []core.Operation `json:"operations"`
	Subscription bool             `json:"subscription"`
}
type liveCapabilityEndpoint struct {
	Method          string         `json:"method"`
	Path            string         `json:"path"`
	Action          string         `json:"action"`
	Operation       core.Operation `json:"operation"`
	ModelLocation   string         `json:"model_location"`
	Framing         core.Framing   `json:"framing"`
	Stateful        bool           `json:"stateful"`
	ResourceIDField string         `json:"resource_id_field"`
}
type liveCapabilities struct {
	ConnectionID string                   `json:"connection_id"`
	Version      int64                    `json:"version"`
	Descriptor   liveCapabilityDescriptor `json:"descriptor"`
	Endpoints    []liveCapabilityEndpoint `json:"endpoints"`
}
type livePolicyLimitData struct {
	Scope      string `json:"scope"`
	ScopeID    string `json:"scope_id"`
	MaxCost    int64  `json:"max_cost"`
	CostWindow string `json:"cost_window"`
}
type livePlan struct {
	c          liveCase
	connection liveResource[liveConnectionData]
	model      liveResource[liveModelData]
	binding    core.Binding
	entry      protocol.Entry
	body       []byte
	bound      int64
	stream     bool
}
type liveHTTP struct {
	client              *http.Client
	gateway, management string
	key, cookie, csrf   string
}

const liveMaxBytes = 4 << 20

func liveStrict(data []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return errors.New("invalid or unknown JSON field")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func liveID(s string) bool {
	if s == "" || len(s) > 200 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func livePrivate(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("required private file is absent or unreadable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > max {
		return nil, errors.New("file must be private regular non-symlink and within size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private file could not be opened")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return nil, errors.New("private file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("private file read failed or exceeded limit")
	}
	return b, nil
}
func liveSecret(path string) (string, error) {
	b, err := livePrivate(path, 16384)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", errors.New("private credential file is empty")
	}
	for _, r := range s {
		if r <= 32 || r >= 127 || r == ';' {
			return "", errors.New("credential must be a single raw header-safe value")
		}
	}
	return s, nil
}
func liveOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", errors.New("gateway URL must be an explicit origin without userinfo/path/query/fragment")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return "", errors.New("HTTPS required except literal loopback HTTP")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}
func (h *liveHTTP) request(ctx context.Context, admin bool, method, path string, body []byte) (*http.Response, error) {
	origin := h.gateway
	if admin {
		origin = h.management
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n") {
		return nil, errors.New("invalid internal route")
	}
	r, err := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot construct pinned request")
	}
	if admin {
		name := "hoorific_session"
		if strings.HasPrefix(origin, "https://") {
			name = "__Host-hoorific_session"
		}
		r.AddCookie(&http.Cookie{Name: name, Value: h.cookie, Path: "/"})
		r.Header.Set("X-CSRF-Token", h.csrf)
	} else {
		r.Header.Set("Authorization", "Bearer "+h.key)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream, application/x-ndjson")
	resp, err := h.client.Do(r)
	if err != nil {
		return nil, errors.New("pinned gateway transport failed or deadline expired (URL/credentials withheld)")
	}
	return resp, nil
}
func (h *liveHTTP) adminJSON(ctx context.Context, path string, into any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := h.request(ctx, true, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("management GET returned HTTP %d; requires authorized live admin session with connection:read and catalog:read", r.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, liveMaxBytes+1))
	if err != nil || len(b) > liveMaxBytes {
		return errors.New("management response read failed or exceeded size limit")
	}
	if json.Unmarshal(b, into) != nil {
		return errors.New("management response does not match resource DTO")
	}
	return nil
}
func liveHardFence(ctx context.Context, h *liveHTTP, plans []livePlan, ceiling int64) error {
	if len(plans) == 0 {
		return nil
	}
	var limits []liveResource[livePolicyLimitData]
	cursor := ""
	seen := map[string]bool{}
	for range 100 {
		var page livePage[livePolicyLimitData]
		if err := h.adminJSON(ctx, "/admin/api/v1/policy_limits?limit=200&cursor="+url.QueryEscape(cursor), &page); err != nil {
			return errors.New("external prerequisite: authenticated gateway policy_limits could not be read; no paid dispatch")
		}
		limits = append(limits, page.Items...)
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return errors.New("external prerequisite: policy limit pagination is cyclic; no paid dispatch")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	tenant := plans[0].model.TenantID
	var aggregate int64
	for _, p := range plans {
		if tenant == "" || p.model.TenantID != tenant || p.connection.TenantID != tenant || p.bound < 0 || p.bound > ceiling-aggregate {
			return errors.New("external prerequisite: all admitted cases must share one tenant and fit the approved ceiling")
		}
		aggregate += p.bound
	}
	for _, limit := range limits {
		// Separate scopes and resetting windows cannot fence the aggregate run.
		// Empty cost_window is the production policy_sync total-window default.
		if limit.TenantID == tenant && limit.Version > 0 && limit.Data.Scope == "tenant" && limit.Data.ScopeID == tenant &&
			(limit.Data.CostWindow == "" || limit.Data.CostWindow == "total") &&
			limit.Data.MaxCost > 0 && limit.Data.MaxCost >= aggregate && limit.Data.MaxCost <= ceiling {
			return nil
		}
	}
	return errors.New("external prerequisite: one existing shared tenant-scoped total-window hard-cost policy must cover all cases, with max_cost at least the aggregate bound and no greater than the approved ceiling; budget:read required; no paid dispatch")
}
func liveModels(ctx context.Context, h *liveHTTP) (map[string]liveResource[liveModelData], error) {
	out := map[string]liveResource[liveModelData]{}
	cursor := ""
	seen := map[string]bool{}
	for range 100 {
		var p livePage[liveModelData]
		if err := h.adminJSON(ctx, "/admin/api/v1/models?limit=200&cursor="+url.QueryEscape(cursor), &p); err != nil {
			return nil, err
		}
		for _, m := range p.Items {
			if !liveID(m.ID) || m.Version <= 0 {
				return nil, errors.New("invalid catalog resource identity/version")
			}
			if _, exists := out[m.ID]; exists {
				return nil, errors.New("duplicate catalog resource")
			}
			out[m.ID] = m
		}
		if p.NextCursor == "" {
			return out, nil
		}
		if seen[p.NextCursor] {
			return nil, errors.New("catalog cursor cycle")
		}
		seen[p.NextCursor] = true
		cursor = p.NextCursor
	}
	return nil, errors.New("catalog exceeds bounded pagination limit")
}

func executeLive(add func(result), allowPaid bool, connections string, opts liveOptions) {
	if !allowPaid {
		add(result{Name: "live", Status: "not-run", Detail: "requires --allow-paid; zero gateway/provider contact"})
		return
	}
	fail := func(detail string) { add(result{Name: "live/setup", Status: "failed", Detail: detail}) }
	ids := map[string]bool{}
	for _, id := range strings.Split(connections, ",") {
		id = strings.TrimSpace(id)
		if !liveID(id) || ids[id] {
			fail("--connections requires distinct explicit connection IDs")
			return
		}
		ids[id] = true
	}
	if len(ids) > 64 || opts.SpendCeilingNano <= 0 {
		fail("at most 64 connections and positive --live-spend-ceiling-nanodollars required")
		return
	}
	gateway, err := liveOrigin(opts.GatewayURL)
	if err != nil {
		fail(err.Error())
		return
	}
	management, err := liveOrigin(opts.ManagementURL)
	if err != nil {
		fail(err.Error())
		return
	}
	b, err := livePrivate(opts.RequestsFile, liveMaxBytes)
	if err != nil {
		fail("--live-cases-file: " + err.Error())
		return
	}
	var cases liveCasesFile
	if liveStrict(b, &cases) != nil || cases.Version != 1 || len(cases.Cases) == 0 || len(cases.Cases) > 64 {
		fail("--live-cases-file requires strict version 1 schema and 1..64 cases")
		return
	}
	byFamily := map[string]liveCase{}
	for _, c := range cases.Cases {
		key := c.ConnectionID + "/" + string(c.Operation)
		if !ids[c.ConnectionID] || !liveID(string(c.Operation)) || !liveID(c.ModelID) || !liveID(c.Action) || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 120 || len(c.Request) == 0 {
			fail("invalid case selection/action/model/timeout; every case must belong to --connections")
			return
		}
		if _, exists := byFamily[key]; exists {
			fail("duplicate case per connection/operation")
			return
		}
		byFamily[key] = c
	}
	key, keyErr := liveSecret(opts.InferenceKeyFile)
	cookie, cookieErr := liveSecret(opts.AdminCookieFile)
	csrf, csrfErr := liveSecret(opts.CSRFFile)
	// Missing inference credentials do not hide case/implementation failures: with
	// admin credentials available discovery and preflight still run, never infer.
	if cookieErr != nil || csrfErr != nil {
		fail("external prerequisite: --live-admin-cookie-file and --live-csrf-file must contain private raw active admin session values; cannot enumerate advertised families without authenticated management access")
		return
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	h := &liveHTTP{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect forbidden") }}, gateway: gateway, management: management, key: key, cookie: cookie, csrf: csrf}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	models, err := liveModels(ctx, h)
	if err != nil {
		fail(err.Error())
		return
	}
	// Construction/binding only. Discovery's client can never reach a provider.
	connectors := app.Builtins(func(core.Connection) (*http.Client, error) { return &http.Client{Transport: liveNoContact{}}, nil }, nil, true)
	var plans []livePlan
	var total int64
	selected := make([]string, 0, len(ids))
	for id := range ids {
		selected = append(selected, id)
	}
	sort.Strings(selected)
	budgetExceeded := false
	for _, id := range selected {
		var remote liveResource[liveConnectionData]
		if err := h.adminJSON(ctx, "/admin/api/v1/connections/"+id, &remote); err != nil {
			add(result{Name: "live/" + id, Status: "failed", Detail: err.Error()})
			continue
		}
		if remote.ID != id || remote.Version <= 0 || !remote.Data.Enabled {
			add(result{Name: "live/" + id, Status: "failed", Detail: "selected connection is absent, invalid or disabled"})
			continue
		}
		connector := connectors[remote.Data.Connector]
		if connector == nil {
			add(result{Name: "live/" + id, Status: "failed", Detail: "local implementation gap: connector unavailable in runner-build descriptors"})
			continue
		}
		var capabilities liveCapabilities
		if err := h.adminJSON(ctx, "/admin/api/v1/connections/"+id+"/capabilities", &capabilities); err != nil {
			add(result{Name: "live/" + id, Status: "failed", Detail: "authenticated connection capability inventory unavailable: " + err.Error()})
			continue
		}
		if capabilities.ConnectionID != id || capabilities.Version != remote.Version || capabilities.Descriptor.ID != remote.Data.Connector {
			add(result{Name: "live/" + id, Status: "failed", Detail: "connection capability inventory is stale or does not match the selected connector"})
			continue
		}
		families := map[core.Operation]bool{}
		for _, op := range capabilities.Descriptor.Operations {
			families[op] = true
		}
		for _, m := range models {
			if m.Data.ConnectionID == id && m.Data.Enabled {
				for _, op := range m.Data.Operations {
					families[op] = true
				}
			}
		}
		ops := make([]string, 0, len(families))
		for op := range families {
			ops = append(ops, string(op))
		}
		sort.Strings(ops)
		for _, op := range ops {
			name := "live/" + id + "/" + op
			c, ok := byFamily[id+"/"+op]
			if !ok {
				add(result{Name: name, Status: "failed", Detail: "missing explicit case for advertised operation; descriptor_source=authenticated-management-capabilities intersected with runner-build codec, catalog_source=authenticated-management"})
				continue
			}
			delete(byFamily, id+"/"+op)
			if budgetExceeded {
				add(result{Name: name, Status: "failed", Detail: "aggregate verified worst-case bound exceeds the configured nanodollar ceiling; no inference dispatched"})
				continue
			}
			p, err := livePrepare(ctx, c, remote, models, connector, capabilities.Endpoints)
			if err != nil {
				add(result{Name: name, Status: "failed", Detail: err.Error()})
				continue
			}
			if p.bound > opts.SpendCeilingNano-total {
				budgetExceeded = true
				fail("aggregate verified worst-case bound exceeds nanodollar ceiling; no inference dispatched")
				add(result{Name: name, Status: "failed", Detail: "aggregate verified worst-case bound exceeds the configured nanodollar ceiling; no inference dispatched"})
				continue
			}
			total += p.bound
			plans = append(plans, p)
		}
	}
	for _, c := range byFamily {
		add(result{Name: "live/" + c.ConnectionID + "/" + string(c.Operation), Status: "failed", Detail: "case not reconciled to configured connection and advertised descriptor/catalog family"})
	}
	if budgetExceeded {
		for _, prior := range plans {
			add(result{Name: "live/" + prior.c.ConnectionID + "/" + string(prior.c.Operation), Status: "failed", Detail: "aggregate verified worst-case bound exceeds the configured nanodollar ceiling; no inference dispatched"})
		}
		return
	}
	// All discovery, payload, route, bound and aggregate checks finish before the
	// first inference. Failed cases are never dispatched; known admitted cases
	// remain useful even when another advertised family has missing local code.
	if keyErr != nil {
		for _, p := range plans {
			add(result{Name: "live/" + p.c.ConnectionID + "/" + string(p.c.Operation), Status: "not-run", Detail: "external prerequisite: private --live-inference-key-file with a gateway key granting selected connection, model alias and operation; " + keyErr.Error()})
		}
		return
	}
	if err := liveHardFence(ctx, h, plans, opts.SpendCeilingNano); err != nil {
		add(result{Name: "live/admission", Status: "not-run", Detail: err.Error()})
		for _, p := range plans {
			add(result{Name: "live/" + p.c.ConnectionID + "/" + string(p.c.Operation), Status: "not-run", Detail: "external prerequisite: no inference dispatched because an existing gateway hard-cost policy fence was not verifiably within the approved ceiling"})
		}
		return
	}
	evidenceDir, err := os.MkdirTemp("", "hoorific-live-")
	if err != nil {
		fail("cannot create private evidence directory; no inference dispatched")
		return
	}
	stop := false
	for i, p := range plans {
		name := "live/" + p.c.ConnectionID + "/" + string(p.c.Operation)
		if stop || ctx.Err() != nil {
			add(result{Name: name, Status: "failed", Detail: "run stopped after a prior operation failure or deadline; this family was not dispatched"})
			continue
		}
		var connNow liveResource[liveConnectionData]
		var modelNow liveResource[liveModelData]
		if h.adminJSON(ctx, "/admin/api/v1/connections/"+p.c.ConnectionID, &connNow) != nil || h.adminJSON(ctx, "/admin/api/v1/models/"+p.c.ModelID, &modelNow) != nil || !liveEqual(connNow, p.connection) || !liveEqual(modelNow, p.model) {
			add(result{Name: name, Status: "failed", Detail: "configuration/pricing changed or could not be revalidated; no dispatch"})
			stop = true
			continue
		}
		started := time.Now()
		usage, err := liveDispatch(ctx, h, p)
		status, detail := "passed", "operation-specific terminal/output/usage verified; no resource creation or cleanup applicable"
		if err != nil {
			status, detail, stop = "failed", err.Error(), true
		}
		evidence := map[string]any{"descriptor_source": "authenticated-management-capabilities intersected with runner-build codecs", "catalog_source": "authenticated-management", "admission": "conservative aggregate snapshot bound with just-in-time catalog reread plus authenticated existing gateway hard-cost policy fence; policy/config can still change after reread", "bound_nanodollars": p.bound, "aggregate_bound_nanodollars": total, "price_version": p.model.Data.Price.Version, "usage": usage, "action": p.c.Action, "request_sha256": liveHash(p.body), "cleanup": "not applicable: stateless-only admission"}
		r := result{Name: name, Status: status, Detail: detail, DurationMS: time.Since(started).Milliseconds(), Evidence: evidence}
		data, marshalErr := json.Marshal(r)
		path := filepath.Join(evidenceDir, fmt.Sprintf("case-%03d.json", i))
		if marshalErr != nil || os.WriteFile(path, data, 0600) != nil {
			r.Status = "failed"
			r.Detail = "private evidence write failed after operation"
			stop = true
		} else {
			evidence["private_evidence_file"] = path
		}
		add(r)
	}
}

type liveNoContact struct{}

func (liveNoContact) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("runner descriptor construction cannot contact provider")
}
func liveEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func liveHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func liveHas(ops []core.Operation, op core.Operation) bool {
	for _, x := range ops {
		if x == op {
			return true
		}
	}
	return false
}

func livePrepare(ctx context.Context, c liveCase, remote liveResource[liveConnectionData], models map[string]liveResource[liveModelData], connector core.Connector, capabilityEndpoints []liveCapabilityEndpoint) (livePlan, error) {
	p := livePlan{c: c, connection: remote}
	bad := func(s string) (livePlan, error) { return p, errors.New(s) }
	if !liveHas(connector.Descriptor().Operations, c.Operation) {
		return bad("configured operation is unknown to runner-build descriptor; local implementation gap")
	}
	switch c.Operation {
	case "generate", "complete", "embed", "rerank", "count_tokens":
	default:
		return bad("local implementation gap: no verified response/whole-session-bound/owned-cleanup executor for advertised family; not an external prerequisite")
	}
	m, ok := models[c.ModelID]
	if !ok || !m.Data.Enabled || m.Data.ConnectionID != c.ConnectionID || m.TenantID != remote.TenantID || m.Data.UpstreamID == "" || !liveHas(m.Data.Operations, c.Operation) {
		return bad("case model_id must identify an enabled model in selected connection's authenticated catalog with matching operation")
	}
	p.model = m
	d := remote.Data
	conn := core.Connection{TenantID: remote.TenantID, ID: remote.ID, Connector: d.Connector, AccountID: d.AccountID, BaseURL: d.BaseURL, Region: d.Region, Project: d.Project, Version: remote.Version, Dedicated: d.Dedicated, Settings: d.Settings}
	var eps []core.NativeEndpoint
	for _, e := range capabilityEndpoints {
		eps = append(eps, core.NativeEndpoint{Method: e.Method, Path: e.Path, Action: e.Action, Operation: e.Operation, ModelLocation: e.ModelLocation, Framing: e.Framing, Stateful: e.Stateful, ResourceIDField: e.ResourceIDField})
	}
	var ep core.NativeEndpoint
	matches := 0
	for _, e := range eps {
		if e.Action == c.Action && e.Operation == c.Operation {
			ep = e
			matches++
		}
	}
	if matches != 1 {
		return bad("case action must uniquely match configured runner-build operation descriptor")
	}
	if ep.Method != "POST" || ep.Stateful || ep.ResourceIDField != "" || ep.ModelLocation != "body" || strings.ContainsAny(ep.Path, "{}?%") {
		return bad("local implementation gap: only stateless body-model fixed POST routes have verified execution; resource/path-model/async cleanup not implemented")
	}
	binder, ok := connector.(core.EndpointBinder)
	if !ok {
		return bad("local implementation gap: connector has no endpoint binder")
	}
	binding, err := binder.BindEndpoint(ctx, conn, ep, map[string]string{})
	if err != nil {
		return bad("production endpoint binding failed")
	}
	if binding.Endpoint != ep.Path || binding.Method != ep.Method || binding.Codec.Operation != c.Operation || binding.Realtime != nil || binding.Response.Async || len(binding.Response.ContinuationFields) > 0 || len(binding.Response.ContinuationHeaders) > 0 {
		return bad("unverified continuation/session/async binding refused before dispatch")
	}
	if binding.Framing != "json" && binding.Framing != "sse-data" && binding.Framing != "sse-named" && binding.Framing != "ndjson" {
		return bad("local implementation gap: unsupported response framing")
	}
	if binding.Codec.Protocol == "openai-responses" {
		return bad("local implementation gap: Responses storage/resource lifecycle has no owned cleanup executor")
	}
	var stateFields map[string]json.RawMessage
	if json.Unmarshal(c.Request, &stateFields) != nil || stateFields == nil {
		return bad("case request must be a JSON object")
	}
	for _, field := range []string{"store", "background", "previous_response_id", "conversation", "file_id", "batch_id", "response_id", "session", "session_id"} {
		if _, present := stateFields[field]; present {
			return bad("state-bearing or resource-reference request field refused: " + field)
		}
	}
	p.binding = binding
	p.stream = binding.Framing != "json"
	entry, ok := protocol.Builtins()[binding.Codec]
	if !ok || entry.Request == nil || entry.Result == nil || p.stream && entry.Stream == nil {
		return bad("local implementation gap: exact production operation codec is absent")
	}
	p.entry = entry
	payload, err := entry.Request.DecodeRequest(ctx, bytes.NewReader(c.Request))
	if err != nil {
		return bad("case request rejected by exact production operation codec")
	}
	model, stream, err := liveSafePayload(payload)
	if err != nil {
		return bad(err.Error())
	}
	if model != m.Data.UpstreamID || stream != p.stream {
		return bad("request model/stream must exactly match catalog upstream ID and selected descriptor framing")
	}
	var canonical bytes.Buffer
	if entry.Request.EncodeRequest(ctx, payload, &canonical) != nil {
		return bad("case cannot be encoded by production codec")
	}
	p.body = canonical.Bytes()
	if len(p.body) > liveMaxBytes {
		return bad("canonical request exceeds size limit")
	}
	price := m.Data.Price
	if price == nil || price.Version == "" {
		return bad("missing verified catalog price bound; no dispatch")
	}
	// Independent full context/output maxima, as documented by boundModel in
	// store/policy_plan.go. Do not trust caller token counts or payload estimates.
	if m.Data.ContextLimit != nil && m.Data.OutputLimit != nil && price.InputPerMillion != nil && price.OutputPerMillion != nil {
		p.bound, err = policy.EstimateCost(*m.Data.ContextLimit, *m.Data.OutputLimit, *price.InputPerMillion, *price.OutputPerMillion, 1000000)
		if err != nil {
			return bad("conservative token bound is negative or overflows nanodollars")
		}
	} else if price.MaximumUnitCost != nil && price.UnitOperation == c.Operation && *price.MaximumUnitCost >= 0 {
		p.bound = *price.MaximumUnitCost
	} else {
		return bad("unknown conservative whole-operation bound: needs full catalog context/output token prices or exact MaximumUnitCost/UnitOperation")
	}
	return p, nil
}

func liveSafePayload(payload core.RequestPayload) (string, bool, error) {
	bad := errors.New("case must be text-only, stateless, without tools/resource references and with explicit positive output limits")
	safeConversation := func(c core.Conversation) bool {
		if len(c.Tools) > 0 || len(c.StructuredOutput) > 0 || c.MaxOutputTokens == nil || *c.MaxOutputTokens <= 0 {
			return false
		}
		blocks := append([]core.ContentBlock(nil), c.System...)
		for _, m := range c.Messages {
			blocks = append(blocks, m.Content...)
		}
		for _, b := range blocks {
			if b.Kind != "text" || b.URL != "" || len(b.Data) > 0 || b.ID != "" || b.Name != "" || b.Arguments != "" {
				return false
			}
		}
		return len(blocks) > 0
	}
	switch v := payload.(type) {
	case core.Conversation:
		if !safeConversation(v) {
			return "", false, bad
		}
		return v.Model, v.Stream, nil
	case core.CompletionRequest:
		if v.MaxOutputTokens == nil || *v.MaxOutputTokens <= 0 || v.Prompt == "" {
			return "", false, bad
		}
		return v.Model, v.Stream, nil
	case core.EmbeddingRequest:
		if len(v.Inputs) == 0 {
			return "", false, bad
		}
		return v.Model, false, nil
	case core.RerankRequest:
		if v.Query == "" || len(v.Documents) == 0 {
			return "", false, bad
		}
		return v.Model, false, nil
	case core.CountTokensRequest:
		if !safeConversation(v.Conversation) {
			return "", false, bad
		}
		return v.Conversation.Model, false, nil
	default:
		return "", false, bad
	}
}

func liveDispatch(parent context.Context, h *liveHTTP, p livePlan) (*core.Usage, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.c.TimeoutSeconds)*time.Second)
	defer cancel()
	// Connection and path are selected from explicit IDs and production binding,
	// never a returned Location, continuation URL or operator-provided URL.
	r, err := h.request(ctx, false, http.MethodPost, "/connect/"+p.c.ConnectionID+"/native/"+p.binding.Endpoint, p.body)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pinned operation returned HTTP %d (body withheld); no retry", r.StatusCode)
	}
	limited := &io.LimitedReader{R: r.Body, N: liveMaxBytes + 1}
	var usage *core.Usage
	var terminal, content bool
	if p.stream {
		decoder, err := p.entry.Stream.NewDecoder(limited)
		if err != nil {
			return nil, errors.New("operation stream decoder initialization failed")
		}
		for n := range 100000 {
			e, err := decoder.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return usage, errors.New("operation stream malformed/truncated or deadline exceeded; pinned request closed, provider cancellation unconfirmed")
			}
			switch v := e.(type) {
			case core.TextDelta:
				content = content || v.Text != ""
			case core.Usage:
				u := v
				usage = &u
			case core.Finish:
				terminal = liveTerminal(v)
			case core.StreamError:
				return usage, errors.New("provider stream reported operation error")
			}
			if n == 99999 {
				return usage, errors.New("operation exceeded finite event limit")
			}
		}
	} else {
		b, err := io.ReadAll(limited)
		if err != nil || len(b) > liveMaxBytes {
			return nil, errors.New("operation body read failed or exceeded size limit")
		}
		v, err := p.entry.Result.DecodeResult(ctx, bytes.NewReader(b))
		if err != nil {
			return nil, errors.New("operation-specific response decoding failed")
		}
		switch x := v.(type) {
		case core.GenerationResult:
			terminal = liveTerminal(x.Finish)
			usage = x.Usage
			for _, b := range x.Blocks {
				content = content || b.Kind == "text" && b.Text != ""
			}
		case core.CompletionResult:
			terminal = liveTerminal(x.Finish)
			content = x.Text != ""
			usage = x.Usage
		case core.EmbeddingResult:
			terminal = true
			content = len(x.Embeddings) > 0
			usage = x.Usage
		case core.RerankResult:
			terminal = true
			content = len(x.Results) > 0
			usage = x.Usage
		case core.CountTokensResult:
			terminal = x.InputTokens != nil && *x.InputTokens >= 0
			content = terminal
			usage = &core.Usage{Input: x.InputTokens, Source: x.Source}
		default:
			return nil, errors.New("local implementation gap: result payload is not a verified operation response")
		}
	}
	if limited.N <= 0 {
		return usage, errors.New("operation response exceeded bounded bytes")
	}
	if !terminal || !content {
		return usage, errors.New("operation lacks successful terminal state or semantic output")
	}
	if usage == nil || usage.Input == nil || *usage.Input < 0 {
		return usage, errors.New("operation lacks measured nonnegative input usage")
	}
	if (p.c.Operation == "generate" || p.c.Operation == "complete") && (usage.Output == nil || *usage.Output < 0) {
		return usage, errors.New("generation lacks measured nonnegative output usage")
	}
	if p.model.Data.ContextLimit != nil && *usage.Input > *p.model.Data.ContextLimit || usage.Output != nil && p.model.Data.OutputLimit != nil && *usage.Output > *p.model.Data.OutputLimit {
		return usage, errors.New("provider usage exceeded verified catalog bound; no further dispatch")
	}
	return usage, nil
}
func liveTerminal(f core.Finish) bool {
	return f.Status == "completed" || f.Status == "stop" || f.Status == "length"
}
