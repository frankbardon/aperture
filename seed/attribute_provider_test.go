package seed

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/storage/memory"
)

// stubAttributes is a minimal provider.AttributeProvider standing in for the
// loader adapters E3-S2 and E3-S3 land. It exists so the wiring ABOVE the loader
// seam — the block, its validation, the connection lookup, the slot precedence,
// the cache options — is provable with no CSV file and no database, which is also
// how CI runs.
//
// enumerable is false for the FETCH-ONLY case a declaration with no get_all
// produces: List and Query refuse, Fetch does not.
type stubAttributes struct {
	bags       map[string]provider.Metadata
	enumerable bool
}

func (s *stubAttributes) Fetch(_ context.Context, id string) (provider.Metadata, error) {
	md, ok := s.bags[id]
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"stub: no such subject", map[string]any{"id": id})
	}
	return md, nil
}

func (s *stubAttributes) List(ctx context.Context) ([]provider.AttributeRecord, error) {
	return s.Query(ctx, provider.AttributeFilter{})
}

func (s *stubAttributes) Query(_ context.Context, _ provider.AttributeFilter) ([]provider.AttributeRecord, error) {
	if !s.enumerable {
		// What a fetch-only slot answers. It is a coded error, not an empty page:
		// an empty enumeration is indistinguishable from a directory that holds
		// nobody.
		return nil, aerr.WithContext(aerr.APERTURE_ATTRIBUTE_PROVIDER_FETCH,
			"stub: this slot declares no get_all and cannot be enumerated", nil)
	}
	out := make([]provider.AttributeRecord, 0, len(s.bags))
	for id, md := range s.bags {
		out = append(out, provider.AttributeRecord{ID: id, Attributes: md})
	}
	return out, nil
}

// recordingOpener is an attributeSourceOpener that captures every resolved
// source and hands back a stub, so a test can assert what the BLOCK resolved to
// without asserting what a loader would do with it.
type recordingOpener struct {
	seen map[provider.AttributeSlot]attributeSource
	bags map[provider.AttributeSlot]map[string]provider.Metadata
}

func newRecordingOpener() *recordingOpener {
	return &recordingOpener{
		seen: map[provider.AttributeSlot]attributeSource{},
		bags: map[provider.AttributeSlot]map[string]provider.Metadata{},
	}
}

func (o *recordingOpener) open(src attributeSource) (provider.AttributeProvider, error) {
	o.seen[src.Slot] = src
	bags := o.bags[src.Slot]
	if bags == nil {
		bags = map[string]provider.Metadata{}
	}
	// A declaration with no get_all is a FETCH-ONLY slot — the asymmetry with
	// providers:, where get_all is required, is deliberate. See
	// AttributeProvider.GetAll.
	return &stubAttributes{bags: bags, enumerable: src.Kind == attributeKindCSV || src.GetAll != ""}, nil
}

// TestAttributeProviders_RefusesAMalformedDeclaration walks every way an
// attribute_providers: entry can be wrong, and pins the CODE rather than the
// message: the code is what carries the operator's fixups.
func TestAttributeProviders_RefusesAMalformedDeclaration(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want aerr.Code
		// substr, when set, must appear in the message — used where the message
		// is the only thing distinguishing two refusals that share a code.
		substr string
	}{
		{
			name: "missing subject",
			yaml: "attribute_providers:\n  - {kind: csv, path: users.csv}\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name: "unknown subject keeps the slot code, not the generic config one",
			yaml: "attribute_providers:\n  - {subject: robot, kind: csv, path: users.csv}\n",
			want: aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
		},
		{
			name: "duplicate slot",
			yaml: "attribute_providers:\n" +
				"  - {subject: user, kind: csv, path: a.csv}\n" +
				"  - {subject: user, kind: csv, path: b.csv}\n",
			want:   aerr.APERTURE_CONFIG_INVALID,
			substr: "declared more than once",
		},
		{
			name: "missing kind",
			yaml: "attribute_providers:\n  - {subject: user, path: users.csv}\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name: "unknown kind",
			yaml: "attribute_providers:\n  - {subject: user, kind: ldap}\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name: "csv without path",
			yaml: "attribute_providers:\n  - {subject: user, kind: csv}\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
		{
			name:   "sql without get_one",
			yaml:   "attribute_providers:\n  - {subject: user, kind: sql, connection: main}\n",
			want:   aerr.APERTURE_CONFIG_INVALID,
			substr: "get_one",
		},
		{
			name:   "sql without connection",
			yaml:   "attribute_providers:\n  - {subject: user, kind: sql, get_one: SELECT 1}\n",
			want:   aerr.APERTURE_CONFIG_INVALID,
			substr: "connection",
		},
		{
			name: "unparseable ttl",
			yaml: "attribute_providers:\n  - {subject: user, kind: csv, path: a.csv, ttl: soon}\n",
			want: aerr.APERTURE_CONFIG_INVALID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := attributeDoc(t, tc.yaml)
			_, err := doc.buildAttributeRegistry("", nil, newRecordingOpener().open)
			if err == nil {
				t.Fatalf("build succeeded; want %s", tc.want)
			}
			if got := aerr.CodeOf(err); got != tc.want {
				t.Fatalf("code = %s; want %s (%v)", got, tc.want, err)
			}
			if tc.substr != "" && !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("message %q does not mention %q", err.Error(), tc.substr)
			}
		})
	}
}

