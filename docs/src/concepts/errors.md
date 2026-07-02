# Error taxonomy

Every failure surfaced by the Aperture library is an **`APERTURE_*` coded error**.
Codes exist so the CLI, Twirp/HTTP, and MCP surfaces can translate a failure to a
transport-appropriate status **without string-matching human-readable messages** —
the code is the stable contract, the message is not.

> This page is the *concept*. The exhaustive, generated list of every code with its
> message and fixups is the [Error Codes reference](../reference/error-codes.md),
> produced from the `Registry` in `errors/codes.go`.

## The coded-error type

`CodedError` is the canonical error type:

```go
type CodedError struct {
    Code    Code           // the failure class, e.g. APERTURE_NOT_FOUND
    Msg     string         // human-readable summary
    Context map[string]any // structured detail
    Inner   error          // the wrapped cause
}
```

It implements `error` and `Unwrap`, so `errors.Is` / `errors.As` inspect it
normally.

## Constructing and recovering codes

Construct a coded error with one of the wrappers; recover the code with `CodeOf`:

| Constructor | Use |
|---|---|
| `New(code, msg)` | a fresh error; empty `msg` falls back to the code's canonical `Registry` message |
| `Newf(code, format, …)` | `New` with a formatted message |
| `WithContext(code, msg, ctx)` | a fresh error carrying a structured `Context` map |
| `Wrap(code, msg, inner)` | attach a code + summary to an existing error |
| `Wrapf(code, inner, format, …)` | `Wrap` with a formatted message |
| `CodeOf(err) Code` | the `APERTURE_*` code for an error, or `""` when none is attached |

```go
if _, err := store.GetGrant(ctx, id); err != nil {
    if errors.CodeOf(err) == errors.APERTURE_NOT_FOUND {
        // ... handle the absent grant
    }
}
```

## Wrapping rules

Two rules govern how codes propagate:

- **Never re-stamp.** An error that already carries an `APERTURE_*` code passes
  through verbatim. `CodeOf` recovers the *existing* code; the wrappers do not
  overwrite it. This lets a lower layer set the precise code (e.g. a provider
  returning `APERTURE_NOT_FOUND` for an absent object) and have it survive
  unchanged up through the callers.
- **Never leak cross-account data.** A code and its message must not carry another
  account's entity ids, names, or contents. Cross-account isolation is a hard
  invariant of the engine; an error message is not an exception to it. Put
  narrowing detail in `Context` (structured, controllable) rather than
  interpolating tenant data into free-text messages.

Across package boundaries, **never** return a bare `errors.New` / `fmt.Errorf` —
wrap it in an `APERTURE_*` coded error so every surface can translate it.

## The Registry and its gates

Every code has one entry in `Registry` (the Orbit pattern), a
`map[Code]Metadata`:

- a canonical **`Message`** (the fallback summary and the reference-table text), and
- either at least one **`Fixup`** (an operator-actionable hint) or
  `FixupNotApplicable = true` when no fixup is meaningful.

Codes are **SCREAMING_SNAKE**, `APERTURE_`-prefixed, and listed in the `AllCodes`
slice. This structure is enforced by non-skippable CI gates:

| Gate | Enforces |
|---|---|
| `TestCodesHaveFixups` | every code has a `Registry` entry with a `Message` and a `Fixup` (or `FixupNotApplicable`) |
| `TestRegistryHasNoOrphans` | `Registry` contains nothing absent from `AllCodes` |
| `TestCodesAreScreamingSnakeNamespaced` | every code is SCREAMING_SNAKE and `APERTURE_`-prefixed |

Adding a code therefore means: append it to `AllCodes`, and add a `Registry` entry
with a message and fixups — or the build fails.

## Related

- [Error Codes reference](../reference/error-codes.md) — the generated table of
  every code, message, and fixup.
- [The decision engine](../library/decision-api.md) — the surface whose failures
  these codes classify.
- [Authentication](auth.md), [The authz gate](authz.md), [Storage](storage.md) —
  packages that raise the auth, authorization, and storage codes.
