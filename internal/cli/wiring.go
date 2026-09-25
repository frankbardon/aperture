package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/frankbardon/aperture/audit"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"

	ucli "github.com/urfave/cli/v3"
)

// `aperture wiring` — the operator's window onto SHARED wiring: the four seed
// sections that live in the database every instance already shares, rather than in
// a file only one instance has.
//
// Wiring is not model state. Model state is who exists and who may do what;
// wiring is where a decision's object metadata and attribute bags are read FROM.
// Four of the seed document's six wiring sections are shared:
//
//	providers:            which object types are served, and by what statements
//	field_types:          which metadata fields hold a date or a datetime
//	connections:          the manifest of connection NAMES an entry may cite
//	attribute_providers:  where each attribute slot's bags come from
//
// Two are NOT, and never will be. objects: and attributes: carry DATA rather than
// a pointer to data — inline object metadata, inline subject bags — and belong to
// the instance whose seed file lists them.
//
// # CLI only, on purpose
//
// There is no RPC method, no MCP tool and no admin-UI panel for any of this, and
// that is a scope decision with a reason rather than an unfinished edge. A wiring
// push changes what EVERY decision in the deployment sees, in every instance, at
// once: it is the shape of change that should require a human at a shell holding
// both the store credential and the document. MCP in particular is
// read + decide + simulate only, and always will be.
//
// # What is deliberately absent from the stored wiring
//
// No DSN, no credential, not even a dsn_env: variable NAME, and no filesystem
// path. Those are per-instance facts: each instance resolves its own credentials,
// sizes its own pool, and may reach the same logical database through a different
// host, while a stored path is a guess about another machine's disk. The schema
// has no column for any of them — which is also why kind: csv is refused here and
// stays perfectly legal in a local seed.

// wiringCommand is `aperture wiring`, the parent. Subcommands land beside push as
// their stories do; the parent carries the shared-versus-local explanation once.
func wiringCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "wiring",
		Usage: "Manage the shared wiring a deployment keeps in its database",
		Description: "SHARED WIRING is the part of a seed document that belongs to the DEPLOYMENT\n" +
			"rather than to one instance: which object types are served and by what\n" +
			"statements (`providers:`), which metadata fields hold dates (`field_types:`),\n" +
			"the manifest of connection NAMES an entry may cite (`connections:`), and where\n" +
			"each attribute slot's bags come from (`attribute_providers:`).\n\n" +
			"Push it once and every instance sharing that database reads the same wiring —\n" +
			"including an instance that has no seed file at all.\n\n" +
			"WIRING IS NOT MODEL STATE. The model says who exists and who may do what;\n" +
			"wiring says where a decision reads object metadata and attribute bags FROM.\n" +
			"The two are pushed by different commands, and wiring for a model that is not\n" +
			"there is refused rather than stored.\n\n" +
			"WHAT IS NEVER STORED: no DSN, no credential, not even the NAME of the\n" +
			"environment variable holding one, and no filesystem path. Those are\n" +
			"per-instance facts — each instance resolves its own credentials and sizes its\n" +
			"own pool — so the manifest carries connection names and nothing else, and\n" +
			"`kind: csv` is refused because its only data source is a path. A csv entry\n" +
			"stays legal in the LOCAL seed file, where the path belongs to the instance\n" +
			"that reads it.\n\n" +
			"The two remaining sections, `objects:` and `attributes:`, are never shared:\n" +
			"they carry inline DATA rather than a pointer to data.",
		Commands: []*ucli.Command{
			wiringPushCommand(),
			wiringShowCommand(),
		},
	}
}

// wiringFlags are the two flags a push needs. They are declared here rather than
// taken from storeFlags(), because --seed means something different: no model
// state is applied from it, and there is no embedded-example fallback.
func wiringFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.StringFlag{Name: "seed", Usage: "path to the JSON/YAML seed document whose four SHARED wiring sections are pushed (required; no model state is applied from it and there is no embedded-example fallback)"},
		wiringStoreFlag(),
	}
}

