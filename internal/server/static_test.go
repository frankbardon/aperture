package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// readAll drains an HTTP response body to a string, failing the test on error.
func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestStaticShellServed asserts the embedded admin shell is reachable at the
// site root and that its vendored assets serve — the chrome loads without a
// node build.
func TestStaticShellServed(t *testing.T) {
	srv, _ := newTestServer(t)

	cases := []struct {
		path      string
		wantSub   string // a substring that must appear in the body
		wantCType string // a content-type prefix the response must carry
	}{
		{"/", "<title>Aperture", "text/html"},
		{"/index.html", "shell()", "text/html"},
		{"/css/bera.css", "--bera-500", "text/css"},
		{"/js/app.js", "apiFetch", ""},
		{"/js/crud.js", "window.crud", ""},
		{"/js/grants.js", "window.grants", ""},
		{"/vendor/alpine.min.js", "", ""},
		{"/vendor/tailwind.min.css", "tailwindcss", "text/css"},
		{"/vendor/daisyui.full.css", ":root", "text/css"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			res, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200", tc.path, res.StatusCode)
			}
			if tc.wantCType != "" {
				if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, tc.wantCType) {
					t.Errorf("GET %s: content-type = %q, want prefix %q", tc.path, got, tc.wantCType)
				}
			}
			if tc.wantSub != "" {
				body := readAll(t, res)
				if !strings.Contains(body, tc.wantSub) {
					t.Errorf("GET %s: body missing %q", tc.path, tc.wantSub)
				}
			}
		})
	}
}

// TestStaticDoesNotShadowAPI asserts that mounting the static file server at "/"
// LAST does not shadow the API routes: an API route still resolves to its
// handler (not the file server's 404), and an unknown path falls through to the
// static handler's 404 rather than an API error.
func TestStaticDoesNotShadowAPI(t *testing.T) {
	srv, _ := newTestServer(t)

	// The plain decision route still answers (a POST, which the file server would
	// never serve) — the longest-match API pattern wins over root "/".
	res, err := http.Post(srv.URL+"/check", "application/json",
		strings.NewReader(`{"account":"acme","principal":"root","action":"read","object":"doc:1"}`))
	if err != nil {
		t.Fatalf("POST /check: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /check: status = %d, want 200 (static must not shadow the API)", res.StatusCode)
	}

	// The liveness probe still answers from its own handler.
	hz, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer hz.Body.Close()
	if hz.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want 200", hz.StatusCode)
	}
	if body := readAll(t, hz); body != "ok" {
		t.Errorf("GET /healthz: body = %q, want %q", body, "ok")
	}

	// A path the static server does not have is a 404 from the file server, not a
	// panic or an API error — the fall-through is clean.
	nf, err := http.Get(srv.URL + "/nope/not-a-real-asset")
	if err != nil {
		t.Fatalf("GET missing asset: %v", err)
	}
	defer nf.Body.Close()
	if nf.StatusCode != http.StatusNotFound {
		t.Errorf("GET missing asset: status = %d, want 404", nf.StatusCode)
	}
}
