package cli

import (
	"context"
	"encoding/json"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/service"
)

// The boot path's declared-key gate.
//
// Applying a --seed document writes rules STRAIGHT TO STORAGE: seed.Document.Apply
// upserts them, and its validateRuleAST is a structural check that knows nothing
// about the deployment's declared attribute keys. So without this, `--seed` was the
// one remaining way to save a rule naming a key the wiring does not guarantee — and
// the declared set's whole argument is that no such rule can be saved BY ANY ROUTE,
// because the keys a local attribute layer adds are then inert. One unchecked writer
// does not leave a gap in that argument; it falsifies it. See rules/declared.go, and
// service.requireDeclaredRuleKeys for the same gate on the Import path.
//
// A rule that reads a key only one instance's local layer serves decides on that
// instance and reads a MISSING PATH on every other one, and a missing path makes
// every predicate over it false — so an inclusive grant denies and an EXCLUSIVE
// grant stops excluding, with nothing in any verdict, trace or note to say why.

// refuseUndeclaredSeedRules refuses to boot when a rule in the --seed document reads
// an attribute key the deployment's wiring does not declare. It runs BEFORE loadSeed,
// so a refused document is not applied at all: a rule that reaches storage is in the
// SHARED database and therefore reaches every other instance, where refusing this
// instance's boot would no longer be any protection.
//
// It reads the shared wiring itself and consumes it locally. The sets are the merge
// of the wiring's declaring slots and the document's own attribute_providers: block
// — the same two layers declaredAttributeKeySets serves the facade from — and the
// collapse onto rule roots is service.DeclaredAttributeKeysFor, so there is exactly
// one definition of "union the principal slots, and only when every one declares".
//
// It is a no-op for the deployments that have not opted in, which is every
// deployment that declares no key set: no wiring read, no parse, no walk.
func refuseUndeclaredSeedRules(ctx context.Context, store model.Storage, seedPath string, kind storeKind) error {
	// Only a --seed FILE can carry a rule an author wrote. The embedded example is
	// this repository's own fixture and a durable store with no --seed applies
	// nothing at all, so neither can be the route a bad rule arrives by.
	if seedPath == "" {
		return nil
	}
	wiring, err := readSharedWiring(ctx, store)
	if err != nil {
		return err
	}
	local, err := seedDocument(seedPath, kind)
	if err != nil {
		return err
	}
	if local == nil || len(local.Rules) == 0 {
		return nil
	}
	declared := service.DeclaredAttributeKeysFor(declaredAttributeKeySets(wiring, local))
	if !declared.Enforcing() {
		return nil
	}
	for _, r := range local.Rules {
		var n rules.Node
		if err := json.Unmarshal(r.AST, &n); err != nil {
			// Left to seed's own structural validation, which runs in loadSeed a few
			// lines later and names the rule with APERTURE_RULE_INVALID. Classifying
			// it here as well would give one bad file two different refusals
			// depending on which check happened to run first.
			continue
		}
		if err := rules.CheckDeclaredAttributeKeys(&n, declared); err != nil {
			// The code is PASSED THROUGH rather than wrapped. bootError would stamp
			// APERTURE_BOOT over it and cost the operator the fixups that name the
			// key and the attribute_providers: entry to go and edit.
			return aerr.WithContext(aerr.CodeOf(err),
				"cli: refusing to start — rule "+r.Name+" in the seed document reads an attribute key "+
					"this deployment's wiring does not declare; nothing from the file was applied: "+err.Error(),
				map[string]any{
					"rule": r.Name,
					"seed": seedPath,
					"fix": "add the key to declared_keys: on the attribute_providers: entry that serves it, " +
						"or stop reading it in rule " + r.Name,
				})
		}
	}
	return nil
}
