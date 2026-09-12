package main

import (
	"errors"
	"io"
	"testing"
	"time"
)

type scriptedStep struct {
	delay time.Duration
	data  []byte
	err   error
}

type scriptedReader struct {
	steps []scriptedStep
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	if step.delay > 0 {
		time.Sleep(step.delay)
	}
	if step.err != nil {
		return 0, step.err
	}
	return copy(p, step.data), nil
}

// TestReadBodyWithTTFBMeasuresFirstByteNotTotalDuration pins the streaming
// timing contract: a slow trickle upstream reports an honest time-to-first-byte
// near the first frame while total elapsed time keeps reflecting the trickle.
func TestReadBodyWithTTFBMeasuresFirstByteNotTotalDuration(t *testing.T) {
	reader := &scriptedReader{steps: []scriptedStep{
		{delay: 50 * time.Millisecond, data: []byte("data: O\n\n")},
		{delay: 250 * time.Millisecond, data: []byte("data: K\n\n")},
		{data: []byte("data: [DONE]\n\n")},
	}}
	start := time.Now()
	body, ttfb, err := readBodyWithTTFB(reader, 1<<20, start)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	total := time.Since(start)
	if ttfb >= 200*time.Millisecond {
		t.Fatalf("ttfb %v drifted into total stream duration", ttfb)
	}
	if ttfb < 40*time.Millisecond {
		t.Fatalf("ttfb %v shorter than the first-byte delay", ttfb)
	}
	if total < 280*time.Millisecond {
		t.Fatalf("total %v did not cover the trickle", total)
	}
	if string(body) != "data: O\n\ndata: K\n\ndata: [DONE]\n\n" {
		t.Fatalf("body mismatch: %q", body)
	}
}

func TestReadBodyWithTTFBReportsEmptyAndErrors(t *testing.T) {
	start := time.Now()
	body, _, err := readBodyWithTTFB(&scriptedReader{}, 1<<20, start)
	if err != nil || body != nil {
		t.Fatalf("empty body read: %q %v", body, err)
	}
	failing := &scriptedReader{steps: []scriptedStep{{delay: 5 * time.Millisecond, err: errors.New("connection reset")}}}
	if _, _, err := readBodyWithTTFB(failing, 1<<20, start); err == nil {
		t.Fatal("read error was not reported")
	}
}
