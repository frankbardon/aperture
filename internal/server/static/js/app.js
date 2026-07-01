/*
 * app.js — Aperture admin shell behaviour (Alpine.js component).
 *
 * Responsibilities of the CHROME (no domain screens live here yet):
 *   - Carry the dev bearer on every API request. Aperture's auth is EXTERNAL —
 *     this shell never issues credentials. For local/demo it uses the dev/static
 *     authenticator (auth/dev.go): the bearer IS the principal id, so "signing
 *     in" is just naming which principal you are. The token is kept in
 *     localStorage and attached as `Authorization: Bearer <id>` by apiFetch.
 *   - Show a sign-in affordance when no token is present.
 *   - Drive the hash-routed nav skeleton (crud / grants / audit / rules). The
 *     sections are intentionally empty — E6-S2/S3/S4 fill them, E7 mounts the
 *     Rete rule canvas in the "rules" section.
 */

const TOKEN_KEY = "aperture.devToken";

// Nav skeleton — the sections the next stories fill. Sentence case, no emoji.
const SECTIONS = [
  { id: "crud", label: "Model", route: "#/crud", story: "E6-S2",
    note: "Principals, roles, groups, permissions and objects." },
  { id: "grants", label: "Grants", route: "#/grants", story: "E6-S3",
    note: "Bestowed grants, delegation and impersonation." },
  { id: "audit", label: "Audit", route: "#/audit", story: "E6-S4",
    note: "The append-only decision and mutation log." },
  { id: "rules", label: "Rules", route: "#/rules", story: "E7",
    note: "The Rete canvas that edits the pulse-expression rule AST." },
];

// apiFetch wraps window.fetch to attach the dev bearer. Every call the shell
// (and later the domain screens) makes to the Aperture API goes through here so
// the auth header is applied in exactly one place. A 401 clears the token and
// re-opens the sign-in affordance — a bad/rotated token never fails silently.
async function apiFetch(input, init) {
  const opts = init ? { ...init } : {};
  const headers = new Headers(opts.headers || {});
  const token = localStorage.getItem(TOKEN_KEY);
  if (token) {
    headers.set("Authorization", "Bearer " + token);
  }
  opts.headers = headers;
  const res = await fetch(input, opts);
  if (res.status === 401) {
    localStorage.removeItem(TOKEN_KEY);
    document.dispatchEvent(new CustomEvent("aperture:unauthenticated"));
  }
  return res;
}

// Expose apiFetch so future per-screen Alpine components share the one wrapper.
window.apiFetch = apiFetch;

function shell() {
  return {
    sections: SECTIONS,
    principal: localStorage.getItem(TOKEN_KEY) || "",
    signInId: "",
    active: "crud",

    get signedIn() {
      return this.principal !== "";
    },
    get activeSection() {
      return this.sections.find((s) => s.id === this.active) || this.sections[0];
    },

    init() {
      this.syncRoute();
      window.addEventListener("hashchange", () => this.syncRoute());
      document.addEventListener("aperture:unauthenticated", () => {
        this.principal = "";
      });
    },

    // syncRoute maps the URL hash to the active section, defaulting to crud.
    syncRoute() {
      const id = (window.location.hash || "").replace(/^#\/?/, "");
      const match = this.sections.find((s) => s.id === id);
      this.active = match ? match.id : this.sections[0].id;
    },

    // signIn records the chosen principal id as the dev bearer. Auth is external:
    // this is not credential issuance, only selecting which principal the dev
    // session presents to the dev/static authenticator.
    signIn() {
      const id = this.signInId.trim();
      if (id === "") {
        return;
      }
      localStorage.setItem(TOKEN_KEY, id);
      this.principal = id;
      this.signInId = "";
      // Notify per-screen components (e.g. crud.js) that a principal is present
      // so they can (re)load their data through the now-authenticated apiFetch.
      document.dispatchEvent(new CustomEvent("aperture:authenticated", { detail: { principal: id } }));
    },

    signOut() {
      localStorage.removeItem(TOKEN_KEY);
      this.principal = "";
      document.dispatchEvent(new CustomEvent("aperture:signout"));
    },
  };
}

window.shell = shell;
