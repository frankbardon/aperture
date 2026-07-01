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
 * (action aperture.admin over system:schema, resolved against the active
 * account); a non-admin sees a read-only view with editing affordances hidden,
 * and any mutation that still reaches the API surfaces its APERTURE_* code + msg.
 */

const CRUD_TOKEN_KEY = "aperture.devToken";
const RPC_PREFIX = "/twirp/aperture.ApertureService/";
const ADMIN_ACTION = "aperture.admin";
const ADMIN_OBJECT = "system:schema"; // the system-tier authority anchor (authz.go)

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
    key: "principals",
    label: "Principals",
    singular: "principal",
    rpc: { list: "ListPrincipals", put: "PutPrincipal", del: "DeletePrincipal" },
    idField: "ID",
    columns: [
      { key: "ID", label: "Id", mono: true },
      { key: "Kind", label: "Kind", badge: true },
      { key: "Identity", label: "Identity", mono: true },
      { key: "DisplayName", label: "Display name" },
      { key: "RoleIDs", label: "Roles", list: true, mono: true },
    ],
    fields: [
      { key: "ID", label: "Id", widget: "text", required: true, mono: true, lockOnEdit: true, hint: "Stable identifier. Cannot change after creation." },
      { key: "Kind", label: "Kind", widget: "select", options: ["user", "machine"], required: true },
      { key: "Identity", label: "Identity", widget: "text", required: true, mono: true, placeholder: "user:alice", hint: "Canonical identity string, e.g. user:alice or machine:ci-bot." },
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
      { key: "ID", label: "Id", widget: "text", required: true, mono: true, lockOnEdit: true },
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
      { key: "ID", label: "Id", widget: "text", required: true, mono: true, lockOnEdit: true },
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
      { key: "ScopeStrategy", label: "Scope strategy", widget: "text", mono: true },
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
    account: "",
    canEdit: false,
    tierChecked: false,

    // Reference lists used to populate multi-select and select widgets. Kept
    // small (the whole model) and refreshed after each mutation.
    refs: { roles: [], permissions: [], objectTypes: [], principals: [] },

    modal: { open: false, mode: "create", form: {}, saving: false, error: null },
    confirm: { open: false, row: null, message: "", deleting: false, error: null },

    init() {
      this.principal = localStorage.getItem(CRUD_TOKEN_KEY) || "";
      document.addEventListener("aperture:authenticated", (e) => {
        this.principal = (e.detail && e.detail.principal) || localStorage.getItem(CRUD_TOKEN_KEY) || "";
        this.bootstrap();
      });
      const clear = () => {
        this.principal = "";
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

    // bootstrap resolves the active account, probes admin authority, loads the
    // reference lists, then lists the active entity.
    async bootstrap() {
      await this.loadAccounts();
      await this.probeTier();
      await this.loadRefs();
      await this.load();
    },

    async loadAccounts() {
      try {
        const resp = await rpc("ListAccounts", {});
        this.accounts = (resp.entities_json || []).map((s) => JSON.parse(s));
        if (!this.account && this.accounts.length > 0) {
          this.account = this.accounts[0].ID;
        }
      } catch (e) {
        // A 401 is handled globally (sign-in re-opens); other errors are shown.
        if (e.status !== 401) this.error = { code: e.code, msg: e.message };
      }
    },

    // probeTier asks the open Check RPC whether the signed-in principal holds
    // system-admin authority, gating the editing affordances. It carries the
    // bearer but requires no auth, so a read-only viewer still gets an answer.
    async probeTier() {
      this.tierChecked = false;
      try {
        const dec = await rpc("Check", {
          account: this.account,
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
      const jobs = [
        ["roles", "ListRoles"],
        ["permissions", "ListPermissions"],
        ["objectTypes", "ListObjectTypes"],
        ["principals", "ListPrincipals"],
      ];
      for (const [key, method] of jobs) {
        try {
          const resp = await rpc(method, {});
          this.refs[key] = (resp.entities_json || []).map((s) => JSON.parse(s));
        } catch (_) {
          this.refs[key] = [];
        }
      }
    },

    async selectType(key) {
      this.activeType = key;
      await this.load();
    },

    async onAccountChange() {
      await this.probeTier();
      await this.loadRefs();
      await this.load();
    },

    async load() {
      if (!this.principal) return;
      this.loading = true;
      this.error = null;
      try {
        const resp = await rpc(this.currentType.rpc.list, {});
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

    setTagList(fieldKey, text) {
      this.modal.form[fieldKey] = text
        .split(",")
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
    },

    // ---- create / edit ----

    openCreate() {
      this.modal = { open: true, mode: "create", form: this.currentType.blank(), saving: false, error: null };
    },

    openEdit(row) {
      // Deep-clone so an aborted edit does not mutate the visible row.
      this.modal = { open: true, mode: "edit", form: JSON.parse(JSON.stringify(row)), saving: false, error: null };
    },

    closeModal() {
      this.modal.open = false;
    },

    async save() {
      this.modal.error = null;
      const type = this.currentType;
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
      return { principal: this.principal, account: this.account };
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
