package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// THE LOCAL HALF OF WHAT IDENTIFIES A WIRING VERSION.
//
// A rebuild re-reads this instance's own document — buildWiredStack -> seedDocument
// -> seed.ParseFile, a fresh disk read on a swap exactly as on a boot — and that
// document decides things: inline objects: metadata, inline attributes: bags, the
// declared attribute-key sets taken from them, and on a deployment that has pushed
// nothing at all the whole of the wiring. wiringDigest covers none of it, which
// left two instances able to report the IDENTICAL shared digest and return
// DIFFERENT verdicts — while docs/src/operations/two-instance-topology.md tells an
// operator to sweep that digest to establish the pair is wired the same.
//
// localWiringDigest is the second value that closes it, reported beside the shared
// one as service.WiringPosture.LocalDigest. The cases here are about four things,
// and each of them fails in a different direction:
//
//   - the value tracks the RUNNING version, so a swap that re-read a CHANGED file
//     advances it. A value captured once at boot would be wrong in precisely the
//     scenario the field exists to expose.
//   - a swap that re-read an UNCHANGED file leaves it alone, or the sweep reports
//     drift on every push and the operator stops reading it.
//   - the two digests are INDEPENDENT in both directions, or the separation is
//     unproven and one of them is really a mix.
//   - an absent local document is a legitimate state — the state of every
//     `aperture serve --store postgres://…` with no --seed, which is the deployment
//     shared wiring exists to enable.

// TestTheLocalDigestReportsTheDocumentTheBootWasBuiltFrom is the baseline. The
// boot's pair is what the poller is baselined on and what the staleness recorder
// is seeded with, and both come from ONE derivation (decisionStack.digests).
func TestTheLocalDigestReportsTheDocumentTheBootWasBuiltFrom(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	boot := probe.live.current()
	if boot.stack.localDigest == "" {
		t.Fatal("a boot from a populated --seed file reported no local digest: the fixture declares objects:, " +
			"attributes: and a model, so \"\" would mean the digest is not reading the document at all")
	}
	if got := probe.poll.digests; got.Local != boot.stack.localDigest || got.Shared != boot.digest {
		t.Errorf("the poller was baselined on %+v, want the boot version's pair (%q / %q). A baseline the "+
			"loop derived for itself is a second answer that can disagree with what this instance is "+
			"actually wired with", got, boot.digest, boot.stack.localDigest)
	}
	if got := probe.live.current().digests(); got.Local != boot.stack.localDigest {
		t.Errorf("the version's own accessor reported %+v, want the local digest %q it was built with",
			got, boot.stack.localDigest)
	}
}

// TestASwapThatReReadsAChangedLocalFileAdvancesTheLocalDigest is the scenario this
// whole field exists for, asserted end to end through the real poller.
//
// The seed file is rewritten in place — the ordinary ConfigMap-volume remount, or a
// redeploy that only updates a mounted file — and then an UNRELATED shared push
// arrives. The rebuild adopts the new file as a side effect, which is desirable and
// documented; what must not happen is that it does so invisibly. Two versions built
// from two different local documents must report two different local digests.
func TestASwapThatReReadsAChangedLocalFileAdvancesTheLocalDigest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")
	probe.poll.health = service.NewWiringHealth(time.Hour, probe.live.current().digests(), nil)

	boot := probe.live.current()
	bootLocal := boot.stack.localDigest

	// The file changes, and then a push that says nothing about anything in it
	// arrives. The department is what the rebuilt attribute slot will serve, so this
	// is a change that really does move a verdict's inputs.
	writeSwapSeed(t, probe.seedPath, "legal")
	probe.pushFieldType(t, ctx)

	next := probe.live.current()
	if next == boot {
		t.Fatal("the push installed no new version, so this case cannot say anything about the digest")
	}
	if next.stack.localDigest == bootLocal {
		t.Fatalf("two versions built from two DIFFERENT local documents report the same local digest %q. "+
			"That is the exact failure the field exists to expose: an operator sweeping the fleet would "+
			"conclude these two configurations are identical", bootLocal)
	}
	if probe.poll.digests.Local != next.stack.localDigest {
		t.Errorf("the poller is still naming %q after adopting a version built from %q. The pair must be "+
			"advanced from the INSTALLED version, or the posture reports a document this process no "+
			"longer decides from", probe.poll.digests.Local, next.stack.localDigest)
	}
	if p := probe.poll.health.Posture(); p.LocalDigest != next.stack.localDigest || p.Digest != next.digest {
		t.Errorf("the recorded posture is %q / %q, want the installed version's %q / %q. A posture naming a "+
			"fresh shared digest beside a superseded local one is a pair no version was ever built from",
			p.Digest, p.LocalDigest, next.digest, next.stack.localDigest)
	}
}

