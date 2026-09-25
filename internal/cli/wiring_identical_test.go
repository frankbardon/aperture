package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"

	ucli "github.com/urfave/cli/v3"
)

// E2-S5: the effort's PREMISE, asserted rather than assumed.
//
// Two instances share one store. One of them was pushed from — and still holds —
// the authoring seed file with all four shared wiring sections in it; the other has
// only the two file-local sections (`objects:` and `attributes:`) and reads its
// providers, field types and attribute slots out of the database. For the same
// question they must return the same `Check`, the same `Enumerate`, the same
// `Search` and the same `Explain`. If they do not, a fleet answers one question two
// ways and nothing anywhere says so: the divergence is a different VERDICT, not an
// error, on two instances that look identically configured.
//
// # One store, both stacks, in the operator's own order
//
// The wiring rows are authoritative whenever they are present, so the two stacks
// cannot coexist on one store at one instant — an instance with rows in front of it
// reads them, whatever its file says. The test therefore follows the real timeline
// instead of faking two databases: model state applied ONCE, the file-wired stack
// built and interrogated while the wiring tables are still empty, the wiring PUSHED,
// and the DB-wired stack built and interrogated against the same open store. That is
// exactly what an operator does, and it is the literal reading of "one store".
//
// The pushed set is not a hand-written model.WiringSet either. It is
// wiringFromDocument's projection of the SAME file the first stack was built from
// (pushWiringFromDocument below runs the three steps `aperture wiring push` runs), so
// "the equivalent seed file" is equivalent by construction rather than by a fixture
// two people have to keep in step.
//
// # What each fixture would catch — because a fixture set that catches nothing is
// worse than no test, since it reads as proof
//
//   - perm-doc-read / rule `public-documents` reads OBJECT METADATA
//     (`object.tags has "public"`). The metadata comes from the local `objects:`
//     section, which the projection has to carry through UNTOUCHED: wiringDocument
//     replaces the four shared sections and keeps the two local ones, and a
//     projection that dropped `objects:` would leave the DB-wired stack reading empty
//     metadata — so document:42 would flip from ALLOW to DENY. It is also the
//     positive control for the whole file: an all-deny build fails it.
//   - perm-doc-list / rule `engineering` reads ATTRIBUTES, both roots
//     (`principal.department == "eng" && account.plan == "enterprise"`). The bags come
//     from the local `attributes:` section, the other thing the projection must carry.
//     A dropped `attributes:` block is the nastiest of the three failures, because a
//     missing bag does not deny — it WIDENS an exclusive grant — and this rule is
//     inclusive precisely so the symptom is visible either way: alice ALLOWS and bob
//     DENIES, so a stack that lost the bags fails on alice and a stack that invented
//     them fails on bob.
//   - perm-doc-review / rule `reviewed-at-the-known-instant` is the one fixture that
//     reads a SHARED section. `reviewed_at` is written with a sub-second component and
//     the shared `field_types:` entry declares it `datetime`, so the loader
//     canonicalises it and drops the nanoseconds; the rule compares against the
//     canonical spelling. Drop `field_types:` from the projection and the raw
//     "…07.500Z" survives, the comparison is false, and the verdict flips. It is the
//     only decision-observable consequence a field-type declaration has — a bare
//     string that looks like a date still parses as one on the comparison path — so it
//     is the fixture that makes the shared half of the projection load-bearing here.
//   - The shared `providers:` and `attribute_providers:` entries are `kind: sql` over
//     a connection that is deliberately never dialled, so no decision can read
//     through them without a host database this suite does not own. They are asserted
//     STRUCTURALLY instead (assertSameWiringShape): the same object types served, the
//     same attribute slots registered, and the same connection manifest routed. That
//     is honest about what it proves — the entry projected and built — and it is what
//     `seed/postgres_integration_test.go` already proves the reading half of.
//   - `Search` ⊆ `Enumerate` is re-asserted on BOTH stacks, per fixture. It is the
//     property four other packages assert and the one every other guarantee rests on,
//     and "both stacks agree" would be satisfied by two stacks that were identically
//     wrong.
//
// # Where it runs
//
// Under `make test` against the in-memory backend and against SQLite, and under the
// gated live run against a real PostgreSQL server (see live_gate_test.go). The
// backends are not interchangeable here: the wiring makes a round trip through the
// store between the two stacks, so this is a claim about a STORE, and `make test`
// can only prove that the two dialects have not drifted apart — never that the
// Postgres one behaves.

