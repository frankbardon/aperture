/*
 * rules-serializer.js — the load-bearing graph <-> rule-AST bridge (E7-S2).
 *
 * PURE, DOM-FREE, DEPENDENCY-FREE. This module knows nothing about Rete, React
 * or Alpine. It defines a plain graph model (nodes with typed data + directed
 * connections) and two pure functions:
 *
 *   graphToAST(graph)  -> rule AST node (the E2-S3 `rules.Node` JSON shape)
 *   astToGraph(ast)    -> plain graph { nodes, connections }
 *
 * The pair round-trips LOSSLESSLY against the E2-S3 AST: for any valid AST,
 *   graphToAST(astToGraph(ast))  deep-equals  ast.
 * There is NO second rule format — the AST here is byte-for-byte the same shape
 * `rules/ast.go` marshals (fields: type, op, name, value, left, right,
 * children, items; all others omitted). The Rete UI (rules.js) is only an
 * editing surface that produces/consumes this plain graph model.
 *
 * Because it is pure it is unit-testable under `node` with no browser — see
 * rules-serializer.test.js (run: `node internal/server/static/js/rules-serializer.test.js`).
 *
 * The module is UMD-ish: it attaches `window.RuleSerializer` in the browser and
 * exports the same object under CommonJS so the node test can require() it.
 */
