package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hoorific/internal/core"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// deepAdminScenarios exercises the management API as a browser/client would. It
// deliberately uses a raw response path instead of extRequest for redaction
// assertions: extRequest is useful evidence plumbing, but it scrubs known
// secrets before a caller can prove that the production response did not leak
// them. Every resource created here has a run-unique ID; the seeded fixture is
// only read or used as an upstream target.
func (e *environment) deepAdminScenarios() []result {
	start := time.Now()
	if e == nil || e.client == nil || e.fixture == nil || e.management == "" || e.inference == "" || e.cookie == "" || e.csrf == "" || e.key == "" || e.tenantID == "" {
		return []result{deepAdminResult("admin/setup", start, nil, errors.New("isolated management session, inference key, fixture, and tenant are required"))}
	}
	state := deepAdminState{
		prefix:         deepAdminUniqueID("deep-admin"),
		originalTenant: e.tenantID,
		fixtureSecret:  "fixture-upstream-secret",
	}
	out := make([]result, 0, 13)
	out = append(out,
		e.deepAdminSessionBoundary(state),
		e.deepAdminResourceLifecycle(state),
		e.deepAdminCredentialProjection(state),
		e.deepAdminTenantIsolation(state),
		e.deepAdminConnectionLifecycle(state),
		e.clientProfileScenario(),
		e.deepAdminRouteDryRun(state),
		e.deepAdminAPIKeyLifecycle(state),
		e.deepAdminConfigLifecycle(state),
		e.deepAdminAdminTokenLifecycle(state),
		e.deepAdminAuditRedaction(state),
		e.deepAdminAuditAuthorization(state),
		e.deepAdminRestartPersistence(state),
	)
	return out
}

type deepAdminState struct {
	prefix, originalTenant, fixtureSecret string
}

func (s deepAdminState) id(kind string) string { return s.prefix + "-" + kind }

type deepRawObservation struct {
	Status        int
	ErrorCode     string
	RequestID     string
	ContentType   string
	Body          []byte
	UpstreamCalls int
}

