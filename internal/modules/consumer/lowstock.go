package consumer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// STORE_LOW_STOCK is the inbox template a store manager raises when a store's
// derived stock is about to run out. It is delivered to every SUPER_ADMIN /
// PCDF_ADMIN through the shared `notifications` collection (the same inbox
// behind GET /notifications/me), so the alert reaches the platform admins
// live and cross-device — named by the store it came from.
const templateStoreLowStock = "STORE_LOW_STOCK"

type lowStockRequest struct {
	StoreName string `json:"store_name"`
	Summary   string `json:"summary"` // e.g. "Toned Milk 500ml (4), Chai Special (2)"
	ItemCount int    `json:"item_count"`
}

// lowStock handles POST /consumer/stores/{storeId}/low-stock (STORE_MANAGER,
// own store, like every other /stores/{storeId} route): upsert (summary
// present) or clear (summary empty) the store's low-stock alert for every
// platform admin. Idempotent per (admin, store) so the 12s store poll can call
// it freely without spamming the inbox. The store check matters twice over
// now that the alert also rings the admins' phones: a manager of store A can
// neither raise nor clear store B's alert.
func (h *handler) lowStock(w http.ResponseWriter, r *http.Request) {
	storeID := chi.URLParam(r, "storeId")
	actor, _ := operatorActor(r)
	if err := h.svc.assertStore(r.Context(), actor, storeID); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	var body lowStockRequest
	_ = decode(r, &body)
	if body.StoreName == "" {
		body.StoreName = storeID
	}
	if err := h.svc.raiseLowStockAlert(r.Context(), storeID, body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// raiseLowStockAlert writes the admins' inbox rows on the service clock and
// rings the admins whose alert re-armed (operator_push.go); the same set
// posted again re-arms nobody, so a Saathi launch stays silent.
func (s *service) raiseLowStockAlert(ctx context.Context, storeID string, body lowStockRequest) error {
	now := s.now()
	rearmed, err := s.repo.raiseLowStockAt(ctx, storeID, body, now)
	if err != nil {
		return err
	}
	s.pushLowStock(ctx, now, storeID, body, rearmed)
	return nil
}

// raiseLowStock resolves the platform admins and upserts (or clears) one
// STORE_LOW_STOCK notification per admin, keyed on the store id.
//
// The alert re-arms (unread again, queued now) only when the low set really
// changed: a different summary or item count than the row holds, or no row
// yet. Saathi's store home starts its last-posted key empty, so every app
// launch or re-login posts the SAME set again; that repeat refreshes the row
// but leaves the admin's read, its status and its place in the inbox alone.
// POST /notifications/{id}/read treats the re-armed read_at: null as unread.
func (r *repository) raiseLowStock(ctx context.Context, storeID string, body lowStockRequest) error {
	_, err := r.raiseLowStockAt(ctx, storeID, body, time.Now())
	return err
}

// raiseLowStockAt is raiseLowStock on an explicit clock, answering which
// admins' alerts re-armed (a new row, or a changed summary or count): the
// ones to ring. Each row is read and written in one atomic step, so two
// concurrent posts of one change re-arm (and ring) an admin once.
func (r *repository) raiseLowStockAt(ctx context.Context, storeID string, body lowStockRequest, at time.Time) ([]primitive.ObjectID, error) {
	admins, err := r.adminRecipients(ctx)
	if err != nil {
		return nil, err
	}
	now := at.UTC()
	var rearmed []primitive.ObjectID
	count := fmt.Sprintf("%d", body.ItemCount)
	// Every value that comes from the request or the database goes in as a
	// $literal: this is a pipeline update, where a string starting with "$"
	// would otherwise be read as a field path.
	literal := func(v any) bson.D { return bson.D{{Key: "$literal", Value: v}} }
	// Evaluated against the row as it stood; on an upsert that is the
	// filter's fields alone, so a first raise always counts as changed.
	changed := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "$ne", Value: bson.A{"$params.summary", literal(body.Summary)}}},
		bson.D{{Key: "$ne", Value: bson.A{"$params.item_count", literal(count)}}},
	}}}
	onChange := func(field string, fresh any) bson.D {
		return bson.D{{Key: "$cond", Value: bson.A{changed, literal(fresh), "$" + field}}}
	}
	for _, a := range admins {
		filter := bson.D{
			{Key: "party_id", Value: a.id},
			{Key: "template_key", Value: templateStoreLowStock},
			{Key: "params.store_id", Value: storeID},
		}
		if body.Summary == "" || body.ItemCount == 0 {
			// Stock recovered — clear this store's alert for the admin.
			if _, err := r.notifications.DeleteMany(ctx, filter); err != nil {
				return nil, httpx.Internal(fmt.Errorf("clear low-stock notification: %w", err))
			}
			continue
		}
		update := mongo.Pipeline{bson.D{{Key: "$set", Value: bson.D{
			{Key: "party_id", Value: a.id},
			{Key: "phone", Value: literal(a.phone)},
			{Key: "channel", Value: domain.ChannelApp},
			{Key: "template_key", Value: templateStoreLowStock},
			{Key: "language", Value: "hi"},
			{Key: "params", Value: literal(bson.M{
				"store_id":   storeID,
				"store":      body.StoreName,
				"summary":    body.Summary,
				"item_count": count,
			})},
			{Key: "status", Value: onChange("status", domain.NotificationQueued)},
			{Key: "queued_at", Value: onChange("queued_at", now)},
			{Key: "read_at", Value: onChange("read_at", nil)},
		}}}}
		var before struct {
			Params map[string]string `bson:"params"`
		}
		err := r.notifications.FindOneAndUpdate(ctx, filter, update,
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.Before)).Decode(&before)
		switch {
		case errors.Is(err, mongo.ErrNoDocuments):
			rearmed = append(rearmed, a.id) // a first raise
		case err != nil:
			return nil, httpx.Internal(fmt.Errorf("upsert low-stock notification: %w", err))
		case before.Params["summary"] != body.Summary || before.Params["item_count"] != count:
			rearmed = append(rearmed, a.id) // the same test the pipeline's $cond made
		}
	}
	return rearmed, nil
}

