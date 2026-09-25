package cli

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/seed"

	ucli "github.com/urfave/cli/v3"
)

// `aperture wiring diff` — the one command that answers "is what is deployed what
// is in the repository?".
//
// Without it an operator's only drift check is pull-then-diff-by-hand, which is
// the manual synchronisation this whole effort exists to remove, and it is a check
// nobody runs in a pipeline because it takes two commands, a scratch file and a
// human reading the output.
//
// # It compares WIRING SETS, never rendered documents
//
// Both sides of the comparison are model.WiringSet values: the deployed side comes
// straight from GetWiring (pullWiringSnapshot, the same atomic read `pull` takes),
// and the local side goes through wiringFromDocument, the same projection a PUSH
// takes. So there is exactly one notion of "what the document means" and exactly
// one notion of "what is deployed", and this command adds neither.
//
// wiringToDocument is the only projection in the store->document direction and
// this file deliberately does not use it or grow a second. Comparing rendered
// documents would compare FORMATTING as well as wiring — key order, quoting, a
// trailing newline — and it would have to reason about the dsn_env: asymmetry a
// pulled document carries, which is a property of the FILE and not of the wiring.
// Two notions of "the same wiring" would be two answers to one question, and the
// one that is wrong is whichever the operator did not run.
//
// # Why drift is an EXIT CODE and not an error
//
// Drift is the ANSWER to the question asked, not a failure to answer it. So it
// carries no APERTURE_* code: a coded error means "Aperture could not do what you
// asked, and here is the remedy", and there is no remedy for a report — the
// deployment and the document really do differ, and which one is wrong is the
// operator's call. It follows `aperture check`'s precedent exactly: a clean DENY
// prints its verdict and returns ucli.Exit with a non-zero code, because the
// verdict is not an error either.
//
// The code is NOT 1, and that is the useful half. Every coded refusal this binary
// makes exits 1 (cmd/aperture/main.go), so a diff that signalled drift with 1
// would be indistinguishable from a diff that could not run at all — a typo'd
// --store, an unreadable --seed, a schema mismatch. A pipeline that fails closed
// on drift would then be failing closed on "I could not tell", with the same
// signal and no way to separate them.

// wiringDriftExitCode is the process exit code for a diff that FOUND DRIFT: the
// report was produced and the two sides differ.
//
// 2 rather than 1, deliberately. 1 is what every coded refusal exits with, so
// reusing it would merge "they differ" into "I could not tell whether they
// differ", and those are the two outcomes a deployment gate has to act on
// differently. It is also diff(1)'s own convention read the only way this binary
// can spell it: differences are distinguished from trouble, with trouble keeping
// the exit code it already has everywhere else.
const wiringDriftExitCode = 2