// deepRawRequest is intentionally local to this file. It is not shared with
// the existing scenarios because it keeps response bytes untouched until the
// caller has completed a no-secret assertion.
func (e *environment) deepRawRequest(ctx context.Context, management bool, method, path string, body []byte, headers func(*http.Request)) (deepRawObservation, error) {
	address := e.inference
	if management {
		address = e.management
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, bytes.NewReader(body))
	if err != nil {
		return deepRawObservation{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if management {
		request.Header.Set("Cookie", e.cookie)
		request.Header.Set("Origin", "http://"+e.management)
		request.Header.Set("X-CSRF-Token", e.csrf)
	} else {
		request.Header.Set("Authorization", "Bearer "+e.key)
	}
	if headers != nil {
		headers(request)
	}
	before := 0
	if e.fixture != nil {
		before = e.fixture.count()
	}
	client := *e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return deepRawObservation{UpstreamCalls: e.fixture.count() - before}, err
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	observation := deepRawObservation{
		Status:        response.StatusCode,
		ErrorCode:     response.Header.Get("X-Hoorific-Error-Code"),
		RequestID:     response.Header.Get("X-Request-ID"),
		ContentType:   response.Header.Get("Content-Type"),
		Body:          raw,
		UpstreamCalls: e.fixture.count() - before,
	}
	if len(raw) > 4<<20 {
		return observation, errors.New("gateway response exceeded 4 MiB evidence limit")
	}
	return observation, readErr
}

func deepJSONBody(value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []byte:
		return append([]byte(nil), v...), nil
	case json.RawMessage:
		return append([]byte(nil), v...), nil
	default:
		return json.Marshal(v)
	}
}

func (e *environment) deepCall(management bool, method, path string, body any, headers func(*http.Request)) (deepRawObservation, error) {
	raw, err := deepJSONBody(body)
	if err != nil {
		return deepRawObservation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return e.deepRawRequest(ctx, management, method, path, raw, headers)
}

func deepAdminResult(name string, start time.Time, evidence any, err error) result {
	return extResult(name, start, evidence, err)
}

func deepSafeObservation(o deepRawObservation) map[string]any {
	return map[string]any{
		"status":         o.Status,
		"error_code":     o.ErrorCode,
		"request_id":     o.RequestID,
		"upstream_calls": o.UpstreamCalls,
	}
}

func deepProblemCode(o deepRawObservation) string {
	if o.ErrorCode != "" {
		return o.ErrorCode
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(o.Body, &envelope) == nil {
		return envelope.Code
	}
	return ""
}

func deepExpectStatus(o deepRawObservation, status int, label string) error {
	if o.Status != status {
		return fmt.Errorf("%s expected HTTP %d, got HTTP %d (%s)", label, status, o.Status, deepProblemCode(o))
	}
	return nil
}

func deepExpect2xx(o deepRawObservation, label string) error {
	if o.Status < 200 || o.Status >= 300 {
		return fmt.Errorf("%s expected a successful response, got HTTP %d (%s)", label, o.Status, deepProblemCode(o))
	}
	return nil
}

func deepExpect4xx(o deepRawObservation, label string) error {
	if o.Status < 400 || o.Status >= 500 {
		return fmt.Errorf("%s expected a client rejection, got HTTP %d (%s)", label, o.Status, deepProblemCode(o))
	}
	return nil
}

func deepAssertNoSecret(raw []byte, label string, secrets ...string) error {
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			return fmt.Errorf("%s response contained credential material", label)
		}
	}
	return nil
}

func deepAssertNoCredentialFields(raw []byte, label string) error {
	lower := strings.ToLower(string(raw))
	for _, field := range []string{`"secret":`, `"access_token":`, `"refresh_token":`, `"private_key":`, `"verifier":`, `"ciphertext":`} {
		if strings.Contains(lower, field) {
			return fmt.Errorf("%s response exposed a credential field", label)
		}
	}
	return nil
}

func deepResourceVersion(raw []byte) (int64, error) {
	var resource struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(raw, &resource); err != nil || resource.Version < 1 {
		return 0, errors.New("resource response omitted a positive version")
	}
	return resource.Version, nil
}

type deepResourcePage struct {
	Items []struct {
		ID      string          `json:"id"`
		Version int64           `json:"version"`
		Data    json.RawMessage `json:"data"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func deepPage(raw []byte) (deepResourcePage, error) {
	var page deepResourcePage
	if err := json.Unmarshal(raw, &page); err != nil {
		return page, err
	}
	return page, nil
}

func deepSnapshotState(raw []byte) (int64, []byte, error) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return 0, nil, err
	}
	var revision int64
	foundRevision := false
	for _, key := range []string{"Revision", "revision"} {
		value, ok := object[key]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil || json.Unmarshal(encoded, &revision) != nil {
			return 0, nil, errors.New("configuration export revision was not an integer")
		}
		delete(object, key)
		foundRevision = true
	}
	if !foundRevision || revision < 1 {
		return 0, nil, errors.New("configuration export omitted a positive revision")
	}
	config, err := json.Marshal(object)
	if err != nil {
		return 0, nil, err
	}
	return revision, config, nil

}

type deepSession struct {
	Principal core.Principal `json:"principal"`
	CSRFToken string         `json:"csrf_token"`
	Tenants   []struct {
		TenantID string `json:"tenant_id"`
		Role     string `json:"role"`
		Enabled  bool   `json:"enabled"`
	} `json:"tenants"`
}

func (e *environment) deepSessionRead() (deepSession, deepRawObservation, error) {
	observation, err := e.deepCall(true, http.MethodGet, "/admin/api/v1/session", nil, nil)
	if err != nil {
		return deepSession{}, observation, err
	}
	if err = deepExpectStatus(observation, http.StatusOK, "session lookup"); err != nil {
		return deepSession{}, observation, err
	}
	var session deepSession
	if err = json.Unmarshal(observation.Body, &session); err != nil {
		return deepSession{}, observation, errors.New("session response was not valid JSON")
	}
	if session.Principal.TenantID == "" || session.Principal.SubjectID == "" || session.CSRFToken == "" {
		return deepSession{}, observation, errors.New("session response omitted principal or CSRF state")
	}
	return session, observation, nil
}

func (e *environment) deepSelectTenant(tenant string) error {
	session, _, err := e.deepSessionRead()
	if err != nil {
		return err
	}
	if session.Principal.TenantID == tenant {
		return nil
	}
	observation, err := e.deepCall(true, http.MethodPost, "/admin/api/v1/session/tenant", map[string]any{"tenant_id": tenant}, nil)
	if err != nil {
		return err
	}
	if err = deepExpectStatus(observation, http.StatusOK, "tenant selection"); err != nil {
		return err
	}
	var selected deepSession
	if json.Unmarshal(observation.Body, &selected) != nil || selected.Principal.TenantID != tenant {
		return errors.New("tenant selection response did not select the requested tenant")
	}
	return nil
}

func (e *environment) deepEnsureOriginal(s deepAdminState) error {
	return e.deepSelectTenant(s.originalTenant)
}

func (e *environment) deepAdminSessionBoundary(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	session, lookup, err := e.deepSessionRead()
	evidence := map[string]any{"session": deepSafeObservation(lookup)}
	if err == nil && session.Principal.TenantID != s.originalTenant {
		err = fmt.Errorf("session started in tenant %q rather than the seeded tenant", session.Principal.TenantID)
	}
	if err == nil {
		found := false
		for _, tenant := range session.Tenants {
			if tenant.TenantID == s.originalTenant && tenant.Enabled {
				found = true
			}
		}
		if !found {
			err = errors.New("session membership did not include the current enabled tenant")
		}
	}
	duplicate, duplicateErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/session", nil, func(r *http.Request) {
		r.Header.Set("Cookie", e.cookie+"; "+e.cookie)
	})
	if err == nil {
		if duplicateErr != nil {
			err = duplicateErr
		} else if duplicate.Status != http.StatusUnauthorized {
			err = fmt.Errorf("duplicate session cookies expected HTTP 401, got HTTP %d", duplicate.Status)
		}
	}
	bearerTenant, bearerErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/session/tenant", map[string]any{"tenant_id": s.originalTenant}, func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Del("X-CSRF-Token")
		r.Header.Set("Authorization", "Bearer "+e.key)
	})
	if err == nil {
		if bearerErr != nil {
			err = bearerErr
		} else if bearerTenant.Status < 400 {
			err = fmt.Errorf("inference bearer unexpectedly selected an administrative tenant, HTTP %d", bearerTenant.Status)
		}
	}
	missing, missingErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/session", nil, func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Del("Authorization")
	})
	if err == nil {
		if missingErr != nil {
			err = missingErr
		} else if missing.Status != http.StatusUnauthorized {
			err = fmt.Errorf("missing session expected HTTP 401, got HTTP %d", missing.Status)
		}
	}
	evidence["duplicate_cookie"] = deepSafeObservation(duplicate)
	evidence["bearer_tenant_selection"] = deepSafeObservation(bearerTenant)
	evidence["missing_session"] = deepSafeObservation(missing)
	evidence["fixture_calls"] = e.fixture.count() - before
	return deepAdminResult("admin/session-boundary", start, evidence, err)
}

func (e *environment) deepAdminResourceLifecycle(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/resource-versioned-crud", start, nil, err)
	}
	aliasOne, aliasTwo := s.id("alias-one"), s.id("alias-two")
	created := map[string]int64{}
	cleanup := func() {
		for id, version := range created {
			if version > 0 {
				_, _ = e.deepCall(true, http.MethodDelete, "/admin/api/v1/model_aliases/"+url.PathEscape(id), nil, func(r *http.Request) {
					r.Header.Set("If-Match", strconv.FormatInt(version, 10))
				})
			}
		}
	}
	defer cleanup()
	aliasData := func(description string) map[string]any {
		return map[string]any{"model_ids": []string{"fixture-model"}, "description": description, "enabled": true}
	}
	create := func(id, description string) (deepRawObservation, error) {
		return e.deepCall(true, http.MethodPost, "/admin/api/v1/model_aliases", map[string]any{"id": id, "data": aliasData(description)}, nil)
	}
	first, err := create(aliasOne, "deep admin first alias")
	evidence := map[string]any{"create_first": deepSafeObservation(first)}
	if err == nil {
		err = deepExpect2xx(first, "first alias create")
	}
	if err == nil {
		created[aliasOne], err = deepResourceVersion(first.Body)
	}
	second, secondErr := create(aliasTwo, "deep admin second alias")
	evidence["create_second"] = deepSafeObservation(second)
	if err == nil && secondErr != nil {
		err = secondErr
	}
	if err == nil {
		err = deepExpect2xx(second, "second alias create")
	}
	if err == nil {
		created[aliasTwo], err = deepResourceVersion(second.Body)
	}
	get, getErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasOne), nil, nil)
	evidence["get"] = deepSafeObservation(get)
	if err == nil && getErr != nil {
		err = getErr
	}
	if err == nil {
		err = deepExpectStatus(get, http.StatusOK, "alias get")
	}
	if err == nil {
		var one struct {
			Version int64 `json:"version"`
		}
		if json.Unmarshal(get.Body, &one) != nil || one.Version != created[aliasOne] {
			err = errors.New("alias get did not return the created version")
		}
	}
	missingMatch, missingMatchErr := e.deepCall(true, http.MethodPut, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasOne), map[string]any{"data": aliasData("missing revision")}, nil)
	evidence["missing_if_match"] = deepSafeObservation(missingMatch)
	if err == nil {
		if missingMatchErr != nil {
			err = missingMatchErr
		} else {
			err = deepExpectStatus(missingMatch, http.StatusPreconditionRequired, "missing alias revision")
		}
	}
	updated, updateErr := e.deepCall(true, http.MethodPut, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasOne), map[string]any{"data": aliasData("deep admin updated alias")}, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(created[aliasOne], 10))
	})
	evidence["update"] = deepSafeObservation(updated)
	if err == nil && updateErr != nil {
		err = updateErr
	}
	if err == nil {
		err = deepExpectStatus(updated, http.StatusOK, "alias update")
	}
	if err == nil {
		created[aliasOne], err = deepResourceVersion(updated.Body)
	}
	stale, staleErr := e.deepCall(true, http.MethodPut, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasOne), map[string]any{"data": aliasData("stale revision")}, func(r *http.Request) {
		r.Header.Set("If-Match", "1")
	})
	evidence["stale_update"] = deepSafeObservation(stale)
	if err == nil {
		if staleErr != nil {
			err = staleErr
		} else {
			err = deepExpectStatus(stale, http.StatusPreconditionFailed, "stale alias update")
		}
	}
	pageOne, pageErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/model_aliases?limit=1", nil, nil)
	evidence["page_one"] = deepSafeObservation(pageOne)
	var page deepResourcePage
	if err == nil && pageErr != nil {
		err = pageErr
	}
	if err == nil {
		err = deepExpectStatus(pageOne, http.StatusOK, "alias first page")
	}
	if err == nil {
		if page, pageErr = deepPage(pageOne.Body); pageErr != nil || len(page.Items) != 1 || page.NextCursor == "" {
			err = errors.New("alias pagination omitted one item or a signed next cursor")
		}
	}
	seenIDs := map[string]struct{}{}
	foundIDs := map[string]bool{}
	scanPage := func(label string, current deepResourcePage) error {
		if len(current.Items) != 1 {
			return fmt.Errorf("%s expected exactly one item for limit=1, got %d", label, len(current.Items))
		}
		for _, item := range current.Items {
			if item.ID == "" {
				return fmt.Errorf("%s returned an item without an ID", label)
			}
			if _, duplicate := seenIDs[item.ID]; duplicate {
				return fmt.Errorf("%s repeated resource ID %q across cursors", label, item.ID)
			}
			seenIDs[item.ID] = struct{}{}
			if item.ID == aliasOne || item.ID == aliasTwo {
				foundIDs[item.ID] = true
			}
		}
		return nil
	}
	if err == nil {
		err = scanPage("alias first page", page)
	}
	nextCursor := page.NextCursor
	pageNumber := 2
	for err == nil && (pageNumber == 2 || !foundIDs[aliasOne] || !foundIDs[aliasTwo]) {
		if nextCursor == "" {
			err = errors.New("alias pagination ended before both created aliases were observed")
			break
		}
		cursorPath := "/admin/api/v1/model_aliases?limit=1&cursor=" + url.QueryEscape(nextCursor)
		next, nextErr := e.deepCall(true, http.MethodGet, cursorPath, nil, nil)
		if pageNumber == 2 {
			evidence["page_two"] = deepSafeObservation(next)
		} else {
			evidence[fmt.Sprintf("page_%d", pageNumber)] = deepSafeObservation(next)
		}
		if nextErr != nil {
			err = nextErr
			break
		}
		if err = deepExpectStatus(next, http.StatusOK, fmt.Sprintf("alias page %d", pageNumber)); err != nil {
			break
		}
		var nextPage deepResourcePage
		if nextPage, err = deepPage(next.Body); err != nil {
			err = fmt.Errorf("alias page %d was not valid JSON: %w", pageNumber, err)
			break
		}
		if nextPage.NextCursor != "" && nextPage.NextCursor == nextCursor {
			err = errors.New("alias pagination cursor did not advance")
			break
		}
		if err = scanPage(fmt.Sprintf("alias page %d", pageNumber), nextPage); err != nil {
			break
		}
		nextCursor = nextPage.NextCursor
		pageNumber++
		if pageNumber > 64 {
			err = errors.New("alias pagination exceeded the bounded cursor scan")
		}
	}
	if err == nil && (!foundIDs[aliasOne] || !foundIDs[aliasTwo]) {
		err = errors.New("alias pagination did not expose both created aliases")
	}
	invalidCursor, invalidCursorErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/model_aliases?limit=1&cursor=invalid-cursor", nil, nil)
	evidence["invalid_cursor"] = deepSafeObservation(invalidCursor)
	if err == nil {
		if invalidCursorErr != nil {
			err = invalidCursorErr
		} else {
			err = deepExpect4xx(invalidCursor, "invalid alias cursor")
		}
	}
	badID := s.id("alias-unknown-field")
	bad, badErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/model_aliases", map[string]any{"id": badID, "data": map[string]any{"model_ids": []string{"fixture-model"}, "enabled": true, "unknown_field": true}}, nil)
	evidence["unknown_field"] = deepSafeObservation(bad)
	if err == nil {
		if badErr != nil {
			err = badErr
		} else {
			err = deepExpect4xx(bad, "unknown alias field")
		}
	}
	badRead, badReadErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/model_aliases/"+url.PathEscape(badID), nil, nil)
	evidence["unknown_field_absent"] = deepSafeObservation(badRead)
	if err == nil {
		if badReadErr != nil {
			err = badReadErr
		} else {
			err = deepExpectStatus(badRead, http.StatusNotFound, "rejected alias visibility")
		}
	}
	deletedTwo, deletedTwoErr := e.deepCall(true, http.MethodDelete, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasTwo), nil, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(created[aliasTwo], 10))
	})
	evidence["delete_second"] = deepSafeObservation(deletedTwo)
	if err == nil && deletedTwoErr != nil {
		err = deletedTwoErr
	}
	if err == nil {
		err = deepExpect2xx(deletedTwo, "second alias delete")
		delete(created, aliasTwo)
	}
	deletedOne, deletedOneErr := e.deepCall(true, http.MethodDelete, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasOne), nil, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(created[aliasOne], 10))
	})
	evidence["delete_first"] = deepSafeObservation(deletedOne)
	if err == nil && deletedOneErr != nil {
		err = deletedOneErr
	}
	if err == nil {
		err = deepExpect2xx(deletedOne, "first alias delete")
		delete(created, aliasOne)
	}
	deletedRead, deletedReadErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasOne), nil, nil)
	evidence["deleted_read"] = deepSafeObservation(deletedRead)
	if err == nil {
		if deletedReadErr != nil {
			err = deletedReadErr
		} else {
			err = deepExpectStatus(deletedRead, http.StatusNotFound, "deleted alias read")
		}
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	if err == nil && e.fixture.count() != before {
		err = errors.New("management CRUD unexpectedly dispatched to the fixture upstream")
	}
	return deepAdminResult("admin/resource-versioned-crud", start, evidence, err)
}

func (e *environment) deepAdminCredentialProjection(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/credential-metadata-boundary", start, nil, err)
	}
	list, listErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/credentials?limit=200", nil, nil)
	evidence := map[string]any{"list": deepSafeObservation(list)}
	if listErr != nil {
		return deepAdminResult("admin/credential-metadata-boundary", start, evidence, listErr)
	}
	err := deepExpectStatus(list, http.StatusOK, "credential metadata list")
	var page deepResourcePage
	if err == nil {
		if json.Unmarshal(list.Body, &page) != nil {
			err = errors.New("credential list was not valid JSON")
		}
	}
	found := false
	if err == nil {
		for _, item := range page.Items {
			if item.ID == "fixture-connection" {
				found = true
			}
		}
		if !found {
			err = errors.New("credential metadata list omitted the seeded connection")
		}
	}
	if err == nil {
		err = deepAssertNoSecret(list.Body, "credential metadata list", s.fixtureSecret, e.key, e.cookie, e.csrf)
	}
	detail, detailErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/credentials/fixture-connection", nil, nil)
	evidence["detail"] = deepSafeObservation(detail)
	if err == nil && detailErr != nil {
		err = detailErr
	}
	if err == nil {
		err = deepExpectStatus(detail, http.StatusOK, "credential metadata detail")
	}
	if err == nil {
		err = deepAssertNoSecret(detail.Body, "credential metadata detail", s.fixtureSecret, e.key, e.cookie, e.csrf)
	}
	if err == nil {
		err = deepAssertNoCredentialFields(detail.Body, "credential metadata detail")
	}
	genericID := s.id("generic-credential")
	generic, genericErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/credentials", map[string]any{"id": genericID, "data": map[string]any{"provider": "anthropic", "account_id": "not-used", "secret": "deep-generic-secret"}}, nil)
	evidence["generic_crud_reject"] = deepSafeObservation(generic)
	if err == nil {
		if genericErr != nil {
			err = genericErr
		} else {
			err = deepExpect4xx(generic, "generic credential CRUD")
		}
	}
	genericRead, genericReadErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/credentials/"+url.PathEscape(genericID), nil, nil)
	evidence["generic_absent"] = deepSafeObservation(genericRead)
	if err == nil {
		if genericReadErr != nil {
			err = genericReadErr
		} else {
			err = deepExpectStatus(genericRead, http.StatusNotFound, "generic credential resource")
		}
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	if err == nil && e.fixture.count() != before {
		err = errors.New("credential metadata reads dispatched to the fixture upstream")
	}
	return deepAdminResult("admin/credential-metadata-boundary", start, evidence, err)
}

func (e *environment) deepAdminTenantIsolation(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/tenant-isolation", start, nil, err)
	}
	tenantID := s.id("tenant")
	create, createErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/tenants", map[string]any{"id": tenantID, "data": map[string]any{"name": "Deep isolated tenant", "enabled": true}}, nil)
	evidence := map[string]any{"create_tenant": deepSafeObservation(create)}
	if createErr != nil {
		return deepAdminResult("admin/tenant-isolation", start, evidence, createErr)
	}
	err := deepExpect2xx(create, "tenant create")
	if err == nil {
		err = e.deepSelectTenant(tenantID)
	}
	selected, selectedObs, selectedErr := e.deepSessionRead()
	evidence["selected_session"] = deepSafeObservation(selectedObs)
	if err == nil && selectedErr != nil {
		err = selectedErr
	}
	if err == nil && selected.Principal.TenantID != tenantID {
		err = errors.New("new tenant was not selected in the durable session")
	}
	fixtureRead, fixtureReadErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/connections/fixture-connection", nil, nil)
	evidence["cross_tenant_connection"] = deepSafeObservation(fixtureRead)
	if err == nil {
		if fixtureReadErr != nil {
			err = fixtureReadErr
		} else {
			err = deepExpectStatus(fixtureRead, http.StatusNotFound, "cross-tenant connection")
		}
	}
	credentialRead, credentialReadErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/credentials/fixture-connection", nil, nil)
	evidence["cross_tenant_credential"] = deepSafeObservation(credentialRead)
	if err == nil {
		if credentialReadErr != nil {
			err = credentialReadErr
		} else {
			err = deepExpectStatus(credentialRead, http.StatusNotFound, "cross-tenant credential")
		}
	}
	newConnID := s.id("isolated-connection")
	newConn, newConnErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections", map[string]any{"id": newConnID, "data": map[string]any{"connector": "anthropic", "account_id": newConnID + "-account", "base_url": e.fixture.server.URL, "dedicated": true, "enabled": true, "settings": map[string]string{"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}}}, nil)
	evidence["tenant_local_create"] = deepSafeObservation(newConn)
	if err == nil && newConnErr != nil {
		err = newConnErr
	}
	if err == nil {
		err = deepExpect2xx(newConn, "tenant-local connection create")
	}
	newConnVersion, versionErr := deepResourceVersion(newConn.Body)
	if err == nil && versionErr != nil {
		err = versionErr
	}
	if newConnVersion > 0 {
		deleted, _ := e.deepCall(true, http.MethodDelete, "/admin/api/v1/connections/"+url.PathEscape(newConnID), nil, func(r *http.Request) {
			r.Header.Set("If-Match", strconv.FormatInt(newConnVersion, 10))
		})
		evidence["tenant_local_delete"] = deepSafeObservation(deleted)
		if err == nil {
			err = deepExpect2xx(deleted, "tenant-local connection delete")
		}
	}
	if selectErr := e.deepSelectTenant(s.originalTenant); err == nil && selectErr != nil {
		err = selectErr
	}
	back, backErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/connections/fixture-connection", nil, nil)
	evidence["original_tenant_connection"] = deepSafeObservation(back)
	if err == nil {
		if backErr != nil {
			err = backErr
		} else {
			err = deepExpectStatus(back, http.StatusOK, "original tenant connection")
		}
	}
	unknownTenant, unknownTenantErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/session/tenant", map[string]any{"tenant_id": s.id("unknown-tenant")}, nil)
	evidence["unknown_tenant_selection"] = deepSafeObservation(unknownTenant)
	if err == nil {
		if unknownTenantErr != nil {
			err = unknownTenantErr
		} else {
			err = deepExpect4xx(unknownTenant, "unknown tenant selection")
		}
	}
	current, currentObs, currentErr := e.deepSessionRead()
	evidence["post_rejection_session"] = deepSafeObservation(currentObs)
	if err == nil && currentErr != nil {
		err = currentErr
	}
	if err == nil && current.Principal.TenantID != s.originalTenant {
		err = errors.New("rejected tenant selection changed the active session")
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	if err == nil && e.fixture.count() != before {
		err = errors.New("tenant management requests dispatched to the fixture upstream")
	}
	return deepAdminResult("admin/tenant-isolation", start, evidence, err)
}

func (e *environment) deepAdminConnectionLifecycle(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/connection-lifecycle", start, nil, err)
	}
	connectionID := s.id("connection")
	accountID := connectionID + "-account"
	created := false
	version := int64(0)
	imported := false
	defer func() {
		if imported {
			_, _ = e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/revoke-credential", map[string]any{"data": map[string]any{"provider": "anthropic", "account_id": accountID, "credential_version": 1}}, func(r *http.Request) {
				if version > 0 {
					r.Header.Set("If-Match", strconv.FormatInt(version, 10))
				}
			})
		}
		if created && version > 0 {
			_, _ = e.deepCall(true, http.MethodDelete, "/admin/api/v1/connections/"+url.PathEscape(connectionID), nil, func(r *http.Request) {
				r.Header.Set("If-Match", strconv.FormatInt(version, 10))
			})
		}
	}()
	create, createErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections", map[string]any{"id": connectionID, "data": map[string]any{"connector": "anthropic", "account_id": accountID, "base_url": e.fixture.server.URL, "dedicated": true, "enabled": true, "settings": map[string]string{"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}}}, nil)
	evidence := map[string]any{"create": deepSafeObservation(create)}
	err := createErr
	if err == nil {
		err = deepExpect2xx(create, "connection create")
	}
	if err == nil {
		created = true
		version, err = deepResourceVersion(create.Body)
	}
	secret := s.fixtureSecret
	if err == nil {
		importedResponse, importErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/import", map[string]any{"data": map[string]any{"provider": "anthropic", "account_id": accountID, "kind": "api_key", "secret": secret}}, func(r *http.Request) {
			r.Header.Set("If-Match", strconv.FormatInt(version, 10))
		})
		evidence["import"] = deepSafeObservation(importedResponse)
		if importErr != nil {
			err = importErr
		} else {
			err = deepExpect2xx(importedResponse, "connection credential import")
		}
		if err == nil {
			imported = true
		}
	}
	status, statusErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/status", map[string]any{"data": map[string]any{"provider": "anthropic", "account_id": accountID}}, nil)
	evidence["status"] = deepSafeObservation(status)
	if err == nil && statusErr != nil {
		err = statusErr
	}
	if err == nil {
		err = deepExpect2xx(status, "connection status")
	}
	capabilities, capabilitiesErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/capabilities", nil, nil)
	evidence["capabilities"] = deepSafeObservation(capabilities)
	if err == nil && capabilitiesErr != nil {
		err = capabilitiesErr
	}
	if err == nil {
		err = deepExpectStatus(capabilities, http.StatusOK, "connection capabilities")
	}
	if err == nil {
		var inventory struct {
			ConnectionID string `json:"connection_id"`
			Endpoints    []any  `json:"endpoints"`
		}
		if json.Unmarshal(capabilities.Body, &inventory) != nil || inventory.ConnectionID != connectionID || len(inventory.Endpoints) == 0 {
			err = errors.New("capability inventory omitted the connection or endpoint set")
		}
	}
	discoveryBefore := e.fixture.count()
	discovery, discoveryErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/discover", map[string]any{"data": map[string]any{}}, nil)
	evidence["discover"] = deepSafeObservation(discovery)
	if err == nil && discoveryErr != nil {
		err = discoveryErr
	}
	if err == nil {
		err = deepExpectStatus(discovery, http.StatusOK, "connection discovery")
	}
	if err == nil {
		var discovered struct {
			ConnectionID string `json:"connection_id"`
			Candidates   []struct {
				ID string `json:"id"`
			} `json:"candidates"`
			Approved bool `json:"approved"`
		}
		if json.Unmarshal(discovery.Body, &discovered) != nil || discovered.ConnectionID != connectionID || discovered.Approved || len(discovered.Candidates) == 0 || discovered.Candidates[0].ID != "fixture-chat" {
			err = errors.New("discovery did not return an unapproved fixture candidate")
		}
	}
	if err == nil {
		if e.fixture.count()-discoveryBefore != 1 {
			err = fmt.Errorf("connection discovery expected one upstream call, got %d", e.fixture.count()-discoveryBefore)
		} else {
			last := e.fixture.last()
			if last.Path != "/models" || last.CredentialHeader != "X-Api-Key" || last.CredentialConflict || last.CredentialCount != 1 {
				err = errors.New("discovery did not use the configured single upstream credential")
			}
		}
	}
	testBefore := e.fixture.count()
	test, testErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/test", map[string]any{"data": map[string]any{}}, nil)
	evidence["test"] = deepSafeObservation(test)
	if err == nil {
		if testErr != nil {
			err = testErr
		} else if test.Status != http.StatusOK {
			err = fmt.Errorf("connection test returned HTTP %d", test.Status)
		} else if e.fixture.count()-testBefore != 1 {
			err = errors.New("connection test did not make exactly one configured upstream request")
		}
	}
	disabled, disabledErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/disable", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(version, 10))
	})
	evidence["disable"] = deepSafeObservation(disabled)
	if err == nil && disabledErr != nil {
		err = disabledErr
	}
	if err == nil {
		err = deepExpect2xx(disabled, "connection disable")
	}
	if err == nil {
		version, err = deepResourceVersion(disabled.Body)
	}
	readDisabled, readDisabledErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/connections/"+url.PathEscape(connectionID), nil, nil)
	evidence["disabled_read"] = deepSafeObservation(readDisabled)
	if err == nil && readDisabledErr != nil {
		err = readDisabledErr
	}
	if err == nil {
		if err = deepExpectStatus(readDisabled, http.StatusOK, "disabled connection read"); err == nil {
			var decoded struct {
				Data struct {
					Enabled bool `json:"enabled"`
				} `json:"data"`
			}
			if json.Unmarshal(readDisabled.Body, &decoded) != nil || decoded.Data.Enabled {
				err = errors.New("disable action did not persist enabled=false")
			}
		}
	}
	postDisableBefore := e.fixture.count()
	postDisable, postDisableErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/discover", map[string]any{"data": map[string]any{}}, nil)
	evidence["disabled_discover"] = deepSafeObservation(postDisable)
	if err == nil {
		if postDisableErr != nil {
			err = postDisableErr
		} else if postDisable.Status < 400 || e.fixture.count() != postDisableBefore {
			err = errors.New("disabled connection discovery was not rejected before dispatch")
		}
	}
	if imported && version > 0 {
		revoked, revokedErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections/"+url.PathEscape(connectionID)+"/revoke-credential", map[string]any{"data": map[string]any{"provider": "anthropic", "account_id": accountID, "credential_version": 1}}, func(r *http.Request) {
			r.Header.Set("If-Match", strconv.FormatInt(version, 10))
		})
		evidence["revoke_credential"] = deepSafeObservation(revoked)
		if err == nil && revokedErr != nil {
			err = revokedErr
		}
		if err == nil {
			err = deepExpect2xx(revoked, "connection credential revoke")
		}
		if err == nil {
			imported = false
		}
	}
	deleted, deletedErr := e.deepCall(true, http.MethodDelete, "/admin/api/v1/connections/"+url.PathEscape(connectionID), nil, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(version, 10))
	})
	evidence["delete"] = deepSafeObservation(deleted)
	if err == nil && deletedErr != nil {
		err = deletedErr
	}
	if err == nil {
		err = deepExpect2xx(deleted, "connection delete")
		if err == nil {
			created = false
		}
	}
	if err == nil {
		err = deepAssertNoSecret(discovery.Body, "discovery response", secret)
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	return deepAdminResult("admin/connection-lifecycle", start, evidence, err)
}

func (e *environment) deepAdminRouteDryRun(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/route-dry-run", start, nil, err)
	}
	body := map[string]any{"data": map[string]any{"alias": "assistant", "operation": "generate", "input_modalities": []string{"text"}, "output_modalities": []string{"text"}}}
	dryRun, dryRunErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/route_policies/assistant/dry-run", body, nil)
	evidence := map[string]any{"dry_run": deepSafeObservation(dryRun)}
	err := dryRunErr
	if err == nil {
		err = deepExpectStatus(dryRun, http.StatusOK, "route dry-run")
	}
	if err == nil {
		var response struct {
			Alias    string `json:"alias"`
			Revision int64  `json:"revision"`
			DryRun   bool   `json:"dry_run"`
			Targets  []any  `json:"targets"`
		}
		if json.Unmarshal(dryRun.Body, &response) != nil || response.Alias != "assistant" || response.Revision < 1 || !response.DryRun || len(response.Targets) == 0 {
			err = errors.New("route dry-run omitted the compiled revision or target explanation")
		}
	}
	invalid, invalidErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/route_policies/assistant/dry-run", map[string]any{"data": map[string]any{"operation": "generate", "unknown_field": true}}, nil)
	evidence["invalid_action"] = deepSafeObservation(invalid)
	if err == nil {
		if invalidErr != nil {
			err = invalidErr
		} else {
			err = deepExpect4xx(invalid, "invalid route dry-run input")
		}
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	if err == nil && e.fixture.count() != before {
		err = errors.New("route dry-run dispatched to the fixture upstream")
	}
	return deepAdminResult("admin/route-dry-run", start, evidence, err)
}

func (e *environment) deepAdminAPIKeyLifecycle(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/api-key-lifecycle", start, nil, err)
	}
	keyID := s.id("key")
	grant := map[string]any{"name": "deep admin lifecycle key", "role": "operator", "permissions": []string{"inference:invoke", "connection:fixture-connection"}, "aliases": []string{"assistant"}, "connections": []string{"fixture-connection"}, "operations": []string{"generate"}, "portable": true, "native_account": false, "realtime": false}
	active := false
	version := int64(0)
	var token, rotated string
	defer func() {
		if active && version > 0 {
			_, _ = e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/revoke", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
				r.Header.Set("If-Match", strconv.FormatInt(version, 10))
			})
		}
	}()
	issued, issueErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/issue", map[string]any{"data": grant}, nil)
	evidence := map[string]any{"issue": deepSafeObservation(issued)}
	err := issueErr
	if err == nil {
		err = deepExpectStatus(issued, http.StatusOK, "API key issue")
	}
	var issuedData struct {
		Resource struct {
			Version int64 `json:"version"`
		} `json:"resource"`
		Token string `json:"token"`
	}
	if err == nil {
		if json.Unmarshal(issued.Body, &issuedData) != nil || issuedData.Token == "" || issuedData.Resource.Version < 1 {
			err = errors.New("API key issue omitted one-time token or resource version")
		} else {
			token, version, active = issuedData.Token, issuedData.Resource.Version, true
		}
	}
	metadata, metadataErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/api_keys/"+url.PathEscape(keyID), nil, nil)
	evidence["metadata"] = deepSafeObservation(metadata)
	if err == nil && metadataErr != nil {
		err = metadataErr
	}
	if err == nil {
		err = deepExpectStatus(metadata, http.StatusOK, "API key metadata")
	}
	if err == nil {
		if bytes.Contains(metadata.Body, []byte(token)) {
			err = errors.New("API key metadata returned the one-time token")
		} else {
			err = deepAssertNoCredentialFields(metadata.Body, "API key metadata")
		}
	}
	invoke := func(secret string) (deepRawObservation, error) {
		body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"deep admin key"}],"max_tokens":8}`)
		return e.deepCall(false, http.MethodPost, "/v1/chat/completions", body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
		})
	}
	firstInvoke, firstInvokeErr := invoke(token)
	evidence["initial_inference"] = deepSafeObservation(firstInvoke)
	if err == nil && firstInvokeErr != nil {
		err = firstInvokeErr
	}
	if err == nil && (firstInvoke.Status != http.StatusOK || firstInvoke.UpstreamCalls != 1) {
		err = fmt.Errorf("issued API key expected one successful fixture dispatch, got HTTP %d and %d calls", firstInvoke.Status, firstInvoke.UpstreamCalls)
	}
	rotatedResponse, rotateErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/rotate", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(version, 10))
	})
	evidence["rotate"] = deepSafeObservation(rotatedResponse)
	if err == nil && rotateErr != nil {
		err = rotateErr
	}
	var rotatedData struct {
		Resource struct {
			Version int64 `json:"version"`
		} `json:"resource"`
		Token string `json:"token"`
	}
	if err == nil {
		if err = deepExpectStatus(rotatedResponse, http.StatusOK, "API key rotate"); err == nil && (json.Unmarshal(rotatedResponse.Body, &rotatedData) != nil || rotatedData.Token == "" || rotatedData.Resource.Version != version+1) {
			err = errors.New("API key rotate omitted the next version or one-time token")
		}
	}
	if err == nil {
		rotated, version = rotatedData.Token, rotatedData.Resource.Version
	}
	oldAfterRotate, oldAfterRotateErr := invoke(token)
	evidence["old_after_rotate"] = deepSafeObservation(oldAfterRotate)
	if err == nil {
		if oldAfterRotateErr != nil {
			err = oldAfterRotateErr
		} else if oldAfterRotate.Status != http.StatusUnauthorized || oldAfterRotate.UpstreamCalls != 0 {
			err = errors.New("rotated API key remained usable or dispatched after rejection")
		}
	}
	newAfterRotate, newAfterRotateErr := invoke(rotated)
	evidence["new_after_rotate"] = deepSafeObservation(newAfterRotate)
	if err == nil {
		if newAfterRotateErr != nil {
			err = newAfterRotateErr
		} else if newAfterRotate.Status != http.StatusOK || newAfterRotate.UpstreamCalls != 1 {
			err = errors.New("rotated API key did not authorize exactly one fixture dispatch")
		}
	}
	staleRotate, staleRotateErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/rotate", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
		r.Header.Set("If-Match", "1")
	})
	evidence["stale_rotate"] = deepSafeObservation(staleRotate)
	if err == nil {
		if staleRotateErr != nil {
			err = staleRotateErr
		} else {
			err = deepExpectStatus(staleRotate, http.StatusPreconditionFailed, "stale API key rotate")
		}
	}
	staleRevoke, staleRevokeErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/revoke", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
		r.Header.Set("If-Match", "1")
	})
	evidence["stale_revoke"] = deepSafeObservation(staleRevoke)
	if err == nil {
		if staleRevokeErr != nil {
			err = staleRevokeErr
		} else {
			err = deepExpectStatus(staleRevoke, http.StatusPreconditionFailed, "stale API key revoke")
		}
	}
	invalidID := s.id("invalid-key")
	invalidIssue, invalidIssueErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(invalidID)+"/issue", map[string]any{"data": map[string]any{"name": "invalid operation", "role": "operator", "permissions": []string{"inference:invoke"}, "connections": []string{"fixture-connection"}, "operations": []string{"not-a-shipped-operation"}, "portable": true, "native_account": false, "realtime": false}}, nil)
	evidence["invalid_operation"] = deepSafeObservation(invalidIssue)
	if err == nil {
		if invalidIssueErr != nil {
			err = invalidIssueErr
		} else {
			err = deepExpect4xx(invalidIssue, "invalid API key operation")
		}
	}
	validRevoke, validRevokeErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/revoke", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
		r.Header.Set("If-Match", strconv.FormatInt(version, 10))
	})
	evidence["revoke"] = deepSafeObservation(validRevoke)
	if err == nil && validRevokeErr != nil {
		err = validRevokeErr
	}
	if err == nil {
		err = deepExpect2xx(validRevoke, "API key revoke")
	}
	if err == nil {
		active = false
	}
	afterRevoke, afterRevokeErr := invoke(rotated)
	evidence["after_revoke"] = deepSafeObservation(afterRevoke)
	if err == nil {
		if afterRevokeErr != nil {
			err = afterRevokeErr
		} else if afterRevoke.Status != http.StatusUnauthorized || afterRevoke.UpstreamCalls != 0 {
			err = errors.New("revoked API key remained usable or dispatched after rejection")
		}
	}
	deletedRead, deletedReadErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/api_keys/"+url.PathEscape(keyID), nil, nil)
	evidence["revoked_metadata"] = deepSafeObservation(deletedRead)
	if err == nil {
		if deletedReadErr != nil {
			err = deletedReadErr
		} else {
			err = deepExpectStatus(deletedRead, http.StatusNotFound, "revoked API key metadata")
		}
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	return deepAdminResult("admin/api-key-lifecycle", start, evidence, err)
}

