// Package csvprovider implements provider.ObjectProvider backed by a CSV file,
// so a host can link object-type instances to real data during development
// before a database-backed provider exists. It is a drop-in adapter: register a
// *Provider under an object-type in a provider.Registry exactly as a future
// SQL-backed provider would be, and the Registry's cache, invalidation, and
// rules wiring are unchanged.
//
//	reg := provider.NewRegistry()
//	reg.MustRegister("brand", csvprovider.New("brands.csv"), provider.WithTTL(0))
//	reg.MustRegister("app",   csvprovider.New("apps.csv"),   provider.WithTTL(0))
//	// swapping to a database later changes only these two lines.
//
// # Two seams, one file format
//
// The same file format also serves the ATTRIBUTE seam: Attributes
// (attributes.go) is a provider.AttributeProvider for one attribute slot, built
// from a file whose id column holds BARE subject ids rather than identities.
//
//	attrs, err := csvprovider.NewAttributes("users.csv")
//	areg.MustRegister(provider.AttributeSlotUser, attrs, provider.WithTTL(0))
//
// Everything documented below — the header grammar, the type suffixes, the
// coercions, the empty-cell policy, the errors — is the shared value model's
// spelling and holds identically for both, because both read through one walk
// (parseTable). One model, one spelling, whether the row describes a document or
// a user. The id column is where the two differ, and only there; see the
// Attributes type doc.
//
// # File shape
//
// The first row is a header. One column MUST be named "id" and holds each
// object's canonical identity string (e.g. "brand:1", "app:be", or a
// hierarchical "account:acme/brand:1"); its terminal segment type is the
// object-type the provider is registered under. Every other column becomes a
// metadata field keyed by the column name.
//
// A column name may carry a type suffix so its cells are coerced to a typed
// value the rules engine reads as a real type rather than a bare string. The
// full grammar is:
//
//	name:type[<elem>][(delim)]
//
// Scalar types are string (the default, no suffix), int (stored as int64),
// float (float64), and bool:
//
//	id,category_id,seats:int,active:bool,budget:float
//
// # Dates
//
// The types "date" and "datetime" declare a column to hold dates, so every cell
// is validated and canonicalised at LOAD through the shared date value model
// (provider.ParseDateValue) instead of travelling as an unexamined string:
//
//	id,hired_at:date,last_seen:datetime
//	brand:1,2026-03-04,2026-03-04T12:30:00Z
//
// The point is where the failure lands. A typo'd or impossible date in an
// untyped column is a perfectly good string that no rule can compare, so it
// becomes a silent deny at decision time, months later, in production; declared
// as a date it is a hard error on the line and column that hold it.
//
// A date is stored as its CANONICAL string — "2006-01-02" for date and
// "2006-01-02T15:04:05Z" for datetime — not as the cell was written, so two rows
// naming one instant are one string and a Filter.Fields equality predicate over
// the column works. The value model is unchanged by this: a date is an ordinary
// string scalar, and ValidateField still runs on it.
//
// Accepted, rejected, and why the offset policy is what it is are all the value
// model's rules, not this loader's — see provider.ParseDateValue. Two
// consequences are worth stating here because they are the ones a CSV author
// meets:
//
//   - An explicit offset is a load ERROR, not a conversion.
//     2026-01-01T00:00:00+05:00 means January 1st to whoever wrote it, and its
//     UTC instant is 2025-12-31T19:00:00Z — converting silently moves the year.
//     A Z suffix is accepted, and so is an offset-free timestamp (read as UTC).
//   - The declared type fixes the granularity. A ":date" column takes days and a
//     ":datetime" column takes instants; the other form is rejected rather than
//     quietly widened to midnight.
//
// An empty cell omits the field, exactly as an empty scalar cell does — an
// absent date is meaningfully different from any date, and a zero time would
// silently satisfy every "before" rule written against the column.
//
// There is no ":list<date>": arrays of dates are out of scope and are rejected
// by name rather than by accident. There is no time-of-day type either.
//
// # Arrays
//
// The type "list" produces a real []any, which is what makes membership rules
// ("premium" in object.tags) decide correctly instead of string-matching a
// delimited blob — a blob match also matches "premium-trial" and grants access
// it shouldn't:
//
//	id,tags:list,seats:list<int>,aliases:list(;)
//	brand:1,premium|launch,3|5,acme;acme-co
//
// The optional <elem> coerces every element through the SAME scalar vocabulary
// (list<int>, list<float>, list<bool>; list alone means list<string>). Element
// typing is not decoration: the expression evaluator does no numeric/string
// coercion, so 5 in object.seats is FALSE against the strings ["3","5"] — a
// silent wrong answer, the worst failure mode an access-control engine has.
//
// The optional (delim) overrides the element separator for that column only;
// the default is "|". There is NO escape syntax: a value that must contain the
// delimiter needs a column delimiter that it does not contain. A stray, doubled,
// leading, or trailing delimiter — which is what a delimiter inside a value
// looks like to the parser — yields an empty element and is a hard error at
// parse rather than a silently mis-split row.
//
// # Nested objects
//
// The type "json" parses its cell as JSON so a rule can read a structured value
// with a dotted path — object.owner.dept. The cell MUST decode to a JSON OBJECT
// at the top level; an array, a scalar, or null is rejected, because "list"
// stays the only array path. That keeps "arrays hold scalars, objects hold
// structure" true everywhere and the operator set flat.
//
// Because a JSON object contains commas and quotes, the cell has to be quoted
// per RFC 4180 — the whole cell in double quotes, with every inner double quote
// doubled. This is the least obvious part of the feature, so here it is in full:
//
//	id,owner:json
//	brand:1,"{""dept"":""eng"",""lead"":""alice""}"
//	brand:2,"{""dept"":""ops"",""tags"":[""oncall"",""eu""]}"
//
// Below the top level it is ordinary JSON, bounded by the shared value model's
// depth and size caps (provider.ValueLimits): {"dept":"eng","tags":["a","b"]}
// is fine, {"members":[{"id":1}]} is not — arrays of objects are rejected at
// any position.
//
// Numbers decode through json.Decoder with UseNumber, so no precision is lost
// before the type is chosen, and are then normalised the SAME way the scalar
// columns coerce: a literal that is an exact integer fitting int64 becomes an
// int64 (as :int and :list<int> do), and anything else becomes a float64 (as
// :float and :list<float> do). A number the machine cannot represent at all is
// a hard error rather than a silent Inf. That consistency is what makes
// cross-column comparisons — object.owner.seats == object.seats — behave.
//
// # Empty cells
//
// An empty cell in a SCALAR or DATE column omits that field for the row, so a
// rule can supply its own default. An empty cell in a JSON column does the same: an
// object that is absent is meaningfully different from one that is empty, and
// reading an absent object is safe. An empty cell in a LIST column is the one
// departure: it yields an empty list ([]any{}), not an absent field, so "x" in
// object.tags is a definite false rather than an evaluation against nil.
//
// # Querying
//
// Query implements provider.Filter's Fields contract by calling
// provider.MatchFields, so it needs no rules of its own: a list column matches by
// MEMBERSHIP and every other column by typed equality.
//
//	Filter{Fields: map[string]any{"tags": "premium"}}   // rows whose tags contain premium
//	Filter{Fields: map[string]any{"seats": 5}}          // rows whose seats list contains 5
//	Filter{Fields: map[string]any{"tier": "gold"}}      // scalar equality
//
// Typing carries through from the column: a :list<int> column holds int64
// elements, so 5 matches and "5" does not — the same answer a rule's `in` gives
// over the same data, which is what keeps Query usable for bounding an
// enumeration.
//
// # Errors
//
// A missing "id" column, a duplicate id, a wrong column count, an unknown type
// or malformed type suffix, a value that will not coerce to its declared type,
// a list cell with an empty element, a json cell that is not valid JSON or
// does not decode to an object, or a date cell that is not a canonical date of
// its column's granularity is an APERTURE_CONFIG_INVALID error naming the
// column (and, for a cell, the line and the offending element). A date
// rejection additionally carries the provider.DateReason, so a caller branches
// on the cause rather than on message text, and — like the json rejection —
// never carries the cell, which is host data and, for a date, frequently
// personal data. A malformed id
// passes through as the identity package's APERTURE_IDENTITY_INVALID. Every
// parsed value is additionally checked against the shared metadata value model
// (provider.ValidateField) before it is stored, so a shape, depth, or size
// violation fails the load as APERTURE_METADATA_INVALID instead of surfacing as
// a runtime error on the Check hot path.
//
// # Loading and the read-only contract
//
// The file is read once, lazily, on the first Fetch/List/Query and held in
// memory; Reload re-reads it. Per the provider.Metadata contract every returned
// map is owned by the provider and MUST be treated as read-only: the provider
// never mutates a map in place — Reload builds a fresh set and swaps it in, so
// maps already handed to (and cached by) the Registry stay immutable. The
// contract is transitive now that a value may be a slice or a map: every list
// cell is parsed into a slice, and every json cell decoded into a map, allocated
// for that row alone, so no two rows (and no two loads) ever share one.
//
// Dependencies stay minimal: csvprovider imports only errors, identity, and
// provider, plus the standard library (pure-Go, CGO-free).
package csvprovider

