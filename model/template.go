package model

import (
	"sort"
	"strconv"
	"strings"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// Template is a named, versioned bundle of parameterized grants used to
// provision principals fast and consistently (FR-18, FR-19). A template
// declares typed PARAMETERS (e.g. account, project, object) and a set of
// template GRANTS whose subject ids and object patterns reference those
// parameters with ${name} tokens. At apply time the parameters are filled with
// concrete values, the tokens are substituted, and the bundle expands to a set
// of concrete model.Grant values applied transactionally.
//
// Versioning: a template is identified by the (Name, Version) pair. Storing a
// new Version under an existing Name keeps the older versions intact, so an
// apply can pin a specific version while new provisioning uses the latest. The
// storage layer's GetTemplate selects the latest version when asked for version
// <= 0.
//
// Template is a PUBLIC contract: E5-S2 (export/import) serializes exactly this
// shape and E6-S3 (templates UI) edits it, so the field set is additive-only.
type Template struct {
	// Name is the template's stable, human-meaningful name. Together with Version
	// it is the template's identity.
	Name string
	// Version is the monotonic version number, starting at 1. A higher version is
	// "newer"; apply selects the latest by default.
	Version int
	// Description is optional human-readable documentation.
	Description string
	// Params are the typed parameters the template declares. Every ${name} token a
	// grant references MUST name one of these (validated at definition time).
	Params []TemplateParam
	// Grants are the parameterized grant templates the bundle expands to.
	Grants []TemplateGrant
	// CreatedAt / UpdatedAt are stamped by the service layer and persisted verbatim.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ParamType is the declared type of a template parameter. It governs how an
// apply-time value is validated before substitution.
type ParamType string

const (
	// ParamSegment is an identity-component value: a single type or id token made
	// of the identity grammar's legal characters (letters, digits, and -._~@+),
	// with no '/' or ':'. It is the type for parameters that fill a type:id slot in
	// an object pattern (e.g. an account id "acme", a project id "atlas", an object
	// id "42"). A segment value is validated so the substituted pattern always
	// parses.
	ParamSegment ParamType = "segment"
	// ParamString is a free-form value with no identity-grammar constraint. Use it
	// for descriptive substitutions that are not part of an identity path.
	ParamString ParamType = "string"
)

// Valid reports whether t is a recognised parameter type.
func (t ParamType) Valid() bool {
	return t == ParamSegment || t == ParamString
}

// TemplateParam declares one typed parameter a template accepts.
type TemplateParam struct {
	// Name is the parameter's name; it is the token referenced as ${Name} in grant
	// subject ids and object patterns. Non-empty and unique within a template.
	Name string
	// Type is the parameter's declared type, validated against an apply value.
	Type ParamType
	// Description is optional human-readable documentation.
	Description string
}

// TemplateGrant is one parameterized grant in a template. Its Subject.ID and
// Object may contain ${name} tokens that are substituted at apply time; the
// AccountID and grant ID are supplied by the apply, not stored on the template
// (a template is account-agnostic so the same bundle provisions any account).
type TemplateGrant struct {
	// Subject is the grant subject; Subject.ID may contain ${name} tokens.
	Subject Subject
	// PermissionID references the granted Permission. It is not parameterized.
	PermissionID string
	// Object is the identity pattern (string form) the grant scopes to; it may
	// contain ${name} tokens.
	Object string
	// Effect is allow or deny.
	Effect Effect
}

// TemplateApplication is the request to apply a template: which template and
// version, the target account every expanded grant is stamped to, the parameter
// values to substitute, and an optional id prefix for the generated grant ids.
type TemplateApplication struct {
	// Name is the template name to apply.
	Name string
	// Version selects the template version; <= 0 means the latest.
	Version int
	// Account is the account every expanded grant is stamped to (the isolation
	// boundary). Mandatory.
	Account string
	// Params maps each declared parameter name to its concrete value.
	Params map[string]string
	// GrantIDPrefix prefixes the generated grant ids so a re-apply with the same
	// prefix upserts (idempotent provisioning) rather than duplicating. When empty
	// it defaults to "<name>-v<version>".
	GrantIDPrefix string
}

// ValidateTemplate checks a template is well-formed at DEFINITION time: a
// non-empty name, a version of at least 1, well-formed unique typed parameters,
// at least one grant, each grant structurally complete, and every ${name} token
// any grant references declared as a parameter. What CANNOT be checked at
// definition time (e.g. whether a substituted action is in an object type's verb
// set) is deferred to apply; everything structural is caught here so a bad
// template never reaches apply.
func ValidateTemplate(t Template) error {
	if t.Name == "" {
		return aerr.New(aerr.APERTURE_TEMPLATE_INVALID, "template name is empty")
	}
	if t.Version < 1 {
		return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
			"template version must be at least 1",
			map[string]any{"template": t.Name, "version": t.Version})
	}
	declared := make(map[string]struct{}, len(t.Params))
	for _, p := range t.Params {
		if p.Name == "" {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template has a parameter with an empty name",
				map[string]any{"template": t.Name})
		}
		if !p.Type.Valid() {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template parameter has an unknown type",
				map[string]any{"template": t.Name, "param": p.Name, "type": string(p.Type)})
		}
		if _, dup := declared[p.Name]; dup {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template declares a duplicate parameter",
				map[string]any{"template": t.Name, "param": p.Name})
		}
		declared[p.Name] = struct{}{}
	}
	if len(t.Grants) == 0 {
		return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
			"template declares no grants",
			map[string]any{"template": t.Name})
	}
	for i, g := range t.Grants {
		if !g.Subject.Kind.Valid() {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template grant subject kind is not principal, role, or group",
				map[string]any{"template": t.Name, "grant_index": i, "subject_kind": string(g.Subject.Kind)})
		}
		if g.Subject.ID == "" {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template grant subject has no id",
				map[string]any{"template": t.Name, "grant_index": i})
		}
		if g.PermissionID == "" {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template grant has no permission reference",
				map[string]any{"template": t.Name, "grant_index": i})
		}
		if !g.Effect.Valid() {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template grant effect is not allow or deny",
				map[string]any{"template": t.Name, "grant_index": i, "effect": string(g.Effect)})
		}
		if g.Object == "" {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
				"template grant has no object pattern",
				map[string]any{"template": t.Name, "grant_index": i})
		}
		// Every token referenced by the grant must be a declared parameter.
		for _, field := range []string{g.Subject.ID, g.Object} {
			toks, err := paramTokens(field)
			if err != nil {
				return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
					"template grant has a malformed ${...} parameter token",
					map[string]any{"template": t.Name, "grant_index": i, "field": field})
			}
			for _, tok := range toks {
				if _, ok := declared[tok]; !ok {
					return aerr.WithContext(aerr.APERTURE_TEMPLATE_INVALID,
						"template grant references an undeclared parameter",
						map[string]any{"template": t.Name, "grant_index": i, "param": tok})
				}
			}
		}
	}
	return nil
}

