package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/frankbardon/aperture/authz"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// `aperture attributes` — the operator's window onto the attribute directories.
//
// Three subcommands, and they are deliberately not equals:
//
//	slots       what this deployment WIRES: which slots have a source, which LAYER
//	            each source fills, where it came from, and how each layer's own
//	            cache is tuned. No key, no bag.
//	query       a page OF a directory. The system-tier admin read, gated.
//	invalidate  drop cached bags so the next decision re-reads them. Gated.
//
// The CLI is the only surface this effort adds — there is no RPC method and no
// MCP tool for any of it. That is a scope decision with a reason: MCP is the
// surface where an agent listing a host's entire user table is least defensible,
// and a directory read is exactly the shape of request that should require a
// human at a shell with the deployment's seed file in hand.
//
// # Why `slots` is ungated and the other two are not
//
// `query` and `invalidate` go through the facade, which runs
// service.requireAttributeAdmin: the surface must be wired, the caller
// authenticated, and the caller a system-admin in its active account. The gate
// runs BEFORE the slot name is parsed, so a refused caller cannot learn which
// slots this deployment wires by reading which error came back.
//
// `slots` takes no actor and asks nothing of the gate, because it discloses
// nothing a caller did not already supply. It reads the SEED FILE the operator
// named on the command line, the SHARED WIRING rows in the --store they named, and
// the cache configuration this process built from the two; the answer is a
// restatement of the operator's own input, and requiring system-admin authority to
// read back a file you just passed in would only mean nobody could diagnose "is the
// user slot even wired?" without also holding the authority the diagnosis exists to
// explain. The wiring half needs no gate for the same reason `aperture wiring show`
// needs none: the --store credential the caller just supplied already grants full
// WRITE access to those rows. It never touches a provider, never names a key, and
// never prints a bag.

// attributeStack builds the decision stack the attribute commands read through,
// plus the cleanup that releases it. It is the SAME builder every other command
// uses, so the slots a listing reports are the slots a decision resolves through
// — the CLI cannot describe a wiring it does not itself run.
// It returns the STORE as well as the stack, because `slots` has to say where each
// layer came from and the shared wiring is a fact only the store holds. A caller with
// nothing to ask the store ignores it; the cleanup closes both either way.
func attributeStack(ctx context.Context, cmd *ucli.Command) (decisionStack, model.Storage, func(), error) {
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return decisionStack{}, nil, nil, err
	}
	stack, err := buildDecisionStack(ctx, cmd, store, cmd.String("seed"))
	if err != nil {
		_ = store.Close()
		return decisionStack{}, nil, nil, err
	}
	stack.reportCollisions(cmd.ErrWriter)
	return stack, store, func() {
		_ = stack.Close()
		_ = store.Close()
	}, nil
}

// attributeService adds the admin gate to that stack.
//
// It has to be built here rather than taken from decisionStack.newService: a
// one-shot command wires no gate, and service.ListAttributes refuses outright
// with APERTURE_UNIMPLEMENTED when it has a directory in hand and nothing to
// authorize against. That refusal is correct — a system-tier read must never
// degrade to "no gate, so serve it" — so the command supplies the gate, the same
// authz.NewGate(engine) `serve` mounts, over the same engine.
func attributeService(ctx context.Context, cmd *ucli.Command) (*service.Service, func(), error) {
	stack, _, done, err := attributeStack(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}
	svc := stack.newService(service.WithGate(authz.NewGate(stack.eng)))
	return svc, done, nil
}

