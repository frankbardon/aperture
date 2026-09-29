package cli

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"

	ucli "github.com/urfave/cli/v3"
)

// E5-S2, driven through the real command tree against a real store.
//
// The fixtures and helpers are wiring_test.go's (wiringModelSeed,
// wiringSharedSeed, newWiringStore, writeWiringSeed, runWiringCLI, openWiringStore,
// mustRefuse, wiringSubcommand) — a diff has to be tested against the wiring a PUSH
// actually produces, and re-declaring the deployed side here would let the two drift
// into a diff that reports clean for a shape nothing deploys.

// runWiringDiffCLI runs `aperture wiring diff --seed <path> --store <dsn>` through
// the real command tree.
//
// It cannot go through runWiringCLI: drift is signalled with a non-zero ExitCoder,
// which urfave/cli hands to os.Exit unless an ExitErrHandler is installed — so the
// shared runner would end the test BINARY on the first drift case rather than fail a
// test. The no-op handler keeps the exit code observable inside the process, the way
// runCheckCommand does for a clean deny.
func runWiringDiffCLI(t *testing.T, seedPath, storeDSN string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	app.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	argv := []string{"aperture", "wiring", "diff"}
	if seedPath != "" {
		argv = append(argv, "--seed", seedPath)
	}
	if storeDSN != "" {
		argv = append(argv, "--store", storeDSN)
	}
	err := app.Run(context.Background(), append(argv, args...))
	return out.String(), err
}

// wiringDiffExit reports the process exit code a returned error would produce, and
// whether it got there as an ExitCoder.
//
// The second half is the assertion that matters. A coded refusal is NOT an
// ExitCoder — cmd/aperture/main.go prints it and exits 1 — so "is it an ExitCoder?"
// is exactly the difference between "the diff ran and found drift" and "the diff
// could not run", which is the separation the drift exit code exists to preserve.
func wiringDiffExit(t *testing.T, err error) (int, bool) {
	t.Helper()
	if err == nil {
		return 0, false
	}
	var coder ucli.ExitCoder
	if errors.As(err, &coder) {
		return coder.ExitCode(), true
	}
	// main.go's fallback for anything that is not an ExitCoder.
	return 1, false
}

// wiringDiffedSeed is wiringSharedSeed with four deliberate differences, one per
// section and one of each kind the report has to name:
//
//   - connections: "reporting" is gone (only deployed) and "extra" is new (only local)
//   - providers: the "project" entry is gone (only deployed) and "document" carries a
//     different ttl: (differs)
//   - field_types: "due" is gone (only deployed) and "created_at" is new (only local)
//   - attribute_providers: identical, so the section reports NOTHING — a diff that
//     listed every entry it compared would bury the four that changed
const wiringDiffedSeed = `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
  extra:
    dsn_env: APERTURE_TEST_EXTRA_DSN
providers:
  - object_type: document
    kind: sql
    connection: main
    get_one: SELECT owner, project_ids FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.owner, d.project_ids FROM documents d
    id_column: id
    ttl: 10m
    max_size: 64
    references:
      project_ids: project
      owner_ids: document
field_types:
  - object_type: document
    fields:
      published_at: datetime
      created_at: date
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department FROM users u
    ttl: 30s
    max_size: 100
`

// wiringSharedSeedWithCSV is wiringSharedSeed's four SHARED sections unchanged, plus
// one csv provider and one csv attribute slot. Nothing a push could deploy differs,
// so the only thing the diff has to say about it is that the two csv entries are
// local by design.
const wiringSharedSeedWithCSV = `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
  reporting:
    dsn_env: APERTURE_TEST_REPORTING_DSN
providers:
  - object_type: document
    kind: sql
    connection: main
    get_one: SELECT owner, project_ids FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.owner, d.project_ids FROM documents d
    id_column: id
    ttl: 5m
    max_size: 64
    references:
      project_ids: project
      owner_ids: document
  - object_type: project
    kind: sql
    connection: reporting
    get_one: SELECT name FROM projects WHERE id = $1
    get_all: SELECT 'project:' || p.id AS id, p.name FROM projects p
  - object_type: note
    kind: csv
    path: notes.csv
field_types:
  - object_type: document
    fields:
      published_at: datetime
      due: date
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department FROM users u
    ttl: 30s
    max_size: 100
  - subject: machine
    kind: csv
    path: machines.csv
`

