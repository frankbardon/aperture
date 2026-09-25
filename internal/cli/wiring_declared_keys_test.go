package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
)

// E3-S2, driven through the real command tree: a shared attribute-provider entry
// carries an OPTIONAL declared key set, and the three states it can be in survive
// the whole round trip.
//
// The fixtures and helpers are wiring_test.go's and wiring_pull_test.go's
// (wiringModelSeed, writeWiringSeed, newWiringStore, openWiringStore, runWiringCLI,
// readWiring, sameWiringShape, mustRefuse, pullPath, runWiringPullCLI) — a declared
// set has to be tested against the wiring a real push produces and a real pull
// emits, and re-declaring either here would let this story's proof drift away from
// the commands it is about.
//
// # Why the three states are the subject of every case below
//
// NOT DECLARED (declared_keys: absent) opts the slot out of key enforcement.
// DECLARED EMPTY (declared_keys: []) opts it IN and permits nothing. Declared with
// names permits those names. The first two are the pair a []string cannot tell
// apart, which is why the seed field is a *[]string and model.DeclaredKeys is a
// struct, and every layer between them was built to keep them apart. A test that
// only ever asserted a non-empty set would pass under an implementation that
// collapsed them.

// wiringDeclaredKeysSeed carries all three states at once, one per slot, so a push
// that got any of them right by accident still fails.
//
// user declares two keys, machine declares an EMPTY set, and account declares no
// key at all. It is the same document the pull and re-push cases below re-use, so
// "what a push stores" and "what a pull emits" are asserted about one fixture.
const wiringDeclaredKeysSeed = `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department, clearance FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department, u.clearance FROM users u
    declared_keys: [department, clearance]
  - subject: machine
    kind: sql
    connection: main
    get_one: SELECT fleet FROM machines WHERE id = $1
    declared_keys: []
  - subject: account
    kind: sql
    connection: main
    get_one: SELECT plan FROM accounts WHERE id = $1
`

// declaredKeysBySlot indexes a set's attribute providers by slot, so a case can
// name the slot it means instead of a position in a sorted slice.
func declaredKeysBySlot(t *testing.T, set model.WiringSet) map[string]model.DeclaredKeys {
	t.Helper()
	out := make(map[string]model.DeclaredKeys, len(set.AttributeProviders))
	for _, ap := range set.AttributeProviders {
		out[ap.Subject] = ap.DeclaredKeys
	}
	return out
}

// assertDeclaredKeys states one slot's expected state in the vocabulary the states
// are named in, rather than as two struct fields a reader has to re-derive.
func assertDeclaredKeys(t *testing.T, where, slot string, got model.DeclaredKeys, declared bool, keys ...string) {
	t.Helper()
	if got.Declared != declared {
		t.Errorf("%s: slot %q Declared = %v, want %v — not-declared opts the slot OUT of key "+
			"enforcement and declared-empty opts it IN and permits nothing, so this bit is the "+
			"whole difference between the two", where, slot, got.Declared, declared)
		return
	}
	if len(got.Keys) != len(keys) {
		t.Errorf("%s: slot %q keys = %v, want %v", where, slot, got.Keys, keys)
		return
	}
	for i, k := range keys {
		if got.Keys[i] != k {
			t.Errorf("%s: slot %q key %d = %q, want %q — a declared set is stored in the order it "+
				"was written, so a pull reproduces the author's list", where, slot, i, got.Keys[i], k)
		}
	}
}

// TestPushStoresADeclaredKeySetAndOmittingItIsLegal is the story's first and third
// criteria together: the schema accepts declared_keys:, and a document that leaves
// it out is a legal document whose slot is stored as NOT DECLARED.
//
// All three states go in through one push, because the failure this guards against
// is not "the field is ignored" — that would be caught by the first state alone — it
// is a projection that reads Declared off the length of the list. Such a projection
// stores user and account correctly and turns machine's declared-empty set into a
// not-declared one, which silently un-enforces the slot.
func TestPushStoresADeclaredKeySetAndOmittingItIsLegal(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringDeclaredKeysSeed), dsn); err != nil {
		t.Fatalf("a declared key set must be pushable: %v\n%s", err, out)
	}
	keys := declaredKeysBySlot(t, readWiring(t, dsn))
	if len(keys) != 3 {
		t.Fatalf("attribute providers = %d, want the three slots the fixture declares", len(keys))
	}
	assertDeclaredKeys(t, "push", "user", keys["user"], true, "department", "clearance")
	assertDeclaredKeys(t, "push", "machine", keys["machine"], true)
	assertDeclaredKeys(t, "push", "account", keys["account"], false)
}

