package seed

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
)

// writeCSV drops content into dir under name and returns dir, so a fixture can
// be built the way a deployment is: a seed file and the files it names, side by
// side, resolved relative to the seed's directory.
func writeCSV(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return dir
}

// TestAttributeProviders_CSVServesASlot is the kind: csv arm end to end at the
// seed layer: a declaration, a real file beside it, and a registry that answers
// the fetch the decision path makes.
//
// The id column holds BARE subject ids — "alice", not "user:alice" — which is
// the one asymmetry with an object provider's file, and the fixture is written
// the correct way precisely so the assertion means something: an
// identity-shaped file would load just as happily and answer nothing.
func TestAttributeProviders_CSVServesASlot(t *testing.T) {
	ctx := context.Background()
	dir := writeCSV(t, "users.csv", strings.Join([]string{
		"id,department,clearance:int,teams:list,hired_at:date",
		"alice,eng,3,platform|oncall,2024-03-04",
		"bob,sales,1,crm,2023-07-01",
		"",
	}, "\n"))
	doc := attributeDoc(t, `
attribute_providers:
  - subject: user
    kind: csv
    path: users.csv
`)
	reg, err := doc.BuildAttributeRegistry(dir)
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}

	// The decision path's fetch, through the principal resolver seam.
	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	// The value model is the shared one, so the typed columns arrive typed: an
	// untyped clearance would decide differently against `principal.clearance >= 3`.
	if bag["department"] != "eng" || bag["clearance"] != int64(3) || bag["hired_at"] != "2024-03-04" {
		t.Fatalf("bag = %#v; want the typed values the column suffixes declare", bag)
	}
	if !reflect.DeepEqual(bag["teams"], []any{"platform", "oncall"}) {
		t.Errorf("teams = %#v; want a real []any", bag["teams"])
	}

	// An unknown subject is not a failed decision — it decides against the floor.
	if bag, err := reg.Attributes(ctx, "user", "mallory"); err != nil || bag != nil {
		t.Errorf("unknown subject = %#v, %v; want a nil bag and no error", bag, err)
	}

	// The admin read enumerates the file, filtered by the Fields contract.
	recs, err := reg.Enumerate(ctx, provider.AttributeSlotUser,
		provider.AttributeFilter{Fields: map[string]any{"teams": "crm"}})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "bob" {
		t.Fatalf("Enumerate = %+v; want bob alone", recs)
	}
}

// TestAttributeProviders_CSVCacheOptionsReachTheSlot pins the wiring the block
// resolves and the loader must not swallow: ttl: and max_size: are per-SLOT,
// because the three slots have genuinely different change rates and
// cardinalities, and a declaration that never reached the cache would be a
// silently ignored knob.
func TestAttributeProviders_CSVCacheOptionsReachTheSlot(t *testing.T) {
	dir := writeCSV(t, "users.csv", "id,department\nalice,eng\n")
	doc := attributeDoc(t, `
attribute_providers:
  - subject: user
    kind: csv
    path: users.csv
    ttl: 45s
    max_size: 7
`)
	reg, err := doc.BuildAttributeRegistry(dir)
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	cfg, ok := reg.CacheConfigFor(provider.AttributeSlotUser)
	if !ok {
		t.Fatal("the user slot has no cache config")
	}
	if cfg.TTL != 45*time.Second || cfg.MaxSize != 7 {
		t.Fatalf("cache config = %+v; want the declared ttl 45s / max_size 7", cfg)
	}
}

// TestAttributeProviders_CSVIsReadAtBuild is where a malformed file must fail.
//
// A file read lazily would fail on the first decision that needed the slot, and
// an attribute bag is read by every rule against every object in a decision — so
// the failure is not one object type going quiet, it is every decision for that
// slot, discovered in production. Reading at build turns it into a boot error an
// operator is present for, naming the file and the row.
//
// The code passes through rather than being re-stamped: APERTURE_METADATA_INVALID
// carries the value model's fixups, and burying it under a generic config code
// would cost the operator the remedy.
func TestAttributeProviders_CSVIsReadAtBuild(t *testing.T) {
	cases := []struct {
		name string
		body string
		want aerr.Code
		// substr must appear in the RENDERED error — its message plus its
		// structured context — so the operator is told which row and which file.
		// Which of the two halves carries the line is the loader's business; that
		// it is carried at all is this test's.
		substr string
	}{
		{
			name:   "a cell that will not coerce",
			body:   "id,clearance:int\nalice,three\n",
			want:   aerr.APERTURE_CONFIG_INVALID,
			substr: "line",
		},
		{
			name:   "a bag the value model refuses",
			body:   "id,note\nalice," + strings.Repeat("x", provider.DefaultMaxValueBytes+1) + "\n",
			want:   aerr.APERTURE_METADATA_INVALID,
			substr: "line 2",
		},
		{
			name:   "the account wildcard as a key",
			body:   "id,plan\n*,enterprise\n",
			want:   aerr.APERTURE_ATTRIBUTE_PROVIDER_INVALID,
			substr: "wildcard",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCSV(t, "users.csv", tc.body)
			doc := attributeDoc(t, "attribute_providers:\n  - {subject: user, kind: csv, path: users.csv}\n")
			_, err := doc.BuildAttributeRegistry(dir)
			if err == nil {
				t.Fatal("build accepted a malformed attribute file")
			}
			if got := aerr.CodeOf(err); got != tc.want {
				t.Fatalf("code = %s; want %s (%v)", got, tc.want, err)
			}
			msg := rendered(t, err)
			if !strings.Contains(msg, tc.substr) {
				t.Errorf("error %q does not mention %q", msg, tc.substr)
			}
			if !strings.Contains(msg, "users.csv") {
				t.Errorf("error %q does not name the file", msg)
			}
		})
	}

	// A path naming no file is the same class of failure, refused at the same
	// moment.
	doc := attributeDoc(t, "attribute_providers:\n  - {subject: user, kind: csv, path: missing.csv}\n")
	if _, err := doc.BuildAttributeRegistry(t.TempDir()); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Fatalf("code = %s; want APERTURE_CONFIG_INVALID for a path naming no file", aerr.CodeOf(err))
	}
}