// attributesCommand is `aperture attributes`, the parent of the three
// subcommands. It carries the staleness explanation once, where both `slots`
// (which shows the window) and `invalidate` (which closes it) inherit it.
func attributesCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "attributes",
		Usage: "Inspect the attribute directories a seed wires, read one, or drop cached bags",
		Description: "An attribute slot is a HOST DIRECTORY — the user table, the service-account\n" +
			"registry, the tenant catalogue — that a rule reads `principal.*` and `account.*`\n" +
			"out of. There are exactly three slots (user, machine, account) and each caches\n" +
			"the bags it has fetched, per slot, with its own ttl: and max_size:.\n\n" +
			"THE CACHE WINDOW IS A SECURITY PROPERTY, not only a tuning knob. Object metadata\n" +
			"that goes stale for a TTL is usually tolerable. An attribute bag is the ASKER'S\n" +
			"STANDING — the clearance, the department, the plan — so until a cached bag\n" +
			"expires, every decision about that subject keeps evaluating against access the\n" +
			"host has ALREADY TAKEN AWAY. Principals are the classic revoke case, and a\n" +
			"revocation that takes effect `ttl:` later is a revocation that has not happened\n" +
			"yet. What a slot's ttl: buys in fetch traffic it pays for in that delay.\n\n" +
			"So: pick a slot's ttl: for how fast its revocations must land, read back what a\n" +
			"deployment is actually running with `aperture attributes slots`, and close the\n" +
			"window on a specific subject with `aperture attributes invalidate`.\n\n" +
			"Reading a directory in bulk (`query`) and dropping cached bags (`invalidate`)\n" +
			"are SYSTEM-TIER operations: both require --principal holding system-admin\n" +
			"authority in --account, and a refusal returns nothing at all — no partial page,\n" +
			"no count, and nothing that tells an unauthorized caller which slots exist.",
		Commands: []*ucli.Command{
			attributesSlotsCommand(),
			attributesQueryCommand(),
			attributesInvalidateCommand(),
		},
	}
}

// attributesSlotsCommand is `aperture attributes slots`: the wiring listing.
func attributesSlotsCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "slots",
		Usage: "List every attribute slot's registration layers, where each one is wired from, and its own cache settings",
		Description: "Prints one row per LAYER of each of the three slots — user, machine, account —\n" +
			"with the source that layer is wired from, its cache freshness window, its\n" +
			"cached-bag cap, and how many bags this process holds for the slot.\n\n" +
			"A SLOT HAS TWO REGISTRATION LAYERS AND EACH ONE CACHES ON ITS OWN DECLARATION.\n" +
			"An `attribute_providers:` entry — a pushed wiring row, or one in this instance's\n" +
			"seed file — is the slot's SHARED layer, and an `attributes:` block is its LOCAL\n" +
			"one. A fetch reads their merge and the shared layer wins every key both serve, so\n" +
			"nothing is discarded and the inline bags still contribute the keys the external\n" +
			"source does not carry. The layers are printed in precedence order, highest first.\n\n" +
			"THE TTL COLUMN IS THE REVOCATION WINDOW, AND THERE IS ONE PER LAYER. A cached bag\n" +
			"keeps authorizing until it expires, so `ttl` is the longest a removed clearance\n" +
			"can keep working — for the keys THAT LAYER answers. Two layers are two caches and\n" +
			"never an average: a shared directory on a five-minute window beside an inline\n" +
			"block that never expires leaves the keys only the inline block serves unbounded.\n" +
			"`never` means a bag, once fetched, is only dropped by eviction or by an explicit\n" +
			"`aperture attributes invalidate` — correct for a fixed inline block, dangerous\n" +
			"for a live directory.\n\n" +
			"THE SOURCE COLUMN NAMES THE PLACE, not just the kind. `(shared wiring)` is a row\n" +
			"in the deployment's database, put there by `aperture wiring push` and read by\n" +
			"every instance; `(--seed file)` is this instance's own document; `(host)` is a\n" +
			"provider the binary embedding Aperture registered in Go, which no document\n" +
			"describes. A slot with no source at all reads `(unwired)`: that is not an empty\n" +
			"directory, it is a party this deployment declared nothing for, and every fetch\n" +
			"against it fails — leniently on the decision path, which means the rule sees only\n" +
			"the floor bag.\n\n" +
			"The `cached` column counts THIS process's cache for the whole SLOT, summed across\n" +
			"its layers — a subject both layers serve is held twice, because it is cached\n" +
			"twice — so it appears once, on the slot's first row, and reads `-` on the second\n" +
			"layer's. A one-shot invocation starts cold, so it reads 0; it is the number that\n" +
			"matters in a long-running `aperture serve`.\n\n" +
			"No actor is required: this reports the wiring this deployment is running — the\n" +
			"seed file you passed, the shared wiring rows in the --store you named, and the\n" +
			"configuration this process built from them. It contacts no provider and prints\n" +
			"no subject key and no attribute value.",
		Flags:  storeFlags(),
		Action: runAttributeSlots,
	}
}

