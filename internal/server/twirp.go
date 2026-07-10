package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/frankbardon/aperture/auth"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/filter"
	"github.com/frankbardon/aperture/internal/wire/rpc"
	"github.com/frankbardon/aperture/model"
	"github.com/frankbardon/aperture/rules"
	"github.com/frankbardon/aperture/seed"
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

// readActor builds the actor an account-scoped READ authorizes against: the
// authenticated principal from context. Reads carry no wire account, so the
// gate resolves system-admin via the platform ("*") grant and account-admin
// against the target account named by the read itself.
func (h *twirpHandler) readActor(ctx context.Context) (service.Actor, error) {
	id, err := h.principal(ctx)
	if err != nil {
		return service.Actor{}, err
	}
	return service.Actor{Principal: id}, nil
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

// entityListResponse marshals a slice of entities to canonical JSON, then applies
// the (optional) server-side filter so filtered-out rows never reach the client.
// A nil/empty spec returns everything, preserving the pre-filter behaviour.
func entityListResponse[T any](items []T, spec filter.Spec) (*rpc.EntityListResponse, error) {
	out := make([]string, len(items))
	for i := range items {
		js, err := json.Marshal(items[i])
		if err != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling entity", err))
		}
		out[i] = string(js)
	}
	out = filter.Apply(out, spec)
	return &rpc.EntityListResponse{EntitiesJson: out}, nil
}

