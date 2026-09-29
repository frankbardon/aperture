package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// THE SERVE-ONLY FACADE EXTRAS ARE COMPOSED IN EXACTLY ONE PLACE.
//
// serveFacadeOptions is the seven-plus-one set of dependencies `serve` layers over
// the shared decision stack: storage for the mutation path, the authority gate, the
// delegation and impersonation services, the audit recorder, the editor's rule
// source, the deployment's entity-management posture and the staleness recorder.
//
// It is a FUNCTION and not a literal because it is composed twice over the life of
// a process: once at the boot, and again for every wiring version a refresh
// installs. Four of those options are built over the stack's ENGINE, so a rebuild
// that composed them by hand — or failed to compose them at all — would leave the
// authority gate, delegation and impersonation deciding through the SUPERSEDED
// engine while Check answered through the new one. That is not a torn read inside
// one decision; it is two engines in one process, and nothing in any verdict, trace
// or note reports it. The other four are process-lifetime values (the store, the
// audit recorder, the managed-entity posture, the one staleness recorder the poller
// writes to), and a rebuild that took a FRESH recorder would report a permanently
// healthy instance no matter what the loop observed.
//
// Both halves of that are a source property rather than a behavioural one: a second
// composition site compiles, passes every existing test, and serves. So this is a
// go/ast scan, in the shape of the other structural gates in this package, and it
// asserts the two things that would have to be true of any correct arrangement:
//
//   - no `service.With*` option is constructed anywhere in serve.go except inside
//     serveFacadeOptions; and
//   - every facade built in serve.go is built by spreading serveFacadeOptions, and
//     there is MORE THAN ONE such site — the boot and the rebuild. A single site
//     would mean the rebuild had stopped rebuilding the facade at all, which is its
//     own regression: service.WithProviders / WithAttributes take CONCRETE
//     registries, so a version that reused the boot's facade would leave
//     ObjectIdentifiers, ObjectMetadataBatch and Simulate's overlay reading the
//     boot's registries after a push.
//
// It deliberately scans serve.go alone. The SHARED stack options
// (service.WithProviders / WithAttributes / WithDeclaredAttributeKeys) are composed
// in decision.go's newService, which is where every command that decides picks them
// up; they are not serve-only extras and are not this gate's business.
func TestTheServeOnlyFacadeExtrasAreComposedInExactlyOnePlace(t *testing.T) {
	const file = "serve.go"
	const composer = "serveFacadeOptions"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		// Fails rather than skips: a gate that cannot find its subject is a gate
		// that is checking nothing, and the file moving is exactly when the
		// invariant needs re-deriving.
		t.Fatalf("parse %s: %v (this gate is about that file; if it moved, move the gate)", file, err)
	}

	// Half one: every service.With* option in this file is built inside the one
	// composer. enclosing tracks which function declaration the walk is inside, so
	// the failure can name the offender.
	var enclosing string
	var strays []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			enclosing = node.Name.Name
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "service" || !strings.HasPrefix(sel.Sel.Name, "With") {
				return true
			}
			if enclosing != composer {
				strays = append(strays, "service."+sel.Sel.Name+" in "+enclosing+
					" at "+fset.Position(node.Pos()).String())
			}
		}
		return true
	})
	if len(strays) > 0 {
		t.Errorf("%s composes a serve-only facade option outside %s:\n  %s\n\n"+
			"Every one of them must be built in %s and nowhere else. Four are built over the "+
			"stack's ENGINE, so a second site leaves the authority gate, delegation and "+
			"impersonation deciding through a superseded engine while Check answers through the "+
			"new one — two engines in one process, which no verdict, trace or note reports.",
			file, composer, strings.Join(strays, "\n  "), composer)
	}

	// Half two: every facade built here is built by spreading the composer, and
	// there is more than one such site.
	var facades, spreads int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "newService" {
			return true
		}
		facades++
		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == composer && call.Ellipsis != token.NoPos {
				spreads++
				return true
			}
		}
		t.Errorf("%s builds a facade at %s without spreading %s. Both the boot and the wiring "+
			"rebuild must go through it, or a rebuilt facade can take a different gate, a different "+
			"recorder or a different engine from the boot's.",
			file, fset.Position(call.Pos()), composer)
		return true
	})
	if facades < 2 {
		t.Errorf("%s builds %d facade(s); expected at least two — the boot and the wiring rebuild. "+
			"If the rebuild stopped building one, a version installed by a swap is serving the BOOT's "+
			"provider and attribute registries, because service.WithProviders / WithAttributes take "+
			"concrete registries.", file, facades)
	}
	if spreads != facades {
		t.Errorf("%s: %d of %d facades spread %s", file, spreads, facades, composer)
	}
}
