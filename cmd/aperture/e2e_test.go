package main

// End-to-end test for the walking skeleton: it seeds the committed example
// fixture and asserts BOTH surfaces — the `aperture check` CLI and the HTTP
// POST /check endpoint — return the same decisions for the same questions,
// covering an allow, a default deny, and a deny-overrides case.
//
// The CLI half builds the real binary and runs it as a subprocess so the
// printed verdict AND the process exit code are exercised exactly as a human
// would see them. The HTTP half boots the same service stack the serve command
// wires (storage -> engine -> service -> server) over httptest.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/internal/server"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

const account = seed.ExampleAccount

// cases are the shared decision expectations both surfaces must satisfy.
var cases = []struct {
	name      string
	principal string
	action    string
	object    string
	wantAllow bool
}{
	{
		name:      "allow: engineering reads a document under atlas",
		principal: "alice",
		action:    "read",
		object:    "account:acme/project:atlas/document:42",
		wantAllow: true,
	},
	{
		name:      "allow: editor writes a document under atlas",
		principal: "alice",
		action:    "write",
		object:    "account:acme/project:atlas/document:42",
		wantAllow: true,
	},
	{
		name:      "deny (default): a viewer cannot write",
		principal: "bob",
		action:    "write",
		object:    "account:acme/project:atlas/document:42",
		wantAllow: false,
	},
	{
		name:      "deny (override): the sealed document is not readable",
		principal: "alice",
		action:    "read",
		object:    "account:acme/project:atlas/document:secret",
		wantAllow: false,
	},
}

// binaryPath is the compiled aperture binary, built once in TestMain.
var binaryPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aperture-e2e")
	if err != nil {
		panic("e2e: mktemp: " + err.Error())
	}
	defer os.RemoveAll(dir)

	binaryPath = filepath.Join(dir, "aperture")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("e2e: build binary: " + err.Error())
	}

	os.Exit(m.Run())
}

// TestE2E_CLICheck runs the built binary against the embedded example seed and
// asserts the printed verdict and the exit code for each case.
func TestE2E_CLICheck(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binaryPath, "check",
				"--account", account, tc.principal, tc.action, tc.object)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stdout
			err := cmd.Run()

			exitCode := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("running check: %v\noutput:\n%s", err, stdout.String())
				}
				exitCode = exit.ExitCode()
			}

			out := stdout.String()
			if tc.wantAllow {
				if exitCode != 0 {
					t.Errorf("want exit 0 for allow, got %d\noutput:\n%s", exitCode, out)
				}
				if !strings.HasPrefix(out, "allow\n") {
					t.Errorf("want output to start with %q, got:\n%s", "allow", out)
				}
			} else {
				if exitCode == 0 {
					t.Errorf("want non-zero exit for deny, got 0\noutput:\n%s", out)
				}
				if !strings.HasPrefix(out, "deny\n") {
					t.Errorf("want output to start with %q, got:\n%s", "deny", out)
				}
			}
		})
	}
}

// TestE2E_HTTPCheck boots the same service stack the serve command wires and
// asserts POST /check returns the matching decision JSON for each case.
func TestE2E_HTTPCheck(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := service.New(engine.New(store))
	srv := httptest.NewServer(server.New(svc))
	defer srv.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{
				"account":   account,
				"principal": tc.principal,
				"action":    tc.action,
				"object":    tc.object,
			})
			resp, err := http.Post(srv.URL+"/check", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /check: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d", resp.StatusCode)
			}
			var got struct {
				Allow            bool     `json:"allow"`
				Reason           string   `json:"reason"`
				DecidingGrantIDs []string `json:"deciding_grant_ids"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Allow != tc.wantAllow {
				t.Errorf("want allow=%v, got allow=%v (reason: %s)", tc.wantAllow, got.Allow, got.Reason)
			}
		})
	}
}

// TestE2E_HTTPBadInput asserts an ill-formed object is a 400 (input validation),
// not a fail-closed deny — the rule the service facade encodes.
func TestE2E_HTTPBadInput(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(server.New(service.New(engine.New(store))))
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"account":   account,
		"principal": "alice",
		"action":    "read",
		"object":    "not-a-valid-identity",
	})
	resp, err := http.Post(srv.URL+"/check", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /check: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for malformed object, got %d", resp.StatusCode)
	}
}
