package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/storage/sqlite"

	ucli "github.com/urfave/cli/v3"
)

// E1-S3, driven through the real command tree.
//
// Every case runs against a real SQLite store rather than the in-memory backend,
// for two reasons. The transactional claim is about a database — a validation
// failure part-way must leave the wiring tables exactly as they were — and the
// object-type edge the push reports actionably is a real foreign key that only an
// enforcing backend has. The in-memory store would pass these tests while proving
// less than half of what they say.

// wiringModelSeed is the model state a push needs to exist: the two object types
// the wiring below serves, in the house example domain.
const wiringModelSeed = `
accounts:
  - {id: acme, name: Acme Corp}
object_types:
  - name: document
    description: A document.
    actions: [document.read]
  - name: project
    description: A project.
    actions: [project.read]
`

// wiringSharedSeed is a document carrying all four SHARED sections and both LOCAL
// ones, so the "and nothing else" half of the first case has something to be about.
const wiringSharedSeed = `
object_types:
  - name: never_applied
    description: Proof that a push applies no model state.
    actions: [never.read]
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
  reporting:
    dsn_env: APERTURE_TEST_REPORTING_DSN
providers:
  - object_type: document
    kind: sql
    connection: main
    get_one: SELECT owner, project_ids FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.owner, d.project_ids FROM documents d
    id_column: id
    ttl: 5m
    max_size: 64
    references:
      project_ids: project
      owner_ids: document
  - object_type: project
    kind: sql
    connection: reporting
    get_one: SELECT name FROM projects WHERE id = $1
    get_all: SELECT 'project:' || p.id AS id, p.name FROM projects p
field_types:
  - object_type: document
    fields:
      published_at: datetime
      due: date
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
    get_all: SELECT u.id AS id, u.department FROM users u
    ttl: 30s
    max_size: 100
objects:
  - id: "account:acme/project:atlas"
    metadata: {name: Atlas}
attributes:
  - subject: account
    id: acme
    metadata: {plan: enterprise}
`

// writeWiringSeed materialises a document in a temp directory and returns its path.
func writeWiringSeed(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wiring.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}

// newWiringStore creates a SQLite store, runs Setup, and applies modelSeed to it
// when one is given. An EMPTY modelSeed is the "no model state at all" case: the
// database exists and its schema is there, and that is precisely the store a push
// must refuse.
func newWiringStore(t *testing.T, modelSeed string) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store.db")
	store, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	if err := store.Setup(ctx); err != nil {
		t.Fatalf("setup store: %v", err)
	}
	if modelSeed != "" {
		if err := seed.Load(ctx, store, []byte(modelSeed), seed.FormatYAML); err != nil {
			t.Fatalf("apply model state: %v", err)
		}
	}
	return dsn
}

// openWiringStore reopens a store for assertions.
func openWiringStore(t *testing.T, dsn string) model.Storage {
	t.Helper()
	store, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// readWiring reads the deployed wiring back.
func readWiring(t *testing.T, dsn string) model.WiringSet {
	t.Helper()
	set, err := openWiringStore(t, dsn).GetWiring(context.Background())
	if err != nil {
		t.Fatalf("get wiring: %v", err)
	}
	return set
}

// runWiringCLI runs `aperture wiring <sub> --seed <path> --store <dsn> ...` through
// the real command tree.
func runWiringCLI(t *testing.T, sub, seedPath, storeDSN string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	app := NewApp("test")
	app.Writer = &out
	app.ErrWriter = &out
	app.Reader = strings.NewReader("")
	argv := []string{"aperture", "wiring", sub}
	if seedPath != "" {
		argv = append(argv, "--seed", seedPath)
	}
	if storeDSN != "" {
		argv = append(argv, "--store", storeDSN)
	}
	err := app.Run(context.Background(), append(argv, args...))
	return out.String(), err
}

// mustRefuse asserts a refusal carries the wanted code and EXACTLY ONE
// Aperture-coded error in its chain.
//
// The code alone is not enough: aerr.Wrap re-stamps rather than passing through,
// so a call site that wrapped an already-coded error in the SAME code produces a
// chain two deep that CodeOf cannot tell from a chain one deep — and the next
// edit, which wraps in a DIFFERENT code, silently buries the specific refusal and
// its fixups under a generic one. Depth is what proves the pass-through guard
// (`if aerr.CodeOf(err) != "" { return err }`) is actually there.
func mustRefuse(t *testing.T, what string, err error, want aerr.Code, mentions ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", what)
	}
	if got := aerr.CodeOf(err); got != want {
		t.Fatalf("%s: code = %s, want %s (err: %v)", what, got, want, err)
	}
	if depth := codedDepth(err); depth != 1 {
		t.Fatalf("%s: %d Aperture-coded errors in the chain, want exactly 1 — "+
			"aerr.Wrap RE-STAMPS, so a call site that wraps an already-coded error "+
			"replaces the code a caller reads. Write the guard: "+
			"if aerr.CodeOf(err) != \"\" { return err }. (err: %v)", what, depth, err)
	}
	for _, m := range mentions {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("%s: refusal does not name %q; an operator cannot act on it: %v", what, m, err)
		}
	}
}