import (
	"context"
	"encoding/csv"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
	"github.com/frankbardon/aperture/provider"
)

// compile-time assertion: a *Provider is a usable ObjectProvider, and one whose
// listings may warm the Registry's metadata cache.
var (
	_ provider.ObjectProvider      = (*Provider)(nil)
	_ provider.FetchCompleteLister = (*Provider)(nil)
)

// Provider is a CSV-file-backed ObjectProvider for one object-type. It is safe
// for concurrent use: the file loads once under a write lock and every read
// serves the in-memory set under a read lock. Reload swaps the set atomically.
type Provider struct {
	path string

	mu     sync.RWMutex
	loaded bool
	byID   map[string]provider.Metadata
	order  []identity.Identity // preserves file order for stable List/Query output
}

// New returns a Provider that reads path on first use. It never fails here; a
// bad file surfaces as an APERTURE_CONFIG_INVALID error from the first
// Fetch/List/Query (or eagerly from Reload). Register it under the object-type
// whose instances the file describes.
func New(path string) *Provider {
	return &Provider{path: path}
}

// FromReader builds an already-loaded Provider from r. It has no path, so Reload
// returns APERTURE_CONFIG_INVALID; use it for embedded or in-memory data (and
// tests) rather than a file on disk.
func FromReader(r io.Reader) (*Provider, error) {
	byID, order, err := parse(r)
	if err != nil {
		return nil, err
	}
	return &Provider{byID: byID, order: order, loaded: true}, nil
}