// ExpandTemplate resolves t against app: it validates the supplied parameters
// against the declared set and their types, substitutes the ${name} tokens in
// every grant, stamps each expanded grant with app.Account, a deterministic id,
// and now, and returns the concrete grants ready to apply. It does NOT write
// anything — the caller applies the returned grants transactionally.
//
// The expansion is all-or-nothing: if any parameter is missing, unknown, or
// ill-typed, or any expanded grant fails grant validation, ExpandTemplate
// returns an error and NO grants, so a partial expansion can never be applied.
func ExpandTemplate(t Template, app TemplateApplication, now time.Time) ([]Grant, error) {
	if app.Account == "" {
		return nil, aerr.WithContext(aerr.APERTURE_TEMPLATE_PARAM,
			"template apply has no target account",
			map[string]any{"template": t.Name})
	}
	// Every declared parameter must have a value; the value must satisfy its type.
	values := make(map[string]string, len(t.Params))
	declared := make(map[string]struct{}, len(t.Params))
	for _, p := range t.Params {
		declared[p.Name] = struct{}{}
		v, ok := app.Params[p.Name]
		if !ok {
			return nil, aerr.WithContext(aerr.APERTURE_TEMPLATE_PARAM,
				"template apply is missing a required parameter",
				map[string]any{"template": t.Name, "param": p.Name})
		}
		if err := validateParamValue(p, v); err != nil {
			return nil, err
		}
		values[p.Name] = v
	}
	// No argument may name a parameter the template does not declare.
	for name := range app.Params {
		if _, ok := declared[name]; !ok {
			return nil, aerr.WithContext(aerr.APERTURE_TEMPLATE_PARAM,
				"template apply supplies an unknown parameter",
				map[string]any{"template": t.Name, "param": name})
		}
	}

	prefix := app.GrantIDPrefix
	if prefix == "" {
		prefix = t.Name + "-v" + strconv.Itoa(t.Version)
	}
	out := make([]Grant, 0, len(t.Grants))
	for i, tg := range t.Grants {
		subjectID, err := substitute(tg.Subject.ID, values)
		if err != nil {
			return nil, err
		}
		object, err := substitute(tg.Object, values)
		if err != nil {
			return nil, err
		}
		g := Grant{
			ID:           prefix + "-" + strconv.Itoa(i),
			AccountID:    app.Account,
			Subject:      Subject{Kind: tg.Subject.Kind, ID: subjectID},
			PermissionID: tg.PermissionID,
			Object:       object,
			Effect:       tg.Effect,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := ValidateGrant(g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// validateParamValue checks an apply value against its parameter's declared
// type. A segment value must be a legal identity component so the substituted
// pattern parses; a string value is unconstrained (but must be non-empty).
func validateParamValue(p TemplateParam, v string) error {
	if v == "" {
		return aerr.WithContext(aerr.APERTURE_TEMPLATE_PARAM,
			"template parameter value is empty",
			map[string]any{"param": p.Name})
	}
	if p.Type == ParamSegment {
		// A segment must be usable as one component of a type:id pair. Parsing
		// "x:<v>" as an identity exercises the same component grammar substitution
		// will rely on.
		if _, err := identity.Parse("x:" + v); err != nil {
			return aerr.WithContext(aerr.APERTURE_TEMPLATE_PARAM,
				"template segment parameter value is not a legal identity component",
				map[string]any{"param": p.Name, "value": v})
		}
	}
	return nil
}

// paramTokens returns the parameter names referenced by ${name} tokens in s, in
// order of first appearance. It errors on a malformed token: a "${" with no
// closing "}", or an empty or illegally-named token.
func paramTokens(s string) ([]string, error) {
	var out []string
	seen := map[string]struct{}{}
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "${")
		if j < 0 {
			break
		}
		start := i + j + 2
		end := strings.IndexByte(s[start:], '}')
		if end < 0 {
			return nil, errBadToken
		}
		name := s[start : start+end]
		if !validTokenName(name) {
			return nil, errBadToken
		}
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			out = append(out, name)
		}
		i = start + end + 1
	}
	return out, nil
}

// substitute replaces every ${name} token in s with values[name]. It assumes
// the tokens were validated at definition time (paramTokens) and the values map
// covers every declared parameter, so an unresolved token is a programming error
// surfaced as a template-param error rather than silently left in place.
func substitute(s string, values map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "${")
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		start := i + j + 2
		end := strings.IndexByte(s[start:], '}')
		if end < 0 {
			return "", errBadToken
		}
		name := s[start : start+end]
		v, ok := values[name]
		if !ok {
			return "", aerr.WithContext(aerr.APERTURE_TEMPLATE_PARAM,
				"template references an unresolved parameter",
				map[string]any{"param": name})
		}
		b.WriteString(v)
		i = start + end + 1
	}
	return b.String(), nil
}

// validTokenName reports whether name is a legal ${...} token name: a non-empty
// identifier of letters, digits, and underscores starting with a letter.
func validTokenName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			// always allowed
		case r == '_':
			// allowed
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var errBadToken = aerr.New(aerr.APERTURE_TEMPLATE_INVALID, "malformed ${...} parameter token")

// SortTemplates orders templates by name then ascending version, so list output
// is stable across backends.
func SortTemplates(ts []Template) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Name != ts[j].Name {
			return ts[i].Name < ts[j].Name
		}
		return ts[i].Version < ts[j].Version
	})
}
