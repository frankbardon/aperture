// Package seed loads a minimal declarative authorization model (JSON or YAML)
// into a model.Storage so the engine can be exercised end to end. It is the
// human-authored on-ramp behind the `aperture check` and `aperture serve`
// demo: a single document describes object types, permissions, principals,
// roles, groups, and grants, and Apply upserts them in dependency order.
//
// This is deliberately a minimal subset — just enough to seed a Check. Full
// round-trip export/import (versioning, deletes, partial merges) is a later
// story (E5-S2); nothing here precludes it, since every write goes through the
// same validated Storage.Put* path the service layer uses.
package seed

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"gopkg.in/yaml.v3"
)

// Example is the committed org -> project -> document fixture (account "acme").
// It is the model the `check` command loads when no --seed file is supplied, and
// it backs the end-to-end test. ExampleAccount is the account it is stamped to.
//
//go:embed testdata/example.yaml
var Example []byte

// ExampleAccount is the account id every grant in Example is stamped to. The CLI
// uses it as the default --account so the demo works with no flags.
const ExampleAccount = "acme"

// Format selects how a seed document is decoded.
type Format string

const (
	// FormatYAML decodes the document as YAML (the default and the format of the
	// committed fixture).
	FormatYAML Format = "yaml"
	// FormatJSON decodes the document as JSON.
	FormatJSON Format = "json"
)

// Document is the declarative model: a flat list of each entity kind. Field tags
// cover both YAML and JSON so either format decodes into the same shape.
//
// This is also the EXPORT/IMPORT state file (E5-S2): the same Document the
// minimal seed loader started as, generalized to the complete model. An export
// file is a strict superset of a seed file — a seed that omits templates/rules
// loads unchanged, and an export reloads through the very same Apply path. The
// field set is additive-only so old seeds keep loading. Slices are emitted in a
// stable order (sorted by id/name) so a round-trip is byte-stable and
// human-diffable. Live host domain-object metadata is deliberately NOT part of
// this shape — that is the provider cache, never source of truth.
type Document struct {
	Accounts    []Account    `yaml:"accounts" json:"accounts"`
	Memberships []Membership `yaml:"memberships" json:"memberships"`
	ObjectTypes []ObjectType `yaml:"object_types" json:"object_types"`
	Permissions []Permission `yaml:"permissions" json:"permissions"`
	Principals  []Principal  `yaml:"principals" json:"principals"`
	Roles       []Role       `yaml:"roles" json:"roles"`
	Groups      []Group      `yaml:"groups" json:"groups"`
	Grants      []Grant      `yaml:"grants" json:"grants"`
	Templates   []Template   `yaml:"templates" json:"templates"`
	Rules       []Rule       `yaml:"rules" json:"rules"`
}

// Account mirrors model.Account in declarative form.
type Account struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
}

// Membership mirrors model.Membership in declarative form: the edge admitting a
// principal to an account.
type Membership struct {
	Principal string `yaml:"principal" json:"principal"`
	Account   string `yaml:"account" json:"account"`
}

// ObjectType mirrors model.ObjectType in declarative form.
type ObjectType struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Actions     []string `yaml:"actions" json:"actions"`
}

// Permission mirrors model.Permission in declarative form.
type Permission struct {
	ID            string `yaml:"id" json:"id"`
	ObjectType    string `yaml:"object_type" json:"object_type"`
	Action        string `yaml:"action" json:"action"`
	ScopeStrategy string `yaml:"scope_strategy" json:"scope_strategy"`
	Delegatable   bool   `yaml:"delegatable,omitempty" json:"delegatable,omitempty"`
	Description   string `yaml:"description" json:"description"`
}

// Principal mirrors model.Principal in declarative form.
type Principal struct {
	ID          string   `yaml:"id" json:"id"`
	Kind        string   `yaml:"kind" json:"kind"`
	Identity    string   `yaml:"identity" json:"identity"`
	DisplayName string   `yaml:"display_name" json:"display_name"`
	Roles       []string `yaml:"roles" json:"roles"`
}

// Role mirrors model.Role in declarative form.
type Role struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Permissions []string `yaml:"permissions" json:"permissions"`
}

// Group mirrors model.Group in declarative form.
type Group struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Members     []string `yaml:"members" json:"members"`
}

// Subject mirrors model.Subject in declarative form.
type Subject struct {
	Kind string `yaml:"kind" json:"kind"`
	ID   string `yaml:"id" json:"id"`
}