// Reload re-reads the file and atomically replaces the in-memory set. Call it
// after the underlying file changes, then Registry.InvalidateType to drop stale
// cache entries. If parsing fails the current set is left untouched.
func (p *Provider) Reload() error {
	if p.path == "" {
		return aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: reader-backed provider cannot be reloaded")
	}
	byID, order, err := parseFile(p.path)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.byID, p.order, p.loaded = byID, order, true
	p.mu.Unlock()
	return nil
}

// ensure lazily loads the file on first use. A load failure is returned and
// retried on the next call (the file may not exist yet at construction time).
func (p *Provider) ensure() error {
	p.mu.RLock()
	loaded := p.loaded
	p.mu.RUnlock()
	if loaded {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return nil
	}
	byID, order, err := parseFile(p.path)
	if err != nil {
		return err
	}
	p.byID, p.order, p.loaded = byID, order, true
	return nil
}

// Fetch returns id's metadata, or APERTURE_NOT_FOUND when the file has no row
// for it (so the Registry can distinguish absent from an operational failure).
func (p *Provider) Fetch(_ context.Context, id identity.Identity) (provider.Metadata, error) {
	if err := p.ensure(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	md, ok := p.byID[id.String()]
	p.mu.RUnlock()
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_NOT_FOUND,
			"csvprovider: no object with this id", map[string]any{"id": id.String()})
	}
	return md, nil
}

// List returns every object in file order.
func (p *Provider) List(_ context.Context) ([]provider.Object, error) {
	if err := p.ensure(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]provider.Object, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, provider.Object{ID: id, Metadata: p.byID[id.String()]})
	}
	return out, nil
}

