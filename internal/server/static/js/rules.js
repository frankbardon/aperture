/*
 * rules.js — the Rules section: a Blueprints-style node editor over the E2-S3
 * pulse-expression rule AST (E7-S2).
 *
 * This replaces the E7-S1 hello-canvas. It builds the real node<->AST editor on
 * the toolkit the vendored Rete bundle re-exports (NodeEditor, ClassicPreset,
 * AreaPlugin, AreaExtensions, ConnectionPlugin/Presets, ReactPlugin/Presets),
 * imported at runtime from /vendor/rete/rete.min.js (no node build in dev/CI).
 *
 * The editor is ONLY an editing surface. All graph<->AST correctness lives in
 * the pure, DOM-free serializer (rules-serializer.js, window.RuleSerializer):
 * the Rete graph is read into that module's plain graph model and folded to the
 * exact rules.Node JSON — there is no second rule format.
 *
 * Alpine holds the section state and exposes the E7-S3 save/load HOOKS on
 * `window.blueprintEditor`:
 *
 *   window.blueprintEditor.toAST()        -> rule AST (rules.Node JSON) | null
 *   window.blueprintEditor.fromAST(ast)   -> Promise (render an AST into the graph)
 *   window.blueprintEditor.validate()     -> [{ code, message, path }]  (client-side)
 *   window.blueprintEditor.getGraph()     -> plain { nodes, connections }
 *   window.blueprintEditor.addNode(kind)  -> Promise (palette add)
 *   window.blueprintEditor.clear()        -> Promise
 *   window.blueprintEditor.destroy()
 *   window.blueprintEditor.onChange = fn  (set by the host; fires on any edit)
 *
 * E7-S3 wires POST-to-API load/save/validate over these hooks; the split is:
 * client-side validate() is structural/AST-shape only (mirrors ast.go Validate);
 * full type-checking is the engine's and runs server-side.
 */

