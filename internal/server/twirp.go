package server

import (
	"context"
	"encoding/json"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/service"

	"github.com/twitchtv/twirp"
)

// twirpHandler implements the generated rpc.ApertureService by translating each
// RPC onto exactly one service.Service facade call. It owns the surface's
// auth policy — decision RPCs are open, entity reads and every mutation require
// an authenticated principal, and the admin-tier enforcement lives in the facade
// gate — and the coded-error → twirp.Error mapping, so the engine/model/storage
// layers stay transport-free.
//
// Security note: the actor a mutation is attributed to is ALWAYS the
// authenticated principal recovered from the request context (set by the
// Authenticate middleware), never a value taken from the request body. The
// wire's Actor.account is honoured (it selects the active account) but the
// principal on the wire is ignored — a caller cannot act as someone else.
type twirpHandler struct {
	svc *service.Service
}

// NewTwirpHandler returns an rpc.ApertureService over the facade.
func NewTwirpHandler(svc *service.Service) rpc.ApertureService {
	return &twirpHandler{svc: svc}
}

// principal recovers the authenticated principal id, or an APERTURE_UNAUTHENTICATED
// coded error when the request is anonymous. It is the require-a-principal gate
// the mutation and entity-read RPCs apply (the decision RPCs do not call it, so
// they stay open).
func (h *twirpHandler) principal(ctx context.Context) (string, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.ID == "" {
		return "", aerr.New(aerr.APERTURE_UNAUTHENTICATED,
			"this endpoint requires an authenticated principal")
	}
	return p.ID, nil
}

// actor builds the mutation actor from the authenticated principal plus the
// account named on the wire.
func (h *twirpHandler) actor(ctx context.Context, account string) (service.Actor, error) {
	id, err := h.principal(ctx)
	if err != nil {
		return service.Actor{}, err
	}
	return service.Actor{Principal: id, Account: account}, nil
}

func actorAccount(a *rpc.Actor) string {
	if a == nil {
		return ""
	}
	return a.Account
}

// ---- Decision API (open) ----