func (e *environment) deepAdminConfigLifecycle(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/config-lifecycle", start, nil, err)
	}
	export, exportErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/config/export", nil, nil)
	evidence := map[string]any{"export": deepSafeObservation(export)}
	err := exportErr
	if err == nil {
		err = deepExpectStatus(export, http.StatusOK, "configuration export")
	}
	if err == nil {
		err = deepAssertNoSecret(export.Body, "configuration export", s.fixtureSecret, e.key, e.cookie, e.csrf)
	}
	var exportedRevision int64
	var exportedConfig []byte
	if err == nil {
		exportedRevision, exportedConfig, err = deepSnapshotState(export.Body)
	}
	if err == nil {
		diff, diffErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/config/diff", map[string]any{"expected_revision": exportedRevision, "config": map[string]any{"model_aliases": map[string]any{}}}, nil)
		evidence["diff"] = deepSafeObservation(diff)
		if diffErr != nil {
			err = diffErr
		} else {
			err = deepExpectStatus(diff, http.StatusOK, "configuration diff")
		}
	}
	secretCandidate, secretCandidateErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/config/apply", map[string]any{"expected_revision": exportedRevision, "config": map[string]any{"secret": "deep-config-secret"}, "prune": false}, nil)
	evidence["secret_candidate"] = deepSafeObservation(secretCandidate)
	if err == nil {
		if secretCandidateErr != nil {
			err = secretCandidateErr
		} else {
			err = deepExpect4xx(secretCandidate, "secret-bearing configuration candidate")
		}
	}
	conflict, conflictErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/config/apply", map[string]any{"expected_revision": exportedRevision + 1, "config": map[string]any{}, "prune": false}, nil)
	evidence["revision_conflict"] = deepSafeObservation(conflict)
	if err == nil {
		if conflictErr != nil {
			err = conflictErr
		} else if conflict.Status != http.StatusConflict || deepProblemCode(conflict) != "configuration_rejected" {
			err = fmt.Errorf("configuration revision conflict expected HTTP 409 configuration_rejected, got HTTP %d (%s)", conflict.Status, deepProblemCode(conflict))
		}
	}
	verifyExport, verifyExportErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/config/export", nil, nil)
	evidence["post_rejection_export"] = deepSafeObservation(verifyExport)
	if err == nil && verifyExportErr != nil {
		err = verifyExportErr
	}
	var verifiedRevision int64
	var verifiedConfig []byte
	if err == nil {
		if err = deepExpectStatus(verifyExport, http.StatusOK, "post-rejection configuration export"); err == nil {
			verifiedRevision, verifiedConfig, err = deepSnapshotState(verifyExport.Body)
		}
	}
	if err == nil && (verifiedRevision != exportedRevision || !bytes.Equal(verifiedConfig, exportedConfig)) {
		err = errors.New("rejected configuration apply changed exported revision or configuration")
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	if err == nil && e.fixture.count() != before {
		err = errors.New("configuration management requests dispatched to the fixture upstream")
	}
	return deepAdminResult("admin/config-lifecycle", start, evidence, err)
}

