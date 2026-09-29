package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/seed"
)

// The `kind:` vocabulary is restated in internal/cli and pinned behaviourally,
// exactly as the field-type vocabulary is
// (TestPushAndTheSeedBuilderAgreeOnTheFieldTypeVocabulary).
//
// There are THREE independent unexported spellings of one vocabulary:
// wiringKindSQL / wiringKindCSV here, the literal "sql"/"csv" cases in
// seed/provider.go's buildObjectProvider, and attributeKindSQL /
// attributeKindCSV in seed/attribute_provider.go. Rename or add one in seed/ and
// checkWiringKind goes on accepting the old word: the push VALIDATES, ReplaceWiring
// STORES it, and every instance in the fleet then fails
// BuildRegistryWithConnections on its next boot or poll tick — a fleet-wide outage
// produced by a rename that compiled.
//
// The agreement is not "both refuse", because the two ends are asking different
// questions. A push refuses `kind: csv` because it cannot be SHARED (a filesystem
// path is machine-local), while the builder implements it perfectly. So the property
// is three-valued, and each value pins a different half:
//
//   - SHARED      -> the push accepts it AND the builder implements it. A word the
//     push stores and the builder cannot build is the fleet-wide outage above.
//   - UNSHAREABLE -> the push refuses it with APERTURE_WIRING_KIND_UNSHAREABLE AND
//     the builder implements it. If the builder did not, the refusal would be
//     pointing at shareability while the real fault was a typo, and its remedy
//     ("keep this entry in the LOCAL seed document") would be wrong.
//   - UNKNOWN     -> both refuse. A word only one of them knows is the drift.
//
// It is asserted for BOTH sections, because checkWiringKind applies ONE vocabulary
// to `providers:` and `attribute_providers:` alike while seed implements them in two
// switches: a kind added for objects and not for attribute slots would be accepted
// for a slot, stored, and then refused on every boot — and the attribute half is the
// worse one, since a slot whose provider cannot be constructed answers with a nil
// bag, which WIDENS an exclusive grant instead of denying.

// wiringKindVerdict is what each end of the wiring should say about one word.
type wiringKindVerdict int

const (
	// kindShared is accepted by the push and implemented by the builder.
	kindShared wiringKindVerdict = iota
	// kindUnshareable is implemented by the builder and refused by the push.
	kindUnshareable
	// kindUnknown is refused by both.
	kindUnknown
)

// wiringKindCases is the whole vocabulary plus the spellings that must stay out of
// it. "SQL" is there because the comparison is case-SENSITIVE on both sides, and a
// push that accepted it would store a word no builder switch matches.
var wiringKindCases = []struct {
	kind    string
	verdict wiringKindVerdict
}{
	{"sql", kindShared},
	{"csv", kindUnshareable},
	{"SQL", kindUnknown},
	{"Csv", kindUnknown},
	{"postgres", kindUnknown},
	{"json", kindUnknown},
	{"", kindUnknown},
}

// TestPushAndTheSeedBuildersAgreeOnTheKindVocabulary runs one document per case
// through this package's projection and through seed's own builder for the SAME
// section, and requires the two to agree.
func TestPushAndTheSeedBuildersAgreeOnTheKindVocabulary(t *testing.T) {
	t.Run("providers:", func(t *testing.T) {
		for _, tc := range wiringKindCases {
			t.Run(kindCaseName(tc.kind), func(t *testing.T) {
				dir := t.TempDir()
				writeKindFixtureFiles(t, dir)
				doc := parseKindDoc(t, objectProviderDoc(tc.kind))

				_, pushErr := wiringFromDocument(doc, time.Now().UTC())
				_, conns, buildErr := doc.BuildRegistryWithConnections(dir,
					seed.WithConnectionOpener(kindFixtureOpener))
				if conns != nil {
					defer func() { _ = conns.Close() }()
				}
				assertKindVerdict(t, "providers:", tc.kind, tc.verdict, pushErr, buildErr)
			})
		}
	})

	t.Run("attribute_providers:", func(t *testing.T) {
		for _, tc := range wiringKindCases {
			t.Run(kindCaseName(tc.kind), func(t *testing.T) {
				dir := t.TempDir()
				writeKindFixtureFiles(t, dir)
				doc := parseKindDoc(t, attributeProviderDoc(tc.kind))

				_, pushErr := wiringFromDocument(doc, time.Now().UTC())
				// The pools the attribute builder reads through are the DOCUMENT's,
				// opened once: the same seam a boot uses, so a kind: sql slot is
				// built rather than refused for a connection it never got.
				_, conns, err := doc.BuildRegistryWithConnections(dir,
					seed.WithConnectionOpener(kindFixtureOpener))
				if err != nil {
					t.Fatalf("opening the document's pools: %v", err)
				}
				defer func() { _ = conns.Close() }()
				_, buildErr := doc.BuildAttributeRegistryWithConnections(dir, conns)
				assertKindVerdict(t, "attribute_providers:", tc.kind, tc.verdict, pushErr, buildErr)
			})
		}
	})
}

