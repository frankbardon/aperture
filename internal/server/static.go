package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// staticFS embeds the admin shell (index.html + css/js) and its vendored,
// pre-built assets (Alpine.js, Tailwind, DaisyUI) under static/. The `all:`
// prefix includes files whose names begin with `_` or `.` and keeps the binary
// self-contained — there is no node build in dev or CI (see static/vendor/README.md).
//
//go:embed all:static
var staticFS embed.FS

// staticHandler serves the embedded shell at the site root. It is mounted LAST
// on the mux (New): net/http's ServeMux resolves by longest matching pattern, so
// the more specific API routes (the Twirp path prefix, POST /check, GET /healthz)
// win over the root "/" this handler owns. A missing embed is a build mistake,
// so the (compile-time-constant) Sub error panics rather than degrading at runtime.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}
