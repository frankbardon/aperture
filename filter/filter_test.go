package filter

import (
	"encoding/json"
	"reflect"
	"testing"
)

var entities = []string{
	`{"ID":"acme","Name":"Acme Corp","Kind":"user","Roles":["editor","admin"],"Description":""}`,
	`{"ID":"beta","Name":"Beta LLC","Kind":"machine","Roles":[],"Description":"a note"}`,
	`{"ID":"acme-two","Name":"Acme Two","Kind":"user","Roles":["viewer"],"Description":"x"}`,
}

func gotIDs(t *testing.T, bodies []string, spec Spec) []string {
	t.Helper()
	res := Apply(bodies, spec)
	out := make([]string, 0, len(res))
	for _, b := range res {
		var m map[string]any
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m["ID"].(string))
	}
	return out
}

func TestEmptySpecReturnsAll(t *testing.T) {
	if got := Apply(entities, Spec{}); len(got) != 3 {
		t.Fatalf("empty spec should return all, got %d", len(got))
	}
	if !(Spec{}).Empty() {
		t.Error("zero spec should be Empty()")
	}
	if !(Spec{Predicates: []Predicate{{Field: "ID", Op: "bogus", Value: "x"}}}).Empty() {
		t.Error("spec with only an unknown op should be Empty()")
	}
}

func TestEq(t *testing.T) {
	got := gotIDs(t, entities, Spec{Predicates: []Predicate{{Field: "Kind", Op: OpEq, Value: "USER"}}}) // case-insensitive
	want := []string{"acme", "acme-two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("eq got %v want %v", got, want)
	}
}

func TestContainsAndStarts(t *testing.T) {
	got := gotIDs(t, entities, Spec{Predicates: []Predicate{{Field: "Name", Op: OpContains, Value: "acme"}}})
	if !reflect.DeepEqual(got, []string{"acme", "acme-two"}) {
		t.Fatalf("contains got %v", got)
	}
	got = gotIDs(t, entities, Spec{Predicates: []Predicate{{Field: "ID", Op: OpStarts, Value: "acme-"}}})
	if !reflect.DeepEqual(got, []string{"acme-two"}) {
		t.Fatalf("starts got %v", got)
	}
}

func TestEmptyOperator(t *testing.T) {
	// Description empty (absent value "" ) — acme has "", others differ.
	got := gotIDs(t, entities, Spec{Predicates: []Predicate{{Field: "Description", Op: OpEmpty}}})
	if !reflect.DeepEqual(got, []string{"acme"}) {
		t.Fatalf("empty(desc) got %v", got)
	}
	// Empty array field: beta has Roles: [].
	got = gotIDs(t, entities, Spec{Predicates: []Predicate{{Field: "Roles", Op: OpEmpty}}})
	if !reflect.DeepEqual(got, []string{"beta"}) {
		t.Fatalf("empty(roles) got %v", got)
	}
}

func TestArrayMembership(t *testing.T) {
	// Roles is an array; contains matches any element.
	got := gotIDs(t, entities, Spec{Predicates: []Predicate{{Field: "Roles", Op: OpEq, Value: "admin"}}})
	if !reflect.DeepEqual(got, []string{"acme"}) {
		t.Fatalf("array eq got %v", got)
	}
}

func TestMatchAllVsAny(t *testing.T) {
	preds := []Predicate{
		{Field: "Kind", Op: OpEq, Value: "user"},
		{Field: "ID", Op: OpStarts, Value: "beta"},
	}
	// ALL: user AND starts-beta => none.
	if got := gotIDs(t, entities, Spec{Predicates: preds}); len(got) != 0 {
		t.Fatalf("all got %v want none", got)
	}
	// ANY: user OR starts-beta => acme, beta, acme-two.
	got := gotIDs(t, entities, Spec{Predicates: preds, MatchAny: true})
	if !reflect.DeepEqual(got, []string{"acme", "beta", "acme-two"}) {
		t.Fatalf("any got %v", got)
	}
}