// Grant mirrors model.Grant in declarative form.
type Grant struct {
	ID         string  `yaml:"id" json:"id"`
	Account    string  `yaml:"account" json:"account"`
	Subject    Subject `yaml:"subject" json:"subject"`
	Permission string  `yaml:"permission" json:"permission"`
	Object     string  `yaml:"object" json:"object"`
	Effect     string  `yaml:"effect" json:"effect"`
}

// Template mirrors model.Template in declarative form: a named, versioned bundle
// of parameterized grants. The (name, version) pair is its identity.
type Template struct {
	Name        string          `yaml:"name" json:"name"`
	Version     int             `yaml:"version" json:"version"`
	Description string          `yaml:"description" json:"description"`
	Params      []TemplateParam `yaml:"params" json:"params"`
	Grants      []TemplateGrant `yaml:"grants" json:"grants"`
}

// TemplateParam mirrors model.TemplateParam in declarative form.
type TemplateParam struct {
	Name        string `yaml:"name" json:"name"`
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description" json:"description"`
}

// TemplateGrant mirrors model.TemplateGrant in declarative form. Subject.ID and
// Object may carry ${name} parameter tokens.
type TemplateGrant struct {
	Subject    Subject `yaml:"subject" json:"subject"`
	Permission string  `yaml:"permission" json:"permission"`
	Object     string  `yaml:"object" json:"object"`
	Effect     string  `yaml:"effect" json:"effect"`
}

// Rule mirrors model.Rule in declarative form: a named rule plus its AST. The
// AST is carried as raw JSON so it is EXACTLY the rules package's canonical
// serialization (a rules.Node) — the same shape the node editor reads and
// writes; this file never invents a second rule format.
type Rule struct {
	Name        string          `yaml:"name" json:"name"`
	Description string          `yaml:"description" json:"description"`
	AST         json.RawMessage `yaml:"ast" json:"ast"`
}

// Parse decodes a seed document from raw bytes in the given format.
//
// The YAML path routes through JSON (yaml -> generic -> json -> Document) so the
// rule AST, carried as raw JSON, decodes by exactly the same rules the JSON path
// uses. The struct's json and yaml tags are identical, so a YAML seed authored
// against the documented field names lands in the same shape either way.
func Parse(data []byte, format Format) (*Document, error) {
	var doc Document
	switch format {
	case FormatJSON:
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "seed: decode JSON document", err)
		}
	case FormatYAML:
		var generic any
		if err := yaml.Unmarshal(data, &generic); err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "seed: decode YAML document", err)
		}
		jb, err := json.Marshal(generic)
		if err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "seed: normalize YAML document", err)
		}
		if err := json.Unmarshal(jb, &doc); err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "seed: decode YAML document", err)
		}
	default:
		return nil, aerr.Newf(aerr.APERTURE_INVALID_INPUT, "seed: unknown format %q", format)
	}
	return &doc, nil
}

