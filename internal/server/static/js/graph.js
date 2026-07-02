/*
 * graph.js — Aperture model visualizer (Alpine.js component + self-contained
 * force-directed graph on <canvas>).
 *
 * WHY canvas + a hand-rolled sim: the admin UI is deliberately dependency-light
 * (no CDN, no vendored icon JS). At this model's scale (tens of nodes) a small
 * velocity-Verlet simulation — charge repulsion, link springs, centering and
 * collision, with alpha cooling — gives smooth, d3-quality motion without a
 * layout library. Heavy simulation state (nodes/links/transform) is kept OUTSIDE
 * Alpine's reactive proxy (module-level SIM) so the 60fps render loop never
 * triggers Alpine reactivity; only small UI state (toggles, filter, the side
 * panel) lives on the component.
 *
 * DATA: one Export RPC returns the whole declarative model. Every node and edge
 * is derived from it, so the graph is always the live store.
 */

// Palette — node fill by type. Chosen for contrast on the dark canvas.
const NODE_STYLE = {
  account: { color: "#6366f1", label: "Account" },
  principal: { color: "#10b981", label: "Principal" },
  role: { color: "#f59e0b", label: "Role" },
  group: { color: "#ec4899", label: "Group" },
  object: { color: "#38bdf8", label: "Object" },
  permission: { color: "#a78bfa", label: "Permission" },
  template: { color: "#94a3b8", label: "Template" },
};

// Edge styling by relationship kind.
const EDGE_STYLE = {
  member: { color: "#64748b", width: 1.2, dash: null },          // principal -> account
  "has-role": { color: "#78716c", width: 1.2, dash: null },      // principal -> role
  "in-group": { color: "#a8567f", width: 1.2, dash: null },      // principal -> group
  allow: { color: "#22c55e", width: 1.6, dash: null },           // grant (allow)
  deny: { color: "#ef4444", width: 1.6, dash: [5, 4] },          // grant (deny)
  delegate: { color: "#22c55e", width: 1.4, dash: [2, 3] },      // grant on aperture.delegate
  "role-perm": { color: "#a78bfa", width: 1, dash: [1, 3] },     // role -> permission (doc)
  "tmpl-perm": { color: "#94a3b8", width: 1.1, dash: [4, 3] },   // template -> permission
};

const WILDCARD = "*";

// SIM holds all non-reactive simulation state. Module-level (single graph
// instance) so the render loop reads it directly without Alpine proxying.
let SIM = null;

