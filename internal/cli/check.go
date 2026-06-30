package cli

import (
	"context"
	"fmt"

	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/seed"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// denyExitCode is the process exit code for a clean DENY decision. A check that
// resolves to allow exits 0; a deny exits non-zero so the command composes in
// shell pipelines (e.g. `aperture check ... && deploy`).
const denyExitCode = 1

// checkCommand is `aperture check <principal> <action> <object>`: it builds a
// store from --seed/--store, asks the decision service the single question, and
// prints the verdict + reason. The exit code reflects the decision.
func checkCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "check",
		Usage:     "Decide whether a principal may take an action on an object",
		ArgsUsage: "<principal> <action> <object>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:  "seed",
				Usage: "path to a JSON/YAML seed model (defaults to the embedded example)",
			},
			&ucli.StringFlag{
				Name:  "store",
				Usage: "sqlite DSN for the backing store (defaults to in-memory)",
			},
			&ucli.StringFlag{
				Name:  "account",
				Usage: "active account the decision is scoped to",
				Value: seed.ExampleAccount,
			},
		},
		Action: runCheck,
	}
}

func runCheck(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 3 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"check takes exactly 3 arguments (<principal> <action> <object>), got %d", args.Len())
	}
	principal, action, object := args.Get(0), args.Get(1), args.Get(2)

	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	svc := service.New(engine.New(store))
	res, err := svc.Check(ctx, service.Query{
		Account:   cmd.String("account"),
		Principal: principal,
		Action:    action,
		Object:    object,
	})
	if err != nil {
		// Genuine input-validation error — surface it as a usage error (exit 1).
		return err
	}

	verdict := "deny"
	if res.Allow {
		verdict = "allow"
	}
	fmt.Fprintf(cmd.Writer, "%s\nreason: %s\n", verdict, res.Reason)

	if !res.Allow {
		// A clean deny: print the decision (above) then exit non-zero with no
		// extra error noise.
		return ucli.Exit("", denyExitCode)
	}
	return nil
}
