// entry.mjs — the single ESM entry esbuild bundles into the vendored Rete blob.
//
// This is the ONLY hand-authored source for the vendored bundle. It is bundled
// ONCE (see the Makefile `vendor-rete` target / vendor/rete/README.md) into a
// self-contained ESM file committed under internal/server/static/vendor/rete/.
// There is NO node build in dev or CI — the committed blob is what ships.
//
// It re-exports the Rete building blocks a Blueprints-style editor needs so the
// real node<->AST editor (E7-S2) imports everything from one module URL, and it
// exposes createHelloCanvas() to prove the bundle mounts an editor + renders.

import { NodeEditor, ClassicPreset } from "rete";
import { AreaPlugin, AreaExtensions } from "rete-area-plugin";
import {
  ConnectionPlugin,
  Presets as ConnectionPresets,
} from "rete-connection-plugin";
import { ReactPlugin, Presets as ReactPresets } from "rete-react-plugin";
import { createRoot } from "react-dom/client";

// Re-export the toolkit E7-S2 builds the real editor on. One import URL, one
// pinned version set — the domain code never touches node_modules.
//
// createRoot is re-exported because rete-react-plugin v2 REQUIRES it: the
// ReactPlugin constructor must be handed react-dom/client's createRoot to mount
// node components. The E7-S2 editor (rules.js) reads it off this module as
// `mod.createRoot`; without the export it is undefined and no node ever renders.
export {
  NodeEditor,
  ClassicPreset,
  AreaPlugin,
  AreaExtensions,
  ConnectionPlugin,
  ConnectionPresets,
  ReactPlugin,
  ReactPresets,
  createRoot,
};

// createHelloCanvas mounts a minimal Rete editor into `container` (a DOM
// element) to prove the vendored bundle loads and renders. It seeds ONE
// placeholder node so the render pipeline (area + connection + React preset) is
// exercised end to end — E7-S2 replaces the seeding with the real rule AST.
//
// Returns { editor, area, destroy } so the caller owns teardown.
export async function createHelloCanvas(container) {
  const editor = new NodeEditor();
  const area = new AreaPlugin(container);
  const connection = new ConnectionPlugin();
  const render = new ReactPlugin({ createRoot });

  render.addPreset(ReactPresets.classic.setup());
  connection.addPreset(ConnectionPresets.classic.setup());

  editor.use(area);
  area.use(connection);
  area.use(render);

  AreaExtensions.simpleNodesOrder(area);
  AreaExtensions.selectableNodes(area, AreaExtensions.selector(), {
    accumulating: AreaExtensions.accumulateOnCtrl(),
  });

  // One placeholder node — proves the render preset mounts a node, a control and
  // a socket. E7-S2 swaps this for nodes generated from the pulse-expression AST.
  const node = new ClassicPreset.Node("Rule");
  node.addControl(
    "expr",
    new ClassicPreset.InputControl("text", { initial: "true" }),
  );
  node.addOutput("out", new ClassicPreset.Output(new ClassicPreset.Socket("socket")));
  await editor.addNode(node);
  await area.translate(node.id, { x: 60, y: 60 });

  await AreaExtensions.zoomAt(area, editor.getNodes());

  return { editor, area, destroy: () => area.destroy() };
}
