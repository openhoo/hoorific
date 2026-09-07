package main

import (
	"context"
	"net/http"
	"reflect"
	"time"

	"hoorific/internal/credential"
)

const (
	refreshAdmissionQuotaName   = "credentials/refresh-admission-quota"
	refreshAdmissionAllowedName = "credentials/refresh-admission-allowed"
)

// refreshAdmissionScenarios runs an isolated configured gateway against an expired
// OAuth credential. The rejected half proves admission precedes refresh; the
// allowed half is the positive control for durable rotation and provider dispatch.
func (e *environment) refreshAdmissionScenarios() []result {
	if e == nil || e.binary == "" {
		return refreshAdmissionSetupFailure()
	}
	f, err := newCredentialLifecycle(e.binary, "")
	if err != nil {
		return refreshAdmissionSetupFailure()
	}
	defer f.close()
	return f.refreshAdmission()
}

func refreshAdmissionSetupFailure() []result {
	return []result{
		{Name: refreshAdmissionQuotaName, Status: "failed", Detail: "isolated refresh-admission fixture setup failed", Evidence: map[string]any{"isolated_fixture": false}},
		{Name: refreshAdmissionAllowedName, Status: "failed", Detail: "isolated refresh-admission fixture setup failed", Evidence: map[string]any{"isolated_fixture": false}},
	}
}

func (f *credentialLifecycleFixture) refreshAdmission() []result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	failed := func() []result {
		return []result{
			lifecycleResult(refreshAdmissionQuotaName, start, false, nil),
			lifecycleResult(refreshAdmissionAllowedName, start, false, nil),
		}
	}

	before, err := f.load(ctx)
	if err != nil {
		return failed()
	}
	manager, err := credential.NewManager(f.keys, f.db)
	if err != nil {
		return failed()
	}
	old := credential.OAuthToken{
		AccessToken:  "refresh-admission-old-access",
		RefreshToken: "lifecycle-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	if _, err = manager.Put(ctx, before.Identity, credential.Secret{OAuth: &old}); err != nil {
		return failed()
	}
	seeded, err := f.load(ctx)
	if err != nil || seeded.Identity.Version != before.Identity.Version+1 || !f.encrypted(ctx, seeded, old.AccessToken, old.RefreshToken) {
		return failed()
	}

	const policyID = "refresh-admission-quota"
	if err = f.e.extCreate("policy_limits", policyID, map[string]any{
		"scope":       "tenant",
		"scope_id":    f.e.tenantID,
		"max_cost":    int64(1),
		"cost_window": "total",
	}); err != nil {
		return failed()
	}

	rejectRefreshCalls := f.i.refreshCalls.Load()
	rejectDispatches := f.e.fixture.count()
	rejected, requestErr := f.e.extRequest(ctx, false, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"admission must reject before refresh"}],"max_tokens":8}`), nil)
	afterReject, loadErr := f.load(ctx)
	rejectedOK := requestErr == nil && rejected.Status == http.StatusTooManyRequests && rejected.ErrorCode == "quota_exceeded" && rejected.UpstreamCalls == 0 && f.i.refreshCalls.Load() == rejectRefreshCalls && f.e.fixture.count() == rejectDispatches && loadErr == nil && afterReject.Identity.Version == seeded.Identity.Version && reflect.DeepEqual(seeded, afterReject)
	rejectedResult := lifecycleResult(refreshAdmissionQuotaName, start, rejectedOK, map[string]any{
		"request_status":               rejected.Status,
		"request_error_code":           rejected.ErrorCode,
		"provider_token_calls":         f.i.refreshCalls.Load() - rejectRefreshCalls,
		"inference_dispatches":         f.e.fixture.count() - rejectDispatches,
		"credential_version_before":    seeded.Identity.Version,
		"credential_version_after":     afterReject.Identity.Version,
		"durable_credential_unchanged": loadErr == nil && reflect.DeepEqual(seeded, afterReject),
	})

	deleteObs, deleteErr := f.e.extRequest(ctx, true, http.MethodDelete, "/admin/api/v1/policy_limits/"+policyID, nil, func(r *http.Request) {
		r.Header.Set("If-Match", "1")
	})
	if deleteErr != nil || deleteObs.Status < 200 || deleteObs.Status >= 300 {
		return []result{rejectedResult, lifecycleResult(refreshAdmissionAllowedName, start, false, map[string]any{"policy_removed": false})}
	}

	allowedBefore, err := f.load(ctx)
	if err != nil {
		return []result{rejectedResult, lifecycleResult(refreshAdmissionAllowedName, start, false, map[string]any{"policy_removed": true})}
	}
	allowedRefreshCalls := f.i.refreshCalls.Load()
	allowedDispatches := f.e.fixture.count()
	allowed, allowedErr := f.e.extRequest(ctx, false, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"allowed request refreshes and dispatches"}],"max_tokens":8}`), nil)
	afterAllowed, allowedLoadErr := f.load(ctx)
	last := f.e.fixture.last()
	allowedOK := allowedErr == nil && allowed.Status == http.StatusOK && allowed.UpstreamCalls == 1 && f.i.refreshCalls.Load()-allowedRefreshCalls == 1 && f.e.fixture.count()-allowedDispatches == 1 && allowedLoadErr == nil && afterAllowed.Identity.Version == allowedBefore.Identity.Version+1 && f.encrypted(ctx, afterAllowed, "lifecycle-rotated-access", "lifecycle-rotated-refresh") && last.CredentialHeader == "Authorization" && last.CredentialCount == 1 && !last.CredentialConflict && last.Credential == "Bearer lifecycle-rotated-access"
	allowedResult := lifecycleResult(refreshAdmissionAllowedName, start, allowedOK, map[string]any{
		"policy_removed":                true,
		"request_status":                allowed.Status,
		"provider_token_calls":          f.i.refreshCalls.Load() - allowedRefreshCalls,
		"inference_dispatches":          f.e.fixture.count() - allowedDispatches,
		"credential_version_before":     allowedBefore.Identity.Version,
		"credential_version_after":      afterAllowed.Identity.Version,
		"durable_rotation_observed":     allowedLoadErr == nil && f.encrypted(ctx, afterAllowed, "lifecycle-rotated-access", "lifecycle-rotated-refresh"),
		"rotated_credential_dispatched": last.CredentialHeader == "Authorization" && last.CredentialCount == 1 && !last.CredentialConflict && last.Credential == "Bearer lifecycle-rotated-access",
	})
	return []result{rejectedResult, allowedResult}
}
