package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// COMPLAINT REGISTER — the customer's grievance channel, to the contract the
// consumer app already calls (HANDOFF-FRONTEND-PHASE2-2026-09-18 §5).
//
// Until now there was no route behind it. The app wrote the complaint on the
// phone, showed the member a reference ("PYS-4F21A registered"), and retried a
// POST that 404ed forever — while the same screen printed the grievance
// officer's name and duties from the Privacy Policy. A grievance channel that
// reaches nobody is worse than no channel at all, so this is the missing half.
//
// The app owns the reference: it files offline first and syncs later, so `ref`
// arrives from the client and is what the member quotes on the phone. It is
// unique PER CONSUMER (a retry of the same filing must not create a second
// row), never globally — two members can hold the same short code without one
// of them being refused.
const collComplaints = "consumer_complaints"

var complaintCategories = map[string]bool{
	"missing": true, "quality": true, "late": true,
	"payment": true, "rider": true, "app": true, "other": true,
}

// Status vocabulary, as the app renders it. "queued" is the client's own
// not-yet-synced state and is never stored here.
const (
	complaintOpen     = "open"
	complaintInReview = "in_review"
	complaintResolved = "resolved"
	complaintClosed   = "closed"
)

var complaintStatuses = map[string]bool{
	complaintOpen: true, complaintInReview: true, complaintResolved: true, complaintClosed: true,
}

type complaint struct {
	MongoID    primitive.ObjectID `bson:"_id,omitempty"      json:"-"`
	ID         string             `bson:"complaint_id"       json:"id"`
	Ref        string             `bson:"ref"                json:"ref"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"        json:"-"`
	Phone      string             `bson:"phone,omitempty"    json:"-"`
	Category   string             `bson:"category"           json:"category"`
	OrderID    string             `bson:"order_id,omitempty" json:"order_id,omitempty"`
	Detail     string             `bson:"detail"             json:"detail"`
	PhotoURI   string             `bson:"photo_uri,omitempty" json:"photo_uri,omitempty"`
	Status     string             `bson:"status"             json:"status"`
	// Resolution is shown to the MEMBER verbatim — write it as something a
	// customer should read.
	Resolution string    `bson:"resolution,omitempty" json:"resolution,omitempty"`
	CreatedAt  time.Time `bson:"created_at"         json:"created_at"`
	UpdatedAt  time.Time `bson:"updated_at"         json:"updated_at"`
}

func newComplaintID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "cmp_" + hex.EncodeToString(b)
}

// newComplaintRef mints the short code the member quotes on the phone, for the
// rare filing that arrives without one.
func newComplaintRef() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "PYS-" + strings.ToUpper(hex.EncodeToString(b))
}

func (r *repository) complaints() *mongo.Collection {
	return r.accounts.Database().Collection(collComplaints)
}

func (r *repository) ensureComplaintIndexes(ctx context.Context) error {
	_, err := r.complaints().Indexes().CreateMany(ctx, []mongo.IndexModel{
		// One row per (consumer, ref): the app retries the same filing until it
		// syncs, and a retry must never mint a second complaint.
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "ref", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}}},
	})
	return err
}

type complaintInput struct {
	Ref      string `json:"ref"`
	Category string `json:"category"`
	OrderID  string `json:"order_id"`
	Detail   string `json:"detail"`
	PhotoURI string `json:"photo_uri"`
}

