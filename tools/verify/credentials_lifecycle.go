package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hoorific/internal/core"
	"hoorific/internal/credential"
	"hoorific/internal/store"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// These qualifications launch a separate configured gateway, never modify the
// caller's fixture mode, and return only fixed diagnostics and numeric evidence.
func (e *environment) credentialLifecycle() []result {
	names := []string{"credentials/oauth-state-replay", "credentials/oauth-issuer", "credentials/oauth-audience", "credentials/refreshrace", "credentials/rotatedtoken-durable", "credentials/expiredlease-staleowner", "credentials/wrongaccount-import", "credentials/device-expiry", "credentials/subscription-optin-disabled"}
	start := time.Now()
	verificationProgress("credentials/lifecycle-setup", "started", 0)
	for _, name := range names {
		verificationProgress(name, "started", 0)
	}
	f, err := newCredentialLifecycle(e.binary, e.qualificationBinary)
	if err != nil {
		evidence := map[string]any{"phase": "isolation_setup", "error_class": "setup_failed"}
		out := make([]result, 0, len(names)+1)
		for _, name := range names {
			out = append(out, lifecycleResult(name, start, false, evidence))
		}
		out = append(out, lifecycleCrashBoundaryUnavailable(start, "credential lifecycle isolation setup failed"))
		return lifecycleAttachFailureArtifacts(out, "")
	}
	defer f.close()
	out := []result{f.oauth("valid"), f.oauth("issuer"), f.oauth("audience")}
	out = append(out, f.refreshRace()...)
	out = append(out, f.staleOwner(), f.wrongAccount(), f.deviceExpiry(), f.subscriptionDisabled(), f.crashBoundary())
	diagnostics := ""
	for _, item := range out {
		if item.Status == "failed" {
			diagnostics, _ = f.e.preserveDiagnostics("")
			break
		}
	}
	return lifecycleAttachFailureArtifacts(out, diagnostics)
}

func lifecycleAttachFailureArtifacts(out []result, diagnostics any) []result {
	if diagnostics == nil || diagnostics == "" {
		return out
	}
	for i := range out {
		if out[i].Status != "failed" {
			continue
		}
		evidence, ok := out[i].Evidence.(map[string]any)
		if !ok {
			evidence = map[string]any{}
			out[i].Evidence = evidence
		}
		evidence["diagnostics_dir"] = diagnostics
	}
	return out
}
func lifecycleCrashBoundaryUnavailable(start time.Time, detail string) result {
	return result{Name: "credentials/rotatedtoken-crashbeforepersist", Status: "failed", Detail: detail, DurationMS: time.Since(start).Milliseconds(), Evidence: map[string]any{"process_crash_exercised": false, "boundary": "credential.Refresher.refreshRecord -> Store.CommitRefresh"}}
}

type lifecycleCode struct{ nonce, challenge, variant string }
type lifecycleIssuer struct {
	server         *httptest.Server
	key            *rsa.PrivateKey
	mu             sync.Mutex
	codes          map[string]lifecycleCode
	requests       atomic.Int64
	refreshEntered chan struct{}
	refreshRelease chan struct{}
	refreshCalls   atomic.Int64
	codeCalls      atomic.Int64
	deviceCalls    atomic.Int64
	deviceStarts   atomic.Int64
	deviceSeconds  atomic.Int64
}

func newLifecycleIssuer() (*lifecycleIssuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	i := &lifecycleIssuer{key: key, codes: make(map[string]lifecycleCode)}
	i.deviceSeconds.Store(60)
	i.server = httptest.NewServer(http.HandlerFunc(i.serve))
	return i, nil
}

