package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file owns the gate. Everything in this package that needs a real
// PostgreSQL server goes through requirePostgres below, and the contract it
// implements is stated here once rather than restated per test file:
//
//		APERTURE_PG_INTEGRATION=1 \
//		APERTURE_PG_DSN='postgres://aperture@127.0.0.1:5432/aperture?sslmode=disable' \
//		go test -run TestPostgresLive ./storage/postgres/
//
//	  - UNGATED it SKIPS. CI runs with no service containers, so `make test` has
//	    to pass with no database present.
//	  - GATED WITH AN EMPTY DSN it FAILS. Asking for the live run and silently not
//	    getting one is the outcome a gate must never produce.
//	  - GATED WITH A VALUE THAT IS NEITHER ON NOR OFF it FAILS. This is the half
//	    the seed precedent leaves open: a bare `!= "1"` test turns
//	    APERTURE_PG_INTEGRATION=true into a silent skip, which is the same failure
//	    as an empty DSN wearing a different hat.
//
// The two environment variables are SHARED with seed/postgres_integration_test.go
// by name, so one exported DSN drives both suites in one shell; that sharing is
// itself a test below rather than a convention nobody rechecks.
//
// The decision is a pure function (decideGate) precisely so the three outcomes
// above are assertable without a server, in `make test`, on every push. A gate
// whose own behaviour is only observable by running it is not a gate.
const (
	pgGateEnv = "APERTURE_PG_INTEGRATION"
	pgDSNEnv  = "APERTURE_PG_DSN"
)

// gateDecision is what the environment asked for. Exactly one of Skip, Fail, or
// a usable DSN is set.
type gateDecision struct {
	DSN  string // the database to run against; set only when the run proceeds
	Skip string // non-empty: the live run was not asked for, and why we say so
	Fail string // non-empty: the live run WAS asked for and cannot happen
}

// gateOn and gateOff are the values the gate recognises. Anything else is a
// typo, and a typo must not be indistinguishable from "off".
var (
	gateOn  = []string{"1", "true", "yes", "on"}
	gateOff = []string{"", "0", "false", "no", "off"}
)

// decideGate maps the two environment values onto the contract. It touches no
// globals so the tests below can drive every branch directly.
func decideGate(gate, dsn string) gateDecision {
	norm := strings.ToLower(strings.TrimSpace(gate))
	switch {
	case contains(gateOff, norm):
		return gateDecision{Skip: "skipping the live PostgreSQL tests: set " + pgGateEnv +
			"=1 and " + pgDSNEnv + "=<dsn> to run them"}
	case !contains(gateOn, norm):
		return gateDecision{Fail: pgGateEnv + " is set to " + strconv.Quote(gate) +
			", which is neither on (" + strings.Join(gateOn, ", ") + ") nor off (" +
			strings.Join(gateOff[1:], ", ") + "). Refusing to guess: set " + pgGateEnv +
			"=1 to run the live tests, or unset it to skip them."}
	case strings.TrimSpace(dsn) == "":
		return gateDecision{Fail: pgGateEnv + "=" + gate + " but " + pgDSNEnv +
			" is empty: the gate is on and there is no database to run against. Export " +
			pgDSNEnv + "=<dsn> in the environment (never in a file), or unset " + pgGateEnv +
			" to skip the live tests."}
	}
	return gateDecision{DSN: dsn}
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// requirePostgres is the one entry point every live test in this package calls.
// It skips, fails, or returns the DSN, per decideGate.
func requirePostgres(t *testing.T) string {
	t.Helper()
	d := decideGate(os.Getenv(pgGateEnv), os.Getenv(pgDSNEnv))
	switch {
	case d.Skip != "":
		t.Skip(d.Skip)
	case d.Fail != "":
		t.Fatal(d.Fail)
	}
	return d.DSN
}

// ---- the gate's own behaviour, proved without a server ----

// TestGateSkipsWhenUngated is the criterion `make test` depends on: with nothing
// set, and with any spelling of "off", the live tests are skipped rather than
// attempted.
func TestGateSkipsWhenUngated(t *testing.T) {
	for _, off := range append([]string{"", "0", "false", "FALSE", "no", "off", "  "}, "OFF") {
		d := decideGate(off, dsnPlaceholder)
		if d.Skip == "" {
			t.Errorf("%s=%q did not skip (fail=%q dsn=%q)", pgGateEnv, off, d.Fail, d.DSN)
			continue
		}
		if d.Fail != "" || d.DSN != "" {
			t.Errorf("%s=%q skipped AND produced fail=%q dsn=%q", pgGateEnv, off, d.Fail, d.DSN)
		}
		// The skip has to say how to turn the run on, or the operator learns
		// only that something did not happen.
		if !strings.Contains(d.Skip, pgGateEnv) || !strings.Contains(d.Skip, pgDSNEnv) {
			t.Errorf("%s=%q skipped without naming both variables: %q", pgGateEnv, off, d.Skip)
		}
	}
}

// TestGateFailsWhenGatedWithAnEmptyDSN is the story's sharpest criterion: asking
// for the run and silently not getting it must be impossible. A whitespace-only
// DSN counts as empty — it is a shell accident, not a database.
func TestGateFailsWhenGatedWithAnEmptyDSN(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "yes", "on"} {
		for _, dsn := range []string{"", "   ", "\t"} {
			d := decideGate(on, dsn)
			if d.Fail == "" {
				t.Fatalf("%s=%q with %s=%q did not fail (skip=%q dsn=%q)",
					pgGateEnv, on, pgDSNEnv, dsn, d.Skip, d.DSN)
			}
			if d.Skip != "" {
				t.Errorf("%s=%q with an empty DSN produced a SKIP as well as a failure: %q",
					pgGateEnv, on, d.Skip)
			}
			// Actionable means: it names the variable to set, and it says the
			// DSN belongs in the environment rather than in a file.
			for _, want := range []string{pgDSNEnv, pgGateEnv, "environment"} {
				if !strings.Contains(d.Fail, want) {
					t.Errorf("the empty-DSN failure does not mention %q, so it does not tell the "+
						"operator what to do: %q", want, d.Fail)
				}
			}
		}
	}
}

