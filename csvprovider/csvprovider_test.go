package csvprovider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// write drops content into a temp .csv and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestFetch_TypedFields(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,category_id,seats:int,active:bool,budget:float",
		"brand:1,category:5,12,true,9.5",
	}, "\n")))

	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["category_id"] != "category:5" {
		t.Errorf("category_id = %#v, want string category:5", md["category_id"])
	}
	if md["seats"] != int64(12) {
		t.Errorf("seats = %#v, want int64(12)", md["seats"])
	}
	if md["active"] != true {
		t.Errorf("active = %#v, want bool true", md["active"])
	}
	if md["budget"] != 9.5 {
		t.Errorf("budget = %#v, want float64 9.5", md["budget"])
	}
}

func TestFetch_MissingIsNotFound(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:1,category:5\n"))
	_, err := p.Fetch(context.Background(), identity.MustParse("brand:404"))
	if got := aerr.CodeOf(err); got != aerr.APERTURE_NOT_FOUND {
		t.Fatalf("code = %s, want APERTURE_NOT_FOUND", got)
	}
}

func TestList_PreservesFileOrder(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:3,c\nbrand:1,c\nbrand:2,c\n"))
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := []string{objs[0].ID.String(), objs[1].ID.String(), objs[2].ID.String()}
	want := []string{"brand:3", "brand:1", "brand:2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestQuery_FieldsAndPatternAndLimit(t *testing.T) {
	p := New(write(t, strings.Join([]string{
		"id,facing",
		"app:be,external",
		"app:bt,external",
		"app:bi,internal",
	}, "\n")))

	// Field equality.
	ext, err := p.Query(context.Background(), provider.Filter{Fields: map[string]any{"facing": "external"}})
	if err != nil {
		t.Fatalf("Query fields: %v", err)
	}
	if len(ext) != 2 {
		t.Fatalf("external apps = %d, want 2", len(ext))
	}

	// Pattern.
	pat := identity.MustParsePattern("app:bi")
	only, err := p.Query(context.Background(), provider.Filter{Pattern: &pat})
	if err != nil {
		t.Fatalf("Query pattern: %v", err)
	}
	if len(only) != 1 || only[0].ID.String() != "app:bi" {
		t.Fatalf("pattern result = %v, want [app:bi]", only)
	}

	// Limit.
	lim, err := p.Query(context.Background(), provider.Filter{Limit: 1})
	if err != nil {
		t.Fatalf("Query limit: %v", err)
	}
	if len(lim) != 1 {
		t.Fatalf("limited result = %d, want 1", len(lim))
	}
}

func TestEmptyCellOmitsField(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:1,\n"))
	md, err := p.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, present := md["category_id"]; present {
		t.Errorf("empty cell should omit field, got %#v", md["category_id"])
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no id column":     "name,category_id\nfoo,bar\n",
		"duplicate id":     "id,category_id\nbrand:1,a\nbrand:1,b\n",
		"unknown type":     "id,seats:money\nbrand:1,5\n",
		"bad int":          "id,seats:int\nbrand:1,notanumber\n",
		"wrong col count":  "id,category_id\nbrand:1\n",
		"duplicate column": "id,category_id,category_id\nbrand:1,a,b\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p_load(t, content)
			if got := aerr.CodeOf(err); got != aerr.APERTURE_CONFIG_INVALID {
				t.Fatalf("code = %s, want APERTURE_CONFIG_INVALID", got)
			}
		})
	}
}

func TestBadIdentityPassesThrough(t *testing.T) {
	_, err := p_load(t, "id,category_id\nbrand:,a\n") // empty segment id
	if got := aerr.CodeOf(err); got != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("code = %s, want APERTURE_IDENTITY_INVALID", got)
	}
}

// p_load forces a load through the public API and returns any error.
func p_load(t *testing.T, content string) (*Provider, error) {
	t.Helper()
	p := New(write(t, content))
	_, err := p.List(context.Background())
	return p, err
}

func TestReload_SwapsSetAndKeepsOldMapsImmutable(t *testing.T) {
	path := write(t, "id,facing\napp:be,external\n")
	p := New(path)

	before, err := p.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch before: %v", err)
	}

	if err := os.WriteFile(path, []byte("id,facing\napp:be,internal\napp:bt,external\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after, err := p.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch after: %v", err)
	}
	if after["facing"] != "internal" {
		t.Errorf("after reload facing = %v, want internal", after["facing"])
	}
	if before["facing"] != "external" {
		t.Errorf("pre-reload map mutated: facing = %v, want external", before["facing"])
	}
	objs, _ := p.List(context.Background())
	if len(objs) != 2 {
		t.Errorf("after reload count = %d, want 2", len(objs))
	}
}

// TestRegistryIntegration proves the provider is a drop-in for the Registry:
// registration, cache-first Fetch, and scope-style List all work through it.
func TestRegistryIntegration(t *testing.T) {
	p := New(write(t, "id,category_id\nbrand:1,category:5\nbrand:2,category:9\n"))
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("registry Fetch: %v", err)
	}
	if md["category_id"] != "category:5" {
		t.Errorf("category_id = %v, want category:5", md["category_id"])
	}

	// Second Fetch is a cache hit (provider not consulted again).
	if _, err := reg.Fetch(context.Background(), identity.MustParse("brand:1")); err != nil {
		t.Fatalf("registry Fetch (cached): %v", err)
	}
	if s, _ := reg.Stats("brand"); s.Hits != 1 || s.Misses != 1 {
		t.Errorf("stats = %+v, want 1 hit / 1 miss", s)
	}

	// Registry.List (scope.ObjectLister) enumerates through the provider.
	ids, err := reg.List(context.Background(), "brand", identity.MustParsePattern("brand:*"), 0)
	if err != nil {
		t.Fatalf("registry List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("List count = %d, want 2", len(ids))
	}
}

func TestFromReader(t *testing.T) {
	p, err := FromReader(strings.NewReader("id,facing\napp:be,external\n"))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	md, err := p.Fetch(context.Background(), identity.MustParse("app:be"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if md["facing"] != "external" {
		t.Errorf("facing = %v, want external", md["facing"])
	}
	if err := p.Reload(); aerr.CodeOf(err) != aerr.APERTURE_CONFIG_INVALID {
		t.Errorf("reader Reload code = %s, want APERTURE_CONFIG_INVALID", aerr.CodeOf(err))
	}
}
