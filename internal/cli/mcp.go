package cli

import (
	"context"
	"fmt"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/mcp/gosdk"
	"github.com/frankbardon/aperture/service"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	ucli "github.com/urfave/cli/v3"
)

// mcpServerName is the MCP server identity reported during initialize.
const mcpServerName = "aperture"

// mcpCommand is `aperture mcp`: it hand-wires the dependency graph
// (storage -> engine -> service) and serves Aperture's READ-ONLY MCP surface over
// stdio — the transport an MCP client uses when it spawns Aperture as a
// subprocess. The surface is read/decide/simulate/inspect only; no mutating tool
// exists, so the command wires the facade with storage (for inspection + what-if
// reads) but NOT the gate / delegation / impersonation mutators.
//
// The CLI stays thin: it builds the graph, constructs the go-sdk server, mounts
// the SDK-free catalog through the single gosdk adapter, and runs the transport.
// All tool logic lives in the mcp/ core; the binary entrypoint (cmd/aperture)
// only assembles the command tree.
func mcpCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "mcp",
		Usage: "Serve the read-only Aperture MCP surface over stdio",
		Description: "Exposes Aperture's decision API (check/enumerate/explain, " +
			"single + bulk), a read-only what-if simulator, and model inspection " +
			"as MCP tools over stdio. No tool mutates. Intended to be spawned over " +
			"stdio by an MCP client.",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:  "seed",
				Usage: "path to a JSON/YAML seed model (defaults to the embedded example)",
			},
			&ucli.StringFlag{
				Name:  "store",
				Usage: "sqlite DSN for the backing store (defaults to in-memory)",
			},
		},
		Action: runMCP,
	}
}

func runMCP(ctx context.Context, cmd *ucli.Command) error {
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Read-only facade: the engine for decisions + the store for inspection and the
	// what-if overlay base. No mutators are wired — the MCP surface never writes.
	eng := engine.New(store)
	svc := service.New(eng, service.WithStorage(store))

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    mcpServerName,
		Version: cmd.Root().Version,
	}, nil)
	if err := gosdk.Register(srv, svc, gosdk.Config{Version: cmd.Root().Version}); err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: registering the mcp surface failed", err)
	}

	fmt.Fprintln(cmd.ErrWriter, "aperture mcp: serving read-only MCP surface over stdio")
	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: mcp server failed", err)
	}
	return nil
}