// TestPushWritesTheFourSharedSectionsAndNothingElse is the story's first
// criterion: the four shared sections reach the store, the other twelve model-state
// sections and the two LOCAL wiring sections do not.
func TestPushWritesTheFourSharedSectionsAndNothingElse(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	seedPath := writeWiringSeed(t, wiringSharedSeed)

	out, err := runWiringCLI(t, "push", seedPath, dsn)
	if err != nil {
		t.Fatalf("wiring push: %v\n%s", err, out)
	}

	set := readWiring(t, dsn)
	if len(set.Connections) != 2 || set.Connections[0].Name != "main" || set.Connections[1].Name != "reporting" {
		t.Fatalf("connections = %+v, want the two declared names in order", set.Connections)
	}
	// The manifest carries the NAME and nothing else. Neither the DSN nor the
	// dsn_env variable name has a column, because which variable holds the
	// credential is a per-instance fact.
	for _, c := range set.Connections {
		if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
			t.Errorf("connection %q is unstamped; the pushing layer stamps every row", c.Name)
		}
	}
	if len(set.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(set.Providers))
	}
	doc := set.Providers[0]
	if doc.ObjectType != "document" || doc.Kind != "sql" || doc.Connection != "main" {
		t.Errorf("first provider = %+v, want the document entry over connection main", doc)
	}
	if doc.TTL != "5m" {
		// The ttl is stored as the TEXT the operator wrote, so a read back is
		// re-pushable byte for byte; a time.Duration would have made it
		// "300000000000".
		t.Errorf("provider ttl = %q, want the verbatim %q", doc.TTL, "5m")
	}
	if doc.MaxSize != 64 || doc.IDColumn != "id" || doc.GetOne == "" || doc.GetAll == "" {
		t.Errorf("provider statement set did not round-trip: %+v", doc)
	}
	// A references: map has no order, so the rows are sorted by field and that
	// order is the canonical one.
	if len(doc.References) != 2 ||
		doc.References[0].Field != "owner_ids" || doc.References[0].TargetType != "document" ||
		doc.References[1].Field != "project_ids" || doc.References[1].TargetType != "project" {
		t.Errorf("references = %+v, want owner_ids then project_ids", doc.References)
	}
	if len(set.FieldTypes) != 2 ||
		set.FieldTypes[0].Field != "due" || set.FieldTypes[0].DeclaredType != "date" ||
		set.FieldTypes[1].Field != "published_at" || set.FieldTypes[1].DeclaredType != "datetime" {
		t.Errorf("field types = %+v, want due/date then published_at/datetime", set.FieldTypes)
	}
	if len(set.AttributeProviders) != 1 {
		t.Fatalf("attribute providers = %d, want 1", len(set.AttributeProviders))
	}
	ap := set.AttributeProviders[0]
	if ap.Subject != "user" || ap.Kind != "sql" || ap.Connection != "main" || ap.TTL != "30s" || ap.MaxSize != 100 {
		t.Errorf("attribute provider = %+v, want the user slot over connection main", ap)
	}

	// A push applies NO model state. The document declares a third object type;
	// the store must still have only the two the model seed gave it, or the "no
	// model state" refusal could be satisfied by the very push it guards.
	types, err := openWiringStore(t, dsn).ListObjectTypes(context.Background())
	if err != nil {
		t.Fatalf("list object types: %v", err)
	}
	if len(types) != 2 {
		names := make([]string, 0, len(types))
		for _, ot := range types {
			names = append(names, ot.Name)
		}
		t.Errorf("object types = %v, want only the two the model seed applied — a push must apply no model state", names)
	}

	if !strings.Contains(out, "providers") || !strings.Contains(out, "attribute providers") {
		t.Errorf("push printed no per-section summary:\n%s", out)
	}
}