// wiringStoreFlag is the --store flag, declared once because every subcommand
// needs it and NONE of them has a default: an in-memory store is private to the
// process that opened it, so there is nothing shared about its wiring either to
// write or to read back.
//
// `show` takes this flag and NOT --seed, deliberately. A listing of what is
// deployed must come from the store alone — if it merged in the document on the
// command line it would answer a different question ("what would a push deploy?")
// while looking like it answered this one.
func wiringStoreFlag() *ucli.StringFlag {
	return &ucli.StringFlag{Name: "store", Usage: "DSN for the shared store the wiring lives in: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (required — there is nothing to share about an in-memory store). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema"}
}

// wiringPushCommand is `aperture wiring push`.
func wiringPushCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "push",
		Usage: "Validate a seed document's four shared wiring sections and write them to the store in one transaction",
		Description: "Reads `providers:`, `field_types:`, `connections:` and `attribute_providers:` out\n" +
			"of --seed, validates every entry, and REPLACES the store's wiring with them in a\n" +
			"single transaction. The document's other sections are not read: no model state\n" +
			"is applied, and the two local wiring sections (`objects:`, `attributes:`) are\n" +
			"untouched.\n\n" +
			"THE PUSH IS ALL OR NOTHING. Every rule below is checked before anything is\n" +
			"written, and the write itself replaces the whole set in one transaction, so a\n" +
			"refusal leaves the deployed wiring exactly as it was. Wiring is only meaningful\n" +
			"whole — an entry naming a connection the manifest does not list is not half-valid\n" +
			"wiring, it is broken wiring — and an instance booting against a half-written set\n" +
			"would build a registry missing exactly the entries whose write failed, while\n" +
			"reporting nothing.\n\n" +
			"REPLACE, not merge. What is in the document is what the deployment will run;\n" +
			"an entry dropped from the document is dropped from the store. Push the whole\n" +
			"wiring every time.\n\n" +
			"A push is refused when:\n\n" +
			"  * the store holds NO MODEL STATE at all — apply the model first, and check the\n" +
			"    --store DSN, because a typo names an empty database Setup will create\n" +
			"  * an entry selects `kind: csv` — its only data source is a filesystem path,\n" +
			"    and a path is machine-local\n" +
			"  * a provider serves an `object_type` the store has no row for (named in the\n" +
			"    refusal)\n" +
			"  * an entry names a `connection:` the pushed `connections:` manifest does not\n" +
			"    declare (named in the refusal)\n" +
			"  * anything carries a literal `dsn:` — only `dsn_env:`, a variable NAME, is ever\n" +
			"    accepted, and shared wiring stores neither\n\n" +
			"No actor is required: the store credential is the authority, exactly as it is for\n" +
			"`aperture import`.",
		Flags:  wiringFlags(),
		Action: runWiringPush,
	}
}

