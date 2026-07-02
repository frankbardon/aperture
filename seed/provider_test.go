package seed

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

func TestBuildRegistry_FromYAMLProviders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brands.csv"),
		[]byte("id,category_id\nbrand:1,category:5\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := Parse([]byte(`
providers:
  - {object_type: brand, kind: csv, path: brands.csv, ttl: "0"}
`), FormatYAML)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Providers) != 1 || doc.Providers[0].ObjectType != "brand" {
		t.Fatalf("providers = %+v", doc.Providers)
	}

	reg, err := doc.BuildRegistry(dir) // relative path resolves against dir
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if !reg.Has("brand") {
		t.Fatal("registry missing brand provider")
	}
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["category_id"] != "category:5" {
		t.Errorf("category_id = %v, want category:5", md["category_id"])
	}
}

func TestBuildRegistry_EmptyWhenNoProviders(t *testing.T) {
	reg, err := (&Document{}).BuildRegistry("")
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Keys()) != 0 {
		t.Errorf("expected empty registry, got %v", reg.Keys())
	}
}

func TestBuildRegistry_Errors(t *testing.T) {
	cases := map[string]Provider{
		"missing object_type": {Kind: "csv", Path: "x.csv"},
		"unknown kind":        {ObjectType: "brand", Kind: "sql", Path: "x"},
		"csv missing path":    {ObjectType: "brand", Kind: "csv"},
		"bad ttl":             {ObjectType: "brand", Kind: "csv", Path: "x.csv", TTL: "notaduration"},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (&Document{Providers: []Provider{p}}).BuildRegistry("")
			if aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
			}
		})
	}
}

func TestBuildRegistry_DuplicateObjectType(t *testing.T) {
	doc := &Document{Providers: []Provider{
		{ObjectType: "brand", Kind: "csv", Path: "a.csv"},
		{ObjectType: "brand", Kind: "csv", Path: "b.csv"},
	}}
	_, err := doc.BuildRegistry("")
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_INVALID {
		t.Fatalf("code = %s, want APERTURE_PROVIDER_INVALID", aerr.CodeOf(err))
	}
}