// runAttributeSlots prints ONE ROW PER LAYER, not one per slot.
//
// # Why per layer
//
// A slot has two registration layers and each one caches on its OWN declaration
// (provider.AttributeLayer). A single-row listing read `CacheConfigFor`, which
// reports the GOVERNING layer's configuration — so a slot whose shared directory is
// on a five-minute window and whose inline block never expires printed `ttl 5m` with
// no hint that a second cache existed. An operator who has just revoked something
// reads 5m and believes the window is closed; for every key only the local layer
// serves it is unbounded. A slot's ttl: is a REVOCATION WINDOW, so "one number per
// slot" is not a simplification, it is the wrong number.
//
// # Where each layer came from
//
// Three origins, and the listing must tell them apart, because on the one command
// whose job is "is the user slot wired, and from where?" a database-wired slot used
// to print `(host)` — the label reserved for "a Go host wired this in Go" — which
// points the operator away from the database that actually wired it. The `ttl` column
// was read off the registry and therefore right, which made the source column the
// only wrong cell and so the believable one.
//
// The origin is a FACT read from the two places wiring can come from, never inferred:
// the shared wiring set in the store, and this instance's own document. The document
// is asked through seed.Document.AttributeSlotSources, so the precedence rule is not
// re-derived here; the LAYER order comes from provider.AttributeRegistry.Layers,
// which is the registry's own precedence. A filled layer neither accounts for is a Go
// registration, and stays `(host)`.
func runAttributeSlots(ctx context.Context, cmd *ucli.Command) error {
	// The local document, read before anything is opened so a malformed file is
	// reported without a store being touched. Its refusals are its own (bootError's
	// pass-through guard): a literal dsn: here must say to rotate the credential.
	doc, err := seedDocument(cmd.String("seed"), classifyStore(cmd.String("store")))
	if err != nil {
		return err
	}
	sources := doc.AttributeSlotSources()

	stack, store, done, err := attributeStack(ctx, cmd)
	if err != nil {
		return err
	}
	defer done()

	// The SHARED half. Read from the same store the stack was built over, and read
	// rather than guessed: a slot whose shared layer this document does not declare
	// was wired either by the database or by Go, and only the rows can say which.
	wiring, err := readSharedWiring(ctx, store)
	if err != nil {
		return err
	}
	shared := make(map[string]string, len(wiring.AttributeProviders))
	for _, ap := range wiring.AttributeProviders {
		shared[strings.TrimSpace(ap.Subject)] = strings.TrimSpace(ap.Kind)
	}

	w := tabwriter.NewWriter(cmd.Writer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "slot\tlayer\tsource\tttl\tmax-size\tcached")
	for _, slot := range provider.AttributeSlots() {
		layers := stack.attributes.Layers(slot)
		if len(layers) == 0 {
			// Unwired is not an error and not an empty directory: it is a
			// deployment that declared no source for this party. Every fetch
			// against it is APERTURE_ATTRIBUTE_PROVIDER_UNREGISTERED, which the
			// decision path treats leniently (the floor bag) and an enumeration
			// does not.
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", slot, "-", "(unwired)", "-", "-", "-")
			continue
		}
		// The counters are the registry's and they are per SLOT, summed across its
		// layers — there is no per-layer Stats, and a subject both layers serve is
		// held twice because it is cached twice. So the number appears ONCE, on the
		// slot's first row, rather than being printed twice as though each layer
		// held it.
		stats, _ := stack.attributes.Stats(slot)
		for i, layer := range layers {
			cfg, _ := stack.attributes.CacheConfigForLayer(slot, layer)
			cached := "-"
			if i == 0 {
				cached = strconv.Itoa(stats.Entries)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				slot, layer, attributeLayerSource(layer, slot, sources, shared),
				renderTTL(cfg.TTL), renderMaxSize(cfg.MaxSize), cached)
		}
	}
	return w.Flush()
}

