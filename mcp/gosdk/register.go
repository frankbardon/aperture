// Package gosdk is the thin, reusable adapter that mounts the SDK-free Aperture
// MCP catalog (github.com/frankbardon/aperture/mcp) onto a caller-supplied
// github.com/modelcontextprotocol/go-sdk server. It is the ONLY package in the
// module that imports the MCP SDK — the firewall test over the mcp/ core keeps the
// core SDK-free, and this adapter is where every SDK type is allowed.
//
// An embedder that already runs its own go-sdk server can mount the full Aperture
// read/decide/simulate surface by calling Register with a constructed
// *service.Service and a Config:
//
//	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "my-host", Version: "..."}, nil)
//	if err := gosdk.Register(srv, svc, gosdk.Config{Version: "1.0.0"}); err != nil {
//	    return err
//	}
//	// caller owns serving: srv.Run(ctx, &mcpsdk.StdioTransport{}) — gosdk never serves.
//
// Register MOUNTS onto the server it is given; it never constructs or returns a
// finished server and never calls Serve/Run. Server lifecycle (creation,
// transport, serving) stays entirely with the caller. All runtime configuration
// is threaded through Config; the adapter holds no process globals.
//
// READ-ONLY: the catalog this adapter mounts is the read/decide/simulate surface.
// The service facade's mutators are never reached — the core defines no mutating
// tool (asserted by mcp.TestNoMutatingTool) and this adapter adds none.
package gosdk

import (
	core "github.com/frankbardon/aperture/mcp"
	"github.com/frankbardon/aperture/service"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Config carries the runtime configuration for one Register call — the single
// place runtime identity is injected. The adapter reads nothing from
// package-level state.
type Config struct {
	// Version is the server/build identity string, threaded into the core tool
	// catalog (mcp.Config.Version). The caller is responsible for the go-sdk
	// Implementation.Version it passes to mcpsdk.NewServer; Register does not touch
	// the server's advertised identity.
	Version string
}

// Core projects the adapter Config onto the SDK-free core Config consumed by
// mcp.Tools.
func (c Config) Core() core.Config {
	return core.Config{Version: c.Version}
}

// Register mounts the full Aperture MCP surface (every read/decide/simulate tool)
// onto the caller-supplied server. It returns an error only on a nil server or
// nil service; tool registration itself is total. The server's lifecycle is the
// caller's: Register never serves.
func Register(server *mcpsdk.Server, svc *service.Service, cfg Config) error {
	if server == nil {
		return errNilServer
	}
	if svc == nil {
		return errNilService
	}
	registerTools(server, svc, cfg)
	return nil
}
