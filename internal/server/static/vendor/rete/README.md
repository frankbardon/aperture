# Vendored Rete.js bundle

`rete.min.js` is a **pre-built, committed** bundle of [Rete.js](https://retejs.org)
and the plugins a Blueprints-style node editor needs. It ships as a single
self-contained ESM file so the Aperture binary stays self-contained
(`//go:embed static`) with **no node build** in dev or CI. Regenerating it is an
occasional, documented manual step — never a CI build.

The `rules` section of the admin shell imports it directly:

```html
<script type="module">
  import { createHelloCanvas } from "/vendor/rete/rete.min.js";
  const { destroy } = await createHelloCanvas(document.getElementById("canvas"));
</script>
```

E7-S2 builds the real node↔AST editor on the toolkit this bundle re-exports
(`NodeEditor`, `ClassicPreset`, `AreaPlugin`, `AreaExtensions`, `ConnectionPlugin`,
`ConnectionPresets`, `ReactPlugin`, `ReactPresets`) plus `createHelloCanvas` as
the reference wiring.

## Pinned versions

| Package | Version | Role |
|---|---|---|
| `rete` | 2.0.6 | Editor core (`NodeEditor`, `ClassicPreset`) |
| `rete-area-plugin` | 2.1.5 | Canvas area, pan/zoom, node positioning |
| `rete-connection-plugin` | 2.0.5 | Draggable connections between sockets |
| `rete-render-utils` | 2.0.3 | Shared render helpers (peer of the renderer) |
| `rete-react-plugin` | 2.1.0 | Node/control/socket renderer (classic preset) |
| `react` | 19.2.7 | Renderer runtime (bundled into the blob) |
| `react-dom` | 19.2.7 | Renderer runtime (bundled into the blob) |
| `styled-components` | 6.4.3 | Renderer styling (bundled into the blob) |
| `esbuild` | 0.28.1 | Bundler (build-time only) |

React, react-dom and styled-components are **bundled into `rete.min.js`** — they
are not separate runtime dependencies and nothing is fetched at page load. The
renderer injects its own styles into the document head at runtime, so there is
no companion CSS file to vendor.

The vanilla/lit renderer was evaluated first but `rete-lit-plugin` is
mis-packaged on npm (its root `package.json` omits `main`/`module` and its dist
peer deps are not resolvable), so the well-packaged, reference React renderer was
chosen. Because it is bundled, no React toolchain runs in dev or CI.

## Regeneration

The bundle is produced ONCE by `build/rete/build.sh`, which does all npm work in
a throwaway temp dir (no `node_modules`/`package-lock` ever land in the repo) and
copies only the built ESM blob back to this directory. Run it from the repo root:

```sh
make vendor-rete          # wraps build/rete/build.sh
# or directly:
build/rete/build.sh
```

Requires node + npm on PATH (developed against node v24 / npm 11). The bundle
entry (`build/rete/entry.mjs`) is the only hand-authored source; it re-exports
the toolkit and defines `createHelloCanvas`. To bump a version, edit the pins in
**both** `build/rete/build.sh` and the table above, re-run, then commit the new
`rete.min.js`.

`make build`, `make test` and `make vet` never invoke node — they consume the
committed blob. Only `make vendor-rete` touches node, and only when run by hand.
