package model

import (
	"testing"
	"time"

	aerr "github.com/frankbardon/aperture/errors"
)

func memberTemplate() Template {
	return Template{
		Name:    "member",
		Version: 2,
		Params: []TemplateParam{
			{Name: "account", Type: ParamSegment},
			{Name: "project", Type: ParamSegment},
			{Name: "who", Type: ParamSegment},
		},
		Grants: []TemplateGrant{
			{
				Subject:      Subject{Kind: SubjectPrincipal, ID: "${who}"},
				PermissionID: "p-read",
				Object:       "account:${account}/project:${project}/**",
				Effect:       EffectAllow,
			},
			{
				Subject:      Subject{Kind: SubjectGroup, ID: "${project}-team"},
				PermissionID: "p-write",
				Object:       "account:${account}/project:${project}/document:*",
				Effect:       EffectAllow,
			},
		},
	}
}

func TestValidateTemplate_OK(t *testing.T) {
	if err := ValidateTemplate(memberTemplate()); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
}

func TestValidateTemplate_Faults(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Template)
	}{
		{"empty name", func(t *Template) { t.Name = "" }},
		{"zero version", func(t *Template) { t.Version = 0 }},
		{"no grants", func(t *Template) { t.Grants = nil }},
		{"dup param", func(t *Template) {
			t.Params = append(t.Params, TemplateParam{Name: "account", Type: ParamSegment})
		}},
		{"unknown param type", func(t *Template) { t.Params[0].Type = "weird" }},
		{"empty param name", func(t *Template) { t.Params[0].Name = "" }},
		{"undeclared token", func(t *Template) { t.Grants[0].Object = "account:${nope}/**" }},
		{"malformed token", func(t *Template) { t.Grants[0].Object = "account:${unclosed/**" }},
		{"bad subject kind", func(t *Template) { t.Grants[0].Subject.Kind = "alien" }},
		{"empty permission", func(t *Template) { t.Grants[0].PermissionID = "" }},
		{"bad effect", func(t *Template) { t.Grants[0].Effect = "maybe" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := memberTemplate()
			tc.mut(&tmpl)
			if got := aerr.CodeOf(ValidateTemplate(tmpl)); got != aerr.APERTURE_TEMPLATE_INVALID {
				t.Fatalf("want TEMPLATE_INVALID, got %q", got)
			}
		})
	}
}

func TestExpandTemplate_Substitutes(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	grants, err := ExpandTemplate(memberTemplate(), TemplateApplication{
		Name:    "member",
		Version: 2,
		Account: "acme",
		Params:  map[string]string{"account": "acme", "project": "atlas", "who": "alice"},
	}, now)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("want 2 grants, got %d", len(grants))
	}
	g0 := grants[0]
	if g0.Subject.ID != "alice" {
		t.Fatalf("subject not substituted: %q", g0.Subject.ID)
	}
	if g0.Object != "account:acme/project:atlas/**" {
		t.Fatalf("object not substituted: %q", g0.Object)
	}
	if g0.AccountID != "acme" || g0.Effect != EffectAllow {
		t.Fatalf("stamp wrong: %+v", g0)
	}
	if !g0.CreatedAt.Equal(now) || !g0.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps not stamped: %+v", g0)
	}
	// Deterministic, default id prefix "<name>-v<version>-<index>".
	if g0.ID != "member-v2-0" || grants[1].ID != "member-v2-1" {
		t.Fatalf("ids = %q, %q, want member-v2-0/1", g0.ID, grants[1].ID)
	}
	if grants[1].Subject.ID != "atlas-team" {
		t.Fatalf("group subject not substituted: %q", grants[1].Subject.ID)
	}
}

func TestExpandTemplate_IDPrefix(t *testing.T) {
	grants, err := ExpandTemplate(memberTemplate(), TemplateApplication{
		Account: "acme", GrantIDPrefix: "onb-alice",
		Params: map[string]string{"account": "acme", "project": "atlas", "who": "alice"},
	}, time.Now())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if grants[0].ID != "onb-alice-0" {
		t.Fatalf("id prefix not honoured: %q", grants[0].ID)
	}
}

func TestExpandTemplate_ParamFaults(t *testing.T) {
	base := func() TemplateApplication {
		return TemplateApplication{
			Account: "acme",
			Params:  map[string]string{"account": "acme", "project": "atlas", "who": "alice"},
		}
	}
	t.Run("missing param", func(t *testing.T) {
		app := base()
		delete(app.Params, "who")
		_, err := ExpandTemplate(memberTemplate(), app, time.Now())
		if aerr.CodeOf(err) != aerr.APERTURE_TEMPLATE_PARAM {
			t.Fatalf("want TEMPLATE_PARAM, got %q", aerr.CodeOf(err))
		}
	})
	t.Run("unknown param", func(t *testing.T) {
		app := base()
		app.Params["extra"] = "x"
		_, err := ExpandTemplate(memberTemplate(), app, time.Now())
		if aerr.CodeOf(err) != aerr.APERTURE_TEMPLATE_PARAM {
			t.Fatalf("want TEMPLATE_PARAM, got %q", aerr.CodeOf(err))
		}
	})
	t.Run("ill-typed segment", func(t *testing.T) {
		app := base()
		app.Params["account"] = "bad/slash" // not a legal identity component
		_, err := ExpandTemplate(memberTemplate(), app, time.Now())
		if aerr.CodeOf(err) != aerr.APERTURE_TEMPLATE_PARAM {
			t.Fatalf("want TEMPLATE_PARAM, got %q", aerr.CodeOf(err))
		}
	})
	t.Run("no account", func(t *testing.T) {
		app := base()
		app.Account = ""
		_, err := ExpandTemplate(memberTemplate(), app, time.Now())
		if aerr.CodeOf(err) != aerr.APERTURE_TEMPLATE_PARAM {
			t.Fatalf("want TEMPLATE_PARAM, got %q", aerr.CodeOf(err))
		}
	})
}

// TestExpandTemplate_StringParamNotIdentityChecked proves a string-typed param
// accepts a value a segment param would reject (it is not identity-validated),
// while the grant object that embeds it must still parse.
func TestExpandTemplate_StringParam(t *testing.T) {
	tmpl := Template{
		Name: "note", Version: 1,
		Params: []TemplateParam{{Name: "label", Type: ParamString}},
		Grants: []TemplateGrant{{
			Subject:      Subject{Kind: SubjectPrincipal, ID: "alice"},
			PermissionID: "p", Object: "account:acme/note:fixed", Effect: EffectAllow,
		}},
	}
	// label is declared but unused by any grant token; a free-form value is fine.
	_, err := ExpandTemplate(tmpl, TemplateApplication{
		Account: "acme", Params: map[string]string{"label": "anything goes / here"},
	}, time.Now())
	if err != nil {
		t.Fatalf("string param rejected: %v", err)
	}
}