// deployedWiringStore pushes wiringSharedSeed to a fresh store and returns its DSN.
func deployedWiringStore(t *testing.T) string {
	t.Helper()
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	return dsn
}

// TestDiffOfTheDeployedDocumentIsCleanAndExitsZero is the story's central
// criterion, and the stamps are what make it non-trivial: a push stamps every row
// with the instant it ran and the document carries no stamp at all, so an
// implementation that compared them would report drift here on every run.
func TestDiffOfTheDeployedDocumentIsCleanAndExitsZero(t *testing.T) {
	dsn := deployedWiringStore(t)
	seedPath := writeWiringSeed(t, wiringSharedSeed)

	out, err := runWiringDiffCLI(t, seedPath, dsn)
	code, isExit := wiringDiffExit(t, err)
	if code != 0 || isExit {
		t.Fatalf("diffing a deployment against the document it was pushed from exited %d (ExitCoder=%v): %v\n%s",
			code, isExit, err, out)
	}
	if !strings.Contains(out, "no drift") {
		t.Errorf("a clean diff does not say so:\n%s", out)
	}
	for _, forbidden := range []string{"only deployed", "only local", "differs:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a clean diff reports %q:\n%s", forbidden, out)
		}
	}
}

// TestDiffNamesWhatIsOnlyDeployedWhatIsOnlyLocalAndWhatDiffers is the report
// criterion, per section and per entry.
func TestDiffNamesWhatIsOnlyDeployedWhatIsOnlyLocalAndWhatDiffers(t *testing.T) {
	dsn := deployedWiringStore(t)
	out, err := runWiringDiffCLI(t, writeWiringSeed(t, wiringDiffedSeed), dsn)
	if code, isExit := wiringDiffExit(t, err); code == 0 || !isExit {
		t.Fatalf("drift must exit non-zero through an ExitCoder; got %d (ExitCoder=%v): %v\n%s",
			code, isExit, err, out)
	}

	// The section column is the DOCUMENT's own section key, so a reported entry is one
	// an operator can search the --seed file for.
	for _, want := range []string{
		"connections", "reporting", "only deployed",
		"extra", "only local",
		"providers", "project",
		"document", "differs: ttl",
		"field_types", "document.due", "document.created_at",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
	// The one section that matches must contribute no row at all.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "attribute_providers") {
			t.Errorf("the identical attribute_providers: section produced a row: %q", line)
		}
	}
	// The remedy is named, and in the one direction that exists.
	if !strings.Contains(out, "wiring push") {
		t.Errorf("the report does not say how to make the deployment match the document:\n%s", out)
	}
}

// TestDriftExitsTwoAndARefusalExitsOne is the property that makes this command
// usable in a pipeline, and it is asserted as ONE comparison because the two halves
// are only meaningful together.
//
// A gate has three outcomes to act on differently: the deployment matches the
// repository (0), it does not (2), or the diff could not tell (1 — an unreadable
// document, a --store naming nothing, a schema this build cannot read). If drift
// exited 1 like every coded refusal does, a gate that failed closed on drift would
// also fail closed on a typo in its own DSN, and report it as drift.
func TestDriftExitsTwoAndARefusalExitsOne(t *testing.T) {
	dsn := deployedWiringStore(t)

	_, driftErr := runWiringDiffCLI(t, writeWiringSeed(t, wiringDiffedSeed), dsn)
	driftCode, driftIsExit := wiringDiffExit(t, driftErr)
	if driftCode != wiringDriftExitCode || !driftIsExit {
		t.Errorf("drift exited %d (ExitCoder=%v), want %d through an ExitCoder",
			driftCode, driftIsExit, wiringDriftExitCode)
	}
	// And it carries NO Aperture code: drift is the answer to the question, not a
	// failure to answer it, so there is no remedy for a registry fixup to name.
	if got := aerr.CodeOf(driftErr); got != "" {
		t.Errorf("the drift exit carries code %s; drift is a verdict and not a refusal, and coding it would put a remedy on a report", got)
	}

	// The same command, unable to run at all.
	_, refusalErr := runWiringDiffCLI(t, "", dsn)
	refusalCode, refusalIsExit := wiringDiffExit(t, refusalErr)
	if refusalIsExit {
		t.Errorf("a refusal came back as an ExitCoder, so it chose its own exit code: %v", refusalErr)
	}
	if refusalCode == wiringDriftExitCode {
		t.Errorf("a refusal and drift exit with the same code %d; a pipeline cannot then tell \"they differ\" from \"I could not tell whether they differ\"", refusalCode)
	}
}

