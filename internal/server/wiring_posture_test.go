package server_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/auth"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/service"

	"github.com/twitchtv/twirp"
)

// E4-S4, the Twirp half. WiringPosture is the deliberate counterpart to
// Capabilities in capabilities_test.go: the same KIND of question ("what is true
// of this deployment?") and the opposite answer about who may ask.
//
// Capabilities is answered anonymously because it carries three booleans of
// boot-time configuration. This carries mutable runtime FAULT state, a duration
// and a coded reason, so it is gated — and the two properties asserted here are
// exactly the two that would be lost by adding a field to Capabilities instead:
// an anonymous caller is REFUSED, and the duration arrives in a form a paged
// human can read.

// postureServer boots the same Twirp stack the rest of this package tests, with a
// staleness recorder wired into the facade.
//
// The option is applied after construction rather than through a new
// newTestService parameter, deliberately: service.Option is exported and
// applying one is how a host layers a dependency on, so this exercises the real
// seam instead of a test-only constructor.
func postureServer(t *testing.T, h *service.WiringHealth) *httptest.Server {
	t.Helper()
	svc, _ := newTestService(t, service.ManagedEntities{})
	service.WithWiringHealth(h)(svc)
	srv := httptest.NewServer(server.Authenticate(auth.NewDev(), server.New(svc)))
	t.Cleanup(srv.Close)
	return srv
}

// TestWiringPosture_AnAnonymousCallerIsRefused is the disclosure boundary, and the
// single most important assertion in this file.
//
// "This instance has been enforcing configuration its operator already replaced,
// and has been for four hours" says the enforced policy is not the intended
// policy, and how long the window has been open. That is operational
// intelligence. The open Capabilities RPC sits one method away in the same
// service, so the wrong door is one line of proto from being opened.
func TestWiringPosture_AnAnonymousCallerIsRefused(t *testing.T) {
	h := service.NewWiringHealth(30*time.Second, "digest-boot", nil)
	h.Failed(aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "no route for a connection this wiring names"))
	srv := postureServer(t, h)

	resp, err := client(srv).WiringPosture(context.Background(), &rpc.WiringPostureRequest{Actor: &rpc.Actor{Account: acct}})
	if err == nil {
		t.Fatalf("an anonymous caller read the wiring posture: %+v", resp)
	}
	if te, ok := err.(twirp.Error); !ok || te.Code() != twirp.Unauthenticated {
		t.Errorf("anonymous read = %v, want a twirp unauthenticated error", err)
	}
	// And the refusal says nothing about the fault it is withholding.
	for _, leak := range []string{"stale", "UNROUTED", "route"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the refusal mentions %q: %v", leak, err)
		}
	}
}

// TestWiringPosture_ANonAdminIsRefused is the other half of the gate: being
// authenticated is not enough. The facade owns the rule (requireWiringAdmin) and
// the handler adds no check of its own, so this asserts the gate reaches the wire
// rather than re-testing the rule.
func TestWiringPosture_ANonAdminIsRefused(t *testing.T) {
	srv := postureServer(t, service.NewWiringHealth(30*time.Second, "digest-boot", nil))
	ctx := asPrincipal(context.Background(), t, "alice")

	if resp, err := client(srv).WiringPosture(ctx, &rpc.WiringPostureRequest{Actor: &rpc.Actor{Account: acct}}); err == nil {
		t.Fatalf("an authenticated non-admin read the wiring posture: %+v", resp)
	} else if te, ok := err.(twirp.Error); !ok || te.Code() != twirp.PermissionDenied {
		t.Errorf("non-admin read = %v, want a twirp permission_denied error", err)
	}
}

