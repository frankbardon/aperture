package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/frankbardon/aperture/authz"
	"github.com/frankbardon/aperture/delegation"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/impersonation"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"

	ucli "github.com/urfave/cli/v3"
)

// This file adds the CLI mutation commands. Every command builds the SAME
// fully-wired facade the serve command mounts (storage -> engine -> gate +
// delegation + impersonation -> service) and calls it in-process, so the CLI is
// a thin adapter over exactly the path HTTP and Twirp drive — there is no
// CLI-only mutation logic. Entity bodies are read as the canonical JSON encoding
// of the model.* struct (the same shape the Twirp entity_json field carries).
//
// Mutations require an actor: --principal (the authenticated caller, also read
// from APERTURE_PRINCIPAL) and --account (the active account; required for the
// system-tier authority check). The admin-tier enforcement happens inside the
// facade gate exactly as it does on the wire.

// buildService constructs and seeds the fully-wired facade for a CLI command,
// returning it plus the store to close. It mirrors serve's manual DI.
func buildService(ctx context.Context, storeDSN, seedPath string) (*service.Service, model.Storage, error) {
	store, err := buildStore(ctx, storeDSN, seedPath)
	if err != nil {
		return nil, nil, err
	}
	eng := engine.New(store)
	svc := service.New(eng,
		service.WithStorage(store),
		service.WithGate(authz.NewGate(eng)),
		service.WithDelegation(delegation.New(store, eng)),
		service.WithImpersonation(impersonation.New(store, eng)),
	)
	return svc, store, nil
}

// storeFlags are the backend-selection flags every mutation command shares.
func storeFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.StringFlag{Name: "seed", Usage: "path to a JSON/YAML seed model (defaults to the embedded example)"},
		&ucli.StringFlag{Name: "store", Usage: "sqlite DSN for the backing store (defaults to in-memory)"},
	}
}

// actorFlags are the principal/account flags a gated mutation needs.
func actorFlags() []ucli.Flag {
	return []ucli.Flag{
		&ucli.StringFlag{Name: "principal", Usage: "authenticated principal performing the mutation", Sources: ucli.EnvVars("APERTURE_PRINCIPAL")},
		&ucli.StringFlag{Name: "account", Usage: "active account (required for system-tier authority resolution)"},
	}
}

func actorFrom(cmd *ucli.Command) (service.Actor, error) {
	p := cmd.String("principal")
	if p == "" {
		return service.Actor{}, aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"a mutation requires --principal (or APERTURE_PRINCIPAL)")
	}
	return service.Actor{Principal: p, Account: cmd.String("account")}, nil
}

// readBody returns the entity JSON from --json, --file, or stdin (in that order).
func readBody(cmd *ucli.Command) ([]byte, error) {
	if j := cmd.String("json"); j != "" {
		return []byte(j), nil
	}
	if f := cmd.String("file"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: reading --file", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(cmd.Reader)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: reading body from stdin", err)
	}
	if len(data) == 0 {
		return nil, aerr.New(aerr.APERTURE_INVALID_INPUT, "cli: no entity body (use --json, --file, or stdin)")
	}
	return data, nil
}

func printJSON(cmd *ucli.Command, v any) error {
	enc := json.NewEncoder(cmd.Writer)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// putCommand is `aperture put <kind>`: it decodes a model entity and upserts it
// through the facade (gated by the admin tier the kind requires).
func putCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "json", Usage: "entity body as inline JSON"},
		&ucli.StringFlag{Name: "file", Usage: "path to a JSON entity body"},
	)
	return &ucli.Command{
		Name:      "put",
		Usage:     "Create or update an entity (object-type|permission|principal|role|group|account|membership|grant)",
		ArgsUsage: "<kind>",
		Flags:     flags,
		Action:    runPut,
	}
}

func runPut(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 1 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "put takes exactly one <kind> argument")
	}
	kind := cmd.Args().Get(0)
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	body, err := readBody(cmd)
	if err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	switch kind {
	case "object-type":
		var v model.ObjectType
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutObjectType(ctx, actor, v)
	case "permission":
		var v model.Permission
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutPermission(ctx, actor, v)
	case "principal":
		var v model.Principal
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutPrincipal(ctx, actor, v)
	case "role":
		var v model.Role
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutRole(ctx, actor, v)
	case "group":
		var v model.Group
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutGroup(ctx, actor, v)
	case "account":
		var v model.Account
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutAccount(ctx, actor, v)
	case "membership":
		var v model.Membership
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutMembership(ctx, actor, v)
	case "grant":
		var v model.Grant
		if err := decode(body, &v); err != nil {
			return err
		}
		err = svc.PutGrant(ctx, actor, v)
	default:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT, "unknown kind %q", kind)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "put %s ok\n", kind)
	return nil
}

