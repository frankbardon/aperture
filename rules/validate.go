package rules

import (
	"encoding/json"

	aerr "github.com/frankbardon/aperture/errors"
)

// astValidator is the package-level compiler used by ValidateAST. An Engine's
// compiler options are read-only and its compiled-rule cache is concurrency-safe,
// so one shared validator serves every caller without per-call allocation. It has
// a nil rule source and nil fetcher because validation only compiles the AST — it
// never resolves a reference or fetches metadata.
var astValidator = NewEngine(nil, nil)

// ValidateAST decodes raw — a rules.Node AST in its canonical JSON form — and
// checks it is a well-formed, COMPILABLE rule: structural validation (the closed
// node set, arities, and the exposed variable roots) plus a compile pass that
// surfaces the deeper type errors and unknown functions the structural check does
// not. It returns nil for a valid rule and an APERTURE_RULE_* coded error
// otherwise, so the save/validate surfaces (E7-S3) can render the failure on the
// canvas.
//
// It is the DEEP validation the editor and PutRule run before persisting — deeper
// than model.ValidateRule, which only checks the AST is a JSON object. It writes
// nothing and resolves no references.
func ValidateAST(raw json.RawMessage) error {
	return ValidateASTDeclaring(raw, DeclaredAttributeKeys{})
}

// ValidateASTDeclaring is ValidateAST plus DECLARED-KEY enforcement: after the
// rule is proved compilable, every attribute key it names off `principal` or
// `account` is checked against the deployment's declared key set
// (CheckDeclaredAttributeKeys), so a rule reading a key the wiring does not
// guarantee is refused at authoring with APERTURE_RULE_UNDECLARED_ATTRIBUTE.
//
// A zero-value d declares nothing and enforces nothing, which is what ValidateAST
// passes and why every rule that validated before declaring existed still does.
//
// The ORDER is compile-then-declare, deliberately. A rule that does not compile has
// a structural or type error to fix first, and reporting "principal.tier is not
// declared" for an expression that is not yet a rule would point its author at the
// wrong line. The declared-key refusal is the LAST word on an otherwise valid rule.
func ValidateASTDeclaring(raw json.RawMessage, d DeclaredAttributeKeys) error {
	if len(raw) == 0 {
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: empty AST")
	}
	var n Node
	if err := json.Unmarshal(raw, &n); err != nil {
		return aerr.Wrap(aerr.APERTURE_RULE_INVALID, "rule: AST is not a valid rules.Node", err)
	}
	// Compile runs Node.Validate first (structure + variable roots), then renders
	// and type-checks the expression, so a single call surfaces every definition-
	// time rule error as its APERTURE_RULE_* code.
	if _, err := astValidator.Compile(&n); err != nil {
		return err
	}
	return CheckDeclaredAttributeKeys(&n, d)
}
