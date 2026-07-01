---
name: ui-shell
description: Aperture's embedded admin shell — a vendored static frontend (Alpine + BERA tokens) served from the site root, carrying the dev bearer on API calls. Chrome + plumbing only; domain screens land later.
applies_to: [frontend, http]
---

# UI shell

Aperture ships a single-page admin shell embedded in the binary. It is the
**chrome** — a sidebar + top bar + content frame with a nav skeleton — that the
model (CRUD), grants, audit, and rules screens fill in later stories; E7 mounts
the Rete rule canvas in the rules section. There is no node build: the whole
frontend is pre-built, committed blobs behind `//go:embed`.

## Where it lives

- `internal/server/static/` — the embedded tree (`//go:embed all:static` in
  `internal/server/static.go`). `index.html` is the shell; `css/bera.css` is the
  authoritative BERA styling; `js/app.js` is the shell Alpine component; per-screen
  components live alongside it (`js/crud.js` fills the Model section, E6-S2, with a
  data-driven entity-CRUD component driving the Twirp entity RPCs); `vendor/` holds
  the pre-built blobs (see `vendor/README.md` for pinned versions + regeneration).
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
