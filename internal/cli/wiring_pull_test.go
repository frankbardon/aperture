package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/memory"
	"github.com/frankbardon/aperture/storage/postgres"
)

// E5-S1, driven through the real command tree, against a real store.
//
// The fixtures and helpers are wiring_test.go's (wiringModelSeed,
// wiringSharedSeed, newWiringStore, runWiringCLI, mustRefuse, readWiring,
// sameWiringShape) — a pull has to be tested against the wiring a PUSH actually
// produces, and re-declaring the document here would let the two drift into a pull
// that round-trips a shape nothing deploys.

// runWiringPullCLI runs `aperture wiring pull --store <dsn> --out <path> ...`
// through the real command tree. It goes through runWiringCLI with no --seed,
// because pull takes none: a read of what IS deployed must not merge in a document
// on the command line.
func runWiringPullCLI(t *testing.T, storeDSN, outPath string, args ...string) (string, error) {
	t.Helper()
	return runWiringCLI(t, "pull", "", storeDSN, append([]string{"--out", outPath}, args...)...)
}

// pullPath is a path in a fresh temp directory that nothing has created yet.
func pullPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// readWiringFrom reads the deployed wiring back through whichever backend the DSN
// names.
//
// wiring_test.go's readWiring reaches for sqlite.Open directly, which is right for
// the SQLite-only cases it was written for and silently wrong here: handed a
// postgres:// DSN it creates a FILE named after the connection string. The
// fixed-point proof runs against both backends, so it reads through buildStore —
// the same selector the commands use — and the two arms cannot diverge on which
// database they are asserting about.
func readWiringFrom(t *testing.T, dsn string) model.WiringSet {
	t.Helper()
	store, err := buildStore(context.Background(), dsn, "")
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	defer func() { _ = store.Close() }()
	set, err := store.GetWiring(context.Background())
	if err != nil {
		t.Fatalf("get wiring: %v", err)
	}
	return set
}

// TestPullEmitsTheFourSharedSectionsAndNoModelState is the story's first
// criterion, plus the half that is easy to get wrong in the other direction: the
// file is the four wiring sections, and it is NOT a model export that found
// nothing.
func TestPullEmitsTheFourSharedSectionsAndNoModelState(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	out := pullPath(t, "wiring.yaml")
	if got, err := runWiringPullCLI(t, dsn, out); err != nil {
		t.Fatalf("wiring pull: %v\n%s", err, got)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read pulled document: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"connections:", "providers:", "field_types:", "attribute_providers:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the pulled document has no %s section:\n%s", want, text)
		}
	}
	// The two LOCAL wiring sections and every model section are absent — not
	// present-and-empty. A populated deployment's model is not empty, so a document
	// spelling `object_types: null` would be making a false claim about the store it
	// was read from, and a false claim in a file is one somebody imports.
	for _, absent := range []string{
		"accounts:", "memberships:", "object_types:", "permissions:", "principals:",
		"roles:", "groups:", "grants:", "templates:", "rules:", "objects:", "attributes:",
	} {
		if strings.Contains(text, absent) {
			t.Errorf("the pulled document spells %s; a wiring pull emits the wiring and no model, "+
				"and it must not claim the model sections are empty either:\n%s", absent, text)
		}
	}

	// It parses as a seed document, which is the whole claim of "re-pushable".
	doc, err := seed.ParseFile(out)
	if err != nil {
		t.Fatalf("the pulled document is not a parseable seed document: %v", err)
	}
	if len(doc.Connections) != 2 || len(doc.Providers) != 2 ||
		len(doc.FieldTypes) != 1 || len(doc.AttributeProviders) != 1 {
		t.Fatalf("pulled sections = %d connections, %d providers, %d field-type entries, %d attribute providers; want 2/2/1/1",
			len(doc.Connections), len(doc.Providers), len(doc.FieldTypes), len(doc.AttributeProviders))
	}
	// field_types: is stored one row per (object type, field) and must come back
	// re-GROUPED into one entry with a fields: map, or the document is not the shape
	// a push reads.
	if got := doc.FieldTypes[0].Fields; len(got) != 2 || got["due"] != "date" || got["published_at"] != "datetime" {
		t.Errorf("the field_types: entry did not regroup into one fields: map: %#v", got)
	}
	// references: likewise: stored as rows, spelled as a map.
	var documentEntry seed.Provider
	for _, p := range doc.Providers {
		if p.ObjectType == "document" {
			documentEntry = p
		}
	}
	if got := documentEntry.References; got["project_ids"] != "project" || got["owner_ids"] != "document" {
		t.Errorf("the provider's reference rows did not come back as a references: map: %#v", got)
	}
	if documentEntry.TTL != "5m" {
		t.Errorf("ttl = %q, want the duration TEXT %q: a pull that rendered a parsed duration would emit 300000000000",
			documentEntry.TTL, "5m")
	}
	if documentEntry.MaxSize != 64 {
		t.Errorf("max_size = %d, want 64", documentEntry.MaxSize)
	}
	// The summary names the file and the sections, so an operator can check the pull
	// without opening it.
	got, _ := runWiringPullCLI(t, dsn, pullPath(t, "again.yaml"))
	for _, want := range []string{"connections", "providers", "field types", "attribute providers", "dsn_env"} {
		if !strings.Contains(got, want) {
			t.Errorf("the pull summary does not mention %q:\n%s", want, got)
		}
	}
}