func (e *environment) deepAdminAdminTokenLifecycle(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/admin-token-lifecycle", start, nil, err)
	}
	session, sessionObs, sessionErr := e.deepSessionRead()
	evidence := map[string]any{"session": deepSafeObservation(sessionObs)}
	if sessionErr != nil {
		return deepAdminResult("admin/admin-token-lifecycle", start, evidence, sessionErr)
	}
	expires := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano)
	issued, issueErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/auth/tokens", map[string]any{"subject": session.Principal.SubjectID, "permissions": []string{"connection:read"}, "expires_at": expires}, nil)
	evidence["issue"] = deepSafeObservation(issued)
	err := issueErr
	if err == nil {
		err = deepExpectStatus(issued, http.StatusOK, "admin token issue")
	}
	var tokenData struct {
		TokenHash string `json:"token_hash"`
		Secret    string `json:"secret"`
	}
	if err == nil {
		if json.Unmarshal(issued.Body, &tokenData) != nil || len(tokenData.TokenHash) != 64 || tokenData.Secret == "" {
			err = errors.New("admin token issue omitted a one-time secret or hash")
		}
	}
	if err == nil {
		read, readErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/connections", nil, func(r *http.Request) {
			r.Header.Del("Cookie")
			r.Header.Del("X-CSRF-Token")
			r.Header.Set("Authorization", "Bearer "+tokenData.Secret)
		})
		evidence["bearer_read"] = deepSafeObservation(read)
		if readErr != nil {
			err = readErr
		} else {
			err = deepExpectStatus(read, http.StatusOK, "scoped admin bearer read")
		}
	}
	if err == nil {
		write, writeErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/connections", map[string]any{"id": s.id("bearer-write-denied"), "data": map[string]any{"connector": "anthropic", "account_id": "bearer-denied", "base_url": e.fixture.server.URL, "dedicated": true, "enabled": true}}, func(r *http.Request) {
			r.Header.Del("Cookie")
			r.Header.Del("X-CSRF-Token")
			r.Header.Set("Authorization", "Bearer "+tokenData.Secret)
		})
		evidence["bearer_write"] = deepSafeObservation(write)
		if writeErr != nil {
			err = writeErr
		} else {
			err = deepExpectStatus(write, http.StatusForbidden, "scoped admin bearer write")
		}
	}
	revoked, revokedErr := e.deepCall(true, http.MethodDelete, "/admin/api/v1/auth/tokens/"+url.PathEscape(tokenData.TokenHash), nil, nil)
	evidence["revoke"] = deepSafeObservation(revoked)
	if err == nil && revokedErr != nil {
		err = revokedErr
	}
	if err == nil {
		err = deepExpect2xx(revoked, "admin token revoke")
	}
	postRevoke, postRevokeErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/connections", nil, func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Del("X-CSRF-Token")
		r.Header.Set("Authorization", "Bearer "+tokenData.Secret)
	})
	evidence["bearer_after_revoke"] = deepSafeObservation(postRevoke)
	if err == nil {
		if postRevokeErr != nil {
			err = postRevokeErr
		} else {
			err = deepExpectStatus(postRevoke, http.StatusUnauthorized, "revoked admin bearer")
		}
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	return deepAdminResult("admin/admin-token-lifecycle", start, evidence, err)
}

