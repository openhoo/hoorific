package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	es "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/coder/websocket"
	"golang.org/x/net/http2"
)

// realtimeSuccess exercises the running gateway, not the relay in isolation.
// Every case owns its upstream and administrative resources; no global fixture
// mode, gateway key, environment variable, or production configuration is changed.
func (e *environment) realtimeSuccess() []result {
	var out []result
	for _, provider := range []string{"openai", "gemini"} {
		for _, mode := range []string{"text-audio-close", "mutation-before-forward", "cancel", "slow-client"} {
			out = append(out, e.rtWebSocket(provider, mode))
		}
	}
	out = append(out, e.rtWebRTC(false), e.rtWebRTC(true))
	out = append(out, e.rtBedrockContracts()...)
	return out
}

var rtSequence atomic.Uint64

func rtID() string {
	return fmt.Sprintf("verify-rt-%d-%d", time.Now().UnixNano(), rtSequence.Add(1))
}

// rtSeed uses the actual connection-bound import and explicit grant APIs.
func (e *environment) rtSeed(id, provider, endpoint string, priced bool) (string, error) {
	settings := map[string]string{"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}
	if err := e.extCreate("connections", id, map[string]any{
		"connector": provider, "account_id": id, "base_url": endpoint,
		"dedicated": true, "enabled": true, "region": "us-east-1", "settings": settings,
	}); err != nil {
		return "", err
	}
	if err := e.extImportCredential(id, provider, id, "verify-realtime-upstream"); err != nil {
		return "", err
	}
	model := map[string]any{
		"connection_id": id, "upstream_id": id, "operations": []string{"realtime", "audio.speech"},
		"input_modalities": []string{"text", "audio"}, "output_modalities": []string{"text", "audio"},
		"enabled": true, "provenance": "isolated realtime verification fixture",
	}
	if priced {
		model["price"] = map[string]any{"version": "fixture-v1", "maximum_unit_cost": 1000, "unit_operation": "realtime"}
	}
	if err := e.extCreate("models", id, model); err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"data": map[string]any{
		"name": id, "role": "operator", "permissions": []string{"inference:invoke", "connection:" + id},
		"connections": []string{id}, "operations": []string{"realtime", "audio.speech"},
		"portable": false, "native_account": true, "realtime": true,
	}})
	if err != nil {
		return "", err
	}
	obs, err := e.extRequest(context.Background(), true, "POST", "/admin/api/v1/api_keys/"+id+"/issue", raw, nil)
	if err != nil {
		return "", err
	}
	var issued struct {
		Token string `json:"token"`
	}
	if obs.Status != 200 || json.Unmarshal([]byte(obs.Body), &issued) != nil || issued.Token == "" {
		return "", fmt.Errorf("realtime key issue HTTP %d; token omitted", obs.Status)
	}
	return issued.Token, nil
}

func rtWire(provider, id string) (string, []string, []string, string) {
	if provider == "gemini" {
		return "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent", []string{
			`{"setup":{"model":"models/` + id + `","generationConfig":{"responseModalities":["AUDIO"]}}}`,
			`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"fixture input text"}]}],"turnComplete":true}}`,
			`{"realtimeInput":{"audio":{"mimeType":"audio/pcm;rate=16000","data":"AQIDBA=="}}}`,
		}, []string{
			`{"setupComplete":{}}`,
			`{"serverContent":{"modelTurn":{"parts":[{"text":"fixture output text"}]}}}`,
			`{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"BQYHCA=="}}]},"turnComplete":true}}`,
		}, `{"setup":{"model":"models/forbidden-mutation"}}`
	}
	return "/realtime", []string{
		`{"type":"session.update","session":{"model":"` + id + `","modalities":["text","audio"]}}`,
		`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"fixture input text"}]}}`,
		`{"type":"input_audio_buffer.append","audio":"AQIDBA=="}`,
	}, []string{
		`{"type":"session.updated","session":{"id":"fixture-session"}}`,
		`{"type":"response.output_text.delta","delta":"fixture output text"}`,
		`{"type":"response.output_audio.delta","delta":"BQYHCA=="}`,
	}, `{"type":"session.update","session":{"model":"forbidden-mutation"}}`
}