// TestPushIsAReplaceAndIsByteStable is the other half of "one transaction": the
// second push of the same document produces the same set, and a push of a TRIMMED
// document retires what it left out rather than merging.
func TestPushIsAReplaceAndIsByteStable(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	full := writeWiringSeed(t, wiringSharedSeed)

	if out, err := runWiringCLI(t, "push", full, dsn); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	first := readWiring(t, dsn)
	if out, err := runWiringCLI(t, "push", full, dsn); err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	second := readWiring(t, dsn)
	// The stamps are a fresh instant each time, so they are the one thing that may
	// differ; everything an instance BUILDS from must not.
	if !sameWiringShape(first, second) {
		t.Errorf("re-pushing the same document changed the wiring:\n%+v\n%+v", first, second)
	}

	trimmed := writeWiringSeed(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
providers:
  - object_type: document
    kind: sql
    connection: main
    get_one: SELECT owner FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.owner FROM documents d
`)
	if out, err := runWiringCLI(t, "push", trimmed, dsn); err != nil {
		t.Fatalf("trimmed push: %v\n%s", err, out)
	}
	after := readWiring(t, dsn)
	if len(after.Connections) != 1 || len(after.Providers) != 1 ||
		len(after.FieldTypes) != 0 || len(after.AttributeProviders) != 0 {
		t.Errorf("a push is a REPLACE, not a merge; entries the document dropped survived: %+v", after)
	}
	if len(after.Providers) == 1 && len(after.Providers[0].References) != 0 {
		t.Errorf("the replaced provider kept its old reference rows: %+v", after.Providers[0].References)
	}
}

// sameWiringShape compares two sets ignoring the stamps, which are a fresh instant
// per push by design.
func sameWiringShape(a, b model.WiringSet) bool {
	strip := func(s model.WiringSet) model.WiringSet {
		out := model.WiringSet{}
		for _, c := range s.Connections {
			c.CreatedAt, c.UpdatedAt = zeroTime, zeroTime
			out.Connections = append(out.Connections, c)
		}
		for _, p := range s.Providers {
			p.CreatedAt, p.UpdatedAt = zeroTime, zeroTime
			out.Providers = append(out.Providers, p)
		}
		for _, ft := range s.FieldTypes {
			ft.CreatedAt, ft.UpdatedAt = zeroTime, zeroTime
			out.FieldTypes = append(out.FieldTypes, ft)
		}
		for _, ap := range s.AttributeProviders {
			ap.CreatedAt, ap.UpdatedAt = zeroTime, zeroTime
			out.AttributeProviders = append(out.AttributeProviders, ap)
		}
		return out
	}
	return equalWiring(strip(a), strip(b))
}

// TestPushRefusesAStoreWithNoModelState is the typo'd-DSN case: Setup creates a
// perfectly valid, perfectly empty database, and without this refusal the wiring
// lands where no instance reads it and the push reports success.
func TestPushRefusesAStoreWithNoModelState(t *testing.T) {
	dsn := newWiringStore(t, "")
	seedPath := writeWiringSeed(t, wiringSharedSeed)

	_, err := runWiringCLI(t, "push", seedPath, dsn)
	mustRefuse(t, "a push against an empty store", err, aerr.APERTURE_WIRING_NO_MODEL_STATE,
		"apply the model state first")

	if set := readWiring(t, dsn); !set.IsEmpty() {
		t.Errorf("a refused push wrote wiring anyway: %+v", set)
	}
}

// TestPushRefusesAnUnknownObjectType names the missing type. The database would
// refuse the row on its own — apt_wiring_providers.object_type carries a real
// foreign key — but "constraint failed" does not say WHICH of the two names in the
// statement was wrong, and it arrives with the generic storage fixups.
func TestPushRefusesAnUnknownObjectType(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	good := writeWiringSeed(t, wiringSharedSeed)
	if out, err := runWiringCLI(t, "push", good, dsn); err != nil {
		t.Fatalf("baseline push: %v\n%s", err, out)
	}
	before := readWiring(t, dsn)

	bad := writeWiringSeed(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
providers:
  - object_type: dataset
    kind: sql
    connection: main
    get_one: SELECT tier FROM datasets WHERE id = $1
    get_all: SELECT 'dataset:' || d.id AS id, d.tier FROM datasets d
`)
	_, err := runWiringCLI(t, "push", bad, dsn)
	mustRefuse(t, "a provider for an undeclared object type", err,
		aerr.APERTURE_WIRING_OBJECT_TYPE_UNKNOWN, "dataset")

	// The refusal lands before the write, and the write is all-or-nothing, so the
	// wiring the deployment was running is exactly what it is still running.
	if after := readWiring(t, dsn); !equalWiring(before, after) {
		t.Errorf("a refused push disturbed the deployed wiring:\nbefore %+v\nafter  %+v", before, after)
	}
}

// TestPushAcceptsAFieldTypeForATypeWithNoRow is the asymmetry's positive control:
// a field_types: declaration may name a type whose objects a LOCAL seed lists
// inline, which needs no object_types row at all, and apt_wiring_field_types
// carries no edge for exactly that reason. Refusing it here would refuse a
// declaration every loader accepts.
func TestPushAcceptsAFieldTypeForATypeWithNoRow(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	seedPath := writeWiringSeed(t, `
field_types:
  - object_type: inline_only
    fields:
      due: date
`)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("a field type for an inline-only object type must be accepted: %v\n%s", err, out)
	}
	set := readWiring(t, dsn)
	if len(set.FieldTypes) != 1 || set.FieldTypes[0].ObjectType != "inline_only" {
		t.Errorf("field types = %+v, want the inline_only declaration", set.FieldTypes)
	}
}

// TestPushRefusesACSVEntry covers both sections a csv entry can appear in. A path
// is machine-local — a relative one resolves against the seed FILE's directory,
// which database-sourced wiring has none of — so the shared schema has no path
// column and there is nowhere to put one.
func TestPushRefusesACSVEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		says string
	}{
		{
			name: "a providers: entry",
			body: `
providers:
  - object_type: document
    kind: csv
    path: documents.csv
`,
			says: "document",
		},
		{
			name: "an attribute_providers: entry",
			body: `
attribute_providers:
  - subject: user
    kind: csv
    path: users.csv
`,
			says: "user",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newWiringStore(t, wiringModelSeed)
			_, err := runWiringCLI(t, "push", writeWiringSeed(t, tc.body), dsn)
			mustRefuse(t, "a csv entry", err, aerr.APERTURE_WIRING_KIND_UNSHAREABLE,
				tc.says, "csv")
			if set := readWiring(t, dsn); !set.IsEmpty() {
				t.Errorf("a refused push wrote wiring anyway: %+v", set)
			}
		})
	}
}

