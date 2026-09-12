package main

import (
	"bufio"
	"io"
	"time"
)

// readBodyWithTTFB reads the whole (bounded) body and separately reports the
// elapsed time until the first body byte arrived. Streaming verdicts must not
// equate time-to-first-byte with full stream duration: a slow upstream that
// trickles frames reports an honest TTFB near zero while total elapsed time
// still reflects the trickle.
func readBodyWithTTFB(body io.Reader, limit int64, start time.Time) ([]byte, time.Duration, error) {
	buffered := bufio.NewReader(io.LimitReader(body, limit))
	first := make([]byte, 1)
	for {
		n, err := buffered.Read(first)
		if n > 0 {
			ttfb := time.Since(start)
			rest, readErr := io.ReadAll(buffered)
			return append(first[:n:n], rest...), ttfb, readErr
		}
		if err != nil {
			if err == io.EOF {
				return nil, time.Since(start), nil
			}
			return nil, time.Since(start), err
		}
	}
}
