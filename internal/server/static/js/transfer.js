/*
 * transfer.js — a reusable dual-pane "transfer list" selector (Alpine.js).
 *
 * Replaces the flat checkbox lists that pick a subset from a growing collection
 * (account members, a principal's accounts, a role's permissions, a group's
 * members, bulk template targets). Those lists become unwieldy as the model
 * grows; a transfer list keeps AVAILABLE and SELECTED in two filterable panes so
 * the current selection is always legible no matter how long the option list is.
 *
 * It owns NO selection state of its own — each call site keeps its existing
 * store (an array, or an id→bool map) and its existing toggle function, and this
 * component is a pure view over them. That is what lets one component serve every
 * site without reshaping four different state models. A config object supplies:
 *
 *   items  — the full option list as [{value, label}, …]. May be an array or a
 *            () => array thunk when the underlying list is itself reactive.
 *   has    — (value) => bool: is this option currently selected?
 *   toggle — (value) => void: flip this option's membership (the call site's own
 *            add/remove logic; the component never mutates the store directly).
 *
 * Reads inside has()/items() stay reactive because Alpine tracks the property
 * access while evaluating the pane getters, so moving an item re-renders both
 * panes. Pass config methods with `this.` (e.g. `toggle: v => this.toggleMember(v)`)
 * so the call site's method keeps its own `this` binding.
 */
function transferList(cfg) {
  return {
    availQuery: "", // filter text for the AVAILABLE pane
    selQuery: "", // filter text for the SELECTED pane

    _all() {
      return (typeof cfg.items === "function" ? cfg.items() : cfg.items) || [];
    },
    _match(label, q) {
      q = (q || "").trim().toLowerCase();
      return q === "" || String(label == null ? "" : label).toLowerCase().includes(q);
    },

    // pick moves an item across (select if available, deselect if selected) by
    // delegating to the call site's toggle — the single source of truth.
    pick(value) {
      cfg.toggle(value);
    },

    get available() {
      return this._all().filter((o) => !cfg.has(o.value) && this._match(o.label, this.availQuery));
    },
    get chosen() {
      return this._all().filter((o) => cfg.has(o.value) && this._match(o.label, this.selQuery));
    },
    get selectedCount() {
      return this._all().reduce((n, o) => n + (cfg.has(o.value) ? 1 : 0), 0);
    },
    get totalCount() {
      return this._all().length;
    },

    // addAll / removeAll act on the CURRENTLY FILTERED pane only, so a filter
    // then "Add all" is a scoped bulk action rather than an all-or-nothing one.
    // Snapshot the list first: toggling mutates membership and would otherwise
    // shift the array out from under the iteration.
    addAll() {
      this.available.slice().forEach((o) => cfg.toggle(o.value));
    },
    removeAll() {
      this.chosen.slice().forEach((o) => cfg.toggle(o.value));
    },
  };
}

window.transferList = transferList;
