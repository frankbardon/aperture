// Package rules is Aperture's rules engine: it decides allow/deny — and, on the
// scope seam, object-membership selection — from a domain object's metadata plus
// the principal/action context, using Pulse's expression evaluator.
//
// The package has three layers:
//
//   - The rule AST (this file). A small, explicit, typed node set — logical
//     and/or/not, comparisons, variable references, scalar literals, list
//     literals, and function calls. The AST is BOTH the engine's input and the
//     node editor's serialization target: it has a stable JSON form that
//     round-trips (marshal→unmarshal→marshal is byte-identical), so the Rete.js
//     editor (E7-S2) reads/writes it and the state file (E5-S2) persists it. The
//     editor maps its nodes one-to-one onto these AST nodes; there is no second
//     rule format.
//   - The compiler + cache (compiler.go, cache.go). An AST is validated, rendered
//     to a Pulse expression, and compiled once to a reusable program by
//     expr-lang/expr — the same pure-Go evaluator Pulse uses for its
//     FILTER_EXPRESSION predicate. Compiled programs are cached by the rule's
//     canonical hash so per-Check cost is bounded (the NFR lever E4-S4 tunes).
//   - The engine (engine.go). It resolves a rule reference through a RuleSource,
//     compiles-and-caches it, builds the evaluation context from the object's
//     metadata (fetched through the provider registry) and the principal/action,
//     and evaluates. It satisfies scope.RuleEvaluator, so the inclusive/exclusive
//     scope resolvers (E2-S1) get their rule-backed variant.
//
// Evaluation is pure and deterministic given a fixed metadata snapshot: the
// expression environment is built only from the supplied context, no wall-clock
// or random builtins are exposed, and the only functions callable are the curated
// pure set plus any a host explicitly registers.
package rules

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"

	aerr "github.com/frankbardon/aperture/errors"
)

// NodeType is the discriminator for an AST node. The set is deliberately small
// and closed so the node editor can map its palette one-to-one onto it.
type NodeType string

const (
	// NodeAnd is the logical conjunction of two or more child nodes.
	NodeAnd NodeType = "and"
	// NodeOr is the logical disjunction of two or more child nodes.
	NodeOr NodeType = "or"
	// NodeNot is the logical negation of exactly one child node.
	NodeNot NodeType = "not"
	// NodeCompare is a binary comparison between a left and a right operand.
	NodeCompare NodeType = "compare"
	// NodeVar is a reference to a context variable by dotted path.
	NodeVar NodeType = "var"
	// NodeLiteral is a scalar constant (string, number, bool, or null).
	NodeLiteral NodeType = "literal"
	// NodeList is an ordered list of operand nodes, used as the right side of an
	// in/nin comparison.
	NodeList NodeType = "list"
	// NodeCall is a call to a registered pure function.
	NodeCall NodeType = "call"
)

// Comparison operators carried in a NodeCompare's Op field.
const (
	OpEq  = "eq"  // ==
	OpNe  = "ne"  // !=
	OpLt  = "lt"  // <
	OpLe  = "le"  // <=
	OpGt  = "gt"  // >
	OpGe  = "ge"  // >=
	OpIn  = "in"  // in  (right operand is a list/collection)
	OpNin = "nin" // not in
)

// compareRender maps each comparison operator to its expr-lang spelling.
var compareRender = map[string]string{
	OpEq:  "==",
	OpNe:  "!=",
	OpLt:  "<",
	OpLe:  "<=",
	OpGt:  ">",
	OpGe:  ">=",
	OpIn:  "in",
	OpNin: "not in",
}

// allowedRoots is the closed set of context-variable roots a rule may reference.
// A variable whose first path segment is outside this set is an unknown variable,
// caught by Validate before compilation — deterministically, without relying on
// the expression checker's wording.
var allowedRoots = map[string]struct{}{
	"object":    {},
	"principal": {},
	"account":   {},
	"action":    {},
}