// TestGateFailsOnAValueThatIsNeitherOnNorOff closes the silent-skip hole a bare
// `!= "1"` comparison leaves: APERTURE_PG_INTEGRATION=yolo is an operator who
// believes the live suite ran.
func TestGateFailsOnAValueThatIsNeitherOnNorOff(t *testing.T) {
	for _, v := range []string{"2", "yolo", "enabled", "-1"} {
		d := decideGate(v, dsnPlaceholder)
		if d.Fail == "" {
			outcome := "run"
			if d.Skip != "" {
				outcome = "skip"
			}
			t.Errorf("%s=%q was treated as %q instead of being refused; a typo must not be "+
				"indistinguishable from switching the suite off", pgGateEnv, v, outcome)
		}
	}
	// Surrounding whitespace is a shell accident, not a typo: "1 " is on.
	if d := decideGate("1 ", dsnPlaceholder); d.Fail != "" || d.Skip != "" {
		t.Errorf(`%s="1 " was refused (skip=%q fail=%q); leading and trailing space is trimmed`,
			pgGateEnv, d.Skip, d.Fail)
	}
}

// dsnPlaceholder stands in for the operator's DSN in the tests above. It is
// deliberately NOT a connection string: TestNoDSNIsCompiledIntoTheseSuites
// forbids one in this package's source, and a gate that exempted its own tests
// would be the first place a real DSN landed.
const dsnPlaceholder = "the-dsn-from-the-environment"

// TestGateRunsWhenGatedWithADSN is the positive case, and the reason the two
// above are not vacuous.
func TestGateRunsWhenGatedWithADSN(t *testing.T) {
	d := decideGate("1", dsnPlaceholder)
	if d.Skip != "" || d.Fail != "" {
		t.Fatalf("a fully gated environment did not run: skip=%q fail=%q", d.Skip, d.Fail)
	}
	if d.DSN != dsnPlaceholder {
		t.Errorf("DSN = %q, want the value from the environment verbatim", d.DSN)
	}
}

// ---- the gate's wiring, proved by parsing rather than by convention ----

// repoFile resolves a path relative to the module root, failing loudly if it has
// moved. Every gate in this file governs something outside this package, so
// "the file is not there" must be a failure and never a skip.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	p := filepath.Join(dir, rel)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("cannot read %s (%v). This gate governs it; if it moved, move the gate with it — "+
			"do not delete it.", rel, err)
	}
	return p
}

