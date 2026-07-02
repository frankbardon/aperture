package rules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	aerr "github.com/frankbardon/aperture/errors"
)

// evalEnv is the typed expression environment every rule compiles and evaluates
// against. It is a struct (not a bare map) so the expression type-checker treats
// the four roots as the ONLY valid top-level identifiers: a reference to anything
// else is an unknown name at compile time. The metadata roots are map[string]any
// so a rule reads host-defined fields dynamically (object.classification), while
// action is typed string so misusing it (action.foo) is a type error.
//
// The expr struct tags fix the identifier each field is exposed under.
type evalEnv struct {
	Object    map[string]any `expr:"object"`
	Principal map[string]any `expr:"principal"`
	Account   map[string]any `expr:"account"`
	Action    string         `expr:"action"`
}

// Input is the per-evaluation context a compiled rule reads. Object is the
// object's metadata snapshot (treated read-only — never mutated), Principal and
// Account are attribute bags, and Action is the action verb. A nil map is read as
// empty.
type Input struct {
	Object    map[string]any
	Principal map[string]any
	Account   map[string]any
	Action    string
}

// env converts the public Input to the tagged evaluation environment. The two
// structs share field layout (Input carries no expr tags), so a direct
// conversion suffices.
func (in Input) env() evalEnv { return evalEnv(in) }

// Compiled is a rule compiled to a reusable program. It is immutable and safe for
// concurrent evaluation, and carries the canonical source + hash the cache keys
// on. Build one with Compiler.Compile (or get a cached one from an Engine).
type Compiled struct {
	program *vm.Program
	source  string
	hash    string
}

// Source returns the canonical expr-lang expression the rule rendered to.
func (c *Compiled) Source() string { return c.source }

// Hash returns the rule's canonical hash — the cache key (sha256 of Source).
func (c *Compiled) Hash() string { return c.hash }

// Eval runs the compiled rule against in and returns its boolean result.
// Evaluation is pure: it reads only in, mutates nothing, and exposes no
// nondeterministic function. A runtime failure or a non-boolean result is an
// APERTURE_RULE_EVAL coded error. The context is accepted for symmetry with the
// engine seam; expression evaluation itself does not block on it.
func (c *Compiled) Eval(_ context.Context, in Input) (bool, error) {
	out, err := expr.Run(c.program, in.env())
	if err != nil {
		return false, aerr.Wrapf(aerr.APERTURE_RULE_EVAL, err,
			"rule: evaluation failed for %q", c.source)
	}
	b, ok := out.(bool)
	if !ok {
		return false, aerr.WithContext(aerr.APERTURE_RULE_EVAL,
			"rule: expression did not evaluate to a boolean",
			map[string]any{"source": c.source})
	}
	return b, nil
}

// Compiler turns a validated AST into a Compiled program. It holds the expression
// options — the curated pure functions plus any a host registers — that every
// rule it compiles shares. A zero Compiler is not usable; build one with
// NewCompiler. Compiler is safe for concurrent use (its options are read-only
// after construction).
type Compiler struct {
	options []expr.Option
}

// CompilerOption configures a Compiler at construction.
type CompilerOption func(*Compiler)

// Function registers a pure function callable from rules under name. It uses
// expr-lang's Function option: the function joins the expression environment and
// is type-checked at the call site. Hosts should only
// register deterministic, side-effect-free functions so rule evaluation stays
// pure.
func Function(name string, fn func(args ...any) (any, error)) CompilerOption {
	return func(c *Compiler) {
		c.options = append(c.options, expr.Function(name, fn))
	}
}

// NewCompiler builds a Compiler. By default the only callable functions are the
// curated pure set (lower, upper, contains, startsWith, endsWith, len); all of
// expr-lang's builtins are disabled, so no wall-clock or random function is
// reachable and evaluation stays deterministic. Host functions added with
// Function join that set.
func NewCompiler(opts ...CompilerOption) *Compiler {
	c := &Compiler{}
	// The typed env, a required boolean result, and a disabled builtin library
	// are fixed for every rule. The curated functions are registered first so a
	// host Function option can shadow or extend them.
	c.options = append(c.options,
		expr.Env(evalEnv{}),
		expr.AsBool(),
		expr.DisableAllBuiltins(),
	)
	c.options = append(c.options, defaultFunctions()...)
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compile validates the AST, renders it to an expr-lang expression, and compiles
// that to a reusable program. Validation failures surface APERTURE_RULE_INVALID /
// APERTURE_RULE_UNKNOWN_VARIABLE; type-check failures surface
// APERTURE_RULE_TYPE_ERROR — all before any evaluation.
func (c *Compiler) Compile(n *Node) (*Compiled, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	src, err := n.Expr()
	if err != nil {
		return nil, err
	}
	return c.compileSource(src)
}

// compileSource compiles an already-rendered expression. The engine reaches this
// directly after a cache miss, having computed the source (and its hash) to probe
// the cache.
func (c *Compiler) compileSource(src string) (*Compiled, error) {
	program, err := expr.Compile(src, c.options...)
	if err != nil {
		return nil, classifyCompileError(err, src)
	}
	return &Compiled{program: program, source: src, hash: hashSource(src)}, nil
}

// classifyCompileError maps an expr-lang compile error to an Aperture code. The
// AST validator has already proven every variable root is exposed, so a remaining
// failure is a type mismatch, a non-boolean result, or a call to an unregistered
// function — all APERTURE_RULE_TYPE_ERROR. The evaluator's message is preserved
// in context for the operator.
func classifyCompileError(err error, src string) error {
	return aerr.WithContext(aerr.APERTURE_RULE_TYPE_ERROR,
		"rule: expression failed type checking",
		map[string]any{"source": src, "detail": strings.TrimSpace(err.Error())})
}

// hashSource is the canonical-hash function the compiled-rule cache keys on: two
// ASTs that render to the same expression share a compiled program.
func hashSource(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// defaultFunctions is the curated pure function set every Compiler exposes. They
// are deterministic and side-effect-free, the safe baseline a rule can rely on
// regardless of host configuration.
func defaultFunctions() []expr.Option {
	return []expr.Option{
		expr.Function("lower", func(args ...any) (any, error) {
			return strings.ToLower(argString(args)), nil
		}),
		expr.Function("upper", func(args ...any) (any, error) {
			return strings.ToUpper(argString(args)), nil
		}),
		expr.Function("contains", func(args ...any) (any, error) {
			s, sub := arg2String(args)
			return strings.Contains(s, sub), nil
		}),
		expr.Function("startsWith", func(args ...any) (any, error) {
			s, p := arg2String(args)
			return strings.HasPrefix(s, p), nil
		}),
		expr.Function("endsWith", func(args ...any) (any, error) {
			s, p := arg2String(args)
			return strings.HasSuffix(s, p), nil
		}),
		expr.Function("len", func(args ...any) (any, error) {
			if len(args) != 1 {
				return 0, nil
			}
			switch v := args[0].(type) {
			case string:
				return len(v), nil
			case []any:
				return len(v), nil
			case map[string]any:
				return len(v), nil
			case nil:
				return 0, nil
			default:
				return 0, nil
			}
		}),
	}
}

func argString(args []any) string {
	if len(args) == 0 {
		return ""
	}
	s, _ := args[0].(string)
	return s
}

func arg2String(args []any) (string, string) {
	var a, b string
	if len(args) > 0 {
		a, _ = args[0].(string)
	}
	if len(args) > 1 {
		b, _ = args[1].(string)
	}
	return a, b
}