// TestPushRefusesAnUndeclaredConnection names the connection and lists the
// manifest. Nothing below this layer can catch it: the column carries no foreign
// key, because an entry of a non-database kind names none.
func TestPushRefusesAnUndeclaredConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "a providers: entry",
			body: `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
providers:
  - object_type: document
    kind: sql
    connection: mian
    get_one: SELECT owner FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.owner FROM documents d
`,
		},
		{
			name: "an attribute_providers: entry",
			body: `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: mian
    get_one: SELECT department FROM users WHERE id = $1
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newWiringStore(t, wiringModelSeed)
			_, err := runWiringCLI(t, "push", writeWiringSeed(t, tc.body), dsn)
			// Both the typo and the declared name are in the message: the mistake is
			// almost always a typo, and the answer is usually visible in the list.
			mustRefuse(t, "an undeclared connection", err,
				aerr.APERTURE_WIRING_CONNECTION_UNDECLARED, `"mian"`, `"main"`)
			if set := readWiring(t, dsn); !set.IsEmpty() {
				t.Errorf("a refused push wrote wiring anyway: %+v", set)
			}
		})
	}
}

// TestPushRefusesALiteralDSN reuses the existing posture: only dsn_env:, a
// variable NAME, is ever accepted. Pushing a literal is strictly worse than
// committing one — shared wiring has no column for a DSN and none for the variable
// name either, so an operator who writes one is describing a per-instance fact in
// the one place every instance reads.
func TestPushRefusesALiteralDSN(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		says string
	}{
		{
			name: "on a connections: entry",
			body: `
connections:
  main:
    dsn: postgres://user:secret@localhost/app
providers:
  - object_type: document
    kind: sql
    connection: main
    get_one: SELECT owner FROM documents WHERE id = $1
    get_all: SELECT 'document:' || d.id AS id, d.owner FROM documents d
`,
			says: "main",
		},
		{
			name: "on an attribute_providers: entry",
			body: `
attribute_providers:
  - subject: user
    kind: sql
    dsn: postgres://user:secret@localhost/app
    get_one: SELECT department FROM users WHERE id = $1
`,
			says: "user",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newWiringStore(t, wiringModelSeed)
			out, err := runWiringCLI(t, "push", writeWiringSeed(t, tc.body), dsn)
			mustRefuse(t, "a literal dsn:", err, aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL,
				tc.says, "dsn_env")
			// The offending entry is named. The value never is.
			if strings.Contains(err.Error()+out, "secret") {
				t.Errorf("the refusal echoed the DSN: %v\n%s", err, out)
			}
			if set := readWiring(t, dsn); !set.IsEmpty() {
				t.Errorf("a refused push wrote wiring anyway: %+v", set)
			}
		})
	}
}

// TestPushRefusesALiteralDSNInAProgrammaticDocument is the projection's own guard.
// seed.Parse refuses a literal dsn: at decode, which is why the case above never
// reaches wiringFromDocument — but the push's contract is the push's, and a
// Document assembled in Go rather than read from a file must not slip a credential
// into shared storage through the back door.
func TestPushRefusesALiteralDSNInAProgrammaticDocument(t *testing.T) {
	doc := &seed.Document{
		Connections: map[string]seed.Connection{
			"main": {DSNLiteral: "postgres://user:secret@localhost/app"},
		},
	}
	_, err := wiringFromDocument(doc, zeroTime.AddDate(2000, 0, 0))
	mustRefuse(t, "a programmatic literal dsn:", err,
		aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL, "main")
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the refusal echoed the DSN: %v", err)
	}
}

// TestPushRefusesTheVocabularyStorageDoesNotPolice covers the rules
// model.ValidateWiringSet deliberately leaves to this layer: an unimplemented
// kind, a ttl that is not a Go duration, an unknown declared field type, and a
// subject outside the closed slot set. Storage records each of those verbatim, and
// says in its own doc comment that it does, so the push is the only thing standing
// between a typo and every instance failing its registry build at once.
func TestPushRefusesTheVocabularyStorageDoesNotPolice(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want aerr.Code
		says []string
	}{
		{
			name: "an unimplemented kind",
			body: `
providers:
  - object_type: document
    kind: redis
    connection: main
`,
			want: aerr.APERTURE_CONFIG_INVALID,
			says: []string{"redis"},
		},
		{
			name: "no kind at all",
			body: `
providers:
  - object_type: document
    connection: main
`,
			want: aerr.APERTURE_CONFIG_INVALID,
			says: []string{"no kind"},
		},
		{
			name: "a ttl that is not a duration",
			body: `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
providers:
  - object_type: document
    kind: sql
    connection: main
    ttl: "30"
    get_one: SELECT owner FROM documents WHERE id = $1
`,
			want: aerr.APERTURE_CONFIG_INVALID,
			says: []string{"ttl", "30"},
		},
		{
			name: "an unknown declared field type",
			body: `
field_types:
  - object_type: document
    fields:
      published_at: timestamp
`,
			want: aerr.APERTURE_CONFIG_INVALID,
			says: []string{"published_at", "date or datetime"},
		},
		{
			name: "a subject outside the closed slot set",
			body: `
attribute_providers:
  - subject: robot
    kind: sql
    connection: main
`,
			want: aerr.APERTURE_ATTRIBUTE_SLOT_UNKNOWN,
			says: []string{"robot"},
		},
		{
			name: "a sql entry naming no connection at all",
			body: `
providers:
  - object_type: document
    kind: sql
    get_one: SELECT owner FROM documents WHERE id = $1
`,
			want: aerr.APERTURE_CONFIG_INVALID,
			says: []string{"names no connection"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newWiringStore(t, wiringModelSeed)
			_, err := runWiringCLI(t, "push", writeWiringSeed(t, tc.body), dsn)
			mustRefuse(t, tc.name, err, tc.want, tc.says...)
			if set := readWiring(t, dsn); !set.IsEmpty() {
				t.Errorf("a refused push wrote wiring anyway: %+v", set)
			}
		})
	}
}

// TestPushAndTheSeedBuilderAgreeOnTheFieldTypeVocabulary pins the two words this
// package restates against seed's own (unexported) constants, behaviourally.
//
// Without it, a spelling change in seed/fields.go would have to remember to visit
// internal/cli, and the failure mode is silent in the wrong direction: a push
// accepting a word the builder rejects stores wiring every instance then refuses
// to boot on.
func TestPushAndTheSeedBuilderAgreeOnTheFieldTypeVocabulary(t *testing.T) {
	for _, tc := range []struct {
		declared string
		legal    bool
	}{
		{"date", true},
		{"datetime", true},
		{"timestamp", false},
		{"Date", false},
		{"time", false},
		{"", false},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			doc, err := seed.Parse([]byte("field_types:\n  - object_type: document\n    fields:\n      due: "+
				`"`+tc.declared+`"`+"\n"), seed.FormatYAML)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			// The seed builder's own verdict. A document with no connections: and no
			// providers: reaches fieldTypeIndex and nothing else, so this is the
			// vocabulary check in isolation.
			_, buildErr := doc.BuildRegistry("")
			_, pushErr := wiringFromDocument(doc, zeroTime.AddDate(2000, 0, 0))

			if tc.legal && (buildErr != nil || pushErr != nil) {
				t.Fatalf("%q is legal; builder=%v push=%v", tc.declared, buildErr, pushErr)
			}
			if !tc.legal && (buildErr == nil || pushErr == nil) {
				t.Fatalf("%q must be refused by BOTH; builder=%v push=%v — the two "+
					"vocabularies have drifted, and a push that accepts a word the builder "+
					"rejects stores wiring every instance then refuses to boot on",
					tc.declared, buildErr, pushErr)
			}
		})
	}
}

// TestPushedAttributeSlotsAreNotDeclaredAndDeclaredEmptySurvives is the
// DeclaredKeys distinction, both halves.
//
// A pushed entry is NOT DECLARED, explicitly: the attribute_providers: schema has
// no key for a declared key set yet, and "absent" must land as the zero value
// rather than as a declared-empty one. The second half proves the other state
// survives the store, so the story that adds the YAML key inherits a working
// round trip rather than discovering the distinction was flattened three layers
// down.
func TestPushedAttributeSlotsAreNotDeclaredAndDeclaredEmptySurvives(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	seedPath := writeWiringSeed(t, wiringSharedSeed)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	set := readWiring(t, dsn)
	if len(set.AttributeProviders) != 1 {
		t.Fatalf("attribute providers = %d, want 1", len(set.AttributeProviders))
	}
	if keys := set.AttributeProviders[0].DeclaredKeys; keys.Declared || len(keys.Keys) != 0 {
		t.Errorf("a pushed slot read back as %+v, want NOT declared — an absent YAML key "+
			"must not become a declared-empty set, which opts the slot INTO enforcement "+
			"and permits nothing", keys)
	}

	// Declared-empty is the opposite answer and must survive the same trip.
	store := openWiringStore(t, dsn)
	set.AttributeProviders[0].DeclaredKeys = model.DeclaredKeys{Declared: true}
	if err := store.ReplaceWiring(context.Background(), set); err != nil {
		t.Fatalf("replace with a declared-empty set: %v", err)
	}
	back, err := store.GetWiring(context.Background())
	if err != nil {
		t.Fatalf("get wiring: %v", err)
	}
	if keys := back.AttributeProviders[0].DeclaredKeys; !keys.Declared || len(keys.Keys) != 0 {
		t.Errorf("declared-empty read back as %+v; the two states are DIFFERENT answers — "+
			"declared-empty opts in and permits nothing, not declared opts out", keys)
	}
}

// TestPushNeedsBothFlags: a missing --seed or --store is a usage error, reported
// before anything is opened. There is no embedded-example fallback here, because
// pushing the demo's wiring into a real deployment is not a default anyone wants,
// and no in-memory default, because there would be nothing shared about it.
func TestPushNeedsBothFlags(t *testing.T) {
	seedPath := writeWiringSeed(t, wiringSharedSeed)
	dsn := newWiringStore(t, wiringModelSeed)

	_, err := runWiringCLI(t, "push", "", dsn)
	mustRefuse(t, "a push with no --seed", err, aerr.APERTURE_INVALID_INPUT, "--seed")

	_, err = runWiringCLI(t, "push", seedPath, "")
	mustRefuse(t, "a push with no --store", err, aerr.APERTURE_INVALID_INPUT, "--store")
}

// TestPushReportsAMalformedDocumentBeforeTouchingAStore: the document is parsed
// and projected first, so a bad file needs no database to report.
func TestPushReportsAMalformedDocumentBeforeTouchingAStore(t *testing.T) {
	seedPath := writeWiringSeed(t, "providers: [this is not a provider entry]\n")
	// A store path that does not exist yet: if the command opened it, the file
	// would be created by Setup.
	dsn := filepath.Join(t.TempDir(), "never-created.db")

	_, err := runWiringCLI(t, "push", seedPath, dsn)
	if err == nil {
		t.Fatal("a malformed document must be refused")
	}
	if d := codedDepth(err); d != 1 {
		t.Errorf("%d coded errors in the chain, want exactly 1: %v", d, err)
	}
	if _, statErr := os.Stat(dsn); statErr == nil {
		t.Errorf("the store was opened for a document that never validated: %s", dsn)
	}
}

// ---- E1-S4: `wiring show`, and the push audit record ----

// wiringSubcommand reaches into the real command tree for one `wiring`
// subcommand, so the cases below assert what an operator can actually type rather
// than what a builder function happens to return.
func wiringSubcommand(t *testing.T, name string) *ucli.Command {
	t.Helper()
	for _, c := range wiringCommand().Commands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("`aperture wiring %s` is not registered", name)
	return nil
}

// queryAudit reads the whole audit trail back out of a store.
func queryAudit(t *testing.T, dsn string, filter model.AuditFilter) []model.AuditEvent {
	t.Helper()
	events, err := openWiringStore(t, dsn).QueryAudit(context.Background(), filter)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return events
}

// TestShowPrintsEverySectionOfTheDeployedWiring is the story's first criterion: an
// operator can answer "what is deployed?" without a database client. Every section
// is named, every entry's key appears, and the statement set — the part that
// actually determines what a provider does — is printed rather than summarised.
func TestShowPrintsEverySectionOfTheDeployedWiring(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	seedPath := writeWiringSeed(t, wiringSharedSeed)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// No --seed: show reads the store and nothing else.
	out, err := runWiringCLI(t, "show", "", dsn)
	if err != nil {
		t.Fatalf("wiring show: %v\n%s", err, out)
	}

	for _, want := range []string{
		// The four section headings.
		"connections", "providers", "field types", "attribute providers",
		// The connection manifest, which carries names and nothing else.
		"main", "reporting",
		// The provider entries and their reference declarations.
		"document", "project", "project_ids -> project", "owner_ids -> document",
		// The field-type declarations.
		"published_at", "datetime", "due", "date",
		// The attribute-provider entry, its ttl (the revocation window) and its cap.
		"user", "30s", "100",
		// The statements themselves.
		"statements",
		"SELECT owner, project_ids FROM documents WHERE id = $1",
		"SELECT 'project:' || p.id AS id, p.name FROM projects p",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show does not print %q:\n%s", want, out)
		}
	}

	// The second provider sets neither ttl nor max_size. Both must read as the
	// registry default and NOT as 0 or as an empty cell: "0" reads as "caches
	// nothing", which is the opposite of what an unset bound means.
	if !strings.Contains(out, "(default)") {
		t.Errorf("an entry that sets no ttl and no max_size must say (default), not leave a blank or print 0:\n%s", out)
	}

	// The shared wiring carries no account, no principal and no object identity, so
	// a listing of it cannot leak one. Asserted rather than assumed, because this
	// is the output an operator pastes into a ticket.
	for _, forbidden := range []string{"acme", "account:", "atlas", "enterprise"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("show printed %q; shared wiring holds no account and no inline data, so a listing of it must not either:\n%s", forbidden, out)
		}
	}
}

// TestShowLabelsAFetchOnlySlot: an attribute slot with no get_all is legitimate —
// it serves every decision and refuses only the system-tier directory read — so
// the listing says so instead of leaving a gap that reads as an unfinished entry.
func TestShowLabelsAFetchOnlySlot(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	seedPath := writeWiringSeed(t, `
connections:
  main:
    dsn_env: APERTURE_TEST_MAIN_DSN
attribute_providers:
  - subject: user
    kind: sql
    connection: main
    get_one: SELECT department FROM users WHERE id = $1
`)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	out, err := runWiringCLI(t, "show", "", dsn)
	if err != nil {
		t.Fatalf("wiring show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FETCH-ONLY") {
		t.Errorf("a slot with no get_all must be labelled fetch-only, not left blank:\n%s", out)
	}
}

// TestShowSaysAnEmptyStoreIsAnAnswer: nothing deployed is not an error. It is also
// exactly what a mistyped --store looks like, because a DSN naming a database that
// does not exist yet is one Setup creates — so the output has to name both of the
// opposite things an operator might do next.
func TestShowSaysAnEmptyStoreIsAnAnswer(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)

	out, err := runWiringCLI(t, "show", "", dsn)
	if err != nil {
		t.Fatalf("an empty wiring set is an answer, not an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no shared wiring is deployed") {
		t.Errorf("an empty store must say so plainly:\n%s", out)
	}
	if !strings.Contains(out, "--seed") {
		t.Errorf("the empty answer must say what an instance does instead — build its wiring from its own --seed file:\n%s", out)
	}
	if !strings.Contains(out, "--store") {
		t.Errorf("the empty answer must name the other possibility, a mistyped --store naming a database Setup just created:\n%s", out)
	}
}

// TestShowDistinguishesNotDeclaredFromDeclaredEmpty is the criterion the whole
// existence of model.DeclaredKeys rests on, asserted at the last layer it could
// still be lost in.
//
// NOT DECLARED and DECLARED EMPTY are DIFFERENT ANSWERS: the first opts a slot out
// of key enforcement entirely, the second opts it IN and permits no keys at all. A
// listing that printed a blank for both would collapse the distinction in the one
// place an operator actually reads it — and E3-S3's enforcement turns on exactly
// that bit.
func TestShowDistinguishesNotDeclaredFromDeclaredEmpty(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	store := openWiringStore(t, dsn)
	// Written straight to storage rather than pushed, because the seed document has
	// no key for a declared set yet (E3-S1) — and the printer must already be right
	// when it does, since the column is there from this schema on.
	set := model.WiringSet{AttributeProviders: []model.WiringAttributeProvider{
		{Subject: "account", Kind: "sql", DeclaredKeys: model.DeclaredKeys{}},
		{Subject: "machine", Kind: "sql", DeclaredKeys: model.DeclaredKeys{Declared: true}},
		{Subject: "user", Kind: "sql", DeclaredKeys: model.DeclaredKeys{
			Declared: true, Keys: []string{"clearance", "department"}}},
	}}
	if err := store.ReplaceWiring(context.Background(), set); err != nil {
		t.Fatalf("replace wiring: %v", err)
	}

	out, err := runWiringCLI(t, "show", "", dsn)
	if err != nil {
		t.Fatalf("wiring show: %v\n%s", err, out)
	}
	for _, want := range []string{"(not declared)", "(declared empty)", "clearance, department"} {
		if !strings.Contains(out, want) {
			t.Errorf("show does not print %q — the three states of a declared key set must read differently, "+
				"because not-declared opts OUT of enforcement and declared-empty opts IN and permits nothing:\n%s", want, out)
		}
	}
}

// TestShowIsUngatedAndReadsOnlyTheStore pins both halves of the gating decision
// structurally, because neither is visible from a passing output assertion.
//
// `show` is ungated in the same sense `aperture attributes slots` is: it restates
// wiring that the --store credential the operator just supplied already grants
// full WRITE access to, so an authority check on top of it would only mean nobody
// could diagnose "is anything even deployed?" without already holding the
// authority the diagnosis explains. And it takes no --seed, because a listing that
// merged in the document on the command line would answer "what would a push
// deploy?" while looking like it answered "what IS deployed?".
func TestShowIsUngatedAndReadsOnlyTheStore(t *testing.T) {
	show := wiringSubcommand(t, "show")
	var names []string
	for _, f := range show.Flags {
		names = append(names, f.Names()...)
	}
	if len(names) != 1 || names[0] != "store" {
		t.Errorf("`wiring show` flags = %v, want exactly [store]: an actor flag would gate a read of "+
			"configuration the store credential already covers, and a --seed flag would let the listing "+
			"answer a different question than the one it is printed under", names)
	}
}

// TestShowNeedsAStore: a missing --store is a usage error, and there is no
// in-memory default, because an in-memory store is private to the process that
// opened it and has nothing deployed to it.
func TestShowNeedsAStore(t *testing.T) {
	_, err := runWiringCLI(t, "show", "", "")
	mustRefuse(t, "a show with no --store", err, aerr.APERTURE_INVALID_INPUT, "--store")
}

// TestEverySuccessfulPushIsAudited is the story's second half: one record per
// successful push, in the SAME trail as a grant change.
//
// It is read back from a reopened store after the command has returned, which is
// also the durability assertion. audit.Recorder buffers DECISIONS asynchronously;
// a push must not take that path, or a one-shot CLI invocation could exit with the
// record still in a channel.
func TestEverySuccessfulPushIsAudited(t *testing.T) {
	dsn := newWiringStore(t, wiringModelSeed)
	seedPath := writeWiringSeed(t, wiringSharedSeed)

	before := time.Now().Add(-time.Second)
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// Filtered by the MUTATION category on purpose: "visible in the same trail as
	// grant changes" is the criterion, and an event filed under a category of its
	// own would satisfy an unfiltered read while failing this one.
	events := queryAudit(t, dsn, model.AuditFilter{EventType: model.AuditMutation})
	if len(events) != 1 {
		t.Fatalf("mutation events = %d, want exactly 1 for one push: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Action != "WiringPush" {
		t.Errorf("action = %q, want WiringPush", ev.Action)
	}
	if ev.Outcome != model.OutcomeSuccess {
		t.Errorf("outcome = %q, want %q", ev.Outcome, model.OutcomeSuccess)
	}
	if ev.Target != "wiring:shared" {
		t.Errorf("target = %q, want wiring:shared", ev.Target)
	}
	// WHO: the store credential, recorded as the credential and not as an invented
	// identity. `wiring push` takes no --principal, so any principal id here would
	// be a name nothing verified — worse than an honest sentinel, because the next
	// reader believes it.
	if ev.Actor != wiringPushActor {
		t.Errorf("actor = %q, want the store-credential sentinel %q — a push has no principal to attribute to", ev.Actor, wiringPushActor)
	}
	if !strings.Contains(ev.Reason, "store credential") {
		t.Errorf("reason = %q; it must say in words what the actor sentinel means, for a reader who meets it cold", ev.Reason)
	}
	// WHEN.
	if ev.Timestamp.IsZero() || ev.Timestamp.Before(before) {
		t.Errorf("timestamp = %v, want an instant from this push (after %v)", ev.Timestamp, before)
	}
	if ev.ID == "" {
		t.Error("the event carries no id")
	}
	// Wiring is not account-scoped and a push affects every account at once, so the
	// record carries the reserved wildcard — which names no real account.
	if ev.Account != model.AccountWildcard {
		t.Errorf("account = %q, want the wildcard %q: wiring has no account column and a push changes every account's decisions at once",
			ev.Account, model.AccountWildcard)
	}
	// WHAT: the same counts the push summary printed, so the trail is checkable
	// against what the operator saw.
	set := readWiring(t, dsn)
	for key, want := range map[string]int{
		"connection_count":         len(set.Connections),
		"provider_count":           len(set.Providers),
		"provider_reference_count": countWiringReferences(set),
		"field_type_count":         len(set.FieldTypes),
		"attribute_provider_count": len(set.AttributeProviders),
	} {
		got, ok := ev.Details[key]
		if !ok {
			t.Errorf("details carry no %s", key)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("details[%s] = %v, want %d", key, got, want)
		}
	}

	// One record per push, not one per run of the binary: a second push is a second
	// deployment-wide change and gets its own entry.
	if out, err := runWiringCLI(t, "push", seedPath, dsn); err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	if again := queryAudit(t, dsn, model.AuditFilter{EventType: model.AuditMutation}); len(again) != 2 {
		t.Errorf("mutation events after two pushes = %d, want 2", len(again))
	}
}

// TestARefusedPushIsNotAudited: nothing was deployed, so nothing is recorded.
//
// A record on a refusal would be worse than no record at all — it would put an
// entry in the trail saying the deployment's wiring changed at an instant when it
// provably did not, and an investigator reading back from that entry would be
// chasing a change that never happened.
func TestARefusedPushIsNotAudited(t *testing.T) {
	cases := []struct {
		name      string
		modelSeed string
		doc       string
	}{
		{
			name:      "no model state at all",
			modelSeed: "",
			doc:       wiringSharedSeed,
		},
		{
			name:      "an unknown object type",
			modelSeed: wiringModelSeed,
			doc: `
providers:
  - object_type: invoice
    kind: sql
    get_one: SELECT 1
    get_all: SELECT 1
`,
		},
		{
			name:      "an undeclared connection",
			modelSeed: wiringModelSeed,
			doc: `
providers:
  - object_type: document
    kind: sql
    connection: nowhere
    get_one: SELECT 1
    get_all: SELECT 1
`,
		},
		{
			name:      "a csv entry",
			modelSeed: wiringModelSeed,
			doc: `
providers:
  - object_type: document
    kind: csv
    path: /srv/documents.csv
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newWiringStore(t, tc.modelSeed)
			seedPath := writeWiringSeed(t, tc.doc)

			if _, err := runWiringCLI(t, "push", seedPath, dsn); err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if events := queryAudit(t, dsn, model.AuditFilter{}); len(events) != 0 {
				t.Errorf("a refused push wrote %d audit event(s): %+v — the deployment's wiring "+
					"did not change, so the trail must not say it did", len(events), events)
			}
		})
	}
}