// Apply upserts the document into store in dependency order: object types,
// permissions, principals, roles, groups, then grants. Each write goes through
// the storage layer's validation, so a malformed entity surfaces the same coded
// error a programmatic Put would. Apply is not transactional — a failure may
// leave a partial model — which is acceptable for the seed-and-demo use case.
func (d *Document) Apply(ctx context.Context, store model.Storage) error {
	for _, a := range d.Accounts {
		if err := store.PutAccount(ctx, model.Account{
			ID:          a.ID,
			Name:        a.Name,
			Description: a.Description,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: account %q", a.ID)
		}
	}
	for _, ot := range d.ObjectTypes {
		if err := store.PutObjectType(ctx, model.ObjectType{
			Name:        ot.Name,
			Actions:     ot.Actions,
			Description: ot.Description,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: object type %q", ot.Name)
		}
	}
	for _, p := range d.Permissions {
		if err := store.PutPermission(ctx, model.Permission{
			ID:            p.ID,
			ObjectType:    p.ObjectType,
			Action:        p.Action,
			ScopeStrategy: p.ScopeStrategy,
			Delegatable:   p.Delegatable,
			Description:   p.Description,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: permission %q", p.ID)
		}
	}
	for _, p := range d.Principals {
		if err := store.PutPrincipal(ctx, model.Principal{
			ID:          p.ID,
			Kind:        model.PrincipalKind(p.Kind),
			Identity:    p.Identity,
			DisplayName: p.DisplayName,
			RoleIDs:     p.Roles,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: principal %q", p.ID)
		}
	}
	for _, m := range d.Memberships {
		if err := store.PutMembership(ctx, model.Membership{
			PrincipalID: m.Principal,
			AccountID:   m.Account,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: membership %q@%q", m.Principal, m.Account)
		}
	}
	for _, r := range d.Roles {
		if err := store.PutRole(ctx, model.Role{
			ID:            r.ID,
			Name:          r.Name,
			Description:   r.Description,
			PermissionIDs: r.Permissions,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: role %q", r.ID)
		}
	}
	for _, g := range d.Groups {
		if err := store.PutGroup(ctx, model.Group{
			ID:                 g.ID,
			Name:               g.Name,
			Description:        g.Description,
			MemberPrincipalIDs: g.Members,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: group %q", g.ID)
		}
	}
	for _, g := range d.Grants {
		if err := store.PutGrant(ctx, model.Grant{
			ID:        g.ID,
			AccountID: g.Account,
			Subject: model.Subject{
				Kind: model.SubjectKind(g.Subject.Kind),
				ID:   g.Subject.ID,
			},
			PermissionID: g.Permission,
			Object:       g.Object,
			Effect:       model.Effect(g.Effect),
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: grant %q", g.ID)
		}
	}
	for _, t := range d.Templates {
		if err := store.PutTemplate(ctx, t.toModel()); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: template %q v%d", t.Name, t.Version)
		}
	}
	for _, r := range d.Rules {
		// Validate the AST against the rules engine's own contract before storing,
		// so an import rejects a structurally broken rule (APERTURE_RULE_*) rather
		// than persisting a rule the engine could never compile.
		if err := validateRuleAST(r); err != nil {
			return aerr.Wrapf(aerr.APERTURE_RULE_INVALID, err, "seed: rule %q", r.Name)
		}
		if err := store.PutRule(ctx, model.Rule{
			Name:        r.Name,
			Description: r.Description,
			AST:         r.AST,
		}); err != nil {
			return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: rule %q", r.Name)
		}
	}
	return nil
}

// toModel converts a declarative Template to its model form.
func (t Template) toModel() model.Template {
	params := make([]model.TemplateParam, len(t.Params))
	for i, p := range t.Params {
		params[i] = model.TemplateParam{
			Name:        p.Name,
			Type:        model.ParamType(p.Type),
			Description: p.Description,
		}
	}
	grants := make([]model.TemplateGrant, len(t.Grants))
	for i, g := range t.Grants {
		grants[i] = model.TemplateGrant{
			Subject:      model.Subject{Kind: model.SubjectKind(g.Subject.Kind), ID: g.Subject.ID},
			PermissionID: g.Permission,
			Object:       g.Object,
			Effect:       model.Effect(g.Effect),
		}
	}
	return model.Template{
		Name:        t.Name,
		Version:     t.Version,
		Description: t.Description,
		Params:      params,
		Grants:      grants,
	}
}

// validateRuleAST parses a declarative rule's AST as a rules.Node and runs the
// engine's structural validation, so an import surfaces the engine's own coded
// error for a malformed rule rather than storing an uncompilable one.
func validateRuleAST(r Rule) error {
	if len(r.AST) == 0 {
		return aerr.Newf(aerr.APERTURE_RULE_INVALID, "rule %q has no AST", r.Name)
	}
	var node rules.Node
	if err := json.Unmarshal(r.AST, &node); err != nil {
		return aerr.Wrapf(aerr.APERTURE_RULE_INVALID, err, "rule %q AST is not a valid node", r.Name)
	}
	return node.Validate()
}

// Load parses data in the given format and applies it to store.
func Load(ctx context.Context, store model.Storage, data []byte, format Format) error {
	doc, err := Parse(data, format)
	if err != nil {
		return err
	}
	return doc.Apply(ctx, store)
}

// LoadFile reads the seed file at path, picks the format from its extension
// (.json -> JSON, .yaml/.yml/other -> YAML), and applies it to store.
func LoadFile(ctx context.Context, store model.Storage, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return aerr.Wrapf(aerr.APERTURE_INVALID_INPUT, err, "seed: read file %q", path)
	}
	return Load(ctx, store, data, formatFor(path))
}

// formatFor maps a file extension to a Format. Anything that is not .json is
// treated as YAML, since YAML is the documented default.
func formatFor(path string) Format {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return FormatJSON
	}
	return FormatYAML
}

// Describe returns a one-line summary of the document's contents — used by the
// CLI/serve startup log so an operator can confirm what was loaded.
func (d *Document) Describe() string {
	return fmt.Sprintf(
		"%d accounts, %d memberships, %d object-types, %d permissions, %d principals, %d roles, %d groups, %d grants, %d templates, %d rules",
		len(d.Accounts), len(d.Memberships), len(d.ObjectTypes), len(d.Permissions),
		len(d.Principals), len(d.Roles), len(d.Groups), len(d.Grants), len(d.Templates), len(d.Rules))
}
