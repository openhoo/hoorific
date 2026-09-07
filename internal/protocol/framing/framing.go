// Package framing implements bounded, incremental HTTP event framing.
package framing

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

const DefaultMaxEventBytes = 1 << 20

var ErrFrameTooLarge = errors.New("protocol frame exceeds configured limit")

type SSEEvent struct{ Event, Data, ID string }
type SSEDecoder struct {
	reader  *bufio.Reader
	maximum int
	first   bool
}
type NDJSONDecoder struct {
	reader  *bufio.Reader
	maximum int
}

func NewSSEDecoder(r io.Reader, maxBytes int) *SSEDecoder {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxEventBytes
	}
	return &SSEDecoder{reader: bufio.NewReaderSize(r, 4096), maximum: maxBytes, first: true}
}
func NewNDJSONDecoder(r io.Reader, maxBytes int) *NDJSONDecoder {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxEventBytes
	}
	return &NDJSONDecoder{reader: bufio.NewReaderSize(r, 4096), maximum: maxBytes}
}

// line reads without Scanner's implicit 64-KiB token ceiling. The bound is
// checked before appending each reader fragment, including newline bytes.
func line(ctx context.Context, r *bufio.Reader, limit int) ([]byte, error) {
	var result []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part, err := r.ReadSlice('\n')
		if len(part) > limit-len(result) {
			return nil, ErrFrameTooLarge
		}
		result = append(result, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if err == io.EOF && len(result) == 0 {
			return nil, io.EOF
		}
		return result, err
	}
}

func (d *SSEDecoder) Next(ctx context.Context) (SSEEvent, error) {
	var event SSEEvent
	var data strings.Builder
	size := 0
	hasData := false
	for {
		raw, err := line(ctx, d.reader, d.maximum-size)
		if err != nil && err != io.EOF {
			return SSEEvent{}, err
		}
		if err == io.EOF { // An unterminated event is not a complete SSE dispatch.
			if size != 0 || len(raw) != 0 {
				return SSEEvent{}, io.ErrUnexpectedEOF
			}
			return SSEEvent{}, io.EOF
		}
		size += len(raw)
		text := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if d.first {
			text = strings.TrimPrefix(text, "\ufeff")
			d.first = false
		}
		if text == "" {
			if hasData {
				event.Data = data.String()
				return event, nil
			}
			event = SSEEvent{}
			size = 0
			continue
		}
		if strings.HasPrefix(text, ":") {
			continue
		}
		field, value, found := strings.Cut(text, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
		case "event":
			event.Event = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				event.ID = value
			}
		}
	}
}

func WriteSSE(w io.Writer, event SSEEvent) error {
	if strings.ContainsAny(event.Event, "\r\n") || strings.ContainsAny(event.ID, "\r\n\x00") {
		return errors.New("invalid SSE event metadata")
	}
	var out strings.Builder
	if event.Event != "" {
		fmt.Fprintf(&out, "event: %s\n", event.Event)
	}
	if event.ID != "" {
		fmt.Fprintf(&out, "id: %s\n", event.ID)
	}
	// CR would terminate a field according to the SSE grammar; normalize it.
	text := strings.ReplaceAll(strings.ReplaceAll(event.Data, "\r\n", "\n"), "\r", "\n")
	for _, value := range strings.Split(text, "\n") {
		out.WriteString("data: ")
		out.WriteString(value)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	_, err := io.WriteString(w, out.String())
	return err
}

func (d *NDJSONDecoder) Next(ctx context.Context) ([]byte, error) {
	for {
		raw, err := line(ctx, d.reader, d.maximum)
		if err != nil && err != io.EOF {
			return nil, err
		}
		raw = []byte(strings.TrimSpace(string(raw)))
		if len(raw) != 0 {
			return raw, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