// TestAnAbsentAndAnExplicitlyNullDeclaredKeySetAreBothNotDeclared pins the two
// spellings of "I am not declaring one".
//
// An absent key never reaches the field at all; an explicit `declared_keys:` is YAML
// null, which decodes to a nil POINTER rather than to a pointer at an empty slice.
// Both are not-declared, and the second is the one that could plausibly have been
// read as declared-empty — so it is asserted rather than assumed.
func TestAnAbsentAndAnExplicitlyNullDeclaredKeySetAreBothNotDeclared(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"absent", ""},
		{"explicit null", "    declared_keys:\n"},
		{"explicit tilde", "    declared_keys: ~\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "connections:\n  main:\n    dsn_env: APERTURE_TEST_MAIN_DSN\n" +
				"attribute_providers:\n  - subject: user\n    kind: sql\n    connection: main\n" +
				"    get_one: SELECT department FROM users WHERE id = $1\n" + tc.line
			doc, err := seed.Parse([]byte(body), seed.FormatYAML)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if doc.AttributeProviders[0].DeclaredKeys != nil {
				t.Fatalf("declared_keys %s decoded to a non-nil pointer (%v); it must be nil, or "+
					"the slot is opted INTO key enforcement by a line that declares nothing",
					tc.name, *doc.AttributeProviders[0].DeclaredKeys)
			}
			set, err := wiringFromDocument(doc, zeroTime.AddDate(2000, 0, 0))
			if err != nil {
				t.Fatalf("project: %v", err)
			}
			assertDeclaredKeys(t, tc.name, "user", set.AttributeProviders[0].DeclaredKeys, false)
		})
	}
}

// TestAPresentButEmptyDeclaredKeySetIsDeclared is the other half, at the layer
// where a []string would have lost it.
//
// `declared_keys: []` decodes to a NON-NIL pointer at an empty slice. The pointer is
// the only thing that distinguishes it from the cases above — the slice it points at
// is length zero either way — which is why the field's shape is a decision and not a
// style choice, and why it is asserted here on the decoded document rather than only
// end to end.
func TestAPresentButEmptyDeclaredKeySetIsDeclared(t *testing.T) {
	doc, err := seed.Parse([]byte("connections:\n  main:\n    dsn_env: APERTURE_TEST_MAIN_DSN\n"+
		"attribute_providers:\n  - subject: user\n    kind: sql\n    connection: main\n"+
		"    get_one: SELECT department FROM users WHERE id = $1\n    declared_keys: []\n"),
		seed.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := doc.AttributeProviders[0].DeclaredKeys
	if got == nil {
		t.Fatal("declared_keys: [] decoded to a nil pointer, so a slot that permits NO key is " +
			"indistinguishable from one that opted out of enforcement entirely — which is the " +
			"single collapse the pointer shape exists to prevent")
	}
	if len(*got) != 0 {
		t.Fatalf("declared_keys: [] decoded to %v", *got)
	}
	set, err := wiringFromDocument(doc, zeroTime.AddDate(2000, 0, 0))
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	assertDeclaredKeys(t, "declared empty", "user", set.AttributeProviders[0].DeclaredKeys, true)
}

// TestADeclaredKeySetRoundTripsThroughAPull is the story's second criterion: push ->
// storage -> pull reproduces the set, and the pulled document re-pushes to the same
// wiring.
//
// The byte-level assertion on the emitted document is the one that matters most.
// `declared_keys: []` and `declared_keys: null` are the same LENGTH and different
// ANSWERS, so an emitter that rendered a nil slice would produce a file that reads as
// correct, diffs as correct, and un-enforces the slot on the next push.
func TestADeclaredKeySetRoundTripsThroughAPull(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringDeclaredKeysSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	pushed := readWiring(t, dsn)

	path := pullPath(t, "wiring.yaml")
	if out, err := runWiringPullCLI(t, dsn, path); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pulled document: %v", err)
	}
	doc := string(body)
	if strings.Contains(doc, "declared_keys: null") {
		t.Errorf("the pull emitted `declared_keys: null`; null decodes back to NOT DECLARED, so "+
			"re-pushing this file would opt a slot that permits no key back out of enforcement:\n%s", doc)
	}
	if !strings.Contains(doc, "declared_keys: []") {
		t.Errorf("the pull did not emit `declared_keys: []` for the declared-EMPTY slot; that state "+
			"has to be spelled explicitly or it is lost:\n%s", doc)
	}
	for _, want := range []string{"- department", "- clearance"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the pull did not emit the declared key %q:\n%s", want, doc)
		}
	}
	// The slot that declared nothing carries no key at all. Three occurrences of the
	// word would mean the not-declared slot had been given a set.
	if n := strings.Count(doc, "declared_keys"); n != 2 {
		t.Errorf("the pulled document spells declared_keys %d times, want 2 — the slot that "+
			"declared NO set must carry no key, because an absent key is how not-declared is "+
			"written:\n%s", n, doc)
	}

	// The fixed point, including the key set: re-pushing the pulled document deploys
	// the same wiring. sameWiringShape compares DeclaredKeys field for field, so a
	// state flipped anywhere on the way round fails here.
	if out, err := runWiringCLI(t, "push", path, dsn); err != nil {
		t.Fatalf("re-push of the pulled document: %v\n%s", err, out)
	}
	repushed := readWiring(t, dsn)
	if !sameWiringShape(pushed, repushed) {
		t.Fatalf("push -> pull -> push is not a fixed point for a declared key set:\n--- pushed ---\n%+v\n--- re-pushed ---\n%+v",
			pushed.AttributeProviders, repushed.AttributeProviders)
	}
	keys := declaredKeysBySlot(t, repushed)
	assertDeclaredKeys(t, "re-push", "user", keys["user"], true, "department", "clearance")
	assertDeclaredKeys(t, "re-push", "machine", keys["machine"], true)
	assertDeclaredKeys(t, "re-push", "account", keys["account"], false)

	// And a second pull is the same bytes, which is what makes the document diffable
	// against version control rather than merely re-pushable.
	again := pullPath(t, "again.yaml")
	if out, err := runWiringPullCLI(t, dsn, again); err != nil {
		t.Fatalf("second pull: %v\n%s", err, out)
	}
	second, err := os.ReadFile(again)
	if err != nil {
		t.Fatalf("read the second pull: %v", err)
	}
	if string(second) != doc {
		t.Errorf("two pulls across the round trip differ:\n--- first ---\n%s\n--- second ---\n%s", doc, second)
	}
}