func (i *lifecycleIssuer) signed(nonce, variant string) (string, error) {
	issuer, audience := i.server.URL, "lifecycle-client"
	if variant == "issuer" {
		issuer += "/wrong"
	}
	if variant == "audience" {
		audience = "other-client"
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "lifecycle", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": audience, "sub": "fixture-account", "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	hash := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, hash[:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), err
}

func (i *lifecycleIssuer) serve(w http.ResponseWriter, r *http.Request) {
	i.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		respond(map[string]any{"issuer": i.server.URL, "authorization_endpoint": i.server.URL + "/authorize", "token_endpoint": i.server.URL + "/token", "jwks_uri": i.server.URL + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	case "/jwks":
		respond(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "lifecycle", "n": base64.RawURLEncoding.EncodeToString(i.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(i.key.E)).Bytes())}}})
	case "/device":
		i.deviceStarts.Add(1)
		respond(map[string]any{"device_code": "lifecycle-device-secret", "user_code": "LOCAL-ONLY", "verification_uri": i.server.URL + "/authorize", "expires_in": i.deviceSeconds.Load(), "interval": 1})
	case "/token":
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("client_id") != "lifecycle-client" {
			w.WriteHeader(400)
			respond(map[string]string{"error": "invalid_request"})
			return
		}
		nonce, variant, access, refresh := "", "valid", "lifecycle-access", "lifecycle-refresh"
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			i.codeCalls.Add(1)
			i.mu.Lock()
			c, ok := i.codes[r.Form.Get("code")]
			delete(i.codes, r.Form.Get("code"))
			i.mu.Unlock()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || c.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || r.Form.Get("redirect_uri") != i.server.URL+"/callback" {
				w.WriteHeader(400)
				respond(map[string]string{"error": "invalid_grant"})
				return
			}
			nonce, variant = c.nonce, c.variant
		case "refresh_token":
			i.refreshCalls.Add(1)
			if r.Form.Get("refresh_token") != "lifecycle-refresh" && r.Form.Get("refresh_token") != "lifecycle-crash-refresh" {
				w.WriteHeader(400)
				respond(map[string]string{"error": "invalid_grant"})
				return
			}
			i.mu.Lock()
			entered, release := i.refreshEntered, i.refreshRelease
			i.mu.Unlock()
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
			}
			if release != nil {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Second):
					w.WriteHeader(504)
					return
				}
			}
			access, refresh = "lifecycle-rotated-access", "lifecycle-rotated-refresh"
		case "urn:ietf:params:oauth:grant-type:device_code":
			i.deviceCalls.Add(1)
			if r.Form.Get("device_code") != "lifecycle-device-secret" {
				w.WriteHeader(400)
				respond(map[string]string{"error": "invalid_grant"})
				return
			}
		default:
			w.WriteHeader(400)
			respond(map[string]string{"error": "unsupported_grant_type"})
			return
		}
		token, err := i.signed(nonce, variant)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		respond(map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "id_token": token})
	default:
		w.WriteHeader(404)
	}
}

type credentialLifecycleFixture struct {
	e    *environment
	i    *lifecycleIssuer
	cfg  core.BootstrapConfig
	db   *store.Store
	keys credential.StaticKeyring
}

func newCredentialLifecycle(binary, qualificationBinary string) (_ *credentialLifecycleFixture, err error) {
	f := &credentialLifecycleFixture{}
	if binary == "" {
		return f, errors.New("gateway executable unavailable")
	}
	defer func() {
		if err != nil {
			f.close()
		}
	}()
	f.i, err = newLifecycleIssuer()
	if err != nil {
		return f, err
	}
	f.e, err = newEnvironment("standalone", "", "normal")
	if err != nil {
		return f, err
	}
	f.e.qualificationBinary = qualificationBinary
	raw, err := os.ReadFile(f.e.config)
	if err != nil {
		return f, err
	}
	if err = json.Unmarshal(raw, &f.cfg); err != nil {
		return f, err
	}
	u := f.i.server.URL
	f.cfg.OAuth = map[string]core.OAuthRegistration{"fixture-connection": {ClientID: "lifecycle-client", AuthorizationURL: u + "/authorize", TokenURL: u + "/token", DeviceURL: u + "/device", RedirectURL: u + "/callback", Issuer: u, Scopes: []string{"openid"}}}
	raw, err = json.Marshal(f.cfg)
	if err != nil {
		return f, err
	}
	if err = os.WriteFile(f.e.config, raw, 0600); err != nil {
		return f, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err = f.e.Start(ctx, binary); err != nil {
		return f, err
	}
	if f.e.bootstrap().Status != "passed" {
		return f, errors.New("isolated bootstrap failed")
	}
	f.keys, err = credential.LoadKeyring(f.cfg.Encryption.KeyFile)
	if err != nil {
		return f, err
	}
	f.db, err = store.Open(ctx, f.cfg)
	if err != nil {
		return f, err
	}
	return f, nil
}

func (f *credentialLifecycleFixture) close() {
	// Release upstream waits before waiting for process shutdown.
	if f.i != nil {
		f.i.mu.Lock()
		if ch := f.i.refreshRelease; ch != nil {
			select {
			case <-ch:
			default:
				close(ch)
			}
			f.i.refreshRelease = nil
		}
		f.i.mu.Unlock()
	}
	if f.db != nil {
		_ = f.db.Close()
	}
	if f.e != nil {
		f.e.Close()
	}
	if f.i != nil {
		f.i.server.CloseClientConnections()
		f.i.server.Close()
	}
}

func lifecycleResult(name string, start time.Time, ok bool, evidence map[string]any) result {
	var err error
	if !ok {
		err = errors.New("credential lifecycle invariant failed; raw errors and secrets withheld")
	}
	r := extResult(name, start, evidence, err)
	if ok {
		r.Detail = "observed isolated configured gateway and durable SQL credential invariants"
	}
	return r
}

func (f *credentialLifecycleFixture) load(ctx context.Context) (credential.Record, error) {
	return f.db.LoadCredential(ctx, f.e.tenantID, "fixture-connection")
}

func (f *credentialLifecycleFixture) action(ctx context.Context, id, action string, data map[string]any) (extObservation, error) {
	raw, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return extObservation{}, err
	}
	obs, err := f.e.extRequest(ctx, true, http.MethodPost, "/admin/api/v1/connections/"+id+"/"+action, raw, func(r *http.Request) { r.Header.Set("If-Match", "1") })
	if obs.ErrorCode == "" {
		obs.ErrorCode = lifecycleBodyErrorCode(obs.Body)
	}
	return obs, err
}

