package mcp

import (
	"strings"
	"testing"

	"github.com/frankbardon/aperture/mcp/toolmeta"
)

// mutatingVerbs are the verb tokens a MUTATING tool name would carry. The
// Aperture MCP surface is read/decide/simulate only, so none may appear in any
// registered tool name. This is the "no mutating tool exists" gate: it enumerates
// the registry rather than trusting a hand-maintained list, so adding a mutator
// tool (or surfacing the facade's Put*/Delete*/Bestow/Revoke) trips it.
var mutatingVerbs = []string{
	"put", "create", "add", "set",
	"delete", "remove", "drop",
	"update", "edit", "write", "save",
	"bestow", "revoke", "grant_", " grant",
	"impersonate", "mutate", "import",
}

// TestNoMutatingTool asserts every registered tool name (from the catalog AND the
// toolmeta table — they must agree) carries no mutating verb. "grant" as a noun is
// allowed (aperture_get_grant / aperture_list_grants inspect grants); the banned
// tokens target the VERB forms (grant_, " grant") a write tool would use.
func TestNoMutatingTool(t *testing.T) {
	for _, name := range toolmeta.Names() {
		lower := strings.ToLower(name)
		for _, verb := range mutatingVerbs {
			if strings.Contains(lower, verb) {
				t.Errorf("tool %q contains mutating verb %q — the MCP surface must be read-only", name, verb)
			}
		}
	}
}

// TestCatalogMatchesToolmeta asserts the type-erased catalog (Tools) is in
// lockstep with the toolmeta table: same names, same order, every descriptor has
// a non-nil Invoke and a non-empty input schema (AddTool requires a non-nil object
// schema). A drift here means a tool was added to one place but not the other.
func TestCatalogMatchesToolmeta(t *testing.T) {
	names := toolmeta.Names()
	tools := Tools(Config{Version: "test"})
	if len(tools) != len(names) {
		t.Fatalf("catalog has %d tools, toolmeta has %d names", len(tools), len(names))
	}
	for i, d := range tools {
		if d.Name != names[i] {
			t.Errorf("catalog[%d] = %q, want %q", i, d.Name, names[i])
		}
		if d.Invoke == nil {
			t.Errorf("tool %q has a nil Invoke", d.Name)
		}
		if len(d.InputSchema) == 0 {
			t.Errorf("tool %q has an empty input schema", d.Name)
		}
		if d.Description == "" {
			t.Errorf("tool %q has an empty description", d.Name)
		}
	}
}

// TestSchemaReflectionClean asserts package-init schema reflection produced no
// errors — the recursive-type guard. If a future contract type introduces a
// Go-level cycle, jsonschema-go returns an error here and this fails, prompting
// the offending field to be typed `any`.
func TestSchemaReflectionClean(t *testing.T) {
	if len(reflectErrors) != 0 {
		for k, err := range reflectErrors {
			t.Errorf("schema reflection error for %s: %v", k, err)
		}
	}
}
