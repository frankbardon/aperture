package seed

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/sqlprovider"

	// The Postgres driver. THIS is the file where Aperture takes on a database
	// driver, and it is the only one: registering "pgx" with database/sql is a
	// package-level side effect, so keeping the blank import in one named place
	// makes "which drivers does this binary link?" a one-line grep.
	//
	// pgx was chosen over lib/pq knowingly. It costs +3.42 MiB (+12.5%) on the
	// stripped binary against lib/pq's +94 KiB — but lib/pq hands back []byte for
	// numeric and uuid, which the value model cannot tell apart from jsonb, so
	// the decode-or-string rule in sqlprovider/values.go would silently turn the
	// numeric 1.50 into the float 1.5. In an authorization engine, where "5" != 5
	// is load-bearing, a silent type change is not worth 3.3 MiB.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// driverName is the database/sql driver every declared connection opens
// through. Postgres only, by decision: one engine means one dialect to document,
// one placeholder syntax in the statements a seed carries, and one driver's
// value mapping to keep sqlprovider honest about.
const driverName = "pgx"

// Documented defaults for the optional fields of a Connection. Only dsn_env is
// required; every other key exists so a deployment can tune what Aperture opens
// against the host's database, and every one of them has an answer when it is
// omitted.
const (
	// DefaultQueryTimeout bounds one provider statement when a connection sets no
	// query_timeout. It matches sqlprovider.DefaultTimeout: a fetch runs under
	// Check, and a statement that has not answered in five seconds has already
	// failed the decision it was serving.
	DefaultQueryTimeout = sqlprovider.DefaultTimeout
	// DefaultMaxOpenConns caps the pool when a connection sets no
	// max_open_conns. It is deliberately NOT database/sql's own default, which is
	// unlimited: an unbounded pool lets a burst of Checks open connections until
	// the host's server refuses them, and that failure lands on the host's own
	// application rather than on Aperture. A negative max_open_conns restores the
	// unlimited behaviour explicitly, where a reviewer can see it.
	DefaultMaxOpenConns = 10
	// DefaultMaxIdleConns is how many of those connections stay warm when a
	// connection sets no max_idle_conns. A provider's traffic is bursty — a wave
	// of Checks, then nothing — so retaining half the pool avoids paying TLS and
	// authentication again on every wave. Zero or negative retains none.
	DefaultMaxIdleConns = 5
	// DefaultConnMaxLifetime retires a connection this long after it was opened
	// when a connection sets no conn_max_lifetime. A finite lifetime is what
	// survives the classic deployment in front of a connection proxy or a
	// failover pair, where a connection kept forever eventually points at a
	// server that is no longer the primary. Set "0" for database/sql's
	// reuse-forever behaviour.
	DefaultConnMaxLifetime = 30 * time.Minute
)