func lifecycleBodyErrorCode(body string) string {
	var envelope struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil {
		return ""
	}
	switch envelope.Code {
	case "credential_action_failed", "credential_identity_mismatch", "unsupported_operation":
		return envelope.Code
	default:
		return ""
	}
}

func lifecycleInput(version int64) map[string]any {
	return map[string]any{"provider": "anthropic", "account_id": "fixture-account", "credential_version": version}
}

// Inspect durable records, not log files or a cached metadata projection. Raw
// ciphertext/secret material never leaves this helper as evidence.
func (f *credentialLifecycleFixture) encrypted(ctx context.Context, r credential.Record, access, refresh string) bool {
	s, err := credential.Open(f.keys, r.Identity, r.Envelope)
	if err != nil || s.OAuth == nil || s.OAuth.AccessToken != access || s.OAuth.RefreshToken != refresh || len(r.Envelope.Ciphertext) == 0 {
		return false
	}
	var raw string
	err = f.db.DB.QueryRowContext(ctx, f.db.Query("SELECT data FROM durable_records WHERE tenant_id=? AND kind='credential' AND id=?"), r.Identity.TenantID, r.Identity.ConnectionID).Scan(&raw)
	return err == nil && !strings.Contains(raw, access) && !strings.Contains(raw, refresh)
}

func (f *credentialLifecycleFixture) oauth(variant string) result {
	name := "credentials/oauth-" + variant
	if variant == "valid" {
		name = "credentials/oauth-state-replay"
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	before, err := f.load(ctx)
	if err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	obs, err := f.action(ctx, "fixture-connection", "oauth-start", lifecycleInput(before.Identity.Version))
	var auth struct {
		URL string `json:"authorization_url"`
	}
	if err != nil || obs.Status != 200 || json.Unmarshal([]byte(obs.Body), &auth) != nil {
		return lifecycleResult(name, start, false, nil)
	}
	u, err := url.Parse(auth.URL)
	if err != nil || u.Scheme+"://"+u.Host != f.i.server.URL {
		return lifecycleResult(name, start, false, nil)
	}
	q := u.Query()
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		return lifecycleResult(name, start, false, nil)
	}
	code := "local-code-" + variant
	f.i.mu.Lock()
	f.i.codes[code] = lifecycleCode{q.Get("nonce"), q.Get("code_challenge"), variant}
	f.i.mu.Unlock()
	input := lifecycleInput(before.Identity.Version)
	input["state"], input["code"], input["redirect_uri"] = q.Get("state"), code, f.i.server.URL+"/callback"
	calls := f.i.codeCalls.Load()
	obs, err = f.action(ctx, "fixture-connection", "oauth-callback", input)
	after, loadErr := f.load(ctx)
	ok := err == nil && loadErr == nil && f.i.codeCalls.Load()-calls == 1
	if variant == "valid" {
		ok = ok && obs.Status == 200 && after.Identity.Version == before.Identity.Version+1 && f.encrypted(ctx, after, "lifecycle-access", "lifecycle-refresh")
	} else {
		ok = ok && obs.Status == 502 && obs.ErrorCode == "credential_action_failed" && reflect.DeepEqual(before, after)
	}
	// Replay both accepted and rejected states: consumption precedes exchange,
	// and no second token request may reach the issuer.
	replay, replayErr := f.action(ctx, "fixture-connection", "oauth-callback", input)
	final, finalErr := f.load(ctx)
	ok = ok && replayErr == nil && replay.Status == 502 && replay.ErrorCode == "credential_action_failed" && f.i.codeCalls.Load()-calls == 1 && finalErr == nil && reflect.DeepEqual(after, final)
	return lifecycleResult(name, start, ok, map[string]any{"token_exchanges": f.i.codeCalls.Load() - calls, "callback_status": obs.Status, "callback_error_code": obs.ErrorCode, "replay_status": replay.Status, "replay_error_code": replay.ErrorCode, "signed_rs256": true, "pkce_checked_by_issuer": true})
}

