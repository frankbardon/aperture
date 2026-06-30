// Package cli assembles Aperture's urfave/cli/v3 command tree. It is the seam
// cmd/aperture/main.go delegates to, keeping the binary entrypoint a pure
// adapter. Each command is thin: it parses flags/args, hand-wires the
// dependency graph (storage -> engine -> service -> surface), translates one
// call into the engine, and maps the result to output + an exit code. No
// business logic lives here — that is the library's job.
package cli

import (
	"context"

	"github.com/frankbardon/aperture/errors"

	ucli "github.com/urfave/cli/v3"
)

// NewApp builds the root command tree. version is stamped onto --version.
func NewApp(version string) *ucli.Command {
	return &ucli.Command{
		Name:    "aperture",
		Usage:   "Fine-grained access control engine",
		Version: version,
		Commands: []*ucli.Command{
			checkCommand(),
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
			serveCommand(),
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