// wiringDiffCommand is `aperture wiring diff`.
func wiringDiffCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "diff",
		Usage: "Report how the deployed shared wiring differs from a seed document's four wiring sections, exiting non-zero on drift",
		Description: "Compares the wiring DEPLOYED in --store against the four shared wiring sections of\n" +
			"--seed — `connections:`, `providers:`, `field_types:` and `attribute_providers:` —\n" +
			"and names, per section and per entry, what is only deployed, what is only local,\n" +
			"and what differs. The `section` column is the document's own section key, so a\n" +
			"reported entry is one you can go and find.\n\n" +
			"IT IS MEANT FOR A PIPELINE. Identical wiring prints a clean report and exits 0;\n" +
			"drift exits 2. Every coded refusal this binary makes exits 1, so the three\n" +
			"outcomes stay apart: 0 is \"the deployment matches the repository\", 2 is \"it does\n" +
			"not\", and 1 is \"I could not tell\" — an unreadable document, a --store that names\n" +
			"nothing, a schema this build cannot read. A gate that failed closed on 1 and 2\n" +
			"alike would report drift for a typo in its own DSN.\n\n" +
			"DRIFT IS NOT AN ERROR and carries no error code. The two sides really do differ;\n" +
			"which of them is wrong is your call, and `aperture wiring push --seed <file>\n" +
			"--store <dsn>` is how you make the deployment agree with the document.\n\n" +
			"FORMATTING IS NEVER DRIFT. Both sides are compared as WIRING, in the one\n" +
			"canonical order a read returns, so re-ordering the document's entries, re-ordering\n" +
			"a `fields:` or `references:` map, or re-indenting the file changes nothing in this\n" +
			"report. The push timestamps are excluded too: a stamp is a fact about the last\n" +
			"push and not about the wiring.\n\n" +
			"A `kind: csv` ENTRY IS REPORTED AS LOCAL-ONLY BY DESIGN, not as drift and not as\n" +
			"an error. Its only data source is a filesystem path, `wiring push` refuses it, and\n" +
			"so no push can ever make the deployment match it. Counting it as drift would leave\n" +
			"a permanently red gate on every deployment that legitimately keeps a local csv\n" +
			"provider, and a gate that cannot go green is a gate somebody switches off.\n\n" +
			"A STORE WITH NOTHING DEPLOYED IS AN ANSWER, not a refusal, and this is the one\n" +
			"place a diff disagrees with `aperture wiring pull`. A pull refuses an empty store,\n" +
			"because the document it would write is one that, pushed back, replaces the\n" +
			"deployment's wiring with nothing. A diff writes no file and pushes nothing, so it\n" +
			"reports every local entry as only-local and says plainly that nothing is deployed\n" +
			"— which is also what a mistyped --store looks like, so check the DSN before\n" +
			"concluding a push was lost.\n\n" +
			"No actor is required, and none is accepted, for the reason `wiring show` accepts\n" +
			"none: this restates wiring the --store credential already grants full write access\n" +
			"to. It contacts no provider, opens no host connection, and prints no account,\n" +
			"principal or object identity, because the shared wiring holds none.",
		Flags: []ucli.Flag{
			// The seed flag is declared here rather than taken from wiringFlags(),
			// because --seed means something different again: a push DEPLOYS these
			// sections and a diff only READS them. Nothing is written by this command,
			// to the store or to the file.
			&ucli.StringFlag{Name: "seed", Usage: "path to the JSON/YAML seed document whose four SHARED wiring sections the deployment is compared against (required; nothing is applied, pushed or written, and there is no embedded-example fallback)"},
			wiringStoreFlag(),
		},
		Action: runWiringDiff,
	}
}

// runWiringDiff is the whole diff: parse, project, read, compare, report.
//
// The ORDER is the contract, and each step is ahead of the next because its
// refusal is the more useful sentence:
//
//  1. the flags, before anything is opened
//  2. the document, parsed by seed's own reader — which is also where a literal
//     dsn: is refused, before the file is usable for anything at all
//  3. the local projection, so a malformed document is reported without a
//     connection being made. Every rule a PUSH refuses on that is answerable from
//     the document alone is refused here too, because a document that cannot be
//     pushed cannot meaningfully be compared with a deployment
//  4. the store, opened and Setup but NOT seeded: a read must not apply a
//     document's model state
//  5. the snapshot, which is allowed to be empty
//  6. the comparison and the report, and only then the exit code
func runWiringDiff(ctx context.Context, cmd *ucli.Command) error {
	seedPath := cmd.String("seed")
	if seedPath == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"wiring diff requires --seed naming the document the deployment is compared against; there is no embedded-example fallback, because diffing a real deployment against the demo's wiring reports drift that means nothing")
	}
	storeDSN := cmd.String("store")
	if storeDSN == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"wiring diff requires --store naming the SHARED store whose deployed wiring is compared; an in-memory store is private to this process and has nothing deployed to it")
	}

	doc, err := seed.ParseFile(seedPath)
	if err != nil {
		if aerr.CodeOf(err) != "" {
			return err
		}
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: parsing the wiring document", err)
	}

	local, unshareable, err := localWiringForDiff(doc)
	if err != nil {
		return err
	}

	// The empty seed path is what keeps this a read, exactly as it is for `show`
	// and `pull`. checkWiringAgainstModel is deliberately NOT run either: it refuses
	// a store with no model state, which is a rule about what may be DEPLOYED, and a
	// diff against a store nothing has been pushed to is a legitimate question with
	// a useful answer.
	store, err := buildStore(ctx, storeDSN, "")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// pullWiringSnapshot, not readWiringSections: a diff assembled from the four
	// independent List reads could compare against a wiring set that never existed —
	// one section's entries beside another section's superseded manifest — and report
	// drift, or clean, about a state no instance ever booted from.
	deployed, err := pullWiringSnapshot(ctx, store)
	if err != nil {
		return err
	}

	diff := diffWiring(deployed, local)
	diff.unshareable = unshareable
	if err := printWiringDiff(cmd, diff, deployed.IsEmpty()); err != nil {
		return err
	}
	if diff.hasDrift() {
		// Not an error: see the exit-code note at the top of this file. The message is
		// empty because the report above already said everything, and ucli.Exit prints
		// a non-empty one to stderr.
		return ucli.Exit("", wiringDriftExitCode)
	}
	return nil
}

