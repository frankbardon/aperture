/*
 * whatif.js — Aperture what-if simulator (E6-S4), the "What-if" section of the
 * admin shell. One READ-ONLY Alpine component runs a hypothetical decision
 * against the LIVE model and renders the full Explain trace, so an operator can
 * ask "would principal P be allowed action A on object O?" and see not just the
 * verdict but WHY: the expanded subject set, every grant considered with its
 * per-grant outcome (action match, scope coverage, specificity), the deny-
 * overrides/specificity tiebreak, and the deciding grant(s).
 *
 * It uses the OPEN Check + Explain RPCs (no auth required, no mutation) — a plain
 * what-if against the live model is exactly Check/Explain(principal, action,
 * object, account). The bearer still rides along so the trace resolves the same
 * way the signed-in session's own checks do.
 *
 * Wrapped in an IIFE so its top-level consts do not collide with the other
 * screens' classic <script> globals. Only window.whatif is exported. Identities
 * render in IBM Plex Mono; no emoji.
 */
(function () {
  const TOKEN_KEY = "aperture.devToken";
  const PREFIX = "/twirp/aperture.ApertureService/";

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

  function whatif() {
    return {
      principal: "",
      accounts: [],
      account: "",

      // The hypothetical question. principal defaults to the signed-in identity
      // but is editable — a what-if may ask about ANY principal.
      form: { principal: "", action: "", object: "" },

      running: false,
      error: null,
      ran: false,
      decision: null, // { allow, reason, deciding_grant_ids } from Check
      trace: null, // engine.Trace from Explain

      init() {
        this.principal = localStorage.getItem(TOKEN_KEY) || "";
        this.form.principal = this.principal;
        document.addEventListener("aperture:authenticated", (e) => {
          this.principal = (e.detail && e.detail.principal) || localStorage.getItem(TOKEN_KEY) || "";
          if (!this.form.principal) this.form.principal = this.principal;
          this.bootstrap();
        });
        const clear = () => {
          this.principal = "";
          this.decision = null;
          this.trace = null;
          this.ran = false;
        };
        document.addEventListener("aperture:unauthenticated", clear);
        document.addEventListener("aperture:signout", clear);
        if (this.principal) this.bootstrap();
      },

      async bootstrap() {
        try {
          this.accounts = parseList(await rpcCall("ListAccounts", {}));
          if (!this.account && this.accounts.length > 0) this.account = this.accounts[0].ID;
        } catch (e) {
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        }
      },

      // run simulates the decision against the LIVE model. Check gives the
      // headline verdict; Explain gives the full derivation. Neither mutates.
      async run() {
        this.error = null;
        if (!this.form.principal || !this.form.action || !this.form.object) {
          this.error = { code: "APERTURE_INVALID_INPUT", msg: "Principal, action and object are required." };
          return;
        }
        this.running = true;
        this.decision = null;
        this.trace = null;
        const q = {
          account: this.account,
          principal: this.form.principal,
          action: this.form.action,
          object: this.form.object,
        };
        try {
          const [dec, exp] = await Promise.all([rpcCall("Check", q), rpcCall("Explain", q)]);
          this.decision = dec;
          this.trace = exp.trace_json ? JSON.parse(exp.trace_json) : null;
          this.ran = true;
        } catch (e) {
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        } finally {
          this.running = false;
        }
      },

      // ---- trace rendering helpers ----

      subjectText(s) {
        return (s && s.Kind ? s.Kind + ":" : "") + (s ? s.ID : "");
      },

      subjects() {
        return (this.trace && this.trace.Subjects) || [];
      },

      considered() {
        return (this.trace && this.trace.Considered) || [];
      },

      decidingIds() {
        return (this.decision && this.decision.deciding_grant_ids) || [];
      },

      yesno(v) {
        return v ? "Yes" : "No";
      },

      dash(v) {
        return v === "" || v === null || v === undefined ? "—" : String(v);
      },
    };
  }

  window.whatif = whatif;
})();
