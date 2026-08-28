package api

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// withUI turns the embedded interface on for a harness.
func withUI(cfg *Config) { cfg.UI = true }

// get fetches a path outside the API prefix, with no credential: the interface
// has to load before anybody has one, because the login screen is what gets
// you one.
func (h *harness) get(t *testing.T, path string, opts ...func(*http.Request)) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.http.URL+path, http.NoBody)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	for _, o := range opts {
		o(req)
	}

	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestUIServesTheDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	resp := h.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: status %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if !strings.Contains(string(body), "<!doctype html>") {
		t.Errorf("the body is not the built document:\n%s", firstLine(string(body)))
	}
}

// A route the client-side router owns is not a route this server knows, and it
// must resolve to the document rather than to a 404. That is the whole reason
// the fallback exists: /zones/example.com. is a page, not a file.
func TestUIFallsBackToTheDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	for _, path := range []string{"/zones", "/zones/example.com.", "/stream", "/a/deep/one"} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("GET %s: Content-Type = %q, want text/html", path, got)
		}
	}
}

// An asset that is not there is a broken build rather than a route, and a
// document served in its place would be reported as a syntax error in a file
// nobody wrote.
func TestUIMissingAssetIsNotTheDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	resp := h.get(t, "/_app/immutable/chunks/nothing-here.js")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want anything but a document", got)
	}
}

// The names under _app/immutable carry a hash of their content, so they may be
// held forever. Everything else has a stable name and has to be revalidated.
func TestUICacheControl(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	asset := firstImmutableAsset(t, h)

	cases := []struct {
		path string
		want string
	}{
		{"/", "no-cache"},
		{"/favicon.svg", "no-cache"},
		{asset, uiCacheImmutable},
	}

	for _, c := range cases {
		resp := h.get(t, c.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", c.path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Cache-Control"); got != c.want {
			t.Errorf("GET %s: Cache-Control = %q, want %q", c.path, got, c.want)
		}
	}
}

// The document is revalidated on every load, so the conditional request has to
// be answered: otherwise the whole page travels again each time somebody
// opens a tab.
func TestUIDocumentRevalidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	first := h.get(t, "/")
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("the document carries no ETag, so it can never be revalidated")
	}

	second := h.get(t, "/", func(r *http.Request) {
		r.Header.Set("If-None-Match", etag)
	})
	if second.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional request: status %d, want 304", second.StatusCode)
	}
}

func TestUISecurityHeaders(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	resp := h.get(t, "/")
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		// frame-ancestors is ignored in the meta element the document carries,
		// so this header is the only place framing is refused at all.
		"X-Frame-Options": "DENY",
		"Referrer-Policy": "no-referrer",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestUIRefusesWrites(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.http.URL+"/", http.NoBody)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

// With the interface on, the API is still reached at its own prefix: the two
// are siblings behind one socket and neither may swallow the other.
func TestUIDoesNotShadowTheAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withUI)

	resp := h.do(http.MethodGet, "/zones", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/zones: status %d, want 200", basePath, resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// Switched off, the server says so rather than answering with a bare 404:
// somebody who set api.ui to false months ago and then opened a browser
// deserves to be told that is what happened.
func TestUIOffExplainsItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // UI is off by default in the harness

	resp := h.get(t, "/")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if !strings.Contains(string(body), "api.ui") {
		t.Errorf("the answer does not name the setting that caused it:\n%s", body)
	}
	if !strings.Contains(string(body), basePath) {
		t.Errorf("the answer does not say where the API is:\n%s", body)
	}
}

// With the interface off the API still answers, which is the entire point of
// the switch.
func TestUIOffKeepsTheAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	resp := h.do(http.MethodGet, "/zones", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s/zones: status %d, want 200", basePath, resp.StatusCode)
	}
}

// firstImmutableAsset returns a path the built document actually references,
// so the cache test cannot pass against a name that no longer exists.
func firstImmutableAsset(t *testing.T, h *harness) string {
	t.Helper()

	body, err := io.ReadAll(h.get(t, "/").Body)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}

	marker := []byte(`"/` + immutablePrefix)
	i := bytes.Index(body, marker)
	if i < 0 {
		t.Fatalf("the document references nothing under %s, so it was not built", immutablePrefix)
	}
	rest := body[i+1:]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		t.Fatal("malformed reference in the document")
	}
	return string(rest[:end])
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