func (f *credentialLifecycleFixture) refreshRace() []result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	failure := func() []result {
		return []result{lifecycleResult("credentials/refreshrace", start, false, nil), lifecycleResult("credentials/rotatedtoken-durable", start, false, nil)}
	}
	before, err := f.load(ctx)
	if err != nil {
		return failure()
	}
	manager, err := credential.NewManager(f.keys, f.db)
	if err != nil {
		return failure()
	}
	old := credential.OAuthToken{AccessToken: "lifecycle-access", RefreshToken: "lifecycle-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(-time.Minute)}
	if _, err = manager.Put(ctx, before.Identity, credential.Secret{OAuth: &old}); err != nil {
		return failure()
	}
	seeded, err := f.load(ctx)
	if err != nil || !f.encrypted(ctx, seeded, old.AccessToken, old.RefreshToken) {
		return failure()
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	f.i.mu.Lock()
	f.i.refreshEntered, f.i.refreshRelease = entered, release
	f.i.mu.Unlock()
	defer func() {
		f.i.mu.Lock()
		select {
		case <-release:
		default:
			close(release)
		}
		f.i.refreshEntered, f.i.refreshRelease = nil, nil
		f.i.mu.Unlock()
	}()
	calls, dispatched := f.i.refreshCalls.Load(), f.e.fixture.count()
	const workers = 8
	type observation struct {
		status int
		err    error
	}
	results := make(chan observation, workers)
	gate := make(chan struct{})
	var launched sync.WaitGroup
	launched.Add(workers)
	for range workers {
		go func() {
			launched.Done()
			<-gate
			o, requestErr := f.e.extRequest(ctx, false, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"local lifecycle race"}],"max_tokens":8}`), nil)
			results <- observation{o.Status, requestErr}
		}()
	}
	launched.Wait()
	close(gate)
	select {
	case <-entered:
	case <-ctx.Done():
		return failure()
	}
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return failure()
	}
	close(release)
	ok, successes := true, 0
	for range workers {
		select {
		case o := <-results:
			if o.err == nil && o.status == 200 {
				successes++
			} else {
				ok = false
			}
		case <-ctx.Done():
			return failure()
		}
	}
	after, err := f.load(ctx)
	count := f.i.refreshCalls.Load() - calls
	ok = ok && err == nil && count == 1 && f.e.fixture.count()-dispatched == workers && after.Identity.Version == seeded.Identity.Version+1
	f.e.fixture.mu.Lock()
	for _, c := range f.e.fixture.calls[dispatched:] {
		if c.CredentialConflict || c.CredentialHeader != "Authorization" || c.CredentialCount != 1 || c.Credential != "Bearer lifecycle-rotated-access" {
			ok = false
		}
	}
	f.e.fixture.mu.Unlock()
	race := lifecycleResult("credentials/refreshrace", start, ok, map[string]any{"workers": workers, "successes": successes, "refresh_requests": count, "inference_dispatches": f.e.fixture.count() - dispatched})
	// Reopen a separate SQL handle to exclude an in-memory-only update. The
	// running process remains alive; this is durable rotation, not crash proof.
	reopened, openErr := store.Open(ctx, f.cfg)
	durable := ok && f.encrypted(ctx, after, "lifecycle-rotated-access", "lifecycle-rotated-refresh")
	if openErr != nil {
		durable = false
	} else {
		r, loadErr := reopened.LoadCredential(ctx, f.e.tenantID, "fixture-connection")
		durable = durable && loadErr == nil && reflect.DeepEqual(after, r)
		_ = reopened.Close()
	}
	staleErr := f.db.PutCredential(ctx, seeded, seeded.Identity.Version-1)
	final, loadErr := f.load(ctx)
	durable = durable && errors.Is(staleErr, credential.ErrConflict) && loadErr == nil && reflect.DeepEqual(after, final)
	return []result{race, lifecycleResult("credentials/rotatedtoken-durable", start, durable, map[string]any{"separate_sql_handle": openErr == nil, "stale_write_rejected": errors.Is(staleErr, credential.ErrConflict), "process_crash_exercised": false})}
}

const lifecycleRefreshGateEnv = "HOORIFIC_QUALIFICATION_REFRESH_COMMIT_GATE"

func lifecycleExecutable(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("qualification binary was not supplied")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("qualification binary is not a regular executable")
	}
	return absolute, nil
}

func lifecycleEnv(gate string) []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, lifecycleRefreshGateEnv+"=") {
			continue
		}
		out = append(out, value)
	}
	if gate != "" {
		out = append(out, lifecycleRefreshGateEnv+"="+gate)
	}
	return out
}

func lifecycleStopServer(e *environment) error {
	if e == nil || e.server == nil || e.server.Process == nil || e.server.ProcessState != nil {
		return nil
	}
	_ = e.server.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- e.server.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		_ = e.server.Process.Kill()
		<-done
		return nil
	}
}

func lifecycleWaitLive(ctx context.Context, e *environment) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+e.management+"/health/live", nil)
		if err == nil {
			resp, requestErr := e.client.Do(req)
			if requestErr == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func lifecycleStartServer(ctx context.Context, e *environment, binary, gate string) (*exec.Cmd, error) {
	absolute, err := lifecycleExecutable(binary)
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(e.root, "refresh-crash.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	migrationCtx, migrationCancel := context.WithTimeout(ctx, 30*time.Second)
	migration := exec.CommandContext(migrationCtx, absolute, "migrate", "--config", e.config)
	migration.Env = lifecycleEnv("")
	migration.Dir = e.root
	migration.Stdout, migration.Stderr = logFile, logFile
	err = migration.Run()
	migrationCancel()
	if err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("qualification migration failed: %w", err)
	}
	cmd := exec.Command(absolute, "serve", "--config", e.config)
	cmd.Env = lifecycleEnv(gate)
	cmd.Dir = e.root
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err = cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("qualification serve failed: %w", err)
	}
	_ = logFile.Close()
	e.server = cmd
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	if err = lifecycleWaitLive(readyCtx, e); err != nil {
		_ = lifecycleStopServer(e)
		return nil, fmt.Errorf("qualification gateway readiness failed: %w", err)
	}
	return cmd, nil
}

func lifecycleWaitReady(ctx context.Context, path string) error {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.New("refresh commit gate readiness marker is not a private regular file")
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if len(raw) == 1 {
				return nil
			}
			if len(raw) > 1 {
				return errors.New("refresh commit gate readiness marker has unexpected contents")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func lifecycleDurableRefresh(ctx context.Context, f *credentialLifecycleFixture) (credential.Record, *credential.RefreshIntent, string, error) {
	var raw string
	err := f.db.DB.QueryRowContext(ctx, f.db.Query("SELECT data FROM durable_records WHERE tenant_id=? AND kind='credential' AND id=?"), f.e.tenantID, "fixture-connection").Scan(&raw)
	if err != nil {
		return credential.Record{}, nil, "", err
	}
	var row struct {
		Record        credential.Record
		Intent        *credential.RefreshIntent
		RefreshStatus string
	}
	if err = json.Unmarshal([]byte(raw), &row); err != nil {
		return credential.Record{}, nil, "", err
	}
	return row.Record, row.Intent, row.RefreshStatus, nil
}

func (f *credentialLifecycleFixture) crashBoundary() result {
	name := "credentials/rotatedtoken-crashbeforepersist"
	start := time.Now()
	if strings.TrimSpace(f.e.qualificationBinary) == "" {
		return result{Name: name, Status: "not-run", Detail: "requires an explicit -tags qualification binary via --qualification-binary; no crash boundary was claimed", DurationMS: time.Since(start).Milliseconds(), Evidence: map[string]any{"process_crash_exercised": false, "boundary": "credential.Refresher.refreshRecord -> Store.CommitRefresh"}}
	}
	qualified, err := lifecycleExecutable(f.e.qualificationBinary)
	if err != nil {
		return lifecycleResult(name, start, false, map[string]any{"process_crash_exercised": false, "boundary": "credential.Refresher.refreshRecord -> Store.CommitRefresh", "error": err.Error()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	before, err := f.load(ctx)
	if err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	manager, err := credential.NewManager(f.keys, f.db)
	if err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	old := credential.OAuthToken{AccessToken: "lifecycle-crash-access", RefreshToken: "lifecycle-crash-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(-time.Minute)}
	if _, err = manager.Put(ctx, before.Identity, credential.Secret{OAuth: &old}); err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	seeded, err := f.load(ctx)
	if err != nil || !f.encrypted(ctx, seeded, old.AccessToken, old.RefreshToken) {
		return lifecycleResult(name, start, false, nil)
	}
	if err = f.db.Close(); err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	f.db = nil
	restored := false
	defer func() {
		if restored {
			return
		}
		_ = lifecycleStopServer(f.e)
		_ = f.e.Start(context.Background(), f.e.binary)
		if f.db == nil {
			f.db, _ = store.Open(context.Background(), f.cfg)
		}
	}()
	if err = lifecycleStopServer(f.e); err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	gate := filepath.Join(f.e.root, fmt.Sprintf("refresh-commit-gate-%d", time.Now().UnixNano()))
	cmd, err := lifecycleStartServer(ctx, f.e, qualified, gate)
	if err != nil {
		return lifecycleResult(name, start, false, map[string]any{"process_crash_exercised": false, "gate": gate})
	}
	refreshBefore := f.i.refreshCalls.Load()
	dispatchBefore := f.e.fixture.count()
	requestCtx, requestCancel := context.WithTimeout(ctx, 20*time.Second)
	type requestResult struct {
		observation extObservation
		err         error
	}
	requestDone := make(chan requestResult, 1)
	go func() {
		obs, requestErr := f.e.extRequest(requestCtx, false, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"crash boundary"}],"max_tokens":8}`), nil)
		requestDone <- requestResult{obs, requestErr}
	}()
	readyPath := gate + ".ready"
	readyCtx, readyCancel := context.WithTimeout(ctx, 20*time.Second)
	err = lifecycleWaitReady(readyCtx, readyPath)
	readyCancel()
	crashed := err == nil
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	requestCancel()
	var request requestResult
	select {
	case request = <-requestDone:
	case <-time.After(5 * time.Second):
		err = errors.New("refresh request did not terminate after the gated process was killed")
	}
	releaseAbsent := false
	if _, statErr := os.Lstat(gate + ".release"); errors.Is(statErr, os.ErrNotExist) {
		releaseAbsent = true
	}
	if err == nil && !crashed {
		err = errors.New("qualified gateway did not reach the commit boundary before termination")
	}
	if err == nil && !releaseAbsent {
		err = errors.New("refresh commit gate release marker unexpectedly exists")
	}
	if err == nil && request.err == nil && request.observation.UpstreamCalls != 0 {
		err = errors.New("gated refresh dispatched inference before durable commit")
	}
	if err == nil && f.i.refreshCalls.Load()-refreshBefore != 1 {
		err = errors.New("refresh boundary did not receive exactly one provider refresh")
	}
	if err == nil {
		err = f.e.Start(ctx, f.e.binary)
	}
	if err != nil {
		return lifecycleResult(name, start, false, map[string]any{"process_crash_exercised": crashed, "release_absent": releaseAbsent, "request": request.observation})
	}
	restored = true
	f.db, err = store.Open(ctx, f.cfg)
	if err != nil {
		return lifecycleResult(name, start, false, map[string]any{"process_crash_exercised": crashed, "release_absent": releaseAbsent})
	}
	after, intent, refreshStatus, loadErr := lifecycleDurableRefresh(ctx, f)
	if loadErr != nil {
		return lifecycleResult(name, start, false, nil)
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 20*time.Second)
	recovery, recoveryErr := f.e.extRequest(recoveryCtx, false, http.MethodPost, "/v1/chat/completions", []byte(`{"model":"assistant","messages":[{"role":"user","content":"post crash recovery"}],"max_tokens":8}`), nil)
	recoveryCancel()
	ok := crashed && releaseAbsent && request.err != nil && request.observation.UpstreamCalls == 0 && after.Identity == seeded.Identity && f.encrypted(ctx, after, old.AccessToken, old.RefreshToken) && intent != nil && refreshStatus == "pending" && recoveryErr == nil && recovery.Status == http.StatusServiceUnavailable && recovery.ErrorCode == "unavailable" && recovery.UpstreamCalls == 0 && f.e.fixture.count() == dispatchBefore
	evidence := map[string]any{"process_crash_exercised": crashed, "release_absent": releaseAbsent, "request_cancelled": request.err != nil, "provider_refreshes": f.i.refreshCalls.Load() - refreshBefore, "durable_identity_version": after.Identity.Version, "refresh_status": refreshStatus, "pending_intent": intent != nil, "recovery_status": recovery.Status, "recovery_error_code": recovery.ErrorCode, "recovery_upstream_calls": recovery.UpstreamCalls, "inference_dispatches": f.e.fixture.count() - dispatchBefore}
	return lifecycleResult(name, start, ok, evidence)
}

