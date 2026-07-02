# Introduction

**Aperture** is a fine-grained access-control engine for the `frankbardon/*`
family of services. It answers one question — *"is this principal allowed to do
this thing to this resource, and why?"* — consistently across every surface that
asks it.

## Library-first

Aperture is **library-first**: the public Go packages at the module root
(`github.com/frankbardon/aperture`) are the product. Every other surface — the
`aperture` CLI, the Twirp/HTTP RPC API, the MCP server, and the admin UI — is a
thin translator over a single decision engine. There is exactly one place a
decision is made, so the answer a script gets from the CLI is the same answer a
service gets over RPC and an agent gets over MCP.

## The decision API

The engine exposes three operations, each available in a single and a
bulk-batched form:

| Operation | Question it answers |
|---|---|
| **Check** | May this principal perform this action on this resource? |
| **Enumerate** | Which resources/actions is this principal allowed? |
| **Explain** | *Why* was a decision reached — which rules and grants applied? |

`Explain` is a first-class citizen, not a debugging afterthought: access
decisions are auditable by construction.

## What's inside

Aperture models principals (`identity`), the resource/object model (`model`,
`scope`), and object providers (`provider`) that resolve live attributes.
Decisions flow through the `engine`, driven by a rules layer (`rules`) that
compiles a rule AST to an [`expr-lang/expr`](https://github.com/expr-lang/expr)
expression and evaluates it in-process — pure-Go, no external policy service.
Grants come in several flavors (direct, `delegation`, `impersonation`), reads
are narrowed by scoped visibility (`filter`), and every decision can be recorded
to an audit log (`audit`). Persistence sits behind one `Storage` interface with
a hand-written SQL / `modernc.org/sqlite` implementation and an in-memory twin.

## Design tenets

- **Pure-Go, `CGO_ENABLED=0` end to end.** No CGO, no external policy engine.
- **Coded errors.** Every failure is an `APERTURE_*` error carrying a stable
  code and an actionable fixup — never a bare string.
- **No cross-account leakage.** Decisions and error messages never expose data
  from an account the caller cannot see.
- **One engine, many surfaces.** CLI, RPC, MCP, and UI are adapters; the
  decision logic lives once, in the library.

## Where to go next

- **Getting Started** — install Aperture and run your first `check`.
- **CLI & Library** — the `aperture` command tree and the Go embedding API.
- **Service Surfaces** — the Twirp/HTTP RPC API, MCP server, and admin UI.
- **Concepts** — identities, the object model, scopes, rules, grants, and audit.
- **Reference** — error codes, configuration, and the CLI/RPC surface tables.
- **Operations** — deployment, storage, and running the engine in production.
- **Internals** — architecture, package layout, and extension points.
- **Contributing** — conventions and the Update-Demand rule.
