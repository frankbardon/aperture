// Package server holds Aperture's HTTP surface: a net/http ServeMux (Go 1.22
// method/pattern routing) whose handlers are thin translators over the decision
// service facade. There is no business logic here — a handler decodes a request,
// calls the facade, and encodes the result.
//
// Two surfaces are mounted on one mux:
//
//   - The full Twirp service (rpc.ApertureService) under its path prefix
//     (/twirp/aperture.ApertureService/) — the decision API AND the model
//     mutations, with twirp.ServerHooks request/error logging (the orbit
//     pattern). The handler (twirp.go) owns the auth policy: decision RPCs are
//     open, mutations require an authenticated principal and the admin tier.
//   - The minimal plain-HTTP POST /check decision route, kept (FR per E1-S5) so
//     the simple decision path survives the Twirp fold-in. It calls the same
//     facade, so its fail-closed semantics are identical.
//   - The embedded admin shell (static.go) served from the site root, mounted
//     LAST so the more specific API patterns above win by longest-match.
//
// The whole mux is wrapped by the Authenticate middleware in the serve command,
// which attaches an authenticated principal to the context when a credential is
// presented; the Twirp mutation handlers then require that principal.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/service"

	"github.com/twitchtv/twirp"
)

// checkRequest is the JSON body of POST /check.
type checkRequest struct {
	Account   string `json:"account"`
	Principal string `json:"principal"`
	Action    string `json:"action"`
	Object    string `json:"object"`
}

// checkResponse is the JSON body returned by POST /check.
type checkResponse struct {
	Allow            bool     `json:"allow"`
	Reason           string   `json:"reason"`
	DecidingGrantIDs []string `json:"deciding_grant_ids,omitempty"`
}

// errorResponse is the JSON body returned for a 4xx/5xx.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// New builds the HTTP handler, mounting the Twirp service and the plain /check
// decision route over svc, with request/error logging hooks on the Twirp
// surface. It returns an http.Handler so the serve command can wrap it in
// Authenticate + an &http.Server{}. A nil logger falls back to slog.Default.
func New(svc *service.Service, logger ...*slog.Logger) http.Handler {
	log := slog.Default()
	if len(logger) > 0 && logger[0] != nil {
		log = logger[0]
	}

	mux := http.NewServeMux()

	// The full Twirp surface (decision API + mutations) under its path prefix.
	twirpServer := rpc.NewApertureServiceServer(NewTwirpHandler(svc), twirp.WithServerHooks(loggingHooks(log)))
	mux.Handle(rpc.ApertureServicePathPrefix, twirpServer)

	// The minimal plain-HTTP decision path, preserved from E1-S5.
	mux.HandleFunc("POST /check", checkHandler(svc))

	// Liveness probe.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// The embedded admin shell at the site root, mounted LAST. net/http's
	// ServeMux resolves by longest matching pattern, so the API routes above
	// (the Twirp path prefix, POST /check, GET /healthz) win over the root "/"
	// this handler owns — the static server never shadows the API.
	mux.Handle("/", staticHandler())

	return mux
}

// loggingHooks returns the twirp.ServerHooks that log each RPC request and error
// via slog — the orbit logging-wrapper pattern.
func loggingHooks(log *slog.Logger) *twirp.ServerHooks {
	return &twirp.ServerHooks{
		RequestReceived: func(ctx context.Context) (context.Context, error) {
			if method, ok := twirp.MethodName(ctx); ok {
				log.InfoContext(ctx, "rpc request", "method", method)
			}
			return ctx, nil
		},
		Error: func(ctx context.Context, err twirp.Error) context.Context {
			method, _ := twirp.MethodName(ctx)
			log.WarnContext(ctx, "rpc error", "method", method, "code", err.Code(), "msg", err.Msg())
			return ctx
		},
	}
}

// checkHandler decodes a single query, asks the service, and encodes the
// decision. A malformed body or an input-validation error from the service is a
// 400; the decision itself (allow or deny) is always a 200 — a deny is a
// successful answer, not an HTTP error.
func checkHandler(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req checkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, aerr.APERTURE_INVALID_INPUT,
				"request body is not valid JSON")
			return
		}

		res, err := svc.Check(r.Context(), service.Query{
			Account:   req.Account,
			Principal: req.Principal,
			Action:    req.Action,
			Object:    req.Object,
		})
		if err != nil {
			// The facade only returns an error for genuine input validation;
			// everything operational is already folded into a fail-closed deny.
			writeError(w, http.StatusBadRequest, aerr.CodeOf(err), err.Error())
			return
		}

		writeJSON(w, http.StatusOK, checkResponse{
			Allow:            res.Allow,
			Reason:           res.Reason,
			DecidingGrantIDs: res.DecidingGrantIDs,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code aerr.Code, msg string) {
	writeJSON(w, status, errorResponse{Code: string(code), Message: msg})
}