// identicalDecisionModel is the MODEL STATE both instances share. It is applied once,
// by whoever provisions the store, and neither instance's wiring can change it.
//
// Three permissions, three rule-backed inclusive scopes, one per axis the fixture set
// has to cover. Two principals, so every attribute-reading case has a negative twin
// that only passes if the rule actually ran.
const identicalDecisionModel = `
accounts:
  - {id: acme, name: Acme Corp}
memberships:
  - {principal: alice, account: acme}
  - {principal: bob, account: acme}
object_types:
  - name: document
    description: A protected document.
    actions: [read, list, review]
  - name: project
    description: The type only the shared wiring serves.
    actions: [read]
permissions:
  - id: perm-doc-read
    object_type: document
    action: read
    scope_strategy: "inclusive;rule=public-documents"
    description: Read a document the public-documents rule selects.
  - id: perm-doc-list
    object_type: document
    action: list
    scope_strategy: "inclusive;rule=engineering"
    description: List documents when the asker is in engineering at an enterprise tenant.
  - id: perm-doc-review
    object_type: document
    action: review
    scope_strategy: "inclusive;rule=reviewed-at-the-known-instant"
    description: Review a document reviewed at the canonical instant.
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice, roles: [viewer]}
  - {id: bob, kind: user, identity: "user:bob", display_name: Bob, roles: [viewer]}
roles:
  - id: viewer
    name: Viewer
    description: May read, list and review documents, subject to the rules.
    permissions: [perm-doc-read, perm-doc-list, perm-doc-review]
grants:
  - id: g-viewer-read
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-read
    object: "account:acme/**"
    effect: allow
  - id: g-viewer-list
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-list
    object: "account:acme/**"
    effect: allow
  - id: g-viewer-review
    account: acme
    subject: {kind: role, id: viewer}
    permission: perm-doc-review
    object: "account:acme/**"
    effect: allow
rules:
  - name: public-documents
    description: A document is public when its tags carry "public".
    ast:
      type: compare
      op: has
      left: {type: var, name: object.tags}
      right: {type: literal, value: "public"}
  - name: engineering
    description: The asker is in engineering and the tenant is on the enterprise plan.
    ast:
      type: and
      children:
        - type: compare
          op: eq
          left: {type: var, name: principal.department}
          right: {type: literal, value: "eng"}
        - type: compare
          op: eq
          left: {type: var, name: account.plan}
          right: {type: literal, value: "enterprise"}
  - name: reviewed-at-the-known-instant
    description: The document's reviewed_at is the canonical spelling of the known instant.
    ast:
      type: compare
      op: eq
      left: {type: var, name: object.reviewed_at}
      right: {type: literal, value: "2026-03-04T05:06:07Z"}
`

// identicalDecisionLocalWiring is what BOTH instances keep in their own seed file:
// the two sections that carry DATA rather than a pointer to data, and which therefore
// belong to the machine whose file lists them.
//
// reviewed_at carries a sub-second component on purpose. It is what the shared
// `field_types:` declaration canonicalises away, and the rule compares against the
// canonical spelling — so this value is the probe for the shared half of the
// projection. See the file header.
const identicalDecisionLocalWiring = `
objects:
  - id: "account:acme/document:42"
    metadata:
      tags: [public]
      title: Atlas Quarterly Report
      reviewed_at: "2026-03-04T05:06:07.500Z"
  - id: "account:acme/document:99"
    metadata:
      tags: [internal]
      title: Atlas Internal Memo
      reviewed_at: "2026-03-04T05:06:07.500Z"
attributes:
  - subject: user
    id: alice
    metadata: {department: eng}
  - subject: user
    id: bob
    metadata: {department: sales}
  - subject: account
    id: acme
    metadata: {plan: enterprise}
`

