package consumer

// Customer complaint register (FE phase 2, §5 of HANDOFF-FRONTEND-PHASE2).
//
// The app never blocks a member from complaining: it writes the row on device
// with a human reference (PYS-XXXXX), shows it immediately, and POSTs it when
// a backend answers — retrying on every refresh for anything filed offline.
// This file is that backend.
//
// TWO RULES THE APP'S DESIGN IMPOSES ON US:
//
//  1. The REFERENCE IS THE CLIENT'S. The member has already been shown
//     "PYS-4K2Q9" and may have quoted it in an email to support before the row
//     ever reached us. So `ref` is supplied by the app, it is what support
//     searches by, and re-POSTing the same ref must return the EXISTING row
//     rather than filing a duplicate — the app retries offline rows on every
//     refresh, so without that guarantee one bad network day becomes five
//     identical complaints in the queue.
//  2. `resolution` IS READ BY THE CUSTOMER, verbatim. It is not an internal
//     note field; whatever an operator types here is what the member sees.
//
// The client keeps a `queued` status for rows it has not yet synced. That value
// never travels — a row that reaches us is at least `open`.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collComplaints = "consumer_complaints"

// The closed enums the app ships. Anything else is refused rather than stored,
// so the support queue cannot grow categories nobody filters on.
var complaintCategories = map[string]bool{
	"missing": true, "quality": true, "late": true,
	"payment": true, "rider": true, "app": true, "other": true,
}

var complaintStatuses = map[string]bool{
	"open": true, "in_review": true, "resolved": true, "closed": true,
}

