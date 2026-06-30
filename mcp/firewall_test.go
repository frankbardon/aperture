package mcp

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// forbiddenSDKModules are the MCP SDK module prefixes the SDK-free mcp/ core (and
// its leaf toolmeta + skills packages) must NEVER transitively depend on. The
// whole point of this package is that a consumer importing it pulls in no MCP SDK;
// this gate makes that guarantee load-bearing rather than aspirational. The SDK
// belongs exclusively in the mcp/gosdk adapter — if it ever leaks into the core
// dep graph this test fails loudly with the exact offending dependency line.
var forbiddenSDKModules = []string{
	"github.com/modelcontextprotocol/go-sdk",
}

// firewallCorePackages are the import paths whose full transitive dependency
// graphs the firewall covers: the core itself plus its leaf metadata and
// embedded-docs packages.
var firewallCorePackages = []string{
	"github.com/frankbardon/aperture/mcp",
	"github.com/frankbardon/aperture/mcp/toolmeta",
	"github.com/frankbardon/aperture/mcp/skills",
}

// allowedReachableDeps are intra-module packages that MUST stay reachable from
// the core. This is the sanity assertion that the firewall test is actually
// inspecting a populated dep graph (not silently passing on an empty/broken
// listing), and that the documented allow-list — the decision facade, the domain
// types it returns, the coded errors, the embedded docs — remains wired.
var allowedReachableDeps = []string{
	"github.com/frankbardon/aperture/service",      // the single decision facade
	"github.com/frankbardon/aperture/engine",       // Trace / BatchResult types
	"github.com/frankbardon/aperture/model",        // entity types
	"github.com/frankbardon/aperture/errors",       // CodedError system
	"github.com/frankbardon/aperture/mcp/skills",   // embedded skill docs
	"github.com/frankbardon/aperture/mcp/toolmeta", // leaf tool metadata
	"github.com/google/jsonschema-go/jsonschema",   // schema reflector
}

// listDeps shells out to `go list -deps <pkg>` and returns the full transitive
// dependency set (the listed package itself is included). It is module-aware and
// must run from the repo root (the test working directory is the package dir,
// which is inside the module, so go list resolves correctly under CI).
func listDeps(t *testing.T, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps %s failed: %v\nstderr: %s", pkg, err, stderr.String())
	}
	var deps []string
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			deps = append(deps, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan go list output for %s: %v", pkg, err)
	}
	if len(deps) == 0 {
		t.Fatalf("go list -deps %s returned no dependencies — firewall cannot inspect an empty graph", pkg)
	}
	return deps
}

// TestMCPCore_NoSDKImport is the import firewall: it asserts the transitive
// dependency set of the SDK-free mcp/ core (and its leaf packages) contains no MCP
// SDK module. If a future edit pulls the SDK into the core, this fails with the
// exact offending dependency so the regression is unambiguous.
//
// This assertion is REAL, not vacuous: moving an `import
// ".../modelcontextprotocol/go-sdk/..."` into any core package makes go list
// report it here and the test fails (verified by TestMCPCore_AllowedDepsReachable
// proving the graph is populated, and by the adapter living in mcp/gosdk, which is
// deliberately NOT in firewallCorePackages).
func TestMCPCore_NoSDKImport(t *testing.T) {
	for _, pkg := range firewallCorePackages {
		deps := listDeps(t, pkg)
		for _, dep := range deps {
			for _, banned := range forbiddenSDKModules {
				if dep == banned || strings.HasPrefix(dep, banned+"/") {
					t.Errorf("FIREWALL BREACH: %s transitively imports forbidden MCP SDK package %q (matched prefix %q). MCP SDK imports belong only in mcp/gosdk.", pkg, dep, banned)
				}
			}
		}
	}
}

// TestMCPCore_AllowedDepsReachable sanity-checks that the firewall is inspecting a
// real, populated dep graph by asserting every documented allow-list package is
// reachable from the core. A broken go-list invocation (or an accidental gutting
// of the core) would otherwise let TestMCPCore_NoSDKImport pass vacuously.
func TestMCPCore_AllowedDepsReachable(t *testing.T) {
	deps := listDeps(t, "github.com/frankbardon/aperture/mcp")
	have := make(map[string]bool, len(deps))
	for _, d := range deps {
		have[d] = true
	}
	for _, want := range allowedReachableDeps {
		if !have[want] {
			t.Errorf("expected allow-list dependency %q to be reachable from mcp/ core, but it was absent", want)
		}
	}
}