// runWiringPush is the whole push: parse, project, validate, write.
//
// The ORDER of the steps is the contract, and each one is ordered ahead of the
// next because its refusal is the more useful sentence:
//
//  1. the flags, before anything is opened — a missing --seed needs no database
//  2. the document, parsed by seed's own reader (which is where a literal dsn: is
//     already refused, before the file is usable for anything at all)
//  3. the projection and every document-only rule, so a malformed document is
//     reported without a connection being made
//  4. the store, opened and Setup — but NOT seeded: a push must not create the
//     very model state whose absence it refuses
//  5. the model-state rules, which need the store
//  6. the write, all-or-nothing
func runWiringPush(ctx context.Context, cmd *ucli.Command) error {
	seedPath := cmd.String("seed")
	if seedPath == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"wiring push requires --seed naming the document whose shared wiring sections are pushed; there is no embedded-example fallback here, because pushing the demo's wiring into a real deployment is not a default anyone wants")
	}
	storeDSN := cmd.String("store")
	if storeDSN == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"wiring push requires --store naming the SHARED store the wiring is written to; an in-memory store is private to this process and would not survive it, so there would be nothing shared about the wiring")
	}

	// seed.ParseFile is the document reader, reused rather than reimplemented. It
	// is also where a literal dsn: is refused — at decode, before the document is
	// usable — and that refusal is already coded, so it is returned as it is.
	doc, err := seed.ParseFile(seedPath)
	if err != nil {
		if aerr.CodeOf(err) != "" {
			return err
		}
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: parsing the wiring document", err)
	}

	// One instant for the whole push. See wiringFromDocument.
	set, err := wiringFromDocument(doc, time.Now().UTC())
	if err != nil {
		return err
	}

	// The empty --seed path is what stops buildStore applying the document's model
	// state. A push that seeded the model would create the object types whose
	// absence it is supposed to refuse, and the "no model state" refusal could then
	// never fire at all.
	store, err := buildStore(ctx, storeDSN, "")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if err := checkWiringAgainstModel(ctx, store, set); err != nil {
		return err
	}
	// ReplaceWiring is the only write: it validates the whole set, empties the five
	// tables and writes the new one in one transaction. Its refusals are coded, so
	// they pass through — burying APERTURE_STORAGE_CONSTRAINT or
	// APERTURE_STORAGE_SCHEMA_INCOMPATIBLE under a generic push code would cost the
	// operator the one fixup that actually applies.
	if err := store.ReplaceWiring(ctx, set); err != nil {
		if aerr.CodeOf(err) != "" {
			return err
		}
		return aerr.Wrap(aerr.APERTURE_STORAGE, "cli: writing the shared wiring", err)
	}
	// AFTER the write, and only after it. Every refusal above has returned
	// already, so there is no path on which a push that changed nothing leaves a
	// record saying it did.
	recordWiringPush(ctx, cmd, store, set)
	return printWiringPushed(cmd, set)
}

// wiringPushActor is what the audit trail records as the actor of a push, and it
// is deliberately not a principal id.
//
// `wiring push` takes no --principal, for the same reason `aperture import` does
// not: the authority for the change IS the store credential. Whoever can open the
// store with write access can replace its wiring, and Aperture has nothing else
// to check. Inventing an identity here — the OS user, a hostname, an empty string
// read as "unknown" — would put a name in the trail that no gate ever verified,
// and a name nobody checked is worse than an honest sentinel, because the next
// reader believes it.
//
// So the trail records what is actually true: the actor is the store credential,
// spelled as a KIND rather than as an id, and the event's Reason says so in words
// for a reader who meets the string cold. WHO held that credential is a question
// for the database's own connection log, which is the only place the answer
// exists.
const wiringPushActor = "store-credential"

