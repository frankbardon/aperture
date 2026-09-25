# CLAUDE.md — Aperture

Conventions catalog for `github.com/frankbardon/aperture`. **This file is the
authority.** It supersedes the per-effort planning artifacts under `.planning/`,
which are story scratch and must never be cited as conventions — several of them
predate decisions this file records (the Pulse dependency, SQLite-only storage).
Where this file is silent, `skills/*.md` and `docs/src/` govern.

## Project overview

Aperture is a fine-grained access-control engine for the frankbardon/* family.
It is **library-first**: the public Go packages at the module root are the
product, and every surface (CLI, Twirp/HTTP, MCP) is a thin translator over a
single decision engine. Aperture mirrors **Orbit** structurally and **Lattice**
for the MCP boundary, but keeps its public packages at the module root like
**Pulse** rather than under `internal/`.

The decision API is `Check` / `Enumerate` / `Search` / `Explain`, each available
single and bulk-batched. `Search` resolves a NAME to an id — it ranks the objects
`Enumerate` would return, never a wider set.

## Stack & build

- **Go 1.26.1**, `CGO_ENABLED=0`, pure-Go end to end. Module
  `github.com/frankbardon/aperture`.
- **CLI** is `urfave/cli/v3`; `cmd/aperture/main.go` only assembles the command
  tree (no business logic).
- **Rules** use the `github.com/expr-lang/expr` evaluator **directly** (pure-Go,
  the same engine Pulse wraps) — Aperture has no dependency on Pulse. The rules
  package renders its AST to an expr-lang expression and compiles it in-process.
- **Storage**: hand-written SQL, `modernc.org/sqlite` (pure-Go) +
  `storage/postgres` (pgx, a full peer — not a variant) + an in-memory impl
  behind one `Storage` interface. No ORM / sqlc / migration tool / schema
  versioning: `Setup` **creates**, never migrates, and a schema change is a hard
  break. See `skills/storage-schema.md`.
- **SQL providers** (`sqlprovider/`, a *host* data source — not Aperture's own
  storage) depend on a two-method `Querier` and link **no** driver. Two packages
  do, each with a blank import of `github.com/jackc/pgx/v5/stdlib` used through
  `database/sql`, Postgres only: `seed/connection.go` (host-provider
  connections) and `storage/postgres/postgres.go` (Aperture's own backend). The
  two share no connection handling and there is **no import edge between `seed/`
  and `storage/`** in either direction — do not create one. pgx over `lib/pq` on
  a correctness argument —
  `lib/pq` returns `[]byte` for `numeric` and `uuid`, which the value model
  cannot tell from `jsonb` — at a measured cost of +3,589,088 bytes for the
  driver alone (+4,246,608, +14.8%, for the epic as landed). Both pure Go, so
  `CGO_ENABLED=0` holds.
- **RPC/HTTP**: `net/http` ServeMux + Twirp (`internal/server/`, proto at
  `internal/wire/rpc/service.proto`), with an admin UI shell served from
  `internal/server/static/`.
- **MCP**: SDK-free core (`mcp/`, surfaced as `aperture mcp` over stdio); one
  adapter package may import the protocol SDK, enforced by a firewall test.

```bash
make build   # produce bin/aperture (-ldflags="-s -w" -trimpath, CGO off)
make test    # go test ./...
make fmt     # go fmt ./...
make vet     # go vet ./...
make lint    # vet + staticcheck (degrades to vet-only when no USABLE analyser is on PATH)
```

## Coding conventions

### Errors

- Every failure is an `APERTURE_*` coded error from the root `errors/` package.
- Codes are **SCREAMING_SNAKE**, namespaced `APERTURE_*`, defined in
  `errors/codes.go` and listed in `AllCodes`.
- Each code has a `Registry` entry with a `Message` and either at least one
  `Fixup` or `FixupNotApplicable=true`. Gated by `TestCodesHaveFixups`.
- Construct via `errors.New` / `Newf` / `Wrap` / `Wrapf`; recover the code with
  `errors.CodeOf`.
- **`Wrap`/`Wrapf` DO re-stamp.** They are not pass-through. `errors/coded_error.go`
  builds a fresh `CodedError` with whatever code it is handed, and `CodeOf` uses
  `errors.As`, which reports the **outermost** code — so wrapping an already-coded
  error in a different code observably replaces the code a caller reads.
- Pass-through is a **call-site idiom**, and you must write it yourself whenever
  the error you are wrapping might already be coded:

  ```go
  if aerr.CodeOf(err) != "" { return err }        // it already says something better
  return aerr.Wrap(aerr.APERTURE_X, "...", err)   // only classify what nothing else did
  ```

  Live examples: `internal/cli/buildStore`'s `bootError`, `delegation.authorize`,
  `provider/registry.go`, `storage/sqlite/sqlite.go`.
- Forgetting the guard is not cosmetic: it buries a specific, actionable code
  (`APERTURE_STORAGE_CONSTRAINT`, `APERTURE_STORAGE_SCHEMA_INCOMPATIBLE`,
  `APERTURE_CONFIG_INVALID`) and its registry fixups under a generic one, and the
  operator loses the remedy. Two surfaces were doing exactly that; both are fixed,
  and a same-code re-stamp is invisible to `CodeOf`, so tests assert **chain
  depth** — exactly one Aperture-coded error in the chain — not just the code.

### Library-first

- Business logic lives in the public root packages. `cmd/aperture/` is a thin
  adapter; later stories add manual DI in the `serve` command (no wire/fx/dig).
- Config is env vars (`APERTURE_*`) + optional YAML; `.env` via dotenv.

### Naming

- No predecessor references (no `Aperture2` / `LegacyX`).

## Update Demand

Any change to a registered surface MUST update the matching `skills/` document in
the **same PR**. A surface change that lands without its doc update is a
non-skippable CI failure. The rule itself is documented in
`skills/update-demand.md` and is self-protecting.

| If you change... | You MUST also update... | Enforced by |
|---|---|---|
| An Aperture error code | `errors/codes.go` (`AllCodes` + `Registry` entry with Message + Fixups) | `TestCodesHaveFixups` |
| A `skills/*.md` doc | its YAML frontmatter (`name` matching the file stem + `description`) | `TestEverySkillHasFrontmatter` |
| The Update-Demand rule | `skills/update-demand.md` (must remain present with frontmatter) | `TestUpdateDemandDocPresent` |
| A rule operator (`Op*` / `opSpecs` in `rules/ast.go`) | `OP_SPECS` in `internal/server/static/js/rules-serializer.js`, the palette in `rules.js`, and `skills/rules-engine.md` | `TestEditorOperatorTablesAgree`, `TestEditorASTContractCoversEveryOperator` |
| A right-operand shape (`rightShape` in `rules/ast.go`) | `RIGHT` in `rules-serializer.js` **and** `jsShapeNames` + `editorJSONForOp` in `rules/editor_js_contract_test.go` | `TestEditorOperatorTablesAgree`, `TestEditorASTContractCoversEveryOperator` |
| A collection operator's shape expectation (`collOps` in `rules/shape.go`) | the matching `opSpecs` entry in `rules/ast.go` | `TestCollectionOperatorTablesAgree` |
| A date operator's runtime policy (`dateOps` in `rules/date.go`) | the matching `opSpecs` entry in `rules/ast.go`, and the deny-safe policy in `skills/rules-engine.md` | `TestDateOperatorTablesAgree` |
| The date-operator SET (an `opSpecs` entry gaining or losing `kind: renderDate`) | `DATE_OPS` in `rules-serializer.js` (it cannot live on an `OP_SPECS` entry — the Go scanner requires the literal `{ right: RIGHT.X }` form) and the serializer section of `skills/ui-shell.md` | `TestEditorOperatorTablesAgree` |
| What `rules.Clock` drives (`rules/engine.go` `WithClock`, `rules/now.go`) | the `WithClock` doc comment and "The clock, and one `NOW` per decision" in `skills/rules-engine.md` | reviewed; no registry gate |
| The `principal` floor bag (`rules/engine.go` `principalBag`: which keys it stamps, that it stamps them LAST, that the map is fresh) or which attribute slot a principal kind resolves (`provider.principalSlot`) | the `principal` root in `skills/rules-engine.md` ("Context variables"), "The floor bags, and why the floor wins" in `skills/attribute-providers.md`, and `docs/src/concepts/rules.md` ("The floor bag, and `principal.kind`" + the roots table) | reviewed; no registry gate — behaviour by `rules/principal_floor_test.go` (`TestTheFloorIsNotShadowedByTheProviderBag`) and `engine/principal_attributes_test.go`. **The floor winning on collision is the load-bearing half**: if a host bag could shadow `id`, `principal.id == object.owner` would silently compare something else, with no error anywhere |
| The `account` floor bag (`rules/engine.go` `accountBag`), the `AccountResolver` seam, or the account-wildcard SHORT-CIRCUIT (`Engine.accountAttributes`: `"*"` never reaches a resolver and `account` reads floor-only) | the `account` root in `skills/rules-engine.md` ("Context variables"), "The `account` root, and the wildcard" in `skills/attribute-providers.md`, and `docs/src/concepts/rules.md` ("The `account` floor, and the wildcard") | reviewed; no registry gate — behaviour by `rules/account_floor_test.go`, `engine/account_attributes_test.go` and `engine/account_boundary_test.go`. **Making the wildcard an error instead of a floor breaks platform scope**: `service/reads.go` really does run `engine.Check` with `Request.Account == model.AccountWildcard`, so every rule-backed grant would become undecidable there — including the ones that never mention `account` |
| When `attributes_floor_only` FIRES (`rules/engine.go` `recordFloorOnly` / `readsBeyondFloor` — that it gates on the paths the rule NAMES, not on what a comparison did) | "`attributes_floor_only`" in `skills/attribute-providers.md` and the note-kind list in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `rules/floor_only_test.go`. **Both directions are failures**: unconditional emission puts two notes on every trace of every rule-backed grant in the many deployments that wire no provider and buries the shape/date notes, while narrowing it past the declared reads hides the one hazard the note exists to expose |
| What the per-decision attribute memo spans (`rules/attributes.go` `WithDecisionAttributes` / `DecisionAttributes`, or a decision boundary in `engine/` that opens — or stops opening — the scope) | the type's doc comment and "One bag per decision" in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `rules/attributes_test.go` and `engine/attribute_memo_test.go`, which count resolutions with a fake directory. **A boundary that stops opening the scope fails by passing**: the decision is still correct, only per-object and inconsistent |
| A `rules.NoteKind` (`rules/notes.go`) | its `Note.String()` case and the note-kind list in `skills/rules-engine.md` | reviewed; no registry gate |
| The rule-evaluation context — `scope.GrantContext`'s fields, `scope.RuleEvaluator.Selected`, or `rules.PrincipalResolver.Attributes` | all three move together (they are one signature in three packages), plus `docs/src/concepts/scopes.md` ("The resolver contract" and the `RuleEvaluator` seam row, including its worked `GrantContext` literal), `docs/src/concepts/rules.md` (`Engine.Selected`'s step list), and "Context variables" in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `engine/grant_context_test.go` (every entry point supplies the account and kind; the kind costs no second `GetPrincipal`), `scope/resolvers_test.go` (`TestRuleContextReachesTheEvaluator`, both rule-backed strategies), `rules/engine_test.go`. **A value carried but not handed on compiles and is silently wrong** — a rule would read attributes for the wrong account — which is why the assertions are per-seam rather than end-to-end only |
| A callable rule function (`defaultFunctions` in `rules/compiler.go`) or a blocked builtin (`blockedCallNames`) | `FUNCTIONS` / `BLOCKED_CALLS` in `rules-serializer.js` | `TestEditorVocabularyTablesAgree` |
| An AST node type, variable root, or var-path grammar | `TYPES` / `ROOTS` / `VAR_PATH` in `rules-serializer.js` | `TestEditorVocabularyTablesAgree` |
| A relative-date vocabulary (`relativeAnchors` / `relativeUnits` / `relativeSnaps` in `rules/relative.go`) | `ANCHORS` / `UNITS` / `SNAPS` in `rules-serializer.js`, the four controls in `rules.js`, and "Relative dates" in `skills/rules-engine.md` | `TestEditorVocabularyTablesAgree` |
| The relative-date node's JSON shape or field validation (`Node.Anchor/Offset/Unit/Snap`, `validateRelativeDate`) | the node's validator + `NODE_SPECS` entry in `rules-serializer.js` and "Relative dates" in `skills/rules-engine.md` | `TestEditorValidationMessagesAgree`, `TestEditorASTContract` |
| The relative-date resolution semantics (`rules/calendar.go`: month-end clamping, the snap-then-offset order, `endOf*` precision, ISO-Monday weeks, the representable range) | "UTC, clamping, and the order of operations" in `skills/rules-engine.md` | reviewed; no registry gate — behaviour by `rules/calendar_test.go`, including a source scan that forbids `AddDate` / `time.Local` in `rules/` |
| The metadata value model (`provider/metadata.go`: legal shapes, depth cap, size cap) | `skills/metadata-values.md` and `docs/src/concepts/providers.md` | `TestEverySkillHasFrontmatter` (doc presence); model behaviour by `provider/metadata_test.go` |
| The date value model (`provider/date.go`: the canonical forms, the accept/reject set, `DateReason`) | `skills/metadata-values.md` ("Dates") and `docs/src/concepts/providers.md` | `TestEverySkillHasFrontmatter` (doc presence); model behaviour by `provider/date_test.go` |
| A loader's spelling of the value model (a CSV column suffix — `:int`, `:list<T>`, `:json`, `:date`, `:datetime` — or a seed key such as `objects:` / `field_types:` / `attributes:`) | `skills/metadata-values.md` ("How each loader spells the model"), the loader's package doc, `docs/src/concepts/providers.md`, and `docs/src/concepts/seed.md` for a seed key | reviewed; no registry gate |
| The seed `attributes:` schema (`seed.Attribute`'s keys, the `subject:` vocabulary, the per-slot dedup rule) or what `Document.BuildAttributeRegistry` / `Document.HasAttributeSources` cover | the field's doc comment, `skills/metadata-values.md` ("The seed document's `attributes:` section"), `skills/attribute-providers.md` ("From a seed document"), `docs/src/concepts/seed.md` ("Inline subject attributes"), and `docs/src/concepts/rules.md` ("Wiring a `*provider.AttributeRegistry`") — a NEW attribute source section must be OR'd into `HasAttributeSources`, which is why that gate lives beside the fields it counts rather than in `internal/cli` | reviewed; no registry gate — behaviour by `seed/attribute_test.go` (incl. `TestAttributeWiringIsNotModelState`) and `internal/cli/attributes_test.go` |
| The attribute SEAM — the `AttributeSlot` set (`provider.AttributeSlots()` / `ParseAttributeSlot`), the `AttributeProvider` / `AttributeFilter` / `AttributeRecord` contract, or `AttributeRegistry`'s registration and LENIENCY behaviour (which codes `Attributes` / `AccountAttributes` collapse to a nil bag) | `skills/attribute-providers.md` ("The three slots", "The leniency contract", "Containment"), `docs/src/concepts/providers.md` ("Attribute providers", which restates the slot set, the leniency contract and the containment boundary), the `APERTURE_ATTRIBUTE_*` fixups in `errors/codes.go`, and — for a slot's spelling — `seed/attribute.go`'s `slotNames` and `internal/cli`'s `slotList`, both of which DERIVE the set rather than restate it | `provider.TestAttributeRegistryIsNotAScopeLister` + `TestProviderPackageImportsOnlyIdentityAndErrors` gate the containment half; the leniency half is reviewed, with behaviour by `provider/attribute_leniency_test.go`, `provider/attribute_wildcard_test.go` and `rules/attribute_leniency_test.go` (incl. `TestAMissingBagWidensAnExclusiveGrant`). **A fourth slot is a contract change, not a feature** — the closed set is what stops a host registering a party the engine cannot fetch, which surfaces as an empty bag, which is a silent denial. **Widening leniency by one more code is access-widening in an exclusive grant**, and nothing in a verdict says so |
| Which call may WRITE a slot's attribute cache (`provider.AttributeRegistry.Enumerate` gaining a `cache.Set`, or `Fetch` losing one) | "`Enumerate` never writes the slot's cache" **and** "The administrative read" in `skills/attribute-providers.md`, and "The listing does not write the decision path's cache" in `docs/src/concepts/providers.md` | reviewed; no registry gate — behaviour by `provider/attribute_enumerate_projection_test.go` and the `enumeration does not write the slot cache` case in `provider/attribute_test.go`, with the end-to-end shape in `seed/attribute_provider_sql_test.go` (`TestAttributeProviders_SQLAnEnumerationDoesNotNarrowTheRuleBag`) and, over a live server, the enumerate-then-decide ORDER in `seed/postgres_integration_test.go`. **The cache is `Fetch`'s, and `Query`'s bag is allowed to be narrower** — `sqlprovider.AttributeConfig.ListQuery` is optional and need only select a bare id, so warming from a listing substitutes the DISPLAY projection for the authoritative bag for the whole `ttl`. An ABSENT key is not a wrong key: every predicate over it is false, so an inclusive grant denies and an **exclusive** one stops excluding, and `aperture attributes query` becomes a way to widen access with nothing in any verdict, trace or note to say why. No gate is possible — a bag is opaque host data and an absent key is indistinguishable from a genuinely unset one. **The object `provider.Registry.List` had the same bug and is fixed DIFFERENTLY** (the row below): it is a decision-path call whose `Fetch` follows in the same candidate walk, so its warm is repaid inside the same decision and is made CONDITIONAL rather than removed, where `Enumerate` has no `Fetch` behind it at all and an `AttributeProvider` is deliberately given no promise to make |
| What licenses an OBJECT listing to write the per-type metadata cache (`provider.FetchCompleteLister`, `typeEntry.warmsFromListing` in `provider/registry.go`, or `sqlprovider.Provider.ListedMetadataMatchesFetch` and where the two column projections are observed) | `docs/src/concepts/providers.md` ("The listing and the fetch must be the same bag", **and** the `List` bullet in "The Registry" + "Fetch, List, and the id column"), `skills/sql-provider.md` ("Project the same columns in both statements" + its Update-Demand row + the concurrency paragraph), "The object registry had the same bug, and is fixed differently" in `skills/attribute-providers.md`, `docs/src/concepts/seed.md` (the `kind: sql` statement-pairing note), and the per-candidate-metadata sentences in `skills/decision-api.md` + `docs/src/library/decision-api.md`, which must not restate the warm as unconditional | reviewed; no registry gate — behaviour by `provider/list_projection_test.go`, `sqlprovider/list_projection_test.go`, `seed/provider_sql_projection_test.go`, and the equal-projection positive control in `seed/postgres_integration_test.go`. **Both directions are failures, which is why the promise is EQUALITY and not containment**: a narrower `get_all` caches a bag with a field missing, and an absent field makes every predicate over it false — denying an inclusive grant and stopping an EXCLUSIVE one excluding — while a wider one caches a field `Fetch` would never produce, which compares true until the entry expires. **Removing the warm instead of conditionalising it is the other regression**: `Registry.List` is how a rule-backed inclusive scope gathers its candidates and a `Fetch` of every one follows in the same `engine.walkAllowed`, so an unconditional removal puts a round trip per candidate into the widest fan-out Aperture has — the super-linear term `APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/` (`TestCheckNFREnumerateBound`) exists to catch. The promise is OPT-IN and the default is no warm; `Static` and `csvprovider` promise unconditionally (one map per object, all three methods), `sqlprovider` DERIVES it from `rows.Columns()` of each statement — the projection, so no row's NULLs can fool it — and answers false until both have run once, which costs one cold enumeration per process per type. **No gate is possible and none should be faked**: the registry cannot compare bags, because one object's agreeing proves nothing about the next object's and comparing per object costs the very `Fetch` the warm avoids |
| Attribute cache INVALIDATION (`provider.AttributeRegistry.Invalidate` / `InvalidateSlot` / `InvalidateAll`, or the three `service.Invalidate*Attribute*` facade methods and their gate) | `skills/attribute-providers.md` ("Staleness is a security window, not a tuning knob" **and** "The administrative read"), `docs/src/cli/attributes.md` ("`invalidate`"), `docs/src/library/service-facade.md` ("The attribute directory"), the methods' doc comments, and the `aperture attributes invalidate` description — **plus `make docs-gen`** when the CLI text moves | reviewed; no registry gate — behaviour by `provider/attribute_invalidate_test.go`, `service/attributes_invalidate_test.go` and `internal/cli/attributes_cli_test.go`. **A slot's `ttl:` is the window a REVOKED clearance keeps authorizing for**, so these are a security control and not a performance knob; documenting them as tuning is the actual regression |
| The seed `attribute_providers:` schema (a `seed.AttributeProvider` YAML key, the `kind:` set in `attributeKinds()`, or the per-slot `ttl:` / `max_size:` handling) or the precedence reporters (`Document.AttributeSlotSources` / `AttributeSourceInline` / `AttributeCollisions`) | the field's doc comment, `skills/attribute-providers.md` ("From a seed document", "Precedence: the external source wins, entirely"), `docs/src/concepts/seed.md` ("External attribute sources", including "Precedence: the external source wins, entirely"), and the `attributes slots` output in `internal/cli/attributes.go` — the CLI reads the precedence rule from `AttributeSlotSources` and must never re-derive it | reviewed; no registry gate — behaviour by `seed/attribute_provider_test.go`, `seed/attribute_provider_csv_test.go`, `seed/attribute_provider_sql_test.go`. **The external entry wins and the inline bags for that slot are discarded ENTIRELY** — no per-subject merge, no fallback — and the discard is reported, never silent |
| The attribute LOADERS' bare-id contract (`csvprovider.Attributes`' id column, or `sqlprovider.AttributeConfig`'s `FetchQuery` binding the key verbatim / `ListQuery` selecting a bare id / `ListQuery` being optional) | ALL THREE statements of it move together — the `csvprovider/attributes.go` file doc, the `sqlprovider/attributes.go` file doc, and `seed.AttributeProvider.GetAll` — plus "The `get_all` bare-id contract" in `skills/attribute-providers.md`, "The attribute seam" in `skills/sql-provider.md`, "The attribute variant" under each loader in `skills/metadata-values.md`, "The bare-id contract" in `docs/src/concepts/seed.md`, and `docs/src/concepts/providers.md` ("Attribute providers") | reviewed; no registry gate, and **there cannot be one**: an identity-shaped key (`'user:' \|\| u.id AS id`) is a legal opaque string that enumerates, caches, and then matches no id any `Fetch` presents. The slot silently never answers. That is precisely why it is written down in three places instead of tested in none |
| The SQL driver-value mapping table (`metadataValue`'s type switch **or** `mappedDriverTypes` in `sqlprovider/values.go`) | the other half of the pair in the same file, the `sqlprovider` package doc ("Driver values become metadata"), `skills/sql-provider.md`, `skills/metadata-values.md` ("`sqlprovider`"), and `docs/src/concepts/providers.md` | `TestDriverValueMappingTableMatchesTheTypeSwitch` — parses `sqlprovider/values.go` with `go/ast` and diffs the type switch against `mappedDriverTypes`; adding a case to one and not the other is build-red |
| The seed `connections:` / `kind: sql` schema (a `Connection` or `Provider` YAML key in `seed/`) or one of its defaults (`DefaultQueryTimeout` / `DefaultMaxOpenConns` / `DefaultMaxIdleConns` / `DefaultConnMaxLifetime` in `seed/connection.go`) | the field's doc comment, the defaults table in `skills/sql-provider.md`, and "Database-backed providers" in `docs/src/concepts/seed.md` | reviewed; no registry gate — behaviour by `seed/connection_test.go` (defaults, pool sharing, the `dsn:` refusal, `BuildRegistry`'s refusal of `connections:`) |
| The SQL provider's statement contract (the four casting rules, what `Fetch` binds, the id column, `Querier`, `sqlprovider.Config`) | the `sqlprovider` package doc, `skills/sql-provider.md`, `docs/src/concepts/providers.md` ("Worked example: `sqlprovider`"), and the `APERTURE_SQL_PROVIDER_*` fixups in `errors/codes.go` | `TestEverySkillHasFrontmatter` (doc presence); behaviour by `sqlprovider/*_test.go` — the casting rules themselves are un-gatable, which is exactly why they must be written down |
| How the rule editor displays a date (anything in `internal/server/static/js/rules.js` or `rules-serializer.js` that renders a stored date, a resolved bound, or the reference instant) | "Reading a saved rule back" + "The date diagnostics" in `skills/ui-shell.md` | `TestRuleEditorNeverFormatsADateThroughADateObject` — scans the served JS and fails on `new Date` / `toLocale*String` / `Intl.DateTimeFormat` and friends |
| The rule what-if preview's response fields (`EvaluateRuleResponse` in `service.proto`, `service.RulePreview`) | `skills/ui-shell.md` ("The date diagnostics"), `skills/api-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/library/service-facade.md` — **and `make proto`** | reviewed; no registry gate |
| The enumerate filter input (`engine.EnumerateRequest.Fields` / `engine.MetadataFetcher`) | ALL of: `service.EnumerateQuery` + its `request()` converter (keep the `json:"...,omitempty"` + `jsonschema` tags — `mcp.EnumerateIn` aliases the struct and drops the REQUIRED marking through `omitempty`); `EnumerateRequest.fields` in `service.proto` **and `make proto`**; `internal/wire/rpc/fields.go` (`FieldsFromWire`/`FieldsToWire`); `internal/server/twirp.go` (`enumerateQuery`, single **and** batch); `internal/cli/fields.go` + the `enumerate` command description; then `skills/api-surface.md`, `skills/decision-api.md`, `mcp/skills/mcp-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/cli/decisions.md` — **and `make docs-gen`** (the CLI flags/description are a generated page) | reviewed; no registry gate — behaviour by `engine/enumerate_fields_test.go`, `service/enumerate_fields_test.go`, `internal/wire/rpc/fields_test.go` (incl. `TestFieldsRoundTrip_LargeIntegerLosesPrecision`), `internal/server/enumerate_fields_test.go`, `internal/cli/{fields,enumerate_filter}_test.go`, `mcp/enumerate_fields_test.go`; `TestEnumerateHelpStatesThePrecedence` pins the help text |
| The `Filter.Fields` matching semantics (`provider/match.go` `MatchFields` / `ValuesEqual`) | `docs/src/concepts/providers.md` ("The `Filter.Fields` contract") **and** every restatement of it on the enumerate filter: `skills/api-surface.md`, `skills/decision-api.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/cli/decisions.md` | reviewed; no registry gate — one definition, one implementation: a provider that filters differently authorizes differently |
| The `references:` schema (`seed.Provider.References`, `Registry.DeclareReference` / `ReferenceTarget` / `ResolveReference*` in `provider/reference.go`, or what a declaration may carry) | the field's doc comment, `skills/object-references.md` ("Declaring a reference"), `docs/src/concepts/seed.md` ("Declaring a reference"), and `docs/src/concepts/providers.md` ("Declared references") — a **new descriptor kind** (e.g. a `type:` key) needs its "three closed doors" rationale rewritten, not appended to | reviewed; no registry gate — behaviour by `provider/reference_test.go`, `seed/reference_test.go` (incl. `TestReferenceWiringIsNotModelState`) |
| The via-reference enumerate input (`engine.EnumerateRequest.References` / `engine.ReferenceEdge` / `engine.ReferenceSource` / `WithReferences`) | ALL of: `service.EnumerateQuery.References` + `service.ReferenceEdge` + the `references()` converter (keep the `json:"...,omitempty"` + `jsonschema` tags — `mcp.EnumerateIn` aliases the struct and `omitempty` is what keeps the edges OPTIONAL); `EnumerateRequest.references` + `message ReferenceEdge` in `service.proto` **and `make proto`**; `internal/server/twirp.go` (`enumerateQuery` + `referenceEdges`, single **and** batch); `internal/cli/references.go` + the `enumerate` command description; then `skills/object-references.md`, `skills/api-surface.md`, `skills/decision-api.md`, `mcp/skills/mcp-surface.md`, `docs/src/surfaces/rpc-reference.md`, `docs/src/surfaces/mcp.md`, `docs/src/library/decision-api.md`, `docs/src/library/service-facade.md`, `docs/src/cli/decisions.md` — **and `make docs-gen`** (the CLI flags/description are a generated page) | reviewed; no registry gate — behaviour by `engine/enumerate_reference_test.go` (incl. `TestTheFourQuestions`), `service/`, `internal/server/`, `internal/cli/`, `mcp/enumerate_references_test.go`. **The empty-vs-`NOT_FOUND` split is asserted PER SURFACE on purpose:** relaxing it in one place is a silent disclosure channel, so a change there must move all five test files together |
| The configured enumeration bound (`engine.WithEnumerateLimit` / `DefaultEnumerateLimit`, the unconditional `Deps.MaxMembers` stamp in `engine.stampScopeBound`, `scope.Deps.MaxMembers` / `provider.DefaultListLimit` no longer clamping DOWN) or the on-bound warning (`engine.warnAtEnumerateBound`) | `skills/decision-api.md` ("Enumerate is bounded" and its four subsections — the one full account), `docs/src/library/decision-api.md` (the `WithEnumerateLimit` and `WithLogger` option rows **and** "A result on the bound is a warning"), the `Limit` clause in `skills/api-surface.md`'s decisions bullet, `docs/src/concepts/scopes.md` + `docs/src/internals/extending.md` (both say `Members` gathers against `Deps.MaxMembers`, not the constant), `docs/src/concepts/providers.md` + `skills/attribute-providers.md` (both say a positive `List` limit is honoured verbatim and `AttributeRegistry.Enumerate` is uncapped), `docs/src/operations/performance.md`, `docs/src/operations/deployment.md`, `docs/src/cli/serve.md`, `docs/src/cli/global-options.md`, `docs/src/cli/decisions.md` — **and `make docs-gen`** when the flag or a command description moves | `TestTheLibraryNormalisesWhatTheBoundaryRefuses` (`internal/cli/enumerate_limit_contract_test.go`) gates the divergence as ONE decision; the rest is reviewed, with behaviour by `engine/enumerate_limit_test.go`, `engine/enumerate_bound_e2e_test.go`, `engine/enumerate_bound_warning_test.go`, `scope/max_members_test.go`, `internal/cli/enumerate_limit_test.go`, `bench/enumerate_test.go`. **The library NORMALISES a non-positive bound to the default and the CLI REFUSES one**, deliberately — and that divergence is written in three places (`skills/decision-api.md`, `docs/src/cli/serve.md`, the `n <= 0` comment in `internal/cli/enumerate_limit.go`), which the gate names in its failure text. It asserts BOTH halves from the same two values (`0` and `-5`), because the per-side tests stay green when the other side changes: drop the normalisation and the CLI tests still pass; drop the refusal and the engine tests still pass, with all three documents silently wrong. It lives in `internal/cli` because only that package can see both — `internal/cli` already imports `engine`, and the mirror placement would be an import cycle AND a public package depending on an internal one, so **do not move it into `engine/`**. **The warning is a hint, not an assertion** — a complete set of exactly that size is indistinguishable, `Enumerate` still returns `([]string, error)`, and restating it as proof of truncation is itself the regression |
| The persisted timestamp encoding (`storage/storagetime`: the int64-nanosecond unit, the `0` unset sentinel, the representable window, `Encode`/`Decode`/`Validate`) | the "Time" header section in **both** `storage/sqlite/schema.sql` and `storage/postgres/schema.sql`, `skills/storage-schema.md`, and `docs/src/concepts/storage.md` | `TestStorageTimeIsTheOnlyTimeIntegerConversion` (no `Unix*` conversion under `storage/` outside `storagetime`) + `storage/storagetest` per backend; the prose itself is reviewed |
| A stamped model entity (a new `CreatedAt`/`UpdatedAt` pair on a `model.Storage` entity) | `stampedEntities()` in `storage/storagetest/storagetest.go` — it is the suite's definition of "every stamped entity", so an entity missing from it has its timestamp cases **silently skipped** — plus the entity list in `skills/storage-schema.md` | reviewed; no registry gate — this is the one that fails by passing |
| The foreign-key edge set or any edge's `ON DELETE` / `ON UPDATE` action | **both** `storage/sqlite/schema.sql` and `storage/postgres/schema.sql` (including the comment stating the reason), the edge table in `skills/storage-schema.md`, `docs/src/concepts/storage.md`, and — if the edge cannot be expressed in SQL — `storage/sqlite/integrity.go`, `storage/postgres/integrity.go` and `storage/memory` **with the refusal wording verbatim**, since `storagetest` asserts the text | `TestDialectSchemasDeclareTheSameForeignKeys` (changing one dialect only is build-red), `storage/sqlite/foreign_keys_test.go` (actions read back via `PRAGMA foreign_key_list`), `storage/storagetest` per backend |
| A column on one of the five `apt_wiring_*` tables (shared wiring: connections, providers, provider references, field types, attribute providers) | **both** `storage/sqlite/schema.sql` and `storage/postgres/schema.sql`, "The five shared-wiring tables" in `skills/storage-schema.md`, and the same section in `docs/src/concepts/storage.md` — plus `expectedTables` in `storage/postgres/postgres.go` and the two `tableFloor`s in `internal/schemagate/schema_naming_test.go` for a new TABLE | `TestDialectSchemasDeclareTheSameTables` / `...Columns`, `TestExpectedTablesMatchTheSchema`, `TestSchemaUsesNoReservedIdentifiers`; the rest is reviewed. **What may NOT be a column there is the contract**: no DSN, no credential, not even the `dsn_env:` variable NAME, and no filesystem `path` — a wiring row is copied to a second instance that resolves its own credentials and has its own filesystem, so a secret column would put one in every backup and a path column would be a guess about the other host. A `ttl` is the operator's duration TEXT (`"30s"`) and never an integer, or a read back stops being re-pushable byte for byte. `declared_keys` distinguishes `''` (NOT declared) from `'[]'` (declared EMPTY); collapsing them silently opts a slot out of the enforcement it asked for |
| The shared-wiring READ/WRITE surface (`model/wiring.go`'s types and `Validate*`, `model.DeclaredKeys`, or a wiring method on `model.Storage` — `ReplaceWiring` / `GetWiring` / the per-section `ListWiring*` / the per-entity `GetWiring*`) | ALL FOUR implementors (`storage/sqlite`, `storage/postgres`, `storage/memory`, and `service.overlayStore` in `service/simulate.go`), `stampedEntities()` in `storage/storagetest/storagetest.go` if the entity is stamped, the wiring cases in `storage/storagetest`, "The wiring read and write surface" in `skills/storage-schema.md`, and the same section in `docs/src/concepts/storage.md` | `storage/postgres/gate_test.go`'s `requiredConformanceCases` is a FLOOR (removing a case is build-red), and the `var _ model.Storage` assertions make a missing implementor build-red; the rest is reviewed, with behaviour by the six `Wiring*` conformance cases plus the wiring rows in `ReferentialWriteRefusesAnUnknownParent`, `...RestrictRefusesADeleteThatWouldOrphan` and `...CascadeRemovesTheJoinRowsWithTheirOwner`. **`ReplaceWiring` being ALL-OR-NOTHING and every read returning CANONICAL ORDER are the contract, not the implementation** — the push surface depends on the first (a set written half-way builds a registry missing exactly the entries whose write failed, and reports nothing) and a re-pushable read back depends on the second. **There is deliberately NO per-row `Put`/`Delete`**: adding one makes "the wiring set" something a caller can leave half-applied. And **`model.DeclaredKeys` stays a struct with an explicit `Declared` bit** — a nil-versus-empty slice expresses `''`-vs-`'[]'` and loses it silently through `cloneStrings` or `encoding/json`, which opts a slot out of the enforcement it asked for |
| `physicalTypes` / `refusedTypes` (`internal/schemagate/schema_parity_test.go`) | the affected `schema.sql` header's divergence section and "The two dialects" in `skills/storage-schema.md` — editing this table changes what "the dialects agree" **means**, so it is a contract change, not a refactor | `TestEveryDialectHasATypeMapping`, `TestTheTypeMappingRefusesTheNarrowingSpellings` |
| Which principal a rule is told about (`engine.effectivePrincipal`, `elevatedSubjects` in `engine/impersonation.go`) — the effective subject is the TARGET under `become` and the OPERATOR under `augment` | `skills/rules-engine.md` (the `principal` root), `docs/src/library/impersonation.md` ("Under `become`, `principal.*` is the target"), `docs/src/concepts/impersonation.md` ("Two modes"), and a release note — `principal.id` changing meaning is invisible to every compiler and every test a host owns | reviewed; no registry gate — behaviour by `engine/impersonation_attributes_test.go` and `TestImpersonationTellsTheRuleAboutTheEffectiveSubject`. The audit half (`Decision.Impersonation` / `Trace.Request.Principal` stay the operator) moves together with it or the change becomes a loss of accountability |
| The attribute-directory read (`service.ListAttributes` / `ExplainAttributeAuthority` / `WithAttributes` in `service/attributes.go`) — the tier it requires, the order the wiring/auth/authority checks run in, or what a refusal carries | `skills/api-surface.md` ("The attribute directory read" **and** the facade surface list), "The administrative read" in `skills/attribute-providers.md`, and `docs/src/library/service-facade.md` ("The attribute directory — a system-tier read", including the `WithAttributes` row in the options table) | reviewed; no registry gate — behaviour by `service/attributes_test.go`. **The check ORDER is the contract**: the gate runs before the slot is resolved, so a non-admin's refusal is identical for a populated, an unregistered, and a nonexistent slot. Moving the slot parse in front of it turns the refusal into a probe, and `TestARefusalDisclosesNothingAboutTheSlot` is what catches that |
| The `aperture attributes` command tree (`internal/cli/attributes.go`: a subcommand, a flag, or which of the three is gated) | `skills/attribute-providers.md` ("The operator surface") and `docs/src/cli/attributes.md` (the narrative page, plus its row in `docs/src/cli/overview.md` and its `SUMMARY.md` entry) — **and `make docs-gen`**, since `docs/src/reference/cli.md` is generated from the flags and descriptions | reviewed; no registry gate — behaviour by `internal/cli/attributes_cli_test.go` and `internal/cli/attributes_csv_test.go`. **`slots` is ungated on purpose and the other two are not**: `slots` restates the seed file the operator just passed in, while `query` and `invalidate` go through `service.requireAttributeAdmin`. Gating `slots` would mean nobody could diagnose "is the user slot even wired?" without already holding the authority the diagnosis explains |
| What a decision trace DISCLOSES about attributes (`engine.TraceAttributes`, `traceAttributes`, or `Trace.String()`'s attribute lines) | `skills/decision-api.md` (the `Attributes` field + the shape-and-path-only note rule it is the named exception to), "The administrative read" in `skills/attribute-providers.md`, `docs/src/library/decision-api.md` ("The attribute bags"), `docs/src/library/service-facade.md`, **and both MCP statements of it** — `docs/src/surfaces/mcp.md` and `mcp/skills/mcp-surface.md`, which disclose it through the `ExplainOut` / `SimulateOut` = `engine.Trace` ALIAS with no `mcp/` code of their own — plus `docs/src/surfaces/rpc-reference.md` if the wire shape moves | reviewed; no registry gate — behaviour by `engine/attribute_trace_test.go`. **This field carries VALUES on purpose** and is the one place a `Trace` does: the two bags are the subjects of the request being explained, so it discloses the asker's own decision and nothing else. Do not redact it into a `Note`, do not widen it past those two subjects, and keep `service.EvaluateRulePreview` supplying neither — a rule editor must not become a directory read oracle |
| Whether an attribute fetch key can carry an ACCOUNT (`provider.AttributeRegistry.Fetch`'s signature, or anything that would key a principal bag per account) | "Account neutrality: a principal bag is global" in `skills/attribute-providers.md` | reviewed; **no gate is possible** — a principal is global (`model.Principal` has no account; `model.Membership` binds it to several), so one principal bag is visible to rules in every account that principal belongs to, and the host obligation to keep principal attributes account-neutral is opaque host data Aperture cannot inspect. The bounded slot is `account`, whose containment IS proven (`engine/account_boundary_test.go`) |
| The object-SEARCH surface — `engine.SearchRequest` / `SearchResult` / `Search` / `SearchAs` / `SearchBatch`, `service.SearchQuery` / `SearchMatch` / `Search` / `SearchBatch`, or the CLI `search` / MCP `aperture_search` translations | ALL of: `skills/object-search.md` (the ONE full account), the four-op list in `skills/decision-api.md`, the facade surface list + "The object search" in `skills/api-surface.md`, `mcp/skills/mcp-surface.md`, `docs/src/library/decision-api.md` ("Search" + the when-to-use table), `docs/src/library/service-facade.md`, `docs/src/surfaces/mcp.md`, `docs/src/cli/decisions.md` + its row in `docs/src/cli/overview.md`, `docs/src/surfaces/rpc-reference.md` — **and `make docs-gen`** when a CLI flag or description moves, **and `make proto`** when the proto messages move | `TestCatalogMatchesToolmeta` + `TestMCPExposesNoMutationHandler` gate the MCP half (a tool with no registered schema is silently dropped from the catalog, and a handler whose name is not a known READ shape is build-red), and the generated `rpc.ApertureService` interface gates the Twirp half (a new rpc that nothing implements is build-red); the rest is reviewed, with behaviour by `engine/search_test.go`, `service/search_test.go`, `internal/cli/search_test.go`, `internal/server/search_test.go`, `mcp/search_test.go`. **`Search` ⊆ `Enumerate` is asserted in FOUR packages on purpose** — it is the property every other one rests on, and a relaxation in one surface is a silent disclosure channel, exactly like the reference edge's empty-vs-`NOT_FOUND` split. **Decide-then-match is the whole design**: `Search` walks `engine.walkAllowed`, the same pipeline `Enumerate` walks, so a score can only ever SUBTRACT. Scoring a candidate before deciding it, or filtering after the walk instead of inside it, reinstates the bare-enumeration shape the surface exists to remove |
| `engine.walkAllowed` (`engine/enumerate.go`) — the shared candidate walk, or which of gather / decide / reference-restrict / `Fields` it performs | both callers move with it (`enumerateWithSubjects` and `searchWithSubjects`) plus the Enumerate and Search sections of `skills/decision-api.md` | reviewed; no registry gate — behaviour by the whole of `engine/enumerate*_test.go` and `engine/search_test.go`. **It exists so "which objects may this subject act on?" has ONE implementation.** Giving Search its own walk would compile, pass its own tests, and be a second answer to the entitlement question |
| The TEXT-MATCH model (`provider/search.go`: `MatchText`'s searched shapes, `ScoreText`'s tiers, `NormalizeText`) | `skills/object-search.md` ("What is matched", "The scorer"), "Searching the model: `MatchText`" in `skills/metadata-values.md`, and "The text-match contract" in `docs/src/concepts/providers.md` | `TestProviderPackageImportsOnlyIdentityAndErrors` keeps it a strict leaf; the model itself is reviewed, with behaviour by `provider/search_test.go`. **It is exported for the same reason `MatchFields` is**: a host that pushes a search into its own storage must rank the way Aperture would, or the ids it proposes and the ids Aperture would have proposed are two answers to one question. The tests pin ORDER and BOUNDARIES, not exact scores — a tuning change that improves ranking must not be a red build |
| A scorer tuning constant (`provider/search.go`: `DefaultMinScore`, `fuzzyFloor`, `fuzzyWeight`, `minFuzzyLen`, `coverageFloor`) | the constant's own comment and "The scorer" / "`MinScore` is a floor, not a knob" in `skills/object-search.md` | `TestTheDefaultFloorSitsUnderTheWeakestFuzzyMatch` gates the one relationship that must hold — the default floor below the weakest admissible fuzzy score, or every approximate match is silently filtered out and the typo tolerance is dead code. **`MinScore` is not a performance knob**: with no floor a fuzzy matcher ranks and returns the subject's ENTIRE entitled set, which is the bulk disclosure a ranked search exists to make unnecessary |
| The batch object-metadata read (`service.ObjectMetadataBatch`) | "The batch metadata read" in `skills/object-search.md`, the facade surface list in `skills/api-surface.md`, and `docs/src/library/service-facade.md` | reviewed; no registry gate — behaviour by `service/search_test.go`. **It labels ids a caller already holds; it never discovers them.** Documenting it as a way to list objects would make the unscoped-then-filter shape look sanctioned |
| The DECLARED-KEY enforcement — `rules.DeclaredAttributeKeys` / `DeclaredKeySet` / `CheckDeclaredAttributeKeys` / `ValidateASTDeclaring` (`rules/declared.go`, `rules/validate.go`), `rules.walkVarFields`, the slot→root collapse in `service.WithDeclaredAttributeKeys` / `declaredAttributeKeys` (`service/declared_keys.go`), where the gate is applied (`service.validateRule`, `service.EvaluateRulePreview`), or where a booting instance collects the sets (`internal/cli/declaredAttributeKeySets`) | ALL of: `skills/attribute-providers.md` ("What enforcement actually refuses" **and** "The floor sits above the declared set, and the `principal` root needs both slots" — the ONE full account), "Declared attribute keys" in `skills/rules-engine.md` (plus its `APERTURE_RULE_UNDECLARED_ATTRIBUTE` bullet in the validation list), "What a declaring slot refuses" in `docs/src/concepts/seed.md`, the `WithDeclaredAttributeKeys` row in `docs/src/library/service-facade.md`, the `APERTURE_RULE_UNDECLARED_ATTRIBUTE` fixups in `errors/codes.go`, and the `codeToTwirp` row below (it is a 400, not the `internal` default) | reviewed; no registry gate — behaviour by `rules/declared_test.go`, `service/declared_keys_test.go` and `internal/cli/declared_keys_test.go`. **Opt-in is the non-negotiable half**: a slot that declares nothing must refuse NOTHING, or every rule in every deployment that has not opted in breaks at once — which is why `ValidateAST` passes a zero value and `TestAFacadeThatDeclaresNothingValidatesEveryRuleItUsedTo` asserts it at the surface. **The floor is not part of any declared set** (`principal.id` / `principal.kind` / `account.id` are stamped LAST and are present with no provider wired), so refusing `principal.id == object.owner` over a set that only named `department` is the regression the floor test exists to catch. **The `principal` root needs BOTH slots and unions them**: `principal.*` resolves per KIND, which validation cannot know — one declaring slot treated as enough silently enforces the other, and intersecting refuses the documented kind-guarded rule. **Enforcement is DEFINITION-TIME only**: moving it onto a decision lets a bad rule ship and then deny (or stop excluding) on some instances and not others, which is the divergence it exists to remove. And **"what does this rule read" stays ONE walk** — `walkVarFields` serves `readsBeyondFloor` too, so a note that fires and a refusal that does not cannot come apart over the same expression |
| `codeToTwirp` (`internal/server/twirp.go`) — adding a code, or moving one between Twirp codes | the code→status table in `docs/src/surfaces/rpc-overview.md`, plus its "why not 500" prose when the mapping is not the default | reviewed; no registry gate — the absence of this row is why the table was already stale for `APERTURE_ENTITY_UNMANAGED` |
| What an omitted `--seed` means (`internal/cli/store.go`: `loadSeed`, or `classifyStore` / `storeKind.durable` — the ONE place a `--store` DSN is classified) | every restatement of the default moves together: the `--seed` flag `Usage` on all eight commands that declare it (`check.go` ×4, `serve.go`, `mcp.go`, `search.go`, `mutate.go`), `docs/src/cli/global-options.md` ("What an omitted `--seed` means"), `docs/src/cli/serve.md` ("Booting against a database"), `docs/src/operations/deployment.md` (the flags table **and** the durable-store paragraph), `docs/src/surfaces/mcp.md`, `docs/src/getting-started/first-decision-cli.md`, `docs/src/cli/overview.md` ("The embedded example model"), and `skills/check-surface.md` — **plus `make docs-gen`**, since the flag text is a generated page | reviewed; no registry gate — behaviour by `internal/cli/store_seed_test.go` (`TestADurableStoreWithNoSeedSeedsNothing`), which asserts BOTH halves because each stays green when the other breaks. **An omitted `--seed` means two different things, deliberately**: the embedded `acme` fixture for the in-memory store, and NOTHING for a durable one. `seed.Document.Apply` upserts the entire model outside the `ManagedEntities` posture, so defaulting to the fixture everywhere wrote the demo model into any database `--store` named and let two instances re-assert their model over each other on every restart. Widening it back is silent data loss; narrowing it to "never seed" takes away the zero-flag demo that the getting-started pages, `seed.ExampleAccount` as the default `--account`, and `cmd/aperture`'s end-to-end test all rest on. Classifying durability anywhere but `classifyStore` is a second answer that can drift from the backend actually opened |
| Where a booting instance's runtime WIRING comes from (`internal/cli/store.go`'s `readSharedWiring` and `seedDocument`'s durable branch, `internal/cli/decision.go`'s choice between the local and the projected document, or `internal/cli/wiring_boot.go`'s `wiringDocument` / `connectionRoutes` / `connectionDSNEnvVar`) | `docs/src/cli/global-options.md` ("It means the same thing for *wiring*", which states the three connection ROUTES and the `APERTURE_CONNECTION_<NAME>_DSN` spelling), `docs/src/cli/serve.md` ("Where its wiring comes from"), and the row above — an omitted `--seed` now means the same thing for wiring as it does for model state, so the two move together | reviewed; no registry gate — behaviour by `internal/cli/wiring_boot_test.go`. **Empty wiring tables must keep meaning "use the local file"**: an empty `model.WiringSet` is an ANSWER (`IsEmpty`), not a failure, and every deployment that has never run `aperture wiring push` is in that branch — turning it into a refusal, or adding a flag to select it, breaks every existing single-instance deployment at once. **The projection goes back onto a `seed.Document` on purpose**: a parallel builder over `model.Wiring*` would restate the statement contract, the kind vocabulary, the ttl parsing, the reference-target check, the field-type words and the slot set, and a divergence between the two does not error — it authorizes differently on two instances that were supposed to be identical. **The shared row carries the connection NAME and never a route**; `seed.WithConnectionOpener` stays the only seam a Go host needs, and the derived environment variable is the CLI's route of last resort, not a second mechanism |
| The MERGE AUTHORITY between database wiring and local wiring (`internal/cli/wiring_boot.go`'s `layerProviders` / `layerFieldTypes` / `layerAttributeProviders` / `localCollision` / `wiringBuildOptions`, or the `seed.BuildOption` set `internal/cli/decision.go` passes on the DB-wired path) | "With both, the database wins and the file may only ADD" in `docs/src/cli/global-options.md`, the same paragraph in `docs/src/cli/serve.md` ("Where its wiring comes from"), and the `APERTURE_WIRING_LOCAL_COLLISION` fixups in `errors/codes.go` | reviewed; no registry gate — behaviour by `internal/cli/wiring_layer_test.go` (DB-only, local-only, disjoint, all four colliding axes, the inline-`objects:` axis, and the Go registration). **ADDITIVE is what makes the surface usable at all**: Arc's `wave` and `metric` providers are hand-written Go and a `kind: csv` provider is refused at the push, so "the database is the only source" would mean those hosts could never read shared wiring. **A collision is refused in BOTH directions, never resolved** — either precedence is silent and either one changes what a decision reads, surfacing as a different verdict on an identically-configured-looking instance rather than as an error. **The rule is about the REGISTRY, not the source syntax**: a Go `Register` call passes through no document, so its collision is refused by `provider.Registry.Register` / `AttributeRegistry.Register`'s own duplicate check (`APERTURE_PROVIDER_INVALID` / `APERTURE_ATTRIBUTE_PROVIDER_INVALID`) — one arbiter, two messages, and do NOT add a projection-side check that tries to anticipate a `Register` that has not happened yet. `seed.StrictProviderCollision()` is REUSED for the `objects:`-vs-shared-`providers:` axis and is passed on the DB-wired path only; the file-only path keeps the documented silent discard, because adding a `providers:` row while inline entries are still in ONE author's file is an ordinary migration step |
| The WIRING POLL (`internal/cli/wiring_poll.go`: `wiringPollFlag` / `wiringPollInterval`'s accepted vocabulary, `defaultWiringPollInterval`, `wiringDigest` / `wiringWithoutStamps`, or `wiringPoll.tick`) or the boot baseline it compares against (`decisionStack.wiringDigest`) | the constant's own comment, `docs/src/cli/serve.md` ("Noticing a push without a restart" — the ONE full account, including the value table, the interval reasoning and what a tick costs) and the pointer to it in `docs/src/cli/global-options.md` ("The wiring is read once, unless you ask for more") — **and `make docs-gen`** when the flag text moves | reviewed; no registry gate — behaviour by `internal/cli/wiring_poll_test.go`, plus the two gated live-Postgres cases (a change and the cross-dialect digest). **OFF must stay the default and is asserted by COUNTING STORE READS, not by reading the configuration back**: a loop that read five tables on a timer and discarded the answer would satisfy every configuration assertion while charging every single-instance deployment for a feature it never asked for. **The digest's two halves fail in opposite directions**: the CONTENT half marshals by reflection so a wiring column added later is covered without anyone remembering — a content field outside the digest is a change no instance ever sees, which is a silently stale instance reporting itself healthy — while the STAMP half is an explicit list, because a stamp left IN only costs an identical re-push a needless rebuild. `TestTheDigestCoversEveryContentFieldAndNoStamp` walks the model by reflection and **fails** on a field it has not been taught to vary rather than skipping it. **A cheaper PROBE is not available and must not be invented**: there is no stamp-only read, and a newest-timestamp probe would trust the pushing host's clock (a lagging one writes rows older than the ones it replaced and the probe says "unchanged"), a row-count probe misses a re-pointed statement, and four per-section reads can compose a set that never existed. The baseline is the BOOT's digest and never the loop's own first read, or a push landing between the two becomes the baseline and the change is never reported |

The Go↔JS rows matter more than they look: **CI is node-free**, so
`rules-serializer.test.js` never runs in the pipeline.
`rules/editor_js_contract_test.go` is what actually enforces parity — it reads
the JS file from disk and diffs the tables, and it **fails** rather than skips if
that file moves.

As the remaining surfaces land (identity, model, engine, scope, account, auth,
audit, mcp), each story adds a `skills/<feature>.md` doc and a coverage gate in
`skills/skills_test.go` that walks the surface's registry, then a row here.

## Non-skippable CI gates

- `TestCodesHaveFixups` — every `APERTURE_*` code has a Registry entry with a
  Message and a Fixup (or `FixupNotApplicable`).
- `TestRegistryHasNoOrphans` — `Registry` contains nothing absent from `AllCodes`.
- `TestCodesAreScreamingSnakeNamespaced` — every code is SCREAMING_SNAKE and
  `APERTURE_`-prefixed.
- `TestUpdateDemandDocPresent` — the Update-Demand seed doc exists with
  frontmatter.
- `TestEverySkillHasFrontmatter` — every `skills/*.md` has a `name` (matching its
  file stem) and a `description`.
- `TestEditorOperatorTablesAgree` / `TestEditorVocabularyTablesAgree` /
  `TestEditorUnaryOperatorsAgree` / `TestEditorValidationMessagesAgree` — the Go
  rule AST and the JS serializer expose the same operators, operand shapes, node
  types, roots, functions, blocked builtins, and validation wording. Reads
  `rules-serializer.js` from disk; fails (never skips) if it is missing.
- `TestEditorASTContractCoversEveryOperator` — every operator in `opSpecs` has a
  byte-stable AST JSON case, so coverage cannot fall behind the registry.
- `TestDriverValueMappingTableMatchesTheTypeSwitch` — the driver-value mapping in
  `sqlprovider/values.go` is one table in two places (`metadataValue`'s type
  switch and `mappedDriverTypes`); the test parses the file with `go/ast` and
  fails if they disagree.
- `TestCollectionOperatorTablesAgree` — `collOps` (`rules/shape.go`) stays in
  lockstep with `opSpecs` (`rules/ast.go`).
- `TestDateOperatorTablesAgree` — `dateOps` (`rules/date.go`) stays in lockstep
  with `opSpecs` (`rules/ast.go`), membership and ternary arity alike.
- `TestRuleEditorNeverFormatsADateThroughADateObject` — the rule editor's served
  JS (`rules.js`, `rules-serializer.js`) constructs no JS `Date` and calls no
  locale date formatter, so a stored UTC date is never restated in the viewer's
  zone. Comments are stripped first, so documenting the hazard is still allowed.
- `TestAttributeRegistryIsNotAScopeLister` (`provider`) — an
  `*AttributeRegistry` must NOT satisfy `scope.ObjectLister`, asserted against the
  real interface with `*Registry` as the positive control. Go's typing is
  structural, so the four differences that make the signature unassignable
  (`Enumerate` not `List`, an `AttributeSlot` not a type string, an
  `AttributeFilter` carrying no `identity.Pattern`, `[]AttributeRecord` not
  `[]identity.Identity`) are the guarantee that a principal directory can never
  become an enumerable object set inside a decision. Attribute enumeration is a
  system-tier admin read and never a scope-resolution source.
- `TestProviderPackageImportsOnlyIdentityAndErrors` (`provider`) — parses every
  non-test file in the package and fails on any import outside `identity`,
  `errors` and the standard library. It is why a principal KIND arrives as a
  function argument rather than being resolved inside the registry, and why the
  attribute-directory gate lives on the facade rather than next to the thing it
  protects.
- `TestSchemaUsesNoReservedIdentifiers` (`internal/schemagate`) — the `apt_`
  database-identifier convention, per dialect. Parses each `schema.sql` with a
  real SQL tokenizer (never greps) and **fails**, never skips, if a file moved.
- `TestEveryDialectSchemaIsGoverned` — globs `storage/*/schema.sql` and fails in
  **both** directions, so a new backend's schema cannot arrive ungoverned and a
  registered path that vanished is caught.
- `TestDialectSchemasDeclareTheSameTables` /
  `TestDialectSchemasDeclareTheSameColumns` /
  `TestDialectSchemasDeclareTheSameForeignKeys` — the two hand-written schema
  files describe the same database: same tables, same columns per table, same
  foreign-key edges including each edge's `ON DELETE` / `ON UPDATE`. Symmetric —
  there is no reference dialect the other must match.
- `TestEveryDialectHasATypeMapping` / `TestTheTypeMappingRefusesTheNarrowingSpellings`
  — the dialects' legitimate type divergences are an **explicit** mapping
  (`physicalTypes` / `refusedTypes`), not a blanket exemption: an unmapped
  spelling fails, and Postgres `INTEGER` / `BOOLEAN` / `JSONB` / `TIMESTAMP*` are
  refused by name with a reason.
- `TestStorageTimeIsTheOnlyTimeIntegerConversion` — a `go/ast` scan over every
  non-test file under `storage/`, banning `Unix*` conversions outside
  `storage/storagetime`.
- `TestConformanceSuiteHasNoPrecisionKnob` / `TestConformanceSuiteIsBackendBlind`
  — `storage/storagetest` carries no tolerance/truncation knob and no
  backend-conditional assertion.

Gated, NOT in `make test` (a loaded runner would flake them):

- `APERTURE_BENCH_ASSERT=1 go test -run TestCheckNFR ./bench/` — cached `Check`
  p99 < 1ms and ≥ 10k checks/sec, including the collection and nested-access
  fixtures. Every case in the gate is NAMED `TestCheckNFR*` on purpose, because
  `-run` is an unanchored regexp and this is the only invocation anyone runs; a
  case reachable only by a second command will not be run. One of them,
  `TestCheckNFREnumerateBound`, asserts a **ratio** rather than a wall clock — a
  rule-backed `Enumerate` at a bound raised through `engine.WithEnumerateLimit`
  must cost no more than 1.5× the default bound's cost **per candidate** — so
  that it catches a super-linear term in the fan-out without flaking on a loaded
  runner. Do not convert it to an absolute ceiling: a bound-sized enumeration is
  milliseconds by design, and the justification for the number lives beside it in
  `bench/enumerate_test.go`. The wall-clock cases (`TestCheckNFR`,
  `TestCheckNFRCollections`, `TestCheckNFRAttributes`) cannot divide machine speed
  out that way — their targets are absolute, from the PRD — so they take the other
  robust measurement: `assertCheckNFR` **partitions** each case's sample budget
  (`nfrSamples`) into rounds (`nfrThroughputRounds` = 50, `nfrP99Rounds` = 10) and
  asserts against the **best** round. Rounds partition, never multiply — a gate
  run does the same total work — and the two counts differ because throughput is a
  rate (many short windows) and p99 is a percentile (fewer, larger ones, or the
  estimator goes noisy). **`p99Ceiling` (1 ms) and `throughputMin` (10 000) are
  not the knob:** loosening a threshold to stop a flake is the regression, not the
  fix. Measured A/B on one machine, same competing load held across both arms:
  at load average ~16–19 the contiguous measurement cleared the floor by 1.10×
  (10,980 checks/sec) where best-of-rounds cleared it by 1.31× (13,055), and the
  contiguous one *failed* at ~5,000–6,200 once load passed ~25. **It widens the
  margin; it does not confer immunity** — past roughly 10× core oversubscription
  every window is contended, there is no clean round to take the best of, and the
  gate fails either way. The cost is written down where the constants are set — an
  *intermittent* regression can now pass, a uniform one still fails — and the
  failure text says "that is the BEST of N rounds" so a red gate is never
  dismissed as one noisy second. Change either constant and
  `docs/benchmarks.md` ("Why best-of-rounds, and what it costs") plus
  `docs/src/operations/performance.md` move with it.
- `node internal/server/static/js/rules-serializer.test.js` — CI is node-free, so
  this is a manual development aid; the Go contract tests above are the real gate.
- `APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresIntegration ./seed/`
  — the host-provider (`connections:` / `kind: sql`) run against a real Postgres
  (CI has no service containers). It skips when ungated and **fails** when gated
  with an empty `APERTURE_PG_DSN`, so asking for it and silently not getting it
  cannot happen. Never put a DSN in a file; pass it in the environment.
- `APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./storage/postgres/`
  — the **only** proof that Aperture's own Postgres backend behaves: the whole
  `storage/storagetest` conformance suite against a real server, unqualified and
  schema-pinned, plus concurrency, hostile schema names, and a residue check.
  Same gate contract (skip ungated, fail on an empty DSN); an unrecognised value
  of `APERTURE_PG_INTEGRATION` also fails rather than skipping. `make test`
  cannot prove this backend behaves — only that it has not fallen behind its
  twin (the parity gates above).
- `APERTURE_PG_INTEGRATION=1 APERTURE_PG_DSN=<dsn> go test -run TestPostgresLive ./internal/cli/`
  — the `aperture wiring push → pull → push` **fixed point** against a real
  server, in a scratch schema dropped afterwards, plus the parity half: the two
  backends must pull the same wiring as the **same document**. The dialect-parity
  gates cannot reach that — they prove the two schemas describe the same database,
  not that a read of one renders the same bytes as a read of the other, and a pull
  that reordered a section or dropped a field on one backend only would pass every
  one of them and make a wiring diff report drift between two identically-wired
  deployments. `aperture wiring diff` is in the same suite, for the same reason and
  in both halves: clean against the document it was pushed from, and the **same
  report** from both backends for the same drift. It also carries the DB-wired
  BOOT: a row this instance cannot construct and a connection name it has no route
  for each refuse the boot, a **disjoint** boot builds both the database's
  providers/slots and the local file's, and a colliding local `providers:` or
  inline `objects:` entry is refused by name — the E2-S2 probe, on the backend the
  deployment it describes actually uses. And it carries the effort's **premise**:
  two decision stacks over ONE store — one wired from the database rows, one from
  the equivalent seed file — return identical `Check` / `Enumerate` / `Search` /
  `Explain` across a fixture set that reads object metadata, both attribute roots
  and a shared `field_types:` declaration, with `Search` ⊆ `Enumerate` re-asserted
  on each stack (`internal/cli/wiring_identical_test.go`; it also runs under `make
  test` against memory and SQLite). **The fixture set is the whole value there** —
  one that exercised no rule, no metadata read and no attribute read would pass on
  two stacks that were identically wrong and read as proof, so every fixture pins
  the verdict the model implies and says in a comment which projection bug it
  catches. The gate contract is exact: skip ungated, **FAIL** on an empty
  `APERTURE_PG_DSN`, and **FAIL** (never skip) on a value of
  `APERTURE_PG_INTEGRATION` that is neither on nor off — it is a pure function
  (`decideLiveGate` in `internal/cli/live_gate_test.go`, which owns the gate for
  the package) so all three outcomes are asserted in `make test` with no server.
  Same two variables as the suites above, so one exported DSN drives every live
  suite in one shell, and each suite creates and drops its own schema so a shared
  container is safe.

## House rules not derivable from the code

These are conventions a reader cannot infer by reading the repo, so they are
written down here rather than rediscovered.

### Commits

`<type>(<effort-slug>/<epic>-<story>): <sentence-case subject>`

```
feat(schema-time-and-keys/E4-S2): two processes can boot against one database
fix(schema-time-and-keys/E3-S1): every service and delegation delete path survives RESTRICT
test(schema-time-and-keys/E5-S2): a dialect cannot drift from its twin
docs(schema-time-and-keys/E6-S1): the schema, time and key contracts get a permanent home
```

Types in use: `feat`, `fix`, `test`, `docs`, `refactor`. When an epic closes, a
`milestone` commit records the slice:

```
milestone(schema-time-and-keys/E5): vertical slice complete — Guardrails that keep the dialects honest
```

Subjects are declarative sentences about what is now true, not imperatives about
what was done.

### Security non-negotiables

- **Account isolation is a hard line.** Account-scoped grants — bestowed or
  direct — must never leak across a principal that belongs to several accounts.
  The lone deliberate exception is `model.AccountWildcard`.
- **Authentication is always external.** Aperture consumes credentials and never
  issues them. `auth/` ships an OIDC/JWT verifier, a Parsec broker adapter, and a
  dev/static authenticator (bearer token = principal id, for local use only).
- **The MCP surface is read + decide + simulate only** — no mutations, ever.
- **No cross-account data in error messages.**

### Frontend

No node build pipeline in development or CI. Everything under
`internal/server/static/vendor/` is a committed pre-built blob, `//go:embed`-ed,
so the binary ships self-contained. Visual conventions — including the
load-bearing "AI-pink is reserved for AI affordances" and "no emoji" rules — are
in [`docs/src/contributing/design-system.md`](docs/src/contributing/design-system.md).

### Example domain

Fixtures, tests, docs, and demos use one generic hierarchy: `org → project →
document`, spelled `account:acme/project:atlas/document:42`.

## What NOT to do

- Don't put business logic in `cmd/aperture/`.
- Don't add a dependency on Pulse — the rules engine uses `expr-lang/expr`
  directly; keep `CGO_ENABLED=0` (no geo/h3 or other CGO packages).
- Don't return bare `errors.New`/`fmt.Errorf` across package boundaries — wrap in
  an `APERTURE_*` coded error.
- Don't commit `.planning/`.
- Don't leak cross-account data through error messages.