// localWiringForDiff projects the document's four shared wiring sections the way a
// PUSH would, minus the entries a push could never deploy.
//
// The csv entries are partitioned out BEFORE the projection rather than tolerated
// inside it, and that ordering is what keeps this a reuse of wiringFromDocument
// instead of a second, more permissive copy of it. Every other rule a push refuses
// on still applies: an unknown kind, an undeclared connection, an unparseable ttl:,
// a field type that is neither "date" nor "datetime" are all refusals here too,
// because a document that cannot be pushed cannot be compared against a deployment
// — the comparison would be with a wiring set that has no meaning.
//
// refuseLiteralWiringDSN runs over the WHOLE document, before anything is removed
// from it, so dropping the csv entries cannot carry a credential past the one rule
// that exists to keep credentials out of shared wiring. seed.Parse already refuses
// a literal dsn: at decode, so this is the second of two guards rather than the
// only one — the same belt-and-braces wiringFromDocument itself keeps.
func localWiringForDiff(doc *seed.Document) (model.WiringSet, []wiringUnshareableEntry, error) {
	if doc == nil {
		return model.WiringSet{}, nil, aerr.New(aerr.APERTURE_INVALID_INPUT,
			"cli: no seed document to compare the deployed wiring against")
	}
	if err := refuseLiteralWiringDSN(doc); err != nil {
		return model.WiringSet{}, nil, err
	}

	shareable, unshareable := splitUnshareableWiring(doc)
	// The ZERO instant, not time.Now(). A diff writes nothing, so there is no push
	// for a stamp to be a fact about — and stamping the local side with the current
	// time would make the one thing the comparison must ignore the one thing that
	// always differs. canonicalWiring drops the deployed side's stamps for the same
	// reason; passing the zero instant here means the local side has none to drop.
	set, err := wiringFromDocument(shareable, time.Time{})
	if err != nil {
		return model.WiringSet{}, nil, err
	}
	return set, unshareable, nil
}

// wiringUnshareableEntry is one local entry a push can never deploy: its section
// and the entry's key.
type wiringUnshareableEntry struct {
	section string
	entry   string
}

// splitUnshareableWiring separates the document's wiring into the part a push
// would deploy and the `kind: csv` entries it would refuse.
//
// It returns a SHALLOW COPY carrying only the four shared sections. The copy is
// what keeps a diff a read of the file as well as of the store: the caller's
// Document — the one the operator's --seed parsed into — is not modified, so
// nothing downstream can observe a document with its csv entries quietly missing.
//
// Only `kind: csv` is removed, and only that exact spelling. An UNKNOWN kind stays
// in the shareable half so wiringFromDocument refuses it: a typo is a mistake to
// report, not an entry to file under "local by design", and silently reclassifying
// it would turn the operator's misspelling into a line of the report that looks
// intentional. A csv entry that NAMES NOTHING stays in the shareable half for the
// same reason — it is reported as the malformed entry it is, rather than as a
// nameless line of the local-by-design list.
func splitUnshareableWiring(doc *seed.Document) (*seed.Document, []wiringUnshareableEntry) {
	out := &seed.Document{
		Connections: doc.Connections,
		FieldTypes:  doc.FieldTypes,
	}
	var unshareable []wiringUnshareableEntry

	for _, p := range doc.Providers {
		objectType := strings.TrimSpace(p.ObjectType)
		if strings.TrimSpace(p.Kind) == wiringKindCSV && objectType != "" {
			unshareable = append(unshareable, wiringUnshareableEntry{
				section: wiringSectionProviders,
				entry:   objectType,
			})
			continue
		}
		out.Providers = append(out.Providers, p)
	}
	for _, ap := range doc.AttributeProviders {
		subject := strings.TrimSpace(ap.Subject)
		if strings.TrimSpace(ap.Kind) == wiringKindCSV && subject != "" {
			unshareable = append(unshareable, wiringUnshareableEntry{
				section: wiringSectionAttributeProviders,
				entry:   subject,
			})
			continue
		}
		out.AttributeProviders = append(out.AttributeProviders, ap)
	}
	sort.Slice(unshareable, func(i, j int) bool {
		if unshareable[i].section != unshareable[j].section {
			return unshareable[i].section < unshareable[j].section
		}
		return unshareable[i].entry < unshareable[j].entry
	})
	return out, unshareable
}