// recordWiringPush writes the one audit record a successful push leaves behind.
//
// # Why this is the same trail as a grant change
//
// EventType is model.AuditMutation — the category every grant write, template
// apply and entity CRUD already uses — because a wiring push IS a mutation of the
// deployment, and a wider one than most grants: it changes where every decision
// in every instance reads its object metadata and attribute bags FROM. An
// investigator asking "what changed before this went wrong?" must not have to
// know that wiring lives in a category of its own to find it. Action is
// "WiringPush", in the PascalCase the other always-on actions use ("PutGrant",
// "ApplyTemplate", "Bestow").
//
// # Why the account is the wildcard
//
// Wiring is not account-scoped — there is no account column anywhere in the five
// tables — and a push affects every account at once. model.AccountWildcard is
// this repository's existing spelling for exactly that, and it is a reserved
// sentinel rather than an account id, so the record names no real account. The
// wiring set itself carries no account, no principal and no object identity, so
// neither do the details: there is nothing here for the no-cross-account-data
// rule to be violated by.
//
// # Why a short-lived process cannot drop it
//
// Recorder.Record is SYNCHRONOUS: it writes through the sink on this goroutine
// and returns the storage error. The async buffering in audit/ belongs to
// RecordDecision, which samples the decision hot path — a push is not the hot
// path and must never be sampled, so it takes the always-on path and the record
// is durable before this function returns. That is what makes a one-shot CLI
// invocation safe. Close below stops an idle background writer rather than
// flushing anything; it is called for hygiene, not for correctness.
//
// A failed audit write does NOT fail the push: the wiring is already replaced,
// and returning an error would tell the operator their change did not land when
// it did. It is reported on stderr instead, because silently losing the record of
// a deployment-wide change is not something an operator should have to infer.
func recordWiringPush(ctx context.Context, cmd *ucli.Command, sink audit.Sink, set model.WiringSet) {
	rec := audit.New(sink)
	defer func() { _ = rec.Close() }()

	objectTypes := make([]string, 0, len(set.Providers))
	for _, p := range set.Providers {
		objectTypes = append(objectTypes, p.ObjectType)
	}
	slots := make([]string, 0, len(set.AttributeProviders))
	for _, a := range set.AttributeProviders {
		slots = append(slots, a.Subject)
	}
	connections := make([]string, 0, len(set.Connections))
	for _, c := range set.Connections {
		connections = append(connections, c.Name)
	}

	err := rec.Record(ctx, model.AuditEvent{
		EventType: model.AuditMutation,
		Action:    "WiringPush",
		Actor:     wiringPushActor,
		Account:   model.AccountWildcard,
		Target:    "wiring:shared",
		Outcome:   model.OutcomeSuccess,
		Reason: "the store's shared wiring was replaced with the pushed set; " +
			"`wiring push` takes no principal because the store credential is the authority for the change, " +
			"so the actor is recorded as that credential rather than as an identity nothing verified",
		// The counts an operator checks a push against, plus the NAMES of what was
		// deployed, so the trail says what changed and not only how much. Every one
		// of these is deployment-wide wiring vocabulary — a connection name, an
		// object type, an attribute slot — and none of it is account data.
		Details: map[string]any{
			"connection_count":         len(set.Connections),
			"provider_count":           len(set.Providers),
			"provider_reference_count": countWiringReferences(set),
			"field_type_count":         len(set.FieldTypes),
			"attribute_provider_count": len(set.AttributeProviders),
			"connections":              connections,
			"object_types":             objectTypes,
			"attribute_slots":          slots,
		},
	})
	if err != nil {
		// Reported, never returned. See the doc comment.
		fmt.Fprintf(cmd.ErrWriter,
			"warning: the shared wiring was replaced, but its audit record could not be written: %v\n", err)
	}
}

// countWiringReferences totals the reference rows across every provider entry. It
// is one function because the push summary and the audit record must report the
// same number, and two loops would be two answers.
func countWiringReferences(set model.WiringSet) int {
	n := 0
	for _, p := range set.Providers {
		n += len(p.References)
	}
	return n
}

// printWiringPushed reports what was written, one row per section.
//
// It counts rather than lists, because the counts are what an operator checks a
// push against ("four providers, yes") and a listing of what is deployed is
// `aperture wiring show`'s job. The row order is the order the sections are
// resolved in when the wiring is built: the manifest, then the entries that cite
// it.
func printWiringPushed(cmd *ucli.Command, set model.WiringSet) error {
	references := countWiringReferences(set)
	w := tabwriter.NewWriter(cmd.Writer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "section\trows")
	fmt.Fprintf(w, "connections\t%d\n", len(set.Connections))
	fmt.Fprintf(w, "providers\t%d\n", len(set.Providers))
	fmt.Fprintf(w, "provider references\t%d\n", references)
	fmt.Fprintf(w, "field types\t%d\n", len(set.FieldTypes))
	fmt.Fprintf(w, "attribute providers\t%d\n", len(set.AttributeProviders))
	if err := w.Flush(); err != nil {
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: writing the push summary", err)
	}
	// Said explicitly, because REPLACE is the surprising half: an operator who
	// pushes a trimmed document has just retired the entries they left out.
	fmt.Fprintln(cmd.Writer, "the store's shared wiring is now exactly this set")
	return nil
}

