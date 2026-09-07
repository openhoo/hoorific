package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type target struct{ Name, URL, Key, Model string }
type workload struct {
	Name                          string
	InputBytes, Concurrency, Rate int
	Stream, Slow                  bool
}
type sample struct {
	Error                    string
	ErrorDetail              string
	TotalMs, TTFTMs, QueueMs float64
}

func workloads() []workload {
	return []workload{{"unary_1k_1000rps_c64", 1024, 64, 1000, false, false}, {"unary_64k_1000rps_c64", 64 * 1024, 64, 1000, false, false}, {"sse_1k_1000rps_c256", 1024, 256, 1000, true, false}, {"sse_64k_1000rps_c256", 64 * 1024, 256, 1000, true, false}, {"closed_loop_sse_1k_c1", 1024, 1, 0, true, false}, {"closed_loop_sse_1k_c64", 1024, 64, 0, true, false}, {"closed_loop_sse_64k_c1", 64 * 1024, 1, 0, true, false}, {"closed_loop_sse_64k_c64", 64 * 1024, 64, 0, true, false}, {"slow_sse_1000", 1024, 1000, 0, true, true}}
}

// Pad the encoded request, not just the prompt, to the contract's wire size.
func requestBody(t target, w workload, prefix string) ([]byte, error) {
	input := prefix
	if w.Slow {
		input += ":STALL:"
	}
	payload := map[string]any{"model": t.Model, "stream": w.Stream, "max_tokens": 1024, "messages": []map[string]string{{"role": "user", "content": input}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(body) > w.InputBytes {
		return nil, fmt.Errorf("request metadata exceeds payload size")
	}
	payload["messages"].([]map[string]string)[0]["content"] += strings.Repeat("i", w.InputBytes-len(body))
	return json.Marshal(payload)
}

func request(ctx context.Context, client *http.Client, t target, w workload, prefix string, start time.Time, timeout time.Duration) (s sample) {
	begin := time.Now()
	s.QueueMs = float64(begin.Sub(start)) / float64(time.Millisecond)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, e := requestBody(t, w, prefix)
	if e != nil {
		s.Error = "encode"
		return
	}
	req, e := http.NewRequestWithContext(cctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if e != nil {
		s.Error = "request"
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Key != "" {
		req.Header.Set("Authorization", "Bearer "+t.Key)
	}
	resp, e := client.Do(req)
	if e != nil {
		s.Error = "transport"
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.Error = fmt.Sprintf("http_%d", resp.StatusCode)
		if w.Name == "preflight" {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			var failure struct {
				Error         struct{ Code, Message string } `json:"error"`
				Code, Message string
			}
			if json.Unmarshal(raw, &failure) == nil {
				code, message := failure.Error.Code, failure.Error.Message
				if code == "" {
					code = failure.Code
				}
				if message == "" {
					message = failure.Message
				}
				detail := code + ": " + message
				for _, secret := range []string{t.Key, "isolated-fixture-only"} {
					if secret != "" {
						detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
					}
				}
				if len(detail) > 512 {
					detail = detail[:512]
				}
				s.ErrorDetail = detail
			}
		}
		return
	}
	if !w.Stream {
		var v struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if e = json.NewDecoder(resp.Body).Decode(&v); e != nil {
			s.Error = "json"
			return
		}
		if len(v.Choices) == 0 || len(v.Choices[0].Message.Content) != 1024 || !strings.HasPrefix(v.Choices[0].Message.Content, prefix) {
			s.Error = "output_identity"
			return
		}
		s.TotalMs = float64(time.Since(begin)) / float64(time.Millisecond)
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	events := 0
	done := false
	finish := false
	var output strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var v struct {
			Choices []struct {
				Finish *string `json:"finish_reason"`
				Delta  struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &v) != nil || len(v.Error) > 0 {
			s.Error = "sse_event"
			return
		}
		if len(v.Choices) == 0 {
			continue
		}
		if v.Choices[0].Finish != nil {
			finish = true
		}
		if v.Choices[0].Delta.Content != "" {
			events++
			if output.Len()+len(v.Choices[0].Delta.Content) > 1024 {
				s.Error = "sse_size"
				return
			}
			output.WriteString(v.Choices[0].Delta.Content)
			if s.TTFTMs == 0 {
				s.TTFTMs = float64(time.Since(begin)) / float64(time.Millisecond)
			}
		}
	}
	if scanner.Err() != nil {
		s.Error = "sse_read"
		return
	}
	if !done || !finish || events != 128 || output.Len() != 1024 || !strings.HasPrefix(output.String(), prefix) {
		s.Error = "sse_identity"
		return
	}
	s.TotalMs = float64(time.Since(begin)) / float64(time.Millisecond)
	return
}

func measure(ctx context.Context, client *http.Client, t target, w workload, prefix string, window, timeout time.Duration, pid int) caseResult {
	if w.Slow {
		return measureSoak(ctx, client, t, w, prefix, window, timeout, pid)
	}
	r := caseResult{Target: t.Name, Workload: w.Name, Concurrency: w.Concurrency, InputBytes: w.InputBytes, Stream: w.Stream, DurationSeconds: window.Seconds(), AllocationMetric: "unavailable: gateway allocation runtime metric not exposed", ErrorCounts: map[string]int64{}}
	caseCtx, cancel := context.WithTimeout(ctx, window+timeout)
	defer cancel()
	started := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	record := func(s sample) {
		mu.Lock()
		defer mu.Unlock()
		r.Attempted++
		r.QueueMs = append(r.QueueMs, s.QueueMs)
		if s.Error != "" {
			r.Errors++
			r.ErrorCounts[s.Error]++
			if strings.HasPrefix(s.Error, "http_") {
				r.Rejected++
			}
			return
		}
		r.Completed++
		r.TotalMs = append(r.TotalMs, s.TotalMs)
		if s.TTFTMs > 0 {
			r.TTFTMs = append(r.TTFTMs, s.TTFTMs)
		}
	}
	stopSampling := sampleProcess(pid, &r, &mu)
	if w.Rate > 0 {
		slots := make(chan struct{}, w.Concurrency)
		offered := int64(window * time.Duration(w.Rate) / time.Second)
		for n := int64(0); n < offered; n++ {
			scheduled := started.Add(time.Duration(n) * time.Second / time.Duration(w.Rate))
			timer := time.NewTimer(time.Until(scheduled))
			select {
			case <-caseCtx.Done():
				timer.Stop()
				mu.Lock()
				r.Dropped += offered - n
				mu.Unlock()
				goto drained
			case <-timer.C:
			}
			select {
			case slots <- struct{}{}:
				wg.Add(1)
				go func(at time.Time) {
					defer wg.Done()
					defer func() { <-slots }()
					record(request(caseCtx, client, t, w, prefix, at, timeout))
				}(scheduled)
			default:
				mu.Lock()
				r.Dropped++
				r.QueueMs = append(r.QueueMs, float64(time.Since(scheduled))/float64(time.Millisecond))
				mu.Unlock()
			}
		}
	} else {
		for n := 0; n < w.Concurrency; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Since(started) < window && caseCtx.Err() == nil {
					record(request(caseCtx, client, t, w, prefix, time.Now(), timeout))
				}
			}()
		}
	}
drained:
	wg.Wait()
	stopSampling()
	r.OfferedRate = float64(r.Attempted+r.Dropped) / window.Seconds()
	r.AcceptedRate = float64(r.Attempted-r.Rejected) / window.Seconds()
	r.CompletedRate = float64(r.Completed) / time.Since(started).Seconds()
	r.RejectedRate = float64(r.Rejected+r.Dropped) / window.Seconds()
	return r
}

// Open all streams, read only their first event, then leave response bodies unread.
// The fixture deliberately never terminates these streams; cancellation is timed.
func measureSoak(ctx context.Context, client *http.Client, t target, w workload, prefix string, window, timeout time.Duration, pid int) caseResult {
	r := caseResult{Target: t.Name, Workload: w.Name, Concurrency: w.Concurrency, InputBytes: w.InputBytes, Stream: true, DurationSeconds: window.Seconds(), AllocationMetric: "unavailable: gateway allocation runtime metric not exposed", ErrorCounts: map[string]int64{}}
	if fixtureActiveStalls.Load() != 0 {
		r.Errors++
		r.ErrorCounts["preexisting_active_stalls"]++
		return r
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	soakClient, closeClient := extendedClient(client, timeout+window+5*time.Second)
	defer closeClient()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var bodies []*http.Response
	stopSampling := sampleProcess(pid, &r, &mu)
	for n := 0; n < w.Concurrency; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, e := requestBody(t, w, prefix)
			if e != nil {
				mu.Lock()
				r.Attempted++
				r.Errors++
				r.ErrorCounts["encode"]++
				mu.Unlock()
				return
			}
			req, e := http.NewRequestWithContext(cctx, "POST", t.URL, bytes.NewReader(body))
			if e != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if t.Key != "" {
				req.Header.Set("Authorization", "Bearer "+t.Key)
			}
			resp, e := soakClient.Do(req)
			if e == nil && resp.StatusCode == 200 {
				scanner := bufio.NewScanner(resp.Body)
				if !scanner.Scan() {
					e = fmt.Errorf("missing first event")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			r.Attempted++
			if e != nil || resp.StatusCode != 200 {
				r.Errors++
				r.ErrorCounts["open_stream"]++
				if resp != nil {
					resp.Body.Close()
				}
				return
			}
			bodies = append(bodies, resp)
			r.OpenStreams++
		}()
	}
	wg.Wait()
	hold := time.NewTimer(window)
	select {
	case <-ctx.Done():
		hold.Stop()
	case <-hold.C:
	}
	r.ActiveStallsAtEnd = fixtureActiveStalls.Load()
	if r.ActiveStallsAtEnd != 1000 {
		r.Errors++
		r.ErrorCounts["not_1000_active_at_measurement_end"]++
	}
	startCancel := time.Now()
	cancel()
	for _, resp := range bodies {
		resp.Body.Close()
	}
	r.ClientCloseMs = float64(time.Since(startCancel)) / float64(time.Millisecond)
	cancellationDeadline := startCancel.Add(time.Second)
	for fixtureActiveStalls.Load() != 0 && time.Now().Before(cancellationDeadline) {
		time.Sleep(time.Millisecond)
	}
	r.CancellationMs = float64(time.Since(startCancel)) / float64(time.Millisecond)
	r.ActiveStallsAfterCancel = fixtureActiveStalls.Load()
	r.UpstreamCloseObserved = r.ActiveStallsAfterCancel == 0 && time.Now().Before(cancellationDeadline)
	if !r.UpstreamCloseObserved {
		r.Errors++
		r.ErrorCounts["upstream_cancellation_not_observed_within_1s"]++
	}
	stopSampling()
	r.Completed = int64(len(bodies))
	if r.OpenStreams != 1000 {
		r.Errors++
		r.ErrorCounts["not_1000_open"]++
	}
	if ctx.Err() != nil {
		r.Errors++
		r.ErrorCounts["interrupted"]++
	}
	r.OfferedRate = float64(r.Attempted) / window.Seconds()
	r.CompletedRate = float64(r.Completed) / window.Seconds()
	return r
}