// Connection declares one named database connection a kind: sql provider entry
// reads through. It is keyed by name in Document.Connections:
//
//	connections:
//	  main:
//	    dsn_env: APP_DATABASE_URL
//	    max_open_conns: 8
//	    query_timeout: 2s
//
// Like the providers: and objects: sections, this is runtime WIRING and not
// model state: Apply writes nothing for it and an export never reproduces it.
//
// # Only the NAME is shared, and the rest is this instance's ROUTE
//
// connections: is one of the four SHARED wiring sections, but what `aperture
// wiring push` writes is a MANIFEST OF NAMES: one row per name in
// apt_wiring_connections, and nothing else. Every other key below — DSNEnv, the
// three pool bounds, QueryTimeout — is deliberately absent from the schema, because
// which server, which credential and how large a pool are per-instance facts, and
// a row is copied to a second host that resolves its own.
//
// So a declaration here does double duty. On the instance that authors the wiring
// it is both the name and the route; on any instance reading shared wiring it is
// purely the ROUTE for a name the manifest already declared. There are three ways
// to supply one, and they are local to each instance: seed.WithConnectionOpener (a
// Go host builds the pool itself — the documented seam, and the only one a library
// host needs), a connections: entry under the same name in this instance's own seed
// file (used verbatim), or the conventional environment variable
// APERTURE_CONNECTION_<NAME>_DSN (the CLI's route of last resort, not a second
// mechanism). A shared name no route answers for refuses the BOOT with
// APERTURE_WIRING_CONNECTION_UNROUTED, naming every unrouted name at once.
//
// A pulled document therefore comes back with an empty dsn_env: for every
// connection — it is re-pushable but not bootable until the routes are filled in,
// and that asymmetry is the security rule made visible rather than a defect.
//
// # Why there is no dsn: key
//
// A seed file is a committed artifact. A DSN carries a password, and a password
// in version control is only ever noticed afterwards — so naming an environment
// variable is not the recommended spelling here, it is the ONLY one. A literal
// dsn: is refused by Parse with APERTURE_SQL_PROVIDER_DSN_LITERAL, before the
// document is usable, and the DSNLiteral field below exists solely so that
// refusal can happen. See Document.Connections.
type Connection struct {
	// DSNEnv names the environment variable holding this connection's DSN, in
	// either of the forms Postgres accepts ("postgres://user:pass@host/db" or
	// "host=... user=... password=..."). Required, and required to be non-empty
	// at BuildRegistryWithConnections time — an unset variable is a hard error
	// there, not a lazy failure on the first decision that needed the database.
	DSNEnv string `yaml:"dsn_env" json:"dsn_env"`
	// DSNLiteral is the FORBIDDEN dsn: key. It is decoded only so that a document
	// carrying one is rejected by name instead of being silently ignored — a
	// seed whose password key was quietly dropped would look like it worked and
	// then fail to connect, which is the worst of both outcomes. It is never
	// read as a DSN.
	DSNLiteral string `yaml:"dsn,omitempty" json:"dsn,omitempty"`
	// MaxOpenConns caps simultaneously-open connections. 0 means
	// DefaultMaxOpenConns; negative means unlimited (database/sql's own meaning).
	MaxOpenConns int `yaml:"max_open_conns,omitempty" json:"max_open_conns,omitempty"`
	// MaxIdleConns caps connections kept warm in the pool. 0 means
	// DefaultMaxIdleConns; negative retains none.
	MaxIdleConns int `yaml:"max_idle_conns,omitempty" json:"max_idle_conns,omitempty"`
	// ConnMaxLifetime retires a connection this long after it was opened, as a Go
	// duration ("30m", "1h"). Empty means DefaultConnMaxLifetime; "0" means reuse
	// forever.
	ConnMaxLifetime string `yaml:"conn_max_lifetime,omitempty" json:"conn_max_lifetime,omitempty"`
	// QueryTimeout bounds ONE provider statement, as a Go duration ("5s",
	// "500ms"). Empty means DefaultQueryTimeout. It must be positive: there is no
	// "no timeout" setting, because an unbounded statement under Check is an
	// unbounded decision. It becomes sqlprovider.Config.Timeout for every
	// provider entry referencing this connection.
	QueryTimeout string `yaml:"query_timeout,omitempty" json:"query_timeout,omitempty"`
}

// ConnectionSettings is a resolved Connection: the DSN read out of the
// environment plus the parsed pool settings, with every default already
// applied. It is what a ConnectionOpener is handed.
//
// DSN is a secret. Never format a ConnectionSettings into an error, a log line,
// or anything else a human might paste somewhere.
type ConnectionSettings struct {
	// DSN is the value read from the connection's dsn_env variable.
	DSN string
	// MaxOpenConns, MaxIdleConns and ConnMaxLifetime are passed straight to the
	// matching database/sql pool setters.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	// QueryTimeout becomes sqlprovider.Config.Timeout for every provider entry
	// referencing this connection. Always positive.
	QueryTimeout time.Duration
}

// Pool is a live database handle: everything sqlprovider needs to read through,
// plus the Close that ends its lifetime. A *sql.DB satisfies it.
//
// It is an interface rather than a *sql.DB so a test — or a host with its own
// instrumented handle — can supply one through WithConnectionOpener without a
// database being present.
type Pool interface {
	sqlprovider.Querier
	// Close releases the pool's connections. Connections.Close calls it exactly
	// once per pool.
	Close() error
}

// a *sql.DB is the Pool the default opener returns.
var _ Pool = (*sql.DB)(nil)

// ConnectionOpener opens the pooled handle for one named connection. name is
// the connections: key, for use in an error message; cfg is the resolved
// settings, whose DSN must not appear in one.
//
// WithConnectionOpener replaces the default (which opens a real pgx pool), so a
// test can build a whole registry from a YAML fixture with no database present.
type ConnectionOpener func(name string, cfg ConnectionSettings) (Pool, error)