// wiringShowCommand is `aperture wiring show`: the human-readable listing of what
// is deployed.
func wiringShowCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "show",
		Usage: "Print the shared wiring this store has deployed, one table per section",
		Description: "Reads the five shared-wiring tables and prints them: the connection manifest, the\n" +
			"provider entries with their reference declarations, the field-type declarations,\n" +
			"the attribute-provider entries, and the statement set every database-backed entry\n" +
			"runs. It answers \"what is this deployment actually wired to?\" without a database\n" +
			"client.\n\n" +
			"AN EMPTY STORE IS AN ANSWER, not an error. A store nothing has been pushed to says\n" +
			"so plainly: every instance booting against it builds its wiring from its own\n" +
			"--seed file instead. It is also what a mistyped --store looks like, because a DSN\n" +
			"naming a database that does not exist yet is one Setup creates — so check the DSN\n" +
			"before concluding a push was lost.\n\n" +
			"THE COLUMNS AN OPERATOR WOULD OTHERWISE HAVE TO GUESS AT are spelled as words\n" +
			"rather than left blank. `ttl` and `max-size` read `(default)` when the entry sets\n" +
			"neither, because the registry's own default applies and `0` would read as\n" +
			"\"caches nothing\". `connection` and `id-column` read `-` when the kind does not use\n" +
			"them. An attribute slot with no `get_all` is reported as FETCH-ONLY: every\n" +
			"decision path works unchanged and only the system-tier directory read refuses.\n\n" +
			"THE DECLARED KEY SET distinguishes three states, because two of them are\n" +
			"different answers a blank column would merge: `(not declared)` is a slot that\n" +
			"opted out of key enforcement entirely, `(declared empty)` is a slot that opted IN\n" +
			"and permits no keys at all, and a list is the keys the slot guarantees.\n\n" +
			"No actor is required, and none is accepted. This restates the wiring that the\n" +
			"--store credential you just supplied already grants full write access to, so\n" +
			"requiring an authority on top of it would only mean nobody could diagnose \"is\n" +
			"anything even deployed?\" without already holding the authority the diagnosis\n" +
			"explains — the same reason `aperture attributes slots` is ungated. It contacts no\n" +
			"provider, opens no host connection, and prints no account, principal or object\n" +
			"identity, because the shared wiring holds none.",
		Flags:  []ucli.Flag{wiringStoreFlag()},
		Action: runWiringShow,
	}
}

// runWiringShow reads the deployed wiring and prints it.
//
// It reads through the four per-section List reads rather than GetWiring. Both
// return the same rows in the same canonical order; the difference is what each is
// FOR. GetWiring is the BOOT read, which must hand a builder one consistent
// snapshot of a set it is about to construct registries from. A listing constructs
// nothing, and reading section by section keeps the printer's structure and the
// storage interface's structure the same shape — a sixth section then arrives here
// as one more read and one more table, rather than as a new field to find a place
// for.
func runWiringShow(ctx context.Context, cmd *ucli.Command) error {
	storeDSN := cmd.String("store")
	if storeDSN == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"wiring show requires --store naming the SHARED store whose deployed wiring is printed; an in-memory store is private to this process and has nothing deployed to it")
	}

	// The empty seed path is what keeps this a read. buildStore would otherwise
	// apply a document's model state, and a command whose whole job is to describe
	// a deployment must not change it.
	store, err := buildStore(ctx, storeDSN, "")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	set, err := readWiringSections(ctx, store)
	if err != nil {
		return err
	}
	return printWiringShow(cmd, set)
}