// TestPullIsAFixedPoint is the story's central criterion: push -> pull -> push
// produces identical stored wiring, and two pulls of an unchanged deployment are
// byte-identical.
//
// Both halves are needed and neither implies the other. A pull that dropped a
// field entirely would still be byte-stable; a pull whose map iteration leaked into
// the output would still re-push to the same set on a store that re-sorts. The
// first is what makes the round trip lossless, the second is what makes a diff
// mean something.
func TestPullIsAFixedPoint(t *testing.T) {
	assertWiringPullIsAFixedPoint(t, newWiringStore(t, wiringModelSeed))
}

// assertWiringPullIsAFixedPoint is the fixed-point proof, factored out so the
// gated live-Postgres case below runs exactly the same assertions rather than a
// paraphrase of them.
func assertWiringPullIsAFixedPoint(t *testing.T, dsn string) {
	t.Helper()
	seedPath := writeWiringSeed(t, wiringSharedSeed)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	pushed := readWiringFrom(t, dsn)

	first := pullPath(t, "first.yaml")
	if out, err := runWiringPullCLI(t, dsn, first); err != nil {
		t.Fatalf("first pull: %v\n%s", err, out)
	}
	second := pullPath(t, "second.yaml")
	if out, err := runWiringPullCLI(t, dsn, second); err != nil {
		t.Fatalf("second pull: %v\n%s", err, out)
	}
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read first pull: %v", err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second pull: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("two pulls of unchanged wiring are not byte-identical, so a diff of them reports drift that is not there:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}

	// The fixed point: pushing the pulled document deploys the same wiring. Compared
	// by shape rather than field-for-field including stamps, because a push restamps
	// the whole set to a fresh instant by design — everything an instance BUILDS from
	// must be identical.
	if out, err := runWiringCLI(t, "push", first, dsn); err != nil {
		t.Fatalf("re-push of the pulled document: %v\n%s", err, out)
	}
	repushed := readWiringFrom(t, dsn)
	if !sameWiringShape(pushed, repushed) {
		t.Errorf("push -> pull -> push is not a fixed point:\n--- pushed ---\n%+v\n--- re-pushed ---\n%+v", pushed, repushed)
	}

	// And a pull of the re-pushed wiring is the same bytes again, which is what makes
	// the cycle closed rather than merely convergent-once.
	third := pullPath(t, "third.yaml")
	if out, err := runWiringPullCLI(t, dsn, third); err != nil {
		t.Fatalf("third pull: %v\n%s", err, out)
	}
	c, err := os.ReadFile(third)
	if err != nil {
		t.Fatalf("read third pull: %v", err)
	}
	if string(a) != string(c) {
		t.Errorf("a pull after the round trip differs from the pull before it:\n--- before ---\n%s\n--- after ---\n%s", a, c)
	}

	assertWiringPullIsAFixedPointForADeclaredKeySet(t, dsn)
}