// Query returns the objects satisfying filter. Filter.Fields go through
// provider.MatchFields — the shared implementation of the Fields contract, so a
// list column matches by MEMBERSHIP ("premium" selects every row whose tags
// array contains it) and everything else by typed equality; Filter.Pattern and
// Filter.Limit are honoured directly. The Registry re-enforces Pattern and
// Limit, so honouring them here is an optimisation that also makes Query correct
// when called standalone.
func (p *Provider) Query(_ context.Context, filter provider.Filter) ([]provider.Object, error) {
	if err := p.ensure(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]provider.Object, 0, len(p.order))
	for _, id := range p.order {
		if filter.Pattern != nil && !filter.Pattern.Matches(id) {
			continue
		}
		md := p.byID[id.String()]
		if !provider.MatchFields(md, filter.Fields) {
			continue
		}
		out = append(out, provider.Object{ID: id, Metadata: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// ListedMetadataMatchesFetch is this loader's unconditional
// provider.FetchCompleteLister promise: a listing through it may warm the
// Registry's per-type metadata cache.
//
// It is true by construction. One CSV row becomes one bag, held once in p.byID,
// and Fetch, List and Query all hand back that same map by reference — there is a
// single parse of a single file behind all three, no second projection that could
// carry fewer or more columns, and Query selects whole objects rather than
// narrowing them. Reload replaces the whole set atomically, so a listing and a
// fetch either both see the old file or both see the new one; neither can see a
// row shaped differently from the other's.
func (p *Provider) ListedMetadataMatchesFetch() bool { return true }

// column describes one non-id header column and where to read it in each row.
// elem and delim are set only for a list column; they stay empty for a scalar.
type column struct {
	name  string
	typ   string // "string", "int", "float", "bool", "date", "datetime", "list", or "json"
	elem  string // element type of a list column ("string" unless <elem> says otherwise)
	delim string // element separator of a list column (defaultListDelim unless (delim) says otherwise)
	index int
}

const (
	// typeList is the column type that yields a []any.
	typeList = "list"
	// typeJSON is the column type that yields a map[string]any decoded from the
	// cell. It is the only column type that produces structure, and it produces
	// nothing else: a cell decoding to an array, a scalar, or null is rejected
	// so that "list" stays the single array path.
	typeJSON = "json"
	// typeDate is the column type whose cells are calendar days, validated and
	// canonicalised through the shared date value model and stored as the string
	// scalar that model defines (provider.DateLayout).
	typeDate = "date"
	// typeDateTime is the timestamp counterpart of typeDate
	// (provider.DateTimeLayout). It is spelled out rather than folded into
	// typeDate with a modifier because the two are different declarations: a
	// column is a day or it is an instant, and mixing the two in one column is
	// exactly what this type rejects.
	typeDateTime = "datetime"
)

// defaultListDelim separates the elements of a list cell unless the header
// overrides it per column with a "(delim)" suffix.
const defaultListDelim = "|"

// isScalarType reports whether t is one of the scalar column types. The same set
// doubles as the legal list element types, deliberately: reusing the scalar
// vocabulary means an author who knows ":int" already knows ":list<int>".
func isScalarType(t string) bool {
	switch t {
	case "string", "int", "float", "bool":
		return true
	}
	return false
}

// isDateType reports whether t is one of the two date column types.
//
// They are deliberately NOT members of isScalarType even though each produces a
// string scalar, because that set doubles as the legal list element types and an
// array of dates is out of scope: ":list<date>" must be rejected with a reason,
// not accepted by inheritance. Keeping the two sets apart is what makes the
// rejection a decision rather than an omission.
func isDateType(t string) bool {
	return t == typeDate || t == typeDateTime
}

// dateLayoutOf names the one canonical layout a date column accepts, for a
// diagnostic. It is the layout, never a value.
func dateLayoutOf(typ string) string {
	if typ == typeDateTime {
		return provider.DateTimeLayout
	}
	return provider.DateLayout
}

// dateGranularityOf is the granularity a date column declares its cells carry.
func dateGranularityOf(typ string) provider.DateGranularity {
	if typ == typeDateTime {
		return provider.GranularityDateTime
	}
	return provider.GranularityDate
}

// parseFile opens path and parses it, wrapping an open failure as
// APERTURE_CONFIG_INVALID.
func parseFile(path string) (map[string]provider.Metadata, []identity.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, aerr.Wrap(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: cannot open data file", err)
	}
	defer f.Close()
	return parse(f)
}

// parse reads a whole CSV document into an id-keyed metadata map plus the file's
// id order. It is parseTable keyed the OBJECT way — see objectKey.
func parse(r io.Reader) (map[string]provider.Metadata, []identity.Identity, error) {
	return parseTable(r, objectKey)
}

// objectKey is the object seam's row key: the id cell is a canonical IDENTITY,
// and a malformed one passes through as the identity package's
// APERTURE_IDENTITY_INVALID rather than being reclassified here.
func objectKey(raw string, _ int) (identity.Identity, string, error) {
	id, err := identity.Parse(raw)
	if err != nil {
		return identity.Identity{}, "", err
	}
	return id, id.String(), nil
}

// rowKey turns one row's raw id cell into the key its seam indexes by, plus the
// canonical STRING that keys the metadata map. The two differ because the object
// seam is ordered by identity.Identity while its map is keyed by that identity's
// canonical text, and the attribute seam's key is a bare string that is both.
//
// It also decides what a legal key IS for the seam, which is the whole reason
// this indirection exists: an object id is an identity and is parsed, an
// attribute key is an opaque handle and is not (see attributes.go).
type rowKey[K any] func(raw string, line int) (K, string, error)

// parseTable is the ONE CSV walk, shared by both seams this package serves.
//
// Everything below the key — the header, the type suffixes, the coercions, the
// list/json/date rules, the value-model check, the empty-cell policy — is the
// value model's spelling and is identical whether the row describes a document
// or a user. There is one model and one spelling, so there is one walk; the seam
// supplies only how a row is KEYED.
//
// Errors name the line, and the row's own key rejection is returned verbatim so
// its code and its fixups survive.
func parseTable[K any](r io.Reader, keyOf rowKey[K]) (map[string]provider.Metadata, []K, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, nil, aerr.Wrap(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: malformed CSV", err)
	}
	if len(rows) == 0 {
		return nil, nil, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: file has no header row")
	}
	header := rows[0]
	cols, idCol, err := parseHeader(header)
	if err != nil {
		return nil, nil, err
	}

	byID := make(map[string]provider.Metadata, len(rows)-1)
	order := make([]K, 0, len(rows)-1)
	for i, row := range rows[1:] {
		line := i + 2 // 1-based, past the header
		if len(row) != len(header) {
			return nil, nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: row has the wrong column count",
				map[string]any{"line": line, "want": len(header), "got": len(row)})
		}
		rawID := strings.TrimSpace(row[idCol])
		if rawID == "" {
			return nil, nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: row has an empty id", map[string]any{"line": line})
		}
		id, key, err := keyOf(rawID, line)
		if err != nil {
			// Verbatim: the seam's key rejection already carries the code whose
			// fixups the author needs (APERTURE_IDENTITY_INVALID for an object,
			// APERTURE_ATTRIBUTE_PROVIDER_INVALID for an attribute key that names
			// nobody), and re-stamping it here would replace the remedy with this
			// package's generic one.
			return nil, nil, err
		}
		if _, dup := byID[key]; dup {
			return nil, nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: duplicate id", map[string]any{"line": line, "id": key})
		}
		md := make(provider.Metadata, len(cols))
		for _, c := range cols {
			cell := strings.TrimSpace(row[c.index])
			var val any
			switch {
			case c.typ == typeList:
				// A list cell always produces a field, empty cell included:
				// [] is a definite "member of nothing", where an absent field
				// would leave the rule evaluating against nil.
				list, err := splitList(cell, c, line)
				if err != nil {
					return nil, nil, err
				}
				val = list
			case c.typ == typeJSON:
				if cell == "" {
					// Unlike a list, an empty json cell omits the field: an
					// absent object is meaningfully different from an empty one,
					// and reading an absent object is safe.
					continue
				}
				obj, err := decodeObject(cell, c, line)
				if err != nil {
					return nil, nil, err
				}
				val = obj
			case isDateType(c.typ):
				if cell == "" {
					// A date column follows the SCALAR rule, not the list rule:
					// an empty cell omits the field. An absent date is
					// meaningfully different from any date, and the alternative
					// — storing a zero time — would silently satisfy every
					// "before <anything>" rule ever written against the column.
					continue
				}
				s, err := coerceDate(cell, c, line)
				if err != nil {
					return nil, nil, err
				}
				val = s
			default:
				if cell == "" {
					continue // omit an empty scalar field; a rule can default it
				}
				v, err := coerce(cell, c.typ)
				if err != nil {
					return nil, nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
						"csvprovider: cannot coerce cell to its declared type",
						map[string]any{"line": line, "field": c.name, "type": c.typ, "value": cell})
				}
				val = v
			}
			// The shared value model is the single authority on what a metadata
			// value may be; the loader never re-implements those rules. A
			// violation fails the load rather than reaching the Check hot path.
			if err := provider.ValidateField(c.name, val); err != nil {
				return nil, nil, aerr.Wrapf(aerr.APERTURE_METADATA_INVALID, err,
					"csvprovider: line %d: metadata field %q rejected by the value model", line, c.name)
			}
			md[c.name] = val
		}
		byID[key] = md
		order = append(order, id)
	}
	return byID, order, nil
}

// parseHeader validates the header row and returns the metadata columns plus the
// index of the required "id" column.
func parseHeader(header []string) (cols []column, idCol int, err error) {
	idCol = -1
	seen := make(map[string]bool, len(header))
	for i, h := range header {
		h = strings.TrimSpace(h)
		name, typ := h, "string"
		if n, tp, ok := strings.Cut(h, ":"); ok {
			name, typ = n, tp
		}
		if name == "" {
			return nil, -1, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: empty column name", map[string]any{"index": i})
		}
		if seen[name] {
			return nil, -1, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: duplicate column name", map[string]any{"name": name})
		}
		seen[name] = true
		if name == "id" {
			idCol = i
			continue // the identity column, not a metadata field
		}
		c, err := parseTypeSuffix(name, typ)
		if err != nil {
			return nil, -1, err
		}
		c.index = i
		cols = append(cols, c)
	}
	if idCol < 0 {
		return nil, -1, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			`csvprovider: header has no "id" column`)
	}
	return cols, idCol, nil
}

