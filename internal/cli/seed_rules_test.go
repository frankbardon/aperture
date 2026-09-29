package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

// The boot path is a rule WRITER, and the declared set's argument is that no rule
// naming an undeclared key can be saved by ANY route. These are the --seed route.

// seedWithRule writes a seed document whose account slot declares `tier` and whose
// one rule reads readPath. The account root needs only ONE declaring slot, which is
// what keeps the fixture to a single attribute_providers: entry.
//
// The csv path need not exist: this gate parses the document and never builds a
// registry, and loadSeed does not build one either.
func seedWithRule(t *testing.T, readPath string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.yaml")
	body := `
object_types:
  - name: document
    actions: [read]
attribute_providers:
  - subject: account
    kind: csv
    path: accounts.csv
    declared_keys: [tier]
rules:
  - name: gated
    ast:
      type: compare
      op: eq
      left: {type: var, name: "` + readPath + `"}
      right: {type: literal, value: "x"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// TestBootRefusesASeedRuleReadingAnUndeclaredAttributeKey is the bypass test for the
// --seed route. It must be refused, with the specific code, and the document must not
// be applied — a rule that reaches storage is in the SHARED database and so reaches
// every other instance, where refusing this instance's boot is no protection at all.
func TestBootRefusesASeedRuleReadingAnUndeclaredAttributeKey(t *testing.T) {
	ctx := context.Background()
	path := seedWithRule(t, "account.region")

	// A DURABLE store, so "nothing was applied" can actually be checked rather than
	// inferred from the call order. This is the load-bearing half: a rule that reaches
	// storage is in the shared database and so reaches every other instance, where
	// refusing THIS instance's boot would no longer be any protection.
	dsn := filepath.Join(t.TempDir(), "aperture.db")

	store, err := buildStore(ctx, dsn, path)
	if err == nil {
		_ = store.Close()
		t.Fatal("a rule reading an undeclared key booted")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE {
		t.Fatalf("code = %q, want %q (err = %v)", got, aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE, err)
	}
	// bootError would stamp APERTURE_BOOT over this and cost the operator the fixups
	// that name the key and the entry to edit. Depth is what catches that; the code
	// alone does not, because a same-code re-stamp is invisible to CodeOf.
	if n := codedDepth(err); n != 1 {
		t.Errorf("chain carries %d Aperture-coded errors, want exactly 1: %v", n, err)
	}
	for _, want := range []string{"region", "gated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}

	reopened, err := openStore(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Setup(ctx); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := reopened.GetRule(ctx, "gated"); err == nil {
		t.Error("the refused document's rule reached storage; a refused boot must apply nothing, " +
			"or the rule is in the shared database and every other instance has it")
	}
}

// TestBootAcceptsASeedRuleReadingADeclaredKey — the gate must refuse only what the
// wiring really does not promise.
func TestBootAcceptsASeedRuleReadingADeclaredKey(t *testing.T) {
	store, err := buildStore(context.Background(), "", seedWithRule(t, "account.tier"))
	if err != nil {
		t.Fatalf("a rule reading a DECLARED key was refused: %v", err)
	}
	_ = store.Close()
}

// TestBootAcceptsASeedRuleReadingTheFloor — account.id is a floor key, present with
// no provider wired and never part of a declared set. Refusing it over a declaration
// that has nothing to say about it is the regression the floor exists to prevent.
func TestBootAcceptsASeedRuleReadingTheFloor(t *testing.T) {
	store, err := buildStore(context.Background(), "", seedWithRule(t, "account.id"))
	if err != nil {
		t.Fatalf("the floor was refused: %v", err)
	}
	_ = store.Close()
}

// TestBootWithNoDeclarationAcceptsEveryRuleItUsedTo is the opt-in bar. A deployment
// that declares nothing must boot exactly what it booted before declaring existed,
// or every such deployment breaks at once.
func TestBootWithNoDeclarationAcceptsEveryRuleItUsedTo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed.yaml")
	body := `
object_types:
  - name: document
    actions: [read]
rules:
  - name: ungated
    ast:
      type: compare
      op: eq
      left: {type: var, name: "account.whatever"}
      right: {type: literal, value: "x"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	store, err := buildStore(context.Background(), "", path)
	if err != nil {
		t.Fatalf("a deployment that declares nothing refused a rule it used to accept: %v", err)
	}
	_ = store.Close()
}