// TestDiffIgnoresTheFormattingOfTheLocalDocument is the canonical-ordering
// criterion. Both sides are compared as WIRING, in the order a read returns, so a
// re-ordered or re-indented repository file is not drift — which is what makes the
// command safe to put in a gate at all.
func TestDiffIgnoresTheFormattingOfTheLocalDocument(t *testing.T) {
	const reordered = `
attribute_providers:
  - {subject: user, kind: sql, connection: main, get_one: "SELECT department FROM users WHERE id = $1", get_all: "SELECT u.id AS id, u.department FROM users u", ttl: 30s, max_size: 100}
field_types:
  - object_type: document
    fields: {due: date, published_at: datetime}
providers:
  - object_type: project
    kind: sql
    connection: reporting
    get_one: SELECT name FROM projects WHERE id = $1
    get_all: SELECT 'project:' || p.id AS id, p.name FROM projects p
  - {object_type: document, kind: sql, connection: main, get_one: "SELECT owner, project_ids FROM documents WHERE id = $1", get_all: "SELECT 'document:' || d.id AS id, d.owner, d.project_ids FROM documents d", id_column: id, ttl: 5m, max_size: 64, references: {owner_ids: document, project_ids: project}}
connections:
  reporting: {dsn_env: SOMETHING_ELSE_ENTIRELY}
  main: {dsn_env: AND_ANOTHER}
`
	dsn := deployedWiringStore(t)
	out, err := runWiringDiffCLI(t, writeWiringSeed(t, reordered), dsn)
	if code, isExit := wiringDiffExit(t, err); code != 0 || isExit {
		t.Fatalf("the same wiring written in a different order reported drift (exit %d, ExitCoder=%v): %v\n%s",
			code, isExit, err, out)
	}
	if !strings.Contains(out, "no drift") {
		t.Fatalf("expected a clean report:\n%s", out)
	}
	// Non-vacuous in the one place it would be easy to get wrong: the dsn_env: names
	// differ between the two documents and are NOT drift, because shared wiring holds
	// the connection's name and nothing else. A diff that compared them would report
	// drift between two correctly-wired instances of one deployment.
	for _, forbidden := range []string{"dsn_env", "SOMETHING_ELSE_ENTIRELY", "AND_ANOTHER"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the report mentions %q, a per-instance fact with no column in shared wiring:\n%s", forbidden, out)
		}
	}
}

// TestDiffReportsACSVEntryAsLocalOnlyByDesign is the kind: csv criterion.
//
// A csv entry is refused at push — its only data source is a filesystem path — so no
// push can ever make the deployment match it. It is reported, and it is NOT drift:
// counting it would leave a permanently non-zero exit code on every deployment that
// legitimately keeps a local csv provider, and a gate that cannot go green is a gate
// somebody switches off — after which the drift that matters goes unreported too.
func TestDiffReportsACSVEntryAsLocalOnlyByDesign(t *testing.T) {
	dsn := deployedWiringStore(t)
	out, err := runWiringDiffCLI(t, writeWiringSeed(t, wiringSharedSeedWithCSV), dsn)

	if code, isExit := wiringDiffExit(t, err); code != 0 || isExit {
		t.Fatalf("a kind: csv entry made the diff exit %d (ExitCoder=%v); it is local-only BY DESIGN and not drift: %v\n%s",
			code, isExit, err, out)
	}
	if got := aerr.CodeOf(err); got != "" {
		t.Fatalf("a csv entry produced code %s; a push refusing it is not this command's refusal", got)
	}
	if !strings.Contains(out, "no drift") {
		t.Errorf("a document whose only difference is two csv entries is not reported as clean:\n%s", out)
	}
	for _, want := range []string{
		"local-only by design", `providers entry "note"`, `attribute_providers entry "machine"`, "kind: csv",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q:\n%s", want, out)
		}
	}
	// The sql entries beside them are still compared, so the partition removed the csv
	// entries and not the section.
	if strings.Contains(out, "only local") {
		t.Errorf("a csv entry was reported as ordinary only-local drift rather than as local by design:\n%s", out)
	}
}