// createBlueprintEditor mounts the Rete editor into `container` and returns the
// blueprintEditor hook object. `mod` is the vendored Rete bundle namespace and
// `S` is window.RuleSerializer.
async function createBlueprintEditor(container, mod, S) {
  const {
    NodeEditor,
    ClassicPreset,
    AreaPlugin,
    AreaExtensions,
    ConnectionPlugin,
    ConnectionPresets,
    ReactPlugin,
    ReactPresets,
  } = mod;

  const editor = new NodeEditor();
  const area = new AreaPlugin(container);
  const connection = new ConnectionPlugin();
  const render = new ReactPlugin({ createRoot: mod.createRoot || undefined });

  // The bundle re-exports React's createRoot indirectly through ReactPlugin's
  // own default when not supplied; pass it if the bundle exposed it.
  render.addPreset(ReactPresets.classic.setup());
  connection.addPreset(ConnectionPresets.classic.setup());

  editor.use(area);
  area.use(connection);
  area.use(render);

  AreaExtensions.simpleNodesOrder(area);
  AreaExtensions.selectableNodes(area, AreaExtensions.selector(), {
    accumulating: AreaExtensions.accumulateOnCtrl(),
  });

  // One shared socket instance: Rete's default compatibility then permits any
  // wire, and the typed-socket rules are enforced by the connectioncreate guard
  // below using the serializer's NODE_SPECS. Keeping one socket avoids the
  // classic preset silently refusing visually-identical pins.
  const socket = new ClassicPreset.Socket("pin");

  const hook = {
    editor,
    area,
    onChange: function () {},
  };

  // fireChange notifies the host (Alpine) that the graph changed so it can
  // re-run validation. Debounced to a microtask so a burst of Rete events
  // (e.g. clearing) collapses into one refresh.
  let pending = false;
  function fireChange() {
    if (pending) return;
    pending = true;
    Promise.resolve().then(function () {
      pending = false;
      try {
        hook.onChange();
      } catch (_) {
        /* host errors must not break the editor pipeline */
      }
    });
  }

  // Typed-socket guard + change notifications. Returning undefined from a
  // 'connectioncreate' cancels the connection (Rete pipe contract).
  editor.addPipe(function (context) {
    if (!context || typeof context !== "object") return context;
    if (context.type === "connectioncreate") {
      if (!isCompatible(context.data)) {
        return; // reject incompatible or self-referential wires
      }
    }
    if (
      context.type === "connectioncreated" ||
      context.type === "connectionremoved" ||
      context.type === "noderemoved" ||
      context.type === "nodecreated"
    ) {
      fireChange();
    }
    return context;
  });

  // isCompatible enforces the typed-socket rules from NODE_SPECS: a source
  // node's output type must be accepted by the target input, and a node may not
  // wire into itself.
  function isCompatible(data) {
    const src = editor.getNode(data.source);
    const tgt = editor.getNode(data.target);
    if (!src || !tgt) return false;
    if (src.id === tgt.id) return false;
    const outType = (S.NODE_SPECS[src.kind] || {}).out;
    const accepts = (S.NODE_SPECS[tgt.kind] || {}).accepts || [];
    return accepts.indexOf(outType) >= 0;
  }

  // makeNode builds a Rete node for an AST kind, seeding its controls from
  // optional `data` (op/name/value) and its variadic pins from `data.arity`.
  function makeNode(kind, data) {
    const spec = S.NODE_SPECS[kind];
    if (!spec) throw new Error("unknown node kind: " + kind);
    const d = data || {};
    const node = new ClassicPreset.Node(spec.title);
    node.kind = kind;

    node.addOutput("out", new ClassicPreset.Output(socket, spec.out));

    if (kind === S.TYPES.COMPARE) {
      node.addControl(
        "op",
        new ClassicPreset.InputControl("text", {
          initial: d.op || "eq",
          change: fireChange,
        })
      );
    }
    if (kind === S.TYPES.VAR) {
      node.addControl(
        "name",
        new ClassicPreset.InputControl("text", {
          initial: d.name || "object.",
          change: fireChange,
        })
      );
    }
    if (kind === S.TYPES.LITERAL) {
      node.addControl(
        "value",
        new ClassicPreset.InputControl("text", {
          initial: S.formatLiteral(d.value === undefined ? "" : d.value),
          change: fireChange,
        })
      );
    }
    if (kind === S.TYPES.CALL) {
      node.addControl(
        "name",
        new ClassicPreset.InputControl("text", {
          initial: d.name || "len",
          change: fireChange,
        })
      );
    }

    // Inputs.
    if (spec.inputs === "child") {
      node.addInput("in", new ClassicPreset.Input(socket, ""));
    } else if (spec.inputs === "leftright") {
      node.addInput("left", new ClassicPreset.Input(socket, "left"));
      node.addInput("right", new ClassicPreset.Input(socket, "right"));
    } else if (spec.inputs === "variadic") {
      const arity = Math.max(minArity(kind), d.arity || 0);
      node.arity = 0;
      for (let i = 0; i < arity; i++) {
        node.addInput("in-" + i, new ClassicPreset.Input(socket, "in " + i));
        node.arity++;
      }
      // A number control drives how many ordered input pins the node exposes;
      // order is meaningful for list/call arguments.
      node.addControl(
        "slots",
        new ClassicPreset.InputControl("number", {
          initial: node.arity,
          change: function (v) {
            adjustArity(node, v);
          },
        })
      );
    }
    return node;
  }

  function minArity(kind) {
    if (kind === S.TYPES.AND || kind === S.TYPES.OR) return 2;
    if (kind === S.TYPES.LIST) return 1;
    return 1; // call: allow zero-arg is possible, but keep one slot for editing
  }

  // adjustArity grows/shrinks a variadic node's ordered input pins, removing any
  // connections that fall off when shrinking.
  async function adjustArity(node, next) {
    next = Math.max(minArity(node.kind), Math.floor(next || 0));
    const cur = node.arity;
    if (next === cur) return;
    if (next > cur) {
      for (let i = cur; i < next; i++) {
        node.addInput("in-" + i, new ClassicPreset.Input(socket, "in " + i));
      }
    } else {
      for (let i = cur; i > next; i--) {
        const key = "in-" + (i - 1);
        const drop = editor.getConnections().filter(function (c) {
          return c.target === node.id && c.targetInput === key;
        });
        for (const c of drop) {
          await editor.removeConnection(c.id);
        }
        node.removeInput(key);
      }
    }
    node.arity = next;
    await area.update("node", node.id);
    fireChange();
  }

  // reteToGraph reads the live Rete editor into the serializer's plain graph
  // model — the ONLY bridge between the runtime and the pure serializer.
  function reteToGraph() {
    const nodes = editor.getNodes().map(function (n) {
      const g = { id: n.id, type: n.kind };
      if (n.controls.op) g.op = n.controls.op.value;
      if (n.kind === S.TYPES.VAR || n.kind === S.TYPES.CALL) {
        g.name = n.controls.name ? n.controls.name.value : "";
      }
      if (n.kind === S.TYPES.LITERAL) {
        g.value = S.parseLiteral(n.controls.value ? n.controls.value.value : "");
      }
      return g;
    });
    const connections = editor.getConnections().map(function (c) {
      return {
        source: c.source,
        sourceKey: c.sourceOutput,
        target: c.target,
        targetKey: c.targetInput,
      };
    });
    return { nodes: nodes, connections: connections };
  }

  async function clear() {
    for (const c of editor.getConnections().slice()) {
      await editor.removeConnection(c.id);
    }
    for (const n of editor.getNodes().slice()) {
      await editor.removeNode(n.id);
    }
  }

  // fromAST renders an AST into the canvas. Node ids/positions are editor
  // concerns produced by astToGraph and are dropped on the way back, so the
  // round-trip stays lossless.
  async function fromAST(ast) {
    await clear();
    const graph = S.astToGraph(ast);

    // Per-node variadic arity = count of its in-N connections.
    const arity = {};
    graph.connections.forEach(function (c) {
      if (/^in-\d+$/.test(c.targetKey)) {
        arity[c.target] = (arity[c.target] || 0) + 1;
      }
    });

    const idMap = {};
    for (const gn of graph.nodes) {
      const node = makeNode(gn.type, {
        op: gn.op,
        name: gn.name,
        value: gn.value,
        arity: arity[gn.id] || 0,
      });
      idMap[gn.id] = node;
      await editor.addNode(node);
      if (gn.position) {
        await area.translate(node.id, gn.position);
      }
    }
    for (const c of graph.connections) {
      const src = idMap[c.source];
      const tgt = idMap[c.target];
      if (!src || !tgt) continue;
      await editor.addConnection(
        new ClassicPreset.Connection(src, c.sourceKey, tgt, c.targetKey)
      );
    }
    await zoomToFit();
    fireChange();
  }

  // addNode drops a fresh palette node onto the canvas near the origin, staggered
  // so successive adds do not stack exactly.
  let addOffset = 0;
  async function addNode(kind) {
    const node = makeNode(kind, {});
    await editor.addNode(node);
    const x = 40 + (addOffset % 5) * 30;
    const y = 40 + (addOffset % 5) * 30;
    addOffset++;
    await area.translate(node.id, { x: x, y: y });
    return node;
  }

  async function zoomToFit() {
    const nodes = editor.getNodes();
    if (nodes.length > 0) {
      await AreaExtensions.zoomAt(area, nodes);
    }
  }

  // toAST folds the live graph to the rule AST via the pure serializer. Throws a
  // structured error ({code, message}) if the graph is not a single tree.
  hook.toAST = function () {
    return S.graphToAST(reteToGraph());
  };
  hook.fromAST = fromAST;
  hook.getGraph = reteToGraph;
  hook.addNode = addNode;
  hook.clear = clear;
  hook.zoomToFit = zoomToFit;
  hook.validate = function () {
    let ast;
    try {
      ast = S.graphToAST(reteToGraph());
    } catch (e) {
      return [{ code: e.code || "APERTURE_RULE_INVALID", message: e.message, path: "$" }];
    }
    if (ast === null) return [];
    return S.validateAST(ast);
  };
  hook.destroy = function () {
    area.destroy();
  };

  return hook;
}