// openPool is the default ConnectionOpener: a database/sql pool over the pgx
// driver, with the connection's pool settings applied.
//
// It does not ping. sql.Open is lazy by design, and making registry
// construction wait on a round-trip to the host's database would make every
// `aperture check` a network operation — including the ones that never touch a
// SQL-backed type. What IS eager is everything Aperture can decide on its own:
// the environment variable, the durations, the connection names, and the
// statements.
func openPool(name string, cfg ConnectionSettings) (Pool, error) {
	db, err := sql.Open(driverName, cfg.DSN)
	if err != nil {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			fmt.Sprintf("seed: opening connection %q failed: %s", name, redactDSN(err.Error(), cfg.DSN)),
			map[string]any{"connection": name, "driver": driverName})
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	return db, nil
}

// Connections is the set of live database pools BuildRegistryWithConnections
// opened for a document, ONE per name in its connections: block, shared by every
// provider entry that references that name. Duplicate pools to one server is the
// failure this indirection exists to prevent: three kind: sql entries over the
// same database are three providers and one pool.
//
// It is the registry's other half. A *provider.Registry holds no resources and
// needs no shutdown; the pools beneath a SQL-backed entry do, and Close is where
// their lifetime ends. A host closes it when it stops serving:
//
//	reg, conns, err := doc.BuildRegistryWithConnections(dir)
//	if err != nil { return err }
//	defer conns.Close()
//
// A Connections is always non-nil when the build succeeds, and is empty (Close
// is then a no-op) for a document declaring no connections at all, so a caller
// can defer unconditionally.
type Connections struct {
	mu     sync.Mutex
	names  []string // declaration-sorted, so Names and Close are deterministic
	pools  map[string]openConn
	closed bool
}

// openConn is one live connection: the pool, plus the statement budget every
// provider entry reading through it inherits.
type openConn struct {
	pool         Pool
	queryTimeout time.Duration
}

// newConnections returns an empty set.
func newConnections() *Connections {
	return &Connections{pools: map[string]openConn{}}
}

// add records a freshly opened pool under name.
func (c *Connections) add(name string, p Pool, queryTimeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.pools[name]; dup {
		return
	}
	c.pools[name] = openConn{pool: p, queryTimeout: queryTimeout}
	c.names = append(c.names, name)
}

// get returns the pool opened for name.
func (c *Connections) get(name string) (Pool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.pools[name]
	if !ok {
		return nil, false
	}
	return e.pool, true
}

// Pool reports the live pool this set opened for name, or false when it holds
// none. It is the exported half of get, and it exists for exactly one kind of
// caller: one that has to build a SECOND registry over the pools a FIRST one
// already opened.
//
// A process that re-reads its wiring while it is serving is that caller. It
// cannot dial a fresh pool per refresh — that would double every deployment's
// connections on every push, and half of them would be held by a registry nobody
// has a handle to Close — so it supplies WithConnectionOpener returning what this
// reports, and the rebuilt registry SHARES the pools instead of duplicating them.
//
// The pool stays OWNED by this set. Close is what ends its lifetime, and a
// borrower must not close it: wrap it in a type whose Close is a no-op, or the
// second registry's shutdown takes the first one's database access with it.
//
// Names only, never a DSN — the same rule Names obeys.
func (c *Connections) Pool(name string) (Pool, bool) {
	return c.get(name)
}

// queryTimeout returns the statement budget resolved for name, or zero when the
// name is unknown — which sqlprovider reads as its own default, and which
// buildSQLProvider has already rejected before it can be reached.
func (c *Connections) queryTimeout(name string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pools[name].queryTimeout
}

// Len reports how many pools are open. It is how a test asserts that N provider
// entries over one connection produced ONE pool.
func (c *Connections) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pools)
}

// Names lists the connection names with an open pool, sorted. Names only — a
// DSN is never exposed.
func (c *Connections) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]string(nil), c.names...)
	sort.Strings(out)
	return out
}

// Close closes every pool and empties the set. It is idempotent: closing twice
// is a no-op, so `defer conns.Close()` alongside an explicit shutdown path is
// safe. Every pool is closed even if an earlier one fails; the failures are
// joined and returned together, because a pool left open because a sibling
// errored is exactly the leak this type exists to prevent.
//
// The providers built over these pools keep working right up to the Close and
// fail with APERTURE_SQL_PROVIDER_QUERY afterwards — a closed pool is a
// shutdown, not a decision.
func (c *Connections) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var errs []error
	for _, name := range c.names {
		if err := c.pools[name].pool.Close(); err != nil {
			errs = append(errs, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: closing connection %q failed: %v", name, err),
				map[string]any{"connection": name}))
		}
	}
	c.pools = map[string]openConn{}
	c.names = nil
	return errors.Join(errs...)
}

