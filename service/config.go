package service

import (
	"os"
	"strconv"
	"strings"

	aerr "github.com/frankbardon/aperture/errors"
)

// Managed answers "does this deployment manage this kind of entity?" for one
// entity kind. It is a three-state type rather than a bool for one reason: the
// documented default is MANAGED, and Go's zero bool is false. A struct of plain
// bools would make ManagedEntities{} — the value a caller gets from forgetting a
// field, or from a future YAML document that omits the key — silently mean "this
// deployment manages nothing", which is the opposite of the default and would
// lock a deployment out of its own entity CRUD.
//
// Keeping "unset" distinct from "explicitly true" also costs nothing and buys
// honesty: an operator posture that was never configured is not the same fact as
// one an operator deliberately turned on, and a capability read can say so.
//
// Polarity is positive throughout — the type, the env vars, and the flags all
// spell the ENABLED state. There is no inverted boolean anywhere on this path.
type Managed uint8

const (
	// ManagedDefault is the zero value and means MANAGED. An operator who has
	// configured nothing gets Aperture's historical behaviour: every account,
	// principal, and membership mutation is available.
	ManagedDefault Managed = iota
	// ManagedYes is an explicit opt-in. Behaviourally identical to
	// ManagedDefault; it records that an operator chose it.
	ManagedYes
	// ManagedNo is an explicit opt-out: this deployment does not manage the
	// entity kind, and Aperture refuses to create, update, or delete it.
	ManagedNo
)

// Enabled reports whether Aperture manages the entity kind. Only an explicit
// ManagedNo disables it, so both the zero value and an explicit opt-in are
// managed — the fail-open-to-today's-behaviour direction, chosen because this
// switch is deployment posture, not an authorization decision.
func (m Managed) Enabled() bool { return m != ManagedNo }

// ManagedFrom converts a plain boolean decision (a CLI flag's value, a
// deserialized field) into an explicit Managed. It never yields ManagedDefault:
// a caller holding a bool has already decided.
func ManagedFrom(enabled bool) Managed {
	if enabled {
		return ManagedYes
	}
	return ManagedNo
}

// String renders the state for diagnostics and error context.
func (m Managed) String() string {
	switch m {
	case ManagedYes:
		return "managed (explicit)"
	case ManagedNo:
		return "unmanaged"
	default:
		return "managed (default)"
	}
}

// ManagedEntities declares which entity kinds this Aperture deployment owns the
// lifecycle of. It is boot-time deployment posture, read once at startup and
// never mutated afterwards, and it is ORTHOGONAL to authorization: authorization
// asks "may this actor do this?", while this asks "does this deployment manage
// this entity at all?". A deployment whose accounts are mastered by an upstream
// system sets Accounts to ManagedNo so Aperture refuses every account write
// regardless of who is asking.
//
// It covers the WHOLE lifecycle, not creation alone — Put and Delete both — which
// is why the verb is "manage". Aperture's entity writes are upserts with no
// create-only variant, so "allow updates but not creates" is not a distinction
// the write path can make without a read-before-write and a TOCTOU window.
//
// Nothing here affects the decision path: Check, Enumerate, and Explain never
// consult it, and reads of the entities themselves stay available.
//
// The zero value means the documented default — every kind managed — so a
// Service constructed without WithManagedEntities behaves exactly as Aperture
// always has.
type ManagedEntities struct {
	// Accounts governs account records: Service.PutAccount and
	// Service.DeleteAccount. Set from APERTURE_MANAGE_ACCOUNTS / --manage-accounts.
	Accounts Managed
	// Principals governs principal records: Service.PutPrincipal and
	// Service.DeletePrincipal. Set from APERTURE_MANAGE_PRINCIPALS /
	// --manage-principals.
	Principals Managed
	// Memberships governs the principal-to-account edge: Service.PutMembership
	// and Service.DeleteMembership. It is independent of Accounts and Principals
	// on purpose — a deployment can master its accounts and principals upstream
	// while still managing who belongs to what, or the reverse. Set from
	// APERTURE_MANAGE_MEMBERSHIPS / --manage-memberships.
	Memberships Managed
}

