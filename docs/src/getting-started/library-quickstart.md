# Library quickstart

Aperture is **library-first**: the CLI, the RPC API, the MCP server, and the
admin UI are all thin adapters over the same Go packages. This page embeds the
decision engine directly and asks it the same question the
[First decision (CLI)](first-decision-cli.md) walkthrough asked — this time from
Go.

## The pieces

A decision needs three collaborators, wired by hand (Aperture uses manual
dependency injection — no wire/fx/dig):

| Package | Role |
|---|---|
| `storage/memory` | A backing store for the model. The in-memory implementation is ideal for a demo; `storage/sqlite` is its persistent twin behind the same interface. |
| `engine` | The decision engine. `engine.New(store)` binds it to a store. |
| `service` | The facade every surface calls. `service.New(engine.New(store))` gives you `Check` / `Enumerate` / `Explain`. |
| `seed` | Loads a model into a store. `seed.Example` is the embedded `acme` fixture. |

## A minimal program

Save this as `main.go` inside a module that requires
`github.com/frankbardon/aperture`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/frankbardon/aperture/engine"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"
	"github.com/frankbardon/aperture/storage/memory"
)

func main() {
	ctx := context.Background()

	// Build an in-memory store and load the embedded example model (account "acme").
	store := memory.New()
	if err := store.Setup(ctx); err != nil {
		log.Fatal(err)
	}
	if err := seed.Load(ctx, store, seed.Example, seed.FormatYAML); err != nil {
		log.Fatal(err)
	}

	// The service facade is the single entry point every surface uses.
	svc := service.New(engine.New(store))

	res, err := svc.Check(ctx, service.Query{
		Account:   seed.ExampleAccount, // "acme"
		Principal: "alice",
		Action:    "read",
		Object:    "account:acme/project:atlas/document:42",
	})
	if err != nil {
		log.Fatalf("check failed: %v", err)
	}

	fmt.Printf("allow=%v\n", res.Allow)
	fmt.Printf("reason: %s\n", res.Reason)
	fmt.Printf("deciding grants: %v\n", res.DecidingGrantIDs)
}
```

Run it:

```bash
go run .
```

```text
allow=true
reason: allowed by grant g-eng-read-atlas (allow account:acme/project:atlas/**) at specificity 39300; 1 matching grant(s) considered
deciding grants: [g-eng-read-atlas]
```

That is the identical decision, reason, and deciding grant the CLI printed —
because it *is* the same code path. There is one engine; every surface is a
translator over it.

## The request and result types

`service.Check` takes a `service.Query` and returns a `service.Result`:

```go
type Query struct {
	Account   string // active account the decision is scoped to
	Principal string // id of the principal asking
	Action    string // the verb being attempted
	Object    string // canonical object-identity string
}

type Result struct {
	Allow            bool     // the verdict
	Reason           string   // human-readable explanation
	DecidingGrantIDs []string // grant ids that produced the verdict
}
```

`Result` never surfaces raw errors as denies inconsistently: an operational
failure fails **closed** (`Allow: false`) with the cause in `Reason`, while a
malformed request returns a non-nil `error` carrying an `APERTURE_*` code. Recover
that code with `errors.CodeOf` from the `errors` package.

## Errors are coded

Every failure Aperture returns across a package boundary is an `APERTURE_*` coded
error, not a bare string. When `Check` returns a non-nil error — for example, a
malformed object identity — inspect the code rather than the message:

```go
import aerr "github.com/frankbardon/aperture/errors"

if err != nil {
	switch aerr.CodeOf(err) {
	case aerr.APERTURE_INVALID_INPUT:
		// the query was malformed — fix the caller
	default:
		// something operational went wrong
	}
}
```

## Beyond `Check`

The facade exposes the whole read API — `Enumerate` (which objects a principal
may act on), `Explain` (the full decision trace), and batch forms of each —
plus, when constructed with the right options, the mutation path (entity CRUD,
grants, delegation, impersonation). A read-only `service.New(eng)` returns
`APERTURE_UNIMPLEMENTED` from any mutation, so a decision-only surface stays
minimal.

## Where to go next

- [Concepts primer](concepts.md) — the vocabulary behind `Query` and `Result`.
- The CLI & Library section of this book covers the full Go embedding API.
- The Reference section lists every `APERTURE_*` error code and its fixups.