// attributeLayerSource names WHERE one filled layer of a slot came from: the kind,
// and the place that declared it.
//
// The three answers are the three places wiring can come from, and the listing exists
// to tell them apart:
//
//	sql (shared wiring)     a row in the deployment's database
//	csv (--seed file)       an attribute_providers: entry in this instance's own file
//	inline (--seed file)    the attributes: block in this instance's own file
//	(host)                  registered in Go, by the binary embedding Aperture
//
// local is what this instance's document says (seed.Document.AttributeSlotSources:
// the external kind: when the document declares one, AttributeSourceInline when only
// the inline block fills the slot). shared maps a slot to the kind of its pushed row.
//
// The SHARED layer is filled by an attribute_providers: entry from either source, so
// the document is asked first: a local entry is a route nothing pushed and a pushed
// row is not in the document, and they cannot both exist — the boot refuses that
// collision by name.
//
// The LOCAL layer is filled only by an inline attributes: block, so the document
// having anything to say about the slot at all is what identifies it. A filled layer
// with no declaration behind it in either place is a Go registration, reported as
// `(host)` rather than guessed at.
func attributeLayerSource(layer provider.AttributeLayer, slot provider.AttributeSlot, local, shared map[string]string) string {
	const host = "(host)"
	name := slot.String()
	switch layer {
	case provider.AttributeLayerShared:
		if kind := local[name]; kind != "" && kind != seed.AttributeSourceInline {
			return kind + " (--seed file)"
		}
		if kind, ok := shared[name]; ok {
			return orUnknownKind(kind) + " (shared wiring)"
		}
		return host
	case provider.AttributeLayerLocal:
		if local[name] != "" {
			return seed.AttributeSourceInline + " (--seed file)"
		}
		return host
	}
	return host
}

// orUnknownKind spells a stored kind that is somehow empty. A row like that cannot
// boot — checkWiringKind refuses it — so this is a defence against printing a bare
// " (shared wiring)" if one ever arrives from a hand-written row.
func orUnknownKind(kind string) string {
	if kind == "" {
		return "(unknown kind)"
	}
	return kind
}

// renderTTL spells a slot's freshness window for the listing. Zero is rendered
// as the word rather than "0s" because the two read very differently to an
// operator scanning the column: "0s" invites "expires immediately", and the
// truth is the opposite — the entry never expires on its own.
func renderTTL(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return d.String()
}

// renderMaxSize spells the cached-bag cap. Zero means no cap at all, which is
// worth a word for the same reason: "0" would read as "caches nothing".
func renderMaxSize(n int) string {
	if n <= 0 {
		return "unbounded"
	}
	return strconv.Itoa(n)
}

// attributeRecordOut is the JSON shape `query` prints. It exists so the output
// has lower-case, stable field names without putting json tags on
// provider.AttributeRecord, whose shape is a library contract and not a
// presentation format.
type attributeRecordOut struct {
	ID         string            `json:"id"`
	Attributes provider.Metadata `json:"attributes"`
}

