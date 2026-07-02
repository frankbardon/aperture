package seed

import (
	"context"
	"encoding/json"
	"sort"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"gopkg.in/yaml.v3"
)

// Export reads the COMPLETE authorization model out of store and returns it as a
// declarative Document — the single state file the model round-trips through.
// It captures every source-of-truth entity: accounts, memberships, object
// types, permissions, principals, roles, groups, grants, templates, and rules
// (AST). It deliberately EXCLUDES live host domain-object metadata: that is the
// provider cache (E2-S2), derived and disposable, never source of truth, so it
// is never exported.
//
// Every slice is emitted in a stable order (sorted by id, name, or the natural
// key) and each rule AST is re-serialized to the rules package's canonical form,
// so a re-export of an unchanged model is byte-identical. Marshal turns the
// returned Document into the on-disk JSON/YAML file; Apply loads one back.
func Export(ctx context.Context, store model.Storage) (*Document, error) {
	doc := &Document{
		Accounts:    []Account{},
		Memberships: []Membership{},
		ObjectTypes: []ObjectType{},
		Permissions: []Permission{},
		Principals:  []Principal{},
		Roles:       []Role{},
		Groups:      []Group{},
		Grants:      []Grant{},
		Templates:   []Template{},
		Rules:       []Rule{},
	}

	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list accounts", err)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	for _, a := range accounts {
		doc.Accounts = append(doc.Accounts, Account{ID: a.ID, Name: a.Name, Description: a.Description})
	}

	// Memberships are enumerated per account (the account-scoped query the model
	// exposes), then sorted by (account, principal) for a stable order. The
	// wildcard account "*" is not a real Account (ValidateAccount rejects it), so
	// it is not in `accounts` — but a principal can be enrolled there to become a
	// member of EVERY account (the cross-account super-admin escape hatch the
	// engine honors). Query it explicitly so those edges round-trip; omitting them
	// would silently drop a super-admin's reach on export/import.
	acctIDs := make([]string, 0, len(accounts)+1)
	for _, a := range accounts {
		acctIDs = append(acctIDs, a.ID)
	}
	acctIDs = append(acctIDs, model.AccountWildcard)
	for _, id := range acctIDs {
		ms, err := store.MembershipsForAccount(ctx, id)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: memberships for account", err)
		}
		for _, m := range ms {
			doc.Memberships = append(doc.Memberships, Membership{Principal: m.PrincipalID, Account: m.AccountID})
		}
	}
	sort.Slice(doc.Memberships, func(i, j int) bool {
		if doc.Memberships[i].Account != doc.Memberships[j].Account {
			return doc.Memberships[i].Account < doc.Memberships[j].Account
		}
		return doc.Memberships[i].Principal < doc.Memberships[j].Principal
	})

	objectTypes, err := store.ListObjectTypes(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list object types", err)
	}
	sort.Slice(objectTypes, func(i, j int) bool { return objectTypes[i].Name < objectTypes[j].Name })
	for _, ot := range objectTypes {
		doc.ObjectTypes = append(doc.ObjectTypes, ObjectType{
			Name: ot.Name, Description: ot.Description, Actions: ot.Actions,
		})
	}

	permissions, err := store.ListPermissions(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list permissions", err)
	}
	sort.Slice(permissions, func(i, j int) bool { return permissions[i].ID < permissions[j].ID })
	for _, p := range permissions {
		doc.Permissions = append(doc.Permissions, Permission{
			ID: p.ID, ObjectType: p.ObjectType, Action: p.Action,
			ScopeStrategy: p.ScopeStrategy, Delegatable: p.Delegatable, Description: p.Description,
		})
	}

	principals, err := store.ListPrincipals(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list principals", err)
	}
	sort.Slice(principals, func(i, j int) bool { return principals[i].ID < principals[j].ID })
	for _, p := range principals {
		doc.Principals = append(doc.Principals, Principal{
			ID: p.ID, Kind: string(p.Kind), Identity: p.Identity,
			DisplayName: p.DisplayName, Roles: p.RoleIDs,
		})
	}

	roles, err := store.ListRoles(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list roles", err)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].ID < roles[j].ID })
	for _, r := range roles {
		doc.Roles = append(doc.Roles, Role{
			ID: r.ID, Name: r.Name, Description: r.Description, Permissions: r.PermissionIDs,
		})
	}

	groups, err := store.ListGroups(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list groups", err)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	for _, g := range groups {
		doc.Groups = append(doc.Groups, Group{
			ID: g.ID, Name: g.Name, Description: g.Description, Members: g.MemberPrincipalIDs,
		})
	}

	// Grants are account-scoped in the model, so they are gathered per account
	// (accounts already sorted) and each account's grants sorted by id. The
	// wildcard account "*" carries cross-account grants (e.g. a super-admin group's
	// reach) and is not among `accounts`, so it is queried explicitly here — same
	// reasoning as the wildcard memberships above; dropping it would lose every
	// "*"-stamped grant on export.
	for _, id := range acctIDs {
		gs, err := store.ListGrants(ctx, id)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list grants", err)
		}
		sort.Slice(gs, func(i, j int) bool { return gs[i].ID < gs[j].ID })
		for _, g := range gs {
			doc.Grants = append(doc.Grants, Grant{
				ID:         g.ID,
				Account:    g.AccountID,
				Subject:    Subject{Kind: string(g.Subject.Kind), ID: g.Subject.ID},
				Permission: g.PermissionID,
				Object:     g.Object,
				Effect:     string(g.Effect),
			})
		}
	}

	templates, err := store.ListTemplates(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list templates", err)
	}
	model.SortTemplates(templates)
	for _, t := range templates {
		doc.Templates = append(doc.Templates, templateFromModel(t))
	}

	storedRules, err := store.ListRules(ctx)
	if err != nil {
		return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: list rules", err)
	}
	model.SortRules(storedRules)
	for _, r := range storedRules {
		ast, err := canonicalRuleAST(r.AST)
		if err != nil {
			return nil, aerr.Wrapf(aerr.APERTURE_RULE_INVALID, err, "export: rule %q", r.Name)
		}
		doc.Rules = append(doc.Rules, Rule{Name: r.Name, Description: r.Description, AST: ast})
	}

	return doc, nil
}

