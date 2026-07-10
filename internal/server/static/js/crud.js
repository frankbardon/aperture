/*
 * crud.js — Aperture model-management screens (E6-S2), the "Model" section of
 * the admin shell. A single, data-driven Alpine component drives list + detail +
 * create/edit/delete for all six schema entities (principals, roles, groups,
 * permissions, object types) rather than six copy-pasted screens: each entity is
 * described by a config (list columns, form fields, RPC method names) and the
 * one component renders + mutates them uniformly.
 *
 * Every API call goes through window.apiFetch (the shell's bearer wrapper) to the
 * Twirp JSON surface — never fetch directly, never storage. Entity bodies ride as
 * `entity_json` (the encoding/json form of the model.* structs, so keys are ID,
 * Kind, RoleIDs, PermissionIDs, MemberPrincipalIDs, Actions, …). Twirp responses
 * use proto snake_case field names (entities_json / entity_json).
 *
 * Auth + tiers: all six entity types here are SYSTEM-tier schema entities, so
 * editing requires system-admin authority. We probe it with the OPEN Check RPC
 * (action aperture.admin over system:schema); this is a global, account-
 * independent check that resolves system-admin via the platform "*" grant, so
 * the operator sees edit controls on load with no account context. A non-admin
 * sees a read-only view with editing affordances hidden, and any mutation that
 * still reaches the API surfaces its APERTURE_* code + msg.
 */

const CRUD_TOKEN_KEY = "aperture.devToken";
const RPC_PREFIX = "/twirp/aperture.ApertureService/";
const ADMIN_ACTION = "aperture.admin";
const ADMIN_OBJECT = "system:schema"; // the system-tier authority anchor (authz.go)
// System-admin authority is an ordinary account-scoped grant, resolved in the
// actor's active account — so both the probe and every schema mutation must name
// one. The platform "*" account (model.AccountWildcard) is where the super-admin
// grant lives, so system-tier work resolves against it.
const ADMIN_ACCOUNT = "*";

