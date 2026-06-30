package model

import (
	"testing"

	aerr "github.com/frankbardon/aperture/errors"
)

func TestValidatePermissionAcceptsDeclaredAction(t *testing.T) {
	ot := ObjectType{Name: "document", Actions: []string{"read", "write", "delete"}}
	p := Permission{ID: "p1", ObjectType: "document", Action: "write"}
	if err := ValidatePermission(p, ot); err != nil {
		t.Fatalf("declared action rejected: %v", err)
	}
}

func TestValidatePermissionRejectsUndeclaredAction(t *testing.T) {
	ot := ObjectType{Name: "document", Actions: []string{"read", "write"}}
	p := Permission{ID: "p1", ObjectType: "document", Action: "publish"}
	err := ValidatePermission(p, ot)
	if err == nil {
		t.Fatal("undeclared action accepted")
	}
	if got := aerr.CodeOf(err); got != aerr.APERTURE_ACTION_UNDECLARED {
		t.Fatalf("code = %s, want %s", got, aerr.APERTURE_ACTION_UNDECLARED)
	}
}

func TestValidateObjectTypeRejectsEmptyVerbSet(t *testing.T) {
	err := ValidateObjectType(ObjectType{Name: "document"})
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %s, want %s", got, aerr.APERTURE_INVALID_INPUT)
	}
}

func TestValidateObjectTypeRejectsDuplicateVerb(t *testing.T) {
	err := ValidateObjectType(ObjectType{Name: "document", Actions: []string{"read", "read"}})
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("code = %s, want %s", got, aerr.APERTURE_INVALID_INPUT)
	}
}

func TestValidateGrantRequiresAccountStamp(t *testing.T) {
	g := Grant{
		ID:           "g1",
		Subject:      Subject{Kind: SubjectPrincipal, ID: "user:alice"},
		PermissionID: "p1",
		Object:       "account:acme/**",
		Effect:       EffectAllow,
	}
	err := ValidateGrant(g)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_INVALID_INPUT {
		t.Fatalf("unstamped grant: code = %s, want %s", got, aerr.APERTURE_INVALID_INPUT)
	}
}

func TestValidateGrantRejectsMalformedPattern(t *testing.T) {
	g := Grant{
		ID:           "g1",
		AccountID:    "acme",
		Subject:      Subject{Kind: SubjectGroup, ID: "eng"},
		PermissionID: "p1",
		Object:       "account:acme/", // trailing empty segment
		Effect:       EffectDeny,
	}
	err := ValidateGrant(g)
	if got := aerr.CodeOf(err); got != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("malformed pattern: code = %s, want %s", got, aerr.APERTURE_IDENTITY_INVALID)
	}
}

func TestValidateGrantAcceptsWildcardPattern(t *testing.T) {
	g := Grant{
		ID:           "g1",
		AccountID:    "acme",
		Subject:      Subject{Kind: SubjectRole, ID: "admin"},
		PermissionID: "p1",
		Object:       "account:acme/project:*/**",
		Effect:       EffectAllow,
	}
	if err := ValidateGrant(g); err != nil {
		t.Fatalf("wildcard grant rejected: %v", err)
	}
}
