// Package main is the entry point for the aperture binary. Aperture is
// library-first: all business logic lives in the public packages at the module
// root (engine, model, storage, service, seed, ...) and is assembled by
// internal/cli. This file is a thin urfave/cli/v3 adapter — it builds the
// command tree and translates a returned error to a process exit code. Keep it
// free of business logic.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/frankbardon/aperture/internal/cli"
)

// version is the binary version. Later stories wire this to build metadata; for
// the scaffold it is a static placeholder.
const version = "0.0.0-dev"

func main() {
	// urfave/cli handles ExitCoder errors itself (it os.Exits with the decision's
	// code for a clean deny). Anything else reaching here is a real failure, so
	// surface it and exit non-zero.
	if err := cli.NewApp(version).Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