// identicalDecisionSharedWiring is the four SHARED sections — the ones
// `aperture wiring push` writes and a second instance reads back out of the database.
//
// The connection's dsn_env: is the CONVENTIONAL variable name on purpose. A DB-wired
// boot derives APERTURE_CONNECTION_MAIN_DSN for a manifest name nothing local routes,
// and naming the same variable in the file means one t.Setenv routes both stacks —
// the two are then reading the same connection through the two different routes the
// design offers, which is the comparison this test is for.
const identicalDecisionSharedWiring = `
connections:
  main:
    dsn_env: APERTURE_CONNECTION_MAIN_DSN
providers:
  - object_type: project
    kind: sql
    connection: main
    get_one: SELECT tier FROM projects WHERE id = $1
    get_all: SELECT 'project:' || p.id AS id, p.tier FROM projects p
field_types:
  - object_type: document
    fields:
      reviewed_at: datetime
attribute_providers:
  - subject: machine
    kind: sql
    connection: main
    get_one: SELECT department FROM machines WHERE id = $1
`

// identicalDecisionFixture is one question asked of both stacks, with the answer the
// model implies pinned beside it.
//
// The pinned answers are what keep the comparison from being vacuous: two stacks that
// both deny everything agree perfectly, and a build with no rule evaluator wired does
// exactly that. So every fixture states the verdict, the enumeration and the ranked
// search it must produce, and the harness checks those on EACH stack before it
// compares the two.
type identicalDecisionFixture struct {
	// name says which axis the case is about.
	name string
	// principal, action and object are the Check question; pattern bounds the
	// Enumerate and the Search.
	principal string
	action    string
	object    string
	pattern   string
	// query is the Search text. Empty skips the Search half (Search rejects an
	// empty query outright, by design).
	query string
	// wantAllow is the verdict Check must return.
	wantAllow bool
	// wantEnumerate is the exact, sorted set Enumerate must return.
	wantEnumerate []string
	// wantSearch is the exact, RANKED set Search must return, best first.
	wantSearch []string
}

// identicalDecisionFixtures is the set. Read the file header for what each one would
// catch; the axis is named in each case's own name.
func identicalDecisionFixtures() []identicalDecisionFixture {
	const pattern = "account:acme/document:*"
	return []identicalDecisionFixture{
		{
			name:          "a metadata-reading rule selects the object",
			principal:     "alice",
			action:        "read",
			object:        "account:acme/document:42",
			pattern:       pattern,
			query:         "atlas quarterly",
			wantAllow:     true,
			wantEnumerate: []string{"account:acme/document:42"},
			wantSearch:    []string{"account:acme/document:42"},
		},
		{
			name:          "a metadata-reading rule does not select the object",
			principal:     "alice",
			action:        "read",
			object:        "account:acme/document:99",
			pattern:       pattern,
			query:         "internal memo",
			wantAllow:     false,
			wantEnumerate: []string{"account:acme/document:42"},
			// document:99 matches the text and is NOT allowed, so the ranked set is
			// empty: Search subtracts from Enumerate and never adds to it.
			wantSearch: nil,
		},
		{
			name:      "an attribute-reading rule reads both roots and allows",
			principal: "alice",
			action:    "list",
			object:    "account:acme/document:42",
			pattern:   pattern,
			query:     "atlas",
			wantAllow: true,
			// The rule reads nothing off the object, so every document in the pattern
			// is selected — which is what makes the bob case below meaningful.
			wantEnumerate: []string{"account:acme/document:42", "account:acme/document:99"},
			wantSearch:    []string{"account:acme/document:42", "account:acme/document:99"},
		},
		{
			name:          "an attribute-reading rule reads both roots and denies",
			principal:     "bob",
			action:        "list",
			object:        "account:acme/document:42",
			pattern:       pattern,
			query:         "atlas",
			wantAllow:     false,
			wantEnumerate: nil,
			wantSearch:    nil,
		},
		{
			name:      "a rule over a field the shared field_types: section canonicalised",
			principal: "alice",
			action:    "review",
			object:    "account:acme/document:42",
			pattern:   pattern,
			query:     "atlas quarterly",
			wantAllow: true,
			// Both documents carry the same reviewed_at, so both are selected: the
			// case is about the VALUE surviving the projection, not about which
			// document it is on. Both titles carry "Atlas", so both rank — and the
			// ORDER is the assertion: the exact phrase match comes first.
			wantEnumerate: []string{"account:acme/document:42", "account:acme/document:99"},
			wantSearch:    []string{"account:acme/document:42", "account:acme/document:99"},
		},
	}
}

