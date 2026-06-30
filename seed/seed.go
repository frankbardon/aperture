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
type Document struct {
	ObjectTypes []ObjectType `yaml:"object_types" json:"object_types"`
	Permissions []Permission `yaml:"permissions" json:"permissions"`
	Principals  []Principal  `yaml:"principals" json:"principals"`
	Roles       []Role       `yaml:"roles" json:"roles"`
	Groups      []Group      `yaml:"groups" json:"groups"`
	Grants      []Grant      `yaml:"grants" json:"grants"`
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

// Parse decodes a seed document from raw bytes in the given format.
func Parse(data []byte, format Format) (*Document, error) {
	var doc Document
	switch format {
	case FormatJSON:
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "seed: decode JSON document", err)
		}
	case FormatYAML:
		if err := yaml.Unmarshal(data, &doc); err != nil {
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
	return nil
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
		"%d object-types, %d permissions, %d principals, %d roles, %d groups, %d grants",
		len(d.ObjectTypes), len(d.Permissions), len(d.Principals),
		len(d.Roles), len(d.Groups), len(d.Grants))
}
