package sqlprovider

import (
	"context"
	"database/sql/driver"
	stderrors "errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// The enumeration half of the provider: List and Query over the developer's
// "get all" statement, with the identity composed by that statement in its id
// column and every predicate applied in Go.

// brandsScript is the canonical three-row result the enumeration tests read.
// tags arrives as a []byte of JSON, which is the ONLY way a list-valued field
// can arrive (see the package doc's casting rules) and therefore the path the
// membership tests must exercise.
func brandsScript() *script {
	return &script{
		cols: []string{"id", "tier", "seats", "tags"},
		rows: [][]driver.Value{
			{"brand:1", "gold", int64(5), []byte(`["premium","launch"]`)},
			{"brand:2", "silver", int64(3), []byte(`["launch"]`)},
			{"account:acme/brand:3", "gold", int64(9), []byte(`["premium"]`)},
		},
	}
}

func ids(objs []provider.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.ID.String())
	}
	return out
}

// List returns one Object per row, with the id column consumed as the IDENTITY
// and every other column mapped to a metadata field exactly as Fetch maps it.
func TestListReturnsEveryRowWithItsMetadata(t *testing.T) {
	s := brandsScript()
	p := newProvider(t, s, Config{})

	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"brand:1", "brand:2", "account:acme/brand:3"}
	if got := ids(objs); !reflect.DeepEqual(got, want) {
		t.Fatalf("identities = %v, want %v in statement order", got, want)
	}

	first := objs[0].Metadata
	if first["tier"] != "gold" || first["seats"] != int64(5) {
		t.Fatalf("metadata = %#v", first)
	}
	// The id column is the identity, not a field: it must not also show up as
	// metadata, where it would be a second, redundant spelling of the same thing
	// that a rule could disagree with.
	if _, ok := first["id"]; ok {
		t.Fatalf("the id column leaked into metadata: %#v", first)
	}
	// E2-S2's []byte -> JSON path is what makes a list-valued field possible.
	if !reflect.DeepEqual(first["tags"], []any{"premium", "launch"}) {
		t.Fatalf("tags = %#v, want a decoded []any", first["tags"])
	}
	for _, o := range objs {
		if err := provider.ValidateMetadata(o.Metadata); err != nil {
			t.Fatalf("enumerated metadata violates the value model: %v", err)
		}
	}
}

// The "get all" statement takes NO parameters and is passed through untouched,
// exactly as the fetch statement is.
func TestListBindsNoParametersAndPassesTheStatementThrough(t *testing.T) {
	const q = `SELECT 'brand:' || b.id AS id, b.tier FROM brands b ORDER BY b.id`
	s := &script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{"brand:1", "gold"}}}
	p := newProvider(t, s, Config{ListQuery: q})

	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	_, got, args := s.observed()
	if got != q {
		t.Fatalf("statement = %q, want it passed through as %q", got, q)
	}
	if len(args) != 0 {
		t.Fatalf("bound %d args, want none: %v", len(args), args)
	}
}

// An empty result is an empty enumeration, not an error: "this type has no
// objects" is a legitimate answer, unlike a statement that could not run.
func TestListOfAnEmptyResultIsEmptyNotAnError(t *testing.T) {
	p := newProvider(t, &script{cols: []string{"id", "tier"}}, Config{})
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("objects = %#v, want none", objs)
	}
}

// A configurable id column, because a developer whose statement already uses
// another alias should not have to rename it.
func TestListReadsTheConfiguredIDColumn(t *testing.T) {
	s := &script{
		cols: []string{"aperture_id", "tier"},
		rows: [][]driver.Value{{"brand:1", "gold"}},
	}
	p := newProvider(t, s, Config{IDColumn: "aperture_id"})
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 || objs[0].ID.String() != "brand:1" {
		t.Fatalf("objects = %#v", objs)
	}
	if _, ok := objs[0].Metadata["aperture_id"]; ok {
		t.Fatalf("the configured id column leaked into metadata: %#v", objs[0].Metadata)
	}
}