// ruleRpc POSTs a Twirp JSON call through the shell's bearer wrapper (window
// .apiFetch) and returns the decoded response. A non-2xx carries a Twirp error
// body ({code, msg, meta:{code}}) which is normalised into an Error with .code
// (the canonical APERTURE_* code), .message, and .status. Named ruleRpc to avoid
// colliding with the other screens' classic-script globals.
async function ruleRpc(method, body) {
  const res = await window.apiFetch(
    "/twirp/aperture.ApertureService/" + method,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    }
  );
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

// A small starter rule so the canvas is not blank on first open; also exercises
// fromAST end to end. object.classification == "public".
const STARTER_AST = {
  type: "compare",
  op: "eq",
  left: { type: "var", name: "object.classification" },
  right: { type: "literal", value: "public" },
};

function rules() {
  return {
    booting: true,
    error: "",
    problems: [],
    // ---- E7-S3 load/save/validate/what-if integration state ----
    // The dev principal + active account the RPCs resolve against, plus the admin
    // tier probe (rule editing is SYSTEM tier — only system-admins may save).
    principal: "",
    accounts: [],
    account: "",
    canEdit: false,
    tierChecked: false,
    // The rule being edited: its name (identity, upsert key), description, and the
    // list of stored rules to load from.
    ruleName: "",
    description: "",
    ruleList: [],
    // Save / validate status and the server-side (engine) validation problems,
    // distinct from the client-side structural `problems` above.
    saving: false,
    validating: false,
    status: null, // { kind: "ok" | "err", code, msg }
    // Live what-if preview (READ-ONLY): a hypothetical decision + Explain trace for
    // the rule CURRENTLY on the canvas, previewed WITHOUT saving it.
    preview: { principal: "", action: "", object: "" },
    previewing: false,
    previewError: null,
    previewDecision: null,
    previewTrace: null,
    // The node palette, grouped by category. Covers the whole AST: logical
    // combinators, comparisons over variables/literals, and the Pulse building
    // blocks (list/call) from E2-S3.
    palette: [
      { group: "Logic", items: [
        { kind: "and", label: "And" },
        { kind: "or", label: "Or" },
        { kind: "not", label: "Not" },
      ] },
      { group: "Compare", items: [
        { kind: "compare", label: "Compare" },
      ] },
      { group: "Operands", items: [
        { kind: "var", label: "Variable" },
        { kind: "literal", label: "Literal" },
      ] },
      { group: "Pulse", items: [
        { kind: "list", label: "List" },
        { kind: "call", label: "Call" },
      ] },
    ],
    _editor: null,

    init() {
      this.principal = localStorage.getItem("aperture.devToken") || "";
      this.preview.principal = this.principal;
      document.addEventListener("aperture:authenticated", (e) => {
        this.principal = (e.detail && e.detail.principal) || localStorage.getItem("aperture.devToken") || "";
        if (!this.preview.principal) this.preview.principal = this.principal;
        this.bootstrap();
      });
      const clear = () => {
        this.principal = "";
        this.canEdit = false;
        this.tierChecked = false;
      };
      document.addEventListener("aperture:unauthenticated", clear);
      document.addEventListener("aperture:signout", clear);
      this.mount();
    },

    async mount() {
      this.booting = true;
      this.error = "";
      try {
        if (!window.RuleSerializer) {
          throw new Error("rule serializer not loaded");
        }
        const mod = await import("/vendor/rete/rete.min.js");
        const el = this.$refs.canvas;
        if (!el) {
          throw new Error("canvas container not found");
        }
        this._editor = await createBlueprintEditor(el, mod, window.RuleSerializer);
        // Expose the E7-S3 save/load hooks and wire validation refresh.
        window.blueprintEditor = this._editor;
        this._editor.onChange = () => {
          this.refreshValidation();
          // A graph edit invalidates the last server verdict and preview.
          this.status = null;
        };
        await this._editor.fromAST(STARTER_AST);
        this.booting = false;
        if (this.principal) this.bootstrap();
      } catch (e) {
        this.error = e && e.message ? e.message : String(e);
        this.booting = false;
      }
    },

    // bootstrap resolves the active account, probes system-admin authority (rule
    // editing is SYSTEM tier), and lists the stored rules to load from.
    async bootstrap() {
      await this.loadAccounts();
      await this.probeTier();
      await this.loadRules();
    },

    async loadAccounts() {
      try {
        const resp = await ruleRpc("ListAccounts", {});
        this.accounts = (resp.entities_json || []).map((s) => JSON.parse(s));
        if (!this.account && this.accounts.length > 0) this.account = this.accounts[0].ID;
      } catch (e) {
        if (e.status !== 401) this.status = { kind: "err", code: e.code, msg: e.message };
      }
    },

    // probeTier asks the OPEN Check RPC whether the signed-in principal holds
    // system-admin authority, gating the Save affordance (E6-S2 pattern). It
    // carries the bearer but needs no auth, so a read-only viewer still gets an
    // answer and a 403 on save is never a surprise.
    async probeTier() {
      this.tierChecked = false;
      try {
        const dec = await ruleRpc("Check", {
          account: this.account,
          principal: this.principal,
          action: "aperture.admin",
          object: "system:schema",
        });
        this.canEdit = !!dec.allow;
      } catch (_) {
        this.canEdit = false;
      }
      this.tierChecked = true;
    },

    // loadRules lists the stored rules for the load picker.
    async loadRules() {
      try {
        const resp = await ruleRpc("ListRules", {});
        this.ruleList = (resp.rules_json || []).map((s) => JSON.parse(s));
      } catch (e) {
        if (e.status !== 401) this.status = { kind: "err", code: e.code, msg: e.message };
      }
    },

    // loadRule fetches one rule by name and renders its AST into the canvas via the
    // fromAST hook — the same rules.Node JSON the engine evaluates and the state
    // file persists.
    async loadRule(name) {
      if (!name) return;
      this.status = null;
      try {
        const resp = await ruleRpc("GetRule", { id: name });
        const rule = JSON.parse(resp.rule_json);
        this.ruleName = rule.Name || name;
        this.description = rule.Description || "";
        if (rule.AST && window.blueprintEditor) {
          await window.blueprintEditor.fromAST(rule.AST);
        }
        this.refreshValidation();
      } catch (e) {
        if (e.status !== 401) this.status = { kind: "err", code: e.code, msg: e.message };
      }
    },

    // currentRule folds the canvas to a model.Rule body ({Name, Description, AST}).
    // toAST throws a structured {code,message} when the graph is not a single tree;
    // the caller surfaces it as a status error.
    currentRule() {
      if (!window.blueprintEditor) throw new Error("editor not ready");
      const ast = window.blueprintEditor.toAST();
      if (ast === null) {
        const err = new Error("the canvas is empty — build a rule before saving");
        err.code = "APERTURE_RULE_INVALID";
        throw err;
      }
      return { Name: this.ruleName.trim(), Description: this.description, AST: ast };
    },

    // serverValidate compiles/validates the current AST on the server WITHOUT
    // persisting it, surfacing the engine's APERTURE_RULE_* verdict on the canvas.
    async serverValidate() {
      this.status = null;
      this.validating = true;
      try {
        const rule = this.currentRule();
        await ruleRpc("ValidateRule", { rule_json: JSON.stringify(rule) });
        this.status = { kind: "ok", msg: "The rule compiled cleanly on the server." };
      } catch (e) {
        this.status = { kind: "err", code: e.code || "APERTURE_RULE_INVALID", msg: e.message };
      } finally {
        this.validating = false;
      }
    },

    // save persists the current rule via the mutation API (system-admin tier). The
    // server re-validates the AST and rejects an invalid rule with its
    // APERTURE_RULE_* code, shown on the canvas; on success the rule takes effect
    // immediately in any scope strategy that references it.
    async save() {
      this.status = null;
      if (!this.ruleName.trim()) {
        this.status = { kind: "err", code: "APERTURE_RULE_INVALID", msg: "A rule name is required." };
        return;
      }
      this.saving = true;
      try {
        const rule = this.currentRule();
        await ruleRpc("PutRule", {
          actor: { account: this.account },
          rule_json: JSON.stringify(rule),
        });
        this.status = { kind: "ok", msg: 'Saved rule "' + rule.Name + '".' };
        await this.loadRules();
      } catch (e) {
        this.status = { kind: "err", code: e.code || "APERTURE_ERROR", msg: e.message };
      } finally {
        this.saving = false;
      }
    },

    // runPreview renders the READ-ONLY what-if for the rule CURRENTLY on the canvas:
    // the decision + Explain trace a rule-backed grant would produce, WITHOUT
    // saving. The unsaved rule is layered over the live model as a Simulate overlay
    // (rules_json), shadowing any stored rule of the same name — so it previews the
    // edit against grants that reference this rule name. Nothing is persisted.
    async runPreview() {
      this.previewError = null;
      this.previewDecision = null;
      this.previewTrace = null;
      if (!this.ruleName.trim()) {
        this.previewError = { code: "APERTURE_RULE_INVALID", msg: "Name the rule so the preview can overlay it." };
        return;
      }
      if (!this.preview.principal || !this.preview.action || !this.preview.object) {
        this.previewError = { code: "APERTURE_INVALID_INPUT", msg: "Principal, action and object are required." };
        return;
      }
      this.previewing = true;
      try {
        const rule = this.currentRule();
        const req = {
          query: {
            account: this.account,
            principal: this.preview.principal,
            action: this.preview.action,
            object: this.preview.object,
          },
          rules_json: [JSON.stringify(rule)],
        };
        const [dec, exp] = await Promise.all([
          ruleRpc("Simulate", req),
          ruleRpc("SimulateExplain", req),
        ]);
        this.previewDecision = dec;
        this.previewTrace = exp.trace_json ? JSON.parse(exp.trace_json) : null;
      } catch (e) {
        this.previewError = { code: e.code || "APERTURE_ERROR", msg: e.message };
      } finally {
        this.previewing = false;
      }
    },

    // refreshValidation runs the client-side structural check and surfaces its
    // problems. Full type-checking is the engine's (server-side, E7-S3).
    refreshValidation() {
      if (!window.blueprintEditor) return;
      try {
        this.problems = window.blueprintEditor.validate();
        this.error = "";
      } catch (e) {
        this.problems = [];
        this.error = e && e.message ? e.message : String(e);
      }
    },

    add(kind) {
      if (window.blueprintEditor) {
        window.blueprintEditor.addNode(kind);
      }
    },

    async clearCanvas() {
      if (window.blueprintEditor) {
        await window.blueprintEditor.clear();
        this.refreshValidation();
      }
    },

    zoomToFit() {
      if (window.blueprintEditor) {
        window.blueprintEditor.zoomToFit();
      }
    },

    get valid() {
      return this.problems.length === 0 && !this.error;
    },

    destroy() {
      if (this._editor && typeof this._editor.destroy === "function") {
        this._editor.destroy();
      }
      if (window.blueprintEditor === this._editor) {
        window.blueprintEditor = null;
      }
      this._editor = null;
    },
  };
}

window.rules = rules;
window.createBlueprintEditor = createBlueprintEditor;