func (h *twirpHandler) Check(ctx context.Context, req *rpc.CheckRequest) (*rpc.Decision, error) {
	res, err := h.svc.Check(ctx, service.Query{
		Account: req.Account, Principal: req.Principal, Action: req.Action, Object: req.Object,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.Decision{Allow: res.Allow, Reason: res.Reason, DecidingGrantIds: res.DecidingGrantIDs}, nil
}

func (h *twirpHandler) CheckBatch(ctx context.Context, req *rpc.CheckBatchRequest) (*rpc.CheckBatchResponse, error) {
	qs := make([]service.Query, len(req.Queries))
	for i, q := range req.Queries {
		qs[i] = service.Query{Account: q.Account, Principal: q.Principal, Action: q.Action, Object: q.Object}
	}
	results := h.svc.CheckBatch(ctx, qs)
	out := make([]*rpc.BatchDecision, len(results))
	for i, r := range results {
		bd := &rpc.BatchDecision{}
		if r.Err != nil {
			bd.ErrorCode = string(aerr.CodeOf(r.Err))
			bd.ErrorMessage = r.Err.Error()
		} else {
			bd.Decision = &rpc.Decision{Allow: r.Result.Allow, Reason: r.Result.Reason, DecidingGrantIds: r.Result.DecidingGrantIDs}
		}
		out[i] = bd
	}
	return &rpc.CheckBatchResponse{Results: out}, nil
}

func (h *twirpHandler) Enumerate(ctx context.Context, req *rpc.EnumerateRequest) (*rpc.EnumerateResponse, error) {
	ids, err := h.svc.Enumerate(ctx, service.EnumerateQuery{
		Account: req.Account, Principal: req.Principal, Action: req.Action, Pattern: req.Pattern, Limit: int(req.Limit),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.EnumerateResponse{ObjectIds: ids}, nil
}

func (h *twirpHandler) EnumerateBatch(ctx context.Context, req *rpc.EnumerateBatchRequest) (*rpc.EnumerateBatchResponse, error) {
	qs := make([]service.EnumerateQuery, len(req.Queries))
	for i, q := range req.Queries {
		qs[i] = service.EnumerateQuery{Account: q.Account, Principal: q.Principal, Action: q.Action, Pattern: q.Pattern, Limit: int(q.Limit)}
	}
	results := h.svc.EnumerateBatch(ctx, qs)
	out := make([]*rpc.BatchEnumeration, len(results))
	for i, r := range results {
		be := &rpc.BatchEnumeration{}
		if r.Err != nil {
			be.ErrorCode = string(aerr.CodeOf(r.Err))
			be.ErrorMessage = r.Err.Error()
		} else {
			be.ObjectIds = r.Result
		}
		out[i] = be
	}
	return &rpc.EnumerateBatchResponse{Results: out}, nil
}

func (h *twirpHandler) Explain(ctx context.Context, req *rpc.CheckRequest) (*rpc.ExplainResponse, error) {
	tr, err := h.svc.Explain(ctx, service.Query{
		Account: req.Account, Principal: req.Principal, Action: req.Action, Object: req.Object,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	js, err := json.Marshal(tr)
	if err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling trace", err))
	}
	return &rpc.ExplainResponse{TraceJson: string(js)}, nil
}

func (h *twirpHandler) ExplainBatch(ctx context.Context, req *rpc.CheckBatchRequest) (*rpc.ExplainBatchResponse, error) {
	qs := make([]service.Query, len(req.Queries))
	for i, q := range req.Queries {
		qs[i] = service.Query{Account: q.Account, Principal: q.Principal, Action: q.Action, Object: q.Object}
	}
	results := h.svc.ExplainBatch(ctx, qs)
	out := make([]*rpc.BatchTrace, len(results))
	for i, r := range results {
		bt := &rpc.BatchTrace{}
		if r.Err != nil {
			bt.ErrorCode = string(aerr.CodeOf(r.Err))
			bt.ErrorMessage = r.Err.Error()
		} else if js, err := json.Marshal(r.Result); err != nil {
			bt.ErrorCode = string(aerr.APERTURE_STORAGE)
			bt.ErrorMessage = err.Error()
		} else {
			bt.TraceJson = string(js)
		}
		out[i] = bt
	}
	return &rpc.ExplainBatchResponse{Results: out}, nil
}

// ---- Entity write/read helpers ----

func decodeEntity(req *rpc.EntityRequest, v any) error {
	if req == nil {
		return aerr.New(aerr.APERTURE_INVALID_INPUT, "twirp: empty request")
	}
	if err := json.Unmarshal([]byte(req.EntityJson), v); err != nil {
		return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: entity_json is not valid JSON", err)
	}
	return nil
}

func entityResponse(v any) (*rpc.EntityResponse, error) {
	js, err := json.Marshal(v)
	if err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling entity", err))
	}
	return &rpc.EntityResponse{EntityJson: string(js)}, nil
}

func entityListResponse[T any](items []T) (*rpc.EntityListResponse, error) {
	out := make([]string, len(items))
	for i := range items {
		js, err := json.Marshal(items[i])
		if err != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling entity", err))
		}
		out[i] = string(js)
	}
	return &rpc.EntityListResponse{EntitiesJson: out}, nil
}

// ---- ObjectType ----

func (h *twirpHandler) PutObjectType(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var ot model.ObjectType
	if err := decodeEntity(req, &ot); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutObjectType(ctx, actor, ot))
}

func (h *twirpHandler) GetObjectType(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	ot, err := h.svc.GetObjectType(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(ot)
}

func (h *twirpHandler) ListObjectTypes(ctx context.Context, _ *rpc.Empty) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListObjectTypes(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeleteObjectType(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteObjectType(ctx, actor, req.Id))
}

// ---- Permission ----

func (h *twirpHandler) PutPermission(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var p model.Permission
	if err := decodeEntity(req, &p); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutPermission(ctx, actor, p))
}

func (h *twirpHandler) GetPermission(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	p, err := h.svc.GetPermission(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(p)
}

func (h *twirpHandler) ListPermissions(ctx context.Context, _ *rpc.Empty) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListPermissions(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeletePermission(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeletePermission(ctx, actor, req.Id))
}

// ---- Principal ----

func (h *twirpHandler) PutPrincipal(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var p model.Principal
	if err := decodeEntity(req, &p); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutPrincipal(ctx, actor, p))
}

func (h *twirpHandler) GetPrincipal(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	p, err := h.svc.GetPrincipal(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(p)
}

func (h *twirpHandler) ListPrincipals(ctx context.Context, _ *rpc.Empty) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListPrincipals(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeletePrincipal(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeletePrincipal(ctx, actor, req.Id))
}

// ---- Role ----

func (h *twirpHandler) PutRole(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var r model.Role
	if err := decodeEntity(req, &r); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutRole(ctx, actor, r))
}

func (h *twirpHandler) GetRole(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	r, err := h.svc.GetRole(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(r)
}

func (h *twirpHandler) ListRoles(ctx context.Context, _ *rpc.Empty) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListRoles(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeleteRole(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteRole(ctx, actor, req.Id))
}

// ---- Group ----

func (h *twirpHandler) PutGroup(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var g model.Group
	if err := decodeEntity(req, &g); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutGroup(ctx, actor, g))
}

func (h *twirpHandler) GetGroup(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	g, err := h.svc.GetGroup(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(g)
}

func (h *twirpHandler) ListGroups(ctx context.Context, _ *rpc.Empty) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListGroups(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeleteGroup(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteGroup(ctx, actor, req.Id))
}

// ---- Account ----

func (h *twirpHandler) PutAccount(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var a model.Account
	if err := decodeEntity(req, &a); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutAccount(ctx, actor, a))
}

func (h *twirpHandler) GetAccount(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	a, err := h.svc.GetAccount(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(a)
}

func (h *twirpHandler) ListAccounts(ctx context.Context, _ *rpc.Empty) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListAccounts(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeleteAccount(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteAccount(ctx, actor, req.Id))
}

// ---- Membership ----

func (h *twirpHandler) PutMembership(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var m model.Membership
	if err := decodeEntity(req, &m); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutMembership(ctx, actor, m))
}

func (h *twirpHandler) DeleteMembership(ctx context.Context, req *rpc.MembershipKeyRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteMembership(ctx, actor, req.PrincipalId, req.AccountId))
}

// ---- Grant ----

func (h *twirpHandler) PutGrant(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var g model.Grant
	if err := decodeEntity(req, &g); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutGrant(ctx, actor, g))
}

