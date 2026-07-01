package scope

import (
	"context"
	"errors"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

func TestParseSpec(t *testing.T) {
	cases := []struct {
		name     string
		ref      string
		strategy string
		ids      []string
		rule     string
	}{
		{"empty is literal", "", StrategyLiteral, nil, ""},
		{"explicit literal", "literal", StrategyLiteral, nil, ""},
		{"implicit bare", "implicit", StrategyImplicit, nil, ""},
		{"inclusive ids", "inclusive;ids=account:acme/document:42,account:acme/document:99",
			StrategyInclusive, []string{"account:acme/document:42", "account:acme/document:99"}, ""},
		{"exclusive single id", "exclusive;ids=account:acme/document:7",
			StrategyExclusive, []string{"account:acme/document:7"}, ""},
		{"inclusive rule", "inclusive;rule=quarantine", StrategyInclusive, nil, "quarantine"},
		{"whitespace tolerant", " inclusive ; ids = document:1 , document:2 ",
			StrategyInclusive, []string{"document:1", "document:2"}, ""},
		{"custom strategy parses", "team-scope;ids=document:1", "team-scope", []string{"document:1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseSpec(tc.ref)
			if err != nil {
				t.Fatalf("ParseSpec(%q): unexpected error: %v", tc.ref, err)
			}
			if spec.Strategy != tc.strategy {
				t.Errorf("strategy = %q, want %q", spec.Strategy, tc.strategy)
			}
			if spec.Rule != tc.rule {
				t.Errorf("rule = %q, want %q", spec.Rule, tc.rule)
			}
			if len(spec.IDs) != len(tc.ids) {
				t.Fatalf("ids = %v, want %v", spec.IDs, tc.ids)
			}
			for i := range tc.ids {
				if spec.IDs[i] != tc.ids[i] {
					t.Errorf("ids[%d] = %q, want %q", i, spec.IDs[i], tc.ids[i])
				}
			}
		})
	}
}

func TestParseSpecInvalid(t *testing.T) {
	bad := []string{
		"inclusive;ids=",        // empty value
		"inclusive;ids=a,,b",    // empty id-list entry
		"inclusive;bogus=x",     // unknown param
		"inclusive;ids=a;ids=b", // duplicate param
		"inclusive;nokey",       // missing '='
		";ids=a",                // empty strategy
		"inclusive; ;ids=a",     // empty parameter segment
	}
	for _, ref := range bad {
		t.Run(ref, func(t *testing.T) {
			_, err := ParseSpec(ref)
			if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_INVALID {
				t.Fatalf("ParseSpec(%q): code = %q, want APERTURE_SCOPE_INVALID", ref, code)
			}
		})
	}
}

func TestRegistryDefaultBuiltins(t *testing.T) {
	r := DefaultRegistry()
	for _, key := range []string{StrategyImplicit, StrategyInclusive, StrategyExclusive} {
		if !r.Has(key) {
			t.Errorf("DefaultRegistry missing built-in %q", key)
		}
	}
	// literal is engine-native, not registered here.
	if r.Has(StrategyLiteral) {
		t.Errorf("DefaultRegistry should not register literal")
	}
	if len(r.Keys()) != 3 {
		t.Errorf("DefaultRegistry has %d keys, want 3", len(r.Keys()))
	}
}

func TestRegistryUnknownStrategy(t *testing.T) {
	r := DefaultRegistry()
	_, err := r.Resolve(GrantContext{
		Pattern: identity.MustParsePattern("**"),
		Spec:    Spec{Strategy: "nope"},
	}, Deps{})
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_UNKNOWN_STRATEGY {
		t.Fatalf("code = %q, want APERTURE_SCOPE_UNKNOWN_STRATEGY", code)
	}
}

func TestRegistryRegisterCustom(t *testing.T) {
	r := NewRegistry()
	called := false
	f := func(gc GrantContext, deps Deps) (ScopeResolver, error) {
		called = true
		return implicitResolver{gc: gc, deps: deps}, nil
	}
	if err := r.Register("custom", f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Duplicate registration is rejected.
	if err := r.Register("custom", f); aerr.CodeOf(err) != aerr.APERTURE_SCOPE_INVALID {
		t.Fatalf("duplicate Register: code = %q, want APERTURE_SCOPE_INVALID", aerr.CodeOf(err))
	}
	if _, err := r.Resolve(GrantContext{
		Pattern: identity.MustParsePattern("**"),
		Spec:    Spec{Strategy: "custom"},
	}, Deps{}); err != nil {
		t.Fatalf("Resolve custom: %v", err)
	}
	if !called {
		t.Fatalf("custom factory was not invoked")
	}
}

func TestRegistryRegisterRejectsBadInput(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("", func(GrantContext, Deps) (ScopeResolver, error) { return nil, nil }); aerr.CodeOf(err) != aerr.APERTURE_SCOPE_INVALID {
		t.Errorf("empty key: want APERTURE_SCOPE_INVALID, got %q", aerr.CodeOf(err))
	}
	if err := r.Register("k", nil); aerr.CodeOf(err) != aerr.APERTURE_SCOPE_INVALID {
		t.Errorf("nil factory: want APERTURE_SCOPE_INVALID, got %q", aerr.CodeOf(err))
	}
}

// noLister / noRules default seams surface their coded errors.
func TestDefaultSeamsAreUnconfigured(t *testing.T) {
	var d Deps
	_, err := d.lister().List(context.Background(), "document", identity.MustParsePattern("**"), 0)
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_LISTER_UNCONFIGURED {
		t.Errorf("lister: code = %q, want APERTURE_SCOPE_LISTER_UNCONFIGURED", code)
	}
	_, err = d.rules().Selected(context.Background(), "r", identity.MustParse("document:1"), "p", "a")
	if code := aerr.CodeOf(err); code != aerr.APERTURE_SCOPE_RULE_UNCONFIGURED {
		t.Errorf("rules: code = %q, want APERTURE_SCOPE_RULE_UNCONFIGURED", code)
	}
	if !errors.As(err, new(*aerr.CodedError)) {
		t.Errorf("rules error is not a CodedError")
	}
}
