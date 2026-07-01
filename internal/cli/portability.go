package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/seed"

	ucli "github.com/urfave/cli/v3"
)

// This file adds the E5-S2 declarative state commands: `aperture export` and
// `aperture import`. Both build the SAME fully-wired facade the serve command
// mounts and drive exactly the path the Twirp Export/Import RPCs drive, so the
// CLI stays a thin adapter over one admin-tier-gated code path.

// exportCommand is `aperture export`: it serializes the whole model to a single
// JSON/YAML state file (system-admin tier).
func exportCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "out", Usage: "write the state file to this path (default: stdout)"},
		&ucli.StringFlag{Name: "format", Usage: "output format: json (default) or yaml"},
	)
	return &ucli.Command{
		Name:   "export",
		Usage:  "Export the whole model to a single JSON/YAML state file (system-admin tier)",
		Flags:  flags,
		Action: runExport,
	}
}

func runExport(ctx context.Context, cmd *ucli.Command) error {
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	doc, err := svc.Export(ctx, actor)
	if err != nil {
		return err
	}
	format, err := exportFormat(cmd.String("format"), cmd.String("out"))
	if err != nil {
		return err
	}
	data, err := seed.Marshal(doc, format)
	if err != nil {
		return err
	}
	if out := cmd.String("out"); out != "" {
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return aerr.Wrap(aerr.APERTURE_STORAGE, "cli: writing export file", err)
		}
		fmt.Fprintf(cmd.Writer, "exported %s -> %s\n", doc.Describe(), out)
		return nil
	}
	_, err = cmd.Writer.Write(data)
	return err
}

// exportFormat resolves the export format from an explicit --format, else the
// --out extension, defaulting to JSON (the fully-fidelity, human-diffable form).
func exportFormat(explicit, outPath string) (seed.Format, error) {
	switch strings.ToLower(explicit) {
	case "json":
		return seed.FormatJSON, nil
	case "yaml", "yml":
		return seed.FormatYAML, nil
	case "":
		if outPath != "" {
			return seed.FormatFor(outPath), nil
		}
		return seed.FormatJSON, nil
	default:
		return "", aerr.Newf(aerr.APERTURE_INVALID_INPUT, "cli: unknown --format %q (use json or yaml)", explicit)
	}
}

// importCommand is `aperture import`: it applies a JSON/YAML state file as an
// idempotent, transactional upsert (system-admin tier).
func importCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "file", Usage: "path to the JSON/YAML state file (default: stdin, treated as JSON)"},
	)
	return &ucli.Command{
		Name:   "import",
		Usage:  "Apply a JSON/YAML state file as an idempotent transactional upsert (system-admin tier)",
		Flags:  flags,
		Action: runImport,
	}
}

func runImport(ctx context.Context, cmd *ucli.Command) error {
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	path := cmd.String("file")
	var (
		data   []byte
		format seed.Format
	)
	if path != "" {
		data, err = os.ReadFile(path)
		if err != nil {
			return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: reading state file", err)
		}
		format = seed.FormatFor(path)
	} else {
		data, err = io.ReadAll(cmd.Reader)
		if err != nil {
			return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: reading state file from stdin", err)
		}
		if len(data) == 0 {
			return aerr.New(aerr.APERTURE_INVALID_INPUT, "cli: no state file (use --file or pipe to stdin)")
		}
		format = seed.FormatJSON
	}
	doc, err := seed.Parse(data, format)
	if err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := svc.Import(ctx, actor, doc); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "imported %s\n", doc.Describe())
	return nil
}
