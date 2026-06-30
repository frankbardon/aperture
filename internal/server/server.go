// Package server holds Aperture's HTTP surface: a net/http ServeMux (Go 1.22
// method/pattern routing) whose handlers are thin translators over the decision
// service facade. There is no business logic here — a handler decodes a request,
// calls service.Check, and encodes the result.
//
// The /check handler is intentionally minimal: it is subsumed by the Twirp
// service in E4-S1. Because it calls the same service.Service the CLI uses, the
// fail-closed decision semantics are identical across surfaces.
package server

import (
	"encoding/json"
	"net/http"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/service"
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

// New builds the HTTP handler, mounting the decision routes over svc. It returns
// an http.Handler so the serve command can wrap it in an &http.Server{}.
func New(svc *service.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /check", checkHandler(svc))
	return mux
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
