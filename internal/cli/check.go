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

// enumerateCommand is `aperture enumerate <principal> <action> <pattern>`: it
// lists the object ids the principal may act on under the pattern.
func enumerateCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "enumerate",
		Usage:     "List the objects a principal may act on",
		ArgsUsage: "<principal> <action> <pattern>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "sqlite DSN for the backing store (defaults to in-memory)"},
			&ucli.StringFlag{Name: "account", Usage: "active account the enumeration is scoped to", Value: seed.ExampleAccount},
			&ucli.IntFlag{Name: "limit", Usage: "cap the number of returned object ids (<=0 means the default)"},
		},
		Action: runEnumerate,
	}
}

func runEnumerate(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 3 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"enumerate takes exactly 3 arguments (<principal> <action> <pattern>), got %d", args.Len())
	}
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	svc := service.New(engine.New(store))
	ids, err := svc.Enumerate(ctx, service.EnumerateQuery{
		Account:   cmd.String("account"),
		Principal: args.Get(0),
		Action:    args.Get(1),
		Pattern:   args.Get(2),
		Limit:     cmd.Int("limit"),
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(cmd.Writer, id)
	}
	return nil
}

// identifiersCommand is `aperture identifiers <object_type>`: it lists every
// valid instance id of an object type, enumerated from its provider (declared in
// the seed's `providers:` section). --exclude drops ids, yielding the positive
// allow-list an exclusive ("all except these") allowance expands to.
func identifiersCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "identifiers",
		Usage:     "List all valid instance ids of an object type from its provider",
		ArgsUsage: "<object_type>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "sqlite DSN for the backing store (defaults to in-memory)"},
			&ucli.StringSliceFlag{Name: "exclude", Usage: "id to omit from the result (repeatable); expands an exclusive allowance"},
		},
		Action: runIdentifiers,
	}
}

func runIdentifiers(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 1 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"identifiers takes exactly 1 argument (<object_type>), got %d", args.Len())
	}
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	doc, err := seedDocument(cmd.String("seed"))
	if err != nil {
		return err
	}
	reg, err := doc.BuildRegistry(seedBaseDir(cmd.String("seed")))
	if err != nil {
		return aerr.Wrap(aerr.APERTURE_BOOT, "cli: building object providers failed", err)
	}

	svc := service.New(engine.New(store), service.WithProviders(reg))
	ids, err := svc.ObjectIdentifiers(ctx, args.Get(0), cmd.StringSlice("exclude")...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Fprintln(cmd.Writer, id)
	}
	return nil
}

// explainCommand is `aperture explain <principal> <action> <object>`: it prints
// the full decision trace.
func explainCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "explain",
		Usage:     "Explain why a decision resolved the way it did",
		ArgsUsage: "<principal> <action> <object>",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
			&ucli.StringFlag{Name: "store", Usage: "sqlite DSN for the backing store (defaults to in-memory)"},
			&ucli.StringFlag{Name: "account", Usage: "active account the decision is scoped to", Value: seed.ExampleAccount},
		},
		Action: runExplain,
	}
}

func runExplain(ctx context.Context, cmd *ucli.Command) error {
	args := cmd.Args()
	if args.Len() != 3 {
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT,
			"explain takes exactly 3 arguments (<principal> <action> <object>), got %d", args.Len())
	}
	store, err := buildStore(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	svc := service.New(engine.New(store))
	tr, err := svc.Explain(ctx, service.Query{
		Account:   cmd.String("account"),
		Principal: args.Get(0),
		Action:    args.Get(1),
		Object:    args.Get(2),
	})
	if err != nil {
		return err
	}
	fmt.Fprint(cmd.Writer, tr.String())
	return nil
}