// readWiringSections reads all four sections into one WiringSet.
func readWiringSections(ctx context.Context, store model.Storage) (model.WiringSet, error) {
	var set model.WiringSet
	var err error
	if set.Connections, err = store.ListWiringConnections(ctx); err != nil {
		return model.WiringSet{}, wiringReadError("the connection manifest", err)
	}
	if set.Providers, err = store.ListWiringProviders(ctx); err != nil {
		return model.WiringSet{}, wiringReadError("the provider entries", err)
	}
	if set.FieldTypes, err = store.ListWiringFieldTypes(ctx); err != nil {
		return model.WiringSet{}, wiringReadError("the field-type declarations", err)
	}
	if set.AttributeProviders, err = store.ListWiringAttributeProviders(ctx); err != nil {
		return model.WiringSet{}, wiringReadError("the attribute-provider entries", err)
	}
	return set, nil
}

// wiringReadError codes a section-read failure, passing an existing code through.
//
// The guard is not cosmetic here. APERTURE_STORAGE_SCHEMA_INCOMPATIBLE is exactly
// the refusal an operator meets on this command — a database written by an older
// build reads as a schema mismatch, whose fixup is to recreate it — and
// re-stamping that as a generic storage fault would replace the one remedy that
// applies with advice true of every storage failure there is.
func wiringReadError(what string, err error) error {
	if aerr.CodeOf(err) != "" {
		return err
	}
	return aerr.Wrap(aerr.APERTURE_STORAGE, "cli: reading "+what, err)
}

// printWiringShow renders the deployed wiring.
//
// The shape follows `aperture attributes slots`: a tabwriter table with a header
// row, and words rather than blanks in the columns an operator would otherwise
// have to guess at. There is one table per section because the sections have
// nothing in common to share columns over, and the statement set gets a table of
// its own rather than two more columns on the provider tables — SQL is the one
// value here with no bounded width, and a column of it would push every entry's
// short, scannable fields off the right of the terminal.
func printWiringShow(cmd *ucli.Command, set model.WiringSet) error {
	if set.IsEmpty() {
		// An answer, not an error. Said in full, because the two things an operator
		// does next are opposites — push, or fix the DSN — and which one is right is
		// not visible from an empty table.
		fmt.Fprintln(cmd.Writer, "no shared wiring is deployed to this store")
		fmt.Fprintln(cmd.Writer, "every instance booting against it builds its wiring from its own --seed file")
		fmt.Fprintln(cmd.Writer, "if you expected a pushed set here, check the --store DSN: one naming a database that does not exist yet is one Setup creates, and a fresh database is indistinguishable from a store nobody has pushed to")
		return nil
	}

	w := tabwriter.NewWriter(cmd.Writer, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "connections")
	fmt.Fprintln(w, "name")
	if len(set.Connections) == 0 {
		fmt.Fprintln(w, "(none declared)")
	}
	for _, c := range set.Connections {
		fmt.Fprintln(w, c.Name)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "providers")
	fmt.Fprintln(w, "object-type\tkind\tconnection\tid-column\tttl\tmax-size\treferences")
	if len(set.Providers) == 0 {
		fmt.Fprintln(w, "(none wired)")
	}
	for _, p := range set.Providers {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.ObjectType, p.Kind, orDash(p.Connection), orDash(p.IDColumn),
			renderWiringTTL(p.TTL), renderWiringMaxSize(p.MaxSize),
			renderWiringReferences(p.References))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "field types")
	fmt.Fprintln(w, "object-type\tfield\tdeclared-type")
	if len(set.FieldTypes) == 0 {
		fmt.Fprintln(w, "(none declared)")
	}
	for _, f := range set.FieldTypes {
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.ObjectType, f.Field, f.DeclaredType)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "attribute providers")
	fmt.Fprintln(w, "slot\tkind\tconnection\tid-column\tttl\tmax-size\tdeclared-keys")
	if len(set.AttributeProviders) == 0 {
		fmt.Fprintln(w, "(none wired)")
	}
	for _, a := range set.AttributeProviders {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Subject, a.Kind, orDash(a.Connection), orDash(a.IDColumn),
			renderWiringTTL(a.TTL), renderWiringMaxSize(a.MaxSize),
			renderDeclaredKeys(a.DeclaredKeys))
	}
	fmt.Fprintln(w)

	// The statements last, and in a table of their own. `sql` is the final column
	// on purpose: tabwriter does not pad a trailing cell, so an arbitrarily long
	// statement costs the columns beside it nothing.
	fmt.Fprintln(w, "statements")
	fmt.Fprintln(w, "section\tentry\tstatement\tsql")
	for _, p := range set.Providers {
		fmt.Fprintf(w, "providers\t%s\tget_one\t%s\n", p.ObjectType, orNone(p.GetOne))
		fmt.Fprintf(w, "providers\t%s\tget_all\t%s\n", p.ObjectType, orNone(p.GetAll))
	}
	for _, a := range set.AttributeProviders {
		fmt.Fprintf(w, "attribute providers\t%s\tget_one\t%s\n", a.Subject, orNone(a.GetOne))
		fmt.Fprintf(w, "attribute providers\t%s\tget_all\t%s\n", a.Subject, renderSlotGetAll(a.GetAll))
	}

	if err := w.Flush(); err != nil {
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: writing the wiring listing", err)
	}
	return nil
}