// varPath matches a dotted identifier path, e.g. object.classification or
// principal.attrs.tier. Each segment is a Go-style identifier, which keeps the
// rendered expression injection-free.
var varPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// Node is one node of the rule AST. A node uses only the fields its Type calls
// for; the rest stay zero and are omitted from JSON, so the serialized form is
// minimal and stable. The JSON shape is the contract the node editor and the
// state file share, so field names and the closed Type set are part of the API.
//
// Field usage by Type:
//
//	and/or   Children (>= 2)
//	not      Children (exactly 1)
//	compare  Op, Left, Right
//	var      Name (dotted path)
//	literal  Value (scalar JSON: string, number, bool, or null)
//	list     Items
//	call     Name (function), Items (arguments)
type Node struct {
	Type     NodeType        `json:"type"`
	Op       string          `json:"op,omitempty"`
	Name     string          `json:"name,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
	Left     *Node           `json:"left,omitempty"`
	Right    *Node           `json:"right,omitempty"`
	Children []*Node         `json:"children,omitempty"`
	Items    []*Node         `json:"items,omitempty"`
}

// And returns a logical-and over children.
func And(children ...*Node) *Node { return &Node{Type: NodeAnd, Children: children} }

// Or returns a logical-or over children.
func Or(children ...*Node) *Node { return &Node{Type: NodeOr, Children: children} }

// Not returns the logical negation of child.
func Not(child *Node) *Node { return &Node{Type: NodeNot, Children: []*Node{child}} }

// Compare returns a binary comparison node for one of the Op* operators.
func Compare(op string, left, right *Node) *Node {
	return &Node{Type: NodeCompare, Op: op, Left: left, Right: right}
}

// Var returns a variable reference for a dotted path (e.g. object.classification).
func Var(path string) *Node { return &Node{Type: NodeVar, Name: path} }

// Lit returns a scalar literal node. v must marshal to a JSON scalar (string,
// number, bool, or null); a non-scalar is rejected by Validate.
func Lit(v any) *Node {
	b, err := json.Marshal(v)
	if err != nil {
		return &Node{Type: NodeLiteral}
	}
	return &Node{Type: NodeLiteral, Value: json.RawMessage(b)}
}

// List returns a list literal over items, used as the right operand of in/nin.
func List(items ...*Node) *Node { return &Node{Type: NodeList, Items: items} }

// Call returns a function-call node. The function must be one registered with the
// compiler (the curated pure set, plus any the host adds); an unknown function is
// caught at compile time.
func Call(name string, args ...*Node) *Node {
	return &Node{Type: NodeCall, Name: name, Items: args}
}

// Validate checks that the node (and its subtree) is structurally well-formed,
// returning an APERTURE_RULE_INVALID coded error for a malformed node and
// APERTURE_RULE_UNKNOWN_VARIABLE for a variable outside the exposed roots.
// Validation is pure structure — it does not type-check; that is the compiler's
// job — so it never touches the expression engine.
func (n *Node) Validate() error {
	if n == nil {
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: nil node")
	}
	switch n.Type {
	case NodeAnd, NodeOr:
		if len(n.Children) < 2 {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: logical node requires at least two children",
				map[string]any{"type": string(n.Type), "children": len(n.Children)})
		}
		return validateAll(n.Children)
	case NodeNot:
		if len(n.Children) != 1 {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: not requires exactly one child",
				map[string]any{"children": len(n.Children)})
		}
		return n.Children[0].Validate()
	case NodeCompare:
		if _, ok := compareRender[n.Op]; !ok {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: unknown comparison operator", map[string]any{"op": n.Op})
		}
		if n.Left == nil || n.Right == nil {
			return aerr.New(aerr.APERTURE_RULE_INVALID,
				"rule: comparison requires a left and a right operand")
		}
		if err := n.Left.Validate(); err != nil {
			return err
		}
		if (n.Op == OpIn || n.Op == OpNin) && n.Right.Type != NodeList && n.Right.Type != NodeVar {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: in/nin requires a list or variable on the right",
				map[string]any{"op": n.Op, "right": string(n.Right.Type)})
		}
		return n.Right.Validate()
	case NodeVar:
		return validateVar(n.Name)
	case NodeLiteral:
		return validateLiteral(n.Value)
	case NodeList:
		return validateAll(n.Items)
	case NodeCall:
		if !varPath.MatchString(n.Name) || n.Name == "" {
			return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
				"rule: call has an invalid function name", map[string]any{"name": n.Name})
		}
		return validateAll(n.Items)
	default:
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: unknown node type", map[string]any{"type": string(n.Type)})
	}
}

func validateAll(nodes []*Node) error {
	for _, c := range nodes {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateVar(path string) error {
	if !varPath.MatchString(path) {
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: variable reference is not a dotted identifier path",
			map[string]any{"name": path})
	}
	root := path
	if i := indexByte(path, '.'); i >= 0 {
		root = path[:i]
	}
	if _, ok := allowedRoots[root]; !ok {
		return aerr.WithContext(aerr.APERTURE_RULE_UNKNOWN_VARIABLE,
			"rule: variable root is not an exposed context root",
			map[string]any{"name": path, "root": root})
	}
	return nil
}

// validateLiteral confirms the literal carries a scalar JSON value. Arrays and
// objects are rejected here so a literal can never smuggle a composite; use a
// NodeList for collections.
func validateLiteral(raw json.RawMessage) error {
	if len(raw) == 0 {
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: literal has no value")
	}
	v, err := decodeScalar(raw)
	if err != nil {
		return err
	}
	_ = v
	return nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Expr renders the validated AST to an expr-lang expression string — the Pulse
// predicate the compiler feeds to the evaluator. Expr assumes the node is valid;
// callers reach it through the compiler, which validates first.
func (n *Node) Expr() (string, error) {
	var b bytes.Buffer
	if err := n.render(&b); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (n *Node) render(b *bytes.Buffer) error {
	switch n.Type {
	case NodeAnd:
		return renderJoined(b, n.Children, " && ")
	case NodeOr:
		return renderJoined(b, n.Children, " || ")
	case NodeNot:
		b.WriteString("!(")
		if err := n.Children[0].render(b); err != nil {
			return err
		}
		b.WriteByte(')')
		return nil
	case NodeCompare:
		b.WriteByte('(')
		if err := n.Left.render(b); err != nil {
			return err
		}
		b.WriteByte(' ')
		b.WriteString(compareRender[n.Op])
		b.WriteByte(' ')
		if err := n.Right.render(b); err != nil {
			return err
		}
		b.WriteByte(')')
		return nil
	case NodeVar:
		b.WriteString(n.Name)
		return nil
	case NodeLiteral:
		return renderLiteral(b, n.Value)
	case NodeList:
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := it.render(b); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case NodeCall:
		b.WriteString(n.Name)
		b.WriteByte('(')
		for i, a := range n.Items {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := a.render(b); err != nil {
				return err
			}
		}
		b.WriteByte(')')
		return nil
	default:
		return aerr.WithContext(aerr.APERTURE_RULE_INVALID,
			"rule: cannot render unknown node type", map[string]any{"type": string(n.Type)})
	}
}

func renderJoined(b *bytes.Buffer, children []*Node, sep string) error {
	b.WriteByte('(')
	for i, c := range children {
		if i > 0 {
			b.WriteString(sep)
		}
		if err := c.render(b); err != nil {
			return err
		}
	}
	b.WriteByte(')')
	return nil
}

func renderLiteral(b *bytes.Buffer, raw json.RawMessage) error {
	v, err := decodeScalar(raw)
	if err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("nil")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(x.String())
	case string:
		b.WriteString(strconv.Quote(x))
	default:
		return aerr.New(aerr.APERTURE_RULE_INVALID, "rule: literal is not a scalar")
	}
	return nil
}

// decodeScalar decodes a literal's raw JSON into a scalar (nil, bool, json.Number,
// or string), rejecting composites. UseNumber keeps integer literals exact so a
// rule round-trips and renders without float reformatting.
func decodeScalar(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_RULE_INVALID, "rule: literal is not valid JSON", err)
	}
	switch v.(type) {
	case nil, bool, json.Number, string:
		return v, nil
	default:
		return nil, aerr.New(aerr.APERTURE_RULE_INVALID,
			"rule: literal must be a scalar (string, number, bool, or null); use a list node for collections")
	}
}
