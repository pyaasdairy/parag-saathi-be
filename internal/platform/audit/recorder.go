// Package audit writes the immutable who-did-what trail required for
// government accountability and DPDP breach forensics (blueprint §12, §16).
// Entries are insert-only; the STATE_AUDITOR role reads them via the
// platformops module.
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
)

const collection = "audit_logs"

// Entry is one audit record.
type Entry struct {
	ID           string         `bson:"_id"            json:"id"`
	ActorPartyID string         `bson:"actor_party_id" json:"actor_party_id"`
	ActorRole    string         `bson:"actor_role,omitempty" json:"actor_role,omitempty"`
	Action       string         `bson:"action"         json:"action"` // e.g. "http.POST /api/v1/collection/pours" or "support.pii_lookup"
	TargetType   string         `bson:"target_type,omitempty" json:"target_type,omitempty"`
	TargetID     string         `bson:"target_id,omitempty"   json:"target_id,omitempty"`
	Status       int            `bson:"status,omitempty"      json:"status,omitempty"`
	IP           string         `bson:"ip,omitempty"          json:"ip,omitempty"`
	RequestID    string         `bson:"request_id,omitempty"  json:"request_id,omitempty"`
	Meta         map[string]any `bson:"meta,omitempty"        json:"meta,omitempty"`
	TS           time.Time      `bson:"ts"             json:"ts"`
}

// Recorder persists audit entries asynchronously so auditing never adds
// latency to the request path.
type Recorder struct {
	coll *mongo.Collection
	log  *slog.Logger
}

// NewRecorder binds the recorder to the database.
func NewRecorder(db *mongo.Database, log *slog.Logger) *Recorder {
	return &Recorder{coll: db.Collection(collection), log: log}
}

// Record fills identity fields from the request context and persists the
// entry in the background (fire-and-forget with its own timeout).
func (r *Recorder) Record(ctx context.Context, e Entry) {
	if a, ok := auth.ActorFrom(ctx); ok {
		if e.ActorPartyID == "" {
			e.ActorPartyID = a.PartyID
		}
		if e.ActorRole == "" {
			e.ActorRole = a.RoleCode
		}
	}
	e.ID = uuid.NewString()
	e.TS = time.Now().UTC()

	go func() {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.coll.InsertOne(writeCtx, e); err != nil {
			r.log.Error("audit write failed", slog.String("action", e.Action), slog.Any("err", err))
		}
	}()
}
