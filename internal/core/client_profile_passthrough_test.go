package core

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestClientProfilePassthroughForwardsSelectedHeadersOnly(t *testing.T) {
	const (
		userAgent    = "codex_cli_rs/0.153.4 (Linux; x86_64)"
		originator   = "codex-tui"
		sessionID    = "session-123"
		turnMetadata = `{"turn_id":"turn-123","kind":"user"}`
	)
	src := http.Header{
		"uSeR-aGeNt":            {userAgent},
		"ORIGINATOR":            {originator},
		"sEsSiOn-iD":            {sessionID},
		"X-cOdEx-TuRn-MeTaDaTa": {turnMetadata},
		"aCcEpT-eNcOdInG":       {"IdEnTiTy"},
		"Authorization":         {"Bearer evil"},
	}
	dst := http.Header{
		"Authorization": {"Bearer upstream"},
		"thread-id":     {"stale-thread"},
		"originator":    {"stale-originator"},
	}
	if err := (&ClientProfile{Preset: "codex-passthrough"}).ApplyRequestHeaders(dst, src); err != nil {
		t.Fatalf("ApplyRequestHeaders() error = %v", err)
	}

	want := map[string]string{
		"User-Agent":            userAgent,
		"Originator":            originator,
		"Session-Id":            sessionID,
		"X-Codex-Turn-Metadata": turnMetadata,
		"Accept-Encoding":       "IdEnTiTy",
	}
	for key, value := range want {
		got, ok := dst[key]
		if !ok || len(got) != 1 || got[0] != value {
			t.Errorf("destination %s = %#v, want [%q]", key, got, value)
		}
	}
	if got := dst["Authorization"]; !reflect.DeepEqual(got, []string{"Bearer upstream"}) {
		t.Fatalf("destination Authorization = %#v, want upstream value", got)
	}
	for key := range dst {
		if strings.EqualFold(key, "authorization") && key != "Authorization" {
			t.Fatalf("source Authorization was copied under stale casing: %q", key)
		}
		if strings.EqualFold(key, "thread-id") {
			t.Fatalf("stale Thread-Id casing remained in destination: %q", key)
		}
		if strings.EqualFold(key, "originator") && key != "Originator" {
			t.Fatalf("stale Originator casing remained in destination: %q", key)
		}
	}

	for key := range want {
		dst[key][0] = "mutated-destination"
	}
	for key, value := range map[string]string{
		"uSeR-aGeNt":            userAgent,
		"ORIGINATOR":            originator,
		"sEsSiOn-iD":            sessionID,
		"X-cOdEx-TuRn-MeTaDaTa": turnMetadata,
		"aCcEpT-eNcOdInG":       "IdEnTiTy",
	} {
		if got := src[key][0]; got != value {
			t.Errorf("source %s changed after destination mutation: %q, want %q", key, got, value)
		}
	}

	incoming := http.Header{
		"Session-Id":            {sessionID},
		"X-Codex-Turn-Metadata": {turnMetadata},
		"Originator":            {originator},
		"User-Agent":            {userAgent},
	}
	for _, test := range []struct {
		name    string
		profile *ClientProfile
	}{
		{name: "nil", profile: nil},
		{name: "default", profile: &ClientProfile{Preset: "custom"}},
		{name: "codex-cli", profile: &ClientProfile{Preset: "codex-cli", Version: "0.153.4"}},
	} {
		t.Run("static profiles omit passthrough/"+test.name, func(t *testing.T) {
			dst := make(http.Header)
			if err := test.profile.ApplyRequestHeaders(dst, incoming); err != nil {
				t.Fatalf("ApplyRequestHeaders() error = %v", err)
			}
			if _, ok := dst["Session-Id"]; ok {
				t.Fatalf("%s profile forwarded Session-Id", test.name)
			}
			if _, ok := dst["X-Codex-Turn-Metadata"]; ok {
				t.Fatalf("%s profile forwarded X-Codex-Turn-Metadata", test.name)
			}
		})
	}
}