// TestDiffAgainstAStoreWithNothingDeployedReportsEverythingLocalOnly is the
// criterion that deliberately disagrees with `wiring pull`.
//
// A pull REFUSES an empty store (APERTURE_WIRING_NOTHING_DEPLOYED), because the
// document it would write is one that, pushed back, replaces the deployment's wiring
// with nothing. A diff writes no file and pushes nothing, so the empty read is a
// perfectly good answer — and it has to say so, because a mistyped --store names a
// database Setup creates and looks exactly like this.
func TestDiffAgainstAStoreWithNothingDeployedReportsEverythingLocalOnly(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	out, err := runWiringDiffCLI(t, writeWiringSeed(t, wiringSharedSeed), dsn)

	if got := aerr.CodeOf(err); got != "" {
		t.Fatalf("a diff against an empty store refused with %s; an empty store is an answer here, not a refusal: %v\n%s",
			got, err, out)
	}
	if code, isExit := wiringDiffExit(t, err); code != wiringDriftExitCode || !isExit {
		t.Fatalf("a wholly undeployed store exited %d (ExitCoder=%v), want %d: everything local is drift\n%s",
			code, isExit, wiringDriftExitCode, out)
	}
	if !strings.Contains(out, "no shared wiring is deployed to this store") {
		t.Errorf("the report does not say the store is empty, so a table of only-local rows is indistinguishable from real drift:\n%s", out)
	}
	if !strings.Contains(out, "--store DSN") {
		t.Errorf("the report does not name the DSN as the thing to check; a fresh database Setup created looks exactly like a store nobody pushed to:\n%s", out)
	}
	// Every section's entries are only-local, one row each, and nothing is only-deployed.
	for _, want := range []string{"main", "reporting", "document", "project", "document.due", "user"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not list %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "only deployed") || strings.Contains(out, "differs:") {
		t.Errorf("a store with nothing deployed reported something as only deployed or differing:\n%s", out)
	}
}

// TestDiffReportsADeclaredKeySetTheDocumentDoesNotDeclare is the inverted form of a
// case that used to assert the opposite.
//
// While the seed `attribute_providers:` schema had no key for a declared key set, a
// slot declared in the store was silent in EVERY document, so comparing the field
// would have reported drift that no edit could clear. The diff excused it and named
// the excusal. That excusal was correct exactly as long as the field was
// inexpressible.
//
// It is expressible now: `declared_keys:` exists. So a silent document really does
// mean "not declared", the difference IS resolvable by an edit, and excusing it
// would hide drift rather than suppress noise — which is worse than the permanent
// red gate the excusal was avoiding, because a clean report is trusted. Removing a
// `declared_keys:` line from the committed document is a real change to what rules
// may name, and it must show.
func TestDiffReportsADeclaredKeySetTheDocumentDoesNotDeclare(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	store := openWiringStore(t, dsn)
	if err := store.ReplaceWiring(context.Background(), model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main"}},
		AttributeProviders: []model.WiringAttributeProvider{{
			Subject: "user", Kind: "sql", Connection: "main",
			GetOne:       "SELECT department FROM users WHERE id = $1",
			GetAll:       "SELECT u.id AS id, u.department FROM users u",
			TTL:          "30s",
			MaxSize:      100,
			DeclaredKeys: model.DeclaredKeys{Declared: true, Keys: []string{"clearance", "department"}},
		}},
	}); err != nil {
		t.Fatalf("replace wiring: %v", err)
	}

	// Everything matches except the declared key set, which this document is silent
	// about — and silence is now a statement, not an absence of vocabulary.
	silent := writeWiringSeed(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department FROM users u
    ttl: 30s
    max_size: 100
`)
	out, err := runWiringDiffCLI(t, silent, dsn)
	code, isExit := wiringDiffExit(t, err)
	if code != wiringDriftExitCode || !isExit {
		t.Fatalf("a store-declared key set against a silent document exited %d (ExitCoder=%v), want %d: "+
			"the field is expressible now, so not reporting it hides a real change to what rules may name: %v\n%s",
			code, isExit, wiringDriftExitCode, err, out)
	}
	if !strings.Contains(out, "declared_keys") {
		t.Errorf("the drift report does not name declared_keys as the differing field:\n%s", out)
	}
	if strings.Contains(out, "not compared") {
		t.Errorf("the field is still being excused, which is what this test exists to forbid:\n%s", out)
	}

	// And the drift is resolvable by an edit, which is the property that made the
	// excusal unnecessary. Same store, same command, one line added.
	declaring := writeWiringSeed(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department FROM users u
    ttl: 30s
    max_size: 100
    declared_keys: [clearance, department]
`)
	out, err = runWiringDiffCLI(t, declaring, dsn)
	if code, isExit := wiringDiffExit(t, err); code != 0 || isExit {
		t.Fatalf("declaring the same key set still reported drift, exit %d (ExitCoder=%v): %v\n%s", code, isExit, err, out)
	}
}

