/*
 * grants.js — Aperture provisioning screens (E6-S3), the "Grants" section of the
 * admin shell. One Alpine component drives three tabs against the Twirp facade
 * (never storage): raw GRANT management, TEMPLATE apply (typed ${params} → a
 * client-side preview of the would-be grants → one transactional ApplyTemplate,
 * plus bulk provisioning of many principals in one BulkPutGrants), and
 * DELEGATION (a delegator bestows a delegatable subset of their own grants).
 *
 * Everything is wrapped in an IIFE so its top-level consts do not collide with
 * crud.js's — classic <script> tags share one global lexical environment, so a
 * bare `const RPC_PREFIX` here would be a redeclaration SyntaxError. Only
 * window.grants is exported.
 *
 * Auth + tiers (reused from crud.js's probe pattern): raw grant ops and
 * delegation + apply are ACCOUNT tier (probe aperture.admin over
 * account:<a>/admin:all); template DEFINITION is SYSTEM tier (probe over
 * system:schema). Non-eligible actions are hidden, but the server is always the
 * source of truth — any rejection surfaces its APERTURE_* code + message.
 */
(function () {
  const TOKEN_KEY = "aperture.devToken";
  const PREFIX = "/twirp/aperture.ApertureService/";
  const ADMIN_ACTION = "aperture.admin";
  const SYSTEM_ANCHOR = "system:schema"; // system-tier authority anchor (authz.go)
  const accountAnchor = (a) => "account:" + a + "/admin:all"; // account-tier anchor
  const ACCOUNT_WILDCARD = "*"; // the all-accounts grant stamp (model.AccountWildcard)
  // GRANT_PAGE_SIZE is the client page size for the grants listing. It stays at
  // or below model.MaxGrantPageSize (500) so a single request never asks the
  // server for more than it will return; the server clamps anything larger, but
  // we never send an over-cap limit. WILDCARD_OVERLAY_LIMIT fetches the "*"
  // (platform) overlay in single-account mode as one small, non-paginated group
  // — wildcard-account grants are few, so a generous single page covers them.
  const GRANT_PAGE_SIZE = 100;
  const WILDCARD_OVERLAY_LIMIT = 500;

  // rpcCall POSTs a Twirp JSON call through the shell's bearer wrapper and
  // normalises a non-2xx Twirp error body into an Error with .code (the canonical
  // APERTURE_* code from meta) + .message, mirroring crud.js's rpc().
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

  // substitute mirrors model.substitute: replace every ${name} token with its
  // param value, so the apply PREVIEW shows the exact grants the server will
  // expand (there is no dry-run endpoint — ApplyTemplate is transactional, so the
  // preview is done client-side from the same ${name} grammar). Unfilled tokens
  // render as ${name} so a partial preview is legible rather than silently wrong.
  function substitute(s, params) {
    return String(s || "").replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (m, name) =>
      params[name] !== undefined && params[name] !== "" ? params[name] : m
    );
  }

  const SUBJECT_KINDS = ["principal", "role", "group"];

  function grants() {
    return {
      tabs: [
        { key: "grants", label: "Grants" },
        { key: "templates", label: "Templates" },
        { key: "delegation", label: "Delegation" },
      ],
      activeTab: "grants",

      principal: "",
      accounts: [],
      // account mirrors the LOCAL grants filter (grantAccountFilter). Empty = the
      // all-accounts view (E1-S3): the Grants view owns its scope locally and no
      // longer follows the shell's global account switcher. Downstream helpers
      // (actor, grant editor default, delegation, apply) read this value.
      account: "",
      // grantAccountFilter is the Grants view's own account filter. Empty string
      // is the default "all accounts" listing (system-admin only, per E1-S2);
      // a non-empty value narrows to that single account. "*" is NOT sent as an
      // all-accounts sentinel — it is the wildcard account.
      grantAccountFilter: "",
      canGrant: false, // account-admin: raw grants, apply, bulk, delegation ops
      canDefineTemplate: false, // system-admin: template definition CRUD
      tierChecked: false,

      loading: false,
      error: null,

      // Reference lists used to populate subject/permission/principal pickers.
      refs: { permissions: [], principals: [], roles: [], groups: [] },

      // ---- grants tab ----
      rows: [],
      // page holds the pagination envelope for the account-specific (or
      // all-accounts) grant listing echoed back by ListGrants: offset/limit are
      // the effective server-clamped window and total is the full pre-pagination
      // match count that drives prev/next. wildcardCount is the number of "*"
      // (platform) overlay grants merged in only in single-account mode — an
      // always-included, NON-paginated group shown alongside the account's page,
      // deliberately excluded from total so the "X–Y of N" count stays honest
      // about the account's own grants.
      page: { offset: 0, limit: GRANT_PAGE_SIZE, total: 0, wildcardCount: 0 },
      grantModal: { open: false, mode: "create", form: {}, strategyFilter: "", saving: false, error: null },
      confirm: { open: false, grant: null, saving: false, error: null },

      // ---- server-side filters (grants + templates tabs) ----
      grantFilters: [],
      grantFilterMatch: "all",
      grantCols: [
        { key: "ID", label: "Id" },
        { key: "AccountID", label: "Account" },
        { key: "PermissionID", label: "Permission" },
        { key: "Object", label: "Object" },
        { key: "Effect", label: "Effect" },
      ],
      tmplFilters: [],
      tmplFilterMatch: "all",
      tmplCols: [
        { key: "Name", label: "Name" },
        { key: "Version", label: "Version" },
        { key: "Description", label: "Description" },
      ],

      // ---- templates tab ----
      templates: [],
      tmplModal: { open: false, mode: "create", form: {}, saving: false, error: null },
      tmplConfirm: { open: false, tmpl: null, saving: false, error: null },
      apply: {
        open: false,
        mode: "single", // "single" | "bulk"
        name: "",
        version: 0,
        params: {},
        prefix: "",
        targetAccount: "",
        subjectParam: "", // bulk: which param the per-principal id fills
        principals: [], // bulk: selected principal ids
        preview: [],
        applied: [],
        applying: false,
        error: null,
      },

      // ---- delegation tab ----
      bestow: { open: false, form: {}, selectedHeldId: "", saving: false, error: null },
      revokeConfirm: { open: false, grant: null, saving: false, error: null },

      init() {
        this.principal = localStorage.getItem(TOKEN_KEY) || "";
        document.addEventListener("aperture:authenticated", (e) => {
          this.principal = (e.detail && e.detail.principal) || localStorage.getItem(TOKEN_KEY) || "";
          this.bootstrap();
        });
        const clear = () => {
          this.principal = "";
          this.account = "";
          this.grantAccountFilter = ""; // reset to the all-accounts default
          this.accounts = [];
          this.rows = [];
          this.templates = [];
          this.canGrant = false;
          this.canDefineTemplate = false;
          this.tierChecked = false;
        };
        document.addEventListener("aperture:unauthenticated", clear);
        document.addEventListener("aperture:signout", clear);
        // E1-S3: the Grants view owns its account scope locally and deliberately
        // does NOT subscribe to the shell's global "aperture:account" broadcast.
        if (this.principal) this.bootstrap();
      },

      async bootstrap() {
        // The Grants view defaults to the all-accounts listing; the local filter
        // (grantAccountFilter) starts empty and account mirrors it.
        this.grantAccountFilter = "";
        this.account = "";
        this.page.offset = 0;
        await this.loadAccounts();
        await this.probeTier();
        await this.loadRefs();
        await this.loadTab();
      },

      // loadAccounts fetches the accounts this principal may see so the local
      // filter (and the grant editor's account picker) have options. The server
      // scopes the list; a failure just leaves the filter as all-accounts.
      async loadAccounts() {
        try {
          this.accounts = parseList(await rpcCall("ListAccounts", {}));
        } catch (_) {
          this.accounts = [];
        }
      },

      // probeTier resolves BOTH tiers via the open Check RPC: account-admin (in
      // the active account) gates grants/apply/bulk/delegation; system-admin gates
      // template definition. A non-admin still gets an answer (Check is open).
      async probeTier() {
        this.tierChecked = false;
        this.canDefineTemplate = await this.check(SYSTEM_ANCHOR); // system-admin
        // The account-admin probe needs a concrete account. In the all-accounts
        // view (empty filter) there is none, so rely on system-admin; when the
        // filter narrows to a single account, probe that account's admin anchor.
        const accountAdmin = this.account ? await this.check(accountAnchor(this.account)) : false;
        // System-admin supersedes account-admin (mirrors the authz gate), so a
        // system-admin can manage grants in the active account too.
        this.canGrant = accountAdmin || this.canDefineTemplate;
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

      async loadRefs() {
        const jobs = [
          ["permissions", "ListPermissions"],
          ["principals", "ListPrincipals"],
          ["roles", "ListRoles"],
          ["groups", "ListGroups"],
        ];
        for (const [key, method] of jobs) {
          try {
            this.refs[key] = parseList(await rpcCall(method, {}));
          } catch (_) {
            this.refs[key] = [];
          }
        }
      },

      actor() {
        return { principal: this.principal, account: this.account };
      },

      async selectTab(key) {
        this.activeTab = key;
        await this.loadTab();
      },

      // onGrantAccountFilterChange reacts to the LOCAL account filter (not the
      // shell). Empty = all accounts; a value narrows to that single account.
      // account mirrors the filter so downstream helpers stay consistent, then
      // the tier is re-probed (a single-account view can gate account-admin ops)
      // and the active tab reloads. Switching scope resets to the first page.
      async onGrantAccountFilterChange() {
        this.account = this.grantAccountFilter;
        this.page.offset = 0;
        await this.probeTier();
        await this.loadTab();
      },

      async loadTab() {
        if (!this.principal) return;
        this.error = null;
        if (this.activeTab === "grants") return this.loadGrants();
        if (this.activeTab === "templates") return this.loadTemplates();
        if (this.activeTab === "delegation") return this.loadGrants(); // delegation reads the same grant list
      },

      // ================= GRANTS TAB =================

      // ---- server-side filter helpers (shared by both tabs) ----
      _buildFilter(list, match) {
        const preds = list
          .filter((f) => f.field && (f.op === "empty" || f.value !== ""))
          .map((f) => ({ field: f.field, op: f.op, value: f.op === "empty" ? "" : f.value }));
        return preds.length ? { filter: { match, predicates: preds } } : {};
      },
      filterNeedsValue(op) { return op !== "empty"; },
      addGrantFilter() { this.grantFilters.push({ field: this.grantCols[0].key, op: "contains", value: "" }); },
      removeGrantFilter(i) { this.grantFilters.splice(i, 1); this.reloadGrants(); },
      clearGrantFilters() { this.grantFilters = []; this.grantFilterMatch = "all"; this.reloadGrants(); },
      addTmplFilter() { this.tmplFilters.push({ field: this.tmplCols[0].key, op: "contains", value: "" }); },
      removeTmplFilter(i) { this.tmplFilters.splice(i, 1); this.loadTemplates(); },
      clearTmplFilters() { this.tmplFilters = []; this.tmplFilterMatch = "all"; this.loadTemplates(); },

      // reloadGrants resets to the first page, then loads. Any change to what the
      // page window means — the account filter or the search Filter — must go
      // through here so the operator is never stranded on an offset past the end
      // of a freshly narrowed result set.
      reloadGrants() {
        this.page.offset = 0;
        return this.loadGrants();
      },

      async loadGrants() {
        this.loading = true;
        this.error = null;
        const flt = this._buildFilter(this.grantFilters, this.grantFilterMatch);
        const scope = this.grantAccountFilter; // "" = all accounts (E1-S3)
        // Clamp the outbound limit at the server cap so a single request never
        // asks for more than the server will return (it clamps too, but we don't
        // rely on that). offset is the current page window.
        const limit = Math.min(GRANT_PAGE_SIZE, 500);
        const offset = Math.max(0, this.page.offset);
        try {
          // All-accounts mode: a single ListGrants with an EMPTY account_id
          // returns every account's grants — including "*" (wildcard-account)
          // grants — inline, and is paginated by offset/limit. Do NOT issue the
          // separate "*" overlay fetch here; that would duplicate the wildcard
          // rows the all-accounts page already carries. (Empty string is the
          // all-accounts sentinel; "*" is the wildcard account and must never be
          // used to mean "all".)
          const resp = await rpcCall("ListGrants", {
            actor: this.actor(),
            account_id: scope,
            offset,
            limit,
            ...flt,
          });
          let rows = parseList(resp);
          // Adopt the effective (server-clamped) window + total from the echoed
          // envelope so prev/next and the "X–Y of N" count reflect what the
          // server actually paged. wildcardCount defaults to 0 (all-accounts
          // mode has no separate overlay).
          this.page.offset = resp.offset || 0;
          this.page.limit = resp.limit || limit;
          this.page.total = resp.total || 0;
          this.page.wildcardCount = 0;
          // Single-account mode: the per-account listing never returns "*"
          // (wildcard-account) grants even though they apply in this account, so
          // overlay them with a second call — a SMALL, always-included,
          // NON-paginated group. Its rows are appended below the account's page
          // but excluded from page.total, which stays the account-specific count
          // that drives prev/next. Skipped when the filter is itself "*".
          // Non-fatal if the overlay fails.
          if (scope && scope !== ACCOUNT_WILDCARD) {
            try {
              const starResp = await rpcCall("ListGrants", {
                actor: this.actor(),
                account_id: ACCOUNT_WILDCARD,
                offset: 0,
                limit: WILDCARD_OVERLAY_LIMIT,
                ...flt,
              });
              const starRows = parseList(starResp);
              this.page.wildcardCount = starRows.length;
              rows = rows.concat(starRows);
            } catch (_) {
              /* keep the account's own grants */
            }
          }
          this.rows = rows;
        } catch (e) {
          this.rows = [];
          this.page.total = 0;
          this.page.wildcardCount = 0;
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        } finally {
          this.loading = false;
        }
      },

      // ---- pagination (grants tab) ----

      // hasPrev / hasNext gate the prev/next controls off the account-specific
      // (or all-accounts) window; the "*" overlay group is intentionally not part
      // of the page count. next reaches the following page while
      // offset + limit < total (matching the server: next_offset < total).
      hasPrev() {
        return this.page.offset > 0;
      },
      hasNext() {
        return this.page.offset + this.page.limit < this.page.total;
      },
      // pageStart / pageEnd are the 1-based inclusive bounds of the account's own
      // grants on the current page, for the "showing X–Y of N" display. When the
      // account's total is 0 the range collapses to 0–0.
      pageStart() {
        return this.page.total === 0 ? 0 : this.page.offset + 1;
      },
      pageEnd() {
        // rows also holds the wildcard overlay in single-account mode, so derive
        // the account-page row count from the window/total, not rows.length.
        const paged = this.rows.length - this.page.wildcardCount;
        return Math.min(this.page.offset + paged, this.page.total);
      },
      async nextPage() {
        if (!this.hasNext()) return;
        this.page.offset = this.page.offset + this.page.limit;
        await this.loadGrants();
      },
      async prevPage() {
        if (!this.hasPrev()) return;
        this.page.offset = Math.max(0, this.page.offset - this.page.limit);
        await this.loadGrants();
      },

      // isWildcardGrant reports whether a grant spans all accounts (stamped "*").
      isWildcardGrant(g) {
        return g && g.AccountID === ACCOUNT_WILDCARD;
      },

      // strategyName reduces an opaque ScopeStrategy reference ("inclusive;ids=…")
      // to just its strategy name ("inclusive") for compact display. An empty ref
      // means the literal default.
      strategyName(ref) {
        const name = String(ref || "").split(";")[0].trim();
        return name || "literal";
      },

      permLabel(id) {
        const p = (this.refs.permissions || []).find((x) => x.ID === id);
        if (!p) return id;
        const scope = p.ScopeStrategy ? " · " + this.strategyName(p.ScopeStrategy) : "";
        return p.ID + " (" + p.Action + scope + ")";
      },

      permScope(id) {
        const p = (this.refs.permissions || []).find((x) => x.ID === id);
        return p && p.ScopeStrategy ? this.strategyName(p.ScopeStrategy) : "";
      },

      subjectText(g) {
        return (g.Subject && g.Subject.Kind ? g.Subject.Kind + ":" : "") + (g.Subject ? g.Subject.ID : "");
      },

      // subjectOptions returns the id options for the subject kind chosen on the
      // grant form (principals / roles / groups), so a grant can never name a
      // subject that does not exist.
      subjectOptions(kind) {
        if (kind === "role") return this.refs.roles || [];
        if (kind === "group") return this.refs.groups || [];
        return this.refs.principals || [];
      },

      // strategyOptions are the recognised scope strategies (the leading token of
      // Permission.ScopeStrategy) plus a blank "any". Selecting one filters the
      // permission picker, making scope strategy selectable per grant at the point
      // the grant references a permission (scope strategy is a permission property
      // in the model, so it is chosen via the permission, not stored on the grant).
      strategyOptions() {
        return ["", "literal", "implicit", "inclusive", "exclusive"];
      },

      filteredPermissions() {
        const f = this.grantModal.strategyFilter;
        const perms = this.refs.permissions || [];
        if (!f) return perms;
        return perms.filter((p) => String(p.ScopeStrategy || "").split(";")[0].trim() === f);
      },

      openCreateGrant() {
        this.grantModal = {
          open: true,
          mode: "create",
          strategyFilter: "",
          form: {
            ID: "",
            AccountID: this.account,
            Subject: { Kind: "principal", ID: "" },
            PermissionID: "",
            Object: "account:" + this.account + "/",
            Effect: "allow",
          },
          saving: false,
          error: null,
        };
      },

      openEditGrant(g) {
        this.grantModal = {
          open: true,
          mode: "edit",
          strategyFilter: "",
          form: JSON.parse(JSON.stringify(g)),
          saving: false,
          error: null,
        };
      },

      async saveGrant() {
        const f = this.grantModal.form;
        this.grantModal.error = null;
        if (!f.ID || !f.AccountID || !f.Subject.ID || !f.PermissionID || !f.Object) {
          this.grantModal.error = { code: "APERTURE_INVALID_INPUT", msg: "Account, id, subject, permission and object are required." };
          return;
        }
        this.grantModal.saving = true;
        try {
          await rpcCall("PutGrant", { actor: this.actor(), entity_json: JSON.stringify(f) });
          this.grantModal.open = false;
          await this.loadGrants();
        } catch (e) {
          this.grantModal.error = { code: e.code, msg: e.message };
        } finally {
          this.grantModal.saving = false;
        }
      },

      askDeleteGrant(g) {
        this.confirm = { open: true, grant: g, saving: false, error: null };
      },

      async doDeleteGrant() {
        this.confirm.saving = true;
        this.confirm.error = null;
        try {
          await rpcCall("DeleteGrant", { actor: this.actor(), id: this.confirm.grant.ID });
          this.confirm.open = false;
          await this.loadGrants();
        } catch (e) {
          this.confirm.error = { code: e.code, msg: e.message };
        } finally {
          this.confirm.saving = false;
        }
      },

      // ================= TEMPLATES TAB =================

      async loadTemplates() {
        this.loading = true;
        this.error = null;
        try {
          this.templates = parseList(await rpcCall("ListTemplates", this._buildFilter(this.tmplFilters, this.tmplFilterMatch)));
        } catch (e) {
          this.templates = [];
          if (e.status !== 401) this.error = { code: e.code, msg: e.message };
        } finally {
          this.loading = false;
        }
      },

      // ---- template definition editor (system-admin) ----

      blankTemplate() {
        return {
          Name: "",
          Version: 1,
          Description: "",
          Params: [{ Name: "", Type: "segment", Description: "" }],
          Grants: [{ Subject: { Kind: "principal", ID: "" }, PermissionID: "", Object: "", Effect: "allow" }],
        };
      },

      openCreateTemplate() {
        this.tmplModal = { open: true, mode: "create", form: this.blankTemplate(), saving: false, error: null };
      },

      openEditTemplate(t) {
        const form = JSON.parse(JSON.stringify(t));
        form.Params = form.Params || [];
        form.Grants = form.Grants || [];
        this.tmplModal = { open: true, mode: "edit", form, saving: false, error: null };
      },

      addParam() {
        this.tmplModal.form.Params.push({ Name: "", Type: "segment", Description: "" });
      },
      removeParam(i) {
        this.tmplModal.form.Params.splice(i, 1);
      },
      addTemplateGrant() {
        this.tmplModal.form.Grants.push({ Subject: { Kind: "principal", ID: "" }, PermissionID: "", Object: "", Effect: "allow" });
      },
      removeTemplateGrant(i) {
        this.tmplModal.form.Grants.splice(i, 1);
      },

      async saveTemplate() {
        const f = this.tmplModal.form;
        this.tmplModal.error = null;
        if (!f.Name || !(f.Version >= 1) || (f.Grants || []).length === 0) {
          this.tmplModal.error = { code: "APERTURE_INVALID_INPUT", msg: "Name, a version of at least 1, and one grant are required." };
          return;
        }
        f.Version = parseInt(f.Version, 10);
        this.tmplModal.saving = true;
        try {
          await rpcCall("PutTemplate", { actor: this.actor(), entity_json: JSON.stringify(f) });
          this.tmplModal.open = false;
          await this.loadTemplates();
        } catch (e) {
          this.tmplModal.error = { code: e.code, msg: e.message };
        } finally {
          this.tmplModal.saving = false;
        }
      },

      askDeleteTemplate(t) {
        this.tmplConfirm = { open: true, tmpl: t, saving: false, error: null };
      },

      async doDeleteTemplate() {
        const t = this.tmplConfirm.tmpl;
        this.tmplConfirm.saving = true;
        this.tmplConfirm.error = null;
        try {
          await rpcCall("DeleteTemplate", { actor: this.actor(), name: t.Name, version: t.Version });
          this.tmplConfirm.open = false;
          await this.loadTemplates();
        } catch (e) {
          this.tmplConfirm.error = { code: e.code, msg: e.message };
        } finally {
          this.tmplConfirm.saving = false;
        }
      },

      // ---- template apply (+ preview + bulk) ----

      openApply(t, mode) {
        const params = {};
        (t.Params || []).forEach((p) => (params[p.Name] = ""));
        this.apply = {
          open: true,
          mode: mode || "single",
          name: t.Name,
          version: t.Version,
          template: t,
          params,
          prefix: "",
          targetAccount: this.account,
          subjectParam: this.guessSubjectParam(t),
          principals: [],
          preview: [],
          applied: [],
          applying: false,
          error: null,
        };
        this.recomputePreview();
      },

      // guessSubjectParam picks the param most likely to be the per-principal slot
      // for bulk provisioning: the first param referenced by any grant's
      // Subject.ID token.
      guessSubjectParam(t) {
        for (const g of t.Grants || []) {
          const m = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/.exec((g.Subject && g.Subject.ID) || "");
          if (m) return m[1];
        }
        return (t.Params && t.Params[0] && t.Params[0].Name) || "";
      },

      applyTemplateObj() {
        return this.apply.template || {};
      },

      // recomputePreview does the client-side substitution the transactional
      // ApplyTemplate has no dry-run for, so the operator sees the exact would-be
      // grants before committing. Single mode expands one param set; bulk mode
      // expands the bundle once per selected principal (what BulkPutGrants writes).
      recomputePreview() {
        const t = this.apply.template;
        if (!t) {
          this.apply.preview = [];
          return;
        }
        const prefix = this.apply.prefix || t.Name + "-v" + t.Version;
        const expandOne = (params, idPrefix) =>
          (t.Grants || []).map((g, i) => ({
            ID: idPrefix + "-" + i,
            AccountID: this.apply.targetAccount,
            Subject: { Kind: g.Subject.Kind, ID: substitute(g.Subject.ID, params) },
            PermissionID: g.PermissionID,
            Object: substitute(g.Object, params),
            Effect: g.Effect,
          }));

        if (this.apply.mode === "bulk") {
          const out = [];
          for (const pid of this.apply.principals) {
            const params = { ...this.apply.params, [this.apply.subjectParam]: pid };
            // Per-principal id prefix keeps bulk-written grant ids unique + idempotent.
            out.push(...expandOne(params, prefix + "-" + pid));
          }
          this.apply.preview = out;
        } else {
          this.apply.preview = expandOne(this.apply.params, prefix);
        }
      },

      // applyTemplate commits: single mode calls ApplyTemplate (one transactional
      // expansion, server-substituted, returns the applied grants); bulk mode
      // sends the client-expanded grants for all selected principals in ONE
      // BulkPutGrants (atomic — a partial failure rolls the whole batch back).
      async applyTemplate() {
        this.apply.error = null;
        this.apply.applied = [];
        this.apply.applying = true;
        try {
          if (this.apply.mode === "bulk") {
            if (this.apply.principals.length === 0) {
              throw Object.assign(new Error("Select at least one principal to provision."), { code: "APERTURE_INVALID_INPUT" });
            }
            const grants_json = this.apply.preview.map((g) => JSON.stringify(g));
            await rpcCall("BulkPutGrants", { actor: this.actor(), grants_json });
            this.apply.applied = this.apply.preview;
          } else {
            const resp = await rpcCall("ApplyTemplate", {
              actor: this.actor(),
              name: this.apply.name,
              version: parseInt(this.apply.version, 10) || 0,
              account: this.apply.targetAccount,
              params: this.apply.params,
              grant_id_prefix: this.apply.prefix || "",
            });
            this.apply.applied = parseList(resp);
          }
          await this.loadGrants();
        } catch (e) {
          this.apply.error = { code: e.code, msg: e.message };
        } finally {
          this.apply.applying = false;
        }
      },

      toggleBulkPrincipal(id) {
        const i = this.apply.principals.indexOf(id);
        if (i === -1) this.apply.principals.push(id);
        else this.apply.principals.splice(i, 1);
        this.recomputePreview();
      },

      // ================= DELEGATION TAB =================

      // rolesOf / groupsOf resolve the roles and groups the delegator sits in, so
      // held-grant filtering can include role- and group-subject grants the
      // principal effectively holds, not just direct principal grants.
      rolesOf(principal) {
        const p = (this.refs.principals || []).find((x) => x.ID === principal);
        return (p && p.RoleIDs) || [];
      },
      groupsOf(principal) {
        return (this.refs.groups || [])
          .filter((g) => (g.MemberPrincipalIDs || []).includes(principal))
          .map((g) => g.ID);
      },

      // heldGrants pragmatically approximates the delegator's effective allow
      // grants from the account grant list: direct principal-subject grants plus
      // grants on roles/groups the principal belongs to. The server enforces the
      // real subset + may-delegate + delegatable rule (engine.EffectiveGrants is
      // not a UI endpoint) — this filter only narrows what we OFFER; a bad bestow
      // still returns APERTURE_DELEGATION_* and we surface it.
      heldGrants() {
        const roles = this.rolesOf(this.principal);
        const groups = this.groupsOf(this.principal);
        return (this.rows || []).filter((g) => {
          if (g.Effect !== "allow") return false;
          const s = g.Subject || {};
          if (s.Kind === "principal" && s.ID === this.principal) return true;
          if (s.Kind === "role" && roles.includes(s.ID)) return true;
          if (s.Kind === "group" && groups.includes(s.ID)) return true;
          return false;
        });
      },

      isDelegatable(permId) {
        const p = (this.refs.permissions || []).find((x) => x.ID === permId);
        return !!(p && p.Delegatable);
      },

      // eligibleHeld narrows heldGrants to those on a DELEGATABLE permission — the
      // only grants a bestow could possibly be a subset of. It is client filtering
      // for a clean UX, never the authority: the server is the source of truth.
      eligibleHeld() {
        return this.heldGrants().filter((g) => this.isDelegatable(g.PermissionID));
      },

      // hasDelegateRight reports whether the delegator holds any may-delegate right
      // (a grant on a permission whose action is aperture.delegate). Shown as a
      // hint; the server enforces coverage of the specific object.
      hasDelegateRight() {
        const delegatePerms = (this.refs.permissions || [])
          .filter((p) => p.Action === "aperture.delegate")
          .map((p) => p.ID);
        return this.heldGrants().some((g) => delegatePerms.includes(g.PermissionID));
      },

      openBestow(held) {
        this.bestow = {
          open: true,
          selectedHeldId: held ? held.ID : "",
          form: {
            ID: "",
            AccountID: this.account,
            Subject: { Kind: "principal", ID: "" },
            PermissionID: held ? held.PermissionID : "",
            Object: held ? held.Object : "account:" + this.account + "/",
            Effect: "allow",
          },
          saving: false,
          error: null,
        };
      },

      onSelectHeld() {
        const held = this.eligibleHeld().find((g) => g.ID === this.bestow.selectedHeldId);
        if (held) {
          this.bestow.form.PermissionID = held.PermissionID;
          this.bestow.form.Object = held.Object;
        }
      },

      async doBestow() {
        const f = this.bestow.form;
        this.bestow.error = null;
        if (!f.ID || !f.Subject.ID || !f.PermissionID || !f.Object) {
          this.bestow.error = { code: "APERTURE_INVALID_INPUT", msg: "Grant id, grantee, permission and object are required." };
          return;
        }
        f.AccountID = this.account;
        this.bestow.saving = true;
        try {
          // delegator is overridden by the authenticated identity server-side.
          await rpcCall("Bestow", { grant_json: JSON.stringify(f) });
          this.bestow.open = false;
          await this.loadGrants();
        } catch (e) {
          this.bestow.error = { code: e.code, msg: e.message };
        } finally {
          this.bestow.saving = false;
        }
      },

      askRevoke(g) {
        this.revokeConfirm = { open: true, grant: g, saving: false, error: null };
      },

      async doRevoke() {
        this.revokeConfirm.saving = true;
        this.revokeConfirm.error = null;
        try {
          await rpcCall("Revoke", { grant_id: this.revokeConfirm.grant.ID });
          this.revokeConfirm.open = false;
          await this.loadGrants();
        } catch (e) {
          this.revokeConfirm.error = { code: e.code, msg: e.message };
        } finally {
          this.revokeConfirm.saving = false;
        }
      },
    };
  }

  window.grants = grants;
})();