// TestAttributeProviders_UnknownConnectionIsAHardErrorAtBuild is the refusal the
// shared pool depends on: connections: is one pool per NAME, so a typo'd
// connection: must not open a second pool to the same server, and must not
// silently open none.
func TestAttributeProviders_UnknownConnectionIsAHardErrorAtBuild(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := &Document{
		Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}},
		AttributeProviders: []AttributeProvider{
			{Subject: "user", Kind: "sql", Connection: "mian", GetOne: "SELECT department FROM users WHERE id = $1"},
		},
	}
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	_, err = doc.buildAttributeRegistry("", conns, newRecordingOpener().open)
	if err == nil {
		t.Fatal("build succeeded with a connection: naming nothing declared")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_SQL_PROVIDER_CONNECTION {
		t.Fatalf("code = %s; want %s", got, aerr.APERTURE_SQL_PROVIDER_CONNECTION)
	}
	if !strings.Contains(err.Error(), `"mian"`) {
		t.Errorf("message does not name the offending connection: %v", err)
	}
	if opener.callsFor("mian") != 0 {
		t.Error("a second pool was opened for the typo'd name")
	}
}

// TestAttributeProviders_SQLNeedsTheDocumentsPools pins the one-registry-per-pool
// rule at the API boundary: the no-connections form refuses a kind: sql entry and
// names the form that takes the pools, instead of quietly opening a set nothing
// can close.
func TestAttributeProviders_SQLNeedsTheDocumentsPools(t *testing.T) {
	doc := attributeDoc(t, `
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
`)
	_, err := doc.BuildAttributeRegistry("")
	if err == nil {
		t.Fatal("BuildAttributeRegistry accepted a kind: sql entry with no pools")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_SQL_PROVIDER_CONNECTION {
		t.Fatalf("code = %s; want %s", got, aerr.APERTURE_SQL_PROVIDER_CONNECTION)
	}
	if !strings.Contains(err.Error(), "BuildAttributeRegistryWithConnections") {
		t.Errorf("message does not name the form that takes the pools: %v", err)
	}
}

// TestParse_RejectsLiteralDSNOnAnAttributeProvider: the dsn: refusal covers the
// new block too, at PARSE, for the reason it covers connections: — the harm is
// that a password was written into a committed file, so the document must not be
// loadable at all.
func TestParse_RejectsLiteralDSNOnAnAttributeProvider(t *testing.T) {
	_, err := Parse([]byte(`
attribute_providers:
  - subject: user
    kind: sql
    dsn: "postgres://app:hunter2@db/app"
    get_one: SELECT department FROM users WHERE id = $1
`), FormatYAML)
	if err == nil {
		t.Fatal("Parse accepted a literal dsn: on an attribute provider")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL {
		t.Fatalf("code = %s; want %s", got, aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("the refusal echoed the DSN it was refusing")
	}
	if !strings.Contains(err.Error(), `"user"`) {
		t.Errorf("message does not name the offending subject: %v", err)
	}
}

// TestBuildAttributeRegistry_CombinesBothSections is the entry point's promise:
// one registry over attribute_providers: and attributes:, the way BuildRegistry
// combines providers: and objects: — including the precedence rule for a slot
// both sections claim, which is now a LAYERING rather than a discard (the shared
// layer wins a contested key; the local layer keeps its own).
func TestBuildAttributeRegistry_CombinesBothSections(t *testing.T) {
	ctx := context.Background()
	doc := attributeDoc(t, `
attribute_providers:
  - subject: user
    kind: csv
    path: users.csv
attributes:
  # The user slot's LOCAL layer: department is contested and the external source
  # wins it; team is the local layer's own and survives.
  - {subject: user, id: alice, metadata: {department: inline, team: atlas}}
  - {subject: machine, id: ci-runner, metadata: {department: eng}}
  - {subject: account, id: acme, metadata: {plan: enterprise}}
`)
	open := newRecordingOpener()
	open.bags[provider.AttributeSlotUser] = map[string]provider.Metadata{
		"alice": {"department": "external"},
	}
	reg, err := doc.buildAttributeRegistry("/seed", nil, open.open)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, want := reg.RegisteredSlots(), provider.AttributeSlots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RegisteredSlots() = %v; want every slot %v", got, want)
	}

	// The external source wins the keys it serves...
	md, err := reg.Fetch(ctx, provider.AttributeSlotUser, "alice")
	if err != nil {
		t.Fatalf("Fetch(user, alice): %v", err)
	}
	if md["department"] != "external" {
		t.Errorf("department = %#v; want the external source's value — the shared layer wins", md["department"])
	}
	if md["team"] != "atlas" {
		t.Errorf("team = %#v; want atlas — the local layer contributes the keys the shared one does not", md["team"])
	}
	// ...and the inline section still fills the slots it alone claims.
	md, err = reg.Fetch(ctx, provider.AttributeSlotAccount, "acme")
	if err != nil {
		t.Fatalf("Fetch(account, acme): %v", err)
	}
	if md["plan"] != "enterprise" {
		t.Errorf("plan = %#v; want enterprise from the inline section", md["plan"])
	}

	if got, want := doc.AttributeCollisions(), []string{"user"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AttributeCollisions() = %v; want %v", got, want)
	}
	// Only slot names — never keys — so the report cannot leak a directory.
	for _, name := range doc.AttributeCollisions() {
		if name == "alice" {
			t.Fatal("AttributeCollisions leaked an attribute key")
		}
	}
}

// TestAttributeProviders_GetAllIsOptional is the asymmetry with providers:
// written as a test. get_all is REQUIRED for an object provider because an
// errored enumeration reads as "no access" one layer up; attribute enumeration
// never participates in scope resolution, so omitting it here yields a
// fetch-only slot that decides exactly as a fully-enumerable one does.
func TestAttributeProviders_GetAllIsOptional(t *testing.T) {
	ctx := context.Background()
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := &Document{
		Connections: map[string]Connection{"main": {DSNEnv: "APERTURE_TEST_DSN"}},
		AttributeProviders: []AttributeProvider{{
			Subject:    "user",
			Kind:       "sql",
			Connection: "main",
			GetOne:     "SELECT department FROM users WHERE id = $1",
			// No GetAll on purpose.
		}},
	}
	conns, err := doc.openConnections(opener.open)
	if err != nil {
		t.Fatalf("openConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()

	open := newRecordingOpener()
	open.bags[provider.AttributeSlotUser] = map[string]provider.Metadata{
		"alice": {"department": "eng"},
	}
	reg, err := doc.buildAttributeRegistry("", conns, open.open)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := open.seen[provider.AttributeSlotUser].GetAll; got != "" {
		t.Fatalf("resolved get_all = %q; want empty", got)
	}

	// The decision path is untouched: the principal resolver answers.
	bag, err := reg.Attributes(ctx, "user", "alice")
	if err != nil {
		t.Fatalf("Attributes(user, alice): %v", err)
	}
	if bag["department"] != "eng" {
		t.Errorf("department = %#v; want eng", bag["department"])
	}
	// Only the admin read refuses, and it refuses with a code rather than an
	// empty page.
	if _, err := reg.Enumerate(ctx, provider.AttributeSlotUser, provider.AttributeFilter{}); err == nil {
		t.Fatal("a fetch-only slot enumerated")
	} else if aerr.CodeOf(err) == "" {
		t.Fatalf("enumeration refusal carries no Aperture code: %v", err)
	}
}

// TestAttributeProviders_ResolveTheEntryBeforeAnyLoaderSeesIt pins what the block
// settles on the loader's behalf: baseDir applied to a relative path, the shared
// pool and the CONNECTION's statement budget attached to a sql entry, and the
// declared cache options carried through to the slot's cache.
func TestAttributeProviders_ResolveTheEntryBeforeAnyLoaderSeesIt(t *testing.T) {
	dsn := newFakeDSN(t, brandDB())
	t.Setenv("APERTURE_TEST_DSN", dsn)
	opener := newCountingOpener(dsn)

	doc := &Document{
		Connections: map[string]Connection{
			"main": {DSNEnv: "APERTURE_TEST_DSN", QueryTimeout: "2s"},
		},
		Providers: []Provider{
			{ObjectType: "brand", Kind: "sql", Connection: "main", GetOne: brandFetch, GetAll: brandList},
		},
		AttributeProviders: []AttributeProvider{
			{Subject: "user", Kind: "csv", Path: "users.csv", TTL: "45s", MaxSize: 7},
			{Subject: "account", Kind: "sql", Connection: "main",
				GetOne: "SELECT plan FROM accounts WHERE id = $1", IDColumn: "account_id"},
		},
	}
	// ONE pool set, shared: the object registry opens it, the attribute registry
	// is handed it.
	objReg, conns, err := doc.BuildRegistryWithConnections("", WithConnectionOpener(opener.open))
	if err != nil {
		t.Fatalf("BuildRegistryWithConnections: %v", err)
	}
	defer func() { _ = conns.Close() }()
	if len(objReg.Keys()) != 1 {
		t.Fatalf("object registry has %d types, want 1", len(objReg.Keys()))
	}

	open := newRecordingOpener()
	reg, err := doc.buildAttributeRegistry(filepath.Join("/etc", "aperture"), conns, open.open)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	csv := open.seen[provider.AttributeSlotUser]
	if want := filepath.Join("/etc", "aperture", "users.csv"); csv.Path != want {
		t.Errorf("csv path = %q; want %q (baseDir applied by the block, not the loader)", csv.Path, want)
	}
	sqlSrc := open.seen[provider.AttributeSlotAccount]
	if sqlSrc.Pool == nil {
		t.Error("sql source got no pool")
	}
	if sqlSrc.QueryTimeout != 2*time.Second {
		t.Errorf("query timeout = %v; want the CONNECTION's 2s", sqlSrc.QueryTimeout)
	}
	if sqlSrc.IDColumn != "account_id" {
		t.Errorf("id column = %q; want account_id", sqlSrc.IDColumn)
	}
	// One connections: entry is ONE pool, however many providers of either kind
	// name it.
	if got := opener.callsFor("main"); got != 1 {
		t.Errorf("opener called %d times for \"main\"; want exactly 1 — an object provider and an attribute provider must SHARE the pool", got)
	}

	cfg, ok := reg.CacheConfigFor(provider.AttributeSlotUser)
	if !ok {
		t.Fatal("user slot has no cache config")
	}
	if cfg.TTL != 45*time.Second || cfg.MaxSize != 7 {
		t.Errorf("cache config = %+v; want the declared ttl 45s / max_size 7", cfg)
	}
}

// TestAttributeProviderWiringIsNotModelState is TestAttributeWiringIsNotModelState
// for the external block. Apply writes nothing for it, and because Export reads
// the model back OUT of storage, an export reproduces none of it — the seed FILE
// is its only source of truth, exactly as it is for providers:.
func TestAttributeProviderWiringIsNotModelState(t *testing.T) {
	ctx := context.Background()
	doc := attributeDoc(t, `
accounts:
  - {id: acme, name: Acme}
principals:
  - {id: alice, kind: user, identity: "user:alice", display_name: Alice}
attribute_providers:
  - {subject: user, kind: csv, path: users.csv}
`)
	store := memory.New()
	if err := doc.Apply(ctx, store); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := Export(ctx, store)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(out.AttributeProviders) != 0 {
		t.Fatalf("export reproduced the attribute_providers: block: %+v", out.AttributeProviders)
	}
	// The model entities themselves DID land — otherwise the assertion above
	// would pass against a build where Apply wrote nothing at all.
	if len(out.Principals) != 1 || out.Principals[0].ID != "alice" {
		t.Fatalf("Apply did not write the model: %+v", out.Principals)
	}
}

// TestAttributeSlotSourcesReportsThePrecedence: the listing a surface prints has
// to name the source a contested key is actually answered from, and that is the
// precedence rule — an attribute_providers: entry is the slot's SHARED layer and
// wins every key both sections serve, with the inline bags layered under it.
// Defined here, in seed, so a CLI that displays it is not a second implementation
// of it.
func TestAttributeSlotSourcesReportsThePrecedence(t *testing.T) {
	doc := attributeDoc(t, `
attribute_providers:
  - {subject: user, kind: csv, path: users.csv}
  - {subject: machine, kind: sql, connection: main, get_one: "SELECT 1"}
attributes:
  # user is also filled by the csv entry above: that entry is the slot's SHARED
  # layer and wins every key both serve, so the listing must name the winner —
  # csv — and never inline.
  - {subject: user, id: alice, metadata: {department: eng}}
  - {subject: account, id: acme, metadata: {plan: enterprise}}
`)
	got := doc.AttributeSlotSources()
	want := map[string]string{
		"user":    "csv",
		"machine": "sql",
		"account": AttributeSourceInline,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttributeSlotSources() = %v, want %v", got, want)
	}

	// A document that declares no attribute source at all reports nothing, which
	// is how a listing tells "unwired" from "wired to something unnamed".
	if got := attributeDoc(t, "accounts:\n  - {id: acme, name: Acme}\n").AttributeSlotSources(); got != nil {
		t.Errorf("a document with no attribute sections reported %v, want nil", got)
	}
	var nilDoc *Document
	if got := nilDoc.AttributeSlotSources(); got != nil {
		t.Errorf("a nil document reported %v, want nil", got)
	}
}
