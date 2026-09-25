package push

// OPERATOR PUSH: the Saathi app's device registry and the fan-out that sends
// store alerts to it over FCM (fcm.go).
//
// The consumer app's registry (consumer_push_devices, POST
// /consumer/push/register) cannot be reused: it is keyed on a consumer id
// under the consumer JWT, while a store manager or rider is an operator PARTY
// signed in with an operator token, and consumer tokens are Expo tokens sent
// through Expo. So operators get their own collection, filled by POST / DELETE
// /api/v1/push/register (platformops), and read here by party id. It lives in
// this platform package because the module that mounts the routes
// (platformops) and the module that raises the alerts (consumer) never import
// each other.
//
// ONE ROW PER DEVICE, keyed by the FCM token. A shared field handset changes
// hands between shifts, so the party id is overwritten on every register:
// the device belongs to whoever signed in on it last, and sign-out unbinds it.
//
// QUIET HOURS (founder decision 6): 22:00-07:00 IST no alert pushes except an
// order or delivery alert (Urgent). A held alert is not queued for the
// morning: every store alert that has an inbox row keeps it (the Saathi bell
// shows it on the next open), so holding only skips the ring.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CollOperatorPushDevices holds one row per Saathi device.
const CollOperatorPushDevices = "operator_push_devices"

// maxDevicesPerParty caps how many of one party's devices get a copy: the
// newest registrations, so a handset lost months ago stops mattering.
const maxDevicesPerParty = 5

var istZone = time.FixedZone("IST", 5*3600+30*60)

// InQuietHours reports whether t falls in 22:00-07:00 IST.
func InQuietHours(t time.Time) bool {
	h := t.In(istZone).Hour()
	return h >= 22 || h < 7
}

// ErrBadDevice is a register or unregister request the registry refuses; the
// message is safe to show the caller.
type ErrBadDevice struct{ Reason string }

func (e ErrBadDevice) Error() string { return e.Reason }

// OperatorDevice is one registered Saathi device.
type OperatorDevice struct {
	Token      string             `bson:"token"`
	PartyID    primitive.ObjectID `bson:"party_id"`
	Platform   string             `bson:"platform"` // android | ios
	Provider   string             `bson:"provider"` // fcm
	AppVersion string             `bson:"app_version,omitempty"`
	RoleCode   string             `bson:"role_code,omitempty"` // the role signed in when it registered (informational)
	CreatedAt  time.Time          `bson:"created_at"`
	UpdatedAt  time.Time          `bson:"updated_at"`
}

// RegisterInput is the POST /push/register body.
type RegisterInput struct {
	Token      string `json:"token"`
	Platform   string `json:"platform"`
	Provider   string `json:"provider"`
	AppVersion string `json:"app_version"`
}

var operatorPlatforms = map[string]bool{"android": true, "ios": true}

// OperatorRegistry is the operator_push_devices collection.
type OperatorRegistry struct{ coll *mongo.Collection }

// NewOperatorRegistry binds the registry to a database.
func NewOperatorRegistry(db *mongo.Database) *OperatorRegistry {
	return &OperatorRegistry{coll: db.Collection(CollOperatorPushDevices)}
}

// EnsureIndexes builds the unique token index (one row per device) and the
// sender's party lookup.
func (r *OperatorRegistry) EnsureIndexes(ctx context.Context) error {
	_, err := r.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token", Value: 1}}, Options: options.Index().SetUnique(true).SetName("uniq_token")},
		{Keys: bson.D{{Key: "party_id", Value: 1}, {Key: "updated_at", Value: -1}}, Options: options.Index().SetName("party_updated")},
	})
	return err
}

func cleanToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrBadDevice{"a push token is required"}
	}
	if len(token) > 4096 {
		return "", ErrBadDevice{"push token is implausibly long"}
	}
	return token, nil
}