// parseGo parses one Go file, test files included.
func parseGo(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// liveTestFiles parses this package's *_live_test.go files.
func liveTestFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve the package directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_live_test.go") {
			continue
		}
		_, f := parseGo(t, filepath.Join(dir, e.Name()))
		out[e.Name()] = f
	}
	if len(out) < 2 {
		t.Fatalf("found %d *_live_test.go files in this package, expected at least 2. "+
			"Either the live tests moved or this gate is inspecting nothing.", len(out))
	}
	return out
}

// stringConst reads a package-level string constant's value out of a parsed file.
func stringConst(f *ast.File, name string) (string, bool) {
	var got string
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range spec.Names {
			if id.Name != name || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil {
				got, found = v, true
			}
		}
		return true
	})
	return got, found
}

// TestOneDSNDrivesBothSuites is the "shared gate" claim, checked rather than
// assumed: this package and seed/postgres_integration_test.go read the SAME two
// environment variables, so one exported DSN runs both in one shell. If either
// side renames its variable, an operator who exports one DSN gets a full run
// from one suite and a silent skip from the other — the exact outcome the gate
// exists to prevent, arriving through the back door.
func TestOneDSNDrivesBothSuites(t *testing.T) {
	_, seed := parseGo(t, repoFile(t, filepath.Join("seed", "postgres_integration_test.go")))

	for name, want := range map[string]string{"pgGateEnv": pgGateEnv, "pgDSNEnv": pgDSNEnv} {
		got, ok := stringConst(seed, name)
		if !ok {
			t.Fatalf("seed/postgres_integration_test.go declares no %s constant. The two suites share "+
				"one gate on purpose; if seed's gate was renamed, this package must follow it.", name)
		}
		if got != want {
			t.Errorf("seed's %s is %q but this package's is %q. One exported DSN must drive both suites.",
				name, got, want)
		}
	}
}

// TestEveryLiveTestGoesThroughTheGate proves the gate has no bypass. A live test
// that read the DSN itself would run in `make test` on any machine that happens
// to export one, and would skip silently on the machine of anyone who does not —
// which is the whole failure mode, reintroduced one test at a time.
func TestEveryLiveTestGoesThroughTheGate(t *testing.T) {
	files := liveTestFiles(t)

	var checked int
	for name, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil {
				continue
			}
			calls := callNames(fn.Body)
			gated := contains(calls, "requirePostgres")

			if strings.HasPrefix(fn.Name.Name, "TestPostgresLive_") {
				checked++
				if !gated {
					t.Errorf("%s: %s is named as a live test but never calls requirePostgres, so it "+
						"runs (or fails) with no database present", name, fn.Name.Name)
				}
				continue
			}
			if gated {
				t.Errorf("%s: %s calls requirePostgres but is not named TestPostgresLive_*, so "+
					"`go test -run TestPostgresLive` does not select it and a gated run silently omits it",
					name, fn.Name.Name)
			}
		}

		// Nothing may read the gate's environment behind requirePostgres's back.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Getenv" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "os" {
				return true
			}
			if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == pgDSNEnvIdent {
				t.Errorf("%s reads %s directly; the DSN must come from requirePostgres so the "+
					"empty-DSN failure cannot be bypassed", name, pgDSNEnvIdent)
			}
			return true
		})
	}

	if checked < 10 {
		t.Fatalf("inspected only %d TestPostgresLive_* functions; the live suite is larger than that, "+
			"so this gate is passing vacuously", checked)
	}
}

// pgDSNEnvIdent is the identifier (not the value) this package uses for the DSN
// variable; the gate above forbids reading it outside requirePostgres.
const pgDSNEnvIdent = "pgDSNEnv"

// callNames returns the names of every function and method called anywhere in a
// body, unqualified selectors included.
func callNames(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			out = append(out, fn.Name)
		case *ast.SelectorExpr:
			out = append(out, fn.Sel.Name)
		}
		return true
	})
	return out
}

// ---- the suite that runs, proved to be the WHOLE suite ----

