package gosdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/mcp/toolmeta"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// example fixture coordinates (seed/testdata/example.yaml).
const (
	exPrincipal = "alice"
	exAction    = "read"
	exObject    = "account:acme/project:atlas/document:42"
	exPermRead  = "perm-doc-read"
)

// mutatingVerbs duplicates the core's ban list so the no-mutator assertion holds
// at the wire level (against the tools the SERVER actually advertises), not just
// over the static toolmeta table.
var mutatingVerbs = []string{
	"put", "create", "add", "set",
	"delete", "remove", "drop",
	"update", "edit", "write", "save",
	"bestow", "revoke", "grant_", " grant",
	"impersonate", "mutate", "import",
}

func TestRegisterGuards(t *testing.T) {
	svc := newReadService(t)
	if err := Register(nil, svc, Config{}); err == nil {
		t.Error("Register(nil server) should fail")
	}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil)
	if err := Register(srv, nil, Config{}); err == nil {
		t.Error("Register(nil service) should fail")
	}
}

func TestRegisteredToolsMirrorsToolmeta(t *testing.T) {
	got := RegisteredTools()
	want := toolmeta.Names()
	if len(got) != len(want) {
		t.Fatalf("RegisteredTools=%d, toolmeta.Names=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RegisteredTools[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// TestEndToEnd_NoMutatingToolAdvertised connects a real client over an in-memory
// transport, lists the tools the SERVER advertises, and asserts the surface is the
// full read/decide/simulate catalog with no mutating tool — the wire-level "no
// mutating tool exists" gate.
func TestEndToEnd_NoMutatingToolAdvertised(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != len(toolmeta.Names()) {
		t.Fatalf("server advertises %d tools, want %d", len(res.Tools), len(toolmeta.Names()))
	}
	for _, tool := range res.Tools {
		lower := strings.ToLower(tool.Name)
		for _, verb := range mutatingVerbs {
			if strings.Contains(lower, verb) {
				t.Errorf("server advertises mutating tool %q (verb %q)", tool.Name, verb)
			}
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q advertises a nil input schema", tool.Name)
		}
	}
}

// TestEndToEnd_CheckRoundTrips calls aperture_check over the wire against the
// seeded example model and asserts a structured decision comes back.
func TestEndToEnd_CheckRoundTrips(t *testing.T) {
	cs := connect(t)
	out := callTool(t, cs, toolmeta.ToolCheck, service.Query{
		Account:   seed.ExampleAccount,
		Principal: exPrincipal,
		Action:    exAction,
		Object:    exObject,
	})
	var dec service.Result
	if err := json.Unmarshal([]byte(out), &dec); err != nil {
		t.Fatalf("decode check result %q: %v", out, err)
	}
	if dec.Reason == "" {
		t.Errorf("check result has empty reason: %+v", dec)
	}
}

// TestEndToEnd_SimulateDoesNotPersist calls aperture_simulate with a hypothetical
// grant overlay, then asserts the live grant list is UNCHANGED — proving the
// what-if surface never writes.
func TestEndToEnd_SimulateDoesNotPersist(t *testing.T) {
	cs := connect(t)

	before := listGrantCount(t, cs)

	in := struct {
		Overlay service.Overlay `json:"overlay"`
		Query   service.Query   `json:"query"`
	}{
		Overlay: service.Overlay{Grants: []model.Grant{{
			ID:           "hypothetical-grant",
			AccountID:    seed.ExampleAccount,
			Subject:      model.Subject{Kind: model.SubjectPrincipal, ID: exPrincipal},
			PermissionID: exPermRead,
			Object:       "account:acme/**",
			Effect:       model.EffectAllow,
		}}},
		Query: service.Query{
			Account:   seed.ExampleAccount,
			Principal: exPrincipal,
			Action:    exAction,
			Object:    exObject,
		},
	}
	_ = callTool(t, cs, toolmeta.ToolSimulate, in)

	after := listGrantCount(t, cs)
	if before != after {
		t.Errorf("simulate persisted a grant: live count %d -> %d", before, after)
	}
}

// --- helpers ----------------------------------------------------------------

func newReadService(t *testing.T) *service.Service {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return service.New(engine.New(store), service.WithStorage(store))
}

func connect(t *testing.T) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "aperture", Version: "test"}, nil)
	if err := Register(srv, newReadService(t), Config{Version: "test"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcpsdk.ClientSession, name string, args any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("CallTool %s returned a tool error: %s", name, contentText(res))
	}
	return contentText(res)
}

func contentText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func listGrantCount(t *testing.T, cs *mcpsdk.ClientSession) int {
	t.Helper()
	out := callTool(t, cs, toolmeta.ToolListGrants, map[string]string{"account": seed.ExampleAccount})
	var lg struct {
		Grants []json.RawMessage `json:"grants"`
	}
	if err := json.Unmarshal([]byte(out), &lg); err != nil {
		t.Fatalf("decode list_grants %q: %v", out, err)
	}
	return len(lg.Grants)
}