func (e *environment) deepAdminAuditRedaction(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/audit-redaction", start, nil, err)
	}
	audit, auditErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/audit_events?limit=200", nil, nil)
	evidence := map[string]any{"audit": deepSafeObservation(audit)}
	err := auditErr
	if err == nil {
		err = deepExpectStatus(audit, http.StatusOK, "audit event projection")
	}
	if err == nil {
		err = deepAssertNoSecret(audit.Body, "audit event projection", s.fixtureSecret, "deep-admin-upstream-secret", "deep-generic-secret", "deep-config-secret", e.key, e.cookie, e.csrf)
	}
	if err == nil {
		err = deepAssertNoCredentialFields(audit.Body, "audit event projection")
	}
	actions := map[string]bool{}
	keyVersions := map[string]int64{}
	keySeen := map[string]bool{}
	auditCursor := ""
	pageNumber := 1
	for err == nil {
		var page struct {
			Items []struct {
				Data struct {
					Action          string `json:"action"`
					Kind            string `json:"kind"`
					ResourceID      string `json:"resource_id"`
					ResourceVersion int64  `json:"resource_version"`
				} `json:"data"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if json.Unmarshal(audit.Body, &page) != nil {
			err = fmt.Errorf("audit page %d was not valid JSON", pageNumber)
			break
		}
		if pageNumber == 1 && len(page.Items) == 0 {
			err = errors.New("audit event projection was empty after management mutations")
			break
		}
		for _, item := range page.Items {
			actions[item.Data.Action] = true
			if item.Data.Kind == "api_keys" && item.Data.ResourceID == s.id("key") {
				if keySeen[item.Data.Action] {
					err = fmt.Errorf("audit API-key action %q was duplicated", item.Data.Action)
					break
				}
				keySeen[item.Data.Action] = true
				keyVersions[item.Data.Action] = item.Data.ResourceVersion
			}
		}
		if err != nil {
			break
		}
		if keyVersions["issue"] == 1 && keyVersions["rotate"] == 2 && keyVersions["delete"] == 3 {
			break
		}
		if page.NextCursor == "" {
			err = errors.New("audit pagination ended before the unique API-key lifecycle history was observed")
			break
		}
		if page.NextCursor == auditCursor {
			err = errors.New("audit pagination cursor did not advance")
			break
		}
		auditCursor = page.NextCursor
		pageNumber++
		next, nextErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/audit_events?limit=200&cursor="+url.QueryEscape(auditCursor), nil, nil)
		evidence[fmt.Sprintf("audit_page_%d", pageNumber)] = deepSafeObservation(next)
		if nextErr != nil {
			err = nextErr
			break
		}
		if err = deepExpectStatus(next, http.StatusOK, fmt.Sprintf("audit page %d", pageNumber)); err != nil {
			break
		}
		if err = deepAssertNoSecret(next.Body, fmt.Sprintf("audit page %d", pageNumber), s.fixtureSecret, "deep-admin-upstream-secret", "deep-generic-secret", "deep-config-secret", e.key, e.cookie, e.csrf); err != nil {
			break
		}
		if err = deepAssertNoCredentialFields(next.Body, fmt.Sprintf("audit page %d", pageNumber)); err != nil {
			break
		}
		audit = next
		if pageNumber > 16 {
			err = errors.New("audit pagination exceeded the bounded cursor scan")
		}
	}
	if err == nil {
		if keyVersions["issue"] != 1 || keyVersions["rotate"] != 2 || keyVersions["delete"] != 3 {
			err = fmt.Errorf("audit omitted unique API-key issue/rotate/delete versions: %#v", keyVersions)
		}
	}
	evidence["event_count"] = len(actions)
	evidence["actions_seen"] = actions
	evidence["fixture_calls"] = e.fixture.count() - before
	if err == nil && e.fixture.count() != before {
		err = errors.New("audit management read dispatched to the fixture upstream")
	}
	return deepAdminResult("admin/audit-redaction", start, evidence, err)
}

func (e *environment) deepAdminAuditAuthorization(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/audit-operator-boundary", start, nil, err)
	}
	operatorID := s.id("operator")
	operatorCreated := false
	operatorTokenHash, operatorToken := "", ""
	defer func() {
		if operatorTokenHash != "" {
			_, _ = e.deepCall(true, http.MethodDelete, "/admin/api/v1/auth/tokens/"+url.PathEscape(operatorTokenHash), nil, nil)
		}
		if operatorCreated {
			_, _ = e.deepCall(true, http.MethodDelete, "/admin/api/v1/operators/"+url.PathEscape(operatorID), nil, func(r *http.Request) {
				r.Header.Set("If-Match", "1")
			})
		}
	}()
	ownerAudit, ownerAuditErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/audit_events?limit=1", nil, nil)
	evidence := map[string]any{"owner_read": deepSafeObservation(ownerAudit)}
	err := ownerAuditErr
	if err == nil {
		err = deepExpectStatus(ownerAudit, http.StatusOK, "owner audit read")
	}
	operator, operatorErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/operators", map[string]any{"id": operatorID, "data": map[string]any{"subject": operatorID, "issuer": "deep-admin-issuer", "identity_subject": operatorID + "-identity", "display_name": "Deep audit operator", "enabled": true}}, nil)
	evidence["operator_create"] = deepSafeObservation(operator)
	if err == nil && operatorErr != nil {
		err = operatorErr
	}
	if err == nil {
		err = deepExpect2xx(operator, "operator create")
	}
	if err == nil {
		operatorCreated = true
	}
	binding, bindingErr := e.deepCall(true, http.MethodPut, "/admin/api/v1/role_bindings/"+url.PathEscape(operatorID), map[string]any{"data": map[string]any{"subject": operatorID, "tenant_id": s.originalTenant, "role": "operator"}}, func(r *http.Request) {
		r.Header.Set("If-Match", "1")
	})
	evidence["operator_promote"] = deepSafeObservation(binding)
	if err == nil && bindingErr != nil {
		err = bindingErr
	}
	if err == nil {
		err = deepExpect2xx(binding, "operator role binding")
	}
	session, _, sessionErr := e.deepSessionRead()
	if err == nil && sessionErr != nil {
		err = sessionErr
	}
	if err == nil {
		expires := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano)
		tokenResponse, tokenErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/auth/tokens", map[string]any{"subject": operatorID, "permissions": []string{"usage:read"}, "expires_at": expires}, nil)
		evidence["operator_token"] = deepSafeObservation(tokenResponse)
		if tokenErr != nil {
			err = tokenErr
		} else if err = deepExpectStatus(tokenResponse, http.StatusOK, "operator scoped token issue"); err == nil {
			var tokenData struct {
				TokenHash string `json:"token_hash"`
				Secret    string `json:"secret"`
			}
			if json.Unmarshal(tokenResponse.Body, &tokenData) != nil || tokenData.TokenHash == "" || tokenData.Secret == "" {
				err = errors.New("operator token response omitted token coordinates")
			} else {
				operatorTokenHash, operatorToken = tokenData.TokenHash, tokenData.Secret
			}
		}
	}
	if err == nil {
		operatorAudit, operatorAuditErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/audit_events?limit=1", nil, func(r *http.Request) {
			r.Header.Del("Cookie")
			r.Header.Del("X-CSRF-Token")
			r.Header.Set("Authorization", "Bearer "+operatorToken)
		})
		evidence["operator_read"] = deepSafeObservation(operatorAudit)
		if operatorAuditErr != nil {
			err = operatorAuditErr
		} else if operatorAudit.Status != http.StatusForbidden {
			// This is deliberately an assertion against the current production
			// permission table: operators have usage:read, not audit:read.
			err = fmt.Errorf("operator with usage:read expected audit HTTP 403, got HTTP %d (%s)", operatorAudit.Status, deepProblemCode(operatorAudit))
		}
	}
	_ = session
	evidence["fixture_calls"] = e.fixture.count() - before
	return deepAdminResult("admin/audit-operator-boundary", start, evidence, err)
}

func (e *environment) deepAdminRestartPersistence(s deepAdminState) result {
	start := time.Now()
	before := e.fixture.count()
	if err := e.deepEnsureOriginal(s); err != nil {
		return deepAdminResult("admin/restart-persistence", start, nil, err)
	}
	aliasID, keyID := s.id("restart-alias"), s.id("restart-key")
	aliasCreated, keyActive := false, false
	aliasVersion := int64(0)
	keyToken := ""
	defer func() {
		if keyActive {
			_, _ = e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/revoke", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
				r.Header.Set("If-Match", "1")
			})
		}
		if aliasCreated {
			_, _ = e.deepCall(true, http.MethodDelete, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasID), nil, func(r *http.Request) {
				r.Header.Set("If-Match", strconv.FormatInt(aliasVersion, 10))
			})
		}
	}()
	alias, aliasErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/model_aliases", map[string]any{"id": aliasID, "data": map[string]any{"model_ids": []string{"fixture-model"}, "description": "restart durable alias", "enabled": true}}, nil)
	evidence := map[string]any{"alias_create": deepSafeObservation(alias)}
	err := aliasErr
	if err == nil {
		err = deepExpect2xx(alias, "restart alias create")
	}
	if err == nil {
		aliasCreated = true
		aliasVersion, err = deepResourceVersion(alias.Body)
	}
	grant := map[string]any{"name": "restart durable key", "role": "operator", "permissions": []string{"inference:invoke", "connection:fixture-connection"}, "aliases": []string{aliasID}, "connections": []string{"fixture-connection"}, "operations": []string{"generate"}, "portable": true, "native_account": false, "realtime": false}
	key, keyErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/issue", map[string]any{"data": grant}, nil)
	evidence["key_create"] = deepSafeObservation(key)
	if err == nil && keyErr != nil {
		err = keyErr
	}
	if err == nil {
		err = deepExpectStatus(key, http.StatusOK, "restart API key issue")
	}
	if err == nil {
		var issued struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(key.Body, &issued) != nil || issued.Token == "" {
			err = errors.New("restart API key omitted its one-time token")
		} else {
			keyToken, keyActive = issued.Token, true
		}
	}
	beforeInvoke, beforeInvokeErr := e.deepCall(false, http.MethodPost, "/v1/chat/completions", map[string]any{"model": aliasID, "messages": []map[string]string{{"role": "user", "content": "restart-before"}}, "max_tokens": 8}, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+keyToken)
	})
	evidence["inference_before_restart"] = deepSafeObservation(beforeInvoke)
	if err == nil {
		if beforeInvokeErr != nil {
			err = beforeInvokeErr
		} else if beforeInvoke.Status != http.StatusOK || beforeInvoke.UpstreamCalls != 1 || !bytes.Contains(beforeInvoke.Body, []byte(`"choices"`)) {
			err = errors.New("new alias API key did not produce one semantic pre-restart fixture response")
		}
	}
	if err == nil {
		if e.server == nil || e.server.Process == nil {
			err = errors.New("served process was not running for restart persistence")
		} else {
			_ = e.server.Process.Kill()
			_ = e.server.Wait()
			if restartErr := e.Start(context.Background(), e.binary); restartErr != nil {
				err = fmt.Errorf("restart failed: %v", restartErr)
			}
		}
	}
	ready, readyErr := e.deepCall(true, http.MethodGet, "/health/ready", nil, nil)
	evidence["ready_after_restart"] = deepSafeObservation(ready)
	if err == nil && readyErr != nil {
		err = readyErr
	}
	if err == nil {
		err = deepExpectStatus(ready, http.StatusOK, "readiness after restart")
	}
	persistedSession, persistedSessionObs, persistedSessionErr := e.deepSessionRead()
	evidence["session_after_restart"] = deepSafeObservation(persistedSessionObs)
	if err == nil && persistedSessionErr != nil {
		err = persistedSessionErr
	}
	if err == nil && persistedSession.Principal.TenantID != s.originalTenant {
		err = errors.New("durable session did not survive restart")
	}
	persistedAlias, persistedAliasErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasID), nil, nil)
	evidence["alias_after_restart"] = deepSafeObservation(persistedAlias)
	if err == nil && persistedAliasErr != nil {
		err = persistedAliasErr
	}
	if err == nil {
		var decoded struct {
			Version int64 `json:"version"`
			Data    struct {
				ModelIDs    []string `json:"model_ids"`
				Description string   `json:"description"`
				Enabled     bool     `json:"enabled"`
			} `json:"data"`
		}
		if json.Unmarshal(persistedAlias.Body, &decoded) != nil || decoded.Version != aliasVersion || !decoded.Data.Enabled || decoded.Data.Description != "restart durable alias" {
			err = errors.New("persisted alias data or version changed across restart")
		} else {
			foundModel := false
			for _, modelID := range decoded.Data.ModelIDs {
				if modelID == "fixture-model" {
					foundModel = true
					break
				}
			}
			if !foundModel {
				err = errors.New("persisted alias lost its fixture model target")
			}
		}
	}
	persistedKey, persistedKeyErr := e.deepCall(true, http.MethodGet, "/admin/api/v1/api_keys/"+url.PathEscape(keyID), nil, nil)
	evidence["key_after_restart"] = deepSafeObservation(persistedKey)
	if err == nil && persistedKeyErr != nil {
		err = persistedKeyErr
	}
	if err == nil {
		err = deepExpectStatus(persistedKey, http.StatusOK, "API key after restart")
	}
	if err == nil && bytes.Contains(persistedKey.Body, []byte(keyToken)) {
		err = errors.New("persisted API key metadata returned the one-time token")
	}
	if err == nil {
		invoke, invokeErr := e.deepCall(false, http.MethodPost, "/v1/chat/completions", map[string]any{"model": aliasID, "messages": []map[string]string{{"role": "user", "content": "restart-after"}}, "max_tokens": 8}, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+keyToken)
		})
		evidence["inference_after_restart"] = deepSafeObservation(invoke)
		if invokeErr != nil {
			err = invokeErr
		} else if invoke.Status != http.StatusOK || invoke.UpstreamCalls != 1 || !bytes.Contains(invoke.Body, []byte(`"choices"`)) {
			err = errors.New("persisted alias API key did not produce one semantic post-restart fixture response")
		}
	}
	if err == nil {
		if revoke, revokeErr := e.deepCall(true, http.MethodPost, "/admin/api/v1/api_keys/"+url.PathEscape(keyID)+"/revoke", map[string]any{"data": map[string]any{}}, func(r *http.Request) {
			r.Header.Set("If-Match", "1")
		}); revokeErr != nil {
			err = revokeErr
		} else {
			evidence["key_cleanup"] = deepSafeObservation(revoke)
			if cleanupErr := deepExpect2xx(revoke, "restart API key cleanup"); cleanupErr != nil {
				err = cleanupErr
			} else {
				keyActive = false
			}
		}
	}
	if err == nil {
		if deleted, deletedErr := e.deepCall(true, http.MethodDelete, "/admin/api/v1/model_aliases/"+url.PathEscape(aliasID), nil, func(r *http.Request) {
			r.Header.Set("If-Match", strconv.FormatInt(aliasVersion, 10))
		}); deletedErr != nil {
			err = deletedErr
		} else {
			evidence["alias_cleanup"] = deepSafeObservation(deleted)
			if cleanupErr := deepExpect2xx(deleted, "restart alias cleanup"); cleanupErr != nil {
				err = cleanupErr
			} else {
				aliasCreated = false
			}
		}
	}
	evidence["fixture_calls"] = e.fixture.count() - before
	return deepAdminResult("admin/restart-persistence", start, evidence, err)
}

func deepAdminUniqueID(prefix string) string {
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err == nil {
		return prefix + "-" + hex.EncodeToString(raw)
	}
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}