// TestDiffRefusesWhatCannotBeCompared: the flags, and a document a push would
// refuse. Every refusal is coded, and every one has EXACTLY ONE Aperture-coded error
// in its chain — aerr.Wrap re-stamps, so depth is what proves the pass-through guard
// is written at each wrap site.
func TestDiffRefusesWhatCannotBeCompared(t *testing.T) {
	deployed := deployedWiringStore(t)

	_, err := runWiringDiffCLI(t, "", deployed)
	mustRefuse(t, "a diff with no --seed", err, aerr.APERTURE_INVALID_INPUT, "--seed")

	_, err = runWiringDiffCLI(t, writeWiringSeed(t, wiringSharedSeed), "")
	mustRefuse(t, "a diff with no --store", err, aerr.APERTURE_INVALID_INPUT, "--store")

	_, err = runWiringDiffCLI(t, writeWiringSeed(t, `
providers:
  - object_type: document
    kind: parquet
    connection: main
`), deployed)
	mustRefuse(t, "a document declaring an unknown kind", err, aerr.APERTURE_CONFIG_INVALID, "parquet")

	_, err = runWiringDiffCLI(t, writeWiringSeed(t, `
providers:
  - object_type: document
    kind: sql
    connection: nowhere
    get_one: SELECT 1 WHERE id = $1
    get_all: SELECT 'document:1' AS id
`), deployed)
	mustRefuse(t, "a document naming an undeclared connection", err,
		aerr.APERTURE_WIRING_CONNECTION_UNDECLARED, "nowhere")

	_, err = runWiringDiffCLI(t, writeWiringSeed(t, `
field_types:
  - object_type: document
    fields:
      due: timestamp
`), deployed)
	mustRefuse(t, "a document declaring an unknown field type", err,
		aerr.APERTURE_CONFIG_INVALID, "timestamp")

	// A document a push would refuse for a MODEL reason is NOT refused here: a diff
	// against a store with no model state is a legitimate question with a useful
	// answer, and checkWiringAgainstModel's rules are about what may be DEPLOYED.
	bare := newWiringStore(t, "")
	if _, err := runWiringDiffCLI(t, writeWiringSeed(t, wiringSharedSeed), bare); aerr.CodeOf(err) != "" {
		t.Errorf("a diff against a store with no model state refused with %s; that rule belongs to a push: %v",
			aerr.CodeOf(err), err)
	}
}

// TestDiffTakesNoActorAndOnlyTheTwoFlagsItNeeds pins the surface structurally, for
// the reasons `show` and `pull` pin their own: it is ungated because the --store
// credential the operator just supplied already grants full WRITE access to this
// wiring, and it writes nothing, so there is no --out and no --force either.
func TestDiffTakesNoActorAndOnlyTheTwoFlagsItNeeds(t *testing.T) {
	diff := wiringSubcommand(t, "diff")
	got := map[string]bool{}
	for _, f := range diff.Flags {
		for _, n := range f.Names() {
			got[n] = true
		}
	}
	for _, want := range []string{"seed", "store"} {
		if !got[want] {
			t.Errorf("`wiring diff` has no --%s flag; its flags are %v", want, got)
		}
	}
	for _, forbidden := range []string{"principal", "actor", "out", "force"} {
		if got[forbidden] {
			t.Errorf("`wiring diff` declares --%s: it reads two sources and writes nothing, and it is ungated for the same reason `wiring show` is", forbidden)
		}
	}
	// The property that makes it worth having in a pipeline has to be in the help
	// text, or nobody puts it in one.
	for _, want := range []string{"exits 0", "exits 2", "exits 1"} {
		if !strings.Contains(diff.Description, want) {
			t.Errorf("the description does not state %q; the exit codes are what make this command usable in CI", want)
		}
	}
}

// ---- The comparison itself ----

