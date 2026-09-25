package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

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
		},
	}
}

// wiringFlags are the two flags a push needs. They are declared here rather than
// taken from storeFlags(), because --seed means something different: no model
// state is applied from it, and there is no embedded-example fallback.
func wiringFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.StringFlag{Name: "seed", Usage: "path to the JSON/YAML seed document whose four SHARED wiring sections are pushed (required; no model state is applied from it and there is no embedded-example fallback)"},
		&ucli.StringFlag{Name: "store", Usage: "DSN for the shared store the wiring is written to: a postgres:// or postgresql:// URL for PostgreSQL, any other value as a SQLite path (required — there is nothing to share about an in-memory store). Set APERTURE_POSTGRES_SCHEMA to place Aperture's tables in a named PostgreSQL schema"},
	}
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
	return printWiringPushed(cmd, set)
}

// printWiringPushed reports what was written, one row per section.
//
// It counts rather than lists, because the counts are what an operator checks a
// push against ("four providers, yes") and a listing of what is deployed is
// `aperture wiring show`'s job. The row order is the order the sections are
// resolved in when the wiring is built: the manifest, then the entries that cite
// it.
func printWiringPushed(cmd *ucli.Command, set model.WiringSet) error {
	references := 0
	for _, p := range set.Providers {
		references += len(p.References)
	}
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