func (s *service) fileComplaint(ctx context.Context, consumerID primitive.ObjectID, in complaintInput) (*complaint, error) {
	detail := strings.TrimSpace(in.Detail)
	if detail == "" {
		return nil, errBadRequest("tell us what went wrong")
	}
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	category := strings.ToLower(strings.TrimSpace(in.Category))
	if !complaintCategories[category] {
		category = "other" // never refuse a complaint over a category typo
	}
	ref := strings.ToUpper(strings.TrimSpace(in.Ref))
	if ref == "" {
		ref = newComplaintRef()
	}
	if len(ref) > 32 {
		ref = ref[:32]
	}
	now := time.Now().UTC()
	c := &complaint{
		MongoID: primitive.NewObjectID(), ID: newComplaintID(), Ref: ref, ConsumerID: consumerID,
		Category: category, OrderID: strings.TrimSpace(in.OrderID), Detail: detail,
		PhotoURI: strings.TrimSpace(in.PhotoURI), Status: complaintOpen,
		CreatedAt: now, UpdatedAt: now,
	}
	if acct, err := s.repo.findAccountByID(ctx, consumerID); err == nil && acct != nil {
		c.Phone = acct.Phone
	}
	if _, err := s.repo.complaints().InsertOne(ctx, c); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// The same filing, retried after an offline spell — hand back the row
			// that already exists so the app stops queueing it.
			var existing complaint
			if e := s.repo.complaints().FindOne(ctx,
				bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "ref", Value: ref}}).Decode(&existing); e == nil {
				return &existing, nil
			}
		}
		return nil, errInternal("complaint could not be filed")
	}
	s.log.InfoContext(ctx, "consumer complaint filed", "ref", ref, "category", category, "order", c.OrderID)
	return c, nil
}

func (s *service) listComplaints(ctx context.Context, consumerID primitive.ObjectID) ([]complaint, error) {
	cur, err := s.repo.complaints().Find(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(200))
	if err != nil {
		return nil, errInternal("complaints lookup failed")
	}
	out := []complaint{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("complaints decode failed")
	}
	return out, nil
}

// ── HTTP (consumer) ─────────────────────────────────────────────────────────

func (h *handler) fileComplaint(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in complaintInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	c, err := h.svc.fileComplaint(r.Context(), id, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *handler) listComplaints(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.svc.listComplaints(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ── HTTP (admin CRM) ────────────────────────────────────────────────────────
//
// The half that makes the register real: somebody at PYAAS has to see these,
// and the member has to get an answer back. Mounted under /consumer/admin/*
// (SUPER_ADMIN token or the website's admin key), like the rest of the CRM.

func (h *handler) crmComplaints(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter := bson.D{}
	if st := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status"))); complaintStatuses[st] {
		filter = append(filter, bson.E{Key: "status", Value: st})
	}
	cur, err := h.svc.repo.complaints().Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errInternal("complaints lookup failed")))
		return
	}
	var rows []complaint
	_ = cur.All(ctx, &rows)
	out := make([]map[string]any, 0, len(rows))
	open := 0
	for _, c := range rows {
		if c.Status == complaintOpen || c.Status == complaintInReview {
			open++
		}
		out = append(out, map[string]any{
			"id": c.ID, "ref": c.Ref, "category": c.Category, "orderId": c.OrderID,
			"detail": c.Detail, "photoUri": c.PhotoURI, "status": c.Status,
			"resolution": c.Resolution, "phone": c.Phone,
			"createdAt": c.CreatedAt, "updatedAt": c.UpdatedAt,
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out), "open": open})
}

// crmUpdateComplaint moves a complaint along and writes the answer the member
// reads. `resolution` is rendered to them verbatim.
func (h *handler) crmUpdateComplaint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Status     string `json:"status"`
		Resolution string `json:"resolution"`
	}
	if err := decode(r, &body); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	st := strings.ToLower(strings.TrimSpace(body.Status))
	if !complaintStatuses[st] {
		httpx.Error(w, r, httpx.BadRequest("BAD_STATUS", "status must be open, in_review, resolved or closed"))
		return
	}
	set := bson.D{{Key: "status", Value: st}, {Key: "updated_at", Value: time.Now().UTC()}}
	if res := strings.TrimSpace(body.Resolution); res != "" {
		if len(res) > 2000 {
			res = res[:2000]
		}
		set = append(set, bson.E{Key: "resolution", Value: res})
	}
	after := options.After
	var updated complaint
	err := h.svc.repo.complaints().FindOneAndUpdate(ctx,
		bson.D{{Key: "complaint_id", Value: chi.URLParam(r, "complaintId")}},
		bson.D{{Key: "$set", Value: set}},
		&options.FindOneAndUpdateOptions{ReturnDocument: &after},
	).Decode(&updated)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(errNotFound("complaint not found")))
		return
	}
	httpx.JSON(w, http.StatusOK, updated)
}
