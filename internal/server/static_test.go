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
		{"/js/rules.js", "window.rules", ""},
		// E7-S2: the pure graph<->AST serializer and the node editor that builds
		// on the vendored Rete bundle both serve as JS the shell loads.
		{"/js/rules-serializer.js", "graphToAST", ""},
		{"/vendor/alpine.min.js", "", ""},
		{"/vendor/tailwind.min.css", "tailwindcss", "text/css"},
		{"/vendor/daisyui.full.css", ":root", "text/css"},
		// The vendored Rete.js bundle is embedded and serves as a JS module so the
		// rules hello-canvas loads with no node build. Assert the entry file is
		// reachable, carries a JS content-type (so `import()` accepts it), and
		// exports createHelloCanvas — the wiring the rules section imports.
		{"/vendor/rete/rete.min.js", "createHelloCanvas", "text/javascript"},
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

// TestRuleCanvasReferencesVendoredRete asserts the reference chain that mounts
// the E7-S1 hello-canvas is intact end to end: the shell loads the rules script,
// and the rules script imports the vendored Rete.js bundle by its served path. A
// broken link here would leave the rules section blank at runtime.
func TestRuleCanvasReferencesVendoredRete(t *testing.T) {
	srv, _ := newTestServer(t)

	index, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer index.Body.Close()
	if body := readAll(t, index); !strings.Contains(body, "/js/rules.js") {
		t.Errorf("index.html does not load /js/rules.js")
	}

	rules, err := http.Get(srv.URL + "/js/rules.js")
	if err != nil {
		t.Fatalf("GET /js/rules.js: %v", err)
	}
	defer rules.Body.Close()
	if body := readAll(t, rules); !strings.Contains(body, "/vendor/rete/rete.min.js") {
		t.Errorf("rules.js does not import the vendored /vendor/rete/rete.min.js bundle")
	}
}

// TestRuleEditorWiring asserts the E7-S2 node-editor chain is intact end to end:
// the shell loads the pure serializer before the editor, the editor consumes it
// and exposes the E7-S3 save/load hooks on window.blueprintEditor, and the
// serializer exports both directions of the graph<->AST bridge. A regression in
// any link would leave the rules editor unable to serialize against the AST.
func TestRuleEditorWiring(t *testing.T) {
	srv, _ := newTestServer(t)

	get := func(path string) string {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, res.StatusCode)
		}
		return readAll(t, res)
	}

	// The shell loads the serializer, and it must precede the editor so
	// window.RuleSerializer exists when rules.js mounts.
	index := get("/")
	sIdx := strings.Index(index, "/js/rules-serializer.js")
	eIdx := strings.Index(index, "/js/rules.js")
	if sIdx < 0 || eIdx < 0 {
		t.Fatalf("index.html must load both rules-serializer.js (%d) and rules.js (%d)", sIdx, eIdx)
	}
	if sIdx > eIdx {
		t.Errorf("rules-serializer.js must load before rules.js (serializer=%d editor=%d)", sIdx, eIdx)
	}

	// The editor consumes the serializer and exposes the E7-S3 hooks.
	editor := get("/js/rules.js")
	for _, sub := range []string{"window.RuleSerializer", "window.blueprintEditor", "toAST", "fromAST", "validate"} {
		if !strings.Contains(editor, sub) {
			t.Errorf("rules.js is missing %q — the E7-S3 hook surface must be present", sub)
		}
	}

	// The serializer exports both directions of the graph<->AST bridge and its
	// client-side structural validation.
	ser := get("/js/rules-serializer.js")
	for _, sub := range []string{"graphToAST", "astToGraph", "validateAST"} {
		if !strings.Contains(ser, sub) {
			t.Errorf("rules-serializer.js is missing %q", sub)
		}
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
