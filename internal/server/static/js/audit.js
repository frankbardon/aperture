/*
 * audit.js — Aperture audit viewer (E6-S4), the "Audit" section of the admin
 * shell. One READ-ONLY Alpine component drives a queryable table over the append-
 * only audit trail via the Twirp facade's QueryAudit RPC (never storage). The
 * trail is the source of truth for who did what: every mutation, impersonation,
 * and delegation is recorded always; decisions are sampled.
 *
 * Wrapped in an IIFE so its top-level consts do not collide with crud.js /
 * grants.js — classic <script> tags share one global lexical environment, so a
 * bare `const PREFIX` here would be a redeclaration SyntaxError. Only
 * window.audit is exported.
 *
 * Auth + tiers (reused from the crud/grants probe pattern): reading the trail is
 * gated — a system-admin reads everything; an account-admin reads only events
 * scoped to their own account. We probe both via the OPEN Check RPC; a viewer
 * with neither is told why the table is empty, and any rejection surfaces its
 * APERTURE_* code + message. Identities render in IBM Plex Mono; no emoji.
 */
(function () {
  const TOKEN_KEY = "aperture.devToken";
  const PREFIX = "/twirp/aperture.ApertureService/";
  const ADMIN_ACTION = "aperture.admin";
  const SYSTEM_ANCHOR = "system:schema"; // system-tier authority anchor (authz.go)
  const accountAnchor = (a) => "account:" + a + "/admin:all"; // account-tier anchor

  // rpcCall POSTs a Twirp JSON call through the shell's bearer wrapper and
  // normalises a non-2xx Twirp error body into an Error with .code (the canonical
  // APERTURE_* code from meta) + .message, mirroring crud.js/grants.js.
  async function rpcCall(method, body) {
    const res = await window.apiFetch(PREFIX + method, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    });
    const text = await res.text();
    let data = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (_) {
        data = null;
      }
    }
    if (!res.ok) {
      const code =
        (data && data.meta && data.meta.code) ||
        (data && data.code) ||
        "APERTURE_ERROR";
      const msg = (data && (data.msg || data.message)) || res.statusText || "Request failed";
      const err = new Error(msg);
      err.code = code;
      err.status = res.status;
      throw err;
    }
    return data || {};
  }

  const parseList = (resp) => (resp.entities_json || []).map((s) => JSON.parse(s));

  const EVENT_TYPES = ["mutation", "decision", "impersonation", "delegation"];
  const OUTCOMES = ["allow", "deny", "success", "failure"];

  // toRFC3339 converts a datetime-local input value ("2026-06-30T12:34") to an
  // RFC3339 instant the QueryAudit filter parses. Empty stays empty (unbounded).
  function toRFC3339(local) {
    if (!local) return "";
    const d = new Date(local);
    return isNaN(d.getTime()) ? "" : d.toISOString();
  }

  function audit() {
    return {
      principal: "",
      accounts: [],
      account: "",
      canReadAll: false, // system-admin: the whole trail
      canReadAccount: false, // account-admin: this account only
      tierChecked: false,

      loading: false,
      error: null,
      rows: [],

      // The audit query filter. Every field is an optional narrowing predicate,
      // ANDed server-side. actor filters by the REAL actor (the operator under
      // impersonation, never the borrowed target).
      filter: { actor: "", eventType: "", outcome: "", since: "", until: "", limit: 100 },

      init() {
        this.principal = localStorage.getItem(TOKEN_KEY) || "";
        document.addEventListener("aperture:authenticated", (e) => {
          this.principal = (e.detail && e.detail.principal) || localStorage.getItem(TOKEN_KEY) || "";
          this.bootstrap();
        });
        const clear = () => {
          this.principal = "";
          this.rows = [];
          this.canReadAll = false;
          this.canReadAccount = false;
          this.tierChecked = false;
        };
        document.addEventListener("aperture:unauthenticated", clear);
        document.addEventListener("aperture:signout", clear);
        if (this.principal) this.bootstrap();
      },

      get eventTypes() {
        return EVENT_TYPES;
      },
      get outcomes() {
        return OUTCOMES;
      },
      get canRead() {
        return this.canReadAll || this.canReadAccount;
      },

      async bootstrap() {
        await this.loadAccounts();
        await this.probeTier();
        await this.load();
      },

      async loadAccounts() {
        try {
          this.accounts = parseList(await rpcCall("ListAccounts", {}));
          if (!this.account && this.accounts.length > 0) this.account = this.accounts[0].ID;
        } catch (e) {
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        }
      },

      // probeTier resolves BOTH read tiers via the open Check RPC: system-admin
      // reads the whole trail; account-admin reads only the active account. A
      // viewer with neither still gets an answer (Check is open).
      async probeTier() {
        this.tierChecked = false;
        this.canReadAll = await this.check(SYSTEM_ANCHOR);
        this.canReadAccount = await this.check(accountAnchor(this.account));
        this.tierChecked = true;
      },

      async check(object) {
        try {
          const dec = await rpcCall("Check", {
            account: this.account,
            principal: this.principal,
            action: ADMIN_ACTION,
            object,
          });
          return !!dec.allow;
        } catch (_) {
          return false;
        }
      },

      async onAccountChange() {
        await this.probeTier();
        await this.load();
      },

      // load runs the query. An account-admin (without system authority) may only
      // read their own account, so the account filter is forced to the active
      // account for them; a system-admin may read across all accounts (account
      // filter left blank unless they narrow it).
      async load() {
        if (!this.principal) return;
        this.loading = true;
        this.error = null;
        const scopedAccount = this.canReadAll ? "" : this.account;
        try {
          const resp = await rpcCall("QueryAudit", {
            actor: { principal: this.principal, account: this.account },
            filter_actor: this.filter.actor || "",
            account: scopedAccount,
            event_type: this.filter.eventType || "",
            outcome: this.filter.outcome || "",
            since: toRFC3339(this.filter.since),
            until: toRFC3339(this.filter.until),
            limit: parseInt(this.filter.limit, 10) || 0,
          });
          this.rows = (resp.events_json || []).map((s) => JSON.parse(s));
        } catch (e) {
          this.rows = [];
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        } finally {
          this.loading = false;
        }
      },

      resetFilter() {
        this.filter = { actor: "", eventType: "", outcome: "", since: "", until: "", limit: 100 };
        this.load();
      },

      // ---- cell rendering helpers (read-only) ----

      when(ev) {
        // AuditEvent.Timestamp is RFC3339; render it compactly in local time.
        const d = new Date(ev.Timestamp);
        return isNaN(d.getTime()) ? ev.Timestamp || "—" : d.toLocaleString();
      },

      // effectiveText shows the borrowed subject + mode for an impersonated or
      // delegated event, so an action is never mis-attributed to the target
      // alone. Empty (em dash) on the ordinary path.
      effectiveText(ev) {
        if (!ev.EffectiveSubject) return "—";
        const mode = ev.ImpersonationMode ? " (" + ev.ImpersonationMode + ")" : "";
        return ev.EffectiveSubject + mode;
      },

      outcomeClass(ev) {
        return "bera-badge--" + String(ev.Outcome || "").toLowerCase();
      },

      dash(v) {
        return v === "" || v === null || v === undefined ? "—" : String(v);
      },
    };
  }

  window.audit = audit;
})();
