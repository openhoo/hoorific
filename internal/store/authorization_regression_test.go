package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

func regressionConfigureKeyAdmission(t *testing.T, s *Store, owner core.Principal, tenant string, concurrency int) int64 {
	t.Helper()
	ctx := context.Background()
	expected, err := s.ConfigRevision(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(struct {
		Connections  map[string]admin.ConnectionData  `json:"connections"`
		PolicyLimits map[string]admin.PolicyLimitData `json:"policy_limits"`
	}{
		Connections: map[string]admin.ConnectionData{
			"conn": {
				Connector: "test",
				AccountID: "acct",
				BaseURL:   "https://upstream.example",
				Enabled:   true,
			},
		},
		PolicyLimits: map[string]admin.PolicyLimitData{
			"tenant-concurrency": {
				Scope:       "tenant",
				ScopeID:     tenant,
				Concurrency: concurrency,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyConfig(ctx, owner, expected, config, true); err != nil {
		t.Fatal(err)
	}
	next, err := s.ConfigRevision(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func regressionKeyMetadata(t *testing.T) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(keyData{
		Role:        "operator",
		Permissions: []string{"inference:invoke"},
		Connections: []string{"conn"},
		Operations:  []core.Operation{"generate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func regressionIssueKey(t *testing.T, s *Store, owner core.Principal, id string) (core.Resource, string) {
	t.Helper()
	issuer := owner
	issuer.Permissions = []string{"*"}
	resource, token, err := s.IssueKey(context.Background(), issuer, id, regressionKeyMetadata(t))
	if err != nil {
		t.Fatal(err)
	}
	return resource, token
}

func regressionKeyAttemptPlan(tenant, keyID string, keyRevision, configRevision int64, requestID, attemptID string) core.AttemptPlan {
	return core.AttemptPlan{
		TenantID:       tenant,
		RequestID:      requestID,
		AttemptID:      attemptID,
		KeyID:          keyID,
		KeyRevision:    keyRevision,
		ConnectionID:   "conn",
		AccountID:      "acct",
		ConfigRevision: configRevision,
		Operation:      "generate",
		Deadline:       time.Now().UTC().Add(time.Hour),
	}
}

func TestDisabledTenantRejectsExistingKeyAndSessionAdmission(t *testing.T) {
	ctx := context.Background()
	s := newTenancyAuthTestStore(t)
	owner := bootstrapTenant(t, s, "tenant", "owner")
	regressionConfigureKeyAdmission(t, s, owner, owner.TenantID, 1)
	key, token := regressionIssueKey(t, s, owner, "admission-key")

	const sessionHash = "disabled-tenant-session"
	if err := s.CreateSession(ctx, admin.Session{
		Hash:      sessionHash,
		Principal: owner,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateKey(ctx, token); err != nil {
		t.Fatalf("fresh key should authenticate before tenant disable: %v", err)
	}

	if _, err := s.Mutate(ctx, owner, core.Mutation{
		Kind:            "tenants",
		ID:              owner.TenantID,
		ExpectedVersion: 1,
		Data:            mustTenantJSON(t, owner.TenantID, false),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateKey(ctx, token); err == nil {
		t.Fatal("existing API key must not authenticate for a disabled tenant")
	}
	if _, err := s.ResolveSession(ctx, sessionHash); err == nil {
		t.Fatal("existing admin session must not resolve for a disabled tenant")
	}

	currentRevision, err := s.ConfigRevision(ctx, owner.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	keyPlan := regressionKeyAttemptPlan(owner.TenantID, key.ID, key.Version, currentRevision, "disabled-key-request", "disabled-key-attempt")
	if _, err = s.BeginAttempt(ctx, keyPlan); err == nil {
		t.Fatal("disabled tenant must reject a new admission through an existing API key")
	}

	sessionPrincipal := owner
	sessionPrincipal.SessionID = sessionHash
	sessionPlan := regressionKeyAttemptPlan(owner.TenantID, "", 0, currentRevision, "disabled-session-request", "disabled-session-attempt")
	if _, err = s.BeginAttempt(core.WithPrincipal(ctx, sessionPrincipal), sessionPlan); err == nil {
		t.Fatal("disabled tenant must reject a new admission through an existing admin session")
	}

	var attempts int
	if err = s.DB.QueryRowContext(ctx, s.Query("SELECT COUNT(1) FROM attempts WHERE tenant_id=?"), owner.TenantID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("rejected disabled-tenant admissions persisted %d attempts", attempts)
	}
	var reserved int64
	if err = s.DB.QueryRowContext(ctx, s.Query("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind='tenant' AND scope_id=? AND window_id='total' AND kind='concurrency'"), owner.TenantID, owner.TenantID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatalf("rejected disabled-tenant admissions changed concurrency hold to %d", reserved)
	}
}

func regressionMemberFixture(t *testing.T, s *Store, owner core.Principal) (core.Principal, core.Resource) {
	t.Helper()
	ctx := context.Background()
	const subject = "member"
	if _, err := s.Mutate(ctx, owner, operatorMutation(t, subject, "https://issuer.example", "member-oidc", true)); err != nil {
		t.Fatal(err)
	}
	binding, err := json.Marshal(admin.RoleBindingData{Subject: subject, TenantID: owner.TenantID, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, owner, core.Mutation{
		Kind:            "role_bindings",
		ID:              subject,
		ExpectedVersion: 1,
		Data:            binding,
	}); err != nil {
		t.Fatal(err)
	}
	member, err := s.ResolveMembership(ctx, owner.TenantID, subject)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := regressionIssueKey(t, s, owner, "member-existing-key")
	return member, key
}

func regressionDisableOrDemoteMember(t *testing.T, s *Store, owner core.Principal, state string) {
	t.Helper()
	ctx := context.Background()
	switch state {
	case "demoted":
		binding, err := json.Marshal(admin.RoleBindingData{Subject: "member", TenantID: owner.TenantID, Role: "viewer"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Mutate(ctx, owner, core.Mutation{
			Kind:            "role_bindings",
			ID:              "member",
			ExpectedVersion: 2,
			Data:            binding,
		}); err != nil {
			t.Fatal(err)
		}
	case "disabled":
		if _, err := s.Mutate(ctx, owner, core.Mutation{
			Kind:            "operators",
			ID:              "member",
			ExpectedVersion: 1,
			Data:            mustOperatorJSON(t, "member", "https://issuer.example", "member-oidc", false),
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown member state %q", state)
	}
}

func TestStaleDemotedOrDisabledMembershipCannotMutateStore(t *testing.T) {
	for _, state := range []string{"demoted", "disabled"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s := newTenancyAuthTestStore(t)
			owner := bootstrapTenant(t, s, "tenant", "owner")
			member, existingKey := regressionMemberFixture(t, s, owner)
			member.Permissions = []string{"*"}
			regressionDisableOrDemoteMember(t, s, owner, state)

			revision, err := s.ConfigRevision(ctx, owner.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ApplyConfig(ctx, member, revision, json.RawMessage(`{}`), false); err == nil {
				t.Fatal("stale membership must not apply configuration")
			}
			if _, _, err = s.IssueKey(ctx, member, "member-new-key", regressionKeyMetadata(t)); err == nil {
				t.Fatal("stale membership must not issue an API key")
			}
			if _, _, err = s.RotateKey(ctx, member, existingKey.ID, existingKey.Version); err == nil {
				t.Fatal("stale membership must not rotate an API key")
			}
			if err = s.RetainAudit(ctx, member, 1); err == nil {
				t.Fatal("stale membership must not retain or delete audit events")
			}

			maximum := int64(100)
			plan := core.AttemptPlan{
				TenantID:    owner.TenantID,
				RequestID:   "member-request",
				AttemptID:   "member-attempt",
				MaximumCost: &maximum,
				Allowances: []core.Allowance{{
					ScopeKind: "tenant", ScopeID: owner.TenantID, WindowID: "member-window", Kind: "cost", Maximum: maximum, Reserve: maximum,
				}},
			}
			insertRegressionAttemptFixture(t, s, plan, "outcome_unknown", []int64{maximum}, member.SubjectID)
			cost := int64(40)
			_, err = s.Reconcile(ctx, member, plan.AttemptID, 1, core.Reconciliation{
				ReconciliationID: "member-reconciliation",
				Mode:             "provider_evidence",
				Reason:           "provider event",
				SourceReference:  "provider-event",
				Cost:             &cost,
			})
			if err == nil {
				t.Fatal("stale membership must not reconcile an admission")
			}
			var reserved int64
			if err = s.DB.QueryRowContext(ctx, s.Query("SELECT reserved FROM allowances WHERE tenant_id=? AND scope_kind='tenant' AND scope_id=? AND window_id=? AND kind='cost'"), owner.TenantID, owner.TenantID, "member-window").Scan(&reserved); err != nil {
				t.Fatal(err)
			}
			if reserved != maximum {
				t.Fatalf("rejected reconciliation changed held cost to %d", reserved)
			}
			var attempts, ledger, reconciliations int
			if err = s.DB.QueryRowContext(ctx, s.Query("SELECT COUNT(1) FROM usage_ledger WHERE tenant_id=? AND attempt_id=?"), owner.TenantID, plan.AttemptID).Scan(&ledger); err != nil {
				t.Fatal(err)
			}
			if err = s.DB.QueryRowContext(ctx, s.Query("SELECT COUNT(1) FROM reconciliations WHERE tenant_id=? AND admission_id=?"), owner.TenantID, plan.AttemptID).Scan(&reconciliations); err != nil {
				t.Fatal(err)
			}
			if err = s.DB.QueryRowContext(ctx, s.Query("SELECT COUNT(1) FROM attempts WHERE tenant_id=? AND attempt_id=?"), owner.TenantID, plan.AttemptID).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || ledger != 0 || reconciliations != 0 {
				t.Fatalf("rejected reconciliation changed durable rows: attempts=%d ledger=%d reconciliations=%d", attempts, ledger, reconciliations)
			}
		})
	}
}

func mustTenantJSON(t *testing.T, name string, enabled bool) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(admin.TenantData{Name: name, Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