// Register upserts one device for the party signed in on it. Idempotent: the
// app calls it on every launch and token refresh.
func (r *OperatorRegistry) Register(ctx context.Context, partyID primitive.ObjectID, roleCode string, in RegisterInput, now time.Time) error {
	token, err := cleanToken(in.Token)
	if err != nil {
		return err
	}
	platform := strings.ToLower(strings.TrimSpace(in.Platform))
	if !operatorPlatforms[platform] {
		return ErrBadDevice{"platform must be android or ios"}
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider == "" {
		provider = "fcm"
	}
	if provider != "fcm" {
		return ErrBadDevice{"provider must be fcm"}
	}
	appVersion := strings.TrimSpace(in.AppVersion)
	if len(appVersion) > 64 {
		appVersion = appVersion[:64]
	}
	_, err = r.coll.UpdateOne(ctx,
		bson.D{{Key: "token", Value: token}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "party_id", Value: partyID}, // the device follows the current signer-in
				{Key: "platform", Value: platform},
				{Key: "provider", Value: provider},
				{Key: "app_version", Value: appVersion},
				{Key: "role_code", Value: roleCode},
				{Key: "updated_at", Value: now.UTC()},
			}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now.UTC()}}},
		},
		options.Update().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		// Two first registrations of one token raced; the loser retries as an update.
		_, err = r.coll.UpdateOne(ctx, bson.D{{Key: "token", Value: token}}, bson.D{{Key: "$set", Value: bson.D{
			{Key: "party_id", Value: partyID}, {Key: "platform", Value: platform}, {Key: "provider", Value: provider},
			{Key: "app_version", Value: appVersion}, {Key: "role_code", Value: roleCode}, {Key: "updated_at", Value: now.UTC()},
		}}})
	}
	if err != nil {
		return fmt.Errorf("register operator device: %w", err)
	}
	return nil
}

// Unregister drops one token when it belongs to the party: the sign-out leg.
// Idempotent; a token that has since moved to another party is left to them.
func (r *OperatorRegistry) Unregister(ctx context.Context, partyID primitive.ObjectID, token string) error {
	token, err := cleanToken(token)
	if err != nil {
		return err
	}
	if _, err := r.coll.DeleteOne(ctx, bson.D{{Key: "token", Value: token}, {Key: "party_id", Value: partyID}}); err != nil {
		return fmt.Errorf("unregister operator device: %w", err)
	}
	return nil
}