// The section names are the DOCUMENT's own section keys, not the words `wiring
// show` prints. A diff exists so somebody can go and edit the document, and
// `field_types` is the string they will search the file for.
const (
	wiringSectionConnections        = "connections"
	wiringSectionProviders          = "providers"
	wiringSectionFieldTypes         = "field_types"
	wiringSectionAttributeProviders = "attribute_providers"
)

// wiringDiff is the whole report: one entry per section, plus the two things that
// are reported and are NOT drift.
type wiringDiff struct {
	sections []wiringDiffSection
	// unshareable is the local `kind: csv` entries. They are local-only BY DESIGN —
	// no push can deploy one — so they are reported and excluded from hasDrift.
	unshareable []wiringUnshareableEntry
	// unexpressedDeclaredKeys is the attribute slots whose DECLARED KEY SET the
	// local document has no key for yet. See reconcileUnexpressedDeclaredKeys.
	unexpressedDeclaredKeys []string
}

// wiringDiffSection is one section's three answers.
type wiringDiffSection struct {
	name         string
	onlyDeployed []string
	onlyLocal    []string
	differing    []wiringEntryDrift
}

// wiringEntryDrift is one entry present on both sides that does not match, and the
// fields that do not.
type wiringEntryDrift struct {
	key    string
	fields []string
}

// hasDrift reports whether anything in the comparison is drift.
//
// The csv entries and the unexpressible declared key sets are deliberately NOT
// counted. Neither is a difference a push could resolve — a csv entry is refused at
// push, and a declared key set has no document key to write it in — so counting
// either would leave the exit code permanently non-zero for a deployment that is
// exactly as wired as its repository says. A gate that can never go green is a gate
// somebody switches off, and then the drift that matters goes unreported too.
func (d wiringDiff) hasDrift() bool {
	for _, s := range d.sections {
		if len(s.onlyDeployed) > 0 || len(s.onlyLocal) > 0 || len(s.differing) > 0 {
			return true
		}
	}
	return false
}

// diffWiring compares the deployed wiring against the local one, section by
// section.
func diffWiring(deployed, local model.WiringSet) wiringDiff {
	reconciled, unexpressed := reconcileUnexpressedDeclaredKeys(deployed.AttributeProviders, local.AttributeProviders)
	return wiringDiff{
		sections: []wiringDiffSection{
			diffWiringSection(wiringSectionConnections,
				indexWiring(deployed.Connections, wiringConnectionKey, canonicalWiringConnection),
				indexWiring(local.Connections, wiringConnectionKey, canonicalWiringConnection)),
			diffWiringSection(wiringSectionProviders,
				indexWiring(deployed.Providers, wiringProviderKey, canonicalWiringProvider),
				indexWiring(local.Providers, wiringProviderKey, canonicalWiringProvider)),
			diffWiringSection(wiringSectionFieldTypes,
				indexWiring(deployed.FieldTypes, wiringFieldTypeKey, canonicalWiringFieldType),
				indexWiring(local.FieldTypes, wiringFieldTypeKey, canonicalWiringFieldType)),
			diffWiringSection(wiringSectionAttributeProviders,
				indexWiring(reconciled, wiringAttributeProviderKey, canonicalWiringAttributeProvider),
				indexWiring(local.AttributeProviders, wiringAttributeProviderKey, canonicalWiringAttributeProvider)),
		},
		unexpressedDeclaredKeys: unexpressed,
	}
}

