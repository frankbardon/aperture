package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

// probe is a terminal handler that records the principal the middleware
// resolved (if any) so tests can assert what reached the downstream handler.
type probe struct {
	sawPrincipal bool
	principalID  string
}

func (p *probe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if pr, ok := auth.PrincipalFromContext(r.Context()); ok {
		p.sawPrincipal = true
		p.principalID = pr.ID
	}
	w.WriteHeader(http.StatusOK)
}

// TestAuthenticate_DevBearerAttachesPrincipal asserts a valid dev bearer is
// resolved and attached to the request context for downstream handlers.
func TestAuthenticate_DevBearerAttachesPrincipal(t *testing.T) {
	p := &probe{}
	h := Authenticate(auth.NewDev(), p)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer alice")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !p.sawPrincipal || p.principalID != "alice" {
		t.Fatalf("downstream principal = (%v, %q), want (true, alice)", p.sawPrincipal, p.principalID)
	}
}

// TestAuthenticate_AnonymousPassesThrough asserts a request with no credential
// proceeds anonymously (no principal, no 401) — the back-compat path that keeps
// the existing unauthenticated decision surface working.
func TestAuthenticate_AnonymousPassesThrough(t *testing.T) {
	p := &probe{}
	h := Authenticate(auth.NewDev(), p)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anonymous pass-through)", rec.Code)
	}
	if p.sawPrincipal {
		t.Fatalf("anonymous request should carry no principal, saw %q", p.principalID)
	}
}

// rejectingAuthenticator always fails verification with APERTURE_INVALID_TOKEN,
// standing in for an OIDC/parsec adapter handed a bad token.
type rejectingAuthenticator struct{}

func (rejectingAuthenticator) Authenticate(context.Context, string) (string, auth.Claims, error) {
	return "", nil, aerr.New(aerr.APERTURE_INVALID_TOKEN, "auth: bad token")
}

// TestAuthenticate_InvalidTokenIs401 asserts a presented-but-invalid credential
// is refused 401 with the coded error — never silently downgraded to anonymous.
func TestAuthenticate_InvalidTokenIs401(t *testing.T) {
	p := &probe{}
	h := Authenticate(rejectingAuthenticator{}, p)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Code != string(aerr.APERTURE_INVALID_TOKEN) {
		t.Errorf("error code = %q, want %s", body.Code, aerr.APERTURE_INVALID_TOKEN)
	}
	if p.sawPrincipal {
		t.Error("a rejected request must not reach the downstream handler with a principal")
	}
}

// TestAuthenticate_NilIsPassthrough asserts a nil authenticator is a no-op
// wrapper — auth is opt-in at the wiring layer.
func TestAuthenticate_NilIsPassthrough(t *testing.T) {
	p := &probe{}
	if got := Authenticate(nil, p); got != http.Handler(p) {
		t.Fatalf("Authenticate(nil, p) = %v, want p unwrapped", got)
	}
}

// TestAuthenticate_DevDoesNotBreakCheck asserts wrapping the real /check handler
// in dev auth leaves the existing decision flow green: an anonymous /check still
// returns a decision, and an authenticated /check returns the same decision.
func TestAuthenticate_DevDoesNotBreakCheck(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		t.Fatalf("seed: %v", err)
	}
	handler := Authenticate(auth.NewDev(), New(service.New(engine.New(store))))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"account":   seed.ExampleAccount,
		"principal": "alice",
		"action":    "read",
		"object":    "account:acme/project:atlas/document:42",
	})

	for _, withAuth := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/check", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withAuth {
			req.Header.Set("Authorization", "Bearer alice")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /check (withAuth=%v): %v", withAuth, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("status = %d (withAuth=%v), want 200", resp.StatusCode, withAuth)
		}
		var got struct {
			Allow bool `json:"allow"`
		}
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if !got.Allow {
			t.Errorf("want allow=true (withAuth=%v)", withAuth)
		}
	}
}