// TokensFor lists the live tokens of the given parties, distinct, newest
// registrations first, at most maxDevicesPerParty each.
func (r *OperatorRegistry) TokensFor(ctx context.Context, parties []primitive.ObjectID) ([]string, error) {
	if len(parties) == 0 {
		return nil, nil
	}
	ids := bson.A{}
	for _, p := range parties {
		ids = append(ids, p)
	}
	cur, err := r.coll.Find(ctx, bson.D{{Key: "party_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}).SetLimit(int64(len(parties)*maxDevicesPerParty*2)))
	if err != nil {
		return nil, fmt.Errorf("find operator devices: %w", err)
	}
	var rows []OperatorDevice
	if err := cur.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("decode operator devices: %w", err)
	}
	perParty := map[primitive.ObjectID]int{}
	seen := map[string]bool{}
	out := make([]string, 0, len(rows))
	for _, d := range rows {
		if seen[d.Token] || perParty[d.PartyID] >= maxDevicesPerParty {
			continue
		}
		seen[d.Token] = true
		perParty[d.PartyID]++
		out = append(out, d.Token)
	}
	return out, nil
}

// Prune drops a token FCM reported UNREGISTERED.
func (r *OperatorRegistry) Prune(ctx context.Context, token string) error {
	_, err := r.coll.DeleteOne(ctx, bson.D{{Key: "token", Value: token}})
	return err
}

// DeviceStore is what the fan-out needs from the registry
// (*OperatorRegistry in production).
type DeviceStore interface {
	TokensFor(ctx context.Context, parties []primitive.ObjectID) ([]string, error)
	Prune(ctx context.Context, token string) error
}

// Sender is what the fan-out needs from FCM (*FCM in production).
type Sender interface {
	Enabled() bool
	Send(ctx context.Context, m FCMMessage) (string, error)
}

// OperatorAlert is one store alert for operator phones.
type OperatorAlert struct {
	Kind  string // the inbox template key (STORE_INSTANT_CLOSING, ...) or STORE_INSTANT_ORDER
	Title string
	Body  string
	Data  map[string]string // for the app's tap handler; "type" is set to Kind
	// Urgent marks an order or delivery alert: it rings even in quiet hours.
	Urgent bool
	// CollapseKey lets a newer alert replace an older one on the device.
	CollapseKey string
}

// NotifyResult says what one fan-out did.
type NotifyResult struct {
	Devices   int  // tokens found for the recipients
	Sent      int  // accepted by FCM
	Pruned    int  // dead tokens dropped
	Failed    int  // other failures (the token stays)
	HeldQuiet bool // quiet hours: nothing was sent
}

// Operator is the operator fan-out: registry + FCM.
type Operator struct {
	devices DeviceStore
	fcm     Sender
	status  string
}

// NewOperator wires the fan-out. A nil or inert FCM leaves it inert.
func NewOperator(reg *OperatorRegistry, fcm *FCM) *Operator {
	o := &Operator{status: fcm.Status()}
	if reg != nil {
		o.devices = reg
	}
	if fcm != nil {
		o.fcm = fcm
	}
	return o
}

// NewOperatorWith is the test seam: any device store and sender (a module's
// test records what would have gone out without a Firebase project).
func NewOperatorWith(devices DeviceStore, sender Sender) *Operator {
	return &Operator{devices: devices, fcm: sender, status: "operator push (Saathi): test sender"}
}

// Enabled reports whether anything can be sent.
func (o *Operator) Enabled() bool {
	return o != nil && o.devices != nil && o.fcm != nil && o.fcm.Enabled()
}

// Status is the boot line.
func (o *Operator) Status() string {
	if o == nil {
		return "operator push (Saathi): inert - no sender"
	}
	return o.status
}

// LogStatus writes the boot line: Info when live or simply unconfigured,
// Warn when a service account is set but unusable.
func (o *Operator) LogStatus(log *slog.Logger) {
	if log == nil {
		return
	}
	s := o.Status()
	if !o.Enabled() && strings.Contains(s, "unreadable") {
		log.Warn(s)
		return
	}
	log.Info(s)
}

// Notify sends one alert to every registered device of the given parties.
// now is the caller's clock (quiet hours are judged on it). Per-device
// failures never abort the rest; a dead token is pruned.
func (o *Operator) Notify(ctx context.Context, now time.Time, parties []primitive.ObjectID, a OperatorAlert) (NotifyResult, error) {
	var res NotifyResult
	if !o.Enabled() || len(parties) == 0 {
		return res, nil
	}
	if !a.Urgent && InQuietHours(now) {
		res.HeldQuiet = true
		return res, nil
	}
	tokens, err := o.devices.TokensFor(ctx, parties)
	if err != nil {
		return res, err
	}
	res.Devices = len(tokens)
	data := map[string]string{}
	for k, v := range a.Data {
		data[k] = v
	}
	if a.Kind != "" {
		data["type"] = a.Kind
	}
	channel := "store_alerts"
	if a.Urgent {
		channel = "store_orders"
	}
	var lastErr error
	for _, tok := range tokens {
		_, err := o.fcm.Send(ctx, FCMMessage{
			Token: tok, Title: a.Title, Body: a.Body, Data: data,
			AndroidChannelID: channel, Urgent: a.Urgent, CollapseKey: a.CollapseKey,
		})
		switch {
		case err == nil:
			res.Sent++
		case errors.Is(err, ErrUnregistered):
			if perr := o.devices.Prune(ctx, tok); perr == nil {
				res.Pruned++
			} else {
				res.Failed++
				lastErr = perr
			}
		default:
			res.Failed++
			lastErr = err
		}
	}
	if res.Sent == 0 && res.Failed > 0 {
		return res, lastErr
	}
	return res, nil
}