// attributesQueryCommand is `aperture attributes query`: the gated bulk read.
func attributesQueryCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags, metadataFilterFlags()...)
	flags = append(flags,
		&ucli.IntFlag{Name: "limit", Usage: "cap the number of returned records (<=0 means the default; the registry clamps it regardless)"},
	)
	return &ucli.Command{
		Name:      "query",
		Usage:     "Read a page of one attribute slot's directory (system-admin tier)",
		ArgsUsage: "<slot>",
		Description: "Returns up to --limit records of <slot> — user, machine, or account — as a JSON\n" +
			"array of {id, attributes}, narrowed by attribute predicates.\n\n" +
			"THIS IS A SYSTEM-TIER READ. Unfiltered, it returns the head of the host's user\n" +
			"table, keys and bags together, so it requires --principal holding system-admin\n" +
			"authority in --account. A refusal returns NOTHING — no partial page, no count,\n" +
			"and no way to tell an empty slot from a full one or from an unwired one. Ask\n" +
			"`aperture explain` about your own authority if a refusal is unexpected.\n\n" +
			"--field and --fields-json narrow the result by ATTRIBUTE, on exactly the\n" +
			"predicate `aperture enumerate` applies to object metadata: predicates are ANDed,\n" +
			"a field the bag does not carry never matches, a list-valued field matches by\n" +
			"membership, and everything else matches by TYPED equality, so the string \"5\"\n" +
			"never matches the number 5. --field always sends a string; use --fields-json\n" +
			"when a number, bool, or list is genuinely meant:\n\n" +
			"  aperture attributes query user --principal alice --account acme \\\n" +
			"    --field department=eng --fields-json '{\"clearance\":3}'\n\n" +
			"Both may be given together: --fields-json is merged FIRST and --field entries\n" +
			"then override it by key.\n\n" +
			"A slot whose sql: entry declares no get_all: is FETCH-ONLY by design — it can\n" +
			"answer the decision path without exposing the whole table to an enumeration —\n" +
			"and this command reports that provider's coded refusal rather than an empty\n" +
			"page.",
		Flags:  flags,
		Action: runAttributeQuery,
	}
}

func runAttributeQuery(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 1 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"attributes query takes exactly 1 argument (<slot>, one of %s), got %d",
			slotList(), cmd.Args().Len())
	}
	// Parsed BEFORE anything is opened: a malformed predicate is a usage error,
	// and there is no reason to boot a decision stack to report one.
	fields, err := parseMetadataFilter(cmd.String(fieldsJSONFlagName), cmd.StringSlice(fieldFlagName))
	if err != nil {
		return err
	}
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, done, err := attributeService(ctx, cmd)
	if err != nil {
		return err
	}
	defer done()

	// The slot string is handed over UNPARSED. service.ListAttributes runs the
	// gate first and parses second, on purpose; parsing here would move the
	// disclosure the ordering exists to prevent back into the CLI.
	recs, err := svc.ListAttributes(ctx, actor, cmd.Args().Get(0), provider.AttributeFilter{
		Fields: fields,
		Limit:  cmd.Int("limit"),
	})
	if err != nil {
		return err
	}
	out := make([]attributeRecordOut, 0, len(recs))
	for _, rec := range recs {
		out = append(out, attributeRecordOut{ID: rec.ID, Attributes: rec.Attributes})
	}
	return printJSON(cmd, out)
}