type rtSocketEvidence struct {
	mu            sync.Mutex
	Calls         int    `json:"calls"`
	ClientFrames  int    `json:"client_frames"`
	ServerFrames  int    `json:"server_frames"`
	FloodFrames   int    `json:"flood_frames"`
	UpstreamClose int    `json:"upstream_close"`
	GatewayKey    string `json:"-"`
	FixtureError  string `json:"fixture_error,omitempty"`
}

func (s *rtSocketEvidence) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"upstream_calls": s.Calls, "client_frames_observed_upstream": s.ClientFrames,
		"server_frames_sent": s.ServerFrames, "flood_frames_sent": s.FloodFrames,
		"upstream_close_code": s.UpstreamClose, "fixture_error": s.FixtureError}
}

func (e *environment) rtWebSocket(provider, mode string) result {
	start := time.Now()
	id := rtID()
	path, inputs, outputs, mutation := rtWire(provider, id)
	state := &rtSocketEvidence{}
	done := make(chan struct{})
	var finish sync.Once
	fixtureCtx, stopFixture := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.Calls++
		state.mu.Unlock()
		defer finish.Do(func() { close(done) })
		fail := func(err error) {
			state.mu.Lock()
			state.FixtureError = err.Error()
			state.mu.Unlock()
		}
		if r.Method != "GET" || r.URL.Path != path || (provider == "openai" && r.URL.Query().Get("model") != id) {
			fail(fmt.Errorf("unexpected upstream method/path/model"))
			http.Error(w, "unexpected route", 400)
			return
		}
		credential := r.Header.Get("Authorization")
		state.mu.Lock()
		gatewayKey := state.GatewayKey
		state.mu.Unlock()
		credentialOK := false
		switch provider {
		case "openai":
			credentialOK = credential == "Bearer verify-realtime-upstream" && r.Header.Get("X-Goog-Api-Key") == ""
		case "gemini":
			credentialOK = credential == "" && r.Header.Get("X-Goog-Api-Key") == "verify-realtime-upstream"
		}
		credentialOK = credentialOK && gatewayKey != "" && r.Header.Get("X-Api-Key") == "" &&
			credential != gatewayKey && credential != "Bearer "+gatewayKey &&
			r.Header.Get("X-Goog-Api-Key") != gatewayKey
		if !credentialOK {
			fail(fmt.Errorf("gateway did not inject fixture provider credential"))
			http.Error(w, "credential mismatch", 401)
			return
		}
		fixtureTimeout := 20 * time.Second
		if mode == "slow-client" {
			fixtureTimeout = 65 * time.Second
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			fail(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(fixtureCtx, fixtureTimeout)
		defer cancel()
		for i, want := range inputs {
			typ, payload, err := conn.Read(ctx)
			if err != nil {
				fail(err)
				return
			}
			state.mu.Lock()
			state.ClientFrames++
			state.mu.Unlock()
			if typ != websocket.MessageText || string(payload) != want {
				fail(fmt.Errorf("client frame %d differed from admitted text/audio envelope", i))
				return
			}
			if err = conn.Write(ctx, websocket.MessageText, []byte(outputs[i])); err != nil {
				fail(err)
				return
			}
			state.mu.Lock()
			state.ServerFrames++
			state.mu.Unlock()
		}
		if mode == "text-audio-close" {
			if err := conn.Close(websocket.StatusNormalClosure, "fixture complete"); err != nil {
				fail(err)
			}
			return
		}
		if mode == "slow-client" {
			// Independent reader proves the gateway closes upstream while its
			// downstream writer is backpressured. The fixture never closes first.
			readDone := make(chan error, 1)
			go func() { _, _, err := conn.Read(ctx); readDone <- err }()
			flood := []byte(`{"type":"fixture.audio","audio":"` + strings.Repeat("A", 256<<10) + `"}`)
			for range 512 {
				if err := conn.Write(ctx, websocket.MessageText, flood); err != nil {
					break
				}
				state.mu.Lock()
				state.FloodFrames++
				state.mu.Unlock()
			}
			select {
			case err := <-readDone:
				if ctx.Err() != nil || err == nil {
					fail(fmt.Errorf("no gateway close before fixture deadline"))
					return
				}
				state.mu.Lock()
				state.UpstreamClose = int(websocket.CloseStatus(err))
				state.mu.Unlock()
			case <-ctx.Done():
				fail(fmt.Errorf("fixture deadline, not gateway slow-client termination"))
			}
			return
		}
		_, _, err = conn.Read(ctx)
		state.mu.Lock()
		if err == nil {
			state.ClientFrames++
			state.FixtureError = "forbidden mutation reached upstream"
		} else if ctx.Err() != nil {
			state.FixtureError = "fixture deadline, not gateway cancellation"
		} else {
			state.UpstreamClose = int(websocket.CloseStatus(err))
		}
		state.mu.Unlock()
	}))
	defer func() {
		stopFixture()
		server.Close()
	}()
	key, err := e.rtSeed(id, provider, server.URL, false)
	name := "realtime/" + provider + "/" + mode
	if err != nil {
		return extResult(name, start, state.snapshot(), err)
	}
	state.mu.Lock()
	state.GatewayKey = key
	state.mu.Unlock()
	clientTimeout := 20 * time.Second
	if mode == "slow-client" {
		clientTimeout = 65 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws://"+e.inference+"/connect/"+id+"/"+provider+path+"?model="+url.QueryEscape(id), &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + key}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return extResult(name, start, state.snapshot(), fmt.Errorf("gateway upgrade failed: %w", err))
	}
	defer conn.CloseNow()
	var received int
	for i, input := range inputs {
		step, stop := context.WithTimeout(ctx, 8*time.Second)
		err = conn.Write(step, websocket.MessageText, []byte(input))
		if err == nil {
			var typ websocket.MessageType
			var payload []byte
			typ, payload, err = conn.Read(step)
			if err == nil && (typ != websocket.MessageText || string(payload) != outputs[i]) {
				err = fmt.Errorf("downstream frame %d differs from fixture output", i)
			}
			if err == nil {
				received++
			}
		}
		stop()
		if err != nil {
			break
		}
	}
	boundaryStart := time.Now()
	var downstreamClose int
	var slowStop *time.Timer
	var forcedSlowClose atomic.Bool
	if err == nil {
		switch mode {
		case "text-audio-close":
			step, stop := context.WithTimeout(ctx, 8*time.Second)
			_, _, closeErr := conn.Read(step)
			downstreamClose = int(websocket.CloseStatus(closeErr))
			if downstreamClose != int(websocket.StatusNormalClosure) {
				err = fmt.Errorf("normal upstream close was not propagated: code %d", downstreamClose)
			}
			stop()
		case "mutation-before-forward":
			step, stop := context.WithTimeout(ctx, 8*time.Second)
			err = conn.Write(step, websocket.MessageText, []byte(mutation))
			if err == nil {
				_, _, closeErr := conn.Read(step)
				downstreamClose = int(websocket.CloseStatus(closeErr))
				if closeErr == nil || step.Err() != nil {
					err = fmt.Errorf("mutation did not terminate downstream before deadline")
				}
			}
			stop()
		case "cancel":
			err = conn.CloseNow()
		case "slow-client":
			// Deliberately no reads. A forced client close is reported as
			// cancellation-under-backpressure, never gateway idle success.
			slowStop = time.AfterFunc(55*time.Second, func() {
				forcedSlowClose.Store(true)
				_ = conn.CloseNow()
			})
		}
	}
	if err == nil {
		bound := 8 * time.Second
		if mode == "slow-client" {
			bound = 60 * time.Second
		}
		timer := time.NewTimer(bound)
		select {
		case <-done:
		case <-timer.C:
			err = fmt.Errorf("upstream did not finish within %s; harness timeout is not success", bound)
		}
		timer.Stop()
	}
	if slowStop != nil {
		slowStop.Stop()
	}
	evidence := state.snapshot()
	evidence["downstream_frames_verified"] = received
	evidence["downstream_close_code"] = downstreamClose
	evidence["boundary_ms"] = time.Since(boundaryStart).Milliseconds()
	evidence["billing_stopped_proven"] = false
	evidence["forced_client_close"] = forcedSlowClose.Load()
	if mode == "slow-client" {
		if forcedSlowClose.Load() {
			evidence["scope"] = "client-cancellation-under-backpressure; gateway idle termination was not proven"
		} else {
			evidence["scope"] = "gateway slow-client termination; not an RSS or billing bound"
		}
	}
	if err == nil && (evidence["fixture_error"] != "" || evidence["upstream_calls"] != 1 || evidence["client_frames_observed_upstream"] != len(inputs) || received != len(outputs)) {
		err = fmt.Errorf("fixture lifecycle/payload evidence failed: %v", evidence)
	}
	if err == nil && mode == "slow-client" && evidence["flood_frames_sent"] == 0 {
		err = fmt.Errorf("no flood frames sent; backpressure was not exercised")
	}
	result := extResult(name, start, evidence, err)
	if mode == "slow-client" && forcedSlowClose.Load() && result.Status == "passed" {
		result.Detail = "observed bounded client cancellation under backpressure; gateway idle termination was not proven"
	}
	return result
}