// indexWiring keys a section's rows by the identity the section is keyed on, after
// canonicalising each row.
func indexWiring[T any](rows []T, key func(T) string, canonical func(T) T) map[string]any {
	out := make(map[string]any, len(rows))
	for _, r := range rows {
		out[key(r)] = canonical(r)
	}
	return out
}

func wiringConnectionKey(c model.WiringConnection) string { return c.Name }
func wiringProviderKey(p model.WiringProvider) string     { return p.ObjectType }

// wiringFieldTypeKey keys a field-type declaration by object type AND field,
// because the section is stored one row per pair and that pair is what an operator
// edits. "document.due" is the entry; "document" would collapse two independent
// declarations into one reported line.
func wiringFieldTypeKey(ft model.WiringFieldType) string {
	return ft.ObjectType + "." + ft.Field
}

func wiringAttributeProviderKey(ap model.WiringAttributeProvider) string { return ap.Subject }

// diffWiringSection is the generic three-way comparison, run once per section.
func diffWiringSection(name string, deployed, local map[string]any) wiringDiffSection {
	s := wiringDiffSection{name: name}
	for key, d := range deployed {
		l, ok := local[key]
		if !ok {
			s.onlyDeployed = append(s.onlyDeployed, key)
			continue
		}
		if fields := differingWiringFields(d, l); len(fields) > 0 {
			s.differing = append(s.differing, wiringEntryDrift{key: key, fields: fields})
		}
	}
	for key := range local {
		if _, ok := deployed[key]; !ok {
			s.onlyLocal = append(s.onlyLocal, key)
		}
	}
	sort.Strings(s.onlyDeployed)
	sort.Strings(s.onlyLocal)
	sort.Slice(s.differing, func(i, j int) bool { return s.differing[i].key < s.differing[j].key })
	return s
}

// differingWiringFields names the fields on which two entries of the same section
// disagree.
//
// It walks the struct with reflect rather than comparing the fields by hand, and
// the reason is the failure mode of the alternative. A hand-written comparison is a
// list of field names that a change to model.WiringProvider does not have to
// update: the new field is simply never compared, the diff reports clean, and the
// deployment and the repository disagree with a green gate over it. Reflection
// cannot fall behind the type. The cost is that a field's REPORTED name is derived
// rather than chosen (see wiringFieldLabel) — a cosmetic answer to a silent one.
//
// The stamps are not special-cased here: canonicalWiring* has already dropped them
// on both sides, so there is exactly one place in this file that decides what is
// not part of the wiring.
func differingWiringFields(deployed, local any) []string {
	dv, lv := reflect.ValueOf(deployed), reflect.ValueOf(local)
	if dv.Type() != lv.Type() {
		// Unreachable: a section indexes one row type. Named rather than ignored, so
		// a future section wired up wrongly reports something instead of nothing.
		return []string{"(entries are not the same shape)"}
	}
	var fields []string
	for i := 0; i < dv.NumField(); i++ {
		if !reflect.DeepEqual(dv.Field(i).Interface(), lv.Field(i).Interface()) {
			fields = append(fields, wiringFieldLabel(dv.Type().Field(i).Name))
		}
	}
	return fields
}