// assertWiringPullIsAFixedPointForADeclaredKeySet is the same proof for the one
// field that used to be lossy.
//
// It is a phase of the fixed point rather than a case beside it, so both backends run
// it: E5-S1's document could not express a declared key set at all, and a pull that
// silently dropped one on Postgres only would pass every dialect-parity gate while
// making `wiring diff` report drift between two identically-wired deployments.
//
// The fixture declares all three states at once — keys, declared-EMPTY, and nothing —
// because the pair that can collapse is the last two. A cycle over a non-empty set
// alone would be a fixed point under an implementation that rendered declared-empty as
// null, which reads back as "not declared" and un-enforces the slot on the re-push.
func assertWiringPullIsAFixedPointForADeclaredKeySet(t *testing.T, dsn string) {
	t.Helper()
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringDeclaredKeysSeed), dsn); err != nil {
		t.Fatalf("push of a document declaring key sets: %v\n%s", err, out)
	}
	pushed := readWiringFrom(t, dsn)

	first := pullPath(t, "keys-first.yaml")
	if out, err := runWiringPullCLI(t, dsn, first); err != nil {
		t.Fatalf("pull of a declared key set: %v\n%s", err, out)
	}
	if out, err := runWiringCLI(t, "push", first, dsn); err != nil {
		t.Fatalf("re-push of the pulled declared key sets: %v\n%s", err, out)
	}
	repushed := readWiringFrom(t, dsn)
	if !sameWiringShape(pushed, repushed) {
		t.Fatalf("push -> pull -> push is not a fixed point for a declared key set:\n--- pushed ---\n%+v\n--- re-pushed ---\n%+v",
			pushed.AttributeProviders, repushed.AttributeProviders)
	}

	second := pullPath(t, "keys-second.yaml")
	if out, err := runWiringPullCLI(t, dsn, second); err != nil {
		t.Fatalf("second pull of a declared key set: %v\n%s", err, out)
	}
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read the first declared-keys pull: %v", err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read the second declared-keys pull: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("a pull of declared key sets is not byte-stable across the round trip:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}
	// The three states as WORDS, on the bytes, because this is the assertion a
	// length-based implementation passes everything else without.
	doc := string(a)
	if strings.Contains(doc, "declared_keys: null") {
		t.Errorf("the pull rendered a declared key set as null, which reads back as NOT DECLARED:\n%s", doc)
	}
	if !strings.Contains(doc, "declared_keys: []") {
		t.Errorf("the pull did not spell the declared-EMPTY set as []:\n%s", doc)
	}
}

// TestPullEmitsNoSecretAndNoCrossAccountData is the security criterion, asserted
// non-vacuously: the fixture's LOCAL sections carry acme, atlas and enterprise, and
// its connections: block carries dsn_env variable NAMES, so an implementation that
// leaked either has something to leak.
func TestPullEmitsNoSecretAndNoCrossAccountData(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	out := pullPath(t, "wiring.yaml")
	if got, err := runWiringPullCLI(t, dsn, out); err != nil {
		t.Fatalf("wiring pull: %v\n%s", err, got)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read pulled document: %v", err)
	}
	text := string(raw)

	for _, forbidden := range []string{
		// No account, no principal, no object identity, no inline data: the shared
		// wiring holds none, so a document rendered from it must hold none either.
		"acme", "account:", "atlas", "enterprise",
		// No credential, and not even the NAME of a variable holding one. dsn_env: is a
		// per-instance fact and has no column in the shared schema, so a pull that
		// emitted one would have invented it.
		"APERTURE_TEST_MAIN_DSN", "APERTURE_TEST_REPORTING_DSN",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the pulled document contains %q:\n%s", forbidden, text)
		}
	}
	// The format has a dsn: key it must never carry a value for, so the assertion is
	// on the key with a value rather than on the substring: `dsn_env: ""` legitimately
	// contains "dsn".
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "dsn:") {
			t.Errorf("the pulled document spells a dsn: key: %q", trimmed)
		}
	}
	// The connection NAMES are the one thing the manifest does carry, so their
	// absence would be a different bug.
	for _, want := range []string{"main", "reporting", "dsn_env"} {
		if !strings.Contains(text, want) {
			t.Errorf("the pulled document does not carry %q; the manifest is names, and the empty dsn_env: is what shows the operator the per-instance fact to supply:\n%s", want, text)
		}
	}
}

