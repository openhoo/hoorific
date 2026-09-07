package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// These fixtures exercise the running gateway, not connector methods. Each owns
// its upstream and resources; no shared fixture mode or inference key is changed.
type operationFixture struct {
	e              *environment
	server         *httptest.Server
	id, key, alias string
	mu             sync.Mutex
	calls          map[string]int
	faults         []string
	handler        func(http.ResponseWriter, *http.Request) error
}

type operationReply struct {
	status int
	header http.Header
	body   []byte
}

func (e *environment) operationFixture(name, provider string, operations []string, handler func(http.ResponseWriter, *http.Request) error) (*operationFixture, error) {
	f := &operationFixture{e: e, id: fmt.Sprintf("verify-op-%s-%d", name, time.Now().UnixNano()), calls: map[string]int{}, handler: handler}
	f.alias = f.id + "-alias"
	// Every fixture imports an API key, so assert that provider's exact wire
	// scheme rather than accepting another provider's credential placement.
	authHeader, authPrefix := "Authorization", ""
	switch provider {
	case "openai", "cohere", "replicate":
		authPrefix = "Bearer "
	case "anthropic":
		authHeader = "X-Api-Key"
	case "fal":
		authPrefix = "Key "
	case "gemini":
		authHeader = "X-Goog-Api-Key"
	case "azure", "azure-openai":
		authHeader = "Api-Key"
	default:
		return nil, fmt.Errorf("fixture has no exact API-key contract for provider %q", provider)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[r.Method+" "+r.URL.Path]++
		if r.Header.Get("Authorization") == "Bearer "+f.key && f.key != "" {
			f.faults = append(f.faults, "gateway key leaked upstream")
			http.Error(w, "credential leak", 500)
			return
		}
		secret := "verify-operation-secret"
		values := r.Header.Values(authHeader)
		authorized := len(values) == 1 && values[0] == authPrefix+secret
		for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"} {
			if header != authHeader && len(r.Header.Values(header)) != 0 {
				authorized = false
			}
		}
		if !authorized {
			f.faults = append(f.faults, "upstream credential missing or incorrect")
			http.Error(w, "missing fixture credential", 401)
			return
		}
		if err := f.handler(w, r); err != nil {
			f.faults = append(f.faults, err.Error())
			http.Error(w, err.Error(), 422)
		}
	}))
	settings := map[string]string{
		"allow_private": "true", "allowed_cidrs": "127.0.0.1/32",
		"credential_header": authHeader, "credential_prefix": strings.TrimSpace(authPrefix),
	}
	if provider == "fal" {
		settings["endpoint"] = "owner/app"
	}
	resources := []struct {
		kind, id string
		data     any
	}{
		{"connections", f.id, map[string]any{"connector": provider, "account_id": f.id, "base_url": f.server.URL, "dedicated": true, "enabled": true, "settings": settings}},
		{"models", f.id + "-model", map[string]any{"connection_id": f.id, "upstream_id": "fixture-model", "operations": operations, "input_modalities": []string{"text", "image", "audio"}, "output_modalities": []string{"text", "image", "audio"}, "provenance": "isolated operation fixture", "enabled": true}},
		{"model_aliases", f.alias, map[string]any{"model_ids": []string{f.id + "-model"}, "enabled": true}},
		{"route_policies", f.alias, map[string]any{"alias": f.alias, "targets": []any{map[string]any{"connection_id": f.id, "model_id": f.id + "-model", "priority": 1, "weight": 100}}, "fallback": false, "affinity": false}},
	}
	for _, resource := range resources {
		if err := e.extCreate(resource.kind, resource.id, resource.data); err != nil {
			f.server.Close()
			return nil, err
		}
	}
	if err := e.extImportCredential(f.id, provider, f.id, "verify-operation-secret"); err != nil {
		f.server.Close()
		return nil, err
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"name": f.id, "role": "operator", "permissions": []string{"inference:invoke", "connection:" + f.id}, "aliases": []string{f.alias}, "connections": []string{f.id}, "operations": operations, "portable": true, "native_account": true, "realtime": false}})
	obs, err := e.extRequest(context.Background(), true, "POST", "/admin/api/v1/api_keys/"+f.id+"/issue", raw, nil)
	var issued struct {
		Token string `json:"token"`
	}
	if err == nil && (obs.Status != 200 || json.Unmarshal([]byte(obs.Body), &issued) != nil || issued.Token == "") {
		err = fmt.Errorf("operation grants issuance HTTP %d; token omitted", obs.Status)
	}
	if err != nil {
		f.server.Close()
		return nil, err
	}
	f.key = issued.Token
	return f, nil
}