func (e *environment) rtWebRTC(hardSpend bool) result {
	start := time.Now()
	id := rtID()
	name := "realtime/webrtc/setup"
	if hardSpend {
		name = "realtime/webrtc/hard-spend-before-forward"
	}
	const offer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=fixture-offer\r\nt=0 0\r\n"
	const answer = "v=0\r\no=- 2 2 IN IP4 127.0.0.1\r\ns=fixture-answer\r\nt=0 0\r\n"
	var calls atomic.Int64
	var valid atomic.Bool
	var gatewayKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		credential := r.Header.Get("Authorization")
		credentialOK := credential == "Bearer verify-realtime-upstream" &&
			r.Header.Get("X-Api-Key") == "" && credential != gatewayKey && credential != "Bearer "+gatewayKey
		valid.Store(err == nil && r.Method == "POST" && r.URL.Path == "/realtime/calls" && r.Header.Get("Content-Type") == "application/sdp" && credentialOK && string(body) == offer)
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()
	key, err := e.rtSeed(id, "openai", server.URL, hardSpend)
	gatewayKey = key
	if err == nil && hardSpend {
		err = e.extCreate("policy_limits", id, map[string]any{"scope": "connection", "scope_id": id, "max_cost": 1000000, "cost_window": "total"})
	}
	var obs extObservation
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		obs, err = e.extRequest(ctx, false, "POST", "/connect/"+id+"/openai/realtime/calls?model="+url.QueryEscape(id), []byte(offer), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("Content-Type", "application/sdp")
		})
		if err == nil && hardSpend && (obs.Status != 400 || obs.ErrorCode != "unsupported_policy" || calls.Load() != 0) {
			err = fmt.Errorf("hard spend must reject HTTP 400 unsupported_policy before SDP dispatch")
		}
		if err == nil && !hardSpend && (obs.Status != 201 || obs.ContentType != "application/sdp" || obs.Body != answer || calls.Load() != 1 || !valid.Load()) {
			err = fmt.Errorf("SDP offer/answer/status did not survive gateway setup exchange")
		}
	}
	return extResult(name, start, map[string]any{"response": obs, "upstream_calls": calls.Load(), "offer_verified": valid.Load(), "direct_media_exercised": false}, err)
}

