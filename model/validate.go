package model

import (
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/identity"
)

// ValidateObjectType checks an object type is well-formed before persistence: a
// non-empty name and a non-empty verb set with no empty or duplicate verbs. A
// type with no declared actions could never carry a valid permission, so it is
// rejected here rather than failing mysteriously at permission time.
func ValidateObjectType(ot ObjectType) error {
	if ot.Name == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "object type name is empty")
	}
	if len(ot.Actions) == 0 {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"object type declares no actions",
			map[string]any{"object_type": ot.Name})
	}
	seen := make(map[string]struct{}, len(ot.Actions))
	for _, a := range ot.Actions {
		if a == "" {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"object type has an empty action verb",
				map[string]any{"object_type": ot.Name})
		}
		if _, dup := seen[a]; dup {
			return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
				"object type has a duplicate action verb",
				map[string]any{"object_type": ot.Name, "action": a})
		}
		seen[a] = struct{}{}
	}
	return nil
}

// ValidatePermission enforces typed-action validation: the permission's action
// MUST be in the object type's declared verb set. ot is the object type the
// permission targets (resolved by the storage layer from p.ObjectType). A
// permission naming an undeclared action is rejected with
// APERTURE_ACTION_UNDECLARED; a structurally malformed permission with
// APERTURE_INVALID_INPUT.
func ValidatePermission(p Permission, ot ObjectType) error {
	if p.ID == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "permission id is empty")
	}
	if p.ObjectType == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"permission has no object type",
			map[string]any{"permission": p.ID})
	}
	if p.Action == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"permission has no action",
			map[string]any{"permission": p.ID})
	}
	if p.ObjectType != ot.Name {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"permission object type does not match the resolved object type",
			map[string]any{"permission": p.ID, "object_type": p.ObjectType, "resolved": ot.Name})
	}
	if !ot.HasAction(p.Action) {
		return aerr.WithContext(aerr.APERTURE_ACTION_UNDECLARED,
			"permission action is not declared on the object type",
			map[string]any{"permission": p.ID, "object_type": p.ObjectType, "action": p.Action})
	}
	return nil
}

// ValidatePrincipal checks a principal is well-formed: non-empty id, a valid
// kind, and a parseable identity string.
func ValidatePrincipal(p Principal) error {
	if p.ID == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "principal id is empty")
	}
	if !p.Kind.Valid() {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"principal kind is not user or machine",
			map[string]any{"principal": p.ID, "kind": string(p.Kind)})
	}
	if p.Identity == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"principal has no identity",
			map[string]any{"principal": p.ID})
	}
	if _, err := identity.Parse(p.Identity); err != nil {
		return err
	}
	return nil
}

// ValidateRole checks a role is well-formed: non-empty id and name.
func ValidateRole(r Role) error {
	if r.ID == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "role id is empty")
	}
	if r.Name == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"role has no name",
			map[string]any{"role": r.ID})
	}
	return nil
}

// ValidateGroup checks a group is well-formed: non-empty id and name.
func ValidateGroup(g Group) error {
	if g.ID == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "group id is empty")
	}
	if g.Name == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"group has no name",
			map[string]any{"group": g.ID})
	}
	return nil
}

// ValidateGrant checks a grant is well-formed and ready to persist: non-empty
// id, an account stamp (the isolation boundary), a valid subject, a permission
// reference, a valid effect, and an object that parses as an identity PATTERN
// (wildcards allowed). The pattern is validated here so a malformed grant can
// never reach the decision hot path.
func ValidateGrant(g Grant) error {
	if g.ID == "" {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "grant id is empty")
	}
	if g.AccountID == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"grant is not stamped with an account",
			map[string]any{"grant": g.ID})
	}
	if !g.Subject.Kind.Valid() {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"grant subject kind is not principal, role, or group",
			map[string]any{"grant": g.ID, "subject_kind": string(g.Subject.Kind)})
	}
	if g.Subject.ID == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"grant subject has no id",
			map[string]any{"grant": g.ID})
	}
	if g.PermissionID == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"grant has no permission reference",
			map[string]any{"grant": g.ID})
	}
	if !g.Effect.Valid() {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"grant effect is not allow or deny",
			map[string]any{"grant": g.ID, "effect": string(g.Effect)})
	}
	if g.Object == "" {
		return aerr.WithContext(aerr.APERTURE_INVALID_INPUT,
			"grant has no object pattern",
			map[string]any{"grant": g.ID})
	}
	if _, err := identity.ParsePattern(g.Object); err != nil {
		return err
	}
	return nil
}