// assertKindVerdict is the three-valued comparison, written once so the two
// sections cannot drift into asserting different things about one vocabulary.
func assertKindVerdict(t *testing.T, section, kind string, want wiringKindVerdict, pushErr, buildErr error) {
	t.Helper()
	switch want {
	case kindShared:
		if pushErr != nil {
			t.Fatalf("%s kind %q must be pushable: %v", section, kind, pushErr)
		}
		if buildErr != nil {
			t.Fatalf("%s kind %q is accepted by the push and NOT implemented by seed's "+
				"builder: the push validates, ReplaceWiring stores it, and every instance "+
				"in the fleet then fails to boot on it: %v", section, kind, buildErr)
		}
	case kindUnshareable:
		if got := aerr.CodeOf(pushErr); got != aerr.APERTURE_WIRING_KIND_UNSHAREABLE {
			t.Fatalf("%s kind %q: push code = %q, want %q (err: %v)",
				section, kind, got, aerr.APERTURE_WIRING_KIND_UNSHAREABLE, pushErr)
		}
		if buildErr != nil {
			t.Fatalf("%s kind %q is refused as UNSHAREABLE, which claims seed IMPLEMENTS it "+
				"and only a path stands in the way — but the builder refuses it too, so the "+
				"refusal's remedy (\"keep this entry in the LOCAL seed document\") is wrong "+
				"and the word is really a typo: %v", section, kind, buildErr)
		}
	case kindUnknown:
		if got := aerr.CodeOf(pushErr); got != aerr.APERTURE_CONFIG_INVALID {
			t.Fatalf("%s kind %q: push code = %q, want %q — an unknown kind is the same "+
				"APERTURE_CONFIG_INVALID the seed builder raises for it (err: %v)",
				section, kind, got, aerr.APERTURE_CONFIG_INVALID, pushErr)
		}
		if buildErr == nil {
			t.Fatalf("%s kind %q is refused by the push as unknown and ACCEPTED by seed's "+
				"builder: the two vocabularies have drifted, and the push is now refusing "+
				"wiring this deployment could have run", section, kind)
		}
	}
}

// objectProviderDoc is a providers: entry that is valid in every respect EXCEPT
// possibly its kind, so the kind is the only thing either end can refuse.
func objectProviderDoc(kind string) string {
	return `
connections:
  main:
    dsn_env: APERTURE_TEST_KIND_DSN
providers:
  - object_type: document
    kind: "` + kind + `"
    connection: main
    path: objects.csv
    get_one: SELECT tier FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.tier FROM documents d
`
}

// attributeProviderDoc is the same for a slot. get_all is declared because a
// FETCH-ONLY slot is legal and would make an omission read as intent.
func attributeProviderDoc(kind string) string {
	return `
connections:
  main:
    dsn_env: APERTURE_TEST_KIND_DSN
attribute_providers:
  - subject: user
    kind: "` + kind + `"
    connection: main
    path: users.csv
    get_one: SELECT department FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department FROM users u
`
}

// writeKindFixtureFiles materialises the two CSVs a kind: csv entry reads, because
// a csv provider opens its file at BUILD: a fixture that only looked declared would
// report every csv case as "the builder refuses it" and invert the whole test.
func writeKindFixtureFiles(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("APERTURE_TEST_KIND_DSN", unroutedDSN)
	for name, body := range map[string]string{
		"objects.csv": "id,tier\ndocument:42,gold\n",
		"users.csv":   "id,department\nalice,eng\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
}

// kindFixtureOpener answers for the document's one connection without a database.
// No statement runs during a build, so a pool that refuses every query is enough.
func kindFixtureOpener(string, seed.ConnectionSettings) (seed.Pool, error) { return fakePool{}, nil }

// parseKindDoc reads a fixture through seed's own reader, so the document under
// test is decoded exactly as an operator's file is.
func parseKindDoc(t *testing.T, body string) *seed.Document {
	t.Helper()
	doc, err := seed.Parse([]byte(body), seed.FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

// kindCaseName names the empty-kind subtest, which would otherwise be "".
func kindCaseName(kind string) string {
	if kind == "" {
		return "(no kind declared)"
	}
	return kind
}
