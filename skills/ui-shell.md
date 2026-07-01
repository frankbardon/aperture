---
name: ui-shell
description: Aperture's embedded admin shell — a vendored static frontend (Alpine + BERA tokens) served from the site root, carrying the dev bearer on API calls. Chrome + plumbing only; domain screens land later.
applies_to: [frontend, http]
---

# UI shell

Aperture ships a single-page admin shell embedded in the binary. It is the
**chrome** — a sidebar + top bar + content frame — plus the domain screens that
fill it. The nav is Model (CRUD), Grants, Audit, What-if, Import / export, and
Rules; E7 mounts the Rete rule canvas in the Rules section and (E7-S3) wires it
into the live system — load / save / server-validate / live what-if. There is no
node build: the whole frontend is pre-built, committed blobs behind `//go:embed`.

## Where it lives

- `internal/server/static/` — the embedded tree (`//go:embed all:static` in
  `internal/server/static.go`). `index.html` is the shell; `css/bera.css` is the
  authoritative BERA styling; `js/app.js` is the shell Alpine component; per-screen
  components live alongside it (`js/crud.js` fills the Model section, E6-S2, with a
  data-driven entity-CRUD component driving the Twirp entity RPCs; `js/grants.js`
  fills the Grants section, E6-S3, with a three-tab provisioning component — raw
  grant management, template apply with a client-side preview + bulk provisioning,
  and delegation bestow/revoke — over the grant / template / delegation RPCs);
  `js/audit.js` fills the Audit section (E6-S4), a READ-ONLY queryable table over
  the append-only trail via the `QueryAudit` RPC (filter by actor, account, event
  type, outcome, and time window), showing both the real actor and the effective
  subject for impersonation/delegation events; `js/whatif.js` fills the What-if
  section (E6-S4), a READ-ONLY decision simulator that runs the open `Check` +
  `Explain` RPCs against the LIVE model and renders the verdict plus the full
  trace (expanded subject set, every grant considered with its action-match /
  coverage / specificity, and the deciding grants); `js/portability.js` fills the
  Import / export section (E6-S4), driving `Export` (download the declarative
  model file) and `Import` (upload → client-side preview DIFF of would-be
  adds/changes against a fresh export → confirm → transactional upsert), all three
  gated by the tier probe (audit read is system- OR account-admin; what-if is
  open; export/import is system-admin); `js/rules.js` fills the Rules section
  (E7-S2 node editor + E7-S3 integration): the Rete canvas over the
  `rules.Node` AST, plus a load picker (`ListRules`/`GetRule` → `fromAST`), a
  Save that persists the serialized AST via `PutRule` (SYSTEM tier, gated by the
  tier probe — non-admins load/validate/preview but cannot save), a server
  Validate (`ValidateRule`, non-persisting, surfacing `APERTURE_RULE_*` on the
  canvas), and a READ-ONLY live what-if that previews the UNSAVED rule on the
  canvas via `Simulate` / `SimulateExplain` with the edited rule as an overlay
  (persists nothing); `vendor/` holds the pre-built blobs (see `vendor/README.md`
  for pinned versions + regeneration).
- Served by `staticHandler()`, mounted **LAST** on the mux in `server.New`
  (`mux.Handle("/", …)`). net/http resolves by longest matching pattern, so the
  Twirp prefix, `POST /check`, and `GET /healthz` win over root `/` — the file
  server never shadows the API. Guarded by `internal/server/static_test.go`.

## Auth wiring (external credentials, never issued here)

Aperture consumes credentials; it never mints them. The shell carries a bearer on
every API request through one wrapper, `window.apiFetch` (`js/app.js`), which
adds `Authorization: Bearer <token>` and, on a `401`, clears the token and
re-opens the sign-in affordance. Sign-in / sign-out dispatch DOM events —
`aperture:authenticated` (`detail.principal`), `aperture:signout`, and
`aperture:unauthenticated` (on a 401) — so per-screen components (e.g.
`js/crud.js`) can (re)load or reset when the presented principal changes. For local/demo the **dev/static authenticator**
(`auth/dev.go`) treats the bearer AS the principal id, so "sign in" only names
which principal the session presents. An unauthenticated shell shows a sign-in
modal; there is no credential issuance UI. Later per-screen Alpine components
reuse `window.apiFetch` so the auth header lives in exactly one place.

## BERA compliance (load-bearing)

The shell obeys `.planning/access-control/research/bera-design-system.md`:

- **Named tokens only** — `css/bera.css` inlines the spec's `:root` block; no raw
  hex in component rules.
- **Sentence case** everywhere; the one uppercase exception is form-field labels.
- **No emoji.** Not in copy, empty states, or confirmations.
- **AI-pink (`#ff3399`) is reserved for AI affordances only** — never a primary
  action, alert, or decoration. It appears only on `.bera-ai-pill`.
- **IBM Plex Mono for identity strings / IDs** (`.bera-numeric` / mono stack);
  IBM Plex Sans body; BwGradual display. Fonts are not hotlinked — they fall back
  to the system stack per spec §12, so the binary stays self-contained.
- **Lucide icons**, stroke-only 1.5px, inlined as an SVG sprite (no icon CDN).
- **Layout §8** — fixed ~232px sidebar + ~52px top bar + flex content.

## Update-Demand

A change to the embedded shell's public surface — the static routes it exposes
(`/`, `/css/*`, `/js/*`, `/vendor/*`), the `apiFetch` bearer convention, the nav
skeleton, or a BERA rule above — updates this doc in the **same PR**. The gate in
`skills/skills_test.go` fails the build if this doc loses its frontmatter.