// filterSpec adapts the wire Filter message into the domain filter.Spec. A nil
// filter (older clients / unfiltered calls) yields the zero spec (match-all).
func filterSpec(f *rpc.Filter) filter.Spec {
	if f == nil {
		return filter.Spec{}
	}
	preds := make([]filter.Predicate, 0, len(f.Predicates))
	for _, p := range f.Predicates {
		preds = append(preds, filter.Predicate{Field: p.Field, Op: p.Op, Value: p.Value})
	}
	return filter.Spec{Predicates: preds, MatchAny: strings.EqualFold(f.Match, "any")}
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

func (h *twirpHandler) ListObjectTypes(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListObjectTypes(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
}

// ObjectIdentifiers enumerates a type's instance ids from its provider. It
// returns the complete set (optionally minus req.Exclude) — an admin/config read
// over all objects, so it requires auth like the entity reads, not the open
// decision path.
func (h *twirpHandler) ObjectIdentifiers(ctx context.Context, req *rpc.ObjectIdentifiersRequest) (*rpc.ObjectIdentifiersResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	ids, err := h.svc.ObjectIdentifiers(ctx, req.ObjectType, req.Exclude...)
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.ObjectIdentifiersResponse{ObjectIds: ids}, nil
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

func (h *twirpHandler) ListPermissions(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListPermissions(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
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

func (h *twirpHandler) ListPrincipals(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	actor, err := h.readActor(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListPrincipals(ctx, actor)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
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

func (h *twirpHandler) ListRoles(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListRoles(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
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

func (h *twirpHandler) ListGroups(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListGroups(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
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

func (h *twirpHandler) ListAccounts(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	actor, err := h.readActor(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListAccounts(ctx, actor)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
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
	actor, err := h.readActor(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	g, err := h.svc.GetGrant(ctx, actor, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(g)
}

// ListGrants lists grants for a scope, paginated. An empty account_id is the
// "all accounts" sentinel (system-admin only); a non-empty account_id lists that
// single account (backward-compatible). The service owns the auth gate and page
// validation; offset/limit are threaded through and echoed back alongside the
// total match count so the client can render prev/next.
func (h *twirpHandler) ListGrants(ctx context.Context, req *rpc.ListGrantsRequest) (*rpc.ListGrantsResponse, error) {
	actor, err := h.readActor(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	offset, limit := int(req.GetOffset()), int(req.GetLimit())
	items, total, err := h.svc.ListGrantsPage(ctx, actor, req.GetAccountId(), offset, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]string, len(items))
	for i := range items {
		js, err := json.Marshal(items[i])
		if err != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling entity", err))
		}
		out[i] = string(js)
	}
	out = filter.Apply(out, filterSpec(req.GetFilter()))
	return &rpc.ListGrantsResponse{
		EntitiesJson: out,
		Total:        int32(total),
		Offset:       req.GetOffset(),
		Limit:        req.GetLimit(),
	}, nil
}

func (h *twirpHandler) DeleteGrant(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteGrant(ctx, actor, req.Id))
}

// ---- Template (definition = system-admin; apply = account-admin) ----

func (h *twirpHandler) PutTemplate(ctx context.Context, req *rpc.EntityRequest) (*rpc.Empty, error) {
	var t model.Template
	if err := decodeEntity(req, &t); err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutTemplate(ctx, actor, t))
}

func (h *twirpHandler) GetTemplate(ctx context.Context, req *rpc.TemplateKeyRequest) (*rpc.EntityResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	t, err := h.svc.GetTemplate(ctx, req.Name, int(req.Version))
	if err != nil {
		return nil, mapErr(err)
	}
	return entityResponse(t)
}

func (h *twirpHandler) ListTemplates(ctx context.Context, req *rpc.ListRequest) (*rpc.EntityListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListTemplates(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(items, filterSpec(req.GetFilter()))
}

func (h *twirpHandler) DeleteTemplate(ctx context.Context, req *rpc.TemplateKeyRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteTemplate(ctx, actor, req.Name, int(req.Version)))
}

func (h *twirpHandler) ApplyTemplate(ctx context.Context, req *rpc.ApplyTemplateRequest) (*rpc.EntityListResponse, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	applied, err := h.svc.ApplyTemplate(ctx, actor, model.TemplateApplication{
		Name:          req.Name,
		Version:       int(req.Version),
		Account:       req.Account,
		Params:        req.Params,
		GrantIDPrefix: req.GrantIdPrefix,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return entityListResponse(applied, filter.Spec{})
}

// ---- Bulk grant / revoke (account-tier, transactional) ----

func (h *twirpHandler) BulkPutGrants(ctx context.Context, req *rpc.BulkGrantsRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	grants := make([]model.Grant, len(req.GrantsJson))
	for i, js := range req.GrantsJson {
		if err := json.Unmarshal([]byte(js), &grants[i]); err != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: grants_json is not valid JSON", err))
		}
	}
	return empty(h.svc.BulkPutGrants(ctx, actor, grants))
}

func (h *twirpHandler) BulkDeleteGrants(ctx context.Context, req *rpc.BulkDeleteGrantsRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.BulkDeleteGrants(ctx, actor, req.GrantIds))
}

// ---- Declarative state: export / import (system-admin tier) ----

func (h *twirpHandler) Export(ctx context.Context, req *rpc.ExportRequest) (*rpc.ExportResponse, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	doc, err := h.svc.Export(ctx, actor)
	if err != nil {
		return nil, mapErr(err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling state document", err))
	}
	return &rpc.ExportResponse{DocumentJson: string(js)}, nil
}

func (h *twirpHandler) Import(ctx context.Context, req *rpc.ImportRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	doc, err := seed.Parse([]byte(req.DocumentJson), seed.FormatJSON)
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.Import(ctx, actor, doc))
}

// ---- Audit query (system- or account-admin gated read) ----

func (h *twirpHandler) QueryAudit(ctx context.Context, req *rpc.QueryAuditRequest) (*rpc.QueryAuditResponse, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	filter := model.AuditFilter{
		Actor:     req.FilterActor,
		Account:   req.Account,
		EventType: model.AuditEventType(req.EventType),
		Outcome:   model.AuditOutcome(req.Outcome),
		Limit:     int(req.Limit),
	}
	if req.Since != "" {
		ts, perr := time.Parse(timeFormat, req.Since)
		if perr != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: since is not an RFC3339 timestamp", perr))
		}
		filter.Since = ts
	}
	if req.Until != "" {
		ts, perr := time.Parse(timeFormat, req.Until)
		if perr != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: until is not an RFC3339 timestamp", perr))
		}
		filter.Until = ts
	}
	events, err := h.svc.QueryAudit(ctx, actor, filter)
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.QueryAuditResponse{EventsJson: mustMarshalEach(events)}, nil
}

// mustMarshalEach marshals each event to canonical JSON. AuditEvent is a plain
// value shape with no marshal-failure modes, so an error here is impossible; the
// helper keeps the RPC body terse.
func mustMarshalEach(events []model.AuditEvent) []string {
	out := make([]string, len(events))
	for i := range events {
		js, _ := json.Marshal(events[i])
		out[i] = string(js)
	}
	return out
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

// ---- Rules (writes require system-admin; reads require auth) ----

func (h *twirpHandler) PutRule(ctx context.Context, req *rpc.RuleRequest) (*rpc.Empty, error) {
	r, err := decodeRule(req)
	if err != nil {
		return nil, mapErr(err)
	}
	actor, err := h.actor(ctx, ruleActorAccount(req))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.PutRule(ctx, actor, r))
}

func (h *twirpHandler) GetRule(ctx context.Context, req *rpc.GetRequest) (*rpc.RuleResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	r, err := h.svc.GetRule(ctx, req.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	js, err := json.Marshal(r)
	if err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling rule", err))
	}
	return &rpc.RuleResponse{RuleJson: string(js)}, nil
}

func (h *twirpHandler) ListRules(ctx context.Context, _ *rpc.Empty) (*rpc.RuleListResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	items, err := h.svc.ListRules(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]string, len(items))
	for i := range items {
		js, mErr := json.Marshal(items[i])
		if mErr != nil {
			return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling rule", mErr))
		}
		out[i] = string(js)
	}
	return &rpc.RuleListResponse{RulesJson: out}, nil
}

func (h *twirpHandler) DeleteRule(ctx context.Context, req *rpc.DeleteRequest) (*rpc.Empty, error) {
	actor, err := h.actor(ctx, actorAccount(req.Actor))
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.DeleteRule(ctx, actor, req.Id))
}

// ValidateRule compiles/validates a rule AST without persisting it. It requires
// an authenticated principal (the editor is an admin tool) but no admin tier — it
// writes nothing. A valid rule returns Empty; an invalid one surfaces its
// APERTURE_RULE_* code for the canvas.
func (h *twirpHandler) ValidateRule(ctx context.Context, req *rpc.RuleRequest) (*rpc.Empty, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	r, err := decodeRule(req)
	if err != nil {
		return nil, mapErr(err)
	}
	return empty(h.svc.ValidateRule(ctx, r))
}

// decodeRule unmarshals a RuleRequest's rule_json into a model.Rule.
func decodeRule(req *rpc.RuleRequest) (model.Rule, error) {
	if req == nil {
		return model.Rule{}, aerr.New(aerr.APERTURE_INVALID_INPUT, "twirp: empty request")
	}
	var r model.Rule
	if err := json.Unmarshal([]byte(req.RuleJson), &r); err != nil {
		return model.Rule{}, aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: rule_json is not valid JSON", err)
	}
	return r, nil
}

func ruleActorAccount(req *rpc.RuleRequest) string {
	if req == nil {
		return ""
	}
	return actorAccount(req.Actor)
}

// ---- Read-only what-if over a hypothetical overlay (E7-S3 live preview) ----

func (h *twirpHandler) Simulate(ctx context.Context, req *rpc.SimulateRequest) (*rpc.Decision, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	ov, q, err := decodeSimulate(req)
	if err != nil {
		return nil, mapErr(err)
	}
	res, err := h.svc.Simulate(ctx, ov, q)
	if err != nil {
		return nil, mapErr(err)
	}
	return &rpc.Decision{Allow: res.Allow, Reason: res.Reason, DecidingGrantIds: res.DecidingGrantIDs}, nil
}

func (h *twirpHandler) SimulateExplain(ctx context.Context, req *rpc.SimulateRequest) (*rpc.ExplainResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	ov, q, err := decodeSimulate(req)
	if err != nil {
		return nil, mapErr(err)
	}
	tr, err := h.svc.SimulateExplain(ctx, ov, q)
	if err != nil {
		return nil, mapErr(err)
	}
	js, err := json.Marshal(tr)
	if err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_STORAGE, "twirp: marshalling trace", err))
	}
	return &rpc.ExplainResponse{TraceJson: string(js)}, nil
}

// EvaluateRule runs an unsaved rule AST against one object's provider metadata
// and returns the boolean result — the rule builder's object-based what-if. It
// requires an authenticated principal (like the entity reads); no account,
// principal, or grant is consulted.
func (h *twirpHandler) EvaluateRule(ctx context.Context, req *rpc.EvaluateRuleRequest) (*rpc.EvaluateRuleResponse, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mapErr(err)
	}
	var r model.Rule
	if err := json.Unmarshal([]byte(req.GetRuleJson()), &r); err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: rule_json is not valid JSON", err))
	}
	var node rules.Node
	if err := json.Unmarshal(r.AST, &node); err != nil {
		return nil, mapErr(aerr.Wrap(aerr.APERTURE_RULE_INVALID, "twirp: rule AST is not valid JSON", err))
	}
	result, md, err := h.svc.EvaluateRule(ctx, &node, req.GetObjectId())
	if err != nil {
		return nil, mapErr(err)
	}
	var objectJSON string
	if md != nil {
		if b, mErr := json.Marshal(md); mErr == nil {
			objectJSON = string(b)
		}
	}
	return &rpc.EvaluateRuleResponse{Result: result, ObjectJson: objectJSON}, nil
}

// decodeSimulate builds the read-only Overlay and Query a Simulate call layers
// over the live model. Every overlay entity rides as canonical JSON; a malformed
// item is an input error (nothing is written regardless).
func decodeSimulate(req *rpc.SimulateRequest) (service.Overlay, service.Query, error) {
	if req == nil || req.Query == nil {
		return service.Overlay{}, service.Query{}, aerr.New(aerr.APERTURE_INVALID_INPUT, "twirp: simulate request needs a query")
	}
	var ov service.Overlay
	if err := unmarshalEach(req.RulesJson, "rule", &ov.Rules); err != nil {
		return ov, service.Query{}, err
	}
	if err := unmarshalEach(req.GrantsJson, "grant", &ov.Grants); err != nil {
		return ov, service.Query{}, err
	}
	if err := unmarshalEach(req.PermissionsJson, "permission", &ov.Permissions); err != nil {
		return ov, service.Query{}, err
	}
	if err := unmarshalEach(req.PrincipalsJson, "principal", &ov.Principals); err != nil {
		return ov, service.Query{}, err
	}
	q := service.Query{
		Account:   req.Query.Account,
		Principal: req.Query.Principal,
		Action:    req.Query.Action,
		Object:    req.Query.Object,
	}
	return ov, q, nil
}

// unmarshalEach decodes each JSON string in src into a fresh T appended to *dst.
func unmarshalEach[T any](src []string, kind string, dst *[]T) error {
	for _, s := range src {
		var v T
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return aerr.Wrap(aerr.APERTURE_INVALID_INPUT, "twirp: overlay "+kind+" is not valid JSON", err)
		}
		*dst = append(*dst, v)
	}
	return nil
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
		aerr.APERTURE_PROVIDER_INVALID, aerr.APERTURE_TEMPLATE_INVALID,
		aerr.APERTURE_TEMPLATE_PARAM:
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