// decisionSnapshot is everything one stack answered, keyed by fixture name, in a form
// two stacks can be diffed byte for byte.
//
// Explain is captured as Trace.String() because that rendering promises to be
// byte-identical for the same decision — it deliberately omits the reference instant
// for exactly that reason — which makes it the strongest single comparison available
// here: it carries the subject set, every grant considered with its strategy and
// specificity, the rule notes, and both attribute bags with their values.
type decisionSnapshot struct {
	check     map[string]string
	enumerate map[string]string
	search    map[string]string
	explain   map[string]string
}

// checkLine renders a Decision for comparison: the verdict, the deciding grants and
// the reason. The reason is included on purpose — two stacks that allowed for
// different reasons have not agreed.
func checkLine(d engine.Decision, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("allow=%t grants=%v reason=%s", d.Allow, d.DecidingGrantIDs, d.Reason)
}

// searchLine renders a ranked result set. The score is included at three decimals: a
// scorer change must not fail this test, but two stacks that ranked the same objects
// differently have not agreed.
func searchLine(matches []engine.SearchResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		parts = append(parts, fmt.Sprintf("%s@%.3f(%s)", m.Object, m.Score, m.Field))
	}
	return strings.Join(parts, " ")
}

// snapshotDecisions asks every fixture of one stack, checks each pinned answer, and
// asserts Search ⊆ Enumerate on this stack before anything is compared with the other.
func snapshotDecisions(t *testing.T, ctx context.Context, which string, stack decisionStack, fixtures []identicalDecisionFixture) decisionSnapshot {
	t.Helper()
	snap := decisionSnapshot{
		check:     map[string]string{},
		enumerate: map[string]string{},
		search:    map[string]string{},
		explain:   map[string]string{},
	}
	for _, f := range fixtures {
		req := engine.Request{Account: "acme", Principal: f.principal, Action: f.action, Object: f.object}

		dec, err := stack.eng.Check(ctx, req)
		snap.check[f.name] = checkLine(dec, err)
		if err != nil {
			t.Fatalf("%s / %s: Check: %v", which, f.name, err)
		}
		if dec.Allow != f.wantAllow {
			t.Errorf("%s / %s: Check allow = %t, want %t — the pinned verdict is what stops "+
				"this file passing on two stacks that are identically wrong (reason: %s)",
				which, f.name, dec.Allow, f.wantAllow, dec.Reason)
		}

		ids, err := stack.eng.Enumerate(ctx, engine.EnumerateRequest{
			Account: "acme", Principal: f.principal, Action: f.action, Pattern: f.pattern,
		})
		sort.Strings(ids)
		snap.enumerate[f.name] = strings.Join(ids, " ")
		if err != nil {
			t.Fatalf("%s / %s: Enumerate: %v", which, f.name, err)
		}
		if got, want := strings.Join(ids, " "), strings.Join(f.wantEnumerate, " "); got != want {
			t.Errorf("%s / %s: Enumerate = %q, want %q", which, f.name, got, want)
		}

		if f.query != "" {
			matches, err := stack.eng.Search(ctx, engine.SearchRequest{
				Account: "acme", Principal: f.principal, Action: f.action,
				Pattern: f.pattern, Query: f.query,
			})
			snap.search[f.name] = searchLine(matches, err)
			if err != nil {
				t.Fatalf("%s / %s: Search: %v", which, f.name, err)
			}
			found := make([]string, 0, len(matches))
			for _, m := range matches {
				found = append(found, m.Object)
			}
			if got, want := strings.Join(found, " "), strings.Join(f.wantSearch, " "); got != want {
				t.Errorf("%s / %s: Search = %q, want %q (ranked, best first)", which, f.name, got, want)
			}
			// Search ⊆ Enumerate, on THIS stack. Four other packages assert it; it is
			// re-asserted here because "the two stacks agree" is also true of two
			// stacks that both disclose more than the subject may see.
			allowed := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				allowed[id] = struct{}{}
			}
			for _, m := range matches {
				if _, ok := allowed[m.Object]; !ok {
					t.Errorf("%s / %s: Search returned %q, which Enumerate did not — a ranked "+
						"search may only ever SUBTRACT from the entitled set", which, f.name, m.Object)
				}
			}
		}

		tr, err := stack.eng.Explain(ctx, req)
		if err != nil {
			t.Fatalf("%s / %s: Explain: %v", which, f.name, err)
		}
		snap.explain[f.name] = tr.String()
		if tr.Decision.Allow != dec.Allow {
			t.Errorf("%s / %s: Explain and Check disagree on one stack (%t vs %t)",
				which, f.name, tr.Decision.Allow, dec.Allow)
		}
	}
	return snap
}

