# Troubleshooting

**Audience:** operators diagnosing a failed request, boot, or CLI command.

Every failure Aperture surfaces is an **`APERTURE_*` coded error**. The code — not
the human-readable message — is the stable contract, and each code carries
operator-actionable **fixups** in the error registry. Troubleshooting Aperture is
therefore mostly: read the code, look it up, apply its fixup.

## Reading an `APERTURE_*` error

An error prints its code alongside a short summary, for example:

```text
APERTURE_CONFIG_INVALID: auth: unknown auth mode
```

or, when it wraps a lower cause, the code is preserved from wherever it was first
stamped (the wrappers **never re-stamp** an already-coded error), so the code you
see is the precise failure class — a provider returning `APERTURE_NOT_FOUND` for
an absent object surfaces as `APERTURE_NOT_FOUND` all the way up.

Two properties make the code trustworthy:

- **Stable and machine-readable.** SCREAMING_SNAKE, `APERTURE_`-prefixed. The CLI,
  HTTP/Twirp, and MCP surfaces map the code to a transport status without
  string-matching the message.
- **Leak-free.** A code and its message never carry another account's ids, names,
  or contents — cross-account isolation is a hard invariant. Narrowing detail
  lives in the structured `Context` map, not in interpolated message text, so it
  is safe to log and share the message.

## Acting on the fixups

Every code has exactly one entry in the registry (`errors.Registry`), and that
entry carries either **at least one fixup** — a concrete next step — or is marked
`FixupNotApplicable` when no action is meaningful. The generated
[Error Codes reference](../reference/error-codes.md) renders the full table:
each code, its canonical message, and its fixups.

The workflow:

1. Note the `APERTURE_*` code from the output or logs.
2. Find it in the [Error Codes reference](../reference/error-codes.md).
3. Apply the listed fixup(s).

A few codes you are likely to meet operating a service:

| Code | Typical trigger | First move |
|---|---|---|
| `APERTURE_CONFIG_INVALID` | An unrecognised `APERTURE_AUTH_MODE` / `--auth`, or bad adapter config at boot. | Check the auth env vars against [Deployment](deployment.md); valid modes are `dev` \| `oidc` \| `parsec`. |
| `APERTURE_BOOT` | `serve` failed to wire up — store open/`Setup`, seed load, provider build, or the listener. | Read the wrapped cause; verify `--store` DSN, `--seed` path, and the seed's `providers:` section. |
| `APERTURE_UNAUTHENTICATED` / `APERTURE_INVALID_TOKEN` | A request carried no bearer, or a token that failed verification. | Confirm the client credential and the configured adapter (`dev` treats the bearer as the principal id). |
| `APERTURE_AUTHZ_DENIED` | The caller lacks the admin tier for a gated mutation. | Expected for under-privileged callers; grant the tier or use an authorized principal. |
| `APERTURE_NOT_FOUND` | A referenced grant, rule, object, or entity does not exist (or is out of the caller's account scope). | Re-check the id; remember cross-account lookups are scoped, so another account's entity reads as absent. |

The reference table is the authority for the exhaustive list and the exact
fixups — the rows above are orientation, not a substitute.

## When the fixup is not enough

- The message is a *summary*; the wrapped cause (visible when the CLI prints the
  chain) and the `Context` map hold the specifics.
- Boot failures under `serve` are almost always configuration: `--store` DSN,
  `--seed` path/format, provider files declared in the seed, or an auth adapter
  that can't reach its IdP at startup (`oidc` discovers at boot). See
  [Deployment](deployment.md).
- If a decision is *slow* rather than *wrong*, that is a performance question —
  see [Performance & the NFR](performance.md) and the `TestCheckNFR` gate.

## Related

- [Error taxonomy](../concepts/errors.md) — the coded-error type, the wrapping
  rules, and the registry gates.
- [Error Codes reference](../reference/error-codes.md) — the generated table of
  every code, message, and fixup.
- [Deployment](deployment.md) — the config surface most boot errors point back to.
