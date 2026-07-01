package cli

import (
	"context"
	"fmt"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"

	ucli "github.com/urfave/cli/v3"
)

// This file adds the E5-S1 provisioning commands: parameterized template CRUD +
// transactional apply, and bulk grant/revoke. Every command builds the SAME
// fully-wired facade the serve command mounts and calls it in-process, so the
// CLI stays a thin adapter over exactly the path Twirp drives.

// templateCommand is `aperture template <put|get|list|delete|apply>`.
func templateCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "template",
		Usage: "Manage and apply provisioning templates",
		Commands: []*ucli.Command{
			templatePutCommand(),
			templateGetCommand(),
			templateListCommand(),
			templateDeleteCommand(),
			templateApplyCommand(),
		},
	}
}

func templatePutCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "json", Usage: "template body as inline JSON"},
		&ucli.StringFlag{Name: "file", Usage: "path to a JSON template body"},
	)
	return &ucli.Command{
		Name:   "put",
		Usage:  "Create or update a template (system-admin tier)",
		Flags:  flags,
		Action: runTemplatePut,
	}
}

func runTemplatePut(ctx context.Context, cmd *ucli.Command) error {
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	body, err := readBody(cmd)
	if err != nil {
		return err
	}
	var t model.Template
	if err := decode(body, &t); err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := svc.PutTemplate(ctx, actor, t); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "put template %s v%d ok\n", t.Name, t.Version)
	return nil
}

func templateGetCommand() *ucli.Command {
	flags := append(storeFlags(),
		&ucli.IntFlag{Name: "version", Usage: "template version (0 = latest)"},
	)
	return &ucli.Command{
		Name:      "get",
		Usage:     "Read a template by name (latest version unless --version)",
		ArgsUsage: "<name>",
		Flags:     flags,
		Action:    runTemplateGet,
	}
}

func runTemplateGet(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 1 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "template get takes <name>")
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	t, err := svc.GetTemplate(ctx, cmd.Args().Get(0), int(cmd.Int("version")))
	if err != nil {
		return err
	}
	return printJSON(cmd, t)
}

func templateListCommand() *ucli.Command {
	return &ucli.Command{
		Name:   "list",
		Usage:  "List every template version",
		Flags:  storeFlags(),
		Action: runTemplateList,
	}
}

func runTemplateList(ctx context.Context, cmd *ucli.Command) error {
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	ts, err := svc.ListTemplates(ctx)
	if err != nil {
		return err
	}
	return printJSON(cmd, ts)
}

func templateDeleteCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.IntFlag{Name: "version", Usage: "template version to delete (0 = all versions of the name)"},
	)
	return &ucli.Command{
		Name:      "delete",
		Usage:     "Delete a template version, or all versions (system-admin tier)",
		ArgsUsage: "<name>",
		Flags:     flags,
		Action:    runTemplateDelete,
	}
}

func runTemplateDelete(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 1 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "template delete takes <name>")
	}
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	name := cmd.Args().Get(0)
	if err := svc.DeleteTemplate(ctx, actor, name, int(cmd.Int("version"))); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "delete template %s ok\n", name)
	return nil
}

func templateApplyCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "name", Usage: "template name to apply", Required: true},
		&ucli.IntFlag{Name: "version", Usage: "template version (0 = latest)"},
		&ucli.StringSliceFlag{Name: "param", Usage: "parameter as name=value (repeatable)"},
		&ucli.StringFlag{Name: "id-prefix", Usage: "prefix for generated grant ids"},
	)
	return &ucli.Command{
		Name:   "apply",
		Usage:  "Apply a template transactionally into --account (account-admin tier)",
		Flags:  flags,
		Action: runTemplateApply,
	}
}

func runTemplateApply(ctx context.Context, cmd *ucli.Command) error {
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	params, err := parseParams(cmd.StringSlice("param"))
	if err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	applied, err := svc.ApplyTemplate(ctx, actor, model.TemplateApplication{
		Name:          cmd.String("name"),
		Version:       int(cmd.Int("version")),
		Account:       cmd.String("account"),
		Params:        params,
		GrantIDPrefix: cmd.String("id-prefix"),
	})
	if err != nil {
		return err
	}
	return printJSON(cmd, applied)
}

// parseParams turns repeated "name=value" flags into a parameter map.
func parseParams(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT, "param %q must be name=value", e)
		}
		out[k] = v
	}
	return out, nil
}

// bulkCommand is `aperture bulk <grant|revoke>`.
func bulkCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "bulk",
		Usage: "Provision or deprovision many grants in one transactional call",
		Commands: []*ucli.Command{
			bulkGrantCommand(),
			bulkRevokeCommand(),
		},
	}
}

func bulkGrantCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "json", Usage: "a JSON array of grant bodies"},
		&ucli.StringFlag{Name: "file", Usage: "path to a JSON array of grant bodies"},
	)
	return &ucli.Command{
		Name:   "grant",
		Usage:  "Apply many grants atomically (account-admin tier)",
		Flags:  flags,
		Action: runBulkGrant,
	}
}

func runBulkGrant(ctx context.Context, cmd *ucli.Command) error {
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	body, err := readBody(cmd)
	if err != nil {
		return err
	}
	var grants []model.Grant
	if err := decode(body, &grants); err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := svc.BulkPutGrants(ctx, actor, grants); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "bulk grant %d ok\n", len(grants))
	return nil
}

func bulkRevokeCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringSliceFlag{Name: "grant", Usage: "grant id to revoke (repeatable)"},
	)
	return &ucli.Command{
		Name:      "revoke",
		Usage:     "Delete many grants atomically (account-admin tier)",
		ArgsUsage: "[<grant-id>...]",
		Flags:     flags,
		Action:    runBulkRevoke,
	}
}

func runBulkRevoke(ctx context.Context, cmd *ucli.Command) error {
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	ids := append([]string{}, cmd.StringSlice("grant")...)
	ids = append(ids, cmd.Args().Slice()...)
	if len(ids) == 0 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "bulk revoke requires at least one --grant or positional id")
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := svc.BulkDeleteGrants(ctx, actor, ids); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "bulk revoke %d ok\n", len(ids))
	return nil
}