// zeroTime is the unset stamp, used where a case only needs A time.
var zeroTime = time.Time{}

// equalWiring compares two sets field for field, stamps included. It is written
// out rather than done with reflect.DeepEqual so a future field has to be added
// here deliberately, and so the two slice-valued members (a provider's references,
// a slot's declared keys) are compared as the ordered sets they are.
func equalWiring(a, b model.WiringSet) bool {
	if len(a.Connections) != len(b.Connections) || len(a.Providers) != len(b.Providers) ||
		len(a.FieldTypes) != len(b.FieldTypes) || len(a.AttributeProviders) != len(b.AttributeProviders) {
		return false
	}
	for i := range a.Connections {
		if a.Connections[i] != b.Connections[i] {
			return false
		}
	}
	for i := range a.Providers {
		x, y := a.Providers[i], b.Providers[i]
		if x.ObjectType != y.ObjectType || x.Kind != y.Kind || x.Connection != y.Connection ||
			x.GetOne != y.GetOne || x.GetAll != y.GetAll || x.IDColumn != y.IDColumn ||
			x.TTL != y.TTL || x.MaxSize != y.MaxSize ||
			!x.CreatedAt.Equal(y.CreatedAt) || !x.UpdatedAt.Equal(y.UpdatedAt) {
			return false
		}
		if len(x.References) != len(y.References) {
			return false
		}
		for j := range x.References {
			if x.References[j] != y.References[j] {
				return false
			}
		}
	}
	for i := range a.FieldTypes {
		if a.FieldTypes[i] != b.FieldTypes[i] {
			return false
		}
	}
	for i := range a.AttributeProviders {
		x, y := a.AttributeProviders[i], b.AttributeProviders[i]
		if x.Subject != y.Subject || x.Kind != y.Kind || x.Connection != y.Connection ||
			x.GetOne != y.GetOne || x.GetAll != y.GetAll || x.IDColumn != y.IDColumn ||
			x.TTL != y.TTL || x.MaxSize != y.MaxSize ||
			!x.CreatedAt.Equal(y.CreatedAt) || !x.UpdatedAt.Equal(y.UpdatedAt) {
			return false
		}
		if x.DeclaredKeys.Declared != y.DeclaredKeys.Declared ||
			len(x.DeclaredKeys.Keys) != len(y.DeclaredKeys.Keys) {
			return false
		}
		for j := range x.DeclaredKeys.Keys {
			if x.DeclaredKeys.Keys[j] != y.DeclaredKeys.Keys[j] {
				return false
			}
		}
	}
	return true
}
