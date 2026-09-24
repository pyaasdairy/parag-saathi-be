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

// COMPLAINT REGISTER - the customer's grievance channel, to the contract the
// consumer app calls (HANDOFF-FRONTEND-PHASE2 s5, pyaas-consumer
// lib/complaints.ts).
//
// The app never blocks a member from complaining: it writes the row on the
// phone with a human reference (PYS-XXXXX), shows it immediately, POSTs it
// here, and retries anything filed offline on every refresh. Two rules
// follow from that design:
//
//  1. THE REFERENCE IS THE CLIENT'S. The member has already been shown it and
//     may have quoted it to support before the row ever reached the server,
//     so `ref` is required on the wire, is what support searches by, and
//     re-POSTing the same ref must return the row already held rather than
//     file a duplicate. It is unique PER CONSUMER, never globally: two members
//     can hold the same short code without one of them being refused.
//  2. `resolution` IS READ BY THE CUSTOMER verbatim. It is not an internal
//     note field; whatever an operator types here is what the member sees.
//
// The client keeps a `queued` status for rows it has not yet synced. That
// value never travels - a row that reaches this register is at least `open`.
//
// Three surfaces share the one store below: the member's own routes
// (/consumer/complaints), the Saathi operator queue (/consumer/ops/complaints,
// STORE_MANAGER or SUPER_ADMIN token) and the website's admin CRM
// (/consumer/admin/crm/complaints, X-Admin-Key). The two operator surfaces
// deliberately live under their own prefixes: the member group and the
// operator group are mounted on the same /consumer router, and chi does not
// panic on a duplicate pattern, it silently serves the last one registered.
const collComplaints = "consumer_complaints"

// The closed category set the app ships. A blank category defaults to
// "other" (the member typed a complaint and must not lose it to a dropdown
// they skipped); anything else is refused rather than stored, so the support
// queue cannot grow categories nobody filters on.
var complaintCategories = map[string]bool{
	"missing": true, "quality": true, "late": true,
	"payment": true, "rider": true, "app": true, "other": true,
}

// Status vocabulary, as the app renders it.
const (
	complaintOpen     = "open"
	complaintInReview = "in_review"
	complaintResolved = "resolved"
	complaintClosed   = "closed"
)

var complaintStatuses = map[string]bool{
	complaintOpen: true, complaintInReview: true, complaintResolved: true, complaintClosed: true,
}

// complaintTextMax caps the member's detail and the operator's resolution.
const complaintTextMax = 4000

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
	// Resolution is shown to the MEMBER verbatim - write it as something a
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

func (r *repository) complaints() *mongo.Collection {
	return r.accounts.Database().Collection(collComplaints)
}

// ensureComplaintIndexes - the unique (consumer_id, ref) index is the race
// guard behind rule 1. Its build is NON-fatal at boot (module.go): the
// register is not a money gate and must never refuse to boot a backend that
// is also serving orders and wallets. fileComplaint therefore checks for the
// row before it inserts, so a retry stays idempotent with the index absent.
func (r *repository) ensureComplaintIndexes(ctx context.Context) error {
	_, err := r.complaints().Indexes().CreateMany(ctx, []mongo.IndexModel{
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

// fileComplaint stores one complaint. Idempotent by (consumer, ref): a repeat
// of a ref already held returns the stored row untouched - status and any
// resolution an operator has since written included - so the app's offline
// retry is safe to run as often as it likes.
func (s *service) fileComplaint(ctx context.Context, consumerID primitive.ObjectID, in complaintInput) (*complaint, error) {
	category := strings.ToLower(strings.TrimSpace(in.Category))
	if category == "" {
		category = "other"
	}
	if !complaintCategories[category] {
		return nil, errBadRequest("unknown complaint category: " + category)
	}
	detail := strings.TrimSpace(in.Detail)
	if detail == "" {
		return nil, errBadRequest("a complaint needs a detail")
	}
	if len(detail) > complaintTextMax {
		detail = detail[:complaintTextMax]
	}
	ref := strings.ToUpper(strings.TrimSpace(in.Ref))
	if ref == "" {
		return nil, errBadRequest("a complaint reference (ref) is required")
	}
	if len(ref) > 32 {
		ref = ref[:32]
	}
	byRef := bson.D{{Key: "consumer_id", Value: consumerID}, {Key: "ref", Value: ref}}

	// IDEMPOTENCY MUST NOT DEPEND ON THE INDEX ALONE. The unique index is the
	// race guard, but its build is non-fatal at boot, so a failed build would
	// otherwise turn every retry into a duplicate. Check first, and keep the
	// duplicate-key path below for the genuine race: correct with the index,
	// still correct without it.
	var existing complaint
	if err := s.repo.complaints().FindOne(ctx, byRef).Decode(&existing); err == nil {
		return &existing, nil
	} else if err != mongo.ErrNoDocuments {
		return nil, errInternal("could not check the complaint register")
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
			// The same filing, retried in the window between the check and the
			// insert - hand back the row that won.
			if e := s.repo.complaints().FindOne(ctx, byRef).Decode(&existing); e == nil {
				return &existing, nil
			}
		}
		return nil, errInternal("could not file the complaint")
	}
	s.log.InfoContext(ctx, "consumer complaint filed", "ref", ref, "category", category, "order", c.OrderID)
	// CRM (contract C6, inert unless CRM_ENABLED): only a NEW row emits; the
	// duplicate returns above are the app's retry of a filing already made.
	if crmEnabled() {
		s.emitCRMEvent(ctx, "complaint.created", consumerID, map[string]any{
			"complaint_id": c.ID, "ref": c.Ref, "category": c.Category, "order_id": c.OrderID,
		})
	}
	return c, nil
}

// listComplaints returns the member's own complaints, newest first.
func (s *service) listComplaints(ctx context.Context, consumerID primitive.ObjectID) ([]complaint, error) {
	cur, err := s.repo.complaints().Find(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(200))
	if err != nil {
		return nil, errInternal("could not read complaints")
	}
	out := []complaint{} // never nil - the app renders a list, not a null
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("could not decode complaints")
	}
	return out, nil
}

// -- Answering a complaint (operator side) -----------------------------------

// complaintUpdateInput is the operator's side. `resolution` is READ BY THE
// CUSTOMER verbatim, so it is written as something a member should read.
type complaintUpdateInput struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
}