// rendered flattens a coded error to its message plus its structured context.
func rendered(t *testing.T, err error) string {
	t.Helper()
	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not a *aerr.CodedError: %v", err)
	}
	return ce.Error() + " " + fmt.Sprint(ce.Context)
}

// TestAttributeProviders_CSVWinsTheSlotItClaims exercises the precedence rule
// against a REAL loader rather than a stub: the external entry is the slot's
// SHARED layer and wins every key both sections serve, while the inline block
// layers under it and contributes the keys and the subjects it does not carry.
//
// It is the seed-level statement of what provider.AttributeLayer guarantees, and
// it asserts all three halves of it in one document, because each one alone would
// pass under a wrong implementation: a contested key (the shared value stands), a
// key only the local layer serves (it is still read), and a SUBJECT only the local
// layer serves (it resolves — the old rule discarded the whole slot, so it did
// not).
func TestAttributeProviders_CSVWinsTheSlotItClaims(t *testing.T) {
	ctx := context.Background()
	dir := writeCSV(t, "users.csv", "id,department\nalice,external\n")
	doc := attributeDoc(t, `
attribute_providers:
  - {subject: user, kind: csv, path: users.csv}
attributes:
  - {subject: user, id: alice, metadata: {department: inline, team: atlas}}
  - {subject: user, id: bob, metadata: {department: inline}}
  - {subject: account, id: acme, metadata: {plan: enterprise}}
`)
	reg, err := doc.BuildAttributeRegistry(dir)
	if err != nil {
		t.Fatalf("BuildAttributeRegistry: %v", err)
	}
	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	if bag["department"] != "external" {
		t.Errorf("department = %#v; want the file's value — the shared layer wins a contested key", bag["department"])
	}
	// The local layer's own key survives the merge. This is the reversal: it used
	// to be discarded with the rest of the slot.
	if bag["team"] != "atlas" {
		t.Errorf("team = %#v; want atlas — the local layer contributes keys the shared layer does not serve", bag["team"])
	}
	// bob is declared only in the local layer, and the shared layer's NOT_FOUND for
	// him is that layer having no record, not the slot having no answer.
	bobBag, err := reg.Attributes(ctx, "user", "bob")
	if err != nil {
		t.Fatalf("Attributes(user, bob): %v", err)
	}
	if bobBag["department"] != "inline" {
		t.Errorf("bob = %#v; want the local layer's bag — a subject only it serves is still resolvable", bobBag)
	}
	// The slot the file did not claim has a local layer only, and behaves exactly
	// as a single-layer slot always did.
	acct, err := reg.AccountAttributes(ctx, "acme")
	if err != nil {
		t.Fatalf("AccountAttributes(acme): %v", err)
	}
	if acct["plan"] != "enterprise" {
		t.Errorf("plan = %#v; want enterprise from the inline section", acct["plan"])
	}
	if got, want := doc.AttributeCollisions(), []string{"user"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AttributeCollisions() = %v; want %v", got, want)
	}
	// The registry reports the shape it built: two layers on the claimed slot,
	// shared first, and one on the slot only the inline block filled.
	if got, want := reg.Layers(provider.AttributeSlotUser),
		[]provider.AttributeLayer{provider.AttributeLayerShared, provider.AttributeLayerLocal}; !reflect.DeepEqual(got, want) {
		t.Errorf("Layers(user) = %v; want %v", got, want)
	}
	if got, want := reg.Layers(provider.AttributeSlotAccount),
		[]provider.AttributeLayer{provider.AttributeLayerLocal}; !reflect.DeepEqual(got, want) {
		t.Errorf("Layers(account) = %v; want %v", got, want)
	}
}
