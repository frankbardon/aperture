package rules

import (
	"fmt"
	"slices"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// Declared attribute keys, and why enforcement is a VALIDATION concern.
//
// A deployment's shared wiring may declare, per attribute slot, the keys that
// slot GUARANTEES to serve (seed's `declared_keys:`, model.DeclaredKeys). An
// attribute slot holds two layers — a SHARED one from the wiring and a LOCAL one
// from this instance's own file or Go code — and the shared layer wins every key
// both serve, so the keys a local layer adds on top are extra fields on one
// machine's bag.
//
// Rules, by contrast, are MODEL state: they live in the shared database, so every
// instance evaluates the same rules. That pairing is the hazard. A rule naming a
// key only one instance's local layer serves decides on that instance and reads a
// MISSING PATH on the others, and a missing path neither denies nor errors — it
// makes every predicate over it false. An inclusive grant therefore denies and an
// EXCLUSIVE grant stops excluding, which is an over-grant, and nothing in any
// verdict, trace or note says so (rules/attribute_leniency_test.go's
// TestAMissingBagWidensAnExclusiveGrant is the same hazard one layer down).
//
// The declared key set closes it BY CONSTRUCTION rather than by convention: a
// local layer may still add keys, but no rule that names one of them can be saved,
// so the extra keys are inert. That only works if the refusal happens where the
// rule is AUTHORED. A decision-time refusal would let a bad rule ship and fail in
// production, on some instances and not others — exactly the divergence being
// removed, wearing an error message.
//
// # Opt-in, per slot
//
// A slot that declares nothing is not enforced. Nothing here fires for a
// DeclaredAttributeKeys zero value, so every rule that validated before the set
// existed still validates, and a deployment opts in one slot at a time.
//
// # The floor sits ABOVE the declared set
//
// `principal.id`, `principal.kind` and `account.id` are the engine's floor bags
// (see principalBag / accountBag): stamped LAST over the resolved bag, so they
// cannot be shadowed and are present in every deployment whether a provider is
// wired or not. They are therefore ALWAYS readable and are never part of a
// declared set — declaring them is neither required nor an error, it is merely
// redundant. Refusing `principal.id` because a slot declared only `department`
// would refuse the single most common rule in existence
// (`principal.id == object.owner`) over a wiring change that has nothing to say
// about it.

// DeclaredKeySet is one rule ROOT's declared key set: the attribute keys a
// deployment's shared wiring guarantees under that root.
//
// The zero value is NOT DECLARED, which is the opt-out, so a DeclaredKeySet a
// caller never fills enforces nothing.
//
// It is a rules-package type rather than model.DeclaredKeys because this package
// sits on the decision path and never imports model: a rule knows ROOTS, and a
// wiring row knows SLOTS. The collapse between the two — which slots back which
// root, and what it means when only some of them declare — belongs to the caller
// that can see both (service.WithDeclaredAttributeKeys), and it is stated there
// exactly once.
type DeclaredKeySet struct {
	// Declared reports whether a set was declared at all. False is the opt-out:
	// every key is readable, exactly as it was before declaring existed. It is a
	// separate bit rather than a len(Keys) test because DECLARED EMPTY — opted in,
	// permitting nothing but the floor — is a different answer from not declared,
	// and reading it off a length would turn a root that permits no key into one
	// that permits every key.
	Declared bool
	// Keys is the declared set. Order is irrelevant here (it is a set), but the
	// declaration order is preserved so a refusal message lists the keys the way
	// their author wrote them.
	Keys []string
	// Slots names the wiring slots this set was built from, for the refusal
	// message alone. It is what turns "that key is not declared" into an
	// instruction — the operator has to edit an `attribute_providers:` entry, and
	// this is which one.
	Slots []string
}

// DeclaredAttributeKeys is the per-ROOT declared key set that rule validation
// enforces against. Its zero value declares nothing and so enforces nothing.
type DeclaredAttributeKeys struct {
	// Principal governs the `principal` root — the union of the principal slots'
	// declared sets, declared only when every one of them declares (see
	// service.WithDeclaredAttributeKeys for why).
	Principal DeclaredKeySet
	// Account governs the `account` root.
	Account DeclaredKeySet
}

// Enforcing reports whether either root declares a set, i.e. whether
// CheckDeclaredAttributeKeys can refuse anything at all. It exists so a caller can
// say "this deployment has opted in" without reaching into the fields.
func (d DeclaredAttributeKeys) Enforcing() bool {
	return d.Principal.Declared || d.Account.Declared
}

// CheckDeclaredAttributeKeys refuses a rule that NAMES an attribute key a
// declaring root does not declare, with APERTURE_RULE_UNDECLARED_ATTRIBUTE naming
// the key and the slot(s) whose entry has to change. It returns nil for a rule
// that names only declared keys, for either root that declares nothing, and for
// a zero-value d.
//
// It gates on the paths the rule NAMES, which is readsBeyondFloor's notion of
// "what does this rule read" and deliberately the same walk (walkVarFields serves
// both). A key is refused whether the comparison over it would have been reached,
// short-circuited away by an `&&` to its left, or evaluated and found false,
// because in every one of those cases the rule's text depends on a field the
// deployment has not promised. That is also what makes the check decidable at all:
// it reads the AST and nothing else — no bag, no principal, no account, no
// storage.
//
// Only the FIRST path segment past the root is compared, because a declared key
// names a top-level key of a bag and the value under it may be a nested metadata
// value: a declared `metadata` permits `principal.metadata.department`.
func CheckDeclaredAttributeKeys(n *Node, d DeclaredAttributeKeys) error {
	if err := checkDeclaredRoot(n, principalRoot, d.Principal, principalKeyID, principalKeyKind); err != nil {
		return err
	}
	return checkDeclaredRoot(n, accountRoot, d.Account, accountKeyID)
}

// checkDeclaredRoot is CheckDeclaredAttributeKeys for one root. floor is that
// root's floor keys, which are always readable — the same argument
// readsBeyondFloor takes, from the same constants, so the two cannot come to
// disagree about what the floor is.
func checkDeclaredRoot(n *Node, root string, set DeclaredKeySet, floor ...string) error {
	if !set.Declared {
		return nil
	}
	var (
		offender  string
		wholeBag  bool
		refusable bool
	)
	walkVarFields(n, root, func(field string) bool {
		switch {
		case field == "":
			// A bare `principal` / `account` hands the rule the WHOLE bag, so it
			// reads whatever the bag happens to carry — including every key a local
			// layer added. Left legal, it would be the one expression that makes a
			// declared set decorative, which is why it is refused rather than
			// treated as naming nothing.
			wholeBag, refusable = true, true
			return true
		case slices.Contains(floor, field):
			return false
		case slices.Contains(set.Keys, field):
			return false
		default:
			offender, refusable = field, true
			return true
		}
	})
	if !refusable {
		return nil
	}
	if wholeBag {
		return aerr.WithContext(aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE,
			fmt.Sprintf("rule: %s reads the WHOLE attribute bag, and %s declares a key set (%s), which a whole-bag read cannot be checked against; name the individual keys the rule needs",
				root, declaredWhere(root, set), declaredList(set)),
			map[string]any{"root": root, "path": root, "slots": strings.Join(set.Slots, ", ")})
	}
	return aerr.WithContext(aerr.APERTURE_RULE_UNDECLARED_ATTRIBUTE,
		fmt.Sprintf("rule: %s.%s reads the attribute key %q, which %s does not declare (declared: %s); add it to declared_keys: on that entry, or stop reading it",
			root, offender, offender, declaredWhere(root, set), declaredList(set)),
		map[string]any{
			"root":  root,
			"path":  root + "." + offender,
			"key":   offender,
			"slots": strings.Join(set.Slots, ", "),
		})
}

// declaredWhere names the wiring the operator has to edit. It falls back to the
// root when a caller supplied no slot names, so a hand-built DeclaredKeySet in a
// test or a Go host still produces a sentence rather than a dangling "for slot ".
func declaredWhere(root string, set DeclaredKeySet) string {
	if len(set.Slots) == 0 {
		return "the wiring for " + root
	}
	return fmt.Sprintf("the wiring's attribute_providers: entry for slot %s", strings.Join(set.Slots, "/"))
}

// declaredList renders the declared set for the refusal message. A declared-EMPTY
// set has to read as something — "(none)" — or the message would trail off into a
// blank where the whole explanation is that nothing is permitted.
func declaredList(set DeclaredKeySet) string {
	if len(set.Keys) == 0 {
		return "none"
	}
	return strings.Join(set.Keys, ", ")
}

// walkVarFields visits every field the rule NAMES under root, in a stable order,
// and stops at the first visit that returns true (which it then reports).
//
// It is the ONE definition of "what does this rule read", shared by
// readsBeyondFloor (which turns it into the attributes_floor_only note) and by
// checkDeclaredRoot (which turns it into a refusal). A second walk would be a
// second answer to that question, and the two would drift in exactly the place
// where drifting is invisible: a note that fires and a refusal that does not, over
// the same expression.
//
// field is the FIRST path segment past the root, or the EMPTY STRING for a bare
// reference to the root itself — a rule handed the whole bag reads whatever is in
// it, which is a distinct fact from naming any particular key, and both callers
// need it told apart rather than dropped.
//
// The walk covers every field a Node can hang a child off — Left, Right, Children,
// Items — rather than switching on Type, so a node type that gains a child
// position cannot silently stop being scanned. It is a walk over a rule AST (tens
// of nodes, not thousands), performed on the validation and diagnostic paths only.
func walkVarFields(n *Node, root string, visit func(field string) bool) bool {
	if n == nil {
		return false
	}
	if n.Type == NodeVar && n.Name != "" {
		if path, ok := strings.CutPrefix(n.Name, root); ok {
			switch {
			case path == "":
				if visit("") {
					return true
				}
			case path[0] == '.':
				field := path[1:]
				if i := indexByte(field, '.'); i >= 0 {
					field = field[:i]
				}
				if visit(field) {
					return true
				}
			}
			// Anything else shares a prefix without sharing a root
			// (`principality.x`) and is not this root at all.
		}
	}
	if walkVarFields(n.Left, root, visit) || walkVarFields(n.Right, root, visit) {
		return true
	}
	for _, c := range n.Children {
		if walkVarFields(c, root, visit) {
			return true
		}
	}
	for _, it := range n.Items {
		if walkVarFields(it, root, visit) {
			return true
		}
	}
	return false
}