// answerComplaint is the ONE write path behind both operator surfaces: it
// moves the status and/or writes the resolution the member reads, on the row
// the filter selects (by ref for the Saathi queue, by complaint id for the
// website's admin CRM). Without this the register was write-only: a member
// could file, and nothing in the product could ever answer them.
func (s *service) answerComplaint(ctx context.Context, filter bson.D, in complaintUpdateInput) (*complaint, error) {
	set := bson.D{{Key: "updated_at", Value: time.Now().UTC()}}
	st := strings.ToLower(strings.TrimSpace(in.Status))
	if st != "" {
		if !complaintStatuses[st] {
			return nil, errBadRequest("unknown complaint status: " + st)
		}
		set = append(set, bson.E{Key: "status", Value: st})
	}
	if res := strings.TrimSpace(in.Resolution); res != "" {
		if len(res) > complaintTextMax {
			res = res[:complaintTextMax]
		}
		set = append(set, bson.E{Key: "resolution", Value: res})
	}
	if len(set) == 1 { // only the timestamp - nothing was actually asked for
		return nil, errBadRequest("send a status or a resolution")
	}
	var updated complaint
	err := s.repo.complaints().FindOneAndUpdate(ctx, filter,
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&updated)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errNotFound("no complaint with that reference")
		}
		return nil, errInternal("could not update the complaint")
	}
	// CRM (contract C6, inert unless CRM_ENABLED): the member hears back.
	if st == complaintResolved && crmEnabled() {
		s.emitCRMEvent(ctx, "complaint.resolved", updated.ConsumerID, map[string]any{
			"complaint_id": updated.ID, "ref": updated.Ref, "resolution": updated.Resolution,
		})
	}
	return &updated, nil
}

// updateComplaint answers the complaint the operator names: by its reference
// (the code the member quotes on the phone) or by its complaint id (cmp_...,
// the row's own id on the queue). A reference is unique PER MEMBER only (rule
// 1), so one that two members hold is refused rather than answered on
// whichever row comes first: the resolution is read by the member verbatim.
func (s *service) updateComplaint(ctx context.Context, ref string, in complaintUpdateInput) (*complaint, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "cmp_") {
		return s.answerComplaint(ctx, bson.D{{Key: "complaint_id", Value: ref}}, in)
	}
	ref = strings.ToUpper(ref)
	if ref == "" {
		return nil, errBadRequest("a complaint reference is required")
	}
	byRef := bson.D{{Key: "ref", Value: ref}}
	n, err := s.repo.complaints().CountDocuments(ctx, byRef, options.Count().SetLimit(2))
	if err != nil {
		return nil, errInternal("could not check the complaint register")
	}
	if n > 1 {
		return nil, errConflict("AMBIGUOUS_REF", "more than one member holds this reference; answer it by its complaint id")
	}
	return s.answerComplaint(ctx, byRef, in)
}

// listAllComplaints is the support queue, newest first, optionally filtered
// to one status. An unknown status is refused rather than silently widened.
func (s *service) listAllComplaints(ctx context.Context, status string) ([]complaint, error) {
	f := bson.D{}
	if st := strings.ToLower(strings.TrimSpace(status)); st != "" {
		if !complaintStatuses[st] {
			return nil, errBadRequest("unknown complaint status: " + st)
		}
		f = bson.D{{Key: "status", Value: st}}
	}
	cur, err := s.repo.complaints().Find(ctx, f,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500))
	if err != nil {
		return nil, errInternal("could not read the complaint queue")
	}
	out := []complaint{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("could not decode the complaint queue")
	}
	return out, nil
}

// -- HTTP (consumer) -----------------------------------------------------------

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

// -- HTTP (Saathi operator queue, /consumer/ops/complaints) -------------------
//
// Saathi's wire format, like every route in the operator group (and like the
// group's own auth refusals): {data} on success, {error:{code,message}} on
// failure, which is the only shape the Saathi ApiClient reads.

func (h *handler) opsListComplaints(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.listAllComplaints(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (h *handler) opsUpdateComplaint(w http.ResponseWriter, r *http.Request) {
	var in complaintUpdateInput
	if err := decode(r, &in); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	c, err := h.svc.updateComplaint(r.Context(), chi.URLParam(r, "ref"), in)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// -- HTTP (admin CRM, /consumer/admin/crm/complaints) -------------------------
//
// The website's half of the register: SUPER_ADMIN token or the website's
// admin key (admin_crm.go), camelCase rows in an {items,total,open} envelope.

func (h *handler) crmComplaints(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.listAllComplaints(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
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

// crmUpdateComplaint answers the complaint the website names by its id.
func (h *handler) crmUpdateComplaint(w http.ResponseWriter, r *http.Request) {
	var in complaintUpdateInput
	if err := decode(r, &in); err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	updated, err := h.svc.answerComplaint(r.Context(),
		bson.D{{Key: "complaint_id", Value: chi.URLParam(r, "complaintId")}}, in)
	if err != nil {
		httpx.Error(w, r, toHTTPErr(err))
		return
	}
	httpx.JSON(w, http.StatusOK, updated)
}