// TestEveryWiringFieldIsComparedAndOnlyTheStampsAreNot is the anti-vacuity gate for
// the reflect-based comparison, and it is the test that stops this command failing by
// PASSING.
//
// A field that is not compared makes a diff report clean over a real difference: the
// deployment and the repository disagree, the gate is green, and nothing anywhere says
// so. So every field of every wiring entry is mutated in turn and the diff is required
// to notice — a claim about the TYPE rather than about a list of field names this test
// maintains, so a field added to model.WiringProvider is covered the moment it exists.
//
// The stamps are the one deliberate exclusion, and they are asserted as such rather
// than skipped: a push restamps the whole set to one instant, so comparing them would
// report drift on every entry of every store forever.
func TestEveryWiringFieldIsComparedAndOnlyTheStampsAreNot(t *testing.T) {
	stamp := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	// keyFields names the fields the section is KEYED on. Changing one of them makes
	// the entry a different entry, which is reported as only-deployed plus only-local
	// rather than as a field difference — field_types: has two, because the section is
	// stored and reported one row per (object type, field) pair.
	probes := []struct {
		section   string
		keyFields []string
		entry     any
		pack      func(any) model.WiringSet
	}{
		{
			section: wiringSectionConnections, keyFields: []string{"Name"},
			entry: model.WiringConnection{Name: "main", CreatedAt: stamp, UpdatedAt: stamp},
			pack: func(v any) model.WiringSet {
				return model.WiringSet{Connections: []model.WiringConnection{v.(model.WiringConnection)}}
			},
		},
		{
			section: wiringSectionProviders, keyFields: []string{"ObjectType"},
			entry: model.WiringProvider{
				ObjectType: "document", Kind: "sql", Connection: "main",
				GetOne: "SELECT 1 WHERE id = $1", GetAll: "SELECT 'document:1' AS id",
				IDColumn: "id", TTL: "5m", MaxSize: 64,
				References: []model.WiringReference{{Field: "project_ids", TargetType: "project"}},
				CreatedAt:  stamp, UpdatedAt: stamp,
			},
			pack: func(v any) model.WiringSet {
				return model.WiringSet{Providers: []model.WiringProvider{v.(model.WiringProvider)}}
			},
		},
		{
			section: wiringSectionFieldTypes, keyFields: []string{"ObjectType", "Field"},
			entry: model.WiringFieldType{
				ObjectType: "document", Field: "due", DeclaredType: "date",
				CreatedAt: stamp, UpdatedAt: stamp,
			},
			pack: func(v any) model.WiringSet {
				return model.WiringSet{FieldTypes: []model.WiringFieldType{v.(model.WiringFieldType)}}
			},
		},
		{
			// DeclaredKeys starts NOT DECLARED, so its mutation runs in the direction that
			// IS drift: the document declares a set the store does not. The reverse —
			// declared in the store, silent in the document — is the one the diff excuses,
			// and TestDiffDoesNotReportADeclaredKeySetTheDocumentCannotExpress owns it.
			section: wiringSectionAttributeProviders, keyFields: []string{"Subject"},
			entry: model.WiringAttributeProvider{
				Subject: "user", Kind: "sql", Connection: "main",
				GetOne:   "SELECT department FROM users WHERE id = $1",
				GetAll:   "SELECT u.id AS id, u.department FROM users u",
				IDColumn: "id", TTL: "30s", MaxSize: 100,
				DeclaredKeys: model.DeclaredKeys{},
				CreatedAt:    stamp, UpdatedAt: stamp,
			},
			pack: func(v any) model.WiringSet {
				return model.WiringSet{AttributeProviders: []model.WiringAttributeProvider{v.(model.WiringAttributeProvider)}}
			},
		},
	}

	for _, probe := range probes {
		typ := reflect.TypeOf(probe.entry)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			t.Run(probe.section+"."+field.Name, func(t *testing.T) {
				mutated, ok := mutateWiringField(probe.entry, i)
				if !ok {
					t.Fatalf("this test has no way to change %s.%s (%s), so it cannot prove the field is compared; "+
						"teach mutateWiringField the new kind rather than leaving the field uncovered",
						typ.Name(), field.Name, field.Type)
				}
				diff := diffWiring(probe.pack(probe.entry), probe.pack(mutated))
				section := wiringDiffSectionNamed(t, diff, probe.section)
				noticed := len(section.onlyDeployed) > 0 || len(section.onlyLocal) > 0 || len(section.differing) > 0

				if field.Type == reflect.TypeOf(time.Time{}) {
					if noticed {
						t.Fatalf("changing %s reported drift; a stamp is a fact about the LAST PUSH and not about the "+
							"wiring, so comparing it reports drift on every entry of every store forever", field.Name)
					}
					return
				}
				if !noticed {
					t.Fatalf("changing %s reported NO drift: the field is not compared, so a deployment and a repository "+
						"that disagree about it produce a clean report and a green gate", field.Name)
				}
				if containsString(probe.keyFields, field.Name) {
					// The section's key: the entry is a DIFFERENT entry, so it is reported as
					// only-deployed and only-local rather than as a field difference.
					if len(section.onlyDeployed) != 1 || len(section.onlyLocal) != 1 {
						t.Fatalf("changing the section key %s reported %+v; want one only-deployed and one only-local entry",
							field.Name, section)
					}
					return
				}
				want := wiringFieldLabel(field.Name)
				if len(section.differing) != 1 || !containsString(section.differing[0].fields, want) {
					t.Fatalf("changing %s reported %+v; the report must name the field as %q, which is how the document spells it",
						field.Name, section.differing, want)
				}
			})
		}
	}
}