// requiredConformanceCases is every top-level case storagetest.Run executes, as
// of this story. It is a FLOOR, not an exact set: adding a case to the shared
// contract needs no edit here, but removing one does. That asymmetry is the
// point. `storagetest.Run` is called by name, so a refactor that quietly drops a
// t.Run from it would shrink what this backend is proved to satisfy while every
// run still reported PASS, and the only visible trace would be a subtest count
// nobody was comparing against anything.
var requiredConformanceCases = []string{
	"AccountCRUD",
	"MembershipCRUDAndQueries",
	"ObjectTypeCRUD",
	"PermissionTypedAction",
	"PermissionUnknownObjectType",
	"PermissionDelegatable",
	"PrincipalCRUD",
	"RoleCRUD",
	"GroupCRUD",
	"GrantCRUDAndUpsert",
	"GrantValidation",
	"ListGrantsAccountScoped",
	"ListGrantsPageAllAccounts",
	"ListGrantsPagePagination",
	"ListGrantsPageMaxPageSize",
	"GrantsForSubjects",
	"GrantsForSubjectsWildcardAccount",
	"GroupsForPrincipal",
	"NotFoundSemantics",
	"TimestampsRoundTrip",
	"TimestampUnsetRoundTrip",
	"TimestampSubMicrosecondPrecision",
	"TimestampRangeBoundaries",
	"TimestampOutOfRangeRefused",
	"AuditTimestampContract",
	"AuditAppendAndQuery",
	"AuditQueryFilters",
	"AuditRetentionPrune",
	"TemplateCRUDAndVersions",
	"TemplateValidation",
	"RuleCRUD",
	"RuleValidation",
	"WiringRoundTrip",
	"WiringReplaceIsWholesale",
	"WiringDeclaredKeySetIsOptional",
	"WiringValidation",
	"WiringReplaceIsAllOrNothing",
	"WiringNotFoundSemantics",
	"AtomicCommit",
	"AtomicRollback",
	"ReferentialWriteRefusesAnUnknownParent",
	"ReferentialRefusedWriteIsAllOrNothing",
	"ReferentialRestrictRefusesADeleteThatWouldOrphan",
	"ReferentialCascadeRemovesTheJoinRowsWithTheirOwner",
	"GrantSubjectMustExistInTheTableItsKindSelects",
	"DeletingAGrantSubjectIsRefused",
	"AccountReferenceIsARowOrTheWildcard",
	"DeletingAnAccountWithLiveChildrenIsRefused",
	"WildcardRowsDoNotPinARealAccount",
}

// TestTheLiveConformanceRunsTheWholeSuite pins both halves of "the whole
// storagetest suite, not a subset".
//
// FIRST, that the suite still holds every case this backend has been proved
// against: storagetest.Run's own body is parsed and its top-level t.Run names
// are diffed against requiredConformanceCases.
//
// SECOND, that this package enters the suite only through Run. storagetest
// exports exactly one runner on purpose; a live test that reached for anything
// else would be choosing its own subset, which is how "conformance" quietly
// becomes "the cases that pass".
func TestTheLiveConformanceRunsTheWholeSuite(t *testing.T) {
	path := repoFile(t, filepath.Join("storage", "storagetest", "storagetest.go"))
	_, suite := parseGo(t, path)

	var run *ast.FuncDecl
	for _, decl := range suite.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "Run" {
			run = fn
		}
	}
	if run == nil || run.Body == nil {
		t.Fatalf("storagetest.Run is not declared in %s. It is the single entry point this backend's "+
			"conformance depends on.", path)
	}

	// Only the direct children of Run's body: the cases Run itself schedules,
	// not the nested subtests each case fans out into.
	present := map[string]bool{}
	for _, stmt := range run.Body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Run" {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "t" {
			continue
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if name, err := strconv.Unquote(lit.Value); err == nil {
			present[name] = true
		}
	}

	var missing []string
	for _, want := range requiredConformanceCases {
		if !present[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("storagetest.Run no longer schedules %d case(s) this backend was proved against:\n  %s\n\n"+
			"CI cannot run this backend, so the gated conformance run is the only evidence it behaves like "+
			"storage/sqlite. A case that leaves the shared contract silently removes that evidence — if the "+
			"removal is deliberate, delete it from requiredConformanceCases in the same change.",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(present) < len(requiredConformanceCases) {
		t.Errorf("storagetest.Run schedules %d top-level cases, fewer than the %d recorded here",
			len(present), len(requiredConformanceCases))
	}

	// And this package enters the suite only through Run.
	var runners int
	for name, f := range liveTestFiles(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "storagetest" {
				return true
			}
			switch sel.Sel.Name {
			case "Run":
				runners++
			case "Factory":
				// a type reference; harmless
			default:
				t.Errorf("%s reaches into storagetest.%s. The conformance suite has one entry point; "+
					"anything else is this backend selecting its own subset.", name, sel.Sel.Name)
			}
			return true
		})
	}
	// Two: once unqualified, once through a configured schema.
	if runners < 2 {
		t.Errorf("found %d call(s) to storagetest.Run in the live tests, want at least 2 — the suite is "+
			"run both unqualified and against a configured schema", runners)
	}
}

