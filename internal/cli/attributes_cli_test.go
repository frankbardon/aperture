package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

// E5-S3, driven through the real command tree.
//
// The seed mixes all three source states on purpose, because the listing's job
// is to tell them apart:
//
//	user     an attribute_providers: entry, kind: csv, with its own ttl/max_size
//	account  the inline attributes: block (registered with ttl 0 — inline data
//	         cannot change under a running process)
//	machine  declared nowhere at all
//
// alice holds system-admin authority in acme; mallory holds none. Both are
// authenticated, so every refusal below is the GATE refusing, never a missing
// --principal.
const attributeAdminSeed = `
accounts:
  - {id: acme, name: Acme Corp}
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice}
  - {id: mallory, kind: user, identity: "user:mallory", display_name: Mallory}
memberships:
  - {principal: alice, account: acme}
  - {principal: mallory, account: acme}
object_types:
  - name: system
    description: The reserved type admin authority is modelled on.
    actions: [aperture.admin]
permissions:
  - id: perm-admin
    object_type: system
    action: aperture.admin
    description: Administer the deployment.
grants:
  - id: g-alice-admin
    account: "*"
    subject: {kind: principal, id: alice}
    permission: perm-admin
    object: "**"
    effect: allow
attribute_providers:
  - {subject: user, kind: csv, path: users.csv, ttl: 5m, max_size: 42}
attributes:
  - subject: account
    id: acme
    metadata: {plan: enterprise}
`

const attributeAdminUsers = `id,department,clearance:int
alice,eng,5
mallory,sales,1
`

// writeAttributeAdminSeed materialises the seed and the CSV it names, side by
// side, and returns the seed's path.
func writeAttributeAdminSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"admin.yaml": attributeAdminSeed,
		"users.csv":  attributeAdminUsers,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "admin.yaml")
}

// runAttributesCLI runs `aperture attributes <sub> --seed <path> ...` through
// the real command tree. --seed is spliced after the SUBCOMMAND, because the
// attribute flags are declared on the subcommands and not on their parent.
func runAttributesCLI(t *testing.T, seedPath, sub string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	argv := append([]string{"aperture", "attributes", sub, "--seed", seedPath}, args...)
	err := app.Run(context.Background(), argv)
	return out.String(), err
}

// TestAttributeSlotsListsEveryLayerAndItsOwnWindow is the operator's first question:
// what is this deployment actually running, where did each piece of it come from, and
// how long can a revoked bag keep authorizing?
//
// It is a row per LAYER rather than a row per slot. The single-row listing read
// CacheConfigFor, which reports the GOVERNING layer's configuration — so a slot with a
// shared directory on a five-minute window and an inline block that never expires
// printed `ttl 5m` and no hint that a second cache existed. An operator who has just
// revoked something reads 5m and believes the window is closed; for every key only the
// local layer serves it is unbounded. A slot's ttl: is a REVOCATION WINDOW, so one
// number per slot is not a simplification.
func TestAttributeSlotsListsEveryLayerAndItsOwnWindow(t *testing.T) {
	seedPath := writeAttributeAdminSeed(t)

	// No actor: the listing reads back the seed the caller just passed and the
	// cache configuration this process built from it, so requiring system-admin
	// authority would only stop an operator diagnosing their own wiring.
	out, err := runAttributesCLI(t, seedPath, "slots")
	if err != nil {
		t.Fatalf("attributes slots: %v\n%s", err, out)
	}

	rows := attributeSlotRows(t, out)
	for _, tc := range []struct {
		key  string
		want string
	}{
		// The declared ttl: and max_size: are read back off the REGISTRY, so this
		// row is proof the per-slot overrides actually took rather than proof the
		// YAML was re-printed. The source names the PLACE as well as the kind: this
		// entry is in the operator's own file, not in a database.
		{"user/shared", "csv (--seed file) 5m0s 42 0"},
		// Unwired is not an empty directory: it is a party this deployment
		// declared no source for.
		{"machine/-", "(unwired) - - -"},
		// Inline bags are fixed for the life of the process, so their window is
		// "never" — nothing can change underneath them — and they are the LOCAL
		// layer.
		{"account/local", "inline (--seed file) never 10000 0"},
	} {
		if got := rows[tc.key]; got != tc.want {
			t.Errorf("slots row %q = %q, want %q\nfull output:\n%s", tc.key, got, tc.want, out)
		}
	}
}