// TestAPullEmitsADeclaredSetWrittenStraightToStorage is the case E5-S1 could only
// warn about, now asserted as a round trip.
//
// A set written straight through ReplaceWiring — the only way to produce one before
// the seed key existed — pulls back as a document that expresses it. The slots are
// written in the states E5-S1's warning test used, so the same fixture that proved
// the gap now proves it closed.
func TestAPullEmitsADeclaredSetWrittenStraightToStorage(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	store := openWiringStore(t, dsn)
	// The manifest and the statements are here so the PULLED document is one a push
	// accepts: a row written straight to storage can legally name no connection, but a
	// document that named none would be refused for that and the declared key set
	// would never be reached.
	set := model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main"}},
		AttributeProviders: []model.WiringAttributeProvider{
			{Subject: "account", Kind: "sql", Connection: "main",
				GetOne:       "SELECT plan FROM accounts WHERE id = $1",
				DeclaredKeys: model.DeclaredKeys{}},
			{Subject: "machine", Kind: "sql", Connection: "main",
				GetOne:       "SELECT fleet FROM machines WHERE id = $1",
				DeclaredKeys: model.DeclaredKeys{Declared: true}},
			{Subject: "user", Kind: "sql", Connection: "main",
				GetOne: "SELECT clearance, department FROM users WHERE id = $1",
				DeclaredKeys: model.DeclaredKeys{
					Declared: true, Keys: []string{"clearance", "department"}}},
		}}
	if err := store.ReplaceWiring(context.Background(), set); err != nil {
		t.Fatalf("replace wiring: %v", err)
	}

	path := pullPath(t, "wiring.yaml")
	got, err := runWiringPullCLI(t, dsn, path)
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, got)
	}
	// No warning any more: the document expresses the set, so a caveat here would be a
	// false alarm on every pull of every deployment that declares one.
	if strings.Contains(got, "warning") && strings.Contains(got, "declared") {
		t.Errorf("the pull still warns about a declared key set it can now express:\n%s", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pulled document: %v", err)
	}
	doc, err := seed.Parse(body, seed.FormatYAML)
	if err != nil {
		t.Fatalf("the pulled document does not parse: %v\n%s", err, body)
	}
	back, err := wiringFromDocument(doc, zeroTime.AddDate(2000, 0, 0))
	if err != nil {
		t.Fatalf("the pulled document does not project: %v\n%s", err, body)
	}
	keys := declaredKeysBySlot(t, back)
	assertDeclaredKeys(t, "pull", "user", keys["user"], true, "clearance", "department")
	assertDeclaredKeys(t, "pull", "machine", keys["machine"], true)
	assertDeclaredKeys(t, "pull", "account", keys["account"], false)
}