// Every way a row can fail to yield a usable identity is one coded error naming
// the row, never a row quietly dropped: a dropped row is a short enumeration,
// and a short enumeration reads as "no access".
func TestListRejectsARowWithoutAUsableIdentity(t *testing.T) {
	cases := []struct {
		name string
		s    *script
	}{
		{
			"no id column in the result",
			&script{cols: []string{"tier"}, rows: [][]driver.Value{{"gold"}}},
		},
		{
			"NULL id",
			&script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{nil, "gold"}}},
		},
		{
			"empty id",
			&script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{"", "gold"}}},
		},
		{
			"blank id",
			&script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{"   ", "gold"}}},
		},
		{
			// A bare primary key is not an identity, and Aperture supplies no
			// template that would turn one into one.
			"non-textual id",
			&script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{int64(1), "gold"}}},
		},
		{
			"unparseable id",
			&script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{"42", "gold"}}},
		},
		{
			"identity of another object-type",
			&script{cols: []string{"id", "tier"}, rows: [][]driver.Value{{"dataset:1", "gold"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t, tc.s, Config{})
			objs, err := p.List(context.Background())
			mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY)
			if objs != nil {
				t.Fatalf("objects = %#v, want nil: an unusable row fails the enumeration", objs)
			}
		})
	}
}

// The error names WHICH row, since the id is exactly what the developer has to
// go and look at.
func TestARowIdentityErrorNamesTheRow(t *testing.T) {
	s := &script{
		cols: []string{"id", "tier"},
		rows: [][]driver.Value{
			{"brand:1", "gold"},
			{"brand:2", "silver"},
			{"dataset:9", "bronze"},
		},
	}
	p := newProvider(t, s, Config{})
	_, err := p.List(context.Background())
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_ROW_IDENTITY)

	var ce *aerr.CodedError
	if !stderrors.As(err, &ce) {
		t.Fatalf("error is not a *CodedError: %v", err)
	}
	if ce.Context["row"] != 3 {
		t.Fatalf("context row = %v, want the 3rd row: %#v", ce.Context["row"], ce.Context)
	}
	if ce.Context["id"] != "dataset:9" {
		t.Fatalf("context id = %v, want the offending identity: %#v", ce.Context["id"], ce.Context)
	}
}

// The criterion this check exists for: a "brand:1" row served by the provider
// registered under "dataset" must NEVER reach the cache. It would be cached
// under an identity this provider's own Fetch could not return, so a later Check
// would read one type's row as another type's object.
func TestARowOfAnotherObjectTypeNeverReachesTheCache(t *testing.T) {
	s := &script{
		cols: []string{"id", "tier"},
		rows: [][]driver.Value{{"brand:1", "gold"}},
	}
	p := newProvider(t, s, Config{ObjectType: "dataset"})

	reg := provider.NewRegistry()
	reg.MustRegister("dataset", p)
	_, err := reg.List(context.Background(), "dataset", identity.MustParsePattern("*:*"), 10)
	if err == nil {
		t.Fatalf("want an error for a brand row served by the dataset provider")
	}
	if _, cached := reg.Stats("dataset"); !cached {
		t.Fatalf("registry has no stats for the type")
	}
	st, _ := reg.Stats("dataset")
	if st.Entries != 0 {
		t.Fatalf("cache holds %d entries; the mistyped row reached it", st.Entries)
	}
}

// A result whose column names cannot be metadata field keys fails the same way
// it does on a fetch — checked once for the statement rather than once per row.
func TestListRejectsUnusableColumnNames(t *testing.T) {
	cases := []struct {
		name string
		s    *script
	}{
		{"unnamed column", &script{
			cols: []string{"id", ""},
			rows: [][]driver.Value{{"brand:1", "gold"}},
		}},
		{"duplicate column", &script{
			cols: []string{"id", "tier", "tier"},
			rows: [][]driver.Value{{"brand:1", "gold", "silver"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t, tc.s, Config{})
			_, err := p.List(context.Background())
			mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_SCAN)
		})
	}
}