// mutateWiringField returns a copy of entry with field i changed to a different
// value, or false when it does not know how.
//
// Returning false rather than skipping is deliberate: the caller FAILS on it, so a
// new field of a kind this helper cannot vary is a red test rather than a field
// quietly dropped from the coverage claim above.
func mutateWiringField(entry any, i int) (any, bool) {
	out := reflect.New(reflect.TypeOf(entry)).Elem()
	out.Set(reflect.ValueOf(entry))
	f := out.Field(i)

	switch v := f.Interface().(type) {
	case string:
		f.SetString(v + "-changed")
		return out.Interface(), true
	case int:
		f.SetInt(int64(v) + 1)
		return out.Interface(), true
	case time.Time:
		f.Set(reflect.ValueOf(v.Add(time.Hour)))
		return out.Interface(), true
	case []model.WiringReference:
		refs := append([]model.WiringReference{}, v...)
		refs = append(refs, model.WiringReference{Field: "added_ids", TargetType: "document"})
		f.Set(reflect.ValueOf(refs))
		return out.Interface(), true
	case model.DeclaredKeys:
		if v.Declared {
			f.Set(reflect.ValueOf(model.DeclaredKeys{}))
		} else {
			f.Set(reflect.ValueOf(model.DeclaredKeys{Declared: true, Keys: []string{"clearance"}}))
		}
		return out.Interface(), true
	default:
		return nil, false
	}
}