// TestASwapThatReReadsAnUnchangedLocalFileLeavesTheLocalDigestAlone is the other
// direction, and it is what keeps the sweep usable.
//
// A push is adopted, the shared digest advances, and the file on disk is exactly
// what it was. A local digest that moved anyway — because it had picked up an
// mtime, a path, a byte count or the shared set — would report drift on every push
// of every deployment, which is how an operator learns to ignore the field.
func TestASwapThatReReadsAnUnchangedLocalFileLeavesTheLocalDigestAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := newSwapProbe(t, ctx, "eng")

	boot := probe.live.current()
	probe.pushFieldType(t, ctx)
	next := probe.live.current()

	if next.digest == boot.digest {
		t.Fatal("the shared digest did not advance, so no swap happened and this case proves nothing")
	}
	if next.stack.localDigest != boot.stack.localDigest {
		t.Errorf("the local digest moved (%q -> %q) across a swap that re-read an UNCHANGED file. It must "+
			"cover the document's content and nothing volatile — not the mtime, not the path, not the "+
			"shared set", boot.stack.localDigest, next.stack.localDigest)
	}
	if probe.poll.digests.Local != boot.stack.localDigest {
		t.Errorf("the poller's local baseline moved to %q for an unchanged file", probe.poll.digests.Local)
	}
}

// TestTheTwoDigestsMoveIndependently is the separation, asserted in BOTH directions
// over the real builder — because one direction alone does not prove it.
//
// A shared change that moved the local digest, or a local change that moved the
// shared one, would each mean one of the two values is really a mix of both: the
// shared digest would stop being comparable with what a push wrote and with
// `aperture wiring diff`, and the local one would stop being a statement about this
// host. Three stacks over one store are enough to pin both.
func TestTheTwoDigestsMoveIndependently(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "digests.db")
	fileA := filepath.Join(dir, "a.yaml")
	fileB := filepath.Join(dir, "b.yaml")
	writeSwapSeed(t, fileA, "eng")
	writeSwapSeed(t, fileB, "legal")

	store, err := buildStore(ctx, dsn, fileA)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	build := func(t *testing.T, seedPath string, wiring model.WiringSet) decisionStack {
		t.Helper()
		stack, err := buildWiredStack(dsn, store, seedPath, wiring, nil, nil, nil)
		if err != nil {
			t.Fatalf("buildWiredStack(%s): %v", filepath.Base(seedPath), err)
		}
		t.Cleanup(func() { _ = stack.Close() })
		return stack
	}

	empty := model.WiringSet{}
	pushed := declaredReleasedWiring(time.Now().UTC())

	a := build(t, fileA, empty)
	localChanged := build(t, fileB, empty)
	sharedChanged := build(t, fileA, pushed)

	// A LOCAL change moves only the local half.
	if localChanged.wiringDigest != a.wiringDigest {
		t.Errorf("the SHARED digest moved (%q -> %q) for a change to the local file alone. It has to stay a "+
			"digest of the five tables as a push wrote them, or it matches neither a push nor "+
			"`aperture wiring diff`", a.wiringDigest, localChanged.wiringDigest)
	}
	if localChanged.localDigest == a.localDigest {
		t.Errorf("the LOCAL digest did not move for a changed local file: both report %q", a.localDigest)
	}

	// A SHARED change moves only the shared half.
	if sharedChanged.localDigest != a.localDigest {
		t.Errorf("the LOCAL digest moved (%q -> %q) for a shared push alone. It must describe this host's "+
			"document, not the wiring it was layered with", a.localDigest, sharedChanged.localDigest)
	}
	if sharedChanged.wiringDigest == a.wiringDigest {
		t.Errorf("the SHARED digest did not move for a pushed field_types: row: both report %q", a.wiringDigest)
	}
}