func TestClientProfilePassthroughRejectsInvalidRequestsAndConfiguration(t *testing.T) {
	const sentinel = "private-turn-metadata-sentinel"
	baseline := func() http.Header {
		return http.Header{
			"User-Agent": {"codex_cli_rs/0.153.4 (Linux; x86_64)"},
			"Originator": {"codex-tui"},
		}
	}
	invalid := []struct {
		name     string
		mutate   func(http.Header)
		sentinel string
	}{
		{name: "missing user agent", mutate: func(h http.Header) { delete(h, "User-Agent") }},
		{name: "missing originator", mutate: func(h http.Header) { delete(h, "Originator") }},
		{name: "empty required", mutate: func(h http.Header) { h["User-Agent"] = []string{""} }},
		{name: "source alias duplicate originator", mutate: func(h http.Header) { h["originator"] = []string{"codex-exec"} }},
		{name: "two Session-Id values", mutate: func(h http.Header) { h["Session-Id"] = []string{"one", "two"} }},
		{
			name:     "metadata newline",
			mutate:   func(h http.Header) { h["X-Codex-Turn-Metadata"] = []string{sentinel + "\n{}"} },
			sentinel: sentinel,
		},
		{name: "invalid UTF-8", mutate: func(h http.Header) { h["Session-Id"] = []string{string([]byte{0xff, 0xfe})} }},
		{name: "edge whitespace", mutate: func(h http.Header) { h["Session-Id"] = []string{" session "} }},
		{name: "value over 16 KiB", mutate: func(h http.Header) { h["Session-Id"] = []string{strings.Repeat("x", 16<<10+1)} }},
		{
			name: "aggregate over 32 KiB",
			mutate: func(h http.Header) {
				h["Session-Id"] = []string{strings.Repeat("a", 12000)}
				h["Thread-Id"] = []string{strings.Repeat("b", 12000)}
				h["X-Client-Request-Id"] = []string{strings.Repeat("c", 12000)}
			},
		},
		{name: "unknown X-Codex-Future", mutate: func(h http.Header) { h["X-Codex-Future"] = []string{"future"} }},
		{
			name: "Connection session-id",
			mutate: func(h http.Header) {
				h["Session-Id"] = []string{"session"}
				h["Connection"] = []string{"session-id"}
			},
		},
		{name: "gzip encoding", mutate: func(h http.Header) { h["Accept-Encoding"] = []string{"gzip"} }},
	}
	assertGatewayError := func(t *testing.T, err error) GatewayError {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		var got GatewayError
		if !errors.As(err, &got) {
			t.Fatalf("error = %T %v, want GatewayError value", err, err)
		}
		if got.Code != "invalid_request" {
			t.Errorf("GatewayError.Code = %q, want invalid_request", got.Code)
		}
		if got.HTTPStatus != http.StatusBadRequest {
			t.Errorf("GatewayError.HTTPStatus = %d, want %d", got.HTTPStatus, http.StatusBadRequest)
		}
		if got.Param == "" {
			t.Error("GatewayError.Param is empty")
		}
		return got
	}

	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			src := baseline()
			test.mutate(src)
			dst := http.Header{
				"Authorization":         {"Bearer upstream"},
				"originator":            {"stale-originator"},
				"Thread-Id":             {"stale-thread"},
				"X-Codex-Turn-Metadata": {"stale-metadata"},
			}
			before := dst.Clone()
			got := assertGatewayError(t, (&ClientProfile{Preset: "codex-passthrough"}).ApplyRequestHeaders(dst, src))
			if !reflect.DeepEqual(dst, before) {
				t.Fatalf("destination mutated on rejected request: before %#v, after %#v", before, dst)
			}
			if test.sentinel != "" && strings.Contains(got.Error(), test.sentinel) {
				t.Fatalf("GatewayError.Error() echoed private sentinel %q: %q", test.sentinel, got.Error())
			}
		})
	}

	for _, connector := range []string{"openai", "compatible", "codex-subscription"} {
		t.Run("valid connector/"+connector, func(t *testing.T) {
			if err := (&ClientProfile{Preset: "codex-passthrough"}).Validate(connector); err != nil {
				t.Fatalf("Validate(%q) error = %v", connector, err)
			}
		})
	}
	for _, test := range []struct {
		name      string
		profile   *ClientProfile
		connector string
	}{
		{name: "nonempty version", profile: &ClientProfile{Preset: "codex-passthrough", Version: "1.0"}, connector: "openai"},
		{name: "nonempty headers", profile: &ClientProfile{Preset: "codex-passthrough", Headers: map[string]string{"User-Agent": "codex"}}, connector: "openai"},
		{name: "incompatible anthropic", profile: &ClientProfile{Preset: "codex-passthrough"}, connector: "anthropic"},
	} {
		t.Run("invalid configuration/"+test.name, func(t *testing.T) {
			assertGatewayError(t, test.profile.Validate(test.connector))
		})
	}
}