// TestTheModelExportStillReproducesNoWiring is the criterion this story must NOT
// break, asserted in the one state that makes it non-vacuous.
//
// seed's own guards (TestAttributeWiringIsNotModelState and its siblings) export a
// store the wiring never reached, so they would pass against a build where Export
// had learned to emit wiring and there was simply none to emit. Here the wiring IS
// in the store, pushed, and the export still reproduces none of it.
//
// It matters because a pull and an export have different exfiltration profiles and
// the difference is the whole reason `pull` is a separate command. Export is
// reachable over Twirp with an admin-tier actor, so anything it learns to emit is
// something a token can take; `wiring pull` is CLI-only and gated by the --store
// credential, and holding that credential already means holding the wiring. Folding
// this capability into Export would have changed the RPC surface's profile — the
// property the original decision was protecting — for no gain. service.Export is
// this function plus the tier gate, so the assertion is placed on the function that
// does the reading.
func TestTheModelExportStillReproducesNoWiring(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	// Non-vacuity: there really is wiring in this store for an export to have leaked.
	if readWiringFrom(t, dsn).IsEmpty() {
		t.Fatal("this test asserts nothing: the store has no wiring in it")
	}

	store := openWiringStore(t, dsn)
	doc, err := seed.Export(context.Background(), store)
	if err != nil {
		t.Fatalf("seed.Export: %v", err)
	}
	if len(doc.Connections) != 0 || len(doc.Providers) != 0 ||
		len(doc.FieldTypes) != 0 || len(doc.AttributeProviders) != 0 {
		t.Errorf("the model export reproduced shared wiring: %d connections, %d providers, %d field types, %d attribute providers — "+
			"`aperture wiring pull` is a separate, CLI-only, store-credential-gated read precisely so Export's exfiltration profile stays what it was",
			len(doc.Connections), len(doc.Providers), len(doc.FieldTypes), len(doc.AttributeProviders))
	}
	// And the model itself still comes back, or the assertion above would hold
	// against an Export that had stopped reading anything.
	if len(doc.ObjectTypes) != 2 {
		t.Errorf("the export did not reproduce the model either: %+v", doc.ObjectTypes)
	}
}

// TestPullRefusesToClobberAnExistingFile is the destructive-operation decision:
// an existing --out is refused, and --force is how an operator says to replace it.
//
// The refusal is the default because the likeliest file at that path is the
// version-controlled document the pull is meant to be DIFFED against, and
// overwriting it destroys the left-hand side of the comparison with nothing left to
// say it had ever been different.
func TestPullRefusesToClobberAnExistingFile(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	out := pullPath(t, "wiring.yaml")
	const existing = "# the operator's own file\n"
	if err := os.WriteFile(out, []byte(existing), 0o600); err != nil {
		t.Fatalf("write the pre-existing file: %v", err)
	}

	_, err := runWiringPullCLI(t, dsn, out)
	mustRefuse(t, "a pull onto an existing file", err, aerr.APERTURE_WIRING_OUTPUT_EXISTS, out, "--force")

	// The refusal left the file exactly as it was. A refusal that had already
	// truncated it would be the failure the refusal exists to prevent.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the file after the refusal: %v", err)
	}
	if string(raw) != existing {
		t.Fatalf("the refused pull changed the file: %q", raw)
	}

	if got, err := runWiringPullCLI(t, dsn, out, "--force"); err != nil {
		t.Fatalf("--force must replace the file: %v\n%s", err, got)
	}
	raw, err = os.ReadFile(out)
	if err != nil {
		t.Fatalf("read the forced file: %v", err)
	}
	if strings.Contains(string(raw), "the operator's own file") {
		t.Errorf("--force did not replace the file's old contents:\n%s", raw)
	}
	if !strings.Contains(string(raw), "providers:") {
		t.Errorf("--force wrote something that is not the wiring:\n%s", raw)
	}
}

