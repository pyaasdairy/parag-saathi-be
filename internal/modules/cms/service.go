package cms

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/orgscope"
)

// deltaBatch caps a single delta pull so a cold client (since=0) can never
// pull an unbounded list in one request — it pages by advancing ?since=.
const deltaBatch = 200

// service holds all business logic of the CMS module.
type service struct {
	repo  *repository
	scope *orgscope.Resolver
	log   *slog.Logger
}

func newService(repo *repository, scope *orgscope.Resolver, log *slog.Logger) *service {
	return &service{repo: repo, scope: scope, log: log}
}

// fail routes an error out of the service: expected AppErrors pass through;
// anything unexpected — or an AppError already carrying a 5xx — is logged at
// ERROR with the failing operation before the client sees an opaque 500.
func (s *service) fail(ctx context.Context, op string, err error) error {
	var appErr *httpx.AppError
	ok := errors.As(err, &appErr)
	if !ok || appErr.Status >= http.StatusInternalServerError {
		s.log.ErrorContext(ctx, "cms operation failed",
			slog.String("op", op),
			slog.Any("err", err))
	}
	if ok {
		return err
	}
	return httpx.Internal(err)
}

// scopeErr handles a RequireInScope failure: scope denials (403) are logged at
// WARN with the actor and target; other errors go through fail.
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

// Delta returns the published content items whose version is greater than
// since (default 0), ordered by version ascending, optionally narrowed by
// content type and region scope (GET /content, blueprint §6.1). The returned
// max version is the cursor the client passes back as ?since= next time; when
// the batch fills, truncated is true and the client pulls again immediately.
func (s *service) Delta(ctx context.Context, contentType, scope string, since int64) ([]domain.CMSContent, DeltaMeta, error) {
	if since < 0 {
		since = 0
	}
	filter := bson.D{
		{Key: "published", Value: true},
		{Key: "version", Value: bson.D{{Key: "$gt", Value: since}}},
	}
	if contentType != "" {
		if !domain.IsValidCMSType(contentType) {
			return nil, DeltaMeta{}, httpx.BadRequest("INVALID_CONTENT_TYPE", "unknown content type "+contentType)
		}
		filter = append(filter, bson.E{Key: "type", Value: contentType})
	}
	if scope != "" {
		if !domain.IsValidCMSScope(scope) {
			return nil, DeltaMeta{}, httpx.BadRequest("INVALID_REGION_SCOPE", "unknown region scope "+scope)
		}
		filter = append(filter, bson.E{Key: "region_scope", Value: scope})
	}

	items, err := s.repo.listDelta(ctx, filter, deltaBatch)
	if err != nil {
		return nil, DeltaMeta{}, s.fail(ctx, "list cms delta", err)
	}
	// Items are version-ascending, so the last one carries the max version;
	// with nothing new the client's own cursor is unchanged.
	maxVersion := since
	if n := len(items); n > 0 {
		maxVersion = items[n-1].Version
	}
	return items, DeltaMeta{
		Count:      len(items),
		MaxVersion: maxVersion,
		Truncated:  int64(len(items)) == deltaBatch,
	}, nil
}

// Helpline returns the published helpline entries for the Get-Help screen
// (GET /content/helpline, blueprint §6.1). An explicit scope narrows to that
// tier plus the unconditional "all" entries; an empty scope returns every
// published helpline.
func (s *service) Helpline(ctx context.Context, scope string) ([]domain.CMSContent, error) {
	filter := bson.D{
		{Key: "type", Value: domain.CMSTypeHelpline},
		{Key: "published", Value: true},
	}
	if scope != "" {
		if !domain.IsValidCMSScope(scope) {
			return nil, httpx.BadRequest("INVALID_REGION_SCOPE", "unknown region scope "+scope)
		}
		if scope != domain.CMSScopeAll {
			filter = append(filter, bson.E{Key: "region_scope", Value: bson.D{
				{Key: "$in", Value: bson.A{scope, domain.CMSScopeAll}},
			}})
		}
	}
	items, err := s.repo.listHelpline(ctx, filter)
	if err != nil {
		return nil, s.fail(ctx, "list cms helpline", err)
	}
	return items, nil
}

