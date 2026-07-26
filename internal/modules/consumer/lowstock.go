package consumer

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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

// lowStock handles POST /consumer/stores/{storeId}/low-stock (STORE_MANAGER):
// upsert (summary present) or clear (summary empty) the store's low-stock
// alert for every platform admin. Idempotent per (admin, store) so the 12s
// store poll can call it freely without spamming the inbox.
func (h *handler) lowStock(w http.ResponseWriter, r *http.Request) {
	storeID := chi.URLParam(r, "storeId")
	var body lowStockRequest
	_ = decode(r, &body)
	if body.StoreName == "" {
		body.StoreName = storeID
	}
	if err := h.svc.repo.raiseLowStock(r.Context(), storeID, body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// raiseLowStock resolves the platform admins and upserts (or clears) one
// STORE_LOW_STOCK notification per admin, keyed on the store id.
func (r *repository) raiseLowStock(ctx context.Context, storeID string, body lowStockRequest) error {
	admins, err := r.adminRecipients(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, a := range admins {
		filter := bson.D{
			{Key: "party_id", Value: a.id},
			{Key: "template_key", Value: templateStoreLowStock},
			{Key: "params.store_id", Value: storeID},
		}
		if body.Summary == "" || body.ItemCount == 0 {
			// Stock recovered — clear this store's alert for the admin.
			if _, err := r.notifications.DeleteMany(ctx, filter); err != nil {
				return httpx.Internal(fmt.Errorf("clear low-stock notification: %w", err))
			}
			continue
		}
		update := bson.D{{Key: "$set", Value: bson.D{
			{Key: "party_id", Value: a.id},
			{Key: "phone", Value: a.phone},
			{Key: "channel", Value: domain.ChannelApp},
			{Key: "template_key", Value: templateStoreLowStock},
			{Key: "language", Value: "hi"},
			{Key: "params", Value: bson.M{
				"store_id":   storeID,
				"store":      body.StoreName,
				"summary":    body.Summary,
				"item_count": fmt.Sprintf("%d", body.ItemCount),
			}},
			{Key: "status", Value: domain.NotificationQueued},
			{Key: "queued_at", Value: now},
			{Key: "read_at", Value: nil},
		}}}
		if _, err := r.notifications.UpdateOne(ctx, filter, update, options.Update().SetUpsert(true)); err != nil {
			return httpx.Internal(fmt.Errorf("upsert low-stock notification: %w", err))
		}
	}
	return nil
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
