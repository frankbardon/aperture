/*
 * rules-serializer.test.js — round-trip correctness for the pure graph<->AST
 * serializer (E7-S2). This is the LOAD-BEARING test: it proves astToGraph and
 * graphToAST are lossless inverses over the exact E2-S3 AST shape, with no
 * browser and no dependencies.
 *
 * Run it directly with node (NOT part of the Go build or CI, which are node-free):
 *
 *   node internal/server/static/js/rules-serializer.test.js
 *
 * Exit code 0 = all round-trips + validation cases passed; 1 = a failure.
 */
"use strict";

const S = require("./rules-serializer.js");

let failures = 0;
function ok(cond, msg) {
  if (!cond) {
    failures++;
    console.error("FAIL: " + msg);
  } else {
    console.log("pass: " + msg);
  }
}
function eq(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

// --- Representative ASTs, byte-for-byte the rules.Node JSON shape ------------
// Covers: nested and/or/not, compare with var+literal, in/nin with a list,
// a call, and the scalar edge cases (false/0/""/null) that must survive.
const cases = {
  "compare var eq string": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.classification" },
    right: { type: "literal", value: "public" },
  },
  "nested and/or/not": {
    type: "and",
    children: [
      {
        type: "or",
        children: [
          { type: "compare", op: "gt", left: { type: "var", name: "object.level" }, right: { type: "literal", value: 3 } },
          { type: "compare", op: "eq", left: { type: "var", name: "principal.tier" }, right: { type: "literal", value: "gold" } },
        ],
      },
      {
        type: "not",
        children: [
          { type: "compare", op: "eq", left: { type: "var", name: "account.suspended" }, right: { type: "literal", value: true } },
        ],
      },
    ],
  },
  "in with list literal": {
    type: "compare",
    op: "in",
    left: { type: "var", name: "object.region" },
    right: {
      type: "list",
      items: [
        { type: "literal", value: "us" },
        { type: "literal", value: "eu" },
        { type: "literal", value: "apac" },
      ],
    },
  },
  "nin with var on right": {
    type: "compare",
    op: "nin",
    left: { type: "var", name: "principal.id" },
    right: { type: "var", name: "object.blocklist" },
  },
  "call len over var compared to number": {
    type: "compare",
    op: "ge",
    left: { type: "call", name: "len", items: [{ type: "var", name: "object.tags" }] },
    right: { type: "literal", value: 1 },
  },
  "nested call contains(lower(var), literal)": {
    type: "call",
    name: "contains",
    items: [
      { type: "call", name: "lower", items: [{ type: "var", name: "object.title" }] },
      { type: "literal", value: "secret" },
    ],
  },
  "falsy scalars survive — false": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.archived" },
    right: { type: "literal", value: false },
  },
  "falsy scalars survive — zero": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.count" },
    right: { type: "literal", value: 0 },
  },
  "falsy scalars survive — empty string": {
    type: "compare",
    op: "ne",
    left: { type: "var", name: "object.note" },
    right: { type: "literal", value: "" },
  },
  "falsy scalars survive — null": {
    type: "compare",
    op: "eq",
    left: { type: "var", name: "object.owner" },
    right: { type: "literal", value: null },
  },
};

// --- Round-trip invariant: graphToAST(astToGraph(ast)) deep-equals ast ------
Object.keys(cases).forEach(function (name) {
  const ast = cases[name];
  const graph = S.astToGraph(ast);
  const back = S.graphToAST(graph);
  ok(eq(back, ast), "round-trip: " + name);
});

// --- astToGraph produces a single-root, acyclic, fully-wired graph ----------
Object.keys(cases).forEach(function (name) {
  const g = S.astToGraph(cases[name]);
  const consumed = {};
  g.connections.forEach(function (c) {
    consumed[c.source] = true;
  });
  const roots = g.nodes.filter(function (n) {
    return !consumed[n.id];
  });
  ok(roots.length === 1, "single root: " + name);
});

// --- Empty canvas -> null AST; null AST -> empty graph ----------------------
ok(S.graphToAST({ nodes: [], connections: [] }) === null, "empty graph -> null AST");
ok(eq(S.astToGraph(null), { nodes: [], connections: [] }), "null AST -> empty graph");

// --- validateAST catches the structural errors ast.go catches --------------
ok(S.validateAST(cases["nested and/or/not"]).length === 0, "valid AST has no problems");
ok(
  S.validateAST({ type: "and", children: [{ type: "literal", value: 1 }] }).some(function (p) {
    return p.code === "APERTURE_RULE_INVALID";
  }),
  "and with one child is invalid"
);
ok(
  S.validateAST({ type: "var", name: "bogus.field" }).some(function (p) {
    return p.code === "APERTURE_RULE_UNKNOWN_VARIABLE";
  }),
  "unknown variable root flagged"
);
ok(
  S.validateAST({
    type: "compare",
    op: "in",
    left: { type: "var", name: "object.x" },
    right: { type: "literal", value: "y" },
  }).some(function (p) {
    return p.code === "APERTURE_RULE_INVALID";
  }),
  "in with non-list/var right is invalid"
);
ok(
  S.validateAST({ type: "not", children: [{ type: "var", name: "object.a" }, { type: "var", name: "object.b" }] }).length > 0,
  "not with two children is invalid"
);

// --- graphToAST rejects a multi-root graph ----------------------------------
try {
  S.graphToAST({
    nodes: [
      { id: "a", type: "literal", value: 1 },
      { id: "b", type: "literal", value: 2 },
    ],
    connections: [],
  });
  ok(false, "multi-root graph should throw");
} catch (e) {
  ok(e.code === "APERTURE_RULE_INVALID", "multi-root graph throws APERTURE_RULE_INVALID");
}

// --- parseLiteral / formatLiteral preserve scalar types ---------------------
ok(S.parseLiteral("false") === false, "parseLiteral false");
ok(S.parseLiteral("0") === 0, "parseLiteral 0");
ok(S.parseLiteral('"hi"') === "hi", "parseLiteral quoted string");
ok(S.parseLiteral("null") === null, "parseLiteral null");
ok(S.parseLiteral("plain words") === "plain words", "parseLiteral bare text -> string");
ok(S.formatLiteral(false) === "false", "formatLiteral false");
ok(S.formatLiteral("hi") === "hi", "formatLiteral string unquoted");

if (failures > 0) {
  console.error("\n" + failures + " failure(s)");
  process.exit(1);
}
console.log("\nAll serializer round-trip tests passed.");