// Bedrock qualification runs in a separately configured child process. Its
// dummy AWS chain and fixture CA are scoped to that child; host credentials and
// trust stores are never inherited.
func (e *environment) rtBedrockContracts() []result {
	out := make([]result, 0, 4)
	for _, mode := range []string{"signed-duplex", "half-close", "terminal-end", "terminal-error"} {
		out = append(out, e.rtBedrock(mode))
	}
	return out
}

type rtBedrockState struct {
	mu                     sync.Mutex
	requests               int
	signed                 bool
	http2                  bool
	chunkSignatureObserved bool
	inputChunks            int
	inputHalfClosed        bool
	outputChunks           int
	terminalErrorSent      bool
	fixtureError           string
}

func (s *rtBedrockState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"upstream_requests": s.requests, "sigv4_observed": s.signed,
		"http2_observed": s.http2, "chunk_signature_observed": s.chunkSignatureObserved,
		"input_chunks": s.inputChunks, "input_half_closed": s.inputHalfClosed,
		"output_chunks": s.outputChunks, "terminal_error_sent": s.terminalErrorSent,
		"fixture_error": s.fixtureError,
	}
}

func (e *environment) rtBedrock(mode string) result {
	start := time.Now()
	name := "realtime/bedrock/" + mode
	state := &rtBedrockState{}
	upstream := httptest.NewUnstartedServer(rtBedrockHandler(mode, state))
	upstream.EnableHTTP2 = true
	http2.ConfigureServer(upstream.Config, nil)
	upstream.StartTLS()
	defer upstream.Close()

	certPath, err := rtWriteFixtureCA(upstream)
	if err != nil {
		return extResult(name, start, state.snapshot(), err)
	}
	defer os.Remove(certPath)
	child, _, err := rtStartAWSChild(e, certPath)
	if err != nil {
		return extResult(name, start, state.snapshot(), err)
	}
	defer child.Close()
	finish := func(r result) result {
		if r.Status != "failed" {
			return r
		}
		diagnostics, diagnosticsErr := child.preserveDiagnostics("")
		evidence, ok := r.Evidence.(map[string]any)
		if !ok {
			evidence = map[string]any{}
			r.Evidence = evidence
		}
		if diagnosticsErr != nil {
			evidence["diagnostics_preserved"] = false
		} else if diagnostics != "" {
			evidence["diagnostics_dir"] = diagnostics
		}
		return r
	}
	id := rtID()
	key, err := rtSeedBedrock(child, id, upstream.URL)
	if err != nil {
		return finish(extResult(name, start, state.snapshot(), err))
	}
	body, err := rtBedrockInput()
	if err != nil {
		return finish(extResult(name, start, state.snapshot(), err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := "/connect/" + id + "/native/model/" + id + "/invoke-with-bidirectional-stream"
	obs, err := child.extRequest(ctx, false, http.MethodPost, path, body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/vnd.amazon.eventstream")
	})
	evidence := state.snapshot()
	evidence["response"] = obs
	evidence["descriptor"] = "POST /connect/{connection}/native/model/{model}/invoke-with-bidirectional-stream"
	evidence["credential_contract"] = "aws_chain with explicit dummy credentials and IMDS disabled"
	evidence["downstream_body_closed"] = err == nil
	if err == nil {
		if evidence["upstream_requests"] != 1 || !evidence["sigv4_observed"].(bool) || !evidence["http2_observed"].(bool) || !evidence["chunk_signature_observed"].(bool) {
			err = fmt.Errorf("Bedrock fixture did not observe one signed HTTP/2 request/chunk stream: %v", evidence)
		}
		if evidence["input_chunks"] != 1 || !evidence["input_half_closed"].(bool) {
			err = fmt.Errorf("Bedrock input was not delivered and half-closed: %v", evidence)
		}
		if err == nil && obs.Body != "" {
			chunk, decodeErr, terminal := rtDecodeBedrockOutput(obs.Body)
			evidence["downstream_chunk_bytes"] = chunk
			evidence["downstream_decode_error"] = decodeErr
			evidence["downstream_terminal_error_envelope"] = terminal
		}
		if err == nil {
			chunk, chunkOK := evidence["downstream_chunk_bytes"].(string)
			decodeErr, _ := evidence["downstream_decode_error"].(string)
			if !chunkOK || decodeErr != "" || chunk != "fixture duplex input" {
				err = fmt.Errorf("downstream Bedrock chunk was not decoded as expected: %v", evidence)
			}
		}
		terminalEnvelope, _ := evidence["downstream_terminal_error_envelope"].(bool)
		bodyClosed, _ := evidence["downstream_body_closed"].(bool)
		if mode == "terminal-error" {
			terminalSent, _ := evidence["terminal_error_sent"].(bool)
			if obs.Status < 200 || obs.Status >= 300 || evidence["output_chunks"] != 1 || !terminalSent || !bodyClosed || !terminalEnvelope {
				err = fmt.Errorf("terminal upstream error lacked committed output, closed body, or propagated native error evidence: %v", evidence)
			}
		} else if obs.Status < 200 || obs.Status >= 300 || evidence["output_chunks"] != 1 {
			err = fmt.Errorf("Bedrock duplex success lacked one output chunk: %v", evidence)
		}
	}
	return finish(extResult(name, start, evidence, err))
}

func rtBedrockHandler(mode string, state *rtBedrockState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		state.requests++
		state.signed = strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ")
		state.http2 = r.ProtoMajor == 2
		state.mu.Unlock()
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/model/") || !strings.HasSuffix(r.URL.Path, "/invoke-with-bidirectional-stream") {
			http.Error(w, "unexpected Bedrock route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		enc := es.NewEncoder()
		flush := func() { _ = http.NewResponseController(w).Flush() }
		write := func(msg es.Message) { _ = enc.Encode(w, msg); flush() }
		w.WriteHeader(http.StatusOK)
		flush()
		dec := es.NewDecoder()
		for {
			outer, err := dec.Decode(r.Body, nil)
			if err == io.EOF {
				state.mu.Lock()
				if !state.inputHalfClosed {
					state.fixtureError = "signed empty half-close frame was not observed"
				}
				state.mu.Unlock()
				return
			}
			if err != nil {
				state.mu.Lock()
				state.fixtureError = err.Error()
				state.mu.Unlock()
				return
			}
			if outer.Headers.Get(":chunk-signature") == nil {
				state.mu.Lock()
				state.fixtureError = "signed chunk omitted :chunk-signature"
				state.mu.Unlock()
				return
			}
			state.mu.Lock()
			state.chunkSignatureObserved = true
			state.mu.Unlock()
			if len(outer.Payload) == 0 {
				state.mu.Lock()
				state.inputHalfClosed = true
				state.mu.Unlock()
				if mode == "terminal-error" {
					state.mu.Lock()
					state.terminalErrorSent = true
					state.mu.Unlock()
					write(es.Message{Headers: es.Headers{
						{Name: ":message-type", Value: es.StringValue("exception")},
						{Name: ":exception-type", Value: es.StringValue("modelStreamErrorException")},
						{Name: ":content-type", Value: es.StringValue("application/json")},
					}, Payload: []byte(`{"message":"fixture terminal error"}`)})
				}
				return
			}
			inner, err := es.NewDecoder().Decode(bytes.NewReader(outer.Payload), nil)
			if err != nil {
				state.mu.Lock()
				state.fixtureError = "invalid inner Bedrock chunk: " + err.Error()
				state.mu.Unlock()
				return
			}
			eventType, ok := inner.Headers.Get(":event-type").(es.StringValue)
			if !ok || string(eventType) != "chunk" {
				state.mu.Lock()
				state.fixtureError = "invalid inner Bedrock chunk event"
				state.mu.Unlock()
				return
			}
			var input struct {
				Bytes []byte `json:"bytes"`
			}
			if json.Unmarshal(inner.Payload, &input) != nil || len(input.Bytes) == 0 {
				state.mu.Lock()
				state.fixtureError = "invalid inner Bedrock chunk event"
				state.mu.Unlock()
				return
			}
			state.mu.Lock()
			state.inputChunks++
			state.mu.Unlock()
			raw, _ := json.Marshal(map[string][]byte{"bytes": input.Bytes})
			write(es.Message{Headers: es.Headers{
				{Name: ":message-type", Value: es.StringValue("event")},
				{Name: ":event-type", Value: es.StringValue("chunk")},
				{Name: ":content-type", Value: es.StringValue("application/json")},
			}, Payload: raw})
			state.mu.Lock()
			state.outputChunks++
			state.mu.Unlock()
		}
	})
}
func rtWriteFixtureCA(server *httptest.Server) (string, error) {
	if server.TLS == nil || len(server.TLS.Certificates) == 0 || len(server.TLS.Certificates[0].Certificate) == 0 {
		return "", fmt.Errorf("TLS fixture did not expose a certificate")
	}
	der := server.TLS.Certificates[0].Certificate[0]
	if _, err := x509.ParseCertificate(der); err != nil {
		return "", fmt.Errorf("parse fixture certificate: %w", err)
	}
	f, err := os.CreateTemp("", "hoorific-realtime-ca-*.pem")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func rtStartAWSChild(parent *environment, certPath string) (*environment, func(), error) {
	child, err := newEnvironment("standalone", "", "normal")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		child.Close()
		_ = os.Remove(certPath)
	}
	emptyDir := filepath.Join(child.root, "empty-aws")
	if err = os.MkdirAll(emptyDir, 0700); err != nil {
		cleanup()
		return nil, nil, err
	}
	emptyFile := filepath.Join(emptyDir, "empty")
	if err = os.WriteFile(emptyFile, nil, 0600); err != nil {
		cleanup()
		return nil, nil, err
	}
	env := []string{
		"AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture-secret",
		"AWS_SESSION_TOKEN=", "AWS_REGION=us-east-1", "AWS_DEFAULT_REGION=us-east-1",
		"AWS_EC2_METADATA_DISABLED=true", "AWS_SHARED_CREDENTIALS_FILE=" + emptyFile,
		"AWS_CONFIG_FILE=" + emptyFile, "SSL_CERT_FILE=" + certPath,
		"SSL_CERT_DIR=" + emptyDir, "HOME=" + child.root, "TMPDIR=" + child.root,
	}
	child.binary = parent.binary
	migrate := exec.Command(parent.binary, "migrate", "--config", child.config)
	migrate.Dir, migrate.Env = child.root, env
	if raw, e := migrate.CombinedOutput(); e != nil {
		cleanup()
		return nil, nil, fmt.Errorf("Bedrock child migrate failed: %w: %s", e, trim(string(raw)))
	}
	logPath := filepath.Join(child.root, "serve.log")
	log, e := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		cleanup()
		return nil, nil, e
	}
	child.server = exec.Command(parent.binary, "serve", "--config", child.config)
	child.server.Dir, child.server.Env = child.root, env
	child.server.Stdout, child.server.Stderr = log, log
	if err = child.server.Start(); err != nil {
		_ = log.Close()
		cleanup()
		return nil, nil, err
	}
	_ = log.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, e := child.client.Get("http://" + child.management + "/health/live")
		if e == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				boot := child.bootstrap()
				if boot.Status != "passed" {
					cleanup()
					return nil, nil, fmt.Errorf("Bedrock child bootstrap failed: %s", boot.Detail)
				}
				return child, cleanup, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	cleanup()
	return nil, nil, fmt.Errorf("Bedrock child health did not become live")
}