type complaint struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"         json:"id"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"           json:"-"`
	Ref        string             `bson:"ref"                   json:"ref"`
	Category   string             `bson:"category"              json:"category"`
	OrderID    string             `bson:"order_id,omitempty"    json:"order_id,omitempty"`
	Detail     string             `bson:"detail"                json:"detail"`
	PhotoURI   string             `bson:"photo_uri,omitempty"   json:"photo_uri,omitempty"`
	Status     string             `bson:"status"                json:"status"`
	Resolution string             `bson:"resolution,omitempty"  json:"resolution,omitempty"`
	CreatedAt  time.Time          `bson:"created_at"            json:"created_at"`
	UpdatedAt  time.Time          `bson:"updated_at"            json:"updated_at"`
}

type complaintInput struct {
	Ref      string `json:"ref"`
	Category string `json:"category"`
	OrderID  string `json:"order_id"`
	Detail   string `json:"detail"`
	PhotoURI string `json:"photo_uri"`
}

// ensureComplaintIndexes — the unique (consumer_id, ref) index IS the
// idempotency guarantee rule 1 describes. Without it a retried offline row
// files a duplicate. Scoped to the consumer so two members' references can
// never collide with each other.
func (r *repository) ensureComplaintIndexes(ctx context.Context) error {
	col := r.accounts.Database().Collection(collComplaints)
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "consumer_id", Value: 1}, {Key: "ref", Value: 1}},
			Options: options.Index().SetUnique(true),
		},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "created_at", Value: -1}}},
	})
	return err
}

func (s *service) complaintsCol() *mongo.Collection {
	return s.repo.accounts.Database().Collection(collComplaints)
}

// fileComplaint stores one complaint. Idempotent by (consumer, ref): a repeat
// of a ref we already hold returns the stored row untouched, so the app's
// offline retry is safe to run as often as it likes.
func (s *service) fileComplaint(ctx context.Context, consumerID primitive.ObjectID, in complaintInput) (*complaint, error) {
	cat := strings.ToLower(strings.TrimSpace(in.Category))
	if cat == "" {
		cat = "other"
	}
	if !complaintCategories[cat] {
		return nil, errBadRequest("unknown complaint category: " + cat)
	}
	detail := strings.TrimSpace(in.Detail)
	if detail == "" {
		return nil, errBadRequest("a complaint needs a detail")
	}
	if len(detail) > 4000 {
		detail = detail[:4000]
	}
	ref := strings.ToUpper(strings.TrimSpace(in.Ref))
	if ref == "" {
		return nil, errBadRequest("a complaint reference (ref) is required")
	}

	// IDEMPOTENCY MUST NOT DEPEND ON THE INDEX ALONE. The unique (consumer, ref)
	// index is the race guard, but its build is deliberately non-fatal at boot —
	// a complaint register is not a money gate and must never refuse to boot the
	// backend. So if that build ever fails, a retry would silently file a
	// duplicate. Check first, and keep the duplicate-key path below for the
	// genuine race: correct with the index, still correct without it.
	var existing complaint
	if err := s.complaintsCol().FindOne(ctx, bson.D{
		{Key: "consumer_id", Value: consumerID}, {Key: "ref", Value: ref},
	}).Decode(&existing); err == nil {
		return &existing, nil
	} else if err != mongo.ErrNoDocuments {
		return nil, errInternal("could not check the complaint register")
	}

	now := time.Now().UTC()
	c := &complaint{
		ID: primitive.NewObjectID(), ConsumerID: consumerID, Ref: ref,
		Category: cat, OrderID: strings.TrimSpace(in.OrderID), Detail: detail,
		PhotoURI: strings.TrimSpace(in.PhotoURI),
		Status:   "open", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.complaintsCol().InsertOne(ctx, c); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// Already filed — hand back what we hold, so the member keeps the
			// status and any resolution an operator has since written.
			var existing complaint
			if ferr := s.complaintsCol().FindOne(ctx, bson.D{
				{Key: "consumer_id", Value: consumerID}, {Key: "ref", Value: ref},
			}).Decode(&existing); ferr == nil {
				return &existing, nil
			}
		}
		return nil, errInternal("could not file the complaint")
	}
	s.log.InfoContext(ctx, "complaint filed", "ref", ref, "category", cat)
	return c, nil
}

// listComplaints returns the member's own complaints, newest first.
func (s *service) listComplaints(ctx context.Context, consumerID primitive.ObjectID) ([]complaint, error) {
	cur, err := s.complaintsCol().Find(ctx,
		bson.D{{Key: "consumer_id", Value: consumerID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(200))
	if err != nil {
		return nil, errInternal("could not read complaints")
	}
	out := []complaint{} // never nil — the app renders a list, not a null
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("could not decode complaints")
	}
	return out, nil
}

// ── HTTP ───────────────────────────────────────────────────────────────────

func (h *handler) createComplaint(w http.ResponseWriter, r *http.Request) {
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

// ── Operator: answering a complaint ────────────────────────────────────────

// complaintUpdateInput is the operator's side. `resolution` is READ BY THE
// CUSTOMER verbatim, so it is written as something a member should read.
type complaintUpdateInput struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
}

// updateComplaint moves a complaint's status and/or writes the resolution the
// member sees. Without this the register was write-only: a member could file,
// and nothing in the product could ever answer them.
func (s *service) updateComplaint(ctx context.Context, ref string, in complaintUpdateInput) (*complaint, error) {
	ref = strings.ToUpper(strings.TrimSpace(ref))
	if ref == "" {
		return nil, errBadRequest("a complaint reference is required")
	}
	set := bson.D{{Key: "updated_at", Value: time.Now().UTC()}}
	if st := strings.ToLower(strings.TrimSpace(in.Status)); st != "" {
		if !complaintStatuses[st] {
			return nil, errBadRequest("unknown complaint status: " + st)
		}
		set = append(set, bson.E{Key: "status", Value: st})
	}
	if res := strings.TrimSpace(in.Resolution); res != "" {
		if len(res) > 4000 {
			res = res[:4000]
		}
		set = append(set, bson.E{Key: "resolution", Value: res})
	}
	if len(set) == 1 { // only the timestamp — nothing was actually asked for
		return nil, errBadRequest("send a status or a resolution")
	}
	var updated complaint
	err := s.complaintsCol().FindOneAndUpdate(ctx,
		bson.D{{Key: "ref", Value: ref}},
		bson.D{{Key: "$set", Value: set}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&updated)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, errNotFound("no complaint with that reference")
		}
		return nil, errInternal("could not update the complaint")
	}
	return &updated, nil
}

// listAllComplaints is the support queue — open ones first, newest first.
func (s *service) listAllComplaints(ctx context.Context, status string) ([]complaint, error) {
	f := bson.D{}
	if st := strings.ToLower(strings.TrimSpace(status)); st != "" {
		if !complaintStatuses[st] {
			return nil, errBadRequest("unknown complaint status: " + st)
		}
		f = bson.D{{Key: "status", Value: st}}
	}
	cur, err := s.complaintsCol().Find(ctx, f,
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

func (h *handler) opsListComplaints(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.listAllComplaints(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) opsUpdateComplaint(w http.ResponseWriter, r *http.Request) {
	var in complaintUpdateInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	c, err := h.svc.updateComplaint(r.Context(), chi.URLParam(r, "ref"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
