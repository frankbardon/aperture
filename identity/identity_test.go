package identity

import (
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

func TestParseAndCanonicalRoundTrip(t *testing.T) {
	cases := []string{
		"account:acme",
		"account:acme/project:atlas",
		"account:acme/project:atlas/document:42",
		"user:alice@example.com",
		"document:a-b_c.d~e+f",
		"tenant:01HZX/group:admins",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			id, err := Parse(s)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", s, err)
			}
			if got := id.String(); got != s {
				t.Fatalf("round-trip: Parse(%q).String() = %q, want %q", s, got, s)
			}
		})
	}
}

func TestParseSegmentsDecomposed(t *testing.T) {
	id, err := Parse("account:acme/project:atlas/document:42")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if id.Len() != 3 {
		t.Fatalf("Len = %d, want 3", id.Len())
	}
	want := []Segment{
		{Type: "account", ID: "acme"},
		{Type: "project", ID: "atlas"},
		{Type: "document", ID: "42"},
	}
	got := id.Segments()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSegmentsReturnsCopy(t *testing.T) {
	id := MustParse("account:acme/project:atlas")
	segs := id.Segments()
	segs[0].ID = "mutated"
	if id.Segments()[0].ID != "acme" {
		t.Fatal("Segments() leaked a mutable reference to internal state")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"leading slash / empty segment", "/account:acme"},
		{"trailing slash / empty segment", "account:acme/"},
		{"doubled slash", "account:acme//project:atlas"},
		{"missing colon", "account:acme/project"},
		{"empty type", ":acme"},
		{"empty id", "account:"},
		{"illegal char in id (space)", "account:ac me"},
		{"illegal char in type (slash already split, star)", "account:acme/pro*ject:x"},
		{"wildcard not allowed in concrete identity", "account:*"},
		{"double wildcard not allowed", "account:acme/**"},
		{"colon inside id", "account:a:b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.in)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want APERTURE_IDENTITY_INVALID", c.in)
			}
			if got := aerr.CodeOf(err); got != aerr.APERTURE_IDENTITY_INVALID {
				t.Fatalf("Parse(%q) code = %q, want APERTURE_IDENTITY_INVALID", c.in, got)
			}
		})
	}
}

func TestParseColonInIDIsSplitAtFirstColon(t *testing.T) {
	// "account:a:b" splits to type="account", id="a:b"; ':' is illegal in id.
	_, err := Parse("account:a:b")
	if aerr.CodeOf(err) != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("want APERTURE_IDENTITY_INVALID, got %v", err)
	}
}

func TestNewValidatesComponents(t *testing.T) {
	if _, err := New(); err == nil {
		t.Fatal("New() with no segments should error")
	}
	if _, err := New(Segment{Type: "account", ID: ""}); err == nil {
		t.Fatal("New with empty id should error")
	}
	if _, err := New(Segment{Type: "account", ID: "ac/me"}); err == nil {
		t.Fatal("New with illegal id char should error")
	}
	id, err := New(Segment{Type: "account", ID: "acme"}, Segment{Type: "project", ID: "atlas"})
	if err != nil {
		t.Fatalf("New valid: %v", err)
	}
	if got := id.String(); got != "account:acme/project:atlas" {
		t.Fatalf("New().String() = %q", got)
	}
}

func TestMustParsePanicsOnInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustParse on invalid input did not panic")
		}
	}()
	MustParse("bogus")
}

func TestSegmentString(t *testing.T) {
	if got := (Segment{Type: "document", ID: "42"}).String(); got != "document:42" {
		t.Fatalf("Segment.String() = %q", got)
	}
}