// resolveConnection turns a declared Connection into ConnectionSettings: it
// reads the DSN out of the environment and applies every documented default.
//
// The environment read is the point of the whole design, so it is strict: an
// unset variable and one set to whitespace are the same mistake, and both fail
// here rather than at the first Check that happened to need the database.
func resolveConnection(name string, c Connection) (ConnectionSettings, error) {
	env := strings.TrimSpace(c.DSNEnv)
	if env == "" {
		return ConnectionSettings{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			fmt.Sprintf("seed: connection %q is missing dsn_env", name),
			map[string]any{"connection": name})
	}
	dsn := strings.TrimSpace(os.Getenv(env))
	if dsn == "" {
		return ConnectionSettings{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			fmt.Sprintf("seed: connection %q reads its DSN from environment variable %s, which is unset or empty", name, env),
			map[string]any{"connection": name, "dsn_env": env})
	}

	settings := ConnectionSettings{
		DSN:             dsn,
		MaxOpenConns:    c.MaxOpenConns,
		MaxIdleConns:    c.MaxIdleConns,
		ConnMaxLifetime: DefaultConnMaxLifetime,
		QueryTimeout:    DefaultQueryTimeout,
	}
	if c.MaxOpenConns == 0 {
		settings.MaxOpenConns = DefaultMaxOpenConns
	}
	if c.MaxIdleConns == 0 {
		settings.MaxIdleConns = DefaultMaxIdleConns
	}
	if s := strings.TrimSpace(c.ConnMaxLifetime); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return ConnectionSettings{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: connection %q has an invalid conn_max_lifetime", name),
				map[string]any{"connection": name, "conn_max_lifetime": c.ConnMaxLifetime})
		}
		if d < 0 {
			return ConnectionSettings{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: connection %q has a negative conn_max_lifetime; use \"0\" for connections that are never retired", name),
				map[string]any{"connection": name, "conn_max_lifetime": c.ConnMaxLifetime})
		}
		settings.ConnMaxLifetime = d
	}
	if s := strings.TrimSpace(c.QueryTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return ConnectionSettings{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: connection %q has an invalid query_timeout", name),
				map[string]any{"connection": name, "query_timeout": c.QueryTimeout})
		}
		if d <= 0 {
			return ConnectionSettings{}, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: connection %q has a non-positive query_timeout; there is no \"no timeout\" setting, because an unbounded statement under Check is an unbounded decision", name),
				map[string]any{"connection": name, "query_timeout": c.QueryTimeout, "default": DefaultQueryTimeout.String()})
		}
		settings.QueryTimeout = d
	}
	return settings, nil
}

// openConnections resolves and opens EVERY connection the document declares,
// ONE pool per name, and returns them as a set the caller owns.
//
// Every declared connection is opened, not just the referenced ones: a
// connections: entry is a statement that this deployment has that database, and
// a name that is declared but unused is far more likely to be a typo'd
// connection: on a provider entry than a deliberate spare. Failing on it says so
// at boot. Nothing has been dialled either way — sql.Open is lazy.
//
// If any connection fails, every pool opened so far is closed before the error
// is returned: a half-built registry must not strand a pool its caller never
// received a handle to.
func (d *Document) openConnections(open ConnectionOpener) (*Connections, error) {
	conns := newConnections()
	if len(d.Connections) == 0 {
		return conns, nil
	}
	if open == nil {
		open = openPool
	}
	names := make([]string, 0, len(d.Connections))
	for name := range d.Connections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			_ = conns.Close()
			return nil, aerr.New(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				"seed: a connections: entry has an empty name; the key is what a provider entry's connection: refers to")
		}
		settings, err := resolveConnection(name, d.Connections[name])
		if err != nil {
			_ = conns.Close()
			return nil, err
		}
		pool, err := open(name, settings)
		if err != nil {
			_ = conns.Close()
			return nil, err
		}
		if pool == nil {
			_ = conns.Close()
			return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
				fmt.Sprintf("seed: the connection opener returned no pool for %q", name),
				map[string]any{"connection": name})
		}
		conns.add(name, pool, settings.QueryTimeout)
	}
	return conns, nil
}