// rpc POSTs a Twirp JSON call through the shared bearer wrapper and returns the
// decoded response. A non-2xx carries a Twirp error body
// ({code, msg, meta:{code}}) which we normalise into an Error with .code (the
// canonical APERTURE_* code) + .message so callers can surface it legibly.
async function rpc(method, body) {
  const res = await window.apiFetch(RPC_PREFIX + method, {
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

// TYPES is the whole model-management surface as data: one entry per entity,
// naming its RPC methods, its list columns, and its form fields.
const TYPES = [
  {
    key: "accounts",
    label: "Accounts",
    singular: "account",
    rpc: { list: "ListAccounts", put: "PutAccount", del: "DeleteAccount" },
    idField: "ID",
    // manageMembers adds a per-row "Members" action that opens the account's
    // membership editor (principals become members via PutMembership).
    manageMembers: true,
    columns: [
      { key: "ID", label: "Id", mono: true },
      { key: "Name", label: "Name" },
      { key: "Description", label: "Description" },
    ],
    fields: [
      { key: "ID", label: "Id", widget: "slug", required: true, mono: true, lockOnEdit: true, placeholder: "acme", hint: "Stable identifier, a lower-case slug. Cannot change after creation." },
      { key: "Name", label: "Name", widget: "text", required: true },
      { key: "Description", label: "Description", widget: "textarea" },
    ],
    blank: () => ({ ID: "", Name: "", Description: "" }),
  },
  {
    key: "principals",
    label: "Principals",
    singular: "principal",
    rpc: { list: "ListPrincipals", put: "PutPrincipal", del: "DeletePrincipal" },
    idField: "ID",
    // membership renders an "Accounts" checklist on the form; the principal's
    // account memberships are reconciled (Put/DeleteMembership) after the
    // principal itself is saved.
    membership: true,
    columns: [
      { key: "ID", label: "Id", mono: true },
      { key: "Kind", label: "Kind", badge: true },
      { key: "Identity", label: "Identity", mono: true },
      { key: "DisplayName", label: "Display name" },
      { key: "RoleIDs", label: "Roles", list: true, mono: true },
    ],
    fields: [
      { key: "ID", label: "Id", widget: "slug", required: true, mono: true, lockOnEdit: true, placeholder: "alice", hint: "Stable identifier, a lower-case slug. Cannot change after creation." },
      { key: "Kind", label: "Kind", widget: "select", options: ["user", "machine"], required: true },
      { key: "Identity", label: "Identity", widget: "derived", mono: true, compute: (f) => (f.Kind || "") + ":" + (f.ID || ""), hint: "Canonical identity, built from the kind and id." },
      { key: "DisplayName", label: "Display name", widget: "text" },
      { key: "RoleIDs", label: "Roles", widget: "refchecks", ref: "roles", refValue: "ID", refLabel: "Name" },
    ],
    blank: () => ({ ID: "", Kind: "user", Identity: "", DisplayName: "", RoleIDs: [] }),
  },
  {
    key: "roles",
    label: "Roles",
    singular: "role",
    rpc: { list: "ListRoles", put: "PutRole", del: "DeleteRole" },
    idField: "ID",
    columns: [
      { key: "ID", label: "Id", mono: true },
      { key: "Name", label: "Name" },
      { key: "PermissionIDs", label: "Permissions", list: true, mono: true },
      { key: "Description", label: "Description" },
    ],
    fields: [
      { key: "ID", label: "Id", widget: "slug", required: true, mono: true, lockOnEdit: true, placeholder: "editor", hint: "Stable identifier, a lower-case slug. Cannot change after creation." },
      { key: "Name", label: "Name", widget: "text", required: true },
      { key: "Description", label: "Description", widget: "textarea" },
      { key: "PermissionIDs", label: "Permissions", widget: "refchecks", ref: "permissions", refValue: "ID", refLabel: "ID" },
    ],
    blank: () => ({ ID: "", Name: "", Description: "", PermissionIDs: [] }),
  },
  {
    key: "groups",
    label: "Groups",
    singular: "group",
    rpc: { list: "ListGroups", put: "PutGroup", del: "DeleteGroup" },
    idField: "ID",
    columns: [
      { key: "ID", label: "Id", mono: true },
      { key: "Name", label: "Name" },
      { key: "MemberPrincipalIDs", label: "Members", list: true, mono: true },
      { key: "Description", label: "Description" },
    ],
    fields: [
      { key: "ID", label: "Id", widget: "slug", required: true, mono: true, lockOnEdit: true, placeholder: "engineering", hint: "Stable identifier, a lower-case slug. Cannot change after creation." },
      { key: "Name", label: "Name", widget: "text", required: true },
      { key: "Description", label: "Description", widget: "textarea" },
      { key: "MemberPrincipalIDs", label: "Members", widget: "refchecks", ref: "principals", refValue: "ID", refLabel: "ID" },
    ],
    blank: () => ({ ID: "", Name: "", Description: "", MemberPrincipalIDs: [] }),
  },
  {
    key: "permissions",
    label: "Permissions",
    singular: "permission",
    rpc: { list: "ListPermissions", put: "PutPermission", del: "DeletePermission" },
    idField: "ID",
    columns: [
      { key: "ID", label: "Id", mono: true },
      { key: "ObjectType", label: "Object type", mono: true },
      { key: "Action", label: "Action", mono: true },
      { key: "ScopeStrategy", label: "Scope strategy", mono: true },
      { key: "Delegatable", label: "Delegatable", bool: true },
    ],
    fields: [
      { key: "ID", label: "Id", widget: "text", required: true, mono: true, lockOnEdit: true },
      { key: "ObjectType", label: "Object type", widget: "refselect", ref: "objectTypes", refValue: "Name", refLabel: "Name", required: true },
      { key: "Action", label: "Action", widget: "actionselect", required: true, hint: "Must be an action the selected object type declares." },
      { key: "ScopeStrategy", label: "Scope strategy", widget: "scopestrategy", mono: true, hint: "How this permission's grants pick objects inside the grant pattern." },
      { key: "Delegatable", label: "Delegatable", widget: "checkbox" },
      { key: "Description", label: "Description", widget: "textarea" },
    ],
    blank: () => ({ ID: "", ObjectType: "", Action: "", ScopeStrategy: "", Delegatable: false, Description: "" }),
  },
  {
    key: "objectTypes",
    label: "Object types",
    singular: "object type",
    rpc: { list: "ListObjectTypes", put: "PutObjectType", del: "DeleteObjectType" },
    idField: "Name",
    columns: [
      { key: "Name", label: "Name", mono: true },
      { key: "Actions", label: "Actions", list: true, mono: true },
      { key: "Description", label: "Description" },
    ],
    fields: [
      { key: "Name", label: "Name", widget: "text", required: true, mono: true, lockOnEdit: true },
      { key: "Description", label: "Description", widget: "textarea" },
      { key: "Actions", label: "Actions", widget: "taglist", mono: true, hint: "Comma-separated action verbs, e.g. read, write, delete." },
    ],
    blank: () => ({ Name: "", Description: "", Actions: [] }),
  },
];

function crud() {
  return {
    types: TYPES,
    activeType: "principals",
    rows: [],
    loading: false,
    error: null,

    principal: "",
    accounts: [],
    canEdit: false,
    tierChecked: false,

    // Reference lists used to populate multi-select and select widgets. Kept
    // small (the whole model) and refreshed after each mutation.
    refs: { roles: [], permissions: [], objectTypes: [], principals: [], rules: [] },

    // Server-side grid filter: predicate rows + match mode. See selectType/load.
    filters: [],
    filterMatch: "all",

    modal: { open: false, mode: "create", form: {}, saving: false, error: null, tagText: {}, scopeSel: {}, memberSel: {}, memberOrig: {}, memberLoading: false },
    confirm: { open: false, row: null, message: "", deleting: false, error: null },

    // members is the account-centric membership editor: pick which principals
    // belong to the chosen account. Its counterpart, the principal-centric
    // "Accounts" checklist, lives on the create/edit modal (modal.memberSel).
    members: { open: false, account: null, selected: {}, original: {}, loading: false, saving: false, error: null },

    init() {
      this.principal = localStorage.getItem(CRUD_TOKEN_KEY) || "";
      document.addEventListener("aperture:authenticated", (e) => {
        this.principal = (e.detail && e.detail.principal) || localStorage.getItem(CRUD_TOKEN_KEY) || "";
        this.bootstrap();
      });
      const clear = () => {
        this.principal = "";
        this.accounts = [];
        this.rows = [];
        this.canEdit = false;
        this.tierChecked = false;
      };
      document.addEventListener("aperture:unauthenticated", clear);
      document.addEventListener("aperture:signout", clear);
      if (this.principal) {
        this.bootstrap();
      }
    },

    get currentType() {
      return this.types.find((t) => t.key === this.activeType) || this.types[0];
    },

    // bootstrap probes admin authority, loads the reference lists, then lists the
    // active entity. These are global schema entities, so nothing here depends on
    // an account.
    async bootstrap() {
      await this.probeTier();
      await this.loadRefs();
      await this.load();
    },

    // probeTier asks the open Check RPC whether the signed-in principal holds
    // system-admin authority, gating the editing affordances. The check is global
    // and account-independent — it resolves system-admin via the platform "*"
    // grant, so the operator sees edit controls on load with no account context.
    // It carries the bearer but requires no auth, so a read-only viewer still gets
    // an answer.
    async probeTier() {
      this.tierChecked = false;
      try {
        const dec = await rpc("Check", {
          account: ADMIN_ACCOUNT,
          principal: this.principal,
          action: ADMIN_ACTION,
          object: ADMIN_OBJECT,
        });
        this.canEdit = !!dec.allow;
      } catch (_) {
        this.canEdit = false;
      }
      this.tierChecked = true;
    },

    async loadRefs() {
      // Most reference lists return an EntityListResponse (entities_json); ListRules
      // returns a RuleListResponse whose field is rules_json, so name the response
      // field per job rather than assuming entities_json for all.
      const jobs = [
        ["roles", "ListRoles", "entities_json"],
        ["permissions", "ListPermissions", "entities_json"],
        ["objectTypes", "ListObjectTypes", "entities_json"],
        ["principals", "ListPrincipals", "entities_json"],
        ["rules", "ListRules", "rules_json"],
      ];
      for (const [key, method, field] of jobs) {
        try {
          const resp = await rpc(method, {});
          this.refs[key] = (resp[field] || []).map((s) => JSON.parse(s));
        } catch (_) {
          this.refs[key] = [];
        }
      }
      // Accounts drive the membership widgets and the active-account selector, so
      // refresh them alongside the other reference lists after every mutation.
      await this.loadAccounts();
    },

    // loadAccounts populates THIS screen's account list (membership widgets)
    // directly from ListAccounts. It is account-agnostic: the list drives the
    // membership editors, not any active-account concept.
    async loadAccounts() {
      try {
        const resp = await rpc("ListAccounts", {});
        this.accounts = (resp.entities_json || []).map((s) => JSON.parse(s));
      } catch (_) {
        this.accounts = [];
      }
    },

    async selectType(key) {
      this.activeType = key;
      this.clearFilters(); // filter fields are per-type; don't carry them across
      await this.load();
    },

    // ---- server-side filtering ----
    // filters is a list of {field, op, value} predicate rows; filterMatch is
    // "all" (AND) or "any" (OR). They are sent to the List RPC so the SERVER
    // returns only matching rows (see filter/filter.go).

    addFilter() {
      const col = (this.currentType.columns[0] || {}).key || "";
      this.filters.push({ field: col, op: "contains", value: "" });
    },
    removeFilter(i) {
      this.filters.splice(i, 1);
      this.load();
    },
    clearFilters() {
      this.filters = [];
      this.filterMatch = "all";
    },
    // buildFilterArg turns the predicate rows into the RPC request body. Rows with
    // no field, or a value-taking op with an empty value, are dropped.
    buildFilterArg() {
      const preds = this.filters
        .filter((f) => f.field && (f.op === "empty" || f.value !== ""))
        .map((f) => ({ field: f.field, op: f.op, value: f.op === "empty" ? "" : f.value }));
      if (preds.length === 0) return {};
      return { filter: { match: this.filterMatch, predicates: preds } };
    },
    filterNeedsValue(op) {
      return op !== "empty";
    },

    async load() {
      if (!this.principal) return;
      this.loading = true;
      this.error = null;
      try {
        const resp = await rpc(this.currentType.rpc.list, this.buildFilterArg());
        this.rows = (resp.entities_json || []).map((s) => JSON.parse(s));
      } catch (e) {
        this.rows = [];
        if (e.status !== 401) this.error = { code: e.code, msg: e.message };
      } finally {
        this.loading = false;
      }
    },

    rowId(row) {
      return row[this.currentType.idField];
    },

    emptyText() {
      return "No " + this.currentType.label.toLowerCase() + " yet.";
    },

    // cellText renders a list value's cell: arrays join, booleans read Yes/No,
    // and empties render an em dash so the table stays scannable.
    cellText(row, col) {
      const v = row[col.key];
      if (col.bool) return v ? "Yes" : "No";
      if (col.list) {
        const arr = v || [];
        return arr.length ? arr.join(", ") : "—";
      }
      return v === "" || v === null || v === undefined ? "—" : String(v);
    },

    // ---- reference option helpers (data-driven form widgets) ----

    refOptions(field) {
      return (this.refs[field.ref] || []).map((r) => ({
        value: r[field.refValue],
        label: r[field.refLabel] || r[field.refValue],
      }));
    },

    // actionOptions returns the declared verbs of the object type currently
    // chosen on the permission form — client-side typed-action validation that
    // mirrors the server's APERTURE_ACTION_UNDECLARED check.
    actionOptions() {
      const ot = (this.refs.objectTypes || []).find((o) => o.Name === this.modal.form.ObjectType);
      return (ot && ot.Actions) || [];
    },

    toggleRef(fieldKey, value) {
      const cur = this.modal.form[fieldKey] || [];
      const i = cur.indexOf(value);
      if (i === -1) cur.push(value);
      else cur.splice(i, 1);
      this.modal.form[fieldKey] = cur;
    },

    // Tag-list fields (e.g. ObjectType.Actions) edit as free text: the input is
    // bound to a raw string buffer (modal.tagText) so the user can type commas
    // and spaces freely. The buffer is parsed into the form's string array only
    // at save time — NOT on every keystroke. Deriving the input value from the
    // parsed array on each keystroke (the old approach) erased the comma the user
    // just typed and jumped the cursor, making a second entry impossible.
    seedTagText(form) {
      const tagText = {};
      for (const f of this.currentType.fields || []) {
        if (f.widget === "taglist") {
          tagText[f.key] = (form[f.key] || []).join(", ");
        }
      }
      return tagText;
    },

    parseTags(text) {
      return (text || "")
        .split(",")
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
    },

    // commitTagLists folds each raw tag buffer back into its form array. Called
    // once at save time.
    commitTagLists() {
      for (const f of this.currentType.fields || []) {
        if (f.widget === "taglist") {
          this.modal.form[f.key] = this.parseTags(this.modal.tagText[f.key]);
        }
      }
    },

    // Slug fields (e.g. every stable ID) are constrained to a continuous
    // lower-case slug so identifiers stay URL- and path-safe. slugLive runs on
    // each keystroke: it lower-cases and collapses any run of non-alphanumerics
    // to a single "-", but keeps a lone trailing "-" so a user mid-typing
    // "team-" can still add the next word. slugFinal drops that trailing "-".
    slugLive(s) {
      return (s || "").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+/, "");
    },

    slugFinal(s) {
      return this.slugLive(s).replace(/-+$/, "");
    },

    commitSlugs() {
      for (const f of this.currentType.fields || []) {
        if (f.widget === "slug") {
          this.modal.form[f.key] = this.slugFinal(this.modal.form[f.key]);
        }
      }
    },

    // Derived fields (e.g. a principal's Identity) are never typed: their value
    // is computed from other form fields via field.compute. derivedValue drives
    // the read-only display; commitDerived folds the computed value into the form
    // at save time, after commitSlugs so it sees the finalized id.
    derivedValue(field) {
      return field.compute ? field.compute(this.modal.form) : "";
    },

    commitDerived() {
      for (const f of this.currentType.fields || []) {
        if (f.widget === "derived") {
          this.modal.form[f.key] = this.derivedValue(f);
        }
      }
    },

    // ---- scope strategy (guided editor over the opaque ScopeStrategy string) ----
    // The stored value is a reference string the engine parses: "" (literal),
    // "implicit", "inclusive;ids=a,b", "inclusive;rule=vip", "exclusive;ids=...".
    // The form edits it as structured state (modal.scopeSel) and assembles the
    // string at save time so operators never hand-write the grammar.

    // The built-in strategies the shipped engine registers (scope.DefaultRegistry
    // plus the natively-handled literal default).
    scopeStrategyOptions() {
      return [
        { value: "literal", label: "Literal — exactly the grant pattern (default)" },
        { value: "implicit", label: "Implicit — every object of the type in the pattern" },
        { value: "inclusive", label: "Inclusive — only listed objects (or a rule)" },
        { value: "exclusive", label: "Exclusive — all of the type except listed (or a rule)" },
      ];
    },

    // scopeNeedsSelection reports whether the chosen strategy takes an id-list or
    // rule (inclusive/exclusive do; literal/implicit do not).
    scopeNeedsSelection(strategy) {
      return strategy === "inclusive" || strategy === "exclusive";
    },

    ruleOptions() {
      // Scope references a rule by its Name (scope grammar: "…;rule=<name>"), and
      // model.Rule has no ID — so both option value and label key off Name.
      return (this.refs.rules || []).map((r) => ({ value: r.Name, label: r.Description ? r.Name + " — " + r.Description : r.Name }));
    },

    // parseScopeRef turns a stored reference string into editor state. It mirrors
    // scope.ParseSpec: a strategy key followed by ";"-separated name=value params.
    parseScopeRef(ref) {
      const out = { strategy: "literal", ids: "", rule: "" };
      const trimmed = (ref || "").trim();
      if (trimmed === "") return out;
      const parts = trimmed.split(";");
      out.strategy = parts[0].trim() || "literal";
      for (const p of parts.slice(1)) {
        const eq = p.indexOf("=");
        if (eq === -1) continue;
        const name = p.slice(0, eq).trim();
        const value = p.slice(eq + 1).trim();
        if (name === "ids") out.ids = value;
        else if (name === "rule") out.rule = value;
      }
      return out;
    },

    // buildScopeRef assembles the stored string from editor state. A rule takes
    // precedence over an id-list; literal collapses to the empty (default) string.
    buildScopeRef(sel) {
      const strategy = (sel && sel.strategy) || "literal";
      if (strategy === "literal") return "";
      if (!this.scopeNeedsSelection(strategy)) return strategy;
      const rule = (sel.rule || "").trim();
      if (rule) return strategy + ";rule=" + rule;
      const ids = (sel.ids || "")
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean)
        .join(",");
      return ids ? strategy + ";ids=" + ids : strategy;
    },

    seedScopeSel(form) {
      const sel = {};
      for (const f of this.currentType.fields || []) {
        if (f.widget === "scopestrategy") sel[f.key] = this.parseScopeRef(form[f.key]);
      }
      return sel;
    },

    commitScopeStrategies() {
      for (const f of this.currentType.fields || []) {
        if (f.widget === "scopestrategy") {
          this.modal.form[f.key] = this.buildScopeRef(this.modal.scopeSel[f.key]);
        }
      }
    },

    // ---- account membership ----
    // Membership is a (principal, account) join with no dedicated read RPC, so
    // the current edges are read from the Export document and written back as
    // individual PutMembership / DeleteMembership upserts. Export is system-admin
    // only, so a non-system-admin gets an empty view (they cannot enumerate the
    // whole membership graph) — the read degrades to [] rather than erroring.

    async allMemberships() {
      try {
        const resp = await rpc("Export", { actor: this.actor() });
        const doc = JSON.parse(resp.document_json || "{}");
        return doc.memberships || [];
      } catch (_) {
        return [];
      }
    },

    // reconcile persists the difference between a desired membership set and the
    // set present at open. `pairs` are { principal, account } objects.
    async reconcileMemberships(toAdd, toRemove) {
      for (const p of toAdd) {
        await rpc("PutMembership", { actor: this.actor(), entity_json: JSON.stringify({ PrincipalID: p.principal, AccountID: p.account }) });
      }
      for (const p of toRemove) {
        await rpc("DeleteMembership", { actor: this.actor(), principal_id: p.principal, account_id: p.account });
      }
    },

    // ---- account-centric member editor ----

    async openMembers(account) {
      this.members = { open: true, account, selected: {}, original: {}, loading: true, saving: false, error: null };
      try {
        const all = await this.allMemberships();
        const sel = {};
        for (const m of all) if (m.account === account.ID) sel[m.principal] = true;
        this.members.selected = sel;
        this.members.original = { ...sel };
      } catch (e) {
        if (e.status !== 401) this.members.error = { code: e.code, msg: e.message };
      } finally {
        this.members.loading = false;
      }
    },

    toggleMember(principalID) {
      const cur = { ...this.members.selected };
      if (cur[principalID]) delete cur[principalID];
      else cur[principalID] = true;
      this.members.selected = cur;
    },

    async saveMembers() {
      const acct = this.members.account.ID;
      const sel = this.members.selected;
      const orig = this.members.original;
      const toAdd = Object.keys(sel).filter((p) => !orig[p]).map((p) => ({ principal: p, account: acct }));
      const toRemove = Object.keys(orig).filter((p) => !sel[p]).map((p) => ({ principal: p, account: acct }));
      this.members.saving = true;
      this.members.error = null;
      try {
        await this.reconcileMemberships(toAdd, toRemove);
        this.members.open = false;
      } catch (e) {
        this.members.error = { code: e.code, msg: e.message };
      } finally {
        this.members.saving = false;
      }
    },

    // ---- principal-centric account checklist (on the create/edit modal) ----

    // seedModalMemberships loads the accounts a principal already belongs to so
    // the form's "Accounts" checklist reflects reality. New principals start empty.
    async seedModalMemberships(principalID) {
      if (!principalID) {
        this.modal.memberSel = {};
        this.modal.memberOrig = {};
        return;
      }
      this.modal.memberLoading = true;
      try {
        const all = await this.allMemberships();
        const sel = {};
        for (const m of all) if (m.principal === principalID) sel[m.account] = true;
        this.modal.memberSel = sel;
        this.modal.memberOrig = { ...sel };
      } catch (_) {
        // Non-fatal: leave the checklist empty rather than block the edit.
      } finally {
        this.modal.memberLoading = false;
      }
    },

    toggleModalAccount(accountID) {
      const cur = { ...this.modal.memberSel };
      if (cur[accountID]) delete cur[accountID];
      else cur[accountID] = true;
      this.modal.memberSel = cur;
    },

    // ---- create / edit ----

    openCreate() {
      const form = this.currentType.blank();
      this.modal = { open: true, mode: "create", form, saving: false, error: null, tagText: this.seedTagText(form), scopeSel: this.seedScopeSel(form), memberSel: {}, memberOrig: {}, memberLoading: false };
    },

    openEdit(row) {
      // Deep-clone so an aborted edit does not mutate the visible row.
      const form = JSON.parse(JSON.stringify(row));
      this.modal = { open: true, mode: "edit", form, saving: false, error: null, tagText: this.seedTagText(form), scopeSel: this.seedScopeSel(form), memberSel: {}, memberOrig: {}, memberLoading: false };
      // Membership types load the principal's current accounts asynchronously.
      if (this.currentType.membership) this.seedModalMemberships(form[this.currentType.idField]);
    },

    closeModal() {
      this.modal.open = false;
    },

    async save() {
      this.modal.error = null;
      const type = this.currentType;
      // Fold free-text tag buffers (e.g. Actions) into their form arrays first.
      this.commitTagLists();
      // Finalize slug ids (strip any trailing "-"), then compute derived fields
      // (e.g. Identity = kind:id) from the finalized values.
      this.commitSlugs();
      this.commitDerived();
      // Assemble guided scope-strategy editors into their reference string.
      this.commitScopeStrategies();
      for (const f of type.fields) {
        if (f.required) {
          const v = this.modal.form[f.key];
          if (v === "" || v === null || v === undefined) {
            this.modal.error = { code: "APERTURE_INVALID_INPUT", msg: f.label + " is required." };
            return;
          }
        }
      }
      this.modal.saving = true;
      try {
        await rpc(type.rpc.put, { actor: this.actor(), entity_json: JSON.stringify(this.modal.form) });
        // Reconcile account membership after the principal itself is saved, so a
        // brand-new principal exists before any PutMembership references it.
        if (type.membership) {
          const pid = this.modal.form[type.idField];
          const sel = this.modal.memberSel || {};
          const orig = this.modal.memberOrig || {};
          const toAdd = Object.keys(sel).filter((a) => !orig[a]).map((a) => ({ principal: pid, account: a }));
          const toRemove = Object.keys(orig).filter((a) => !sel[a]).map((a) => ({ principal: pid, account: a }));
          await this.reconcileMemberships(toAdd, toRemove);
        }
        this.closeModal();
        await this.load();
        await this.loadRefs();
      } catch (e) {
        this.modal.error = { code: e.code, msg: e.message };
      } finally {
        this.modal.saving = false;
      }
    },

    actor() {
      // Global schema mutations resolve against system-admin authority in the
      // platform "*" account (where the super-admin grant lives), so the actor
      // must name it — the authz gate rejects an empty actor account. Membership
      // upserts pass their AccountID in the entity body, not via the actor.
      return { principal: this.principal, account: ADMIN_ACCOUNT };
    },

    // ---- delete ----

    askDelete(row) {
      this.confirm = { open: true, row, message: this.confirmMessage(row), deleting: false, error: null };
    },

    // confirmMessage states the world: for entities whose removal ripples, it
    // reports the reference count cheaply from the already-loaded ref lists.
    confirmMessage(row) {
      const t = this.currentType.key;
      if (t === "roles") {
        const n = (this.refs.principals || []).filter((p) => (p.RoleIDs || []).includes(row.ID)).length;
        return 'Delete role "' + (row.Name || row.ID) + '"? This removes it from ' + n + " principal(s).";
      }
      if (t === "groups") {
        const n = (row.MemberPrincipalIDs || []).length;
        return 'Delete group "' + (row.Name || row.ID) + '"? It has ' + n + " member(s).";
      }
      if (t === "permissions") {
        const n = (this.refs.roles || []).filter((r) => (r.PermissionIDs || []).includes(row.ID)).length;
        return 'Delete permission "' + row.ID + '"? It is referenced by ' + n + " role(s).";
      }
      if (t === "objectTypes") {
        const n = (this.refs.permissions || []).filter((p) => p.ObjectType === row.Name).length;
        return 'Delete object type "' + row.Name + '"? ' + n + " permission(s) reference it.";
      }
      // principals
      const label = row.DisplayName ? row.DisplayName + " (" + row.ID + ")" : row.ID;
      return 'Delete principal "' + label + '"?';
    },

    async doDelete() {
      this.confirm.deleting = true;
      this.confirm.error = null;
      try {
        await rpc(this.currentType.rpc.del, { actor: this.actor(), id: this.rowId(this.confirm.row) });
        this.confirm.open = false;
        await this.load();
        await this.loadRefs();
      } catch (e) {
        this.confirm.error = { code: e.code, msg: e.message };
      } finally {
        this.confirm.deleting = false;
      }
    },
  };
}

window.crud = crud;