// TestAnAbsentLocalDocumentReportsNoLocalDigest pins the state the whole
// shared-wiring feature exists to enable: `aperture serve --store postgres://…`
// with no --seed, whose wiring comes from the database and whose local document
// does not exist.
//
// "" is an ANSWER there and not a missing value, and it is the same answer a
// document that declares nothing gives. The two are deliberately not
// distinguished: they contribute the same nothing to what this instance decides,
// and telling them apart here would mean re-deriving seedDocument's rule (no
// --seed plus a durable --store) in a second place that could drift from it.
func TestAnAbsentLocalDocumentReportsNoLocalDigest(t *testing.T) {
	if got, err := localWiringDigest(nil); err != nil || got != "" {
		t.Errorf("localWiringDigest(nil) = %q, %v; want \"\", nil", got, err)
	}
	if got, err := localWiringDigest(&seed.Document{}); err != nil || got != "" {
		t.Errorf("localWiringDigest(an empty document) = %q, %v; want \"\", nil — a document that declares "+
			"nothing decides nothing, which is the same answer as having none", got, err)
	}

	// And through the real builder, on the real path: a durable store with no
	// --seed has no local wiring at all (seedDocument's asymmetry).
	ctx := context.Background()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "noseed.db")
	store, err := buildStore(ctx, dsn, "")
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	stack, err := buildWiredStack(dsn, store, "", model.WiringSet{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildWiredStack with no --seed: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close() })

	if stack.localDigest != "" {
		t.Errorf("a durable store with no --seed reported local digest %q, want empty: there is no local "+
			"document to digest, and inventing a value for one would make every such instance look "+
			"different from every other", stack.localDigest)
	}
	if stack.wiringDigest == "" {
		t.Error("the SHARED digest is empty for an empty wiring set. An empty set has an ordinary digest " +
			"like any other, which is what makes the FIRST ever push to a database detectable")
	}
	if got := stack.digests(); got.Shared != stack.wiringDigest || got.Local != "" {
		t.Errorf("stack.digests() = %+v, want the pair the stack was built from", got)
	}
}

// TestTheLocalDigestIsAContentDigestAndNotAFileDigest is the "nothing volatile"
// half, asserted where it is cheap to assert: the same content, spelled
// differently, in a different file, digests the same.
//
// Reformatting is the real-world case — a redeploy that regenerates a document
// from a template, re-indents it or drops its comments — and a digest that moved
// for it would report drift across a fleet whose instances decide identically. The
// value digests the PARSED document (seed.Parse normalises YAML through
// encoding/json on the way in), so this holds by construction rather than by
// accident.
func TestTheLocalDigestIsAContentDigestAndNotAFileDigest(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.yaml")
	writeSwapSeed(t, plain, "eng")

	raw, err := os.ReadFile(plain)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Same document, re-spelled: a comment, a blank line, and a flow mapping written
	// out in block form. Nothing a decision can see has changed.
	reformatted := filepath.Join(dir, "reformatted.yaml")
	body := "# regenerated by the deploy pipeline\n\n" + strings.Replace(string(raw),
		"  - {id: acme, name: Acme Corp}",
		"  - id: acme\n    name: Acme Corp",
		1)
	if body == "# regenerated by the deploy pipeline\n\n"+string(raw) {
		t.Fatal("the fixture no longer contains the flow mapping this case re-spells")
	}
	if err := os.WriteFile(reformatted, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	digestOf := func(t *testing.T, path string) string {
		t.Helper()
		doc, err := seed.ParseFile(path)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", filepath.Base(path), err)
		}
		d, err := localWiringDigest(doc)
		if err != nil {
			t.Fatalf("localWiringDigest(%s): %v", filepath.Base(path), err)
		}
		return d
	}

	if a, b := digestOf(t, plain), digestOf(t, reformatted); a != b {
		t.Errorf("a reformatted but equivalent document digests differently (%q vs %q). The digest is taken "+
			"over the PARSED value for exactly this reason: a value that moved on re-indentation would "+
			"report drift between instances that decide identically", a, b)
	}
}

