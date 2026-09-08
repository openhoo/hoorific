package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func (e *environment) clusterAdminPost(kind, id string, data any) (int, []byte, error) {
	raw, err := json.Marshal(map[string]any{"id": id, "data": data})
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+e.management+"/admin/api/v1/"+kind, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", e.cookie)
	req.Header.Set("Origin", "http://"+e.management)
	req.Header.Set("X-CSRF-Token", e.csrf)
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

func (e *environment) clusterCall(target *environment, key string) (int, []byte, error) {
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"Reply exactly OK"}],"max_tokens":16}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+target.inference+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := target.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, raw, err
}

func (e *environment) clusterReserve(c *verifyCluster) (int64, error) {
	raw, err := c.sql("SELECT COALESCE(SUM(reserved),0) FROM allowances WHERE tenant_id='" + e.tenantID + "'")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(raw, 10, 64)
}

func (e *environment) clusterCharges(c *verifyCluster) (count, cost int64, err error) {
	raw, err := c.sql("SELECT COUNT(*) || ',' || COALESCE(SUM(amount),0) FROM usage_ledger WHERE effect_kind='charge' AND tenant_id='" + e.tenantID + "'")
	if err != nil {
		return 0, 0, err
	}
	_, err = fmt.Sscanf(raw, "%d,%d", &count, &cost)
	return count, cost, err
}