// getCommand is `aperture get <kind> <id>`: it reads one entity through the
// facade and prints it as JSON. Reads require no admin tier.
func getCommand() *ucli.Command {
	return &ucli.Command{
		Name:      "get",
		Usage:     "Read one entity by id (object-type|permission|principal|role|group|account|grant)",
		ArgsUsage: "<kind> <id>",
		Flags:     storeFlags(),
		Action:    runGet,
	}
}

func runGet(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 2 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "get takes <kind> <id>")
	}
	kind, id := cmd.Args().Get(0), cmd.Args().Get(1)
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var v any
	switch kind {
	case "object-type":
		v, err = svc.GetObjectType(ctx, id)
	case "permission":
		v, err = svc.GetPermission(ctx, id)
	case "principal":
		v, err = svc.GetPrincipal(ctx, id)
	case "role":
		v, err = svc.GetRole(ctx, id)
	case "group":
		v, err = svc.GetGroup(ctx, id)
	case "account":
		v, err = svc.GetAccount(ctx, id)
	case "grant":
		v, err = svc.GetGrant(ctx, service.Actor{}, id)
	default:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT, "unknown kind %q", kind)
	}
	if err != nil {
		return err
	}
	return printJSON(cmd, v)
}

// listCommand is `aperture list <kind>`: it lists entities of a kind as JSON.
// Grants are listed per --account.
func listCommand() *ucli.Command {
	flags := append(storeFlags(),
		&ucli.StringFlag{Name: "account", Usage: "account to list grants for (required for kind=grant)"},
	)
	return &ucli.Command{
		Name:      "list",
		Usage:     "List entities of a kind (object-types|permissions|principals|roles|groups|accounts|grants)",
		ArgsUsage: "<kind>",
		Flags:     flags,
		Action:    runList,
	}
}

func runList(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() != 1 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "list takes exactly one <kind> argument")
	}
	kind := cmd.Args().Get(0)
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var v any
	switch kind {
	case "object-types", "object-type":
		v, err = svc.ListObjectTypes(ctx)
	case "permissions", "permission":
		v, err = svc.ListPermissions(ctx)
	case "principals", "principal":
		v, err = svc.ListPrincipals(ctx, service.Actor{})
	case "roles", "role":
		v, err = svc.ListRoles(ctx)
	case "groups", "group":
		v, err = svc.ListGroups(ctx)
	case "accounts", "account":
		v, err = svc.ListAccounts(ctx, service.Actor{})
	case "grants", "grant":
		account := cmd.String("account")
		if account == "" {
			return aerr.New(aerr.APERTURE_INVALID_INPUT, "listing grants requires --account")
		}
		v, err = svc.ListGrants(ctx, service.Actor{}, account)
	default:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT, "unknown kind %q", kind)
	}
	if err != nil {
		return err
	}
	return printJSON(cmd, v)
}

// deleteCommand is `aperture delete <kind> <id>`: it removes one entity (gated by
// the admin tier the kind requires). Memberships are keyed by (principal,
// account), so they use --principal-id/--account-id rather than a single id.
func deleteCommand() *ucli.Command {
	flags := append(storeFlags(), actorFlags()...)
	flags = append(flags,
		&ucli.StringFlag{Name: "principal-id", Usage: "membership principal id (kind=membership)"},
		&ucli.StringFlag{Name: "account-id", Usage: "membership account id (kind=membership)"},
	)
	return &ucli.Command{
		Name:      "delete",
		Usage:     "Delete an entity (object-type|permission|principal|role|group|account|grant|membership)",
		ArgsUsage: "<kind> [<id>]",
		Flags:     flags,
		Action:    runDelete,
	}
}