func (f *operationFixture) request(method, path, contentType string, body []byte) (operationReply, error) {
	return f.requestHeaders(method, path, contentType, body, nil)
}

func (f *operationFixture) requestHeaders(method, path, contentType string, body []byte, headers map[string]string) (operationReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !strings.HasPrefix(path, "/") {
		path = "/connect/" + f.id + "/native/" + path
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://"+f.e.inference+path, bytes.NewReader(body))
	if err != nil {
		return operationReply{}, err
	}
	r.Header.Set("Authorization", "Bearer "+f.key)
	r.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	client := *f.e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(r)
	if err != nil {
		return operationReply{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	out := operationReply{resp.StatusCode, resp.Header.Clone(), b}
	if err == nil && len(b) > 4<<20 {
		err = fmt.Errorf("response exceeded fixture bound")
	}
	return out, err
}

func operationJSON(w http.ResponseWriter, value any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(value)
}

func operationObject(r *http.Request, fields map[string]any) error {
	var got map[string]any
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		return err
	}
	for key, want := range fields {
		b, _ := json.Marshal(want)
		var normalized any
		_ = json.Unmarshal(b, &normalized)
		if !reflect.DeepEqual(got[key], normalized) {
			return fmt.Errorf("upstream field %s: got %v want %v", key, got[key], normalized)
		}
	}
	return nil
}

func operationFields(reply operationReply, fields map[string]any) error {
	if reply.status < 200 || reply.status >= 300 {
		return fmt.Errorf("gateway HTTP %d code=%s body=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"), trim(string(reply.body)))
	}
	return operationObject(&http.Request{Body: io.NopCloser(bytes.NewReader(reply.body))}, fields)
}

func operationMultipart(fields map[string]string, field, filename string, data []byte) ([]byte, string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	p, err := w.CreateFormFile(field, filename)
	if err != nil {
		return nil, "", err
	}
	if _, err = p.Write(data); err != nil {
		return nil, "", err
	}
	if err = w.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), w.FormDataContentType(), nil
}

func operationUpload(r *http.Request, fields map[string]string, field, filename string, data []byte) error {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return err
	}
	defer r.MultipartForm.RemoveAll()
	for k, v := range fields {
		if r.FormValue(k) != v {
			return fmt.Errorf("multipart %s differs", k)
		}
	}
	file, h, err := r.FormFile(field)
	if err != nil {
		return err
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if h.Filename != filename || !bytes.Equal(got, data) {
		return fmt.Errorf("multipart filename or bytes changed")
	}
	return nil
}