// TestPullRefusesAStoreWithNothingDeployed is the one place pull disagrees with
// show, and the disagreement is deliberate.
//
// `show` DESCRIBES an empty store, where nothing deployed is a useful answer. A
// pull produces a file whose purpose is to be pushed back, and an empty document
// pushed back replaces the deployment's wiring with nothing — while an empty read
// is also exactly what a mistyped --store naming a database Setup just created
// looks like. Writing the file would commit the first reading of a situation that
// is usually the second.
func TestPullRefusesAStoreWithNothingDeployed(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	out := pullPath(t, "wiring.yaml")

	_, err := runWiringPullCLI(t, dsn, out)
	mustRefuse(t, "a pull from a store with no wiring", err, aerr.APERTURE_WIRING_NOTHING_DEPLOYED,
		"--store", "push")

	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("the refused pull still created %s; an empty wiring document is the thing the refusal exists to avoid writing", out)
	}

	// The same store answers `show` without an error, so the asymmetry is asserted
	// rather than assumed: it is a property of what each command's output is FOR.
	if got, showErr := runWiringCLI(t, "show", "", dsn); showErr != nil {
		t.Errorf("`wiring show` must still describe an empty store rather than refusing: %v\n%s", showErr, got)
	}
}

// TestPullNeedsBothFlags: neither --out nor --store has a default. An in-memory
// store has nothing deployed to it, and there is no stdout fallback because
// `wiring show` is the command for reading.
func TestPullNeedsBothFlags(t *testing.T) {
	_, err := runWiringCLI(t, "pull", "", "", "--out", "/dev/null")
	mustRefuse(t, "a pull with no --store", err, aerr.APERTURE_INVALID_INPUT, "--store")

	dsn := newWiringStore(t, wiringModelSeed)
	_, err = runWiringCLI(t, "pull", "", dsn)
	mustRefuse(t, "a pull with no --out", err, aerr.APERTURE_INVALID_INPUT, "--out")
}

// TestPullTakesNoSeedAndNoActor pins the surface structurally, for the reasons
// `wiring show`'s equivalent case pins its own.
//
// No --seed: a read of what IS deployed that merged in the document on the command
// line would answer "what would a push deploy?" while looking like it answered this
// question. No actor: the --store credential the operator just supplied already
// grants full WRITE access to this wiring, so an authority check on top of it would
// only mean nobody could diff a deployment without already holding it.
func TestPullTakesNoSeedAndNoActor(t *testing.T) {
	pull := wiringSubcommand(t, "pull")
	got := map[string]bool{}
	for _, f := range pull.Flags {
		for _, n := range f.Names() {
			got[n] = true
		}
	}
	for _, want := range []string{"store", "out", "format", "force"} {
		if !got[want] {
			t.Errorf("`wiring pull` has no --%s flag; its flags are %v", want, got)
		}
	}
	for _, forbidden := range []string{"seed", "principal", "actor"} {
		if got[forbidden] {
			t.Errorf("`wiring pull` declares --%s: a pull reads the store and nothing else, and it is ungated for the same reason `wiring show` is", forbidden)
		}
	}
}