func runDelete(ctx context.Context, cmd *ucli.Command) error {
	if cmd.Args().Len() < 1 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "delete takes <kind> [<id>]")
	}
	kind := cmd.Args().Get(0)
	actor, err := actorFrom(cmd)
	if err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if kind == "membership" {
		pid, aid := cmd.String("principal-id"), cmd.String("account-id")
		if pid == "" || aid == "" {
			return aerr.New(aerr.APERTURE_INVALID_INPUT, "deleting a membership requires --principal-id and --account-id")
		}
		if err := svc.DeleteMembership(ctx, actor, pid, aid); err != nil {
			return err
		}
		fmt.Fprintln(cmd.Writer, "delete membership ok")
		return nil
	}

	if cmd.Args().Len() != 2 {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "delete takes <kind> <id>")
	}
	id := cmd.Args().Get(1)
	switch kind {
	case "object-type":
		err = svc.DeleteObjectType(ctx, actor, id)
	case "permission":
		err = svc.DeletePermission(ctx, actor, id)
	case "principal":
		err = svc.DeletePrincipal(ctx, actor, id)
	case "role":
		err = svc.DeleteRole(ctx, actor, id)
	case "group":
		err = svc.DeleteGroup(ctx, actor, id)
	case "account":
		err = svc.DeleteAccount(ctx, actor, id)
	case "grant":
		err = svc.DeleteGrant(ctx, actor, id)
	default:
		return aerr.Newf(aerr.APERTURE_INVALID_INPUT, "unknown kind %q", kind)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "delete %s %s ok\n", kind, id)
	return nil
}

// bestowCommand is `aperture bestow`: it bestows a grant on behalf of the
// delegator, enforcing the delegation subset rule (not the admin tier).
func bestowCommand() *ucli.Command {
	flags := append(storeFlags(),
		&ucli.StringFlag{Name: "delegator", Usage: "principal bestowing the grant", Sources: ucli.EnvVars("APERTURE_PRINCIPAL"), Required: true},
		&ucli.StringFlag{Name: "json", Usage: "grant body as inline JSON"},
		&ucli.StringFlag{Name: "file", Usage: "path to a JSON grant body"},
	)
	return &ucli.Command{
		Name:   "bestow",
		Usage:  "Bestow (delegate) a grant you hold to another principal",
		Flags:  flags,
		Action: runBestow,
	}
}

func runBestow(ctx context.Context, cmd *ucli.Command) error {
	body, err := readBody(cmd)
	if err != nil {
		return err
	}
	var g model.Grant
	if err := decode(body, &g); err != nil {
		return err
	}
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := svc.Bestow(ctx, cmd.String("delegator"), g); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "bestow %s ok\n", g.ID)
	return nil
}

// revokeCommand is `aperture revoke`: the inverse of bestow.
func revokeCommand() *ucli.Command {
	flags := append(storeFlags(),
		&ucli.StringFlag{Name: "delegator", Usage: "principal revoking the grant", Sources: ucli.EnvVars("APERTURE_PRINCIPAL"), Required: true},
		&ucli.StringFlag{Name: "grant", Usage: "id of the grant to revoke", Required: true},
	)
	return &ucli.Command{
		Name:   "revoke",
		Usage:  "Revoke a grant you previously bestowed",
		Flags:  flags,
		Action: runRevoke,
	}
}

func runRevoke(ctx context.Context, cmd *ucli.Command) error {
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := svc.Revoke(ctx, cmd.String("delegator"), cmd.String("grant")); err != nil {
		return err
	}
	fmt.Fprintf(cmd.Writer, "revoke %s ok\n", cmd.String("grant"))
	return nil
}

// impersonateCommand is `aperture impersonate`: it starts a time-boxed session
// for the operator to act as the target, enforcing the impersonation guardrails.
func impersonateCommand() *ucli.Command {
	flags := append(storeFlags(),
		&ucli.StringFlag{Name: "operator", Usage: "operator principal", Sources: ucli.EnvVars("APERTURE_PRINCIPAL"), Required: true},
		&ucli.StringFlag{Name: "target", Usage: "target principal to impersonate", Required: true},
		&ucli.StringFlag{Name: "account", Usage: "active account", Required: true},
		&ucli.StringFlag{Name: "mode", Usage: "augment|become", Value: "augment"},
	)
	return &ucli.Command{
		Name:   "impersonate",
		Usage:  "Start a time-boxed impersonation session (prints the session)",
		Flags:  flags,
		Action: runImpersonate,
	}
}

func runImpersonate(ctx context.Context, cmd *ucli.Command) error {
	svc, store, err := buildService(ctx, cmd.String("store"), cmd.String("seed"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	sess, err := svc.ImpersonationStart(ctx, cmd.String("operator"), cmd.String("target"), cmd.String("account"), engine.Mode(cmd.String("mode")))
	if err != nil {
		return err
	}
	return printJSON(cmd, sess)
}

// decode unmarshals an entity body, mapping a JSON error to APERTURE_INVALID_INPUT.
func decode(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "cli: entity body is not valid JSON", err)
	}
	return nil
}