// A driver failure on the list statement is coded and wraps its cause, as the
// fetch statement's is.
func TestListDriverErrorIsCodedAndWrapsTheCause(t *testing.T) {
	boom := stderrors.New("connection refused")
	p := newProvider(t, &script{queryErr: boom}, Config{})
	_, err := p.List(context.Background())
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	if !stderrors.Is(err, boom) {
		t.Fatalf("cause is not reachable with errors.Is: %v", err)
	}
}

// An already-coded error from a host's wrapping Querier passes through verbatim;
// wrappers never re-stamp a code.
func TestListCodedDriverErrorPassesThroughVerbatim(t *testing.T) {
	coded := aerr.New(aerr.APERTURE_UNAUTHENTICATED, "host: read replica refused the connection")
	p := newProvider(t, &script{queryErr: coded}, Config{})
	_, err := p.List(context.Background())
	mustCode(t, err, aerr.APERTURE_UNAUTHENTICATED)
}

// rows.Err is the difference between "there are two brands" and "there are two
// brands SO FAR". A mid-iteration failure must fail the enumeration: a truncated
// result reported as a complete one silently under-reports access.
func TestListMidIterationFailureIsNotAShortSuccess(t *testing.T) {
	s := brandsScript()
	s.rowsErr = stderrors.New("connection reset mid-result")
	p := newProvider(t, s, Config{})

	objs, err := p.List(context.Background())
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	if objs != nil {
		t.Fatalf("objects = %#v, want nil: a truncated enumeration is not a short success", objs)
	}
	if !stderrors.Is(err, s.rowsErr) {
		t.Fatalf("cause is not reachable with errors.Is: %v", err)
	}
}

// The per-query timeout bounds the list statement exactly as it bounds a fetch.
func TestListAppliesTheTimeoutToTheStatement(t *testing.T) {
	s := brandsScript()
	p := newProvider(t, s, Config{})
	if _, err := p.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	deadline, ok := s.deadline()
	if !ok {
		t.Fatalf("the list statement ran without a deadline")
	}
	if d := time.Until(deadline); d <= 0 || d > DefaultTimeout {
		t.Fatalf("deadline is %v away, want within the %v default", d, DefaultTimeout)
	}
}

func TestListTimeoutIsOverridableAndFires(t *testing.T) {
	s := brandsScript()
	s.delay = 2 * time.Second
	p := newProvider(t, s, Config{Timeout: 20 * time.Millisecond})

	start := time.Now()
	_, err := p.List(context.Background())
	mustCode(t, err, aerr.APERTURE_SQL_PROVIDER_QUERY)
	if !stderrors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline-exceeded cause, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("List took %v; the configured 20ms timeout did not bound the statement", elapsed)
	}
}