// rejectLiteralDSN fails a document that spells credentials inline. It runs in
// Parse — before Apply, before BuildRegistry, before the document is usable for
// anything — because the harm of a literal DSN is that it was written down at
// all, and a load that "worked" is what makes a reviewer stop looking.
//
// It scans BOTH places a dsn: key can be written: a connections: entry, where it
// is the forbidden spelling of dsn_env:, and an attribute_providers: entry, where
// it is doubly wrong — credentials never belong to a provider entry at all, since
// the pool is the document's and is named with connection:. Both are refused by
// name rather than ignored, because a key that is quietly dropped looks like it
// worked and then fails to connect, which is the worst of both outcomes.
//
// The offending entry is named. The value never is.
func (d *Document) rejectLiteralDSN() error {
	var offenders []string
	for name, c := range d.Connections {
		if strings.TrimSpace(c.DSNLiteral) != "" {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		return aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL,
			fmt.Sprintf("seed: connection %s carries a literal dsn: key; use dsn_env: naming the environment variable that holds the DSN, because a seed file is committed and a DSN carries a password",
				strings.Join(quoteAll(offenders), ", ")),
			map[string]any{"connections": offenders, "forbidden_key": "dsn", "use_instead": "dsn_env"})
	}
	var subjects []string
	for _, ap := range d.AttributeProviders {
		if strings.TrimSpace(ap.DSNLiteral) != "" {
			subjects = append(subjects, strings.TrimSpace(ap.Subject))
		}
	}
	if len(subjects) == 0 {
		return nil
	}
	sort.Strings(subjects)
	return aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_DSN_LITERAL,
		fmt.Sprintf("seed: attribute provider for subject %s carries a literal dsn: key; a provider entry never carries credentials — declare the database once in connections: with dsn_env: and name it with connection:, because a seed file is committed and a DSN carries a password",
			strings.Join(quoteAll(subjects), ", ")),
		map[string]any{"subjects": subjects, "forbidden_key": "dsn", "use_instead": "connection"})
}

// quoteAll quotes each name so a multi-connection message stays readable.
func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

// redactDSN removes a DSN, and the password inside it, from a driver's error
// message. Forbidding a literal dsn: keeps the password out of committed text;
// letting the driver echo it into an error — which is logged text — would give
// it straight back. net/url's own errors quote the whole URL, so this is not
// hypothetical.
//
// It replaces the DSN verbatim, its URL-escaped form, and the password on its
// own (a message may quote only the userinfo). An empty password is not
// replaced: substituting for "" would rewrite the entire string.
func redactDSN(msg, dsn string) string {
	const mask = "<redacted>"
	if dsn == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, dsn, mask)
	if esc := url.QueryEscape(dsn); esc != dsn {
		msg = strings.ReplaceAll(msg, esc, mask)
	}
	for _, secret := range dsnSecrets(dsn) {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, mask)
	}
	return msg
}

// dsnSecrets extracts the password from either Postgres DSN form: the URL form
// ("postgres://user:pw@host/db") and the keyword form ("host=h password=pw").
func dsnSecrets(dsn string) []string {
	var out []string
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			out = append(out, pw)
			if unesc, err := url.QueryUnescape(pw); err == nil && unesc != pw {
				out = append(out, unesc)
			}
		}
	}
	for _, field := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(field, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "password") {
			out = append(out, strings.Trim(v, "'\""))
		}
	}
	return out
}

// buildSQLProvider turns one kind: sql entry into a *sqlprovider.Provider over
// the pool its connection: names.
//
// The YAML path and the Go path (sqlprovider.New(querier, cfg)) meet exactly
// here, and neither is a special case of the other: this function resolves a
// name to a Querier and then calls the SAME constructor a host calls, so every
// rule the constructor enforces — required statements, the id column, the
// timeout — is enforced identically whichever way the provider was declared.
func buildSQLProvider(p Provider, conns *Connections) (*sqlprovider.Provider, error) {
	name := strings.TrimSpace(p.Connection)
	if name == "" {
		return nil, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"seed: sql provider is missing connection; name an entry of the document's connections: block",
			map[string]any{"object_type": p.ObjectType})
	}
	pool, ok := conns.get(name)
	if !ok {
		return nil, aerr.WithContext(aerr.APERTURE_SQL_PROVIDER_CONNECTION,
			fmt.Sprintf("seed: sql provider for object type %q references connection %q, which the connections: block does not declare", p.ObjectType, name),
			map[string]any{
				"object_type": p.ObjectType,
				"connection":  name,
				"declared":    conns.Names(),
			})
	}
	return sqlprovider.New(pool, sqlprovider.Config{
		ObjectType: p.ObjectType,
		FetchQuery: p.GetOne,
		ListQuery:  p.GetAll,
		IDColumn:   p.IDColumn,
		// The statement budget belongs to the CONNECTION, not to the entry: it is
		// a property of the database being read, and three object types over one
		// pool that disagreed about how long the server may take would be three
		// different opinions about the same server.
		Timeout: conns.queryTimeout(name),
	})
}
