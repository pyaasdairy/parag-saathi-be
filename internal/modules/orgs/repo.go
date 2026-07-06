package orgs

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/mongodb"
)

// repository is the module's only MongoDB access point. It reads the
// role_assignments and parties collections (allowed by spec for the members
// roster) but writes only org_units.
type repository struct {
	orgs        *mongo.Collection
	assignments *mongo.Collection
	parties     *mongo.Collection
}

func newRepository(db *mongo.Database) *repository {
	return &repository{
		orgs:        db.Collection(mongodb.CollOrgUnits),
		assignments: db.Collection(mongodb.CollRoleAssignments),
		parties:     db.Collection(mongodb.CollParties),
	}
}

// insertOrg inserts a new org unit. The raw driver error is returned so the
// service can map a duplicate-key violation on code to a Conflict.
func (r *repository) insertOrg(ctx context.Context, org *domain.OrgUnit) error {
	_, err := r.orgs.InsertOne(ctx, org)
	return err
}

// getOrg fetches one org unit by ID.
func (r *repository) getOrg(ctx context.Context, id string) (*domain.OrgUnit, error) {
	var org domain.OrgUnit
	err := r.orgs.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&org)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("org unit " + id)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &org, nil
}

// countByType counts org units of the given type (federation-uniqueness check).
func (r *repository) countByType(ctx context.Context, orgType string) (int64, error) {
	n, err := r.orgs.CountDocuments(ctx, bson.D{{Key: "type", Value: orgType}})
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return n, nil
}

// updateOrg applies a $set patch and returns the updated document.
func (r *repository) updateOrg(ctx context.Context, id string, set bson.D) (*domain.OrgUnit, error) {
	var org domain.OrgUnit
	err := r.orgs.FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&org)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("org unit " + id)
	}
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &org, nil
}

// listChildren pages the direct children of an org unit (parent_id index)
// and returns the total child count for pagination meta.
func (r *repository) listChildren(ctx context.Context, parentID string, page httpx.Page) ([]domain.OrgUnit, int64, error) {
	filter := bson.D{{Key: "parent_id", Value: parentID}}
	total, err := r.orgs.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	units, err := r.findOrgs(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "code", Value: 1}}).
			SetSkip(page.Offset).
			SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	return units, total, nil
}

// listOrgs pages org units matching an arbitrary filter (type/district) and
// returns the total matching count for pagination meta.
func (r *repository) listOrgs(ctx context.Context, filter bson.D, page httpx.Page) ([]domain.OrgUnit, int64, error) {
	total, err := r.orgs.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	units, err := r.findOrgs(ctx, filter,
		options.Find().
			SetSort(bson.D{{Key: "code", Value: 1}}).
			SetSkip(page.Offset).
			SetLimit(page.Limit))
	if err != nil {
		return nil, 0, err
	}
	return units, total, nil
}

// listDescendants returns every org unit whose ancestor Path contains rootID
// (path index, array containment), capped at limit nodes.
func (r *repository) listDescendants(ctx context.Context, rootID string, limit int64) ([]domain.OrgUnit, error) {
	return r.findOrgs(ctx, bson.D{{Key: "path", Value: rootID}},
		options.Find().
			SetSort(bson.D{{Key: "code", Value: 1}}).
			SetLimit(limit))
}

func (r *repository) findOrgs(ctx context.Context, filter bson.D, opts *options.FindOptions) ([]domain.OrgUnit, error) {
	cur, err := r.orgs.Find(ctx, filter, opts)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	units := []domain.OrgUnit{}
	if err := cur.All(ctx, &units); err != nil {
		return nil, httpx.Internal(err)
	}
	return units, nil
}

// countActiveAssignments counts the ACTIVE role assignments at one org unit —
// the members-listing total.
func (r *repository) countActiveAssignments(ctx context.Context, orgID string) (int64, error) {
	n, err := r.assignments.CountDocuments(ctx, bson.D{
		{Key: "org_unit_id", Value: orgID},
		{Key: "status", Value: domain.RoleAssignmentActive},
	})
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return n, nil
}

// listActiveAssignments pages the ACTIVE role assignments at one org unit
// (org_unit_id + role_code + status index).
func (r *repository) listActiveAssignments(ctx context.Context, orgID string, page httpx.Page) ([]domain.RoleAssignment, error) {
	cur, err := r.assignments.Find(ctx,
		bson.D{
			{Key: "org_unit_id", Value: orgID},
			{Key: "status", Value: domain.RoleAssignmentActive},
		},
		options.Find().
			SetSort(bson.D{{Key: "role_code", Value: 1}, {Key: "created_at", Value: 1}}).
			SetSkip(page.Offset).
			SetLimit(page.Limit))
	if err != nil {
		return nil, httpx.Internal(err)
	}
	assignments := []domain.RoleAssignment{}
	if err := cur.All(ctx, &assignments); err != nil {
		return nil, httpx.Internal(err)
	}
	return assignments, nil
}

// partiesByIDs fetches the given parties keyed by ID for the in-memory join.
func (r *repository) partiesByIDs(ctx context.Context, ids []string) (map[string]domain.Party, error) {
	byID := make(map[string]domain.Party, len(ids))
	if len(ids) == 0 {
		return byID, nil
	}
	cur, err := r.parties.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	var parties []domain.Party
	if err := cur.All(ctx, &parties); err != nil {
		return nil, httpx.Internal(err)
	}
	for _, p := range parties {
		byID[p.ID] = p
	}
	return byID, nil
}