// assertSameSnapshots diffs the two stacks, one surface and one fixture at a time, so
// a failure names the decision that diverged rather than dumping four maps.
func assertSameSnapshots(t *testing.T, a, b decisionSnapshot, fixtures []identicalDecisionFixture) {
	t.Helper()
	for _, surface := range []struct {
		name string
		a, b map[string]string
	}{
		{"Check", a.check, b.check},
		{"Enumerate", a.enumerate, b.enumerate},
		{"Search", a.search, b.search},
		{"Explain", a.explain, b.explain},
	} {
		for _, f := range fixtures {
			if surface.a[f.name] == surface.b[f.name] {
				continue
			}
			t.Errorf("%s diverges on %q between an instance wired from its FILE and one wired "+
				"from the DATABASE:\n  file: %s\n    db: %s\n"+
				"Two instances of one deployment must not answer one question two ways; the "+
				"divergence is a verdict, not an error, so nothing downstream would report it.",
				surface.name, f.name, surface.a[f.name], surface.b[f.name])
		}
	}
}

// assertSameWiringShape compares what the two stacks actually WIRED, for the sections
// no decision in this file can read through.
//
// The shared `providers:` and `attribute_providers:` entries are `kind: sql` over a
// connection nothing dials, so a decision cannot observe them; this is the honest
// assertion for them — the entry projected out of the row, reached the same builder,
// and produced the same registration. The reading half is `seed`'s own live suite.
func assertSameWiringShape(t *testing.T, fileWired, dbWired decisionStack) {
	t.Helper()
	if got, want := strings.Join(sortedCopy(dbWired.registry.Keys()), " "), strings.Join(sortedCopy(fileWired.registry.Keys()), " "); got != want {
		t.Errorf("the two stacks serve different object types (db %q vs file %q); the shared "+
			"providers: section did not project to the same registry", got, want)
	}
	slotWords := func(slots []provider.AttributeSlot) string {
		out := make([]string, 0, len(slots))
		for _, s := range slots {
			out = append(out, s.String())
		}
		return strings.Join(sortedCopy(out), " ")
	}
	if got, want := slotWords(dbWired.attributes.RegisteredSlots()), slotWords(fileWired.attributes.RegisteredSlots()); got != want {
		t.Errorf("the two stacks register different attribute slots (db %q vs file %q); the "+
			"shared attribute_providers: section did not project to the same registry", got, want)
	}
	// Anti-vacuity: both of the above are also true of two empty registries.
	if !dbWired.registry.Has("project") || !dbWired.registry.Has("document") {
		t.Errorf("the DB-wired registry serves %v, and this test is not about the wiring it "+
			"thinks it is: `project` comes from the shared providers: row and `document` from "+
			"the local objects: section, and both have to be there", dbWired.registry.Keys())
	}
	if !dbWired.attributes.Has(provider.AttributeSlotMachine) || !dbWired.attributes.Has(provider.AttributeSlotUser) {
		t.Errorf("the DB-wired attribute registry wires %v; `machine` comes from the shared row "+
			"and `user` from the local attributes: block", dbWired.attributes.RegisteredSlots())
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// stackOverStore builds a decision stack over an ALREADY-OPEN store, the way a real
// command does: through a command whose --store and --seed flags were actually
// PARSED, because buildDecisionStack reads --store to answer loadSeed's own question
// (does an absent --seed mean the embedded demo, or nothing?).
//
// It takes the store rather than a DSN because this file's whole point is that both
// stacks decide over ONE store: opening it twice would be two stores, and for the
// in-memory backend it would be two DIFFERENT stores with no wiring in the second.
func stackOverStore(t *testing.T, ctx context.Context, store model.Storage, storeDSN, seedPath string) decisionStack {
	t.Helper()
	var stack decisionStack
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: storeFlags(),
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			var err error
			stack, err = buildDecisionStack(ctx, cmd, store, cmd.String("seed"))
			return err
		},
	}
	args := []string{"probe"}
	if storeDSN != "" {
		args = append(args, "--store", storeDSN)
	}
	if seedPath != "" {
		args = append(args, "--seed", seedPath)
	}
	if err := cmd.Run(ctx, args); err != nil {
		t.Fatalf("buildDecisionStack(--store %q --seed %q): %v", storeDSN, seedPath, err)
	}
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

// pushWiringFromDocument performs `aperture wiring push` against an already-open
// store: parse the document, project it, check it against the model, replace the
// wiring. Those are runWiringPush's own four steps in its own order, called directly
// rather than through the command tree because the command opens the store itself by
// DSN — which the in-memory backend has none of, and which would be a second store
// even for the two that do.
//
// Using the real projection is the point. A hand-written model.WiringSet would be a
// second author's idea of what the file means, and the two could drift into exactly
// the equivalent-but-different wiring this whole test exists to rule out.
func pushWiringFromDocument(t *testing.T, ctx context.Context, store model.Storage, seedPath string) {
	t.Helper()
	doc, err := seed.ParseFile(seedPath)
	if err != nil {
		t.Fatalf("parse the wiring document: %v", err)
	}
	set, err := wiringFromDocument(doc, time.Now().UTC())
	if err != nil {
		t.Fatalf("project the wiring document: %v", err)
	}
	if err := checkWiringAgainstModel(ctx, store, set); err != nil {
		t.Fatalf("the pushed wiring does not match the model this store holds: %v", err)
	}
	if err := store.ReplaceWiring(ctx, set); err != nil {
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if set.IsEmpty() {
		t.Fatal("the pushed wiring set is empty, so the second stack would read the local " +
			"file and this test would compare one stack with itself")
	}
}

// assertTwoInstancesDecideIdentically is the whole proof, over one store.
func assertTwoInstancesDecideIdentically(t *testing.T, ctx context.Context, store model.Storage, storeDSN string) {
	t.Helper()
	// One route, both stacks: the file names the conventional variable and a DB-wired
	// boot derives it, so this single export is the "each instance supplies its own
	// connection route" half of the topology. It is never dialled — sql.Open is lazy
	// and pgx's stdlib driver only parses the string.
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	dir := t.TempDir()
	// The authoring file, and the second instance's file, are the SAME text minus the
	// four shared sections — which is what "the equivalent seed file" has to mean.
	authoring := filepath.Join(dir, "authoring.yaml")
	if err := os.WriteFile(authoring, []byte(identicalDecisionModel+identicalDecisionLocalWiring+identicalDecisionSharedWiring), 0o600); err != nil {
		t.Fatalf("write the authoring seed: %v", err)
	}
	// The second instance is RBAC-shaped: no model state, no shared wiring, only the
	// two sections that are its own.
	localOnly := filepath.Join(dir, "local-only.yaml")
	if err := os.WriteFile(localOnly, []byte(identicalDecisionLocalWiring), 0o600); err != nil {
		t.Fatalf("write the local-only seed: %v", err)
	}

	if err := seed.Load(ctx, store, []byte(identicalDecisionModel), seed.FormatYAML); err != nil {
		t.Fatalf("apply model state: %v", err)
	}

	fixtures := identicalDecisionFixtures()

	// FIRST, with the wiring tables still empty: the instance whose file is the wiring.
	// Every deployment that has never been pushed to is in this state, so it is also
	// the control the comparison is against.
	fileWired := stackOverStore(t, ctx, store, storeDSN, authoring)
	fileSnap := snapshotDecisions(t, ctx, "file-wired", fileWired, fixtures)

	// THEN the push, from that same file.
	pushWiringFromDocument(t, ctx, store, authoring)

	// AND the second instance, over the same store, reading its wiring out of it.
	dbWired := stackOverStore(t, ctx, store, storeDSN, localOnly)
	dbSnap := snapshotDecisions(t, ctx, "db-wired", dbWired, fixtures)

	assertSameSnapshots(t, fileSnap, dbSnap, fixtures)
	assertSameWiringShape(t, fileWired, dbWired)
}

// TestTwoInstancesAgainstOneMemoryStoreDecideIdentically runs the proof on the
// in-memory backend. It is the fastest of the three and it proves the wiring round
// trip through model.Storage rather than through SQL — the memory backend is a full
// implementor of the wiring surface, so a projection bug shows here with no database
// in the way.
func TestTwoInstancesAgainstOneMemoryStoreDecideIdentically(t *testing.T) {
	ctx := context.Background()
	// An empty --store is the in-memory backend. The seed path is empty here so
	// buildStore does NOT apply the embedded acme example over this test's own model;
	// the model goes in through seed.Load below, once.
	store, err := buildStore(ctx, "", "")
	if err != nil {
		t.Fatalf("open the in-memory store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertTwoInstancesDecideIdentically(t, ctx, store, "")
}

// TestTwoInstancesAgainstOneSQLiteStoreDecideIdentically runs the proof against a
// real database: the wiring is written to the five tables, read back out, and
// projected, so every column's encoding is on the path.
func TestTwoInstancesAgainstOneSQLiteStoreDecideIdentically(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "identical.db")
	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the SQLite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertTwoInstancesDecideIdentically(t, ctx, store, dsn)
}

// ---- The gated live-Postgres proof ----
//
// See live_gate_test.go for the gate contract and the scratch-schema helper:
//
//	APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/

// TestPostgresLiveTwoInstancesAgainstOneStoreDecideIdentically is the acceptance
// scenario on the backend the deployment it describes actually uses.
//
// `make test` cannot reach this claim. The wiring makes a round trip through the
// store between the two stacks, and the dialect-parity gates prove only that the two
// schemas DESCRIBE the same database — not that a write-then-read through the
// Postgres one yields a document that builds the same registries. A column that came
// back trimmed, reordered or type-coerced on one backend only would pass every parity
// gate and change what a decision reads on exactly the instance that has no seed
// file to check.
func TestPostgresLiveTwoInstancesAgainstOneStoreDecideIdentically(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	liveScratchSchema(t, ctx, dsn, "aperture_cli_identical")

	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertTwoInstancesDecideIdentically(t, ctx, store, dsn)
}

// TestPostgresLiveADisjointDatabaseWiredBootBuildsItsProvidersAndSlots is the probe
// E2-S2 ran and could not keep: at the time this package had no gated live surface to
// put it in.
//
// The disjoint case is Arc's permanent situation — hand-written Go providers and a
// machine-local kind: csv entry beside a pushed wiring — and it is the reason the
// local layer is ADDITIVE rather than replaced. Against a real server because the
// claim is that the ROW survives the round trip: the kind, the connection name and
// the statements are stored verbatim so the vocabulary question is answered at the
// boot, and a backend that normalised any of them would turn a working wiring into a
// refusal, or worse, the other way round.
func TestPostgresLiveADisjointDatabaseWiredBootBuildsItsProvidersAndSlots(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// livePostgresWiringStore creates the scratch schema, runs Setup and applies
	// wiringModelSeed, which declares the object types the provider rows point at.
	dsn = livePostgresWiringStore(t, ctx, dsn)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	if err := store.ReplaceWiring(ctx, sharedWiringSet(time.Now().UTC())); err != nil {
		_ = store.Close()
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after provisioning: %v", err)
	}

	stack := bootStack(t, dsn, writeLocalAddition(t, localAdditionSeed))

	if !stack.registry.Has("document") {
		t.Errorf("the registry serves %v and not the DATABASE-declared type; the shared "+
			"wiring is authoritative and must still be built", stack.registry.Keys())
	}
	if !stack.registry.Has("project") {
		t.Fatalf("the registry serves %v and not the type only the LOCAL file declares; a "+
			"kind: csv provider cannot be shared wiring at all, so dropping the local "+
			"providers: section makes a push a silent loss of every csv-backed type",
			stack.registry.Keys())
	}
	for _, want := range []provider.AttributeSlot{provider.AttributeSlotUser, provider.AttributeSlotAccount} {
		if !stack.attributes.Has(want) {
			t.Errorf("the attribute registry wires %v, and not %q",
				stack.attributes.RegisteredSlots(), want)
		}
	}
}

// TestPostgresLiveACollidingLocalProviderRefusesTheBoot is the other half of E2-S2's
// probe: the refusal, against a real server, naming the entry and the section.
//
// Both collision axes are in one test because they are one rule with two arbiters —
// wiringDocument refuses a local `providers:` entry for a shared object type, and
// seed.StrictProviderCollision() refuses a local inline `objects:` entry for one —
// and a live run that covered only the first would leave the axis where a silent
// discard is worst untested on the backend the deployment uses.
func TestPostgresLiveACollidingLocalProviderRefusesTheBoot(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	live := livePostgresWiringStore(t, ctx, dsn)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	now := time.Now().UTC()
	// The shared set declares BOTH types: `document` for the providers: axis and
	// `project` for the inline objects: axis (bootWiringSeed declares project inline).
	set := sharedWiringSet(now)
	set.Providers = append(set.Providers, model.WiringProvider{
		ObjectType: "project", Kind: "sql", Connection: "main",
		GetOne:    "SELECT tier FROM projects WHERE id = $1",
		GetAll:    "SELECT 'project:' || p.id AS id, p.tier FROM projects p",
		CreatedAt: now, UpdatedAt: now,
	})
	model.SortWiringProviders(set.Providers)

	store, err := buildStore(ctx, live, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	if err := store.ReplaceWiring(ctx, set); err != nil {
		_ = store.Close()
		t.Fatalf("ReplaceWiring: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close after provisioning: %v", err)
	}

	t.Run("a local providers: entry for a shared object type", func(t *testing.T) {
		body := strings.Replace(localAdditionSeed,
			"  - object_type: project\n    kind: csv\n    path: projects.csv\n",
			"  - object_type: document\n    kind: csv\n    path: projects.csv\n", 1)
		if !strings.Contains(body, "object_type: document\n    kind: csv") {
			t.Fatal("the fixture edit did not apply; the test would assert nothing")
		}
		mustRefuse(t, "a local providers: entry colliding with a live-Postgres row",
			bootStackError(t, live, writeLocalAddition(t, body)),
			aerr.APERTURE_WIRING_LOCAL_COLLISION,
			"document", "providers:")
	})

	t.Run("a local inline objects: entry for a shared object type", func(t *testing.T) {
		// seed.StrictProviderCollision()'s own refusal, not a second mechanism: the
		// DB-wired path passes that option precisely so the rule is not restated.
		mustRefuse(t, "an inline objects: entry colliding with a live-Postgres row",
			bootStackError(t, live, writeSeed(t, "inline.yaml", bootWiringSeed)),
			aerr.APERTURE_CONFIG_INVALID,
			"project")
	})
}