func (e *environment) operationRun(name, provider string, operations []string, handler func(http.ResponseWriter, *http.Request) error, exercise func(*operationFixture) error) result {
	verificationProgress("operations-"+name, "started", 0)
	start := time.Now()
	f, err := e.operationFixture(name, provider, operations, handler)
	if err != nil {
		return extResult("operations-"+name, start, nil, fmt.Errorf("fixture setup: %w", err))
	}
	defer f.server.Close()
	err = exercise(f)
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := map[string]int{}
	for k, v := range f.calls {
		calls[k] = v
	}
	if len(f.faults) != 0 {
		err = fmt.Errorf("%s fixture contract failure: %s; gateway outcome: %v", name, strings.Join(f.faults, "; "), err)
	} else if err == nil && len(calls) == 0 {
		err = fmt.Errorf("%s fixture produced no upstream operation", name)
	} else if err != nil {
		if strings.HasPrefix(err.Error(), "fixture isolation") {
			err = fmt.Errorf("%s fixture isolation gap: %w", name, err)
		} else {
			err = fmt.Errorf("%s production operation gap: %w", name, err)
		}
	}
	return extResult("operations-"+name, start, map[string]any{"upstream_calls": calls, "upstream_contract_errors": f.faults}, err)
}
func (e *environment) operationsSuccess() []result {
	out := []result{}
	out = append(out, e.operationEmbeddings())
	out = append(out, e.operationRerank())
	out = append(out, e.operationImages())
	out = append(out, e.operationSpeech())
	out = append(out, e.operationMultipartAudio())
	out = append(out, e.operationAsyncVideo())
	out = append(out, e.operationCollisionIsolation())
	out = append(out, e.operationJSONLBatch())
	out = append(out, e.operationReplicateLifecycle())
	out = append(out, e.falNamespace()...)
	out = append(out, e.operationGeminiUpload())
	return out
}
func (e *environment) operationEmbeddings() result {
	return e.operationRun("embeddings", "openai", []string{"embed"}, func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if body["model"] == nil || body["input"] == nil || body["dimensions"] != float64(2) || (body["encoding_format"] != "base64" && body["encoding_format"] != "float") {
			return fmt.Errorf("embedding request lost model/input/dimensions/encoding_format: %v", body)
		}
		if body["encoding_format"] == "float" {
			return operationJSON(w, map[string]any{"object": "list", "model": "fixture-embed", "data": []any{map[string]any{"object": "embedding", "index": 1, "embedding": []float64{0.25, -0.5}}}})
		}
		raw := make([]byte, 8)
		binary.LittleEndian.PutUint32(raw[0:4], math.Float32bits(0.25))
		binary.LittleEndian.PutUint32(raw[4:8], math.Float32bits(-0.5))
		return operationJSON(w, map[string]any{"object": "list", "model": "fixture-embed", "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": base64.StdEncoding.EncodeToString(raw)}}})
	}, func(f *operationFixture) error {
		base := `{"model":"` + f.alias + `","input":["alpha"],"dimensions":2,"encoding_format":"`
		floatReply, err := f.request("POST", "/v1/embeddings", "application/json", []byte(base+`float"}`))
		if err != nil {
			return err
		}
		if floatReply.status != 200 {
			return fmt.Errorf("embedding float status %d code=%s", floatReply.status, floatReply.header.Get("X-Hoorific-Error-Code"))
		}
		var floats struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(floatReply.body, &floats); err != nil {
			return err
		}
		if len(floats.Data) != 1 || floats.Data[0].Index != 1 || !reflect.DeepEqual(floats.Data[0].Embedding, []float64{.25, -.5}) {
			return fmt.Errorf("embedding float dimensions/values changed: %s", floatReply.body)
		}
		reply, err := f.request("POST", "/v1/embeddings", "application/json", []byte(base+`base64"}`))
		if err != nil {
			return err
		}
		if reply.status != 200 {
			return fmt.Errorf("embedding base64 status %d code=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"))
		}
		var got struct {
			Data []struct {
				Index     int    `json:"index"`
				Embedding string `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(reply.body, &got); err != nil {
			return err
		}
		if len(got.Data) != 1 || got.Data[0].Index != 0 {
			return fmt.Errorf("embedding index not preserved: %s", reply.body)
		}
		decoded, err := base64.StdEncoding.DecodeString(got.Data[0].Embedding)
		if err != nil || len(decoded) != 8 {
			return fmt.Errorf("embedding base64 bytes invalid: %v", err)
		}
		if math.Float32frombits(binary.LittleEndian.Uint32(decoded[:4])) != .25 || math.Float32frombits(binary.LittleEndian.Uint32(decoded[4:])) != -.5 {
			return fmt.Errorf("embedding decoded values changed")
		}
		return nil
	})
}

func (e *environment) operationRerank() result {
	return e.operationRun("rerank", "cohere", []string{"rerank"}, func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if body["model"] == nil || body["query"] != "needle" {
			return fmt.Errorf("rerank request fields changed: %v", body)
		}
		return operationJSON(w, map[string]any{"results": []any{map[string]any{"index": 2, "relevance_score": 0.91}, map[string]any{"index": 0, "relevance_score": 0.12}}})
	}, func(f *operationFixture) error {
		reply, err := f.request("POST", "/v1/rerank", "application/json", []byte(`{"model":"`+f.alias+`","query":"needle","documents":["a","b","c"],"top_n":2}`))
		if err != nil {
			return err
		}
		if reply.status != 200 {
			return fmt.Errorf("rerank gateway status %d code=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"))
		}
		var got struct {
			Results []struct {
				Index int     `json:"index"`
				Score float64 `json:"relevance_score"`
			} `json:"results"`
		}
		if err := json.Unmarshal(reply.body, &got); err != nil {
			return err
		}
		if len(got.Results) != 2 || got.Results[0].Index != 2 || got.Results[1].Index != 0 || got.Results[0].Score != .91 {
			return fmt.Errorf("rerank indexes/scores changed: %s", reply.body)
		}
		return nil
	})
}

func (e *environment) operationImages() result {
	return e.operationRun("imagegen-edit", "openai", []string{"image.generate", "image.edit"}, func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if r.URL.Path == "/v1/images/generations" && body["prompt"] != "draw a square" {
			return fmt.Errorf("image prompt changed")
		}
		if r.URL.Path == "/v1/images/edits" && body["image"] != "data:image/png;base64,AAAA" {
			return fmt.Errorf("image edit source changed")
		}
		return operationJSON(w, map[string]any{"created": 1700000000, "data": []any{map[string]any{"b64_json": "aW1hZ2UtZml4dHVyZQ=="}}})
	}, func(f *operationFixture) error {
		for path, body := range map[string]string{"/v1/images/generations": `{"model":"` + f.alias + `","prompt":"draw a square"}`, "/v1/images/edits": `{"model":"` + f.alias + `","image":"data:image/png;base64,AAAA","prompt":"edit"}`} {
			reply, err := f.request("POST", path, "application/json", []byte(body))
			if err != nil {
				return err
			}
			if reply.status != 200 {
				return fmt.Errorf("%s status %d code=%s", path, reply.status, reply.header.Get("X-Hoorific-Error-Code"))
			}
			var got struct {
				Data []struct {
					B64 string `json:"b64_json"`
				} `json:"data"`
			}
			if json.Unmarshal(reply.body, &got) != nil || len(got.Data) != 1 {
				return fmt.Errorf("%s image response malformed", path)
			}
			b, err := base64.StdEncoding.DecodeString(got.Data[0].B64)
			if err != nil || string(b) != "image-fixture" {
				return fmt.Errorf("%s image bytes changed", path)
			}
		}
		return nil
	})
}

func (e *environment) operationSpeech() result {
	return e.operationRun("speech", "openai", []string{"audio.speech"}, func(w http.ResponseWriter, r *http.Request) error {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return err
		}
		if body["input"] != "say fixture" {
			return fmt.Errorf("speech input changed")
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, err := w.Write([]byte{0, 1, 2, 255, 4})
		return err
	}, func(f *operationFixture) error {
		reply, err := f.request("POST", "/v1/audio/speech", "application/json", []byte(`{"model":"`+f.alias+`","voice":"alloy","input":"say fixture","response_format":"mp3"}`))
		if err != nil {
			return err
		}
		if reply.status != 200 || !bytes.Equal(reply.body, []byte{0, 1, 2, 255, 4}) {
			return fmt.Errorf("speech bytes/lifecycle changed: HTTP %d body=%x", reply.status, reply.body)
		}
		return nil
	})
}

func (e *environment) operationMultipartAudio() result {
	return e.operationRun("multipart-audio", "openai", []string{"audio.transcribe", "audio.translate"}, func(w http.ResponseWriter, r *http.Request) error {
		if err := operationUpload(r, nil, "file", "fixture.wav", []byte("RIFF-fixture")); err != nil {
			return err
		}
		if r.URL.Path == "/audio/transcriptions" && r.FormValue("language") != "en" {
			return fmt.Errorf("transcription language was lost")
		}
		if r.FormValue("model") == "" {
			return fmt.Errorf("multipart model was not rewritten")
		}
		return operationJSON(w, map[string]any{"text": "decoded fixture audio"})
	}, func(f *operationFixture) error {
		for path, fields := range map[string]map[string]string{"/v1/audio/transcriptions": {"model": f.alias, "language": "en"}, "/v1/audio/translations": {"model": f.alias}} {
			body, contentType, err := operationMultipart(fields, "file", "fixture.wav", []byte("RIFF-fixture"))
			if err != nil {
				return err
			}
			reply, err := f.request("POST", path, contentType, body)
			if err != nil {
				return err
			}
			if reply.status != 200 {
				return fmt.Errorf("%s status %d code=%s", path, reply.status, reply.header.Get("X-Hoorific-Error-Code"))
			}
			var got struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(reply.body, &got) != nil || got.Text != "decoded fixture audio" {
				return fmt.Errorf("%s audio text changed", path)
			}
		}
		return nil
	})
}
func (e *environment) operationAsyncVideo() result {
	return e.operationRun("asyncvideo", "openai", []string{"video"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/videos" && r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["prompt"] != "fixture video" {
				return fmt.Errorf("video prompt changed")
			}
			return operationJSON(w, map[string]any{"id": "collision-video", "status": "queued"})
		}
		if r.URL.Path == "/videos/collision-video" && r.Method == http.MethodGet {
			return operationJSON(w, map[string]any{"id": "collision-video", "status": "completed"})
		}
		return fmt.Errorf("unexpected video lifecycle route %s %s", r.Method, r.URL.Path)
	}, func(f *operationFixture) error {
		reply, err := f.request("POST", "/connect/"+f.id+"/openai/v1/videos", "application/json", []byte(`{"prompt":"fixture video"}`))
		if err != nil {
			return err
		}
		if reply.status != 200 {
			return fmt.Errorf("video submit status %d code=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"))
		}
		var got struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(reply.body, &got); err != nil {
			return err
		}
		if got.ID != "collision-video" || got.Status != "queued" {
			return fmt.Errorf("video lifecycle ack changed: %s", reply.body)
		}
		reply, err = f.request("GET", "/connect/"+f.id+"/openai/v1/videos/collision-video", "application/json", nil)
		if err != nil {
			return err
		}
		if reply.status != 200 || !bytes.Contains(reply.body, []byte(`"status":"completed"`)) {
			return fmt.Errorf("video retrieval lifecycle changed: HTTP %d %s", reply.status, reply.body)
		}
		return nil
	})
}

func (e *environment) operationJSONLBatch() result {
	return e.operationRun("jsonl-batch", "anthropic", []string{"batch"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/messages/batches" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			requests, ok := body["requests"].([]any)
			if !ok || len(requests) != 1 {
				return fmt.Errorf("JSONL requests missing")
			}
			row, ok := requests[0].(map[string]any)
			if !ok || row["custom_id"] != "row-1" {
				return fmt.Errorf("JSONL custom_id changed: %v", body)
			}
			return operationJSON(w, map[string]any{"id": "batch-jsonl", "processing_status": "in_progress"})
		}
		if r.URL.Path == "/messages/batches/batch-jsonl/results" {
			w.Header().Set("Content-Type", "application/jsonl")
			_, err := io.WriteString(w, `{"custom_id":"row-1","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"ok"}]}}}`+"\n")
			return err
		}
		return fmt.Errorf("unexpected JSONL batch route %s", r.URL.Path)
	}, func(f *operationFixture) error {
		body := []byte(`{"requests":[{"custom_id":"row-1","params":{"model":"fixture-model","max_tokens":1,"messages":[{"role":"user","content":"ok"}]}}]}`)
		reply, err := f.request("POST", "/connect/"+f.id+"/anthropic/v1/messages/batches", "application/json", body)
		if err != nil {
			return err
		}
		if reply.status != 200 {
			return fmt.Errorf("batch submit status %d code=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"))
		}
		if !bytes.Contains(reply.body, []byte("batch-jsonl")) {
			return fmt.Errorf("batch ID missing")
		}
		reply, err = f.request("GET", "/connect/"+f.id+"/anthropic/v1/messages/batches/batch-jsonl/results", "application/json", nil)
		if err != nil {
			return err
		}
		if reply.status != 200 || !bytes.Contains(reply.body, []byte(`"custom_id":"row-1"`)) {
			return fmt.Errorf("JSONL result/custom_id changed: HTTP %d %s", reply.status, reply.body)
		}
		return nil
	})
}
func (e *environment) operationCollisionIsolation() result {
	verificationProgress("operations-collision-isolation", "started", 0)
	start := time.Now()
	handler := func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/same-upstream-id") {
			return operationJSON(w, map[string]any{"id": "same-upstream-id", "status": "completed"})
		}
		return operationJSON(w, map[string]any{"id": "same-upstream-id", "status": "queued"})
	}
	f1, err := e.operationFixture("collision-a", "openai", []string{"video"}, handler)
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	defer f1.server.Close()
	f2, err := e.operationFixture("collision-b", "openai", []string{"video"}, handler)
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	defer f2.server.Close()
	r1, err := f1.request("POST", "/connect/"+f1.id+"/openai/v1/videos", "application/json", []byte(`{"prompt":"a"}`))
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	r2, err := f2.request("POST", "/connect/"+f2.id+"/openai/v1/videos", "application/json", []byte(`{"prompt":"b"}`))
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	if r1.status != 200 || r2.status != 200 || !bytes.Contains(r1.body, []byte("same-upstream-id")) || !bytes.Contains(r2.body, []byte("same-upstream-id")) {
		return extResult("operations-collision-isolation", start, map[string]any{"a": r1.status, "b": r2.status}, fmt.Errorf("same upstream ID lifecycle was not independently acknowledged"))
	}
	g1, err := f1.request("GET", "/connect/"+f1.id+"/openai/v1/videos/same-upstream-id", "application/json", nil)
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	g2, err := f2.request("GET", "/connect/"+f2.id+"/openai/v1/videos/same-upstream-id", "application/json", nil)
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	if g1.status != 200 || g2.status != 200 || !bytes.Contains(g1.body, []byte(`"status":"completed"`)) || !bytes.Contains(g2.body, []byte(`"status":"completed"`)) {
		return extResult("operations-collision-isolation", start, map[string]any{"get_a": g1.status, "get_b": g2.status}, fmt.Errorf("same upstream ID retrieval was not owner-isolated"))
	}
	cross, err := f1.request("POST", "/connect/"+f2.id+"/openai/v1/videos", "application/json", []byte(`{"prompt":"cross"}`))
	if err != nil {
		return extResult("operations-collision-isolation", start, nil, err)
	}
	if cross.status != http.StatusForbidden || cross.header.Get("X-Hoorific-Error-Code") != "forbidden" {
		return extResult("operations-collision-isolation", start, map[string]any{"cross_status": cross.status, "cross_body": trim(string(cross.body))}, fmt.Errorf("connection/tenant collision isolation gap"))
	}
	return extResult("operations-collision-isolation", start, map[string]any{"same_upstream_id": "same-upstream-id", "owner_a": f1.id, "owner_b": f2.id, "retrieval_a": g1.status, "retrieval_b": g2.status, "cross_status": cross.status}, nil)
}

