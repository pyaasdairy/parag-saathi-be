package consumer

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

// PUSH DEVICE REGISTRY — POST /consumer/push/register, the contract the
// consumer app already calls after the member grants notification permission
// (HANDOFF-FRONTEND-PHASE2-2026-09-18 §3).
//
// WHAT THIS IS AND IS NOT. This stores the device token. It does NOT send
// anything: sending needs an FCM/APNs sender and a Firebase project that does
// not exist yet. Until then a member whose app is closed still gets nothing —
// the app's own in-app feed remains the only notification layer.
//
// It is still worth having now, for two reasons. The app posts here today and
// swallows the 404, so every token a member grants is thrown away; and when the
// sender does land it needs a populated registry from day one rather than
// waiting for every member to reopen the app.
//
// One row per (consumer, token). A token moves between accounts when a phone is
// handed on, so the token is the identity and the newest owner wins — sending
// to the previous owner's account would deliver one member's order news to
// another member's phone.
const collPushDevices = "consumer_push_devices"

type pushDevice struct {
	MongoID    primitive.ObjectID `bson:"_id,omitempty"      json:"-"`
	Token      string             `bson:"token"              json:"-"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"        json:"-"`
	Platform   string             `bson:"platform"           json:"platform"`
	Provider   string             `bson:"provider"           json:"provider"`
	AppVersion string             `bson:"app_version,omitempty" json:"app_version,omitempty"`
	CreatedAt  time.Time          `bson:"created_at"         json:"created_at"`
	UpdatedAt  time.Time          `bson:"updated_at"         json:"updated_at"`
}

func (r *repository) pushDevices() *mongo.Collection {
	return r.accounts.Database().Collection(collPushDevices)
}

func (r *repository) ensurePushIndexes(ctx context.Context) error {
	_, err := r.pushDevices().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "consumer_id", Value: 1}}},
	})
	return err
}

type pushRegisterInput struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	Provider   string `json:"provider"`
	AppVersion string `json:"app_version"`
}

func (s *service) registerPushDevice(ctx context.Context, consumerID primitive.ObjectID, in pushRegisterInput) error {
	token := strings.TrimSpace(in.Token)
	if token == "" || len(token) > 512 {
		return errBadRequest("a device token is required")
	}
	platform := strings.ToLower(strings.TrimSpace(in.Platform))
	switch platform {
	case "android", "ios", "web":
	default:
		platform = "unknown"
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider == "" {
		provider = "expo"
	}
	now := time.Now().UTC()
	_, err := s.repo.pushDevices().UpdateOne(ctx,
		bson.D{{Key: "token", Value: token}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "consumer_id", Value: consumerID}, {Key: "platform", Value: platform},
				{Key: "provider", Value: provider}, {Key: "app_version", Value: strings.TrimSpace(in.AppVersion)},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
		},
		options.Update().SetUpsert(true))
	if err != nil {
		return errInternal("device registration failed")
	}
	return nil
}

// unregisterPushDevice drops ONE token when it belongs to the caller: the
// sign-out leg (contract C2), so a shared phone stops carrying the previous
// member's order news. Idempotent: a token that is absent, or that has since
// moved to another account, is not an error and is left to its newest owner.
func (s *service) unregisterPushDevice(ctx context.Context, consumerID primitive.ObjectID, token string) error {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 512 {
		return errBadRequest("a device token is required")
	}
	if _, err := s.repo.pushDevices().DeleteOne(ctx,
		bson.D{{Key: "token", Value: token}, {Key: "consumer_id", Value: consumerID}}); err != nil {
		return errInternal("device unregistration failed")
	}
	return nil
}

// forgetPushDevices drops a consumer's tokens — called from the erasure cascade
// so a deleted account cannot keep receiving notifications on its old phone.
func (r *repository) forgetPushDevices(ctx context.Context, consumerID primitive.ObjectID) {
	_, _ = r.pushDevices().DeleteMany(ctx, bson.D{{Key: "consumer_id", Value: consumerID}})
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
	// Tell the app plainly that the token is stored but nothing can be sent yet,
	// so "push is silent" is a documented state rather than a mystery.
	writeJSON(w, http.StatusOK, map[string]any{"registered": true, "delivery": "pending_sender"})
}

// DELETE /consumer/push/register {"token"} -> 204 (contract C2). 204 also
// when the row is absent; the app calls this before it clears the session.
func (h *handler) unregisterPushDevice(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.unregisterPushDevice(r.Context(), id, in.Token); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