// ---- the gate stays OUT of the default build ----

// TestTheLiveSuiteIsNotInMakeTestOrCI is the "not added to `make test` or CI"
// criterion, kept honest by reading the two files that could break it. A gated
// suite that something switches on centrally is not gated; it is a service
// dependency CI does not have.
func TestTheLiveSuiteIsNotInMakeTestOrCI(t *testing.T) {
	paths := []string{filepath.Join("Makefile")}
	wf := repoFile(t, filepath.Join(".github", "workflows"))
	entries, err := os.ReadDir(wf)
	if err != nil {
		t.Fatalf("read .github/workflows: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")) {
			paths = append(paths, filepath.Join(".github", "workflows", e.Name()))
		}
	}
	if len(paths) < 2 {
		t.Fatalf("found no workflow files to inspect; this gate is passing vacuously")
	}

	for _, rel := range paths {
		b, err := os.ReadFile(repoFile(t, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		// Comments are stripped first. Both files DOCUMENT the gate on purpose —
		// the Makefile's `test` target carries the invocation an operator needs —
		// and prose starts no database.
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "#"); j >= 0 {
				code = code[:j]
			}
			if strings.TrimSpace(code) == "" {
				continue
			}
			for _, needle := range []string{pgGateEnv, pgDSNEnv, "TestPostgresLive"} {
				if strings.Contains(code, needle) {
					t.Errorf("%s:%d wires %s into the build: %q\n\n"+
						"The live PostgreSQL suite is gated because CI has no service containers. "+
						"Switching it on centrally makes every push depend on a database that is not there.",
						rel, i+1, needle, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestNoDSNIsCompiledIntoTheseSuites is the "no DSN appears in any file"
// criterion, stated as the thing a compiler could otherwise carry: no string
// LITERAL reachable from a test that CONNECTS is a database connection string.
// The DSN reaches the live suites through the environment and nowhere else.
//
// Two exclusions, both deliberate:
//
//   - COMMENTS are not scanned. The invocation lines at the top of this file, of
//     seed/postgres_integration_test.go, and in the Makefile are how an operator
//     learns to run the suite at all, and prose connects to nothing.
//   - The OFFLINE tests are not scanned. config_test.go and postgres_test.go
//     pass a deliberately unreachable placeholder to Open, which by contract does
//     not connect; that literal is a fixture, not a target. Scanning them would
//     force the placeholder into an environment variable and turn tests that need
//     no database into tests that read one.
//
// What is left is exactly the set that matters: every file that opens a real
// connection.
func TestNoDSNIsCompiledIntoTheseSuites(t *testing.T) {
	files := map[string]*ast.File{
		filepath.Join("seed", "postgres_integration_test.go"): nil,
	}
	for rel := range files {
		_, f := parseGo(t, repoFile(t, rel))
		files[rel] = f
	}
	for name, f := range liveTestFiles(t) {
		files[name] = f
	}

	// A DSN is a connection string with a target in it. "postgres" alone is a
	// driver name and appears legitimately; "postgres://host/db" is a database.
	isDSN := func(s string) bool {
		low := strings.ToLower(s)
		for _, scheme := range []string{"postgres://", "postgresql://"} {
			if i := strings.Index(low, scheme); i >= 0 && len(low) > i+len(scheme) {
				return true
			}
		}
		// libpq keyword form: a DSN only once it names both a host and a database.
		return strings.Contains(low, "host=") && strings.Contains(low, "dbname=")
	}

	var scanned int
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			scanned++
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if isDSN(v) {
				t.Errorf("%s holds a connection string (%q). The DSN belongs in the environment: a "+
					"committed one is a credential in git and a suite that talks to whatever it names.",
					name, v)
			}
			return true
		})
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d string literals across the live suites; this gate is passing vacuously",
			scanned)
	}
}