func (e *environment) operationReplicateLifecycle() result {
	return e.operationRun("replicate-result-cancel", "replicate", []string{"prediction.create", "prediction.get", "prediction.cancel"}, func(w http.ResponseWriter, r *http.Request) error {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/predictions":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return err
			}
			if body["version"] == nil || body["input"] == nil {
				return fmt.Errorf("Replicate version/input changed")
			}
			return operationJSON(w, map[string]any{"id": "same-upstream-id", "status": "starting"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/predictions/same-upstream-id":
			return operationJSON(w, map[string]any{"id": "same-upstream-id", "status": "succeeded", "output": "fixture-result"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/predictions/same-upstream-id/cancel":
			return operationJSON(w, map[string]any{"id": "same-upstream-id", "status": "canceled"})
		default:
			return fmt.Errorf("unexpected Replicate lifecycle route %s %s", r.Method, r.URL.Path)
		}
	}, func(f *operationFixture) error {
		reply, err := f.request("POST", "/connect/"+f.id+"/native/v1/predictions", "application/json", []byte(`{"version":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","input":{"x":1}}`))
		if err != nil {
			return err
		}
		if reply.status != 200 {
			return fmt.Errorf("Replicate submit status %d code=%s body=%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"), trim(string(reply.body)))
		}
		if !bytes.Contains(reply.body, []byte(`"id":"same-upstream-id"`)) || !bytes.Contains(reply.body, []byte(`"status":"starting"`)) {
			return fmt.Errorf("Replicate submit acknowledgement changed: %s", reply.body)
		}
		reply, err = f.request("GET", "/connect/"+f.id+"/native/v1/predictions/same-upstream-id", "application/json", nil)
		if err != nil {
			return err
		}
		if reply.status != 200 || !bytes.Contains(reply.body, []byte(`"status":"succeeded"`)) || !bytes.Contains(reply.body, []byte("fixture-result")) {
			return fmt.Errorf("Replicate result lifecycle changed: HTTP %d %s", reply.status, reply.body)
		}
		reply, err = f.request("POST", "/connect/"+f.id+"/native/v1/predictions/same-upstream-id/cancel", "application/json", nil)
		if err != nil {
			return err
		}
		if reply.status != 200 || !bytes.Contains(reply.body, []byte(`"status":"canceled"`)) {
			return fmt.Errorf("Replicate cancel lifecycle changed: HTTP %d %s", reply.status, reply.body)
		}
		return nil
	})
}

func (e *environment) operationGeminiUpload() result {
	return e.operationRun("gemini-upload-continuation", "gemini", []string{"upload"}, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/upload/v1beta/files" {
			if r.Header.Get("X-Goog-Upload-Protocol") != "resumable" {
				return fmt.Errorf("upload protocol missing")
			}
			w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/v1beta/files/continue-fixture")
			w.WriteHeader(http.StatusOK)
			return nil
		}
		if strings.HasSuffix(r.URL.Path, "/continue-fixture") {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return err
			}
			if !bytes.Equal(b, []byte("continuation-bytes")) {
				return fmt.Errorf("continuation bytes changed")
			}
			return operationJSON(w, map[string]any{"file": map[string]any{"name": "files/fixture", "state": "ACTIVE"}})
		}
		return fmt.Errorf("unexpected Gemini upload path %s", r.URL.Path)
	}, func(f *operationFixture) error {
		failureEvidence := func(step, path string, reply operationReply) error {
			f.mu.Lock()
			startCalls := f.calls["POST /upload/v1beta/files"]
			continuationCalls := f.calls["POST /upload/v1beta/files/continue-fixture"]
			f.mu.Unlock()
			code := reply.header.Get("X-Hoorific-Error-Code")
			if len(code) > 128 {
				code = "[invalid error code]"
			}
			for _, ch := range code {
				if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
					code = "[invalid error code]"
					break
				}
			}
			return fmt.Errorf("Gemini step=%s HTTP=%d code=%q path=%q local_continuation_path=/upload/v1beta/files/continue-fixture start_calls=%d continuation_calls=%d", step, reply.status, code, path, startCalls, continuationCalls)
		}
		reply, err := f.requestHeaders("POST", "/connect/"+f.id+"/gemini/upload/v1beta/files", "application/json", []byte(`{"file":{"display_name":"fixture"}}`), map[string]string{"X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start"})
		if err != nil {
			return err
		}
		if reply.status != 200 {
			return failureEvidence("upload-start", "/upload/v1beta/files", reply)
		}
		continuation := reply.header.Get("X-Goog-Upload-URL")
		if continuation == "" {
			return fmt.Errorf("Gemini upload continuation URL missing")
		}
		u, err := url.Parse(continuation)
		if err != nil || strings.Contains(u.Host, "generativelanguage.googleapis.com") || !strings.Contains(u.Path, "/connect/"+f.id+"/continuations/") {
			return fmt.Errorf("Gemini continuation URL was not gateway-owned (URL omitted)")
		}
		reply, err = f.requestHeaders("POST", u.Path, "application/octet-stream", []byte("continuation-bytes"), map[string]string{"X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"})
		if err != nil {
			return err
		}
		if reply.status != 200 || !bytes.Contains(reply.body, []byte("files/fixture")) {
			return failureEvidence("upload-finalize", u.Path, reply)
		}
		return nil
	})
}
