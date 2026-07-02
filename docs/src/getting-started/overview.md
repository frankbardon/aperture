# Getting Started

This section is the on-ramp. In four short pages you install Aperture, learn the
vocabulary the rest of the book assumes, and make your first access-control
decision two ways — from the command line and from Go.

Everything here runs against Aperture's **embedded example model**, a
self-contained fixture for the tenant `acme`. You need nothing but the built
binary; there is no store to provision and no data to load.

## Read in order

1. [Installation](installation.md) — build `bin/aperture` from source
   (Go 1.26.1, pure-Go, `CGO_ENABLED=0`).
2. [Concepts primer](concepts.md) — principals, actions, objects, patterns and
   specificity, rules, scopes, and accounts: enough to read any later chapter.
3. [First decision (CLI)](first-decision-cli.md) — a runnable `aperture check`
   walkthrough showing an allow, a default deny, and a deny-override.
4. [Library quickstart](library-quickstart.md) — the same decision from a
   minimal Go program embedding the engine.

Already know what Aperture is? Skip to [Installation](installation.md). Want the
big picture first? See the [Introduction](../introduction.md).