(function (root, factory) {
  const api = factory();
  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (typeof window !== "undefined") {
    window.RuleSerializer = api;
  }
})(this, function () {
  "use strict";

  // ---- AST vocabulary (mirrors rules/ast.go exactly) -----------------------

  // Node types — the closed discriminator set. Kept identical to NodeType in
  // rules/ast.go so the editor palette maps one-to-one.
  const TYPES = {
    AND: "and",
    OR: "or",
    NOT: "not",
    COMPARE: "compare",
    VAR: "var",
    LITERAL: "literal",
    LIST: "list",
    CALL: "call",
  };

  // Comparison operators carried in a compare node's `op`.
  const OPS = ["eq", "ne", "lt", "le", "gt", "ge", "in", "nin"];

  // Context-variable roots a `var` may reference (allowedRoots in ast.go).
  const ROOTS = ["object", "principal", "account", "action"];

  // The default pure functions a `call` may name (rules/compiler.go
  // defaultFunctions). Advisory only for the palette — the server is the
  // authority (a host may register more), so validateAST does NOT reject an
  // unknown function name; it only checks the identifier shape, like ast.go.
  const FUNCTIONS = ["lower", "upper", "contains", "startsWith", "endsWith", "len"];

  // Dotted-identifier-path matcher (varPath in ast.go). Each segment is a
  // Go-style identifier, which keeps the rendered expression injection-free.
  const VAR_PATH = /^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$/;

  // ---- Node specs: the single source of truth for pins the UI + serializer
  // share. `inputs` names the arity shape; the serializer derives connection
  // target keys from it, and the Rete UI builds the matching input sockets.
  //
  //   out      the socket TYPE a node's single output produces
  //   inputs   'none' | 'child' | 'leftright' | 'variadic'
  //   accepts  the socket type(s) an input pin accepts (for typed sockets)
  const SOCKET = { BOOL: "bool", VALUE: "value", LIST: "list" };

  const NODE_SPECS = {
    and: { title: "And", category: "logic", out: SOCKET.BOOL, inputs: "variadic", accepts: [SOCKET.BOOL] },
    or: { title: "Or", category: "logic", out: SOCKET.BOOL, inputs: "variadic", accepts: [SOCKET.BOOL] },
    not: { title: "Not", category: "logic", out: SOCKET.BOOL, inputs: "child", accepts: [SOCKET.BOOL] },
    compare: { title: "Compare", category: "compare", out: SOCKET.BOOL, inputs: "leftright", accepts: [SOCKET.VALUE, SOCKET.LIST] },
    var: { title: "Variable", category: "operand", out: SOCKET.VALUE, inputs: "none", accepts: [] },
    literal: { title: "Literal", category: "operand", out: SOCKET.VALUE, inputs: "none", accepts: [] },
    list: { title: "List", category: "pulse", out: SOCKET.LIST, inputs: "variadic", accepts: [SOCKET.VALUE] },
    call: { title: "Call", category: "pulse", out: SOCKET.VALUE, inputs: "variadic", accepts: [SOCKET.VALUE, SOCKET.LIST] },
  };

  // inputKeys returns the ordered input-socket keys a node of `type` exposes
  // given how many variadic slots it currently has. The keys are the contract
  // between the graph model and both serializer directions.
  function inputKeys(type, arity) {
    const spec = NODE_SPECS[type];
    if (!spec) return [];
    switch (spec.inputs) {
      case "none":
        return [];
      case "child":
        return ["in"];
      case "leftright":
        return ["left", "right"];
      case "variadic": {
        const n = Math.max(0, arity || 0);
        const keys = [];
        for (let i = 0; i < n; i++) keys.push("in-" + i);
        return keys;
      }
      default:
        return [];
    }
  }

  // ---- graph -> AST --------------------------------------------------------

  // graphToAST folds a plain graph into a rule AST. It finds the single root
  // (the node whose output feeds no input) and recurses. Throws a structured
  // error ({ code, message }) if the graph is not a single connected tree — the
  // editor surfaces that through validate() before any save.
  function graphToAST(graph) {
    const g = graph || {};
    const nodes = {};
    (g.nodes || []).forEach(function (n) {
      nodes[n.id] = n;
    });

    // incoming[targetId] = { key: sourceId }; consumed[sourceId] = true.
    const incoming = {};
    const consumed = {};
    (g.connections || []).forEach(function (c) {
      if (!incoming[c.target]) incoming[c.target] = {};
      incoming[c.target][c.targetKey] = c.source;
      consumed[c.source] = true;
    });

    const ids = Object.keys(nodes);
    if (ids.length === 0) {
      return null; // empty canvas -> no rule
    }
    const roots = ids.filter(function (id) {
      return !consumed[id];
    });
    if (roots.length !== 1) {
      throw serErr(
        "APERTURE_RULE_INVALID",
        "graph must have exactly one root node (found " + roots.length + ")"
      );
    }

    const seen = {};
    function build(id) {
      const n = nodes[id];
      if (!n) throw serErr("APERTURE_RULE_INVALID", "connection references a missing node: " + id);
      if (seen[id]) throw serErr("APERTURE_RULE_INVALID", "graph has a cycle at node " + id);
      seen[id] = true;
      const inc = incoming[id] || {};
      const type = n.type;
      switch (type) {
        case TYPES.AND:
        case TYPES.OR:
          return { type: type, children: orderedSources(inc).map(build) };
        case TYPES.NOT:
          return { type: TYPES.NOT, children: [build(requireSource(inc, "in", id))] };
        case TYPES.COMPARE:
          return {
            type: TYPES.COMPARE,
            op: n.op || "",
            left: build(requireSource(inc, "left", id)),
            right: build(requireSource(inc, "right", id)),
          };
        case TYPES.VAR:
          return { type: TYPES.VAR, name: n.name || "" };
        case TYPES.LITERAL:
          // Always emit `value`, even when falsy (false/0/""/null): those are
          // non-empty RawMessage on the Go side and must survive the round-trip.
          return { type: TYPES.LITERAL, value: n.value === undefined ? null : n.value };
        case TYPES.LIST:
          return { type: TYPES.LIST, items: orderedSources(inc).map(build) };
        case TYPES.CALL:
          return { type: TYPES.CALL, name: n.name || "", items: orderedSources(inc).map(build) };
        default:
          throw serErr("APERTURE_RULE_INVALID", "unknown node type: " + String(type));
      }
    }
    return build(roots[0]);
  }

  // orderedSources returns the sources of `in-0`, `in-1`, ... in numeric order.
  function orderedSources(inc) {
    return Object.keys(inc)
      .filter(function (k) {
        return /^in-\d+$/.test(k);
      })
      .sort(function (a, b) {
        return parseInt(a.slice(3), 10) - parseInt(b.slice(3), 10);
      })
      .map(function (k) {
        return inc[k];
      });
  }

  function requireSource(inc, key, id) {
    const s = inc[key];
    if (s === undefined) {
      throw serErr("APERTURE_RULE_INVALID", "node " + id + " is missing its `" + key + "` input");
    }
    return s;
  }

  // ---- AST -> graph --------------------------------------------------------

  // astToGraph expands an AST into a plain graph the editor can render. Node ids
  // and positions are editor concerns (NOT part of the AST) and are dropped when
  // going back the other way, so the round-trip stays lossless. Layout is a
  // simple left-to-right layered placement (x by depth, y by leaf order) — purely
  // cosmetic; pan/zoom/reroute in the canvas can move anything afterwards.
  function astToGraph(ast) {
    const nodes = [];
    const connections = [];
    let counter = 0;
    let leafCursor = 0;
    const nextId = function () {
      return "n" + ++counter;
    };
    const COL = 260;
    const ROW = 96;

    function walk(node, depth) {
      const id = nextId();
      const g = { id: id, type: node.type, position: { x: depth * COL, y: 0 } };
      let childYs = [];

      function wire(childNode, targetKey) {
        const childId = walk(childNode, depth + 1);
        connections.push({ source: childId, sourceKey: "out", target: id, targetKey: targetKey });
        const cn = nodeById(nodes, childId);
        if (cn) childYs.push(cn.position.y);
        return childId;
      }

      switch (node.type) {
        case TYPES.AND:
        case TYPES.OR:
          (node.children || []).forEach(function (ch, i) {
            wire(ch, "in-" + i);
          });
          break;
        case TYPES.NOT:
          wire((node.children || [])[0], "in");
          break;
        case TYPES.COMPARE:
          g.op = node.op;
          wire(node.left, "left");
          wire(node.right, "right");
          break;
        case TYPES.VAR:
          g.name = node.name;
          break;
        case TYPES.LITERAL:
          g.value = node.value === undefined ? null : node.value;
          break;
        case TYPES.LIST:
          (node.items || []).forEach(function (it, i) {
            wire(it, "in-" + i);
          });
          break;
        case TYPES.CALL:
          g.name = node.name;
          (node.items || []).forEach(function (it, i) {
            wire(it, "in-" + i);
          });
          break;
        default:
          throw serErr("APERTURE_RULE_INVALID", "unknown node type: " + String(node.type));
      }

      // Position: a parent sits at the average y of its children; a leaf takes
      // the next free row. Cosmetic only.
      if (childYs.length > 0) {
        g.position.y = childYs.reduce(function (a, b) { return a + b; }, 0) / childYs.length;
      } else {
        g.position.y = leafCursor * ROW;
        leafCursor++;
      }
      nodes.push(g);
      return id;
    }

    if (ast) walk(ast, 0);
    return { nodes: nodes, connections: connections };
  }

  function nodeById(nodes, id) {
    for (let i = 0; i < nodes.length; i++) {
      if (nodes[i].id === id) return nodes[i];
    }
    return null;
  }

  // ---- client-side structural validation -----------------------------------

  // validateAST mirrors rules/ast.go Validate: it checks AST SHAPE only, not
  // types. Full type-checking (operand types, function arity/signatures) is the
  // engine's job and runs server-side in E7-S3 — this is the fast, offline
  // pre-flight that catches malformed graphs and unknown-variable roots before a
  // save round-trips to the API. Returns an array of { code, message, path }.
  function validateAST(ast) {
    const problems = [];
    function report(code, message, path) {
      problems.push({ code: code, message: message, path: path });
    }
    function walk(n, path) {
      if (n === null || n === undefined) {
        report("APERTURE_RULE_INVALID", "nil node", path);
        return;
      }
      switch (n.type) {
        case TYPES.AND:
        case TYPES.OR:
          if (!Array.isArray(n.children) || n.children.length < 2) {
            report("APERTURE_RULE_INVALID", n.type + " requires at least two children", path);
          }
          (n.children || []).forEach(function (c, i) {
            walk(c, path + ".children[" + i + "]");
          });
          break;
        case TYPES.NOT:
          if (!Array.isArray(n.children) || n.children.length !== 1) {
            report("APERTURE_RULE_INVALID", "not requires exactly one child", path);
          }
          (n.children || []).forEach(function (c, i) {
            walk(c, path + ".children[" + i + "]");
          });
          break;
        case TYPES.COMPARE:
          if (OPS.indexOf(n.op) < 0) {
            report("APERTURE_RULE_INVALID", "unknown comparison operator: " + String(n.op), path);
          }
          if (n.left === undefined || n.left === null || n.right === undefined || n.right === null) {
            report("APERTURE_RULE_INVALID", "comparison requires a left and a right operand", path);
          } else {
            if ((n.op === "in" || n.op === "nin") && n.right.type !== TYPES.LIST && n.right.type !== TYPES.VAR) {
              report("APERTURE_RULE_INVALID", "in/nin requires a list or variable on the right", path + ".right");
            }
            walk(n.left, path + ".left");
            walk(n.right, path + ".right");
          }
          break;
        case TYPES.VAR:
          validateVar(n.name, path, report);
          break;
        case TYPES.LITERAL:
          validateLiteral(n.value, path, report);
          break;
        case TYPES.LIST:
          (n.items || []).forEach(function (it, i) {
            walk(it, path + ".items[" + i + "]");
          });
          break;
        case TYPES.CALL:
          if (!n.name || !VAR_PATH.test(n.name)) {
            report("APERTURE_RULE_INVALID", "call has an invalid function name: " + String(n.name), path);
          }
          (n.items || []).forEach(function (it, i) {
            walk(it, path + ".items[" + i + "]");
          });
          break;
        default:
          report("APERTURE_RULE_INVALID", "unknown node type: " + String(n.type), path);
      }
    }
    walk(ast, "$");
    return problems;
  }

  function validateVar(name, path, report) {
    if (!name || !VAR_PATH.test(name)) {
      report("APERTURE_RULE_INVALID", "variable reference is not a dotted identifier path: " + String(name), path);
      return;
    }
    const root = name.indexOf(".") >= 0 ? name.slice(0, name.indexOf(".")) : name;
    if (ROOTS.indexOf(root) < 0) {
      report("APERTURE_RULE_UNKNOWN_VARIABLE", "variable root is not an exposed context root: " + root, path);
    }
  }

  function validateLiteral(value, path, report) {
    if (value === undefined) {
      report("APERTURE_RULE_INVALID", "literal has no value", path);
      return;
    }
    const t = typeof value;
    if (value !== null && t !== "boolean" && t !== "number" && t !== "string") {
      report("APERTURE_RULE_INVALID", "literal must be a scalar (string, number, bool, or null); use a list node for collections", path);
    }
  }

  // parseLiteral turns the text a literal control holds into a scalar JS value,
  // preserving type: true/false/null, JSON numbers, and JSON strings parse to
  // their value; any other bare text is taken as a string. This is what lets
  // false/0/""/null survive as real scalars rather than the word "false".
  function parseLiteral(text) {
    if (text === null || text === undefined) return null;
    const s = String(text).trim();
    if (s === "") return "";
    try {
      const v = JSON.parse(s);
      const t = typeof v;
      if (v === null || t === "boolean" || t === "number" || t === "string") {
        return v;
      }
    } catch (_) {
      /* fall through: treat as a plain string */
    }
    return s;
  }

  // formatLiteral is the inverse of parseLiteral for display in a text control:
  // strings show unquoted, everything else shows its JSON form.
  function formatLiteral(value) {
    if (typeof value === "string") return value;
    if (value === undefined) return "";
    return JSON.stringify(value);
  }

  function serErr(code, message) {
    const e = new Error(message);
    e.code = code;
    return e;
  }

  return {
    TYPES: TYPES,
    OPS: OPS,
    ROOTS: ROOTS,
    FUNCTIONS: FUNCTIONS,
    SOCKET: SOCKET,
    NODE_SPECS: NODE_SPECS,
    VAR_PATH: VAR_PATH,
    inputKeys: inputKeys,
    graphToAST: graphToAST,
    astToGraph: astToGraph,
    validateAST: validateAST,
    parseLiteral: parseLiteral,
    formatLiteral: formatLiteral,
  };
});