// TestPullChoosesTheFormatTheSameWayExportDoes: a filename means the same format
// to every command that takes one, and --format overrides it. The resolver is
// `aperture export`'s own, reused rather than reimplemented.
func TestPullChoosesTheFormatTheSameWayExportDoes(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	dir := t.TempDir()

	cases := []struct {
		name     string
		file     string
		format   string
		wantJSON bool
	}{
		{"a .json extension is JSON", "wiring.json", "", true},
		{"a .yaml extension is YAML", "wiring.yaml", "", false},
		{"anything else is YAML, the documented default", "wiring", "", false},
		{"--format wins over the extension", "wiring.json.but-yaml", "yaml", false},
		{"--format json over a .yaml name", "explicit.yaml", "json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			args := []string{}
			if tc.format != "" {
				args = append(args, "--format", tc.format)
			}
			if out, err := runWiringPullCLI(t, dsn, path, args...); err != nil {
				t.Fatalf("wiring pull: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			isJSON := strings.HasPrefix(strings.TrimSpace(string(raw)), "{")
			if isJSON != tc.wantJSON {
				t.Fatalf("JSON = %v, want %v:\n%s", isJSON, tc.wantJSON, raw)
			}
			// Either way it has to be re-parseable, which is the only thing a format is
			// for here.
			if _, err := seed.ParseFile(path); err != nil {
				t.Fatalf("the pulled %s document does not parse: %v", tc.file, err)
			}
		})
	}

	_, err := runWiringPullCLI(t, dsn, filepath.Join(dir, "bad.yaml"), "--format", "toml")
	mustRefuse(t, "an unknown --format", err, aerr.APERTURE_INVALID_INPUT, "toml")
}

// TestPullReadsOneAtomicSnapshot pins the read E1-S4 deliberately did NOT use.
//
// `wiring show` reads section by section, because a listing constructs nothing. A
// pull produces a document that will be PUSHED BACK, and four independent reads
// straddling a concurrent push would emit a set that never existed — entries citing
// a manifest that has since been replaced — which is broken wiring behind a
// clean-looking diff. The assertion is on the CALLS rather than on the output,
// because both reads return the same rows on a quiet store: the difference is only
// observable under a concurrent write, which is precisely the case a test cannot
// schedule reliably.
func TestPullReadsOneAtomicSnapshot(t *testing.T) {
	inner := memory.New()
	ctx := context.Background()
	if err := inner.ReplaceWiring(ctx, model.WiringSet{
		Connections: []model.WiringConnection{{Name: "main"}},
	}); err != nil {
		t.Fatalf("seed the fake store: %v", err)
	}
	counting := &countingWiringReads{Storage: inner}

	set, err := pullWiringSnapshot(ctx, counting)
	if err != nil {
		t.Fatalf("pullWiringSnapshot: %v", err)
	}
	if len(set.Connections) != 1 {
		t.Fatalf("the snapshot did not come back: %+v", set)
	}
	if counting.getWiring != 1 {
		t.Errorf("GetWiring was called %d times, want exactly 1", counting.getWiring)
	}
	if counting.listCalls != 0 {
		t.Errorf("a pull made %d per-section List reads; it must take ONE atomic snapshot through GetWiring, "+
			"or it can emit a document assembled from a wiring set that never existed", counting.listCalls)
	}
}

// countingWiringReads counts which wiring read a caller took. It embeds a real
// model.Storage rather than faking 60-odd methods, so the reads it does not count
// still behave.
type countingWiringReads struct {
	model.Storage
	getWiring int
	listCalls int
}

func (c *countingWiringReads) GetWiring(ctx context.Context) (model.WiringSet, error) {
	c.getWiring++
	return c.Storage.GetWiring(ctx)
}

func (c *countingWiringReads) ListWiringConnections(ctx context.Context) ([]model.WiringConnection, error) {
	c.listCalls++
	return c.Storage.ListWiringConnections(ctx)
}

func (c *countingWiringReads) ListWiringProviders(ctx context.Context) ([]model.WiringProvider, error) {
	c.listCalls++
	return c.Storage.ListWiringProviders(ctx)
}

func (c *countingWiringReads) ListWiringFieldTypes(ctx context.Context) ([]model.WiringFieldType, error) {
	c.listCalls++
	return c.Storage.ListWiringFieldTypes(ctx)
}

func (c *countingWiringReads) ListWiringAttributeProviders(ctx context.Context) ([]model.WiringAttributeProvider, error) {
	c.listCalls++
	return c.Storage.ListWiringAttributeProviders(ctx)
}

// TestPullEmitsTheSectionsInACanonicalOrderRegardlessOfPushOrder: the output order
// is the store's canonical order (model.WiringSet.Sort) and not the order the
// document happened to list entries in. Two documents that declare the same wiring
// in different orders therefore pull to the same bytes, which is what stops a
// reordered repository file reading as drift.
func TestPullEmitsTheSectionsInACanonicalOrderRegardlessOfPushOrder(t *testing.T) {
	const forwards = `
connections:
  alpha: {dsn_env: A}
  beta: {dsn_env: B}
providers:
  - {object_type: document, kind: sql, connection: alpha, get_one: "SELECT 1 WHERE id = $1", get_all: "SELECT 'document:1' AS id"}
  - {object_type: project, kind: sql, connection: beta, get_one: "SELECT 2 WHERE id = $1", get_all: "SELECT 'project:1' AS id"}
field_types:
  - object_type: document
    fields: {due: date, published_at: datetime}
`
	const backwards = `
connections:
  beta: {dsn_env: B}
  alpha: {dsn_env: A}
providers:
  - {object_type: project, kind: sql, connection: beta, get_one: "SELECT 2 WHERE id = $1", get_all: "SELECT 'project:1' AS id"}
  - {object_type: document, kind: sql, connection: alpha, get_one: "SELECT 1 WHERE id = $1", get_all: "SELECT 'document:1' AS id"}
field_types:
  - object_type: document
    fields: {published_at: datetime, due: date}
`
	pullOf := func(body string) string {
		t.Helper()
		dsn := newWiringStore(t, wiringModelSeed)
		if out, err := runWiringCLI(t, "push", writeWiringSeed(t, body), dsn); err != nil {
			t.Fatalf("push: %v\n%s", err, out)
		}
		path := pullPath(t, "wiring.yaml")
		if out, err := runWiringPullCLI(t, dsn, path); err != nil {
			t.Fatalf("pull: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read pull: %v", err)
		}
		return string(raw)
	}

	a, b := pullOf(forwards), pullOf(backwards)
	if a != b {
		t.Errorf("the same wiring declared in a different order pulls to different bytes, so a reordered repository file would read as drift:\n--- forwards ---\n%s\n--- backwards ---\n%s", a, b)
	}
	if !strings.Contains(a, "alpha") || !strings.Contains(a, "beta") {
		t.Fatalf("this test is not about the wiring it thinks it is:\n%s", a)
	}
	if strings.Index(a, "alpha") > strings.Index(a, "beta") {
		t.Errorf("the connection manifest is not in the canonical sorted order a read returns:\n%s", a)
	}
}

// ---- The gated live-Postgres proof ----
//
// SQLite is a real database and the cases above are real proof against it, but the
// fixed point is a claim about the STORE: GetWiring reads all four sections inside a
// transaction, and the ordering that makes a pull byte-stable is an ORDER BY the two
// backends spell differently (Postgres needs COLLATE "C", SQLite does not). A round
// trip that is a fixed point on one dialect and not the other is exactly the drift
// the dialect-parity gates exist for, and only a server settles it.
//
// GATED, and it must stay gated: CI has no service containers. The environment
// variables are the ones storage/postgres and seed already use, so one exported DSN
// drives every live suite in one shell:
//
//	APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/
//
// Never put a DSN in a file; pass it in the environment.
//
// Ungated it SKIPS. Gated with an empty DSN it FAILS — asking for the live proof and
// silently not getting it is the outcome a gate must never produce.
const (
	pullPGGateEnv = "APERTURE_PG_INTEGRATION"
	pullPGDSNEnv  = "APERTURE_PG_DSN"
)

// requireLivePostgres skips unless the gate is on, and returns the DSN.
func requireLivePostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv(pullPGGateEnv) != "1" {
		t.Skipf("skipping the live PostgreSQL proof: set %s=1 and %s=<dsn> to run it", pullPGGateEnv, pullPGDSNEnv)
	}
	dsn := os.Getenv(pullPGDSNEnv)
	if strings.TrimSpace(dsn) == "" {
		t.Fatalf("%s=1 but %s is empty: the gate is on and there is no database to run against. Export %s=<dsn> in the environment, never in a file", pullPGGateEnv, pullPGDSNEnv, pullPGDSNEnv)
	}
	return dsn
}

// livePostgresWiringStore gives one test its OWN PostgreSQL schema, creates
// Aperture's tables in it, applies the model state a push needs, and drops the
// schema afterwards so a live run leaves no residue in whatever database the
// operator pointed it at.
//
// The schema is chosen through APERTURE_POSTGRES_SCHEMA rather than a flag because
// that variable IS the knob — there is no --store-schema — and buildStore reads it
// where it opens the backend. t.Setenv restores it, and it also fails the test if
// the package is ever made parallel, which is the right outcome: two tests sharing
// one process cannot each have their own value of it.
func livePostgresWiringStore(t *testing.T, ctx context.Context, dsn string) string {
	t.Helper()
	name := fmt.Sprintf("aperture_cli_pull_%d", time.Now().UnixNano())
	if err := postgres.ValidateSchemaName(name); err != nil {
		t.Fatalf("this test's own generated schema name is not one Aperture accepts: %v", err)
	}
	// "pgx" is registered by storage/postgres, which internal/cli already imports; the
	// admin handle is only here to create and drop the scratch schema.
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Registered FIRST so it runs LAST: cleanups run in reverse order, and a
	// `defer admin.Close()` would close the pool before the DROP below.
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", pullPGDSNEnv, err)
	}
	t.Cleanup(func() {
		// Reported, not discarded. A cleanup that cannot fail is a cleanup nobody finds
		// out has stopped working, and this one is what keeps a live run residue-free.
		if _, err := admin.Exec(`DROP SCHEMA IF EXISTS "` + name + `" CASCADE`); err != nil {
			t.Errorf("dropping the scratch schema %s left residue behind: %v", name, err)
		}
	})

	t.Setenv(postgres.EnvSchema, name)
	// buildStore with an empty seed path opens the backend and runs Setup, which
	// creates the schema and the tables. The model state then goes in through the same
	// loader newWiringStore uses, so the two backends' fixtures are the same fixture.
	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("open the live store: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := seed.Load(ctx, store, []byte(wiringModelSeed), seed.FormatYAML); err != nil {
		t.Fatalf("apply model state: %v", err)
	}
	return dsn
}

