// Package main is the entry point for the aperture binary. Aperture is
// library-first: all business logic lives in the public packages at the module
// root (engine, model, storage, ...) and is assembled by internal/cli in later
// stories. This file is a thin urfave/cli/v3 adapter — it only builds the
// command tree and translates a returned error to a process exit code. Keep it
// free of business logic.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// version is the binary version. Later stories wire this to build metadata; for
// the scaffold it is a static placeholder.
const version = "0.0.0-dev"

func main() {
	if err := buildApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func buildApp() *ucli.Command {
	return &ucli.Command{
		Name:    "aperture",
		Usage:   "Fine-grained access control engine",
		Version: version,
		// Placeholder command tree. Each leaf returns APERTURE_UNIMPLEMENTED
		// until its story wires the real internal/cli command over the engine.
		Commands: []*ucli.Command{
			{
				Name:   "check",
				Usage:  "Decide whether a principal may take an action on an object",
				Action: unimplemented("check"),
			},
			{
				Name:   "enumerate",
				Usage:  "List the objects a principal may act on",
				Action: unimplemented("enumerate"),
			},
			{
				Name:   "explain",
				Usage:  "Explain why a decision resolved the way it did",
				Action: unimplemented("explain"),
			},
			{
				Name:   "serve",
				Usage:  "Run the Aperture HTTP/Twirp + MCP server",
				Action: unimplemented("serve"),
			},
		},
	}
}

// unimplemented returns an action that surfaces a coded APERTURE_UNIMPLEMENTED
// error so placeholder leaves fail consistently through the error taxonomy.
func unimplemented(surface string) ucli.ActionFunc {
	return func(_ context.Context, _ *ucli.Command) error {
		return errors.Newf(errors.APERTURE_UNIMPLEMENTED, "%q is not implemented yet", surface)
	}
}
