package cli

import (
	"slices"
	"testing"

	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
)

// E3-S3, boot side: where a booting instance finds the declared key sets.
//
// Two sources, one precedence, and the DB path reads the ROWS rather than the
// projected document — which is the half that fails silently, because a projection
// that drops the set leaves enforcement switched off on exactly the deployments
// that pushed a declaration.

func keysPtr(keys ...string) *[]string {
	out := append([]string{}, keys...)
	return &out
}

func TestADeclaredSetIsReadFromTheLocalSeedWhenNoWiringIsPushed(t *testing.T) {
	local := &seed.Document{AttributeProviders: []seed.AttributeProvider{
		{Subject: "user", Kind: "csv", DeclaredKeys: keysPtr("department", "clearance")},
		{Subject: "machine", Kind: "csv"}, // no declared_keys: — opted out
		{Subject: "account", Kind: "csv", DeclaredKeys: keysPtr()},
	}}
	got := declaredAttributeKeySets(model.WiringSet{}, local)

	user := got[provider.AttributeSlotUser]
	if !user.Declared || !slices.Equal(user.Keys, []string{"department", "clearance"}) {
		t.Errorf("user slot = %+v, want declared [department clearance]", user)
	}
	if machine := got[provider.AttributeSlotMachine]; machine.Declared {
		t.Errorf("machine slot = %+v, want NOT declared (no declared_keys: key)", machine)
	}
	// `declared_keys: []` is DECLARED EMPTY, not opted out. Collapsing the two here
	// would silently un-enforce a slot the operator asked to lock down.
	acct := got[provider.AttributeSlotAccount]
	if !acct.Declared || len(acct.Keys) != 0 {
		t.Errorf("account slot = %+v, want declared with no keys", acct)
	}
}

func TestTheWiringRowsWinTheDeclaredSet(t *testing.T) {
	// The DB-wired boot projects its rows onto a Document that does NOT carry the
	// declared set (wiringSeedAttributeProvider), so the rows have to be read
	// directly. A collector that went through the projected document would return
	// "not declared" here and enforce nothing.
	local := &seed.Document{AttributeProviders: []seed.AttributeProvider{
		// A slot the database never declared: the local file may ADD one.
		{Subject: "account", Kind: "csv", DeclaredKeys: keysPtr("plan")},
	}}
	wiring := model.WiringSet{AttributeProviders: []model.WiringAttributeProvider{
		{Subject: "user", Kind: "sql", DeclaredKeys: model.DeclaredKeys{Declared: true, Keys: []string{"department"}}},
		{Subject: "machine", Kind: "sql"},
	}}
	got := declaredAttributeKeySets(wiring, local)

	if user := got[provider.AttributeSlotUser]; !user.Declared || !slices.Equal(user.Keys, []string{"department"}) {
		t.Errorf("user slot = %+v, want the database's declared [department]", user)
	}
	if machine := got[provider.AttributeSlotMachine]; machine.Declared {
		t.Errorf("machine slot = %+v, want NOT declared", machine)
	}
	if acct := got[provider.AttributeSlotAccount]; !acct.Declared || !slices.Equal(acct.Keys, []string{"plan"}) {
		t.Errorf("account slot = %+v, want the local file's added [plan]", acct)
	}
}

func TestNothingDeclaredIsTheDefault(t *testing.T) {
	// The backward-compatibility bar at the boot: a seed with attribute providers
	// but no declared_keys: anywhere declares nothing, so no slot is enforced.
	local := &seed.Document{AttributeProviders: []seed.AttributeProvider{
		{Subject: "user", Kind: "csv"},
		{Subject: "account", Kind: "csv"},
	}}
	for slot, set := range declaredAttributeKeySets(model.WiringSet{}, local) {
		if set.Declared {
			t.Errorf("slot %s declares a set with no declared_keys: anywhere", slot)
		}
	}
	if got := declaredAttributeKeySets(model.WiringSet{}, nil); len(got) != 0 {
		t.Errorf("no document at all = %v, want an empty map", got)
	}
}

func TestADeclaredSetIsNormalisedRatherThanRefusedAtBoot(t *testing.T) {
	// `aperture wiring push` refuses an empty or repeated key, naming the slot and
	// the key, which is where a person wrote it. Refusing them again on a BOOT would
	// take an instance down over a malformation that cannot change a verdict: an
	// empty name is not a legal rule variable segment, so no rule can read it, and a
	// set has no use for a name twice.
	local := &seed.Document{AttributeProviders: []seed.AttributeProvider{
		{Subject: " user ", Kind: "csv", DeclaredKeys: keysPtr(" department ", "", "department", "clearance")},
		{Subject: "nonsense", Kind: "csv", DeclaredKeys: keysPtr("x")},
	}}
	got := declaredAttributeKeySets(model.WiringSet{}, local)
	user := got[provider.AttributeSlotUser]
	if !user.Declared || !slices.Equal(user.Keys, []string{"department", "clearance"}) {
		t.Errorf("user slot = %+v, want declared [department clearance]", user)
	}
	if len(got) != 1 {
		t.Errorf("collected %d slots, want 1 — an unknown subject is not a slot", len(got))
	}
}