function graph() {
  return {
    // ---- reactive UI state (small; safe to proxy) ----
    loading: false,
    error: null,
    ready: false,          // a model has been loaded and laid out
    principal: localStorage.getItem("aperture.devToken") || "",
    counts: { nodes: 0, edges: 0 },
    search: "",
    layers: { accounts: true, objects: true, permissions: false, templates: false },
    enforcement: true,     // reachability honors membership enforcement
    reachId: "",           // node id currently in reachability focus ("" = off)
    selected: null,        // { title, type, rows:[[k,v]] } for the side panel
    accountList: [],       // for the filter dropdown
    accountFilter: "",     // "" = all accounts

    init() {
      // The section is toggled by the shell's hash router; drive off the hash
      // directly rather than watching the parent's Alpine state (robust, and
      // decoupled from the shell component's internals).
      window.addEventListener("hashchange", () => { if (this.onGraphRoute()) this.ensureLoaded(); });
      window.addEventListener("resize", () => { if (this.ready) this.resize(); });
      document.addEventListener("aperture:authenticated", (e) => {
        this.principal = e.detail.principal;
        if (this.onGraphRoute()) this.load();
      });
      document.addEventListener("aperture:signout", () => {
        this.principal = "";
        this.teardown();
      });
      if (this.onGraphRoute()) this.ensureLoaded();
    },

    onGraphRoute() {
      return (window.location.hash || "").replace(/^#\/?/, "") === "graph";
    },

    ensureLoaded() {
      if (this.principal && !this.ready && !this.loading) this.load();
      else if (this.ready) this.$nextTick(() => this.resize());
    },

    // actor mirrors crud.js: the Twirp Actor is a message {principal, account}.
    // The account is where admin authority resolves; a system-admin (or "*"
    // member) is admitted in any account, so the first account is sufficient.
    actor() { return { principal: this.principal, account: this.account }; },
    account: "",

    // ---- load + build ----

    async load() {
      this.loading = true;
      this.error = null;
      try {
        // Export authority resolves against an account, so seed one first (any
        // account works for a system-admin / "*" member — Export reads the whole
        // model regardless).
        if (!this.account) {
          const ar = await rpc("ListAccounts", {});
          const accts = (ar.entities_json || []).map((s) => JSON.parse(s));
          if (accts.length) this.account = accts[0].ID;
        }
        const resp = await rpc("Export", { actor: this.actor() });
        const doc = JSON.parse(resp.document_json || "{}");
        this.build(doc);
        this.ready = true;
        this.$nextTick(() => this.mountCanvas());
      } catch (e) {
        if (e.status !== 401) this.error = { code: e.code || "APERTURE_ERROR", msg: e.message };
      } finally {
        this.loading = false;
      }
    },

    // build derives nodes + links from the Export document and seeds SIM.
    build(doc) {
      const nodes = new Map();
      const links = [];
      const node = (type, id, label, meta) => {
        const key = type + " " + id;
        if (!nodes.has(key)) {
          nodes.set(key, {
            key, type, rid: id, label: label || id, meta: meta || {},
            deg: 0, x: 0, y: 0, vx: 0, vy: 0, fx: null, fy: null,
          });
        }
        return nodes.get(key);
      };
      const link = (a, b, kind, meta) => {
        if (!a || !b) return;
        a.deg++; b.deg++;
        links.push({ source: a, target: b, kind, meta: meta || {} });
      };

      (doc.accounts || []).forEach((a) => node("account", a.id, a.name || a.id, a));
      // A synthetic wildcard-account node makes cross-account "*" reach visible.
      const anyAccount = node("account", WILDCARD, "All accounts (*)", { wildcard: true });

      const principalsById = {};
      (doc.principals || []).forEach((p) => {
        const pn = node("principal", p.id, p.display_name || p.id, p);
        principalsById[p.id] = pn;
        (p.roles || []).forEach((r) => link(pn, node("role", r, r, {}), "has-role"));
      });
      (doc.roles || []).forEach((r) => {
        const rn = node("role", r.id, r.name || r.id, r);
        (r.permissions || []).forEach((pid) => link(rn, node("permission", pid, pid, {}), "role-perm"));
      });
      (doc.groups || []).forEach((g) => {
        const gn = node("group", g.id, g.name || g.id, g);
        (g.members || []).forEach((m) => link(principalsById[m] || node("principal", m, m, {}), gn, "in-group"));
      });
      (doc.permissions || []).forEach((p) => node("permission", p.id, p.id, p));
      (doc.memberships || []).forEach((m) => {
        const acct = m.account === WILDCARD ? anyAccount : node("account", m.account, m.account, {});
        link(principalsById[m.principal] || node("principal", m.principal, m.principal, {}), acct, "member");
      });

      // Grants: subject -> object, styled by effect / delegate. The permission and
      // account ride as edge metadata (shown in the side panel and reachability).
      const permByID = {};
      (doc.permissions || []).forEach((p) => (permByID[p.id] = p));
      (doc.grants || []).forEach((g) => {
        const subj = this.subjectNode(node, principalsById, g.subject);
        const obj = node("object", g.object, g.object, { wildcard: /[*]/.test(g.object) });
        const perm = permByID[g.permission] || {};
        const isDelegate = (perm.action || "").indexOf("delegate") >= 0;
        const kind = g.effect === "deny" ? "deny" : (isDelegate ? "delegate" : "allow");
        link(subj, obj, kind, { grant: g, permission: perm });
      });

      (doc.templates || []).forEach((t) => {
        const tn = node("template", t.name, t.name, t);
        (t.grants || []).forEach((tg) => {
          if (tg.permission) link(tn, node("permission", tg.permission, tg.permission, {}), "tmpl-perm");
        });
      });

      const list = [...nodes.values()];
      // Radius by degree so hubs (e.g. the shared `manager` role) read bigger,
      // but kept small (dot-like, per the reference) and capped so no single hub
      // dominates.
      list.forEach((n) => (n.r = Math.min(15, 3.5 + Math.sqrt(n.deg) * 1.9)));

      // Seed on a phyllotaxis (sunflower) spiral inside a disc: an even, isotropic
      // start with no directional bias, so the organic force layout relaxes into a
      // tidy circular envelope (charge + centering + collision do the rest). No
      // Math.random (banned here); the golden-angle spiral is deterministic.
      const GOLDEN = Math.PI * (3 - Math.sqrt(5));
      const spread = 48;
      list.forEach((n, i) => {
        const rad = spread * Math.sqrt(i + 0.5);
        const ang = i * GOLDEN;
        n.x = Math.cos(ang) * rad;
        n.y = Math.sin(ang) * rad;
      });

      SIM = {
        nodes: list, links, alpha: 1, alphaMin: 0.001, alphaDecay: 0.011,
        k: 1, tx: 0, ty: 0, canvas: null, ctx: null, dpr: 1,
        raf: null, drag: null, hover: null, pan: null,
      };
      this.counts = { nodes: list.length, edges: links.length };
      this.accountList = [...new Set((doc.accounts || []).map((a) => a.id))].sort();
    },

    subjectNode(node, principalsById, s) {
      if (!s) return null;
      if (s.kind === "principal") return principalsById[s.id] || node("principal", s.id, s.id, {});
      if (s.kind === "role") return node("role", s.id, s.id, {});
      if (s.kind === "group") return node("group", s.id, s.id, {});
      return node(s.kind || "principal", s.id, s.id, {});
    },

    // ---- canvas mount + resize ----

    mountCanvas() {
      const canvas = this.$refs.canvas;
      if (!canvas || !SIM) return;
      SIM.canvas = canvas;
      SIM.ctx = canvas.getContext("2d");
      SIM.sized = false;
      this.bindPointer(canvas);
      // The section may still be hidden (0×0) when we mount — e.g. a hard reload
      // straight to #/graph, before the shell's hash router reveals it. A
      // ResizeObserver drives sizing so the FIRST time the wrap gets real
      // dimensions we size the canvas, fit the view, and reheat; it also handles
      // later container/window resizes for free.
      if (SIM.ro) SIM.ro.disconnect();
      SIM.ro = new ResizeObserver(() => this.resize());
      SIM.ro.observe(canvas.parentElement);
      this.resize();
      this.tick();
    },

    resize() {
      if (!SIM || !SIM.canvas) return;
      const wrap = SIM.canvas.parentElement;
      const w = wrap.clientWidth, h = wrap.clientHeight;
      if (w === 0 || h === 0) return; // still hidden — don't zero the canvas
      const dpr = window.devicePixelRatio || 1;
      SIM.dpr = dpr;
      SIM.canvas.width = w * dpr;
      SIM.canvas.height = h * dpr;
      SIM.canvas.style.width = w + "px";
      SIM.canvas.style.height = h + "px";
      if (!SIM.sized) {
        // First real size: fit the layout and give the sim a nudge to settle.
        SIM.sized = true;
        this.centerView();
        this.reheat();
      }
    },

    centerView() {
      if (!SIM || !SIM.canvas) return;
      // Fit to the ACTUAL bounding box of the nodes (with padding) so the layout
      // fills the canvas instead of floating in the middle. Uses visible nodes so
      // toggling a layer re-frames sensibly.
      const W = SIM.canvas.width / SIM.dpr, H = SIM.canvas.height / SIM.dpr;
      const vis = SIM.nodes.filter((n) => this.isVisible(n));
      const use = vis.length ? vis : SIM.nodes;
      let minx = Infinity, maxx = -Infinity, miny = Infinity, maxy = -Infinity;
      for (const n of use) {
        minx = Math.min(minx, n.x - n.r); maxx = Math.max(maxx, n.x + n.r);
        miny = Math.min(miny, n.y - n.r); maxy = Math.max(maxy, n.y + n.r);
      }
      const pad = 120; // generous margin → the round envelope floats, not edge-to-edge
      const bw = (maxx - minx) || 1, bh = (maxy - miny) || 1;
      SIM.k = Math.max(0.2, Math.min(1.6, Math.min((W - pad) / bw, (H - pad) / bh)));
      SIM.tx = W / 2 - ((minx + maxx) / 2) * SIM.k;
      SIM.ty = H / 2 - ((miny + maxy) / 2) * SIM.k;
      SIM.fitK = SIM.k; // remember the "fit" zoom so label-reveal is relative to it
    },

    // isVisible mirrors the render-time layer test (used for fit + fitting only).
    isVisible(n) {
      if (!this.layers.objects && n.type === "object") return false;
      if (!this.layers.permissions && n.type === "permission") return false;
      if (!this.layers.templates && n.type === "template") return false;
      if (!this.layers.accounts && n.type === "account") return false;
      return true;
    },

    teardown() {
      if (SIM && SIM.raf) cancelAnimationFrame(SIM.raf);
      if (SIM && SIM.ro) SIM.ro.disconnect();
      SIM = null;
      this.ready = false;
    },

    // ---- force simulation (velocity Verlet + cooling) ----

    reheat() { if (SIM) SIM.alpha = Math.max(SIM.alpha, 0.7); },

    step() {
      const s = SIM;
      if (!s) return;
      const nodes = s.nodes, links = s.links, a = s.alpha;

      // Charge: pairwise repulsion (O(n^2), fine at this scale). Stronger than a
      // typical d3 default so labels get breathing room; capped at close range so
      // two coincident nodes don't fling apart violently.
      const CHARGE = -3600;
      for (let i = 0; i < nodes.length; i++) {
        const ni = nodes[i];
        for (let j = i + 1; j < nodes.length; j++) {
          const nj = nodes[j];
          let dx = ni.x - nj.x, dy = ni.y - nj.y;
          let d2 = dx * dx + dy * dy;
          if (d2 < 0.01) { dx = (i - j) * 0.5 + 0.1; dy = 0.3; d2 = dx * dx + dy * dy; }
          if (d2 < 400) d2 = 400; // clamp near-field so repulsion stays bounded
          const f = (CHARGE * a) / d2;
          const d = Math.sqrt(dx * dx + dy * dy) || 1;
          const fx = (dx / d) * f, fy = (dy / d) * f;
          ni.vx -= fx; ni.vy -= fy;
          nj.vx += fx; nj.vy += fy;
        }
      }
      // Link springs — a longer rest length gives the airy, spread-out stars of
      // the reference (satellites orbit their hub at a distance).
      const REST = 115, STR = 0.06;
      for (const l of links) {
        const dx = l.target.x - l.source.x, dy = l.target.y - l.source.y;
        const d = Math.sqrt(dx * dx + dy * dy) || 1;
        const f = ((d - REST) * STR * a);
        const fx = (dx / d) * f, fy = (dy / d) * f;
        l.source.vx += fx; l.source.vy += fy;
        l.target.vx -= fx; l.target.vy -= fy;
      }
      // Centering: a single attraction toward the origin (strength scales mildly
      // with distance) balances the charge repulsion into a round, evenly-filled
      // DISC — no type grouping, just an organic circular envelope.
      const DECAY = 0.85, CENTER = 0.019;
      for (const n of nodes) {
        n.vx += (-n.x) * CENTER * a;
        n.vy += (-n.y) * CENTER * a;
        if (n.fx != null) { n.x = n.fx; n.vx = 0; }
        if (n.fy != null) { n.y = n.fy; n.vy = 0; }
        n.vx *= DECAY; n.vy *= DECAY;
        n.x += n.vx; n.y += n.vy;
      }
      // Collision resolution — modest even padding so spacing is uniform and tight
      // (matching the reference) without overlaps.
      for (let pass = 0; pass < 3; pass++) {
        for (let i = 0; i < nodes.length; i++) {
          const ni = nodes[i];
          for (let j = i + 1; j < nodes.length; j++) {
            const nj = nodes[j];
            const min = ni.r + nj.r + 14;
            let dx = nj.x - ni.x, dy = nj.y - ni.y;
            let d = Math.sqrt(dx * dx + dy * dy) || 0.01;
            if (d < min) {
              const push = (min - d) / 2;
              const ux = dx / d, uy = dy / d;
              if (ni.fx == null) { ni.x -= ux * push; ni.y -= uy * push; }
              if (nj.fx == null) { nj.x += ux * push; nj.y += uy * push; }
            }
          }
        }
      }
      s.alpha += (s.alphaMin - s.alpha) * s.alphaDecay;
    },

    tick() {
      const s = SIM;
      if (!s) return;
      if (s.alpha > s.alphaMin) this.step();
      else if (!s.fitted && s.sized) {
        // One auto-fit the first time the layout settles: the mount-time fit
        // framed the un-relaxed seed positions; now frame the real result.
        s.fitted = true;
        this.centerView();
      }
      this.render();
      s.raf = requestAnimationFrame(() => this.tick());
    },

    // ---- reachability ----

    // reachSet computes the node keys + link set a principal can reach, honoring
    // subject expansion (self ∪ roles ∪ groups), account-scoping, the "*" account,
    // and (optionally) membership enforcement.
    reachSet() {
      const s = SIM;
      const focus = s && s.nodes.find((n) => n.key === this.reachId);
      if (!focus || focus.type !== "principal") return null;
      const p = focus.meta;
      const subjectKeys = new Set([focus.key]);
      (p.roles || []).forEach((r) => subjectKeys.add("role " + r));
      s.nodes.forEach((n) => {
        if (n.type === "group" && (n.meta.members || []).includes(p.id)) subjectKeys.add(n.key);
      });
      // Accounts the principal belongs to (member edges), plus "*" if enrolled there.
      const accounts = new Set();
      let wildcardMember = false;
      s.links.forEach((l) => {
        if (l.kind === "member" && l.source.key === focus.key) {
          if (l.target.rid === WILDCARD) wildcardMember = true;
          else accounts.add(l.target.rid);
        }
      });
      const nodeKeys = new Set([...subjectKeys]);
      accounts.forEach((a) => nodeKeys.add("account " + a));
      if (wildcardMember) nodeKeys.add("account " + WILDCARD);
      const linkSet = new Set();
      s.links.forEach((l, idx) => {
        // subject membership + has-role/in-group edges into the subject set
        if (subjectKeys.has(l.source.key) && (l.kind === "member" || l.kind === "has-role")) linkSet.add(idx);
        if (l.kind === "in-group" && subjectKeys.has(l.target.key)) linkSet.add(idx);
        // grant edges from a subject the principal expands to
        if (["allow", "deny", "delegate"].includes(l.kind) && subjectKeys.has(l.source.key)) {
          const g = l.meta.grant || {};
          const inAccount = g.account === WILDCARD || accounts.has(g.account) || (wildcardMember);
          const enforced = !this.enforcement || inAccount;
          if (inAccount && enforced && l.kind !== "deny") {
            nodeKeys.add(l.target.key);
            linkSet.add(idx);
          }
        }
      });
      return { nodeKeys, linkSet };
    },

    // ---- render ----

    matchSearch(n) {
      const q = this.search.trim().toLowerCase();
      if (!q) return false;
      return n.rid.toLowerCase().includes(q) || (n.label || "").toLowerCase().includes(q);
    },

    render() {
      const s = SIM;
      if (!s || !s.ctx) return;
      const ctx = s.ctx;
      ctx.setTransform(s.dpr, 0, 0, s.dpr, 0, 0);
      const W = s.canvas.width / s.dpr, H = s.canvas.height / s.dpr;
      ctx.clearRect(0, 0, W, H);
      ctx.save();
      ctx.translate(s.tx, s.ty);
      ctx.scale(s.k, s.k);

      const reach = this.reachId ? this.reachSet() : null;
      const hoverKey = s.hover ? s.hover.key : null;
      const neighbors = new Set();
      if (hoverKey) {
        neighbors.add(hoverKey);
        s.links.forEach((l) => {
          if (l.source.key === hoverKey) neighbors.add(l.target.key);
          if (l.target.key === hoverKey) neighbors.add(l.source.key);
        });
      }
      const activeFilter = this.accountFilter;

      const visibleNode = (n) => {
        if (!this.layers.objects && n.type === "object") return false;
        if (!this.layers.permissions && n.type === "permission") return false;
        if (!this.layers.templates && n.type === "template") return false;
        if (!this.layers.accounts && n.type === "account") return false;
        return true;
      };
      // Dimming: reachability focus > hover neighborhood > none.
      const dimNode = (n) => {
        if (reach) return !reach.nodeKeys.has(n.key);
        if (hoverKey) return !neighbors.has(n.key);
        return false;
      };

      // Edges first. Connectors are COLOURLESS — a single faint neutral stroke,
      // thin and constant on screen at any zoom (width and dash are divided by k to
      // cancel the canvas scale). Relationship kind survives only as line SHAPE
      // (solid allow / dashed deny / dotted delegate), not colour.
      const focused = reach || hoverKey;
      ctx.lineCap = "round";
      ctx.strokeStyle = "#8b98ad";
      s.links.forEach((l, idx) => {
        if (!visibleNode(l.source) || !visibleNode(l.target)) return;
        const st = EDGE_STYLE[l.kind] || EDGE_STYLE.member;
        let dim = false;
        if (reach) dim = !reach.linkSet.has(idx);
        else if (hoverKey) dim = !(l.source.key === hoverKey || l.target.key === hoverKey);
        if (activeFilter && l.meta.grant && l.meta.grant.account !== activeFilter && l.meta.grant.account !== WILDCARD) dim = true;
        ctx.globalAlpha = dim ? 0.04 : (focused ? 0.7 : 0.22);
        ctx.lineWidth = 0.9 / s.k;
        ctx.setLineDash(st.dash ? st.dash.map((v) => v / s.k) : []);
        ctx.beginPath();
        ctx.moveTo(l.source.x, l.source.y);
        ctx.lineTo(l.target.x, l.target.y);
        ctx.stroke();
      });
      ctx.setLineDash([]);

      // Nodes.
      s.nodes.forEach((n) => {
        if (!visibleNode(n)) return;
        const style = NODE_STYLE[n.type] || NODE_STYLE.object;
        const dim = dimNode(n);
        const hit = this.matchSearch(n);
        ctx.globalAlpha = dim && !hit ? 0.12 : 1;
        ctx.beginPath();
        ctx.arc(n.x, n.y, n.r, 0, Math.PI * 2);
        ctx.fillStyle = style.color;
        ctx.fill();
        // Wildcard accounts/objects get a gold ring — the cross-account reach cue.
        if (n.meta && n.meta.wildcard) {
          ctx.lineWidth = 2.5; ctx.strokeStyle = "#eab308"; ctx.stroke();
        } else if (hit) {
          ctx.lineWidth = 3; ctx.strokeStyle = "#fde047"; ctx.stroke();
        } else {
          ctx.lineWidth = 1.5; ctx.strokeStyle = "rgba(15,23,42,0.9)"; ctx.stroke();
        }
      });

      // LABELS in a second pass so they sit above every node/edge (never buried),
      // with a background pill for legibility. Clean by default (like the
      // reference): NO labels at rest — only the hovered/selected/searched node
      // and its focused neighbourhood are named. Zoom past k > 1.3 to reveal all.
      const selKey = this.selected ? this.selected.type + " " + this.selected.id : null;
      // Reveal all labels only when zoomed in well past the fit — so the default
      // framed view stays clean regardless of how big the fit zoom happens to be.
      const showAll = s.k > (s.fitK || 1) * 1.7;
      const fontPx = Math.max(7.5, 10 / Math.sqrt(s.k));
      ctx.font = `${fontPx}px ui-monospace, monospace`;
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      s.nodes.forEach((n) => {
        if (!visibleNode(n)) return;
        const hit = this.matchSearch(n);
        const dim = dimNode(n);
        const focused = reach || hoverKey;
        const show = hit || n.key === selKey || (focused ? !dim : showAll);
        if (!show) return;
        const label = this.short(n.label);
        const w = ctx.measureText(label).width;
        const ly = n.y + n.r + fontPx * 0.9 + 3;
        ctx.globalAlpha = dim && !hit ? 0.35 : 1;
        ctx.fillStyle = "rgba(8,13,24,0.72)";
        const padX = 3, h = fontPx + 3;
        this.roundRect(ctx, n.x - w / 2 - padX, ly - h / 2, w + padX * 2, h, 3);
        ctx.fill();
        ctx.fillStyle = hit ? "#fde047" : "#dbe4f0";
        ctx.fillText(label, n.x, ly);
      });
      ctx.restore();
    },

    roundRect(ctx, x, y, w, h, r) {
      ctx.beginPath();
      ctx.moveTo(x + r, y);
      ctx.arcTo(x + w, y, x + w, y + h, r);
      ctx.arcTo(x + w, y + h, x, y + h, r);
      ctx.arcTo(x, y + h, x, y, r);
      ctx.arcTo(x, y, x + w, y, r);
      ctx.closePath();
    },

    short(s) { return s && s.length > 22 ? s.slice(0, 20) + "…" : s; },

    // ---- pointer: drag nodes, pan, zoom, select ----

    bindPointer(canvas) {
      const toWorld = (ev) => {
        const rect = canvas.getBoundingClientRect();
        const sx = ev.clientX - rect.left, sy = ev.clientY - rect.top;
        return { x: (sx - SIM.tx) / SIM.k, y: (sy - SIM.ty) / SIM.k, sx, sy };
      };
      const pick = (w) => {
        let best = null, bestD = Infinity;
        for (const n of SIM.nodes) {
          const dx = n.x - w.x, dy = n.y - w.y;
          const d = dx * dx + dy * dy;
          if (d < (n.r + 4) * (n.r + 4) && d < bestD) { best = n; bestD = d; }
        }
        return best;
      };

      canvas.addEventListener("pointerdown", (ev) => {
        canvas.setPointerCapture(ev.pointerId);
        const w = toWorld(ev);
        const n = pick(w);
        if (n) { SIM.drag = { node: n, moved: false }; n.fx = n.x; n.fy = n.y; this.reheat(); }
        else SIM.pan = { x: ev.clientX, y: ev.clientY, tx: SIM.tx, ty: SIM.ty };
      });
      canvas.addEventListener("pointermove", (ev) => {
        const w = toWorld(ev);
        if (SIM.drag) {
          SIM.drag.node.fx = w.x; SIM.drag.node.fy = w.y; SIM.drag.moved = true;
          this.reheat();
        } else if (SIM.pan) {
          SIM.tx = SIM.pan.tx + (ev.clientX - SIM.pan.x);
          SIM.ty = SIM.pan.ty + (ev.clientY - SIM.pan.y);
        } else {
          SIM.hover = pick(w);
          canvas.style.cursor = SIM.hover ? "pointer" : "grab";
        }
      });
      const end = (ev) => {
        if (SIM.drag) {
          const n = SIM.drag.node;
          if (!SIM.drag.moved) this.onSelect(n);
          n.fx = null; n.fy = null;   // release so it settles
          SIM.drag = null;
        } else if (SIM.pan) {
          SIM.pan = null;
        }
      };
      canvas.addEventListener("pointerup", end);
      canvas.addEventListener("pointercancel", end);
      canvas.addEventListener("wheel", (ev) => {
        ev.preventDefault();
        const w = toWorld(ev);
        const factor = ev.deltaY < 0 ? 1.1 : 1 / 1.1;
        const k = Math.min(3, Math.max(0.2, SIM.k * factor));
        // Zoom around the cursor.
        SIM.tx = w.sx - w.x * k;
        SIM.ty = w.sy - w.y * k;
        SIM.k = k;
      }, { passive: false });
    },

    onSelect(n) {
      const rows = [["Type", NODE_STYLE[n.type] ? NODE_STYLE[n.type].label : n.type], ["Id", n.rid]];
      if (n.type === "principal") {
        rows.push(["Roles", (n.meta.roles || []).join(", ") || "—"]);
      }
      if (n.type === "group") rows.push(["Members", (n.meta.members || []).join(", ") || "—"]);
      if (n.type === "permission" && n.meta.action) {
        rows.push(["Action", n.meta.action]);
        rows.push(["Object type", n.meta.object_type || "—"]);
        if (n.meta.delegatable) rows.push(["Delegatable", "yes"]);
      }
      // Incident grants (as a subject) — the "what can this reach" preview.
      const outs = SIM.links.filter((l) => l.source.key === n.key && ["allow", "deny", "delegate"].includes(l.kind));
      if (outs.length) {
        rows.push(["Grants", outs.map((l) => `${l.meta.permission.id || "?"} ${l.kind === "deny" ? "✗" : "→"} ${l.target.rid} @${l.meta.grant.account}`).join("\n")]);
      }
      this.selected = { title: n.label, type: n.type, id: n.rid, canReach: n.type === "principal", rows };
    },

    focusReach() {
      if (!this.selected) return;
      this.reachId = "principal " + this.selected.id;
    },
    clearReach() { this.reachId = ""; },

    resetView() { this.centerView(); this.reheat(); },
  };
}

window.graph = graph;