// templateFromModel converts a model.Template to its declarative form.
func templateFromModel(t model.Template) Template {
	params := make([]TemplateParam, len(t.Params))
	for i, p := range t.Params {
		params[i] = TemplateParam{Name: p.Name, Type: string(p.Type), Description: p.Description}
	}
	grants := make([]TemplateGrant, len(t.Grants))
	for i, g := range t.Grants {
		grants[i] = TemplateGrant{
			Subject:    Subject{Kind: string(g.Subject.Kind), ID: g.Subject.ID},
			Permission: g.PermissionID,
			Object:     g.Object,
			Effect:     string(g.Effect),
		}
	}
	return Template{
		Name: t.Name, Version: t.Version, Description: t.Description,
		Params: params, Grants: grants,
	}
}

// canonicalRuleAST re-serializes a stored rule AST through rules.Node so the
// exported bytes are EXACTLY the rules package's canonical form (stable field
// order, omitempty, no incidental whitespace) — the same serialization the node
// editor round-trips. This is what makes a rule export byte-stable regardless of
// how the AST was originally authored.
func canonicalRuleAST(raw json.RawMessage) (json.RawMessage, error) {
	var node rules.Node
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	out, err := json.Marshal(&node)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// Marshal renders a Document to the on-disk state-file bytes in the given format.
// JSON is indented and human-diffable with a trailing newline; YAML routes
// through JSON so the rule AST keeps its canonical shape and every json tag /
// omitempty is honoured before re-encoding. Both are deterministic given a
// Document produced by Export, so a re-export is byte-stable.
func Marshal(doc *Document, format Format) ([]byte, error) {
	switch format {
	case FormatJSON:
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: marshal JSON", err)
		}
		return append(b, '\n'), nil
	case FormatYAML:
		jb, err := json.Marshal(doc)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: marshal document", err)
		}
		var generic any
		if err := json.Unmarshal(jb, &generic); err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: normalize document", err)
		}
		b, err := yaml.Marshal(generic)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_STORAGE, "export: marshal YAML", err)
		}
		return b, nil
	default:
		return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT, "export: unknown format %q", format)
	}
}

// FormatFor maps a file path's extension to the state-file Format: .json -> JSON,
// anything else -> YAML (the documented default). It is exported so the CLI and
// service surfaces pick a format from a filename identically to the seed loader.
func FormatFor(path string) Format { return formatFor(path) }
