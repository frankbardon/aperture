# Concepts primer

Aperture answers one question — *"is this principal allowed to do this action to
this object, and why?"* This page defines the vocabulary that question is built
from. It is deliberately brief: enough to read any later chapter. Each term links
forward to its full treatment in the **Concepts** section of the book.

Every example on this page is drawn from the committed example model (account
`acme`), the same fixture the [First decision (CLI)](first-decision-cli.md)
walkthrough uses.

## Principal

A **principal** is the actor a decision is made *for* — a user, a service, or
any other identity that can attempt an action. In the example model, `alice`
and `bob` are principals. Each principal has a stable id, a kind, an identity
string (such as `user:alice`), and a set of roles and group memberships that
carry its grants. The `identity` package owns this model.

## Action

An **action** is the verb being attempted — `read`, `write`, `delete`, `share`.
Actions are declared per object type: a `document` in the example model permits
`read`, `write`, `delete`, and `share`. A decision always names exactly one
action.

## Object

An **object** is the resource an action targets, named by a canonical
**object-identity** string. Identities are hierarchical, path-like, and always
rooted at an account. In the example model a document is written as:

```text
account:acme/project:atlas/document:42
```

Each `type:id` segment narrows the path. This structure is what lets a single
grant cover a whole subtree while a more specific grant overrides it for one
object. The `model` package defines object types and identities; live object
attributes are resolved by object **providers** (the `provider` package).

## Identity patterns and specificity

A **pattern** is an object-identity with wildcards, used by a grant to cover many
objects at once. `**` matches any remaining path; a plain segment matches
exactly. In the example model:

```text
account:acme/project:atlas/**              # every object under project atlas
account:acme/project:atlas/document:secret # exactly one document
```

**Specificity** decides which grant wins when several match. A more specific
pattern (one that matches fewer objects) outranks a broader one. That is how the
example model seals a single document: a broad *allow* on
`account:acme/project:atlas/**` lets engineering read everything, while a
narrower *deny* on `account:acme/project:atlas/document:secret` overrides it for
that one object. Deny-overrides plus specificity is the core resolution rule.

## Grant

A **grant** binds a subject (a principal, role, or group), a permission (an
object-type + action pair), and an object pattern, with an `effect` of `allow`
or `deny`. Grants are the raw material of every decision. They come in several
flavors: direct grants, time-bounded **delegations** (`delegation`), and
**impersonation** grants that let one principal act as another (`impersonation`).

## Rule

A **rule** is a conditional predicate that gates a grant on the attributes of the
request — the principal, the object, or the environment. Rules are authored as an
AST that Aperture compiles to an [`expr-lang/expr`](https://github.com/expr-lang/expr)
expression and evaluates in-process; there is no external policy service and no
Pulse dependency. The `rules` package owns compilation and caching.

## Scope

A **scope** narrows what a principal can see or act on within an account —
Aperture's mechanism for row-level and subtree-level visibility. Scoped reads are
narrowed by the `filter` package so a listing never returns objects the caller is
not entitled to see. The `scope` package defines the scoping model.

## Account

An **account** is the top-level tenant boundary. Every object identity is rooted
at an account (`account:acme/...`), every grant is stamped to an account, and
every decision is scoped to one active account. Accounts are hard isolation
boundaries: a decision or an error message never leaks data from an account the
caller cannot see. Principals join an account through a **membership**, which the
engine can optionally enforce (a non-member is denied). Accounts are modeled as
entities in the `model` package.

## How they fit together

A `Check` takes an **account**, a **principal**, an **action**, and an
**object**. Aperture gathers every **grant** whose subject includes the principal
and whose pattern matches the object, evaluates any attached **rules**, applies
**deny-overrides** by **specificity**, and returns a verdict plus a reason naming
the deciding grants. The same resolution drives `Enumerate` (which objects a
principal may act on) and `Explain` (the full decision trace).

## Where to go next

- [First decision (CLI)](first-decision-cli.md) — see these terms resolve into a
  real verdict.
- [Library quickstart](library-quickstart.md) — ask the same question from Go.
- The **Concepts** section of this book expands each term above into its own
  chapter (identities, the object model, patterns and specificity, rules,
  scopes, grants, and accounts).