// TestTheLocalDigestCoversEverySectionOfTheDocument is the coverage half, and it is
// written by reflection-free enumeration on purpose: each section is varied ALONE
// and must move the digest.
//
// A digest that missed a section is a difference no sweep ever reports, which is
// the silently divergent fleet this feature exists to expose. The two LOCAL
// sections (objects:, attributes:) are the ones the shared digest can never cover;
// the others are covered because on a deployment that has pushed nothing at all
// the local file IS the wiring, and a fleet whose files disagree about a
// providers: statement decides differently with the same shared digest.
func TestTheLocalDigestCoversEverySectionOfTheDocument(t *testing.T) {
	base := func() *seed.Document {
		return &seed.Document{
			Accounts:   []seed.Account{{ID: "acme", Name: "Acme"}},
			Objects:    []seed.Object{{ID: "account:acme/document:42", Metadata: json.RawMessage(`{"released":"yes"}`)}},
			Attributes: []seed.Attribute{{Subject: "user", ID: "alice", Metadata: json.RawMessage(`{"department":"eng"}`)}},
			FieldTypes: []seed.FieldType{{ObjectType: "document", Fields: map[string]string{"released": "datetime"}}},
		}
	}
	baseline, err := localWiringDigest(base())
	if err != nil {
		t.Fatalf("localWiringDigest: %v", err)
	}
	if again, _ := localWiringDigest(base()); again != baseline {
		t.Fatalf("the same document digested twice gave %q and %q: the value is not stable", baseline, again)
	}

	for _, tc := range []struct {
		what   string
		change func(*seed.Document)
	}{
		{"an inline objects: metadata value — a LOCAL section the shared digest can never cover",
			func(d *seed.Document) { d.Objects[0].Metadata = json.RawMessage(`{"released":"no"}`) }},
		{"an inline attributes: bag — the other LOCAL section, and the one a rule reads as principal.*",
			func(d *seed.Document) { d.Attributes[0].Metadata = json.RawMessage(`{"department":"legal"}`) }},
		{"a field_types: declaration, which CANONICALISES what a rule compares against",
			func(d *seed.Document) { d.FieldTypes[0].Fields["released"] = "date" }},
		{"a providers: entry, which on a file-wired deployment IS this instance's wiring",
			func(d *seed.Document) {
				d.Providers = []seed.Provider{{ObjectType: "document", Kind: "csv", Path: "documents.csv"}}
			}},
		{"an attribute_providers: entry, whose ttl: is a revocation window",
			func(d *seed.Document) {
				d.AttributeProviders = []seed.AttributeProvider{{Subject: "user", Kind: "csv", Path: "users.csv"}}
			}},
		{"a connections: entry, whose NAME is the half a push shares",
			func(d *seed.Document) {
				d.Connections = map[string]seed.Connection{"main": {DSNEnv: "APERTURE_CONNECTION_MAIN_DSN"}}
			}},
		{"a model section, which --seed APPLIES to the shared store at boot",
			func(d *seed.Document) { d.Accounts[0].Name = "Acme Corporation" }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			doc := base()
			tc.change(doc)
			got, err := localWiringDigest(doc)
			if err != nil {
				t.Fatalf("localWiringDigest: %v", err)
			}
			if got == baseline {
				t.Errorf("changing %s did not move the local digest. A section outside the digest is a "+
					"difference no fleet sweep ever reports, which is the silently divergent "+
					"deployment this value exists to expose", tc.what)
			}
		})
	}
}

// TestTheLocalDigestLeaksNothing is the disclosure check. The document is host
// configuration — object ids, attribute keys and their values, a connection's
// dsn_env variable NAME — and the only thing that may ever leave is the hash.
func TestTheLocalDigestLeaksNothing(t *testing.T) {
	doc := &seed.Document{
		Objects:     []seed.Object{{ID: "account:acme/document:42", Metadata: json.RawMessage(`{"owner":"alice"}`)}},
		Attributes:  []seed.Attribute{{Subject: "user", ID: "alice", Metadata: json.RawMessage(`{"clearance":"top-secret"}`)}},
		Connections: map[string]seed.Connection{"main": {DSNEnv: "APERTURE_CONNECTION_MAIN_DSN"}},
	}
	got, err := localWiringDigest(doc)
	if err != nil {
		t.Fatalf("localWiringDigest: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("the local digest is %d characters (%q), want 64 hex characters of SHA-256: the value is a "+
			"hash and never a projection of the document", len(got), got)
	}
	for _, leak := range []string{"acme", "alice", "document:42", "clearance", "top-secret", "owner",
		"main", "APERTURE_CONNECTION_MAIN_DSN"} {
		if strings.Contains(got, leak) {
			t.Errorf("the local digest contains %q: %s", leak, got)
		}
	}
}