func rtSeedBedrock(e *environment, id, endpoint string) (string, error) {
	settings := map[string]string{
		"auth_mode": "aws_chain", "allow_private": "true",
		"allowed_cidrs": "127.0.0.1/32",
	}
	if err := e.extCreate("connections", id, map[string]any{
		"connector": "bedrock", "account_id": id, "base_url": endpoint,
		"region": "us-east-1", "dedicated": true, "enabled": true, "settings": settings,
	}); err != nil {
		return "", err
	}
	if err := e.extCreate("models", id, map[string]any{
		"connection_id": id, "upstream_id": id, "operations": []string{"audio.speech", "realtime"},
		"input_modalities": []string{"audio"}, "output_modalities": []string{"audio"},
		"enabled": true, "provenance": "isolated signed Bedrock fixture",
	}); err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"data": map[string]any{
		"name": id, "role": "operator",
		"permissions": []string{"inference:invoke", "connection:" + id},
		"connections": []string{id}, "operations": []string{"audio.speech", "realtime"},
		"portable": false, "native_account": true, "realtime": true,
	}})
	if err != nil {
		return "", err
	}
	obs, err := e.extRequest(context.Background(), true, http.MethodPost, "/admin/api/v1/api_keys/"+id+"/issue", raw, nil)
	if err != nil {
		return "", err
	}
	var issued struct {
		Token string `json:"token"`
	}
	if obs.Status != http.StatusOK || json.Unmarshal([]byte(obs.Body), &issued) != nil || issued.Token == "" {
		return "", fmt.Errorf("Bedrock key issue HTTP %d", obs.Status)
	}
	return issued.Token, nil
}

