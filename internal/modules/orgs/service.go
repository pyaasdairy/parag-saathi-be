package orgs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
)

// maxTreeNodes caps a subtree response (root included) so a federation-level
// tree read can never return an unbounded list.
const maxTreeNodes = 500

// service holds all business logic of the orgs module.
type service struct {
	repo  *repository
	scope *orgscope.Resolver
	log   *slog.Logger
}

func newService(repo *repository, scope *orgscope.Resolver, log *slog.Logger) *service {
	return &service{repo: repo, scope: scope, log: log}
}

// fail routes an error out of the service: expected AppErrors (404s from the
// repo, platform errors) pass through untouched; anything unexpected — or an
// AppError already carrying a 5xx — is logged at ERROR with the failing
// operation before the client sees an opaque 500.
func (s *service) fail(ctx context.Context, op string, err error) error {
	var appErr *httpx.AppError
	ok := errors.As(err, &appErr)
	if !ok || appErr.Status >= http.StatusInternalServerError {
		s.log.ErrorContext(ctx, "orgs operation failed",
			slog.String("op", op),
			slog.Any("err", err))
	}
	if ok {
		return err
	}
	return httpx.Internal(err)
}

// scopeErr handles a RequireInScope failure: scope denials (403) are logged
// at WARN with the actor and target; other errors go through fail.
func (s *service) scopeErr(ctx context.Context, op string, actor auth.Actor, targetOrgID primitive.ObjectID, err error) error {
	var appErr *httpx.AppError
	if errors.As(err, &appErr) && appErr.Status == http.StatusForbidden {
		s.log.WarnContext(ctx, "org scope denial",
			slog.String("op", op),
			slog.String("target_org_id", targetOrgID.Hex()),
			slog.String("actor_party_id", actor.PartyID),
			slog.String("actor_org_id", actor.OrgUnitID),
			slog.String("reason", appErr.Message))
		return err
	}
	return s.fail(ctx, op, err)
}

// validateHierarchy checks whether an org unit of childType may be created
// under a parent of parentType ("" = no parent), per domain.ValidOrgParent.
// Pure function — unit-tested against the full type matrix.
func validateHierarchy(childType, parentType string) *httpx.AppError {
	if !domain.IsValidOrgType(childType) {
		return httpx.BadRequest("INVALID_ORG_TYPE", "unknown org type "+childType)
	}
	allowed := domain.ValidOrgParent[childType]
	if len(allowed) == 0 { // hierarchy root
		if parentType != "" {
			return httpx.Unprocessable("ORG_PARENT_TYPE_INVALID",
				childType+" is the hierarchy root and cannot have a parent")
		}
		return nil
	}
	if parentType == "" {
		return httpx.BadRequest("ORG_PARENT_REQUIRED", childType+" requires a parent_id")
	}
	for _, t := range allowed {
		if t == parentType {
			return nil
		}
	}
	return httpx.Unprocessable("ORG_PARENT_TYPE_INVALID",
		childType+" cannot be created under a "+parentType)
}