// wiringFieldLabel spells a Go field name the way the seed document spells it, so a
// reported field is one an operator can search the file for: ObjectType ->
// object_type, GetOne -> get_one, IDColumn -> id_column, TTL -> ttl, MaxSize ->
// max_size, DeclaredKeys -> declared_keys.
//
// It is a derivation rather than a table because a table is a second list of field
// names to forget: a field added to model.WiringProvider gets a reasonable label
// here with no edit, where a missing table entry would print an empty column.
func wiringFieldLabel(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			// A boundary is either lower/digit followed by upper (MaxSize -> max_size) or
			// the end of a run of capitals followed by a lowercase letter (IDColumn ->
			// id_column). A run with nothing after it stays one word (TTL -> ttl).
			if !unicode.IsUpper(prev) || (i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// ---- Canonical form: the one place something is excluded from the comparison ----

// canonicalWiringConnection drops the stamps.
//
// A stamp is a fact about the LAST PUSH and not about the wiring: ReplaceWiring
// restamps the whole set to one instant, so two deployments wired identically from
// the same document carry different stamps, and comparing them would report drift
// on every entry of every store forever. This is the same reason a pulled document
// carries no stamp and the same reason a pull is byte-stable across pushes.
func canonicalWiringConnection(c model.WiringConnection) model.WiringConnection {
	c.CreatedAt, c.UpdatedAt = time.Time{}, time.Time{}
	return c
}

// canonicalWiringProvider drops the stamps and canonicalises the reference rows.
//
// The reference rows arrive sorted from both sides — a push sorts them because a
// references: map has no order, and a read returns them sorted — so the sort here
// is insurance rather than the mechanism, and it is done on a COPY so canonicalising
// never reorders the caller's slice. An empty slice becomes nil because
// reflect.DeepEqual tells []T{} from nil and the two mean the same thing here:
// "this entry declares no references".
func canonicalWiringProvider(p model.WiringProvider) model.WiringProvider {
	p.CreatedAt, p.UpdatedAt = time.Time{}, time.Time{}
	if len(p.References) == 0 {
		p.References = nil
		return p
	}
	refs := make([]model.WiringReference, len(p.References))
	copy(refs, p.References)
	model.SortWiringReferences(refs)
	p.References = refs
	return p
}

// canonicalWiringFieldType drops the stamps.
func canonicalWiringFieldType(ft model.WiringFieldType) model.WiringFieldType {
	ft.CreatedAt, ft.UpdatedAt = time.Time{}, time.Time{}
	return ft
}

// canonicalWiringAttributeProvider drops the stamps and canonicalises the declared
// key set.
//
// DeclaredKeys.Clone is what canonicalises it, and it is the right helper rather
// than a convenient one: it is the function that already preserves the distinction
// the type exists for — a NOT DECLARED set clones to nil keys and a DECLARED EMPTY
// one clones to an empty slice — so a declared-empty slot can never be flattened
// into a never-declared one on the way into a comparison that reflect.DeepEqual
// would then call equal.
func canonicalWiringAttributeProvider(ap model.WiringAttributeProvider) model.WiringAttributeProvider {
	ap.CreatedAt, ap.UpdatedAt = time.Time{}, time.Time{}
	ap.DeclaredKeys = ap.DeclaredKeys.Clone()
	return ap
}

// reconcileUnexpressedDeclaredKeys keeps a declared key set the local document
// CANNOT EXPRESS from reading as drift on every run.
//
// The seed attribute_providers: schema has no key for a declared key set yet — it
// gains one in its own story — while the column and model.DeclaredKeys have existed
// since the schema was created, because Setup creates and never migrates. So a slot
// whose set was written straight to storage is declared in the store and silent in
// every document, and a diff that compared the field would report drift no edit to
// the document could ever clear. That is the worst kind of red gate: permanent,
// unfixable, and about the one field a push cannot deploy.
//
// The reconciliation is deliberately ONE-DIRECTIONAL. Only a store-declared,
// locally-SILENT set is excused, and the slot is named in the report so the excusal
// is visible rather than assumed. The reverse — the document declares a set and the
// store does not — stays drift, because that is a real difference a push resolves;
// it cannot arise until the seed key lands, and it is already handled when it does.
//
// WHEN THE SEED KEY LANDS, this function goes away in the same change that starts
// parsing the key: at that point a silent document really does mean "not declared",
// the difference is expressible, and excusing it would hide drift instead of noise.
func reconcileUnexpressedDeclaredKeys(deployed, local []model.WiringAttributeProvider) ([]model.WiringAttributeProvider, []string) {
	localDeclares := make(map[string]bool, len(local))
	for _, ap := range local {
		localDeclares[ap.Subject] = ap.DeclaredKeys.Declared
	}

	out := make([]model.WiringAttributeProvider, 0, len(deployed))
	var slots []string
	for _, ap := range deployed {
		declaredLocally, present := localDeclares[ap.Subject]
		// A slot the document does not mention at all is already reported as
		// only-deployed; there is nothing to excuse on an entry that has no counterpart
		// to be compared with.
		if present && ap.DeclaredKeys.Declared && !declaredLocally {
			slots = append(slots, ap.Subject)
			ap.DeclaredKeys = model.DeclaredKeys{}
		}
		out = append(out, ap)
	}
	sort.Strings(slots)
	return out, slots
}

// ---- The report ----

// printWiringDiff renders the comparison.
//
// One table with a `section` column rather than four tables, because the rows carry
// the same three columns and a reader scanning for "is anything red?" should not
// have to read four headers to find out. The rows are grouped by VERDICT within each
// section — only deployed, then only local, then differs — because those are the
// three questions the command was asked, and each group is sorted by entry so two
// runs against the same pair of states print the same bytes.
//
// emptyStore is passed rather than re-derived so the one sentence an operator needs
// when their --store names a database Setup just created is printed by the reporter
// and not inferred from a table full of only-local rows.
func printWiringDiff(cmd *ucli.Command, diff wiringDiff, emptyStore bool) error {
	if emptyStore {
		// Said before the table, because it changes what the table MEANS: every local
		// entry below is only-local, and whether that is real drift or a typo in the DSN
		// is not visible from the rows themselves.
		fmt.Fprintln(cmd.Writer, "no shared wiring is deployed to this store, so every local entry below is only-local")
		fmt.Fprintln(cmd.Writer, "if you expected a pushed set here, check the --store DSN: one naming a database that does not exist yet is one Setup creates, and a fresh database is indistinguishable from a store nobody has pushed to")
	}

	w := tabwriter.NewWriter(cmd.Writer, 0, 0, 2, ' ', 0)
	if diff.hasDrift() {
		fmt.Fprintln(w, "section\tentry\tdrift")
		for _, s := range diff.sections {
			for _, key := range s.onlyDeployed {
				fmt.Fprintf(w, "%s\t%s\tonly deployed\n", s.name, key)
			}
			for _, key := range s.onlyLocal {
				fmt.Fprintf(w, "%s\t%s\tonly local\n", s.name, key)
			}
			for _, e := range s.differing {
				fmt.Fprintf(w, "%s\t%s\tdiffers: %s\n", s.name, e.key, strings.Join(e.fields, ", "))
			}
		}
	}
	if err := w.Flush(); err != nil {
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: writing the wiring diff", err)
	}

	if diff.hasDrift() {
		// Said in words as well as in the exit code: an operator reading the table has
		// to know which direction a push moves, because a push makes the DEPLOYMENT
		// match the DOCUMENT and there is no command that does the reverse.
		fmt.Fprintln(cmd.Writer, "the deployed wiring and this document differ")
		fmt.Fprintln(cmd.Writer, "`aperture wiring push --seed <file> --store <dsn>` makes the deployment match the document; there is no command that edits the document to match the deployment, and `aperture wiring pull` writes what is deployed to a NEW file for you to merge")
	} else {
		fmt.Fprintln(cmd.Writer, "no drift: the deployed wiring is exactly this document's four shared wiring sections")
	}

	// The two things that are REPORTED and are not drift go last, and to ErrWriter,
	// for the reason reportCollisions and printWiringPulled do it: they are caveats
	// about the comparison rather than part of its result, so a diff whose report is
	// being captured by a pipeline still shows them to the operator.
	if len(diff.unshareable) > 0 {
		for _, e := range diff.unshareable {
			fmt.Fprintf(cmd.ErrWriter,
				"local-only by design: %s entry %q selects kind: csv, which `wiring push` refuses — its only data source is a filesystem path — so no push can deploy it and it is not counted as drift\n",
				e.section, e.entry)
		}
	}
	if len(diff.unexpressedDeclaredKeys) > 0 {
		fmt.Fprintf(cmd.ErrWriter,
			"not compared: attribute slot %s declares a key set in the store, which the seed attribute_providers: schema has no key for yet, so this document cannot express it and the difference is not counted as drift\n",
			strings.Join(quoteEach(diff.unexpressedDeclaredKeys), ", "))
		fmt.Fprintln(cmd.ErrWriter,
			"not compared: `aperture wiring show --store <dsn>` prints the declared keys, so they can still be read")
	}
	return nil
}
