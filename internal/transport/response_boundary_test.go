package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSanitizeResponseHeadersNeutralizesActiveContent pins the response
// boundary for relayed upstream responses: browsers must never receive relayed
// bytes as an executable document on a Hoorific origin, even when navigating a
// relay URL directly.
func TestSanitizeResponseHeadersNeutralizesActiveContent(t *testing.T) {
	for _, contentType := range []string{
		"text/html",
		"text/html; charset=utf-8",
		"TEXT/HTML; charset=iso-8859-1",
		"application/xhtml+xml",
		"image/svg+xml",
		"text/xml",
		"application/xml",
	} {
		h := http.Header{}
		h.Set("Content-Type", contentType)
		SanitizeResponseHeaders(h)
		if got := h.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("active content type %q relayed as %q", contentType, got)
		}
		if h.Get("Content-Security-Policy") != "sandbox" {
			t.Errorf("active content type %q relayed without sandboxing CSP", contentType)
		}
	}
}

func TestSanitizeResponseHeadersKeepsInertContent(t *testing.T) {
	for _, contentType := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"text/event-stream",
		"text/plain; charset=utf-8",
		"application/octet-stream",
		"audio/mpeg",
		"",
	} {
		h := http.Header{}
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		SanitizeResponseHeaders(h)
		if got := h.Get("Content-Type"); got != contentType {
			t.Errorf("inert content type %q rewritten to %q", contentType, got)
		}
	}
}

// TestRelayHTTPEnforcesResponseBoundary covers the relay path end to end: an
// upstream serving active HTML must reach the client as inert text with
// boundary headers, and upstream credential/storage controls must be dropped.
func TestRelayHTTPEnforcesResponseBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Set-Cookie", "upstream=session")
		w.Header().Set("Clear-Site-Data", `"cache", "cookies"`)
		w.Header().Set("X-Upstream-Custom", "yes")
		_, _ = w.Write([]byte("<html><script>document.title='PWNED'</script></html>"))
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	response, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := RelayHTTP(t.Context(), recorder, response, RelayOptions{}); err != nil {
		t.Fatalf("relay failed: %v", err)
	}
	if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("active content type relayed: %q", recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("relayed response lacks nosniff boundary")
	}
	if recorder.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatal("relayed response lacks sandboxing CSP")
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("Clear-Site-Data") != "" {
		t.Fatal("upstream credential/storage control headers were relayed")
	}
	if !strings.Contains(recorder.Body.String(), "PWNED") {
		t.Fatalf("relay dropped upstream body bytes: %q", recorder.Body.String())
	}
}