// wiringDiffSectionNamed finds one section of a diff.
func wiringDiffSectionNamed(t *testing.T, diff wiringDiff, name string) wiringDiffSection {
	t.Helper()
	for _, s := range diff.sections {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("the diff has no %q section; its sections are %+v", name, diff.sections)
	return wiringDiffSection{}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestAFieldLabelIsSpelledTheWayTheDocumentSpellsIt: the report names a field the way
// the operator's --seed does, or the row it prints is not one they can act on. The
// labels are DERIVED rather than tabulated — a table is a second list of field names
// to forget — so this pins the derivation on the spellings that actually occur.
func TestAFieldLabelIsSpelledTheWayTheDocumentSpellsIt(t *testing.T) {
	for name, want := range map[string]string{
		"ObjectType":   "object_type",
		"Kind":         "kind",
		"Connection":   "connection",
		"GetOne":       "get_one",
		"GetAll":       "get_all",
		"IDColumn":     "id_column",
		"TTL":          "ttl",
		"MaxSize":      "max_size",
		"References":   "references",
		"Field":        "field",
		"DeclaredType": "declared_type",
		"Subject":      "subject",
		"DeclaredKeys": "declared_keys",
		"Name":         "name",
	} {
		if got := wiringFieldLabel(name); got != want {
			t.Errorf("wiringFieldLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestTheDiffComparesWiringSetsAndNotRenderedDocuments pins the seam E5-S1 named.
//
// wiringToDocument is the ONE projection in the store->document direction, and a diff
// must not grow a second: two notions of "the same wiring" are two answers to one
// question, and the wrong one is whichever the operator did not run. The assertion is
// on the SOURCE, because the hazard is a future edit that finds rendering both sides
// and comparing the bytes easier than comparing the values — it would pass every
// behavioural test above and start reporting a reordered map, a quoting change, or
// the empty dsn_env: a pulled document carries as drift.
//
// It parses the file with go/ast rather than grepping it, so the prose in this file
// may keep explaining the hazard by name.
func TestTheDiffComparesWiringSetsAndNotRenderedDocuments(t *testing.T) {
	const path = "wiring_diff.go"
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v — this gate fails rather than skips if the file moves", path, err)
	}

	called := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.Ident:
			called[fn.Name] = true
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok {
				called[pkg.Name+"."+fn.Sel.Name] = true
			}
		}
		return true
	})

	for _, forbidden := range []string{"wiringToDocument", "seed.MarshalWiring", "seed.Marshal"} {
		if called[forbidden] {
			t.Errorf("%s calls %s: a diff compares model.WiringSet VALUES — the store's from GetWiring and the "+
				"document's through wiringFromDocument — so formatting, key order and the dsn_env: asymmetry of a "+
				"rendered document cannot read as drift", path, forbidden)
		}
	}
	for _, required := range []string{"wiringFromDocument", "pullWiringSnapshot"} {
		if !called[required] {
			t.Errorf("%s does not use %s; both sides of the comparison must come through the projection and the read "+
				"the other wiring commands already use, or this command is a second answer to the same question",
				path, required)
		}
	}
}

// ---- The gated live-Postgres proof ----
//
// These cases join E5-S1's live suite rather than opening a second gate: same
// variables, same command, so one exported DSN still drives every live suite in one
// shell.
//
//	APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/
//
// SQLite is a real database and every case above is real proof against it, but a diff
// is a claim about the STORE: the canonical order that keeps formatting from reading
// as drift is an ORDER BY the two backends spell differently (Postgres needs
// COLLATE "C"), and GetWiring reads all four sections inside a transaction only on
// the backends that have one. A diff that is clean on one dialect and not the other
// is exactly the drift the dialect-parity gates exist for, and only a server settles
// it.

// TestPostgresLiveWiringDiffIsCleanAgainstThePushedDocument: the same push-then-diff
// that is clean on SQLite must be clean on a real server, in its own schema.
func TestPostgresLiveWiringDiffIsCleanAgainstThePushedDocument(t *testing.T) {
	live := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := livePostgresWiringStore(t, ctx, live)
	seedPath := writeWiringSeed(t, wiringSharedSeed)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	out, err := runWiringDiffCLI(t, seedPath, dsn)
	if code, isExit := wiringDiffExit(t, err); code != 0 || isExit {
		t.Fatalf("a live Postgres deployment reports drift against the document it was pushed from (exit %d, ExitCoder=%v): %v\n%s",
			code, isExit, err, out)
	}
}

// TestPostgresLiveWiringDiffReportsTheSameDriftAsSQLite is the parity half, and it is
// the one the two-dialect schema gates cannot reach: they prove the two schemas
// DESCRIBE the same database, not that a comparison against one produces the same
// report as a comparison against the other. Ordering a section differently, or
// dropping a field on one backend only, would pass every parity gate and make this
// command report different drift on two identically-wired deployments — which is the
// exact failure the command exists to detect.
func TestPostgresLiveWiringDiffReportsTheSameDriftAsSQLite(t *testing.T) {
	live := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	drifted := writeWiringSeed(t, wiringDiffedSeed)
	reportFrom := func(dsn, name string) (string, error) {
		t.Helper()
		if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
			t.Fatalf("push to %s: %v\n%s", name, err, out)
		}
		return runWiringDiffCLI(t, drifted, dsn)
	}

	// The SQLite store first: t.Setenv inside livePostgresWiringStore applies to the
	// whole test, and only the Postgres backend reads it.
	fromSQLite, sqliteErr := reportFrom(newWiringStore(t, wiringModelSeed), "sqlite")
	fromPostgres, pgErr := reportFrom(livePostgresWiringStore(t, ctx, live), "postgres")

	sqliteCode, _ := wiringDiffExit(t, sqliteErr)
	pgCode, _ := wiringDiffExit(t, pgErr)
	if sqliteCode != pgCode {
		t.Errorf("the two backends exit differently for the same drift: sqlite %d, postgres %d", sqliteCode, pgCode)
	}
	if fromSQLite != fromPostgres {
		t.Errorf("the two backends report the same drift differently:\n--- sqlite ---\n%s\n--- postgres ---\n%s",
			fromSQLite, fromPostgres)
	}
}