// parseTypeSuffix parses the part of a header cell after the ":" into a column,
// per the grammar type[<elem>][(delim)]. The optional groups are stripped from
// the right in the order they must appear, so a reordered or unterminated suffix
// leaves a stray bracket behind and is rejected rather than silently accepted.
// Every failure is APERTURE_CONFIG_INVALID naming the column.
func parseTypeSuffix(name, suffix string) (column, error) {
	rest := suffix

	// (delim) — the outermost optional group.
	delim, delimSet := "", false
	if strings.HasSuffix(rest, ")") {
		open := strings.Index(rest, "(")
		if open < 0 {
			return column{}, malformedSuffix(name, suffix)
		}
		delim, delimSet = rest[open+1:len(rest)-1], true
		rest = rest[:open]
	}
	if strings.ContainsAny(rest, "()") {
		return column{}, malformedSuffix(name, suffix)
	}

	// <elem> — the inner optional group.
	elem, elemSet := "", false
	if strings.HasSuffix(rest, ">") {
		open := strings.Index(rest, "<")
		if open < 0 {
			return column{}, malformedSuffix(name, suffix)
		}
		elem, elemSet = rest[open+1:len(rest)-1], true
		rest = rest[:open]
	}
	if strings.ContainsAny(rest, "<>") {
		return column{}, malformedSuffix(name, suffix)
	}

	typ := rest
	// Everything but "list" is a single, undecorated type. "json", "date", and
	// "datetime" join the scalars here rather than growing a branch of their
	// own: a json cell carries its whole structure and a date cell is one
	// canonical string, so an element type or a delimiter is as meaningless on
	// them as it is on ":int" and is rejected identically.
	if typ != typeList {
		if !isScalarType(typ) && typ != typeJSON && !isDateType(typ) {
			return column{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: unknown column type",
				map[string]any{"name": name, "type": typ})
		}
		if elemSet || delimSet {
			return column{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: only a list column takes an element type or a delimiter",
				map[string]any{"name": name, "type": typ, "suffix": suffix})
		}
		return column{name: name, typ: typ}, nil
	}

	if !elemSet {
		elem = "string" // ":list" is ":list<string>"
	}
	if isDateType(elem) {
		// Named explicitly rather than swept into "unknown element type",
		// because ":list<date>" is a reasonable thing to try and the author
		// deserves to be told it is out of scope rather than left guessing that
		// they mistyped a supported element type.
		return column{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: a list column cannot hold dates; declare a single date or datetime column instead",
			map[string]any{"name": name, "elem": elem})
	}
	if !isScalarType(elem) {
		return column{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: unknown list element type; use string, int, float, or bool",
			map[string]any{"name": name, "elem": elem})
	}
	if !delimSet {
		delim = defaultListDelim
	}
	if delim == "" {
		return column{}, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: list delimiter cannot be empty",
			map[string]any{"name": name, "suffix": suffix})
	}
	return column{name: name, typ: typeList, elem: elem, delim: delim}, nil
}