// Create validates the hierarchy edge and inserts a new org unit
// (POST /orgs, blueprint §5.1).
func (s *service) Create(ctx context.Context, actor auth.Actor, req CreateOrgRequest) (*domain.OrgUnit, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, httpx.BadRequest("INVALID_NAME", "name is required")
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return nil, httpx.BadRequest("INVALID_CODE", "code is required")
	}

	// Resolve the parent (if any) and validate the hierarchy edge.
	var parent *domain.OrgUnit
	if req.ParentID != nil && !req.ParentID.IsZero() {
		var err error
		parent, err = s.repo.getOrg(ctx, *req.ParentID)
		if err != nil {
			return nil, s.fail(ctx, "get parent org unit", err)
		}
	}
	parentType := ""
	if parent != nil {
		parentType = parent.Type
	}
	if aerr := validateHierarchy(req.Type, parentType); aerr != nil {
		s.log.WarnContext(ctx, "org create rejected: invalid hierarchy edge",
			slog.String("org_code", code),
			slog.String("org_type", req.Type),
			slog.String("parent_type", parentType),
			slog.String("reason", aerr.Code),
			slog.String("actor_party_id", actor.PartyID))
		return nil, aerr
	}

	// The federation is the unique root: reject a second one.
	if req.Type == domain.OrgTypeFederation {
		n, err := s.repo.countByType(ctx, domain.OrgTypeFederation)
		if err != nil {
			return nil, s.fail(ctx, "count federation org units", err)
		}
		if n > 0 {
			s.log.WarnContext(ctx, "org create rejected: federation root already exists",
				slog.String("org_code", code),
				slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("FEDERATION_EXISTS", "a federation root org unit already exists")
		}
	}

	// The new unit must be created inside the caller's scope.
	if parent != nil {
		if err := s.scope.RequireInScope(ctx, actor, parent.ID); err != nil {
			return nil, s.scopeErr(ctx, "create org unit", actor, parent.ID, err)
		}
	}

	now := time.Now().UTC()
	org := &domain.OrgUnit{
		// Pre-generate the id so insert + cache invalidation see one value.
		ID:        primitive.NewObjectID(),
		Type:      req.Type,
		Name:      name,
		NameHi:    strings.TrimSpace(req.NameHi),
		Code:      code,
		Village:   strings.TrimSpace(req.Village),
		District:  strings.TrimSpace(req.District),
		State:     strings.TrimSpace(req.State),
		Path:      []primitive.ObjectID{},
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if parent != nil {
		parentID := parent.ID
		org.ParentID = &parentID
		org.Path = append(append([]primitive.ObjectID{}, parent.Path...), parent.ID)
	}
	if req.GeoLat != nil {
		org.GeoLat = *req.GeoLat
	}
	if req.GeoLng != nil {
		org.GeoLng = *req.GeoLng
	}

	if err := s.repo.insertOrg(ctx, org); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			s.log.WarnContext(ctx, "org create rejected: duplicate code",
				slog.String("org_code", code),
				slog.String("actor_party_id", actor.PartyID))
			return nil, httpx.Conflict("ORG_CODE_EXISTS", "an org unit with code "+code+" already exists")
		}
		return nil, s.fail(ctx, "insert org unit", err)
	}
	s.scope.Invalidate(org.ID)
	s.log.InfoContext(ctx, "org unit created",
		slog.String("org_id", org.ID.Hex()),
		slog.String("org_code", org.Code),
		slog.String("org_type", org.Type),
		slog.String("parent_org_id", hexOrEmpty(org.ParentID)),
		slog.String("actor_party_id", actor.PartyID))
	return org, nil
}

// hexOrEmpty renders an optional ObjectID reference for logging.
func hexOrEmpty(id *primitive.ObjectID) string {
	if id == nil {
		return ""
	}
	return id.Hex()
}

// Update patches name/district/active/geo on an org unit (PATCH /orgs/{id}).
// Type or parent moves are rejected — restructuring the tree is out of scope
// for v1 because Path denormalisation across a subtree is not transactional.
func (s *service) Update(ctx context.Context, actor auth.Actor, id primitive.ObjectID, req UpdateOrgRequest) (*domain.OrgUnit, error) {
	if req.Type != nil || req.ParentID != nil {
		s.log.WarnContext(ctx, "org update rejected: hierarchy move unsupported",
			slog.String("org_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("ORG_MOVE_UNSUPPORTED",
			"changing an org unit's type or parent is not supported in v1")
	}
	if err := s.scope.RequireInScope(ctx, actor, id); err != nil {
		return nil, s.scopeErr(ctx, "update org unit", actor, id, err)
	}

	set := bson.D{{Key: "updated_at", Value: time.Now().UTC()}}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, httpx.BadRequest("INVALID_NAME", "name cannot be empty")
		}
		set = append(set, bson.E{Key: "name", Value: name})
	}
	if req.NameHi != nil {
		set = append(set, bson.E{Key: "name_hi", Value: strings.TrimSpace(*req.NameHi)})
	}
	if req.Village != nil {
		set = append(set, bson.E{Key: "village", Value: strings.TrimSpace(*req.Village)})
	}
	if req.District != nil {
		set = append(set, bson.E{Key: "district", Value: strings.TrimSpace(*req.District)})
	}
	if req.Active != nil {
		set = append(set, bson.E{Key: "active", Value: *req.Active})
	}
	if req.GeoLat != nil {
		set = append(set, bson.E{Key: "geo_lat", Value: *req.GeoLat})
	}
	if req.GeoLng != nil {
		set = append(set, bson.E{Key: "geo_lng", Value: *req.GeoLng})
	}
	if len(set) == 1 { // only updated_at
		return nil, httpx.BadRequest("EMPTY_UPDATE", "no updatable fields provided")
	}

	org, err := s.repo.updateOrg(ctx, id, set)
	if err != nil {
		return nil, s.fail(ctx, "update org unit", err)
	}
	s.scope.Invalidate(id)
	s.log.InfoContext(ctx, "org unit updated",
		slog.String("org_id", org.ID.Hex()),
		slog.String("org_code", org.Code),
		slog.Bool("active", org.Active),
		slog.Int("fields_patched", len(set)-1),
		slog.String("actor_party_id", actor.PartyID))
	return org, nil
}