// TestPostgresLiveWiringPullIsAFixedPoint runs the whole fixed-point proof against
// a real server, in its own schema, with the same assertions the SQLite case makes.
func TestPostgresLiveWiringPullIsAFixedPoint(t *testing.T) {
	dsn := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	assertWiringPullIsAFixedPoint(t, livePostgresWiringStore(t, ctx, dsn))
}

// TestPostgresLiveWiringPullMatchesTheSQLitePull is the parity half, and it is the
// one the two-dialect schema gates cannot reach: they prove the two schemas DESCRIBE
// the same database, not that a read of one renders the same document as a read of
// the other. Emitting the sections in a different order, or dropping a field on one
// backend only, would pass every parity gate and make `wiring diff` report drift
// between two identically-wired deployments.
func TestPostgresLiveWiringPullMatchesTheSQLitePull(t *testing.T) {
	live := requireLivePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pullFrom := func(dsn, name string) string {
		t.Helper()
		if out, err := runWiringCLI(t, "push", writeWiringSeed(t, wiringSharedSeed), dsn); err != nil {
			t.Fatalf("push to %s: %v\n%s", name, err, out)
		}
		path := pullPath(t, name+".yaml")
		if out, err := runWiringPullCLI(t, dsn, path); err != nil {
			t.Fatalf("pull from %s: %v\n%s", name, err, out)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the %s pull: %v", name, err)
		}
		return string(raw)
	}

	// The SQLite store first: t.Setenv inside livePostgresWiringStore applies to the
	// whole test, and only the Postgres backend reads it.
	fromSQLite := pullFrom(newWiringStore(t, wiringModelSeed), "sqlite")
	fromPostgres := pullFrom(livePostgresWiringStore(t, ctx, live), "postgres")
	if fromSQLite != fromPostgres {
		t.Errorf("the two backends pull the same wiring as different documents:\n--- sqlite ---\n%s\n--- postgres ---\n%s", fromSQLite, fromPostgres)
	}
}

// TestTheLiveGateSharesItsVariablesWithTheOtherLiveSuites: one exported DSN has to
// drive every live suite in one shell, so the two variable names are asserted rather
// than left as a convention nobody rechecks. The DSN itself is never in a file —
// there is no connection string in this package, only the two names below.
func TestTheLiveGateSharesItsVariablesWithTheOtherLiveSuites(t *testing.T) {
	if pullPGGateEnv != "APERTURE_PG_INTEGRATION" || pullPGDSNEnv != "APERTURE_PG_DSN" {
		t.Errorf("this suite gates on %s/%s, which are not the variables storage/postgres and seed use; "+
			"one exported DSN must drive every live suite", pullPGGateEnv, pullPGDSNEnv)
	}
}
