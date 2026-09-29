package seed

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// A top-level key that names no section is SILENTLY ABSENT. For the four shared
// wiring sections that is indistinguishable from deliberately dropping one, which is
// how a mistyped `providers:` retires wiring for a whole deployment through a command
// that exits 0. Parse reports them; nothing refuses them.

func TestParseReportsTopLevelKeysThatNameNoSection(t *testing.T) {
	doc, err := Parse([]byte(`
object_types:
  - name: document
    actions: [read]
provider:
  - object_type: document
    kind: csv
    path: docs.csv
wibble: 1
`), FormatYAML)
	if err != nil {
		t.Fatalf("an unknown key must not be an error: %v", err)
	}
	want := []string{"provider", "wibble"}
	if !slices.Equal(doc.UnknownKeys, want) {
		t.Fatalf("UnknownKeys = %v, want %v (sorted)", doc.UnknownKeys, want)
	}
	// The point of the diagnostic: the mistyped section really did vanish.
	if len(doc.Providers) != 0 {
		t.Errorf("providers = %v; a mistyped key must not populate the section", doc.Providers)
	}
}

func TestAWellFormedDocumentReportsNoUnknownKeys(t *testing.T) {
	doc, err := Parse([]byte(`
object_types:
  - name: document
    actions: [read]
providers:
  - object_type: document
    kind: csv
    path: docs.csv
`), FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(doc.UnknownKeys) != 0 {
		t.Fatalf("UnknownKeys = %v, want none", doc.UnknownKeys)
	}
	if len(doc.Providers) != 1 {
		t.Fatalf("providers = %v, want the one declared", doc.Providers)
	}
}

// Every section the struct declares must be recognised. A section added later and a
// scanner that was not taught about it would report the new section as a typo on every
// document that used it — so the set is derived from the tags, and this pins that.
func TestEverySectionTheDocumentDeclaresIsRecognised(t *testing.T) {
	known := documentSectionKeys()
	for _, section := range []string{
		"accounts", "memberships", "object_types", "permissions", "principals",
		"roles", "groups", "grants", "rules", "templates", "providers", "objects",
		"field_types", "connections", "attributes", "attribute_providers",
	} {
		if _, ok := known[section]; !ok {
			t.Errorf("section %q is not recognised, so a document using it would be warned about", section)
		}
	}
	if _, ok := known["-"]; ok {
		t.Error(`a json:"-" field was read as a section name`)
	}
	if _, ok := known["UnknownKeys"]; ok {
		t.Error("UnknownKeys is a parse observation, not a section")
	}
}

// UnknownKeys is an observation and not content: it must not reach an export, a
// re-parse, or the digest a wiring version is identified by (which marshals the whole
// document by reflection).
func TestUnknownKeysIsNotContent(t *testing.T) {
	doc, err := Parse([]byte("object_types: []\nnonsense: 1\n"), FormatYAML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(doc.UnknownKeys) == 0 {
		t.Fatal("fixture did not produce an unknown key")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"UnknownKeys", "unknown_keys", "nonsense"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("marshalled document carries %q; the field must be json:\"-\" so it stays out of an export and out of the local wiring digest", leak)
		}
	}
}

// A malformed document is the decoder's error to report, not the scanner's.
func TestAScalarDocumentYieldsNoUnknownKeys(t *testing.T) {
	if got := unknownTopLevelKeys([]byte(`"just a string"`)); got != nil {
		t.Fatalf("unknownTopLevelKeys(scalar) = %v, want nil", got)
	}
}
