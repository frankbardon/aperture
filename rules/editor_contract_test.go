package rules

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestEditorASTContract guards the E7-S2 node-editor <-> AST contract from the
// Go side. The JS serializer (internal/server/static/js/rules-serializer.js)
// emits these exact JSON shapes; this test parses the same representative ASTs
// through rules.Node and asserts each is (a) structurally valid and (b) stable
// under marshal->unmarshal->marshal. Because the JS round-trip test is not in
// the Go-only CI path, this is the CI-guarded half of the invariant: if the AST
// JSON shape ever drifts from what the editor produces, one side breaks here.
//
// The cases mirror rules-serializer.test.js one-for-one (nested and/or/not,
// compare with var+literal, in/nin with a list and with a var, a call, and the
// falsy-scalar edge cases false/0/""/null that must survive omitempty).
func TestEditorASTContract(t *testing.T) {
	cases := map[string]string{
		"compare var eq string": `{"type":"compare","op":"eq","left":{"type":"var","name":"object.classification"},"right":{"type":"literal","value":"public"}}`,

		"nested and/or/not": `{"type":"and","children":[` +
			`{"type":"or","children":[` +
			`{"type":"compare","op":"gt","left":{"type":"var","name":"object.level"},"right":{"type":"literal","value":3}},` +
			`{"type":"compare","op":"eq","left":{"type":"var","name":"principal.tier"},"right":{"type":"literal","value":"gold"}}]},` +
			`{"type":"not","children":[` +
			`{"type":"compare","op":"eq","left":{"type":"var","name":"account.suspended"},"right":{"type":"literal","value":true}}]}]}`,

		"in with list literal": `{"type":"compare","op":"in","left":{"type":"var","name":"object.region"},"right":{"type":"list","items":[{"type":"literal","value":"us"},{"type":"literal","value":"eu"},{"type":"literal","value":"apac"}]}}`,

		"nin with var on right": `{"type":"compare","op":"nin","left":{"type":"var","name":"principal.id"},"right":{"type":"var","name":"object.blocklist"}}`,

		"call len ge number": `{"type":"compare","op":"ge","left":{"type":"call","name":"len","items":[{"type":"var","name":"object.tags"}]},"right":{"type":"literal","value":1}}`,

		"falsy false": `{"type":"compare","op":"eq","left":{"type":"var","name":"object.archived"},"right":{"type":"literal","value":false}}`,
		"falsy zero":  `{"type":"compare","op":"eq","left":{"type":"var","name":"object.count"},"right":{"type":"literal","value":0}}`,
		"falsy empty": `{"type":"compare","op":"ne","left":{"type":"var","name":"object.note"},"right":{"type":"literal","value":""}}`,
		"falsy null":  `{"type":"compare","op":"eq","left":{"type":"var","name":"object.owner"},"right":{"type":"literal","value":null}}`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var n Node
			if err := json.Unmarshal([]byte(src), &n); err != nil {
				t.Fatalf("unmarshal editor JSON: %v", err)
			}
			if err := n.Validate(); err != nil {
				t.Fatalf("editor AST failed validation: %v", err)
			}
			// The literal edge cases must survive the round-trip: false/0/""/null
			// are non-empty RawMessage, so they are NOT dropped by omitempty.
			out, err := json.Marshal(&n)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal([]byte(src), out) {
				t.Errorf("editor AST is not byte-stable\n  in:  %s\n  out: %s", src, out)
			}
		})
	}
}