func (h *twirpHandler) GetGrant(ctx context.Context, req *rpc.GetRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	g, err := h.svc.GetGrant(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(g)
}

func (h *twirpHandler) ListGrants(ctx context.Context, req *rpc.ListGrantsRequest) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListGrants(ctx, req.AccountId)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items)
}

func (h *twirpHandler) DeleteGrant(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteGrant(ctx, actor, req.Id))
}

// ---- Delegation (actor = the authenticated delegator) ----

func (h *twirpHandler) Bestow(ctx context.Context, req *rpc.BestowRequest) (*rpc.Empty, error) {
	delegator, err := h.principal(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	var g model.Grant
	if err := json.Unmarshal([]byte(req.GrantJson), &g); err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: grant_json is not valid JSON", err))
	}
	return empty(h.svc.Bestow(ctx, delegator, g))
}

func (h *twirpHandler) Revoke(ctx context.Context, req *rpc.RevokeRequest) (*rpc.Empty, error) {
	delegator, err := h.principal(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.Revoke(ctx, delegator, req.GrantId))
}

// ---- Impersonation (operator = the authenticated principal) ----

func (h *twirpHandler) ImpersonationStart(ctx context.Context, req *rpc.ImpersonationStartRequest) (*rpc.ImpersonationSession, error) {
	operator, err := h.principal(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	sess, err := h.svc.ImpersonationStart(ctx, operator, req.Target, req.Account, engine.Mode(req.Mode))
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.ImpersonationSession{
		RealActor: sess.RealActor,
		Subject:   sess.Subject,
		Account:   sess.Account,
		Mode:      string(sess.Mode),
		StartedAt: sess.StartedAt.UTC().Format(timeFormat),
		ExpiresAt: sess.ExpiresAt.UTC().Format(timeFormat),
	}, nil
}

func (h *twirpHandler) ImpersonationStop(ctx context.Context, req *rpc.ImpersonationStopRequest) (*rpc.Empty, error) {
	operator, err := h.principal(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.ImpersonationStop(ctx, operator, nil))
}

// empty adapts a mutation's error-only return to the (*Empty, error) RPC shape.
func empty(err error) (*rpc.Empty, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.Empty{}, nil
}

// mapErr maps an Aperture coded error onto a twirp.Error, attaching the
// canonical code as meta["code"] so clients can dispatch without parsing the
// message. A nil error passes through; an already-twirp error is returned as-is.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if te, ok := err.(twirp.Error); ok {
		return te
	}
	code := aerr.CodeOf(err)
	te := twirp.NewError(codeToTwirp(code), err.Error())
	if code != "" {
		te = te.WithMeta("code", string(code))
	}
	return te
}

// codeToTwirp maps an Aperture code to a Twirp error code (and thus an HTTP
// status): InvalidArgument→400, Unauthenticated→401, PermissionDenied→403,
// NotFound→404, Unimplemented→501, Internal→500.
func codeToTwirp(code aerr.Code) twirp.ErrorCode {
	switch code {
	case aerr.APERTURE_INVALID_INPUT, aerr.APERTURE_IDENTITY_INVALID,
		aerr.APERTURE_ACTION_UNDECLARED, aerr.APERTURE_SCOPE_INVALID,
		aerr.APERTURE_SCOPE_UNKNOWN_STRATEGY, aerr.APERTURE_RULE_INVALID,
		aerr.APERTURE_RULE_UNKNOWN_VARIABLE, aerr.APERTURE_RULE_TYPE_ERROR,
		aerr.APERTURE_PROVIDER_INVALID:
		return twirp.InvalidArgument
	case aerr.APERTURE_NOT_FOUND, aerr.APERTURE_RULE_NOT_FOUND,
		aerr.APERTURE_PROVIDER_UNREGISTERED:
		return twirp.NotFound
	case aerr.APERTURE_UNAUTHENTICATED, aerr.APERTURE_INVALID_TOKEN:
		return twirp.Unauthenticated
	case aerr.APERTURE_AUTHZ_DENIED, aerr.APERTURE_DELEGATION_DENIED,
		aerr.APERTURE_DELEGATION_NOT_DELEGATABLE, aerr.APERTURE_IMPERSONATION_DENIED,
		aerr.APERTURE_IMPERSONATION_EXPIRED:
		return twirp.PermissionDenied
	case aerr.APERTURE_UNIMPLEMENTED:
		return twirp.Unimplemented
	default:
		return twirp.Internal
	}
}

// timeFormat is the RFC3339 form impersonation session timestamps render as on
// the wire.
const timeFormat = "2006-01-02T15:04:05Z07:00"