// Capabilities is what a client may DO with this deployment's entity model,
// flattened to plain booleans: one per gated entity kind, true when Aperture
// manages the kind and false when an operator has declared it mastered
// elsewhere. It is the shape a surface hands a caller so the caller can render
// the controls it is allowed to use, instead of discovering the lock by
// attempting a write and reading APERTURE_ENTITY_UNMANAGED back.
//
// It deliberately collapses Managed's third state. ManagedDefault and
// ManagedYes differ only in whether an operator typed the value; both mean the
// kind is writable, and no client can act on the distinction. Reporting it would
// also disclose more of an operator's configuration than the question asked.
//
// It carries booleans and nothing else — no ids, no counts, no model data — so
// that a surface can answer the question without authenticating the caller.
//
// # What it deliberately does not absorb
//
// Every field here is BOOT-TIME operator configuration, and that is a contract
// and not an accident. It is what makes the read safe to leave open, safe to
// cache once on page load, and unable to fail.
//
// Runtime FAULT state does not belong here, however posture-shaped it looks. The
// worked case is stale shared wiring: an instance whose background refresh is
// failing keeps deciding from the wiring it has, which is mutable state, whose
// useful half is a DURATION rather than a boolean, and whose disclosure ("this
// instance is enforcing configuration its operator already replaced, and has
// been for four hours") is operational intelligence an anonymous caller should
// not get. It lives on WiringPosture behind a system-admin gate instead. See
// service/wiring_posture.go for the full argument — it is written down because
// the pull towards adding one more boolean here is strong and the contract this
// paragraph protects is invisible from the call site.
type Capabilities struct {
	// ManageAccounts reports whether PutAccount and DeleteAccount are available.
	ManageAccounts bool
	// ManagePrincipals reports whether PutPrincipal and DeletePrincipal are
	// available.
	ManagePrincipals bool
	// ManageMemberships reports whether PutMembership and DeleteMembership are
	// available. Independent of the other two.
	ManageMemberships bool
}

// Capabilities reports what this deployment lets a client do to the gated entity
// kinds. It reads immutable boot-time configuration, touches no storage, needs
// no actor, and cannot fail — which is what lets every surface expose it as an
// open, unauthenticated call.
//
// The zero-value posture (a Service built without WithManagedEntities) reports
// all three true, Aperture's historical behaviour.
func (s *Service) Capabilities() Capabilities {
	m := s.ManagedEntities()
	return Capabilities{
		ManageAccounts:    m.Accounts.Enabled(),
		ManagePrincipals:  m.Principals.Enabled(),
		ManageMemberships: m.Memberships.Enabled(),
	}
}

// Environment variable names. All under the APERTURE_ namespace per convention,
// all positive polarity, and all defaulting to enabled when unset or empty.
const (
	EnvManageAccounts    = "APERTURE_MANAGE_ACCOUNTS"
	EnvManagePrincipals  = "APERTURE_MANAGE_PRINCIPALS"
	EnvManageMemberships = "APERTURE_MANAGE_MEMBERSHIPS"
)

// ManagedEntitiesFromEnv reads the deployment's entity-management posture from
// the APERTURE_MANAGE_* environment variables. An unset or empty variable leaves
// its kind at ManagedDefault (managed), so a process with no configuration at
// all keeps today's behaviour.
//
// A variable that is set but not parseable as a boolean is
// APERTURE_CONFIG_INVALID, never a silent default: an operator who typed
// "flase" and expected accounts to be locked must be told, not quietly given the
// opposite posture. Accepted spellings are strconv.ParseBool's (1/t/T/TRUE/true/
// True and 0/f/F/FALSE/false/False), which is also what the CLI's own boolean
// parsing accepts, so the two can never disagree about a value. Surrounding
// whitespace is trimmed first.
func ManagedEntitiesFromEnv() (ManagedEntities, error) {
	var cfg ManagedEntities
	for _, f := range []struct {
		env   string
		field *Managed
	}{
		{EnvManageAccounts, &cfg.Accounts},
		{EnvManagePrincipals, &cfg.Principals},
		{EnvManageMemberships, &cfg.Memberships},
	} {
		m, err := managedFromEnv(f.env)
		if err != nil {
			return ManagedEntities{}, err
		}
		*f.field = m
	}
	return cfg, nil
}

// managedFromEnv reads one APERTURE_MANAGE_* variable. Unset or empty is
// ManagedDefault; anything else must parse as a boolean.
func managedFromEnv(name string) (Managed, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return ManagedDefault, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return ManagedDefault, aerr.WithContext(aerr.APERTURE_CONFIG_INVALID,
			"service: "+name+" is not a boolean",
			map[string]any{
				"env":     name,
				"value":   raw,
				"valid":   "true|false (also 1|0, t|f, TRUE|FALSE)",
				"default": "true — the variable may simply be unset",
			})
	}
	return ManagedFrom(enabled), nil
}