type adminRecipient struct {
	id    primitive.ObjectID
	phone string
}

// adminRecipients resolves the active SUPER_ADMIN / PCDF_ADMIN party holders
// (distinct) with their phone — the platform admins who see store alerts.
func (r *repository) adminRecipients(ctx context.Context) ([]adminRecipient, error) {
	cur, err := r.roleAssignments.Find(ctx, bson.D{
		{Key: "role_code", Value: bson.D{{Key: "$in", Value: bson.A{domain.RoleSuperAdmin, domain.RolePCDFAdmin}}}},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}, options.Find().SetProjection(bson.D{{Key: "party_id", Value: 1}}).SetLimit(50))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find admin assignments: %w", err))
	}
	var rows []struct {
		PartyID primitive.ObjectID `bson:"party_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode admin assignments: %w", err))
	}
	seen := map[primitive.ObjectID]struct{}{}
	ids := make([]primitive.ObjectID, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.PartyID]; ok {
			continue
		}
		seen[row.PartyID] = struct{}{}
		ids = append(ids, row.PartyID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	pcur, err := r.parties.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetProjection(bson.D{{Key: "phone", Value: 1}}))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find admin parties: %w", err))
	}
	var prows []struct {
		ID    primitive.ObjectID `bson:"_id"`
		Phone string             `bson:"phone"`
	}
	if err := pcur.All(ctx, &prows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode admin parties: %w", err))
	}
	out := make([]adminRecipient, 0, len(prows))
	for _, p := range prows {
		out = append(out, adminRecipient{id: p.ID, phone: p.Phone})
	}
	return out, nil
}