// Create authors a new content item (POST /content). It assigns the next
// monotonic version so the field app's delta pull will carry it, and — when
// the item is pinned to an org unit via RegionRef — enforces that the author
// is acting within that org's scope.
func (s *service) Create(ctx context.Context, actor auth.Actor, req CreateContentRequest) (*domain.CMSContent, error) {
	if !domain.IsValidCMSType(req.Type) {
		return nil, httpx.BadRequest("INVALID_CONTENT_TYPE", "unknown content type "+req.Type)
	}
	if len(req.TitleI18n) == 0 {
		return nil, httpx.BadRequest("INVALID_TITLE", "title_i18n must have at least one language entry")
	}
	scopeVal := req.RegionScope
	if scopeVal == "" {
		scopeVal = domain.CMSScopeAll
	}
	if !domain.IsValidCMSScope(scopeVal) {
		return nil, httpx.BadRequest("INVALID_REGION_SCOPE", "unknown region scope "+scopeVal)
	}
	if req.Type == domain.CMSTypeHelpline && len(req.PhoneNumbers) == 0 {
		return nil, httpx.Unprocessable("HELPLINE_NUMBER_REQUIRED", "a helpline item requires at least one phone number")
	}
	if req.ValidFrom != nil && req.ValidTo != nil && req.ValidTo.Before(*req.ValidFrom) {
		return nil, httpx.Unprocessable("INVALID_VALIDITY_WINDOW", "valid_to must not precede valid_from")
	}

	// A region-pinned item must be authored inside the caller's scope.
	if req.RegionRef != nil && !req.RegionRef.IsZero() {
		if err := s.scope.RequireInScope(ctx, actor, *req.RegionRef); err != nil {
			return nil, s.scopeErr(ctx, "create cms content", actor, *req.RegionRef, err)
		}
	}

	version, err := s.repo.nextVersion(ctx)
	if err != nil {
		return nil, s.fail(ctx, "mint cms version", err)
	}

	now := time.Now().UTC()
	c := &domain.CMSContent{
		ID:              primitive.NewObjectID(),
		Type:            req.Type,
		TitleI18n:       req.TitleI18n,
		DescriptionI18n: req.DescriptionI18n,
		URL:             strings.TrimSpace(req.URL),
		ThumbnailURL:    strings.TrimSpace(req.ThumbnailURL),
		PhoneNumbers:    req.PhoneNumbers,
		Languages:       req.Languages,
		RegionScope:     scopeVal,
		RegionRef:       req.RegionRef,
		Category:        strings.TrimSpace(req.Category),
		Order:           req.Order,
		Published:       req.Published,
		Version:         version,
		ValidFrom:       req.ValidFrom,
		ValidTo:         req.ValidTo,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.repo.insert(ctx, c); err != nil {
		return nil, s.fail(ctx, "insert cms content", err)
	}
	s.log.InfoContext(ctx, "cms content created",
		slog.String("content_id", c.ID.Hex()),
		slog.String("type", c.Type),
		slog.String("region_scope", c.RegionScope),
		slog.Int64("version", c.Version),
		slog.Bool("published", c.Published),
		slog.String("actor_party_id", actor.PartyID))
	return c, nil
}

// Update patches a content item (PUT /content/{id}) and mints a fresh version
// so the change propagates through the delta pull. Type is immutable; a
// RegionRef change is scope-checked against the new target.
func (s *service) Update(ctx context.Context, actor auth.Actor, id primitive.ObjectID, req UpdateContentRequest) (*domain.CMSContent, error) {
	existing, err := s.repo.getByID(ctx, id)
	if err != nil {
		return nil, s.fail(ctx, "get cms content", err)
	}
	if req.Type != nil && *req.Type != existing.Type {
		s.log.WarnContext(ctx, "cms update rejected: type change unsupported",
			slog.String("content_id", id.Hex()),
			slog.String("actor_party_id", actor.PartyID))
		return nil, httpx.Unprocessable("CONTENT_TYPE_IMMUTABLE", "changing a content item's type is not supported")
	}

	// Scope-check the item's org pin: the target after this edit (if changed)
	// or the current one, so an author can only ever touch in-scope content.
	target := existing.RegionRef
	if req.RegionRef != nil {
		target = req.RegionRef
	}
	if target != nil && !target.IsZero() {
		if err := s.scope.RequireInScope(ctx, actor, *target); err != nil {
			return nil, s.scopeErr(ctx, "update cms content", actor, *target, err)
		}
	}

	set := bson.D{}
	if req.TitleI18n != nil {
		if len(*req.TitleI18n) == 0 {
			return nil, httpx.BadRequest("INVALID_TITLE", "title_i18n must have at least one language entry")
		}
		set = append(set, bson.E{Key: "title_i18n", Value: *req.TitleI18n})
	}
	if req.DescriptionI18n != nil {
		set = append(set, bson.E{Key: "description_i18n", Value: *req.DescriptionI18n})
	}
	if req.URL != nil {
		set = append(set, bson.E{Key: "url", Value: strings.TrimSpace(*req.URL)})
	}
	if req.ThumbnailURL != nil {
		set = append(set, bson.E{Key: "thumbnail_url", Value: strings.TrimSpace(*req.ThumbnailURL)})
	}
	if req.PhoneNumbers != nil {
		set = append(set, bson.E{Key: "phone_numbers", Value: *req.PhoneNumbers})
	}
	if req.Languages != nil {
		set = append(set, bson.E{Key: "languages", Value: *req.Languages})
	}
	if req.RegionScope != nil {
		if !domain.IsValidCMSScope(*req.RegionScope) {
			return nil, httpx.BadRequest("INVALID_REGION_SCOPE", "unknown region scope "+*req.RegionScope)
		}
		set = append(set, bson.E{Key: "region_scope", Value: *req.RegionScope})
	}
	if req.RegionRef != nil {
		set = append(set, bson.E{Key: "region_ref", Value: req.RegionRef})
	}
	if req.Category != nil {
		set = append(set, bson.E{Key: "category", Value: strings.TrimSpace(*req.Category)})
	}
	if req.Order != nil {
		set = append(set, bson.E{Key: "order", Value: *req.Order})
	}
	if req.Published != nil {
		set = append(set, bson.E{Key: "published", Value: *req.Published})
	}
	if req.ValidFrom != nil {
		set = append(set, bson.E{Key: "valid_from", Value: req.ValidFrom})
	}
	if req.ValidTo != nil {
		set = append(set, bson.E{Key: "valid_to", Value: req.ValidTo})
	}
	if len(set) == 0 {
		return nil, httpx.BadRequest("EMPTY_UPDATE", "no updatable fields provided")
	}

	// A helpline that ends up with no numbers is unusable — guard the
	// resulting state, not just the incoming patch.
	if existing.Type == domain.CMSTypeHelpline && req.PhoneNumbers != nil && len(*req.PhoneNumbers) == 0 {
		return nil, httpx.Unprocessable("HELPLINE_NUMBER_REQUIRED", "a helpline item requires at least one phone number")
	}

	version, err := s.repo.nextVersion(ctx)
	if err != nil {
		return nil, s.fail(ctx, "mint cms version", err)
	}
	set = append(set,
		bson.E{Key: "version", Value: version},
		bson.E{Key: "updated_at", Value: time.Now().UTC()})

	c, err := s.repo.update(ctx, id, set)
	if err != nil {
		return nil, s.fail(ctx, "update cms content", err)
	}
	s.log.InfoContext(ctx, "cms content updated",
		slog.String("content_id", c.ID.Hex()),
		slog.String("type", c.Type),
		slog.Int64("version", c.Version),
		slog.Bool("published", c.Published),
		slog.Int("fields_patched", len(set)-2),
		slog.String("actor_party_id", actor.PartyID))
	return c, nil
}
