# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly.

**Do not open a public issue.** Use GitHub's private vulnerability reporting or
email the maintainer.

Please include:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

You should receive a response within 72 hours. We'll work with you to understand
the issue and coordinate a fix before any public disclosure.

## Scope

Aperture is an access-control engine, so authorization correctness is the
security surface. In-scope areas (as they land):

- **Decision engine** — `Check` / `Enumerate` / `Explain` correctness;
  deny-overrides and specificity tiebreak must never resolve a deny to an allow.
- **Account isolation** — account-scoped bestowed grants must never leak across
  a multi-account user. This is a hard security line.
- **Authentication** — Aperture always consumes external credentials and never
  issues them. Bearer/OIDC/JWT and broker adapters must fail closed.
- **Storage** — hand-written SQL must not admit injection; the in-memory and
  SQLite implementations must enforce the same isolation invariants.
- **MCP surface** — read + decide + simulate only; no mutations reachable.
- **Rules** — Pulse-expression evaluation over untrusted metadata must not
  permit sandbox escape; treat input reaching Pulse as Pulse's responsibility
  to validate, and pin a published Pulse version.

## Known Considerations

- **Credentials** are loaded from environment variables or referenced files,
  never logged or persisted.
- **Error codes** (`APERTURE_*`) are stable identifiers; messages must not leak
  secrets or cross-account data.
