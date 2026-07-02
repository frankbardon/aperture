# mcp

**Audience:** integrators wiring Aperture into an MCP client (an AI assistant or
agent runtime).

```text
aperture mcp [options]
```

`mcp` serves Aperture's **read-only** decision surface over stdio — the transport
an MCP client uses when it spawns Aperture as a subprocess. It exposes the
decision API (`check` / `enumerate` / `explain`, single and bulk), a read-only
what-if *simulator*, and model inspection as MCP tools. **No tool mutates.** The
command wires the facade with storage for inspection and what-if reads, but
deliberately *not* the gate, delegation, or impersonation mutators — the surface
can never write.

Because it speaks the MCP protocol over stdio, you normally don't run `mcp`
interactively; an MCP client launches it. A minimal client configuration points
at the binary and the model to serve:

```json
{
  "mcpServers": {
    "aperture": {
      "command": "bin/aperture",
      "args": ["mcp", "--store", "./aperture.db"]
    }
  }
}
```

On start it prints one line to **stderr** (stdout is reserved for the protocol):

```text
aperture mcp: serving read-only MCP surface over stdio
```

## What the flags control

- `--seed` / `--store` — the model the surface reads, exactly as elsewhere (see
  [Global options](global-options.md)). With neither, it serves the embedded
  example model over an in-memory store.

There are no other flags: the surface is read-only by construction, so it needs
no acting principal, account, or auth adapter.

Full flags: [`mcp`](../reference/cli.md#aperture-mcp).

## Related

- [Global options](global-options.md) — `--seed` / `--store`.
- [serve](serve.md) — the read/write HTTP + Twirp surface.
- [Decisions](decisions.md) — the same check/enumerate/explain questions from a shell.
- [Command-Line Reference](../reference/cli.md#aperture-mcp) — the generated flag table.
