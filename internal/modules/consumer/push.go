package consumer

// Push device registry (FE phase 2, §3 of HANDOFF-FRONTEND-PHASE2).
//
// The app asks for notification permission ONLY from its notifications screen,
// after a primer — never at launch — and on a grant it posts the Expo push
// token here. This file stores it. It does NOT send anything: an FCM/APNs
// sender is a separate piece of work that also needs google-services.json in
// the Android build, and until it exists a member whose app is closed gets
// nothing. Storing the token first is what makes that sender possible later
// without asking every member to re-grant.
//
// ONE ROW PER DEVICE, keyed by the token itself. A token is already unique per
// install; keying on it means a reinstall adds a row and a re-grant updates one
// rather than accumulating duplicates that would each get a copy of every push.
//
// A token can MOVE between accounts — a shared family handset where one member
// signs out and another signs in. The consumer id is therefore overwritten on
// every upsert, so the device belongs to whoever registered it last. Anything
// else would push one member's wallet balance to another member's phone.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collPushDevices = "consumer_push_devices"

type pushDevice struct {
	ID         primitive.ObjectID `bson:"_id,omitempty"      json:"-"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"        json:"-"`
	Token      string             `bson:"token"              json:"token"`
	Platform   string             `bson:"platform"           json:"platform"` // ios | android
	Provider   string             `bson:"provider"           json:"provider"` // expo
	CreatedAt  time.Time          `bson:"created_at"         json:"created_at"`
	UpdatedAt  time.Time          `bson:"updated_at"         json:"updated_at"`
}

type pushRegisterInput struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
	Provider string `json:"provider"`
}

func (r *repository) ensurePushIndexes(ctx context.Context) error {
	col := r.accounts.Database().Collection(collPushDevices)
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// The token IS the device — unique, so a re-grant updates in place.
		{Keys: bson.D{{Key: "token", Value: 1}}, Options: options.Index().SetUnique(true)},
		// The sender's query: every live device for one member.
		{Keys: bson.D{{Key: "consumer_id", Value: 1}}},
	})
	return err
}

func (s *service) pushCol() *mongo.Collection {
	return s.repo.accounts.Database().Collection(collPushDevices)
}

// registerPushDevice upserts one device. Idempotent: the app may call this on
// every launch after a grant, and it must stay one row.
func (s *service) registerPushDevice(ctx context.Context, consumerID primitive.ObjectID, in pushRegisterInput) error {
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return errBadRequest("a push token is required")
	}
	if len(token) > 512 {
		return errBadRequest("push token is implausibly long")
	}
	platform := strings.ToLower(strings.TrimSpace(in.Platform))
	if platform != "ios" && platform != "android" {
		return errBadRequest("platform must be ios or android")
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider == "" {
		provider = "expo"
	}
	now := time.Now().UTC()
	_, err := s.pushCol().UpdateOne(ctx,
		bson.D{{Key: "token", Value: token}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "consumer_id", Value: consumerID}, // the device follows the current signer-in
				{Key: "platform", Value: platform},
				{Key: "provider", Value: provider},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{
				{Key: "token", Value: token},
				{Key: "created_at", Value: now},
			}},
		},
		options.Update().SetUpsert(true))
	if err != nil {
		return errInternal("could not register the device")
	}
	return nil
}

// pushDevicesFor is the seam an FCM/APNs sender will read. Unused until that
// sender exists — deliberately here so the storage side is complete and the
// sender is the only thing left to build.
func (s *service) pushDevicesFor(ctx context.Context, consumerID primitive.ObjectID) ([]pushDevice, error) {
	cur, err := s.pushCol().Find(ctx, bson.D{{Key: "consumer_id", Value: consumerID}})
	if err != nil {
		return nil, errInternal("could not read devices")
	}
	out := []pushDevice{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, errInternal("could not decode devices")
	}
	return out, nil
}

func (h *handler) registerPushDevice(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in pushRegisterInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.registerPushDevice(r.Context(), id, in); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}