// TestWiringPosture_TheAdminReadsTheDurationInAReadableForm is the story's
// requirement over the wire: an operator sees the staleness AND how long, without
// reading logs.
//
// The duration and the instant are asserted as TEXT on purpose. A count of
// milliseconds would be correct and useless — the person reading this has just
// been paged and should not have to divide — and the rest of the proto already
// spells an instant RFC3339 (ImpersonationSession) and a ttl as duration text.
func TestWiringPosture_TheAdminReadsTheDurationInAReadableForm(t *testing.T) {
	clk := &serverClock{at: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	h := service.NewWiringHealth(30*time.Second, "0123456789abcdef", clk.now)
	srv := postureServer(t, h)
	ctx := asPrincipal(context.Background(), t, "root")

	// Healthy first: every field empty or false, so "nothing is wrong" needs no
	// interpretation.
	resp, err := client(srv).WiringPosture(ctx, &rpc.WiringPostureRequest{Actor: &rpc.Actor{Account: acct}})
	if err != nil {
		t.Fatalf("admin read: %v", err)
	}
	if resp.Stale || resp.StaleFor != "" || resp.StaleSince != "" || resp.Code != "" || resp.Failures != 0 {
		t.Errorf("a healthy instance reported %+v, want stale=false with every fault field empty", resp)
	}
	if !resp.Polling || resp.PollInterval != "30s" {
		t.Errorf("polling=%v interval=%q, want true / \"30s\"", resp.Polling, resp.PollInterval)
	}
	if resp.Digest != "0123456789abcdef" {
		t.Errorf("digest = %q, want the wiring this instance decides from", resp.Digest)
	}

	// Then stale, for four hours.
	h.Failed(aerr.New(aerr.APERTURE_WIRING_CONNECTION_UNROUTED, "no route for a connection this wiring names"))
	clk.advance(4 * time.Hour)

	resp, err = client(srv).WiringPosture(ctx, &rpc.WiringPostureRequest{Actor: &rpc.Actor{Account: acct}})
	if err != nil {
		t.Fatalf("admin read while stale: %v", err)
	}
	switch {
	case !resp.Stale:
		t.Error("a stale instance reported stale=false")
	case resp.StaleFor != "4h0m0s":
		t.Errorf("stale_for = %q, want \"4h0m0s\" — the duration is the half an operator escalates on", resp.StaleFor)
	case resp.StaleSince != "2026-05-01T12:00:00Z":
		t.Errorf("stale_since = %q, want RFC3339 in UTC", resp.StaleSince)
	case resp.Failures != 1:
		t.Errorf("failures = %d, want 1", resp.Failures)
	case resp.Code != string(aerr.APERTURE_WIRING_CONNECTION_UNROUTED):
		t.Errorf("code = %q, want the underlying failure's own code, whose fixups are the remedy", resp.Code)
	}

	// Recovery over the wire, because a latching alarm is the bug this half is most
	// likely to ship with: every fault field empties again.
	clk.advance(time.Minute)
	h.Refreshed("0123456789abcdef")
	resp, err = client(srv).WiringPosture(ctx, &rpc.WiringPostureRequest{Actor: &rpc.Actor{Account: acct}})
	if err != nil {
		t.Fatalf("admin read after recovery: %v", err)
	}
	if resp.Stale || resp.StaleFor != "" || resp.StaleSince != "" || resp.Code != "" || resp.Failures != 0 {
		t.Errorf("after recovery the wire still reported %+v", resp)
	}
	if resp.LastRefresh != "2026-05-01T16:01:00Z" {
		t.Errorf("last_refresh = %q, want the instant the refresh succeeded", resp.LastRefresh)
	}
}

// TestWiringPosture_AnInstanceThatDoesNotPollAnswersRatherThanRefusing pins the
// answer a fleet-wide probe needs. Every deployment that has not set
// --wiring-poll is in this case, and a refusal would make the read useless: an
// operator sweeping ten instances would have to read it as either "fine" or
// "broken" and would be wrong about one of them.
func TestWiringPosture_AnInstanceThatDoesNotPollAnswersRatherThanRefusing(t *testing.T) {
	// No recorder at all — a facade built without WithWiringHealth, which is every
	// facade that existed before this story.
	srv, _ := newTestServer(t)
	ctx := asPrincipal(context.Background(), t, "root")

	resp, err := client(srv).WiringPosture(ctx, &rpc.WiringPostureRequest{Actor: &rpc.Actor{Account: acct}})
	if err != nil {
		t.Fatalf("a server with no recorder refused the read: %v", err)
	}
	if resp.Polling || resp.Stale || resp.PollInterval != "" || resp.Digest != "" {
		t.Errorf("an unwired instance reported %+v, want not polling and not stale", resp)
	}
}

// serverClock is the pinnable clock behind the recorder. It is only ever advanced
// from the test goroutine and read from a request's, so it carries no lock and the
// reads that matter are ordered by the round trip.
type serverClock struct{ at time.Time }

func (c *serverClock) now() time.Time          { return c.at }
func (c *serverClock) advance(d time.Duration) { c.at = c.at.Add(d) }