// TestAFailedAdoptionAdvancesNeitherDigest is last-good for the pair, through the
// real poller.
//
// The local file is rewritten AND a push arrives that this instance cannot adopt (a
// connection name it cannot renegotiate). Nothing is installed, so neither digest
// may advance — and the local one in particular must not, because the new file has
// not been adopted: the version still running was built from the old one.
func TestAFailedAdoptionAdvancesNeitherDigest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	routeSharedMain(t)
	probe := newSwapProbeWiredWith(t, ctx, "eng", swapSeed, mainOnlyWiring(time.Now().UTC()))
	probe.poll.health = service.NewWiringHealth(time.Hour, probe.live.current().digests(), nil)

	before := probe.poll.digests
	writeSwapSeed(t, probe.seedPath, "legal")

	// A push that adds a connection name is refused WHOLE, before anything is built.
	if err := probe.store.ReplaceWiring(ctx, withConnections(time.Now().UTC(), "main", "replica")); err != nil {
		t.Fatalf("pushing the connection change: %v", err)
	}
	if probe.poll.tick(ctx) {
		t.Fatal("a push that changes the connection NAME SET was adopted")
	}

	if got := probe.poll.digests; got != before {
		t.Errorf("a refused adoption moved a digest: %+v -> %+v. Nothing was installed, so this instance is "+
			"still deciding from the wiring AND the document it had — and the local file it re-read on "+
			"the way to the refusal was never adopted", before, got)
	}
	p := probe.poll.health.Posture()
	if !p.Stale {
		t.Fatal("a refused adoption did not record the alarm")
	}
	if p.Digest != before.Shared || p.LocalDigest != before.Local {
		t.Errorf("the posture reports %q / %q after a refused adoption, want the last-good pair %q / %q",
			p.Digest, p.LocalDigest, before.Shared, before.Local)
	}
}

// TestTheBootSeedsTheRecorderAndThePollerFromOnePair is the wiring-level property
// behind "the two cannot get out of step".
//
// serve derives the boot pair ONCE (stack.digests()) and hands the same value to
// service.NewWiringHealth and to startWiringPoll. A recorder seeded with one half
// and a poller baselined on the other would report a pair no version was built
// from, from the first tick onwards.
func TestTheBootSeedsTheRecorderAndThePollerFromOnePair(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "boot.yaml")
	writeSwapSeed(t, seedPath, "eng")
	dsn := "file:" + filepath.Join(dir, "boot.db")

	store, err := buildStore(ctx, dsn, seedPath)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var stack decisionStack
	cmd := &ucli.Command{
		Name:  "probe",
		Flags: storeFlags(),
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			var err error
			stack, err = buildDecisionStack(ctx, cmd, store, seedPath)
			return err
		},
	}
	if err := cmd.Run(ctx, []string{"probe", "--store", dsn, "--seed", seedPath}); err != nil {
		t.Fatalf("booting: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close() })

	pair := stack.digests()
	health := service.NewWiringHealth(time.Hour, pair, nil)
	poll := startWiringPoll(ctx, store, time.Hour, pair, noWiringSwap, nil, health)
	if poll == nil {
		t.Fatal("an hourly interval started no poller")
	}
	t.Cleanup(func() { _ = poll.Close() })

	if poll.digests != pair {
		t.Errorf("the poller is baselined on %+v, want the boot's pair %+v", poll.digests, pair)
	}
	p := health.Posture()
	if p.Digest != pair.Shared || p.LocalDigest != pair.Local {
		t.Errorf("the recorder was seeded with %q / %q, want the boot's pair %q / %q",
			p.Digest, p.LocalDigest, pair.Shared, pair.Local)
	}
	if p.LocalDigest == "" {
		t.Error("the boot seeded the recorder with no local digest although the --seed file is populated")
	}
}