// attributeSlotRows indexes a `slots` listing by "<slot>/<layer>", which is the row
// key now that a slot can occupy more than one row.
func attributeSlotRows(t *testing.T, out string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	header := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "slot" {
			header = true
			continue
		}
		rows[fields[0]+"/"+fields[1]] = strings.Join(fields[2:], " ")
	}
	if !header {
		t.Fatalf("no header row in:\n%s", out)
	}
	return rows
}

// TestAttributeSlotsNamesTheDatabaseThatWiredASlot is the source column's whole
// point, on the one command whose job is "is the user slot wired, and from where?".
//
// A slot wired from the SHARED wiring used to print `(host)` — the label reserved for
// "a Go host wired this in Go" — because the source was derived from the LOCAL
// document alone. It pointed the operator away from the database that actually wired
// it, and the ttl column beside it was read off the registry and therefore correct,
// which made the source column the only wrong cell and so the believable one.
func TestAttributeSlotsNamesTheDatabaseThatWiredASlot(t *testing.T) {
	// A durable store with wiring rows and, deliberately, NO --seed: the deployment
	// shape the shared wiring exists for, where the local document has nothing to
	// say about any slot at all.
	dsn := "file:" + filepath.Join(t.TempDir(), "wired.db")
	pushWiring(t, dsn, sharedWiringSet(time.Now().UTC()))
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	out, err := runAttributeSlotsCLI(t, "", dsn)
	if err != nil {
		t.Fatalf("attributes slots over a DB-wired instance: %v\n%s", err, out)
	}
	rows := attributeSlotRows(t, out)
	if got, want := rows["user/shared"], "sql (shared wiring)"; !strings.HasPrefix(got, want) {
		t.Fatalf("the user slot's shared layer reads %q, want it to start %q — a slot wired "+
			"from the database must not be labelled (host), which means \"a Go host wired "+
			"this in Go\" and sends the operator looking in the wrong place\nfull output:\n%s",
			got, want, out)
	}
}

// TestAttributeSlotsReportsBothLayersOfAContestedSlot is the ttl half of the same
// listing, and the one CLAUDE.md names as the regression: "TWO CACHES, NOT ONE,
// because a layer's ttl: is its own revocation window … documenting the ttl as one
// number per slot is the regression".
//
// The shared row declares a 5m window and the inline block never expires. A listing
// that reported one number would show 5m — the governing layer's — while every key
// only the inline block serves stays cached forever.
func TestAttributeSlotsReportsBothLayersOfAContestedSlot(t *testing.T) {
	set := sharedWiringSet(time.Now().UTC())
	set.AttributeProviders[0].TTL = "5m"
	dsn := "file:" + filepath.Join(t.TempDir(), "two-layers.db")
	pushWiring(t, dsn, set)
	t.Setenv(connectionDSNEnvVar("main"), unroutedDSN)

	// The same slot, filled inline as well: the newly legal layering.
	seedPath := writeSeed(t, "layered.yaml", `
attributes:
  - subject: user
    id: alice
    metadata: {cost_centre: cc-42}
`)

	out, err := runAttributeSlotsCLI(t, seedPath, dsn)
	if err != nil {
		t.Fatalf("attributes slots over a layered slot: %v\n%s", err, out)
	}
	rows := attributeSlotRows(t, out)
	if got, want := rows["user/shared"], "sql (shared wiring) 5m0s"; !strings.HasPrefix(got, want) {
		t.Errorf("the shared layer reads %q, want it to start %q\nfull output:\n%s", got, want, out)
	}
	if got, want := rows["user/local"], "inline (--seed file) never"; !strings.HasPrefix(got, want) {
		t.Fatalf("the LOCAL layer reads %q, want it to start %q. Both layers must be listed "+
			"with their OWN window: a slot's ttl: is a revocation window, and reporting only "+
			"the governing layer's tells an operator who has just revoked something that the "+
			"window is closed while every key the inline block serves stays cached forever"+
			"\nfull output:\n%s", got, want, out)
	}
	// The cached counter is the registry's and it is per SLOT, summed over layers, so
	// it is printed once rather than twice as though each layer held it.
	if !strings.HasSuffix(rows["user/local"], " -") {
		t.Errorf("the second layer's cached cell = %q, want \"-\": there is no per-layer "+
			"Stats, and printing the slot total on both rows would double-count it",
			rows["user/local"])
	}
}