func rtBedrockInput() ([]byte, error) {
	var b bytes.Buffer
	enc := es.NewEncoder()
	payload, _ := json.Marshal(map[string][]byte{"bytes": []byte("fixture duplex input")})
	err := enc.Encode(&b, es.Message{Headers: es.Headers{
		{Name: ":message-type", Value: es.StringValue("event")},
		{Name: ":event-type", Value: es.StringValue("chunk")},
		{Name: ":content-type", Value: es.StringValue("application/json")},
	}, Payload: payload})
	return b.Bytes(), err
}
func rtDecodeBedrockOutput(raw string) (string, string, bool) {
	dec := es.NewDecoder()
	reader := bytes.NewReader([]byte(raw))
	var chunk string
	var terminal bool
	for {
		msg, err := dec.Decode(reader, nil)
		if err == io.EOF {
			return chunk, "", terminal
		}
		if err != nil {
			return chunk, err.Error(), terminal
		}
		messageType, _ := msg.Headers.Get(":message-type").(es.StringValue)
		if string(messageType) == "exception" {
			terminal = true
			continue
		}
		eventType, ok := msg.Headers.Get(":event-type").(es.StringValue)
		if !ok {
			return chunk, "downstream event omitted :event-type", terminal
		}
		switch string(eventType) {
		case "chunk":
			fields := map[string][]byte{}
			if err := json.Unmarshal(msg.Payload, &fields); err != nil {
				return chunk, err.Error(), terminal
			}
			chunk = string(fields["bytes"])
		case "model-stream-error":
			terminal = true
		}
	}
}