func clusterOperationRequest(target *environment, f *operationFixture, body []byte, headers map[string]string) (operationReply, error) {
	if target == nil || target.client == nil || target.inference == "" || f == nil || f.key == "" {
		return operationReply{}, fmt.Errorf("cluster idempotency request requires two live endpoints and a scoped fixture key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+target.inference+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return operationReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := *target.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return operationReply{header: make(http.Header)}, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	reply := operationReply{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}
	if readErr != nil {
		return reply, readErr
	}
	if len(raw) > 4<<20 {
		return reply, fmt.Errorf("cluster idempotency response exceeded fixture bound")
	}
	return reply, nil
}

func (e *environment) clusterIdempotencyReplay(replica *environment) result {
	start := time.Now()
	f, err := e.operationFixture("cluster-idempotency", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		return operationJSON(w, map[string]any{
			"id":     "cluster-idempotent",
			"object": "chat.completion",
			"model":  "fixture-model",
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "OK"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
		})
	})
	if f != nil {
		defer f.server.Close()
	}
	if err != nil {
		return result{"cluster/idempotency-replay", "failed", "scoped fixture setup failed: " + err.Error(), time.Since(start).Milliseconds(), nil}
	}
	body := []byte(`{"model":"` + f.alias + `","messages":[{"role":"user","content":"cluster durable replay"}],"max_tokens":8}`)
	headers := map[string]string{"Idempotency-Key": "cluster-durable-replay"}
	first, firstErr := clusterOperationRequest(e, f, body, headers)
	second, secondErr := clusterOperationRequest(replica, f, body, headers)
	firstRequestID := first.header.Get("X-Request-ID")
	secondRequestID := second.header.Get("X-Request-ID")
	evidence := map[string]any{
		"fixture_id":                f.id,
		"alias":                     f.alias,
		"primary_status":            first.status,
		"replica_status":            second.status,
		"primary_request_id":        firstRequestID,
		"replica_request_id":        secondRequestID,
		"request_id_equal":          firstRequestID != "" && firstRequestID == secondRequestID,
		"response_body_equal":       bytes.Equal(first.body, second.body),
		"response_contains_fixture": bytes.Contains(first.body, []byte("OK")),
		"upstream_calls":            f.count(),
	}
	if firstErr != nil {
		err = fmt.Errorf("primary idempotency request failed: %w", firstErr)
	} else if secondErr != nil {
		err = fmt.Errorf("replica idempotency replay failed: %w", secondErr)
	} else if first.status != http.StatusOK || second.status != http.StatusOK || !bytes.Equal(first.body, second.body) || !bytes.Contains(first.body, []byte("OK")) || firstRequestID == "" || firstRequestID != secondRequestID || f.count() != 1 {
		err = fmt.Errorf("completed cross-replica idempotency replay was not exact or redispatched: statuses=%d/%d request_ids=%q/%q calls=%d", first.status, second.status, firstRequestID, secondRequestID, f.count())
	}

	var concurrentErr error
	var blockedCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseOwner := func() {
		releaseOnce.Do(func() { close(release) })
	}
	blocked, blockedSetupErr := e.operationFixture("cluster-idempotency-pending", "openai", []string{"generate"}, func(w http.ResponseWriter, r *http.Request) error {
		blockedCalls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		return operationJSON(w, map[string]any{
			"id":      "cluster-pending",
			"object":  "chat.completion",
			"model":   "fixture-model",
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})
	if blocked != nil {
		defer blocked.server.Close()
	}
	pendingEvidence := map[string]any{}
	if blockedSetupErr != nil {
		concurrentErr = fmt.Errorf("scoped pending fixture setup failed: %w", blockedSetupErr)
	} else {
		pendingBody := []byte(`{"model":"` + blocked.alias + `","messages":[{"role":"user","content":"cluster pending replay"}],"max_tokens":8}`)
		pendingHeaders := map[string]string{"Idempotency-Key": "cluster-pending-replay"}
		type requestResult struct {
			reply operationReply
			err   error
		}
		ownerDone := make(chan requestResult, 1)
		go func() {
			reply, requestErr := clusterOperationRequest(e, blocked, pendingBody, pendingHeaders)
			ownerDone <- requestResult{reply: reply, err: requestErr}
		}()
		pending := operationReply{}
		var pendingErr error
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			pendingErr = fmt.Errorf("idempotency owner did not reach the scoped upstream fixture")
		}
		if pendingErr == nil {
			pending, pendingErr = clusterOperationRequest(replica, blocked, pendingBody, pendingHeaders)
			if pendingErr == nil && (pending.status != http.StatusConflict || pending.header.Get("X-Hoorific-Error-Code") != "idempotency_in_progress" || blockedCalls.Load() != 1) {
				pendingErr = fmt.Errorf("in-flight cross-replica request was not rejected without dispatch: HTTP %d/%s calls=%d", pending.status, pending.header.Get("X-Hoorific-Error-Code"), blockedCalls.Load())
			}
		}
		releaseOwner()
		owner := requestResult{}
		var ownerWaitErr error
		select {
		case owner = <-ownerDone:
		case <-time.After(5 * time.Second):
			ownerWaitErr = fmt.Errorf("idempotency owner did not complete after release")
		}
		replay := operationReply{}
		var replayErr error
		if ownerWaitErr == nil && owner.err == nil && owner.reply.status == http.StatusOK {
			replay, replayErr = clusterOperationRequest(replica, blocked, pendingBody, pendingHeaders)
			if replayErr == nil && (replay.status != http.StatusOK || !bytes.Equal(replay.body, owner.reply.body) || owner.reply.header.Get("X-Request-ID") == "" || replay.header.Get("X-Request-ID") != owner.reply.header.Get("X-Request-ID") || blockedCalls.Load() != 1) {
				replayErr = fmt.Errorf("completed cross-replica replay after pending owner was not exact or redispatched: status=%d request_ids=%q/%q calls=%d", replay.status, owner.reply.header.Get("X-Request-ID"), replay.header.Get("X-Request-ID"), blockedCalls.Load())
			}
		}
		if pendingErr != nil {
			concurrentErr = pendingErr
		} else if ownerWaitErr != nil {
			concurrentErr = ownerWaitErr
		} else if owner.err != nil {
			concurrentErr = fmt.Errorf("idempotency owner request failed: %w", owner.err)
		} else if owner.reply.status != http.StatusOK || !bytes.Contains(owner.reply.body, []byte("OK")) {
			concurrentErr = fmt.Errorf("idempotency owner returned HTTP %d without fixture response", owner.reply.status)
		} else if replayErr != nil {
			concurrentErr = replayErr
		}
		fixtureCalls := -1
		if ownerWaitErr == nil {
			fixtureCalls = blocked.count()
		}
		ownerRequestID := owner.reply.header.Get("X-Request-ID")
		replayRequestID := replay.header.Get("X-Request-ID")
		pendingError := ""
		if pendingErr != nil {
			pendingError = pendingErr.Error()
		}
		ownerError := ""
		if owner.err != nil {
			ownerError = owner.err.Error()
		}
		replayError := ""
		if replayErr != nil {
			replayError = replayErr.Error()
		}
		pendingEvidence = map[string]any{
			"pending_status":      pending.status,
			"pending_error_code":  pending.header.Get("X-Hoorific-Error-Code"),
			"pending_error":       pendingError,
			"owner_status":        owner.reply.status,
			"owner_request_id":    ownerRequestID,
			"owner_error":         ownerError,
			"replay_status":       replay.status,
			"replay_request_id":   replayRequestID,
			"replay_error":        replayError,
			"request_id_equal":    ownerRequestID != "" && ownerRequestID == replayRequestID,
			"response_body_equal": bytes.Equal(owner.reply.body, replay.body),
			"upstream_calls":      blockedCalls.Load(),
			"fixture_calls":       fixtureCalls,
		}
	}
	if err == nil && concurrentErr != nil {
		err = fmt.Errorf("concurrent cross-replica idempotency proof failed: %w", concurrentErr)
	}
	return extResult("cluster/idempotency-replay", start, map[string]any{
		"completed":   evidence,
		"pending":     pendingEvidence,
		"quota_scope": "checked before cluster-hard-cost allowance mutation",
	}, err)
}

func (e *environment) clusterScenarios() []result {
	start := time.Now()
	if e.mode != "cluster" {
		return nil
	}
	originalFixtureMode := e.fixture.mode.Load().(string)
	e.fixture.mode.Store("slow")
	defer e.fixture.mode.Store(originalFixtureMode)
	c, err := clusterFor(e.root)
	if err != nil {
		return []result{{"cluster/setup", "failed", err.Error(), 0, nil}}
	}
	replica, stop, err := e.clusterReplica()
	if err != nil {
		return []result{{"cluster/replica-start", "failed", err.Error(), 0, nil}}
	}
	defer stop()
	if err = e.clusterReady(); err != nil {
		return []result{{"cluster/primary-ready", "failed", err.Error(), time.Since(start).Milliseconds(), nil}}
	}
	if err = replica.clusterReady(); err != nil {
		return []result{{"cluster/replica-ready", "failed", err.Error(), time.Since(start).Milliseconds(), nil}}
	}
	out := []result{{"cluster/replica-start", "passed", "two independent gateway processes reached readiness against shared SQL/Redis", time.Since(start).Milliseconds(), map[string]any{"primary_pid": e.server.Process.Pid, "replica_pid": replica.server.Process.Pid}}}
	for name, target := range map[string]*environment{"primary": e, "replica": replica} {
		status, body, callErr := e.clusterCall(target, e.key)
		if callErr != nil || status != http.StatusOK || !bytes.Contains(body, []byte("OK")) {
			out = append(out, result{"cluster/baseline-" + name, "failed", fmt.Sprintf("status=%d error=%v", status, callErr), time.Since(start).Milliseconds(), nil})
		} else {
			out = append(out, result{"cluster/baseline-" + name, "passed", "independent process served validated gateway API response", time.Since(start).Milliseconds(), map[string]any{"status": status}})
		}
	}
	out = append(out, e.clusterIdempotencyReplay(replica))

	// Declared context bound 32768 at 1000 nanodollars/token plus the
	// request's 16 output tokens at 2000 nanodollars/token. One reservation
	// fits exactly; two cannot fit, even after the first settles for 13000.
	const oneAttemptReserve int64 = 32768*1000 + 16*2000
	const actualCharge int64 = 9*1000 + 2*2000
	status, body, err := e.clusterAdminPost("policy_limits", "cluster-hard-cost", map[string]any{"scope": "tenant", "scope_id": e.tenantID, "max_cost": oneAttemptReserve, "cost_window": "total"})
	if err != nil || status < 200 || status >= 300 {
		return append(out, result{"cluster/allowance-setup", "failed", fmt.Sprintf("status=%d error=%v body=%s", status, err, trim(string(body))), time.Since(start).Milliseconds(), nil})
	}
	before, err := e.clusterReserve(c)
	if err != nil {
		return append(out, result{"cluster/allowance-race", "failed", err.Error(), time.Since(start).Milliseconds(), nil})
	}
	beforeCharges, beforeCost, err := e.clusterCharges(c)
	if err != nil {
		return append(out, result{"cluster/allowance-race", "failed", err.Error(), time.Since(start).Milliseconds(), nil})
	}
	const n = 12
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	ch := make(chan outcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := e
			if i%2 == 1 {
				target = replica
			}
			s, b, x := e.clusterCall(target, e.key)
			ch <- outcome{s, b, x}
		}(i)
	}
	wg.Wait()
	close(ch)
	accepted, denied, transport := 0, 0, 0
	for item := range ch {
		if item.err != nil {
			transport++
		} else if item.status == http.StatusOK && bytes.Contains(item.body, []byte("OK")) {
			accepted++
		} else {
			denied++
		}
	}
	// Response EOF may precede the handler's deferred SQL finalization. Observe
	// that one completed race without issuing any additional inference requests.
	settlementStart := time.Now()
	deadline := settlementStart.Add(5 * time.Second)
	var after, afterCharges, afterCost int64
	var ledgerErr error
	query := "SELECT (SELECT COALESCE(SUM(reserved),0) FROM allowances WHERE tenant_id='" + e.tenantID + "') || ',' || COUNT(*) || ',' || COALESCE(SUM(amount),0) FROM usage_ledger WHERE effect_kind='charge' AND tenant_id='" + e.tenantID + "'"
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			ledgerErr = fmt.Errorf("settlement was not observed within 5 seconds")
			break
		}
		raw, readErr := c.command(remaining, "exec", "-e", "PGOPTIONS=-c statement_timeout=1000", c.postgres, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-U", "hoorific", "-d", "hoorific", "-c", query)
		if readErr != nil {
			ledgerErr = readErr
			break
		}
		if _, ledgerErr = fmt.Sscanf(string(raw), "%d,%d,%d", &after, &afterCharges, &afterCost); ledgerErr != nil {
			break
		}
		if time.Now().After(deadline) {
			ledgerErr = fmt.Errorf("settlement observation exceeded 5 seconds")
			break
		}
		if afterCharges-beforeCharges == 1 && afterCost-beforeCost == actualCharge && after-before == actualCharge {
			break
		}
		if afterCharges-beforeCharges > 1 || afterCost-beforeCost > actualCharge {
			ledgerErr = fmt.Errorf("settlement exceeded one charge")
			break
		}
		delay := 25 * time.Millisecond
		if remaining = time.Until(deadline); remaining < delay {
			delay = remaining
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	if ledgerErr != nil || accepted != 1 || denied < 1 || transport != 0 || after-before != actualCharge || afterCharges-beforeCharges != 1 || afterCost-beforeCost != actualCharge {
		return append(out, result{"cluster/allowance-race", "failed", fmt.Sprintf("one-attempt allowance/settlement mismatch accepted=%d denied=%d transport=%d before=%d after=%d charges=%d cost=%d ledger_error=%v", accepted, denied, transport, before, after, afterCharges-beforeCharges, afterCost-beforeCost, ledgerErr), time.Since(start).Milliseconds(), map[string]any{"nanodollars": oneAttemptReserve, "settlement_wait_ms": time.Since(settlementStart).Milliseconds()}})
	}
	out = append(out, result{"cluster/allowance-race", "passed", "two real gateways admitted exactly one attempt and SQL recorded exactly one charge", time.Since(start).Milliseconds(), map[string]any{"accepted": accepted, "denied": denied, "transport": transport, "before_reserved_nanodollars": before, "after_reserved_nanodollars": after, "one_attempt_reserve_nanodollars": oneAttemptReserve, "charge_count_delta": afterCharges - beforeCharges, "charged_nanodollars": afterCost - beforeCost, "settlement_wait_ms": time.Since(settlementStart).Milliseconds()}})

	faultStart := time.Now()
	beforeCalls := e.fixture.count()
	redisBefore, snapErr := e.clusterReserve(c)
	faultErr := snapErr
	if faultErr == nil {
		faultErr = c.fault("redis", "stop")
	}
	redisStatus := 0
	if faultErr == nil {
		redisStatus, _, _ = e.clusterCall(replica, e.key)
	}
	redisAfter, afterErr := e.clusterReserve(c)
	if faultErr == nil && afterErr != nil {
		faultErr = afterErr
	}
	_ = c.fault("redis", "start")
	if faultErr == nil {
		faultErr = c.ready()
	}
	if faultErr == nil && redisAfter > redisBefore+oneAttemptReserve {
		faultErr = fmt.Errorf("Redis loss increased SQL reserved allowance from %d to %d nanodollars", redisBefore, redisAfter)
	}
	faultStatus, detail := "passed", "owned Redis loss did not increase SQL authority and service recovered"
	if faultErr != nil {
		faultStatus, detail = "failed", faultErr.Error()
	}
	out = append(out, result{"cluster/redis-loss", faultStatus, detail, time.Since(faultStart).Milliseconds(), map[string]any{"request_status": redisStatus, "upstream_calls": e.fixture.count() - beforeCalls, "before_reserved_nanodollars": redisBefore, "after_reserved_nanodollars": redisAfter}})
	faultStart = time.Now()
	beforeCalls = e.fixture.count()
	faultErr = c.fault("postgres", "stop")
	sqlStatus := 0
	if faultErr == nil {
		sqlStatus, _, _ = e.clusterCall(e, e.key)
	}
	_ = c.fault("postgres", "start")
	if faultErr == nil {
		faultErr = c.ready()
	}
	if faultErr == nil && (sqlStatus < 400 || sqlStatus >= 500 || e.fixture.count() != beforeCalls) {
		faultErr = fmt.Errorf("SQL loss was not fail-closed status=%d upstream_delta=%d", sqlStatus, e.fixture.count()-beforeCalls)
	}
	faultStatus, detail = "passed", "authoritative SQL loss denied new dispatch and service recovered"
	if faultErr != nil {
		faultStatus, detail = "failed", faultErr.Error()
	}
	out = append(out, result{"cluster/sql-loss", faultStatus, detail, time.Since(faultStart).Milliseconds(), nil})

	restartStart := time.Now()
	beforeRestartCalls := e.fixture.count()
	faultErr = e.clusterRestart()
	if faultErr == nil {
		faultErr = replica.clusterReady()
	}
	restartStatus, restartCode := 0, ""
	if faultErr == nil {
		var responseBody []byte
		restartStatus, responseBody, faultErr = e.clusterCall(replica, e.key)
		var response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if faultErr == nil {
			faultErr = json.Unmarshal(responseBody, &response)
			restartCode = response.Error.Code
		}
		if faultErr == nil && (restartStatus != http.StatusTooManyRequests || restartCode != "quota_exceeded" || e.fixture.count() != beforeRestartCalls) {
			faultErr = fmt.Errorf("persisted tenant cap expected 429 quota_exceeded without dispatch, got status=%d code=%s upstream_delta=%d", restartStatus, restartCode, e.fixture.count()-beforeRestartCalls)
		}
	}
	readStatuses := map[string]int{}
	if faultErr == nil {
		for _, path := range []string{"/admin/api/v1/models", "/admin/api/v1/policy_limits/cluster-hard-cost", "/admin/api/v1/api_keys/fixture-key"} {
			obs, readErr := replica.extRequest(context.Background(), true, "GET", path, nil, nil)
			readStatuses[path] = obs.Status
			if readErr != nil || obs.Status != http.StatusOK {
				faultErr = fmt.Errorf("post-restart authenticated metadata read failed path=%s status=%d error=%v", path, obs.Status, readErr)
				break
			}
		}
	}
	var restartHeld, restartCharges, restartCost int64
	if faultErr == nil {
		restartHeld, faultErr = e.clusterReserve(c)
	}
	if faultErr == nil {
		restartCharges, restartCost, faultErr = e.clusterCharges(c)
		if faultErr == nil && (restartHeld != after || restartCharges != afterCharges || restartCost != afterCost) {
			faultErr = fmt.Errorf("persisted SQL state changed held=%d/%d charges=%d/%d cost=%d/%d", restartHeld, after, restartCharges, afterCharges, restartCost, afterCost)
		}
	}
	faultStatus, detail = "passed", "restart retained exact tenant quota denial and SQL settlement; authenticated catalog/policy/key reads remain usable"
	if faultErr != nil {
		faultStatus, detail = "failed", faultErr.Error()
	}
	out = append(out, result{"cluster/restart-durability", faultStatus, detail, time.Since(restartStart).Milliseconds(), map[string]any{"primary_pid": e.server.Process.Pid, "replica_pid": replica.server.Process.Pid, "request_status": restartStatus, "error_code": restartCode, "upstream_delta": e.fixture.count() - beforeRestartCalls, "metadata_statuses": readStatuses, "held_nanodollars": restartHeld, "charge_count": restartCharges, "charged_nanodollars": restartCost}})
	return out
}