// TestShowPrintsAPushedDeclaredKeySet confirms E1-S4's display still holds, now that
// a push can produce all three states rather than only the storage layer.
//
// E1-S4 asserted the printer against rows written straight to storage, because that
// was the only way to make one. The same three words must read back from a document
// an operator actually wrote, or the listing and the file disagree about what was
// deployed.
func TestShowPrintsAPushedDeclaredKeySet(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringDeclaredKeysSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	out, err := runWiringCLI(t, "show", "", dsn)
	if err != nil {
		t.Fatalf("wiring show: %v\n%s", err, out)
	}
	for _, want := range []string{"(not declared)", "(declared empty)", "department, clearance"} {
		if !strings.Contains(out, want) {
			t.Errorf("show does not print %q for a PUSHED declared key set; the three states must "+
				"read differently in the one place an operator reads them:\n%s", want, out)
		}
	}
}

// TestPushRefusesAMalformedDeclaredKeySet: an empty name and a repeated one are
// refused before anything is written, each naming what the operator has to fix.
//
// Both are refused at the CLI rather than left to the store. Storage refuses the same
// two, but of a row whose names have already been trimmed, and it carries the
// offending key in Context where a CLI refusal is printed with %v — so the one thing
// the operator needs would be invisible exactly where it is read.
func TestPushRefusesAMalformedDeclaredKeySet(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	head := "connections:\n  main:\n    dsn_env: APERTURE_TEST_MAIN_DSN\n" +
		"attribute_providers:\n  - subject: user\n    kind: sql\n    connection: main\n" +
		"    get_one: SELECT department FROM users WHERE id = $1\n"

	_, err := runWiringCLI(t, "push", writeWiringSeed(t, head+"    declared_keys: [department, \"\"]\n"), dsn)
	mustRefuse(t, "a declared key set with an empty name", err, aerr.APERTURE_CONFIG_INVALID,
		"user", "empty attribute key", "declared_keys: []")

	_, err = runWiringCLI(t, "push", writeWiringSeed(t, head+"    declared_keys: [department, department]\n"), dsn)
	mustRefuse(t, "a declared key set with a repeated name", err, aerr.APERTURE_CONFIG_INVALID,
		"user", "department", "twice")

	// An all-whitespace name is the empty one after trimming, and it must not slip
	// through as a key no rule can ever name.
	_, err = runWiringCLI(t, "push", writeWiringSeed(t, head+"    declared_keys: [\"   \"]\n"), dsn)
	mustRefuse(t, "a declared key set with a whitespace-only name", err, aerr.APERTURE_CONFIG_INVALID,
		"user", "empty attribute key")

	// Nothing was written: a refused push changes nothing, so a malformed declared set
	// cannot leave a partially deployed wiring behind.
	if set := readWiring(t, dsn); len(set.AttributeProviders) != 0 || len(set.Connections) != 0 {
		t.Errorf("a refused push deployed something: %+v", set)
	}
}

// TestADeclaredKeyIsTrimmedAndKeptInDeclarationOrder pins the two things a rule
// author depends on: the name stored is the name written, and the order is theirs.
//
// Trimming matters because a key is compared against the path a rule NAMES, and a
// stored " department" would match nothing while reading identically in every
// listing. Order matters because a pull re-emits the list, and sorting it would make
// every deployment's first pull a spurious diff.
func TestADeclaredKeyIsTrimmedAndKeptInDeclarationOrder(t *testing.T) {
	doc, err := seed.Parse([]byte("connections:\n  main:\n    dsn_env: APERTURE_TEST_MAIN_DSN\n"+
		"attribute_providers:\n  - subject: user\n    kind: sql\n    connection: main\n"+
		"    get_one: SELECT department FROM users WHERE id = $1\n"+
		"    declared_keys: [\"  zeta \", alpha, \" middle\"]\n"), seed.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	set, err := wiringFromDocument(doc, zeroTime.AddDate(2000, 0, 0))
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	assertDeclaredKeys(t, "trim", "user", set.AttributeProviders[0].DeclaredKeys,
		true, "zeta", "alpha", "middle")
}
