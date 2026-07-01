/*
 * portability.js — Aperture import/export (E6-S4), the "Import / export" section
 * of the admin shell. One Alpine component drives the declarative model file over
 * the Twirp facade (never storage): EXPORT downloads the whole model as one
 * canonical JSON document (system-admin read); IMPORT uploads such a file and,
 * because Import is a transactional idempotent-upsert with no server dry-run,
 * shows a client-side PREVIEW DIFF (what would be added or changed) against a
 * fresh Export before the operator commits.
 *
 * Import is ADDITIVE: it upserts every entity in the file and never deletes
 * entities absent from it, so an entity present only in the current model is
 * RETAINED, not removed — the diff says so rather than pretending it is a delete.
 *
 * Wrapped in an IIFE so its top-level consts do not collide with the other
 * screens' classic <script> globals. Only window.portability is exported.
 * Identities render in IBM Plex Mono; no emoji.
 */
(function () {
  const TOKEN_KEY = "aperture.devToken";
  const PREFIX = "/twirp/aperture.ApertureService/";
  const ADMIN_ACTION = "aperture.admin";
  const SYSTEM_ANCHOR = "system:schema"; // system-tier authority anchor (authz.go)

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

  // COLLECTIONS describes the seed.Document top-level entity sets and how each is
  // keyed for the diff. Import is idempotent per-key, so the same key in both
  // files with a different body is a CHANGE; a key only in the upload is an ADD.
  const COLLECTIONS = [
    { key: "accounts", label: "Accounts", idFields: ["id"] },
    { key: "memberships", label: "Memberships", idFields: ["principal", "account"] },
    { key: "object_types", label: "Object types", idFields: ["name"] },
    { key: "permissions", label: "Permissions", idFields: ["id"] },
    { key: "principals", label: "Principals", idFields: ["id"] },
    { key: "roles", label: "Roles", idFields: ["id"] },
    { key: "groups", label: "Groups", idFields: ["id"] },
    { key: "grants", label: "Grants", idFields: ["id"] },
    { key: "templates", label: "Templates", idFields: ["name", "version"] },
    { key: "rules", label: "Rules", idFields: ["name"] },
  ];

  const keyOf = (col, item) => col.idFields.map((f) => String(item[f])).join(" · ");

  // stableStringify canonicalises an object for equality comparison independent of
  // key order (the uploaded file may order keys differently than the export).
  function stableStringify(v) {
    if (Array.isArray(v)) return "[" + v.map(stableStringify).join(",") + "]";
    if (v && typeof v === "object") {
      return (
        "{" +
        Object.keys(v)
          .sort()
          .map((k) => JSON.stringify(k) + ":" + stableStringify(v[k]))
          .join(",") +
        "}"
      );
    }
    return JSON.stringify(v);
  }

  function portability() {
    return {
      principal: "",
      accounts: [],
      account: "",
      canManage: false, // system-admin: export + import
      tierChecked: false,

      loading: false,
      error: null,

      // export state
      exported: null, // parsed current document
      exportSummary: [], // [{label, count}]

      // import state
      importFile: "", // uploaded filename
      uploadedText: "", // raw uploaded JSON
      diff: null, // computed preview
      importing: false,
      importError: null,
      importDone: false,

      init() {
        this.principal = localStorage.getItem(TOKEN_KEY) || "";
        document.addEventListener("aperture:authenticated", (e) => {
          this.principal = (e.detail && e.detail.principal) || localStorage.getItem(TOKEN_KEY) || "";
          this.bootstrap();
        });
        const clear = () => {
          this.principal = "";
          this.canManage = false;
          this.tierChecked = false;
          this.resetImport();
        };
        document.addEventListener("aperture:unauthenticated", clear);
        document.addEventListener("aperture:signout", clear);
        if (this.principal) this.bootstrap();
      },

      get collections() {
        return COLLECTIONS;
      },

      async bootstrap() {
        await this.loadAccounts();
        await this.probeTier();
      },

      async loadAccounts() {
        try {
          const resp = await rpcCall("ListAccounts", {});
          this.accounts = (resp.entities_json || []).map((s) => JSON.parse(s));
          if (!this.account && this.accounts.length > 0) this.account = this.accounts[0].ID;
        } catch (e) {
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        }
      },

      // probeTier: export + import are SYSTEM tier, gated on system-admin. A
      // non-admin is told why the actions are unavailable; the server is the
      // source of truth and any rejection surfaces its APERTURE_* code.
      async probeTier() {
        this.tierChecked = false;
        try {
          const dec = await rpcCall("Check", {
            account: this.account,
            principal: this.principal,
            action: ADMIN_ACTION,
            object: SYSTEM_ANCHOR,
          });
          this.canManage = !!dec.allow;
        } catch (_) {
          this.canManage = false;
        }
        this.tierChecked = true;
      },

      async onAccountChange() {
        await this.probeTier();
      },

      actor() {
        return { principal: this.principal, account: this.account };
      },

      summarise(doc) {
        return COLLECTIONS.map((c) => ({ label: c.label, count: (doc[c.key] || []).length }));
      },

      // ---- EXPORT ----

      // fetchExport reads the current model document from the server. It is the
      // source both for the download and for the import diff's "current" side.
      async fetchExport() {
        const resp = await rpcCall("Export", { actor: this.actor() });
        return JSON.parse(resp.document_json);
      },

      async exportModel() {
        this.loading = true;
        this.error = null;
        try {
          this.exported = await this.fetchExport();
          this.exportSummary = this.summarise(this.exported);
          this.download(JSON.stringify(this.exported, null, 2));
        } catch (e) {
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        } finally {
          this.loading = false;
        }
      },

      // download triggers a browser save of the declarative model file.
      download(text) {
        const blob = new Blob([text], { type: "application/json" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = "aperture-model.json";
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
      },

      // ---- IMPORT (preview diff, then apply) ----

      resetImport() {
        this.importFile = "";
        this.uploadedText = "";
        this.diff = null;
        this.importError = null;
        this.importDone = false;
      },

      // onFile reads the chosen file and computes the preview diff against a fresh
      // Export. It never applies anything — the operator confirms after reviewing.
      async onFile(event) {
        this.importError = null;
        this.importDone = false;
        this.diff = null;
        const file = event.target.files && event.target.files[0];
        if (!file) return;
        this.importFile = file.name;
        try {
          this.uploadedText = await file.text();
        } catch (e) {
          this.importError = { code: "APERTURE_INVALID_INPUT", msg: "Could not read the file." };
          return;
        }
        await this.computeDiff();
      },

      async computeDiff() {
        this.importError = null;
        let uploaded;
        try {
          uploaded = JSON.parse(this.uploadedText);
        } catch (_) {
          this.importError = { code: "APERTURE_INVALID_INPUT", msg: "The uploaded file is not valid JSON." };
          return;
        }
        let current;
        try {
          current = await this.fetchExport();
        } catch (e) {
          this.importError = { code: e.code, msg: e.message };
          return;
        }
        this.diff = this.buildDiff(current, uploaded);
      },

      // buildDiff computes, per collection, the entities the import would ADD (in
      // the upload, absent from current) and CHANGE (present in both, different
      // body), plus a count of entities only in current (RETAINED — import never
      // deletes). This is the idempotent-upsert semantics made visible.
      buildDiff(current, uploaded) {
        const groups = [];
        let totalAdded = 0;
        let totalChanged = 0;
        for (const col of COLLECTIONS) {
          const cur = new Map((current[col.key] || []).map((it) => [keyOf(col, it), it]));
          const up = uploaded[col.key] || [];
          const added = [];
          const changed = [];
          for (const it of up) {
            const k = keyOf(col, it);
            if (!cur.has(k)) {
              added.push({ key: k, item: it });
            } else if (stableStringify(cur.get(k)) !== stableStringify(it)) {
              changed.push({ key: k, item: it, before: cur.get(k) });
            }
          }
          const upKeys = new Set(up.map((it) => keyOf(col, it)));
          const retained = [...cur.keys()].filter((k) => !upKeys.has(k)).length;
          totalAdded += added.length;
          totalChanged += changed.length;
          groups.push({ label: col.label, key: col.key, added, changed, retained });
        }
        return { groups, totalAdded, totalChanged, empty: totalAdded === 0 && totalChanged === 0 };
      },

      // applyImport commits the uploaded document via the transactional Import.
      async applyImport() {
        this.importError = null;
        this.importing = true;
        try {
          await rpcCall("Import", { actor: this.actor(), document_json: this.uploadedText });
          this.importDone = true;
          // Refresh the export summary so the applied state is visible.
          this.exported = await this.fetchExport();
          this.exportSummary = this.summarise(this.exported);
          // Recompute the diff — after an idempotent import it should be empty.
          await this.computeDiff();
        } catch (e) {
          this.importError = { code: e.code, msg: e.message };
        } finally {
          this.importing = false;
        }
      },
    };
  }

  window.portability = portability;
})();