// attributesInvalidateCommand is `aperture attributes invalidate`: the gated
// cache drop.
func attributesInvalidateCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "id", Usage: "drop only this subject's cached bag (a bare principal or account id); omit to clear the whole slot"},
		&ucli.BoolFlag{Name: "all", Usage: "clear EVERY slot's cache; takes no <slot> argument and no --id"},
	)
	return &ucli.Command{
		Name:      "invalidate",
		Usage:     "Drop cached attribute bags so the next decision re-reads them (system-admin tier)",
		ArgsUsage: "<slot>",
		Description: "Drops cached bags, so the next decision about the affected subjects pulls fresh\n" +
			"ones from the host directory. Three forms:\n\n" +
			"  aperture attributes invalidate user --id alice   one subject, one slot\n" +
			"  aperture attributes invalidate user             every bag in one slot\n" +
			"  aperture attributes invalidate --all            every bag in every slot\n\n" +
			"INVALIDATION IS A SECURITY CONTROL, NOT A PERFORMANCE KNOB. A cached attribute\n" +
			"bag is the asker's standing, so a REVOKED CLEARANCE KEEPS AUTHORIZING until that\n" +
			"bag expires: for the length of the slot's ttl:, every decision about that\n" +
			"subject is made against access the host has already removed. Waiting the window\n" +
			"out is not a remedy, it is the exposure. An operator who has just removed\n" +
			"someone's access invalidates that subject here, and then the removal is true.\n\n" +
			"Scope: this drops the caches of THE PROCESS THAT RUNS IT. That makes it exact\n" +
			"for a host embedding Aperture (it is the operator's spelling of\n" +
			"provider.AttributeRegistry.Invalidate, which such a host calls the moment its\n" +
			"directory changes) and it makes a ONE-SHOT invocation self-contained: this\n" +
			"process starts with a cold cache and exits with it, so there is nothing here for\n" +
			"a stale bag to survive in. For a long-running `aperture serve`, the controls\n" +
			"that reach ITS cache are the slot's ttl: — set it to how fast that directory's\n" +
			"revocations must land — and a restart.\n\n" +
			"Requires --principal holding system-admin authority in --account: the result\n" +
			"reports whether a bag was cached, which is a fact about who has recently been\n" +
			"decided about, and clearing a large slot costs the next wave of decisions a\n" +
			"provider round-trip each.",
		Flags:  flags,
		Action: runAttributeInvalidate,
	}
}

func runAttributeInvalidate(ctx context.Context, cmd *ucli.Command) error {
	var (
		all  = cmd.Bool("all")
		id   = cmd.String("id")
		args = cmd.Args()
	)
	// The three forms are mutually exclusive, and a conflict is refused rather
	// than resolved by precedence: "--all plus a slot" has two plausible readings
	// (everything, or just that slot) and guessing at the broader one would clear
	// caches the operator did not ask to clear.
	switch {
	case all && args.Len() > 0:
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"attributes invalidate --all clears every slot and takes no <slot> argument; drop --all to clear one slot")
	case all && id != "":
		return aerr.New(aerr.APERTURE_INVALID_INPUT,
			"attributes invalidate --all clears every slot and takes no --id; drop --all to invalidate one subject")
	case !all && args.Len() != 1:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"attributes invalidate takes exactly 1 argument (<slot>, one of %s) or --all, got %d",
			slotList(), args.Len())
	}

	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, done, err := attributeService(ctx, cmd)
	if err != nil {
		return err
	}
	defer done()

	if all {
		if err := svc.InvalidateAllAttributes(ctx, actor); err != nil {
			return err
		}
		fmt.Fprintln(cmd.Writer, "cleared every attribute slot's cache")
		return nil
	}

	// Unparsed, for the same reason as query: the facade gates first and parses
	// second so a refusal cannot report which slots exist.
	slot := args.Get(0)
	if id == "" {
		if err := svc.InvalidateAttributeSlot(ctx, actor, slot); err != nil {
			return err
		}
		fmt.Fprintf(cmd.Writer, "cleared the %s slot's cache\n", slot)
		return nil
	}
	dropped, err := svc.InvalidateAttribute(ctx, actor, slot, id)
	if err != nil {
		return err
	}
	if dropped {
		fmt.Fprintf(cmd.Writer, "dropped the cached %s bag for %q\n", slot, id)
		return nil
	}
	// Not an error: "nothing was cached" is the state the caller asked for. It is
	// reported rather than silently succeeding, because an operator invalidating
	// a subject they believe is cached wants to know their key did not match.
	fmt.Fprintf(cmd.Writer, "no cached %s bag for %q\n", slot, id)
	return nil
}

// slotList renders the closed slot set for a usage error. It reads the set from
// provider rather than restating it, so a CLI message cannot name a different
// three from the registry.
func slotList() string {
	slots := provider.AttributeSlots()
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.String())
	}
	return strings.Join(out, ", ")
}
