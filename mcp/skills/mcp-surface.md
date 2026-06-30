---
name: mcp-surface
description: Aperture's read-only MCP surface — the decide, simulate, and inspect tools an agent drives over stdio, with the SDK-free-core + gosdk-adapter + import-firewall house pattern that keeps the protocol SDK out of the core.
applies_to: [mcp, service, engine, what-if]
---

# Aperture MCP surface

Aperture exposes a **read-only** Model Context Protocol surface: an agent may
**decide**, **simulate**, and **inspect**, but **never mutate**. Every tool calls
the single `service.Service` facade's read and decision methods; the facade's
mutators (Put*/Delete*, Bestow/Revoke, impersonation) are not surfaced, and a
test enumerates the registered tools to assert none carries a mutating verb.

Serve it with `aperture mcp` (stdio) — the transport an MCP client uses when it
spawns Aperture as a subprocess.

## Tools

### Decide (single + bulk)
- `aperture_check` — verdict + reason + deciding grant ids for one
  (account, principal, action, object). Fail-closed: an operational failure
  renders as DENY, never an allow-on-error.
- `aperture_check_batch` — many questions in one round-trip, results aligned with
  the input queries; one bad query never fails the batch.
- `aperture_enumerate` — the object ids under a pattern a principal may act on
  (the inverse of check); every id is one check would allow.
- `aperture_enumerate_batch` — bulk enumerate, aligned with queries.
- `aperture_explain` — the full decision trace: subject set, every grant
  considered with its per-grant outcome, the deciding grants, the verdict.
- `aperture_explain_batch` — bulk explain, aligned with queries.

### Simulate (what-if, never persisted)
- `aperture_simulate` — render the decision (full trace) for a question under a
  hypothetical overlay of principals, groups, permissions, grants, and
  memberships, **without writing anything**. The overlay is additive; an overlay
  entity with the same id as a stored one shadows it. Use it to preview
  "what if I bestowed this grant" before doing it. Nothing is audited.

### Inspect (read the model)
- `aperture_list_object_types` / `aperture_get_object_type`
- `aperture_list_permissions` / `aperture_get_permission`
- `aperture_list_roles` / `aperture_get_role`
- `aperture_list_groups` / `aperture_get_group`
- `aperture_list_principals` / `aperture_get_principal`
- `aperture_list_grants` (account-scoped) / `aperture_get_grant`

### Docs
- `aperture_skills_list` / `aperture_skills_get` — this skill pack.

## House pattern: SDK-free core + adapter + firewall

The MCP layer is split so that importing the tool contract never drags in the
protocol SDK:

- **`mcp/` core** is SDK-free. It defines the typed In/Out contract per tool,
  reflects a JSON Schema for each via `github.com/google/jsonschema-go`, and holds
  the handlers that call the `service.Service` facade. It imports the facade (and
  the domain types the facade returns) plus the schema reflector — **never** the
  MCP SDK.
- **`mcp/gosdk`** is the single adapter and the ONLY package allowed to import
  `github.com/modelcontextprotocol/go-sdk`. It mounts the SDK-free catalog onto a
  caller-supplied server via the low-level `Server.AddTool` path, adapting raw
  tool arguments into the core's type-erased `Invoke` and rendering coded errors
  as a structured `{code, message, details}` envelope.
- **`mcp/firewall_test.go`** runs `go list -deps` over the core packages and fails
  if the protocol SDK appears anywhere in their transitive dependency set — making
  the SDK-free guarantee load-bearing rather than aspirational.

Deeply-nested or self-referential schema fields are typed `any` so the schema
reflector never recurses (it errors on a Go-level type cycle); the current
contract types are all non-cyclic, and the schema test asserts reflection
produced no errors.