// orDash spells a column the entry's kind does not use. An empty cell would read
// as a missing value rather than as an inapplicable one.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// orNone spells an absent statement.
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// renderSlotGetAll spells an attribute slot's enumeration statement.
//
// Its absence is legitimate HERE where a provider entry must declare one, so it is
// labelled rather than left looking like an omission: a slot with no get_all is
// FETCH-ONLY. Every decision path works unchanged — it serves the attributes of
// the subject being decided about — and only the system-tier directory read
// refuses, which is the feature that lets a host answer decisions without exposing
// its whole user table to an enumeration.
func renderSlotGetAll(s string) string {
	if s == "" {
		return "(none — this slot is FETCH-ONLY: it serves decisions and refuses the system-tier directory read)"
	}
	return s
}

// renderWiringTTL spells a wiring entry's freshness window.
//
// It is deliberately NOT renderTTL (attributes.go), and the difference is the
// point. That one renders a time.Duration the registry is already running with,
// where zero really does mean "never expires on its own". A wiring TTL is the
// duration TEXT the operator wrote, and the empty string means they wrote nothing
// — so the registry's own default applies, which is a different claim from
// "never". Rendering the two the same way would tell an operator their cached bags
// never go stale when they do, and a cache window is a revocation window.
func renderWiringTTL(ttl string) string {
	if ttl == "" {
		return "(default)"
	}
	return ttl
}

// renderWiringMaxSize spells a wiring entry's cache bound. Zero means the
// registry default, and "0" would read as "caches nothing" — the opposite.
func renderWiringMaxSize(n int) string {
	if n == 0 {
		return "(default)"
	}
	return strconv.Itoa(n)
}

// renderWiringReferences spells an entry's reference declarations as
// `field -> type` pairs, in the canonical field order a read returns them in.
func renderWiringReferences(refs []model.WiringReference) string {
	if len(refs) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, r.Field+" -> "+r.TargetType)
	}
	return strings.Join(parts, ", ")
}

// renderDeclaredKeys spells the three states of a declared key set, all three as
// words.
//
// NOT DECLARED and DECLARED EMPTY are DIFFERENT ANSWERS, and the whole reason
// model.DeclaredKeys is a struct rather than a []string is that the distinction
// survives being cloned, marshalled and persisted. A listing that printed a blank
// for both would lose it at the last step, in the one place an operator actually
// reads it: `(not declared)` is a slot that opted out of key enforcement entirely,
// and `(declared empty)` is a slot that opted IN and permits no keys at all.
func renderDeclaredKeys(d model.DeclaredKeys) string {
	if !d.Declared {
		return "(not declared)"
	}
	if len(d.Keys) == 0 {
		return "(declared empty)"
	}
	return strings.Join(d.Keys, ", ")
}
