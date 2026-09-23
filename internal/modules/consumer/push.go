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

// PUSH DEVICE REGISTRY - POST and DELETE /consumer/push/register, the
// contract the consumer app calls after the member grants notification
// permission (HANDOFF-FRONTEND-PHASE2 s3, pyaas-consumer lib/notifications.ts).
//
// The app asks for permission ONLY from its notifications screen, after a
// primer, never at launch, and on a grant it posts the Expo push token here.
// This file stores it. Sending is the CRM's job: the Expo transport in
// crm_push.go reads these rows and is inert until EXPO_PUSH_ENABLED=true, so
// a member whose app is closed hears nothing until that switch is on.
// Storing the token first is what lets the sender go live without asking
// every member to re-grant.
//
// ONE ROW PER DEVICE, keyed by the token itself. A token is already unique
// per install; keying on it means a reinstall adds a row and a re-grant
// updates one rather than accumulating duplicates that would each get a
// copy of every push.
//
// A token can MOVE between accounts - a shared family handset where one
// member signs out and another signs in. The consumer id is therefore
// overwritten on every upsert, so the device belongs to whoever registered
// it last. Anything else would push one member's wallet balance to another
// member's phone. Sign-out unbinds the token explicitly (contract C2).
const collPushDevices = "consumer_push_devices"

type pushDevice struct {
	MongoID    primitive.ObjectID `bson:"_id,omitempty"      json:"-"`
	Token      string             `bson:"token"              json:"-"`
	ConsumerID primitive.ObjectID `bson:"consumer_id"        json:"-"`
	Platform   string             `bson:"platform"           json:"platform"` // ios | android | web
	Provider   string             `bson:"provider"           json:"provider"` // expo
	AppVersion string             `bson:"app_version,omitempty" json:"app_version,omitempty"`
	CreatedAt  time.Time          `bson:"created_at"         json:"created_at"`
	UpdatedAt  time.Time          `bson:"updated_at"         json:"updated_at"`
}

// pushPlatforms is the closed set the app can send (React Native's
// Platform.OS on a phone, plus the Expo web target). Anything else is refused
// rather than stored under a made-up value.
var pushPlatforms = map[string]bool{"ios": true, "android": true, "web": true}

func (r *repository) pushDevices() *mongo.Collection {
	return r.accounts.Database().Collection(collPushDevices)
}

func (r *repository) ensurePushIndexes(ctx context.Context) error {
	_, err := r.pushDevices().Indexes().CreateMany(ctx, []mongo.IndexModel{
		// The token IS the device - unique, so a re-grant updates in place.
		{Keys: bson.D{{Key: "token", Value: 1}}, Options: options.Index().SetUnique(true)},
		// The sender's query: every live device for one member.
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
	if !pushPlatforms[platform] {
		return errBadRequest("platform must be ios, android or web")
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
				{Key: "consumer_id", Value: consumerID}, // the device follows the current signer-in
				{Key: "platform", Value: platform},
				{Key: "provider", Value: provider},
				{Key: "app_version", Value: strings.TrimSpace(in.AppVersion)},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
		},
		options.Update().SetUpsert(true))
	if err != nil {
		return errInternal("could not register the device")
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
		return errBadRequest("a push token is required")
	}
	if _, err := s.repo.pushDevices().DeleteOne(ctx,
		bson.D{{Key: "token", Value: token}, {Key: "consumer_id", Value: consumerID}}); err != nil {
		return errInternal("could not unregister the device")
	}
	return nil
}

// pushDevicesFor lists a member's live devices - the storage-side seam any
// sender reads (crm_push.go expoPushTokens narrows the same rows to the Expo
// provider). Erasure empties it through deleteAccountCascade.
func (s *service) pushDevicesFor(ctx context.Context, consumerID primitive.ObjectID) ([]pushDevice, error) {
	cur, err := s.repo.pushDevices().Find(ctx, bson.D{{Key: "consumer_id", Value: consumerID}})
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
	// Say plainly whether anything can be sent to this token yet, so "push is
	// silent" is a documented state rather than a mystery: "expo" once the
	// CRM transport is switched on, "pending_sender" until then.
	delivery := "pending_sender"
	if h.svc.crmPush.Enabled() {
		delivery = "expo"
	}
	writeJSON(w, http.StatusOK, map[string]any{"registered": true, "status": "registered", "delivery": delivery})
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