// malformedSuffix is the one error for a type suffix that does not fit the
// grammar (a reordered, unterminated, or stray bracket group).
func malformedSuffix(name, suffix string) error {
	return aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
		"csvprovider: malformed column type suffix; want name:type[<elem>][(delim)]",
		map[string]any{"name": name, "suffix": suffix})
}

// splitList turns one list cell into its coerced elements. The slice it returns
// is freshly allocated for this row — no two rows and no two loads ever share
// one, which is what keeps the read-only metadata contract true at depth.
//
// An empty cell is an EMPTY list, not an absent field. An empty element (a
// stray, doubled, leading, or trailing delimiter — how a delimiter inside a
// value looks to the parser) is a hard error: there is no escape syntax, so the
// fix is a per-column delimiter the data does not contain.
func splitList(cell string, c column, line int) ([]any, error) {
	if cell == "" {
		return []any{}, nil
	}
	parts := strings.Split(cell, c.delim)
	out := make([]any, 0, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: list cell has an empty element, so a value collides with the column delimiter; "+
					`there is no escape syntax — give the column a delimiter its data does not contain, e.g. name:list(;)`,
				map[string]any{"line": line, "field": c.name, "delimiter": c.delim, "index": i, "value": cell})
		}
		v, err := coerce(part, c.elem)
		if err != nil {
			return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: cannot coerce list element to its declared element type",
				map[string]any{"line": line, "field": c.name, "elem": c.elem, "index": i, "value": part})
		}
		out = append(out, v)
	}
	return out, nil
}