// Query runs the SAME statement List runs — the predicates are never templated
// into the developer's SQL — and applies Filter.Fields in Go.
func TestQueryRunsTheSameStatementAndFiltersInGo(t *testing.T) {
	s := brandsScript()
	p := newProvider(t, s, Config{})

	objs, err := p.Query(context.Background(), provider.Filter{
		Fields: map[string]any{"tier": "gold"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got, want := ids(objs), []string{"brand:1", "account:acme/brand:3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identities = %v, want %v", got, want)
	}
	_, stmtRun, args := s.observed()
	if stmtRun != allStmt {
		t.Fatalf("statement = %q, want the unmodified get-all statement %q", stmtRun, allStmt)
	}
	if len(args) != 0 {
		t.Fatalf("Query bound %d parameters; a predicate reached the SQL: %v", len(args), args)
	}
}

// The seam where this epic meets the last one: E2-S2's []byte -> JSON mapping
// produces the []any that E1's MatchFields matches by MEMBERSHIP. Neither half
// is useful without the other, so it is tested end to end.
func TestQueryMatchesAJSONArrayColumnByMembership(t *testing.T) {
	cases := []struct {
		name string
		want any
		ids  []string
	}{
		{"a tag two rows carry", "launch", []string{"brand:1", "brand:2"}},
		{"a tag two other rows carry", "premium", []string{"brand:1", "account:acme/brand:3"}},
		{"a tag nothing carries", "retired", nil},
		// Equality against the whole array, not membership, when the want is
		// itself a container.
		{"the whole array", []any{"premium", "launch"}, []string{"brand:1"}},
		{"a partial array is not a subset match", []any{"premium"}, []string{"account:acme/brand:3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t, brandsScript(), Config{})
			objs, err := p.Query(context.Background(), provider.Filter{
				Fields: map[string]any{"tags": tc.want},
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := ids(objs)
			if len(got) == 0 && len(tc.ids) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.ids) {
				t.Fatalf("identities = %v, want %v", got, tc.ids)
			}
		})
	}
}

// Comparison is TYPED, and it is typed because these are the rules engine's own
// semantics: an Enumerate that selected what a Check then denies over the same
// value is the disagreement the Fields contract exists to prevent. A database
// would coerce '5' to 5 here; MatchFields must not.
func TestQueryComparesByTypeNotByStringRendering(t *testing.T) {
	cases := []struct {
		name  string
		want  any
		count int
	}{
		{"an int against an int64 column", 5, 1},
		{"a float against an int64 column", float64(5), 1},
		{"the string spelling of the same number", "5", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t, brandsScript(), Config{})
			objs, err := p.Query(context.Background(), provider.Filter{
				Fields: map[string]any{"seats": tc.want},
			})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(objs) != tc.count {
				t.Fatalf("matched %v, want %d object(s)", ids(objs), tc.count)
			}
		})
	}
}

// An absent field never matches — not even a nil want. A NULL column omits its
// field, so this is the absent-vs-zero rule reaching all the way to the filter.
func TestQueryNeverMatchesAnAbsentField(t *testing.T) {
	s := &script{
		cols: []string{"id", "tier", "retired_on"},
		rows: [][]driver.Value{
			{"brand:1", "gold", nil},
			{"brand:2", "silver", "2026-01-02"},
		},
	}
	p := newProvider(t, s, Config{})
	for _, want := range []any{nil, "2026-01-02"} {
		objs, err := p.Query(context.Background(), provider.Filter{
			Fields: map[string]any{"retired_on": want},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		for _, o := range objs {
			if o.ID.String() == "brand:1" {
				t.Fatalf("the row whose retired_on is NULL matched want %#v", want)
			}
		}
	}
}

// Every predicate must hold: Fields is an AND.
func TestQueryAndsItsPredicates(t *testing.T) {
	p := newProvider(t, brandsScript(), Config{})
	objs, err := p.Query(context.Background(), provider.Filter{
		Fields: map[string]any{"tier": "gold", "tags": "launch"},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got, want := ids(objs), []string{"brand:1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identities = %v, want %v", got, want)
	}
}

// Pattern and Limit are enforced on the results, as the Filter contract allows a
// provider to do — and doing it here is what makes a scope-bounded enumeration
// stop early instead of materialising the whole type.
func TestQueryEnforcesPatternAndLimit(t *testing.T) {
	t.Run("pattern", func(t *testing.T) {
		pat := identity.MustParsePattern("account:acme/brand:*")
		p := newProvider(t, brandsScript(), Config{})
		objs, err := p.Query(context.Background(), provider.Filter{Pattern: &pat})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if got, want := ids(objs), []string{"account:acme/brand:3"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("identities = %v, want %v", got, want)
		}
	})

	t.Run("limit", func(t *testing.T) {
		p := newProvider(t, brandsScript(), Config{})
		objs, err := p.Query(context.Background(), provider.Filter{Limit: 2})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if got, want := ids(objs), []string{"brand:1", "brand:2"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("identities = %v, want %v", got, want)
		}
	})

	t.Run("the limit counts what the filter kept, not rows read", func(t *testing.T) {
		p := newProvider(t, brandsScript(), Config{})
		objs, err := p.Query(context.Background(), provider.Filter{
			Fields: map[string]any{"tier": "gold"},
			Limit:  1,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if got, want := ids(objs), []string{"brand:1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("identities = %v, want %v", got, want)
		}
	})

	t.Run("a non-positive limit means no limit", func(t *testing.T) {
		p := newProvider(t, brandsScript(), Config{})
		objs, err := p.Query(context.Background(), provider.Filter{Limit: 0})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(objs) != 3 {
			t.Fatalf("matched %v, want every row", ids(objs))
		}
	})
}

// The zero Filter selects everything, so List and Query agree.
func TestQueryWithTheZeroFilterEqualsList(t *testing.T) {
	listed, err := newProvider(t, brandsScript(), Config{}).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	queried, err := newProvider(t, brandsScript(), Config{}).Query(context.Background(), provider.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !reflect.DeepEqual(ids(listed), ids(queried)) {
		t.Fatalf("List %v != Query %v", ids(listed), ids(queried))
	}
}

// provider.Metadata is read-only TRANSITIVELY, which only holds if every object
// gets its own containers. Two rows carrying the identical driver buffer must
// not come back sharing a slice.
func TestEnumeratedObjectsGetFreshNestedContainers(t *testing.T) {
	shared := []byte(`["premium","launch"]`)
	s := &script{
		cols: []string{"id", "tags"},
		rows: [][]driver.Value{
			{"brand:1", shared},
			{"brand:2", shared},
		},
	}
	p := newProvider(t, s, Config{})
	objs, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	a, ok := objs[0].Metadata["tags"].([]any)
	if !ok {
		t.Fatalf("tags = %#v, want a []any", objs[0].Metadata["tags"])
	}
	b := objs[1].Metadata["tags"].([]any)
	a[0] = "mutated"
	if b[0] != "premium" {
		t.Fatalf("two objects share one nested slice; the read-only contract is not transitive")
	}
	if _, same := any(objs[0].Metadata).(provider.Metadata); !same {
		t.Fatalf("unexpected metadata type")
	}
	if fmt.Sprintf("%p", objs[0].Metadata) == fmt.Sprintf("%p", objs[1].Metadata) {
		t.Fatalf("two objects share one metadata map")
	}
}

// The point of the whole story, one layer up: the Registry enumerates through
// Query, bounds it with the grant's pattern, and caches what comes back — so a
// following Fetch of an enumerated object needs no second statement.
//
// The warm is CONDITIONAL (E3-S6): the Registry asks
// provider.FetchCompleteLister, and a *Provider answers by comparing the two
// statements' real column projections. So this fixture has to give the fake a
// get_one result whose columns are the get_all's columns MINUS the id column,
// which is what a correctly paired document looks like — and it must run the
// fetch first, because a projection the provider has not observed yet is one it
// will not assume anything about. TestAListingWithANarrowerProjectionDoesNotWarm
// is the other side of the condition.
func TestRegistryEnumeratesAndCachesThroughQuery(t *testing.T) {
	s := brandsScript()
	// The matching get_one: the same fields, one row, no id column (a fetch's
	// identity is the caller's input, not something the row carries).
	s.fetchCols = []string{"tier", "seats", "tags"}
	s.fetchRows = [][]driver.Value{{"gold", int64(5), []byte(`["premium","launch"]`)}}
	p := newProvider(t, s, Config{})
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)

	// One fetch of an unrelated object, so the provider has seen its own get_one
	// projection before the enumeration asks whether the two agree.
	if _, err := reg.Fetch(context.Background(), identity.MustParse("brand:9")); err != nil {
		t.Fatalf("priming Fetch: %v", err)
	}

	got, err := reg.List(context.Background(), "brand", identity.MustParsePattern("brand:*"), 10)
	if err != nil {
		t.Fatalf("Registry.List: %v", err)
	}
	want := []string{"brand:1", "brand:2"}
	out := make([]string, 0, len(got))
	for _, id := range got {
		out = append(out, id.String())
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("identities = %v, want %v", out, want)
	}

	calls, _, _ := s.observed()
	md, err := reg.Fetch(context.Background(), identity.MustParse("brand:1"))
	if err != nil {
		t.Fatalf("Registry.Fetch: %v", err)
	}
	if md["tier"] != "gold" {
		t.Fatalf("metadata = %#v", md)
	}
	if after, _, _ := s.observed(); after != calls {
		t.Fatalf("Fetch ran %d extra statement(s); the enumeration did not populate the cache", after-calls)
	}
}