// Get returns one org unit (GET /orgs/{id}).
func (s *service) Get(ctx context.Context, id primitive.ObjectID) (*domain.OrgUnit, error) {
	org, err := s.repo.getOrg(ctx, id)
	if err != nil {
		return nil, s.fail(ctx, "get org unit", err)
	}
	return org, nil
}

// Children pages the direct children of an org unit (GET /orgs/{id}/children),
// returning the total child count alongside the page.
func (s *service) Children(ctx context.Context, id primitive.ObjectID, page httpx.Page) ([]domain.OrgUnit, int64, error) {
	if _, err := s.repo.getOrg(ctx, id); err != nil {
		return nil, 0, s.fail(ctx, "get org unit", err)
	}
	units, total, err := s.repo.listChildren(ctx, id, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list org children", err)
	}
	return units, total, nil
}

// Tree returns the root plus its whole subtree as a flat list (each node
// carries parent_id; the client assembles), capped at maxTreeNodes.
func (s *service) Tree(ctx context.Context, actor auth.Actor, id primitive.ObjectID) ([]domain.OrgUnit, bool, error) {
	root, err := s.repo.getOrg(ctx, id)
	if err != nil {
		return nil, false, s.fail(ctx, "get org unit", err)
	}
	if err := s.scope.RequireInScope(ctx, actor, id); err != nil {
		return nil, false, s.scopeErr(ctx, "read org subtree", actor, id, err)
	}
	descendants, err := s.repo.listDescendants(ctx, id, maxTreeNodes-1)
	if err != nil {
		return nil, false, s.fail(ctx, "list org descendants", err)
	}
	nodes := append([]domain.OrgUnit{*root}, descendants...)
	truncated := len(descendants) == maxTreeNodes-1
	return nodes, truncated, nil
}

// List pages org units filtered by type, district and/or exact code — the
// unique business key, e.g. ?code=DCS-01842, used to discover an org's
// ObjectID (GET /orgs) — returning the total matching count alongside the page.
func (s *service) List(ctx context.Context, orgType, district, code string, page httpx.Page) ([]domain.OrgUnit, int64, error) {
	filter := bson.D{}
	if orgType != "" {
		if !domain.IsValidOrgType(orgType) {
			return nil, 0, httpx.BadRequest("INVALID_ORG_TYPE", "unknown org type "+orgType)
		}
		filter = append(filter, bson.E{Key: "type", Value: orgType})
	}
	if district != "" {
		filter = append(filter, bson.E{Key: "district", Value: district})
	}
	if code != "" {
		filter = append(filter, bson.E{Key: "code", Value: code})
	}
	units, total, err := s.repo.listOrgs(ctx, filter, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list org units", err)
	}
	return units, total, nil
}

// Members pages the ACTIVE role assignments at an org unit joined in memory
// with party identity fields (GET /orgs/{id}/members) — two indexed queries,
// no $lookup.
func (s *service) Members(ctx context.Context, actor auth.Actor, id primitive.ObjectID, page httpx.Page) ([]Member, int64, error) {
	if _, err := s.repo.getOrg(ctx, id); err != nil {
		return nil, 0, s.fail(ctx, "get org unit", err)
	}
	if err := s.scope.RequireInScope(ctx, actor, id); err != nil {
		return nil, 0, s.scopeErr(ctx, "read org members", actor, id, err)
	}

	total, err := s.repo.countActiveAssignments(ctx, id)
	if err != nil {
		return nil, 0, s.fail(ctx, "count active role assignments", err)
	}
	assignments, err := s.repo.listActiveAssignments(ctx, id, page)
	if err != nil {
		return nil, 0, s.fail(ctx, "list active role assignments", err)
	}
	partyIDs := make([]primitive.ObjectID, 0, len(assignments))
	seen := make(map[primitive.ObjectID]struct{}, len(assignments))
	for _, a := range assignments {
		if _, dup := seen[a.PartyID]; !dup {
			seen[a.PartyID] = struct{}{}
			partyIDs = append(partyIDs, a.PartyID)
		}
	}
	parties, err := s.repo.partiesByIDs(ctx, partyIDs)
	if err != nil {
		return nil, 0, s.fail(ctx, "fetch member parties", err)
	}

	members := make([]Member, 0, len(assignments))
	for _, a := range assignments {
		m := Member{
			PartyID:          a.PartyID,
			RoleCode:         a.RoleCode,
			RoleAssignmentID: a.ID,
			ValidFrom:        a.ValidFrom,
			ValidTo:          a.ValidTo,
		}
		if p, ok := parties[a.PartyID]; ok {
			m.Phone = p.Phone
			m.FullName = p.FullName
			m.KYCTier = p.KYCTier
		}
		members = append(members, m)
	}
	return members, total, nil
}