// decodeObject parses one json cell into a map[string]any. The map — and every
// container inside it — is decoded fresh for this row, so no two rows and no two
// loads ever share one, which is what keeps the read-only metadata contract true
// at depth.
//
// The cell MUST decode to a JSON object. An array, a scalar, or null is rejected
// here rather than left to the value model, because "list" is the only array
// path and a scalar already has a scalar column type; below the top level it is
// ordinary JSON, which provider.ValidateField then bounds.
//
// Errors name the column and the line and carry the JSON kind or the decoder's
// message — never the cell, which is host data.
func decodeObject(cell string, c column, line int) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(cell))
	// UseNumber is the house pattern (rules/ast.go decodeScalar): it defers the
	// int/float choice to normalizeNumbers instead of floating every integer.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: json cell is not valid JSON; quote the whole cell per RFC 4180 and double every inner double quote",
			map[string]any{"line": line, "field": c.name, "type": typeJSON, "error": err.Error()})
	}
	if _, err := dec.Token(); !stderrors.Is(err, io.EOF) {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: json cell has trailing content after its JSON value",
			map[string]any{"line": line, "field": c.name, "type": typeJSON})
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: json cell must decode to a JSON object; use a list column for an array and a scalar column type for a scalar",
			map[string]any{"line": line, "field": c.name, "type": typeJSON, "kind": jsonKind(v)})
	}
	if err := normalizeObject(obj, c, line); err != nil {
		return nil, err
	}
	return obj, nil
}