func (f *credentialLifecycleFixture) staleOwner() result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := "credentials/expiredlease-staleowner"
	manager, err := credential.NewManager(f.keys, f.db)
	if err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	id := credential.Identity{TenantID: f.e.tenantID, ConnectionID: "lifecycle-lease", CredentialID: "lifecycle-lease", AccountID: "fixture-account", Provider: "anthropic"}
	token := credential.OAuthToken{AccessToken: "lease-access", RefreshToken: "lease-refresh", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err = manager.Put(ctx, id, credential.Secret{OAuth: &token}); err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	id.Version = 1
	before, err := f.db.LoadCredential(ctx, id.TenantID, id.ConnectionID)
	if err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	intent := credential.RefreshIntent{Identity: id, AttemptID: "local-owner", Fence: "local-fence", LeaseUntil: time.Now().Add(300 * time.Millisecond)}
	if err = f.db.BeginRefresh(ctx, intent); err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	next := before
	next.Identity.Version++
	next.Envelope, err = credential.Seal(f.keys, next.Identity, credential.Secret{OAuth: &token})
	if err != nil {
		return lifecycleResult(name, start, false, nil)
	}
	forged := intent
	forged.Fence = "other-owner"
	wrongFence := f.db.CommitRefresh(ctx, forged, next)
	select {
	case <-time.After(time.Until(intent.LeaseUntil) + 20*time.Millisecond):
	case <-ctx.Done():
		return lifecycleResult(name, start, false, nil)
	}
	expired := f.db.CommitRefresh(ctx, intent, next)
	takeover := intent
	takeover.AttemptID, takeover.Fence, takeover.LeaseUntil = "new-owner", "new-fence", time.Now().Add(time.Minute)
	blocked := f.db.BeginRefresh(ctx, takeover)
	_, rewrapErr := manager.Rewrap(ctx, id.TenantID, id.ConnectionID)
	after, loadErr := f.db.LoadCredential(ctx, id.TenantID, id.ConnectionID)
	ok := errors.Is(wrongFence, credential.ErrConflict) && errors.Is(expired, credential.ErrConflict) && errors.Is(blocked, credential.ErrReauthRequired) && errors.Is(rewrapErr, credential.ErrReauthRequired) && loadErr == nil && reflect.DeepEqual(before, after) && f.encrypted(ctx, after, token.AccessToken, token.RefreshToken)
	return lifecycleResult(name, start, ok, map[string]any{"surface": "production SQL repository and credential manager", "wrong_fence_rejected": errors.Is(wrongFence, credential.ErrConflict), "expired_owner_rejected": errors.Is(expired, credential.ErrConflict), "takeover_requires_reauth": errors.Is(blocked, credential.ErrReauthRequired)})
}

