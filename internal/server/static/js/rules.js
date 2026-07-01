/*
 * rules.js — the Rules section Alpine component (E7-S1 hello-canvas).
 *
 * Proves the vendored Rete.js bundle (static/vendor/rete/rete.min.js) loads and
 * mounts an editor inside the embedded admin shell, with NO node build in dev or
 * CI. It is deliberately minimal: it dynamically imports the vendored ESM bundle
 * at runtime and mounts createHelloCanvas() — an empty editor seeded with one
 * placeholder node — to exercise the area + connection + render pipeline.
 *
 * A classic <script defer> like the other domain screens (crud.js, grants.js):
 * app.js registers its factory on window before Alpine walks the DOM. The ESM
 * bundle is pulled in via a runtime dynamic import(), so no module-script load
 * ordering is involved. E7-S2 replaces the placeholder graph with the real
 * node<->AST editor built on the toolkit the same bundle re-exports.
 */

function rules() {
  return {
    booting: true,
    error: "",
    _canvas: null,

    // init runs when the rules section's x-if creates this component (i.e. when
    // the section becomes active). mount() loads the bundle and renders.
    init() {
      this.mount();
    },

    async mount() {
      this.booting = true;
      this.error = "";
      try {
        // Runtime dynamic import of the committed, embedded ESM blob. Served by
        // the Go file server from the //go:embed static tree — no CDN, no build.
        const mod = await import("/vendor/rete/rete.min.js");
        const el = this.$refs.canvas;
        if (!el) {
          throw new Error("canvas container not found");
        }
        this._canvas = await mod.createHelloCanvas(el);
        this.booting = false;
      } catch (e) {
        this.error = e && e.message ? e.message : String(e);
        this.booting = false;
      }
    },

    // destroy tears the editor down when the section is left (x-if removes it).
    destroy() {
      if (this._canvas && typeof this._canvas.destroy === "function") {
        this._canvas.destroy();
      }
      this._canvas = null;
    },
  };
}

window.rules = rules;