// normalizeObject rewrites every json.Number below obj in place. Numbers are the
// one place a json column could disagree with the rest of the file, and a
// disagreement is invisible: the evaluator does no numeric coercion, so
// object.owner.seats == object.seats would be a silent false if one side were a
// float64 and the other an int64. The rule is therefore exactly the scalar
// columns' rule — an exact integer that fits int64 becomes an int64, everything
// else a float64 — applied at every depth.
func normalizeObject(obj map[string]any, c column, line int) error {
	for k, v := range obj {
		nv, err := normalizeValue(v, c, line)
		if err != nil {
			return err
		}
		obj[k] = nv
	}
	return nil
}

// normalizeValue applies normalizeObject's rule to one decoded value.
func normalizeValue(v any, c column, line int) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil // matches :int and :list<int>
		}
		if f, err := x.Float64(); err == nil {
			return f, nil // matches :float and :list<float>
		}
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: json cell holds a number no int64 or float64 can represent",
			map[string]any{"line": line, "field": c.name, "type": typeJSON, "value": x.String()})
	case map[string]any:
		return x, normalizeObject(x, c, line)
	case []any:
		for i, elem := range x {
			nv, err := normalizeValue(elem, c, line)
			if err != nil {
				return nil, err
			}
			x[i] = nv
		}
		return x, nil
	default:
		return v, nil
	}
}

// jsonKind names the JSON kind of a decoded value, so a rejection can say what
// the cell held without repeating the cell itself.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// coerceDate validates one date cell through the shared date value model and
// returns the CANONICAL text to store. The loader never re-implements the
// parsing, the accepted forms, or the offset policy — provider/date.go is the
// single definition, shared with the seed loader and the rules engine, so a
// value the CSV accepts is exactly a value a rule can compare.
//
// It stores the canonical rendering rather than the cell as written, so
// "2026-03-04T01:02:03.456Z" and "2026-03-04T01:02:03" both become
// "2026-03-04T01:02:03Z" on disk-to-memory. Two rows that name the same instant
// are then the same string, which is what makes a Filter.Fields equality
// predicate over a date column usable at all.
//
// The declared type also fixes the GRANULARITY. A ":date" column holds calendar
// days and a ":datetime" column holds instants; a cell carrying the other form
// is rejected rather than stored, because the column type would otherwise be a
// claim the data does not honour — and because the implicit repair (reading a
// bare day in a :datetime column as midnight) is precisely the silent assumption
// this model exists to refuse. Write the midnight out.
//
// Every rejection names the column, the line, and the field, and carries the
// DateReason so a caller can branch on the cause. It NEVER carries the cell: a
// date is frequently personal data — a birth date, a termination date — and an
// error is a thing that gets logged. That is also why the value model's own
// error is classified and then dropped rather than wrapped; it says nothing this
// one does not, and the chain is shorter for it.
func coerceDate(cell string, c column, line int) (string, error) {
	v, err := provider.ParseDateValue(cell)
	if err != nil {
		return "", aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: cannot parse cell as its declared date type",
			map[string]any{
				"line": line, "field": c.name, "type": c.typ,
				"reason": string(provider.DateReasonOf(err)), "expected": dateLayoutOf(c.typ),
			})
	}
	if want := dateGranularityOf(c.typ); v.Granularity() != want {
		return "", aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"csvprovider: cell is a valid date of the wrong granularity for its declared column type",
			map[string]any{
				"line": line, "field": c.name, "type": c.typ,
				"granularity": v.Granularity().String(), "expected": dateLayoutOf(c.typ),
			})
	}
	return v.String(), nil
}

// coerce converts a raw cell to the value for its declared column type. int is
// stored as int64 and float as float64 so the rules engine reads native types.
func coerce(cell, typ string) (any, error) {
	switch typ {
	case "string":
		return cell, nil
	case "int":
		return strconv.ParseInt(cell, 10, 64)
	case "float":
		return strconv.ParseFloat(cell, 64)
	case "bool":
		return strconv.ParseBool(cell)
	}
	return nil, fmt.Errorf("unknown type %q", typ) // unreachable: header validated
}