func (f *credentialLifecycleFixture) wrongAccount() result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before, err := f.load(ctx)
	if err != nil {
		return lifecycleResult("credentials/wrongaccount-import", start, false, nil)
	}
	input := lifecycleInput(before.Identity.Version)
	input["account_id"], input["kind"], input["secret"] = "other-account", "api_key", "lifecycle-wrong-account-secret"
	calls := f.e.fixture.count()
	obs, requestErr := f.action(ctx, "fixture-connection", "import", input)
	after, loadErr := f.load(ctx)
	ok := requestErr == nil && obs.Status == 400 && obs.ErrorCode == "credential_identity_mismatch" && loadErr == nil && reflect.DeepEqual(before, after) && f.e.fixture.count() == calls
	return lifecycleResult("credentials/wrongaccount-import", start, ok, map[string]any{"http_status": obs.Status, "error_code": obs.ErrorCode, "durable_unchanged": loadErr == nil && reflect.DeepEqual(before, after), "upstream_calls": f.e.fixture.count() - calls})
}

func (f *credentialLifecycleFixture) deviceExpiry() result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	name := "credentials/device-expiry"
	// Positive control uses the same configured device factory, SQL repository,
	// signed issuer, and HTTP poll route before testing expiration.
	for _, expire := range []bool{false, true} {
		seconds := int64(60)
		if expire {
			seconds = 1
		}
		f.i.deviceSeconds.Store(seconds)
		before, err := f.load(ctx)
		if err != nil {
			return lifecycleResult(name, start, false, nil)
		}
		starts, calls := f.i.deviceStarts.Load(), f.i.deviceCalls.Load()
		obs, err := f.action(ctx, "fixture-connection", "device-start", lifecycleInput(before.Identity.Version))
		var handle struct {
			ID        string    `json:"flow_id"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err != nil || obs.Status != 200 || json.Unmarshal([]byte(obs.Body), &handle) != nil || handle.ID == "" || f.i.deviceStarts.Load()-starts != 1 {
			return lifecycleResult(name, start, false, nil)
		}
		state, err := f.db.LoadDevice(ctx, f.e.tenantID, handle.ID)
		if err != nil || state.Status != "pending" || len(state.DeviceCodeEnvelope.Ciphertext) == 0 {
			return lifecycleResult(name, start, false, nil)
		}
		waitUntil := state.PollAfter.Add(30 * time.Millisecond)
		if expire {
			waitUntil = state.ExpiresAt.Add(30 * time.Millisecond)
		}
		select {
		case <-time.After(time.Until(waitUntil)):
		case <-ctx.Done():
			return lifecycleResult(name, start, false, nil)
		}
		input := lifecycleInput(before.Identity.Version)
		input["flow_id"] = handle.ID
		obs, err = f.action(ctx, "fixture-connection", "device-poll", input)
		after, loadErr := f.load(ctx)
		if err != nil || loadErr != nil {
			return lifecycleResult(name, start, false, nil)
		}
		if expire {
			final, stateErr := f.db.LoadDevice(ctx, f.e.tenantID, handle.ID)
			ok := obs.Status == 502 && obs.ErrorCode == "credential_action_failed" && f.i.deviceCalls.Load() == calls && reflect.DeepEqual(before, after) && stateErr == nil && reflect.DeepEqual(state, final)
			return lifecycleResult(name, start, ok, map[string]any{"positive_control": true, "expired_poll_status": obs.Status, "expired_token_requests": f.i.deviceCalls.Load() - calls, "durable_unchanged": reflect.DeepEqual(before, after)})
		}
		if obs.Status != 200 || f.i.deviceCalls.Load()-calls != 1 || after.Identity.Version != before.Identity.Version+1 || !f.encrypted(ctx, after, "lifecycle-access", "lifecycle-refresh") {
			return lifecycleResult(name, start, false, nil)
		}
	}
	return lifecycleResult(name, start, false, nil)
}

func (f *credentialLifecycleFixture) subscriptionDisabled() result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const id = "lifecycle-subscription"
	before, loadErr := f.load(ctx)
	if loadErr != nil {
		return lifecycleResult("credentials/subscription-optin-disabled", start, false, nil)
	}
	data := map[string]any{
		"connector":  "codex-subscription",
		"account_id": "fixture-account",
		"base_url":   f.i.server.URL,
		"dedicated":  true,
		"enabled":    true,
		"settings": map[string]string{
			"allow_private":        "true",
			"allowed_cidrs":        "127.0.0.1/32",
			"subscription_enabled": "true",
			"subscription_auth":    "oauth",
			"consent_ack":          "true",
			"consent_tenant":       f.e.tenantID,
			"consent_account":      "fixture-account",
			"consent_provider":     "codex-subscription",
		},
	}
	raw, err := json.Marshal(map[string]any{"id": id, "data": data})
	if err != nil {
		return lifecycleResult("credentials/subscription-optin-disabled", start, false, nil)
	}
	created, err := f.e.extRequest(ctx, true, http.MethodPost, "/admin/api/v1/connections", raw, nil)
	if err != nil || created.Status < 200 || created.Status >= 300 {
		return lifecycleResult("credentials/subscription-optin-disabled", start, false, map[string]any{"create_status": created.Status})
	}
	calls := f.e.fixture.count()
	issuerCalls := f.i.requests.Load()
	input := map[string]any{"provider": "codex-subscription", "account_id": "fixture-account", "credential_version": 0}
	obs, requestErr := f.action(ctx, id, "oauth-start", input)
	after, afterErr := f.load(ctx)
	_, subscriptionErr := f.db.LoadCredential(ctx, f.e.tenantID, id)
	ok := requestErr == nil &&
		obs.Status == http.StatusBadRequest &&
		obs.ErrorCode == "unsupported_operation" &&
		f.e.fixture.count() == calls &&
		f.i.requests.Load()-issuerCalls == 0 &&
		afterErr == nil &&
		reflect.DeepEqual(before, after) &&
		errors.Is(subscriptionErr, sql.ErrNoRows)
	return lifecycleResult("credentials/subscription-optin-disabled", start, ok, map[string]any{
		"create_status":                  created.Status,
		"request_status":                 obs.Status,
		"request_error_code":             obs.ErrorCode,
		"upstream_calls":                 f.e.fixture.count() - calls,
		"issuer_calls":                   f.i.requests.Load() - issuerCalls,
		"fixture_credential_unchanged":   afterErr == nil && reflect.DeepEqual(before, after),
		"subscription_credential_absent": errors.Is(subscriptionErr, sql.ErrNoRows),
		"registration_policy":            "subscription connectors omitted while opt-in is disabled",
	})
}
