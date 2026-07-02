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
// # File shape
//
// The first row is a header. One column MUST be named "id" and holds each
// object's canonical identity string (e.g. "brand:1", "app:be", or a
// hierarchical "account:acme/brand:1"); its terminal segment type is the
// object-type the provider is registered under. Every other column becomes a
// metadata field keyed by the column name.
//
// A column name may carry a "name:type" suffix so its cells are coerced to a
// typed value the rules engine reads as a real type rather than a bare string:
//
//	id,category_id,seats:int,active:bool,budget:float
//
// Supported types: string (the default, no suffix), int (stored as int64),
// float (float64), and bool. An empty cell omits that field for the row, so a
// rule can supply its own default. A missing "id" column, a duplicate id, a
// wrong column count, or a value that will not coerce to its declared type is an
// APERTURE_CONFIG_INVALID error; a malformed id passes through as the identity
// package's APERTURE_IDENTITY_INVALID.
//
// # Loading and the read-only contract
//
// The file is read once, lazily, on the first Fetch/List/Query and held in
// memory; Reload re-reads it. Per the provider.Metadata contract every returned
// map is owned by the provider and MUST be treated as read-only: the provider
// never mutates a map in place — Reload builds a fresh set and swaps it in, so
// maps already handed to (and cached by) the Registry stay immutable.
//
// Dependencies stay minimal: csvprovider imports only errors, identity, and
// provider, plus the standard library (pure-Go, CGO-free).
package csvprovider

import (
	"context"
	"encoding/csv"
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

// compile-time assertion: a *Provider is a usable ObjectProvider.
var _ provider.ObjectProvider = (*Provider)(nil)

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

// Query returns the objects satisfying filter. Filter.Fields are matched by
// string-equality against each object's metadata (a field absent from an object
// never matches); Filter.Pattern and Filter.Limit are honoured directly. The
// Registry re-enforces Pattern and Limit, so honouring them here is an
// optimisation that also makes Query correct when called standalone.
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
		if !matchFields(md, filter.Fields) {
			continue
		}
		out = append(out, provider.Object{ID: id, Metadata: md})
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// matchFields reports whether md satisfies every predicate in fields by
// string-equality. An empty/nil fields map matches everything.
func matchFields(md provider.Metadata, fields map[string]any) bool {
	for k, want := range fields {
		got, ok := md[k]
		if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

// column describes one non-id header column and where to read it in each row.
type column struct {
	name  string
	typ   string
	index int
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
// id order.
func parse(r io.Reader) (map[string]provider.Metadata, []identity.Identity, error) {
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
	order := make([]identity.Identity, 0, len(rows)-1)
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
		id, err := identity.Parse(rawID)
		if err != nil {
			return nil, nil, err // passes through APERTURE_IDENTITY_INVALID
		}
		key := id.String()
		if _, dup := byID[key]; dup {
			return nil, nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: duplicate id", map[string]any{"line": line, "id": key})
		}
		md := make(provider.Metadata, len(cols))
		for _, c := range cols {
			cell := strings.TrimSpace(row[c.index])
			if cell == "" {
				continue // omit an empty field; a rule can default it
			}
			val, err := coerce(cell, c.typ)
			if err != nil {
				return nil, nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
					"csvprovider: cannot coerce cell to its declared type",
					map[string]any{"line": line, "field": c.name, "type": c.typ, "value": cell})
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
		switch typ {
		case "string", "int", "float", "bool":
		default:
			return nil, -1, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
				"csvprovider: unknown column type",
				map[string]any{"name": name, "type": typ})
		}
		cols = append(cols, column{name: name, typ: typ, index: i})
	}
	if idCol < 0 {
		return nil, -1, aerr.New(aerr.APERTURE_CONFIG_INVALID,
			`csvprovider: header has no "id" column`)
	}
	return cols, idCol, nil
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