// runAttributeSlotsCLI runs `aperture attributes slots` with an explicit --store,
// which runAttributesCLI does not take: every case above needs a durable store,
// because that is where shared wiring lives.
func runAttributeSlotsCLI(t *testing.T, seedPath, storeDSN string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	argv := []string{"aperture", "attributes", "slots", "--store", storeDSN}
	if seedPath != "" {
		argv = append(argv, "--seed", seedPath)
	}
	err := app.Run(context.Background(), argv)
	return out.String(), err
}

// TestQueryingASlotIsGatedFromTheCLI is the warning E5-S2 left for this story: a
// one-shot command wires no gate, so a facade built the default way reports
// APERTURE_UNIMPLEMENTED for every caller. The command builds the gated facade,
// and the two halves of that are both asserted here — the admin gets rows, and
// the non-admin gets APERTURE_AUTHZ_DENIED rather than "not implemented".
func TestQueryingASlotIsGatedFromTheCLI(t *testing.T) {
	seedPath := writeAttributeAdminSeed(t)

	out, err := runAttributesCLI(t, seedPath, "query", "user", "--principal", "alice", "--account", "acme")
	if err != nil {
		t.Fatalf("admin query: %v\n%s", err, out)
	}
	var recs []struct {
		ID         string         `json:"id"`
		Attributes map[string]any `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("query output is not a JSON array: %v\n%s", err, out)
	}
	if len(recs) != 2 {
		t.Fatalf("admin query returned %d record(s), want 2:\n%s", len(recs), out)
	}
	for _, rec := range recs {
		if rec.Attributes["department"] == nil {
			t.Errorf("record %q carries no department; the CSV bag did not reach the CLI: %+v", rec.ID, rec.Attributes)
		}
	}

	out, err = runAttributesCLI(t, seedPath, "query", "user", "--principal", "mallory", "--account", "acme")
	if aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Fatalf("non-admin query = %v, want APERTURE_AUTHZ_DENIED (APERTURE_UNIMPLEMENTED means the command wired no gate)", err)
	}
	// A refusal returns NOTHING: no partial page, no count, and no directory
	// content anywhere in what was printed.
	if strings.Contains(out, "mallory") || strings.Contains(out, "sales") || strings.Contains(out, "alice") {
		t.Errorf("a refused query printed directory content:\n%s", out)
	}

	if _, err := runAttributesCLI(t, seedPath, "query", "user"); aerr.CodeOf(err) != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("query with no --principal = %v, want APERTURE_UNAUTHENTICATED", err)
	}
}

// TestQueryingASlotFiltersOnAttributes: the enumerate predicate, applied to
// bags. Typed equality is the part worth pinning — clearance is an int column,
// so the string "5" must not match it and --fields-json must.
func TestQueryingASlotFiltersOnAttributes(t *testing.T) {
	seedPath := writeAttributeAdminSeed(t)
	admin := []string{"--principal", "alice", "--account", "acme"}

	count := func(t *testing.T, args ...string) int {
		t.Helper()
		out, err := runAttributesCLI(t, seedPath, "query", append([]string{"user"}, append(admin, args...)...)...)
		if err != nil {
			t.Fatalf("query %v: %v\n%s", args, err, out)
		}
		var recs []json.RawMessage
		if err := json.Unmarshal([]byte(out), &recs); err != nil {
			t.Fatalf("query output is not a JSON array: %v\n%s", err, out)
		}
		return len(recs)
	}

	if got := count(t, "--field", "department=eng"); got != 1 {
		t.Errorf("--field department=eng matched %d record(s), want 1", got)
	}
	if got := count(t, "--field", "clearance=5"); got != 0 {
		t.Errorf("--field clearance=5 matched %d record(s); the string \"5\" must never match the number 5", got)
	}
	if got := count(t, "--fields-json", `{"clearance":5}`); got != 1 {
		t.Errorf("--fields-json {\"clearance\":5} matched %d record(s), want 1", got)
	}
}

// TestInvalidatingIsGatedAndReportsWhatItDid covers the three forms plus the
// refusal. The cache is cold in a one-shot invocation — that is stated in the
// command's help and asserted here rather than left as a surprise.
func TestInvalidatingIsGatedAndReportsWhatItDid(t *testing.T) {
	seedPath := writeAttributeAdminSeed(t)
	admin := []string{"--principal", "alice", "--account", "acme"}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"one subject in one slot", append([]string{"user", "--id", "alice"}, admin...), `no cached user bag for "alice"`},
		{"a whole slot", append([]string{"user"}, admin...), "cleared the user slot's cache"},
		{"everything", append([]string{"--all"}, admin...), "cleared every attribute slot's cache"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runAttributesCLI(t, seedPath, "invalidate", tc.args...)
			if err != nil {
				t.Fatalf("invalidate: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("invalidate printed %q, want it to contain %q", strings.TrimSpace(out), tc.want)
			}
		})
	}

	if _, err := runAttributesCLI(t, seedPath, "invalidate", "user", "--principal", "mallory", "--account", "acme"); aerr.CodeOf(err) != aerr.APERTURE_AUTHZ_DENIED {
		t.Errorf("non-admin invalidate = %v, want APERTURE_AUTHZ_DENIED", err)
	}
	if _, err := runAttributesCLI(t, seedPath, "invalidate", "user"); aerr.CodeOf(err) != aerr.APERTURE_UNAUTHENTICATED {
		t.Errorf("invalidate with no --principal = %v, want APERTURE_UNAUTHENTICATED", err)
	}
}

// TestInvalidateRefusesAnAmbiguousForm: --all plus a slot has two plausible
// readings, and guessing at the broader one would clear caches nobody asked to
// clear. Refused, with the remedy in the message.
func TestInvalidateRefusesAnAmbiguousForm(t *testing.T) {
	seedPath := writeAttributeAdminSeed(t)
	admin := []string{"--principal", "alice", "--account", "acme"}

	cases := []struct {
		name string
		args []string
	}{
		{"--all with a slot", append([]string{"--all", "user"}, admin...)},
		{"--all with an id", append([]string{"--all", "--id", "alice"}, admin...)},
		{"neither a slot nor --all", admin},
		{"two slots", append([]string{"user", "machine"}, admin...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runAttributesCLI(t, seedPath, "invalidate", tc.args...)
			if aerr.CodeOf(err) != aerr.APERTURE_INVALID_INPUT {
				t.Fatalf("invalidate %v = %v, want APERTURE_INVALID_INPUT\n%s", tc.args, err, out)
			}
		})
	}
}

// TestTheAttributeCommandHelpStatesTheSecurityProperty is an acceptance
// criterion, not decoration. The staleness window is the interval a revoked
// clearance keeps authorizing, and an operator who reads the help as a tuning
// note tunes it for fetch traffic. The wording is asserted so it cannot be
// edited down to "cache TTL" by someone who does not know that.
func TestTheAttributeCommandHelpStatesTheSecurityProperty(t *testing.T) {
	var cmd = attributesCommand()

	help := cmd.Description
	for _, sub := range cmd.Commands {
		help += "\n" + sub.Usage + "\n" + sub.Description
	}
	lower := strings.ToLower(help)
	for _, want := range []string{
		"security property",
		"security control, not a performance knob",
		"revoke",
		"already taken away",
		"system-admin",
	} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("the attributes help never says %q; the staleness window must be documented as a SECURITY property, not only as a tuning note", want)
		}
	}
}
