// Package provenance implements the append-only, hash-chained event ledger —
// the source of truth for milk traceability (blueprint §7). Every link in the
// pour→QR chain is an immutable Event whose SHA-256 hash covers the previous
// event's hash, so history cannot be quietly rewritten (tamper-evidence,
// FSSAI recall-grade audit).
//
// Corrections are NEW events referencing the superseded entity — nothing is
// ever updated or deleted in this collection.
package provenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collection = "provenance_events"

// Ref is a directed edge to another entity in the trace graph
// (e.g. a consignment event referencing the pours it aggregates).
type Ref struct {
	EntityType string `bson:"entity_type" json:"entity_type"`
	EntityID   string `bson:"entity_id"   json:"entity_id"`
	Relation   string `bson:"relation"    json:"relation"` // aggregates | supersedes | tested | yields | labels ...
}

// ActorRef records who caused the event.
type ActorRef struct {
	PartyID  string `bson:"party_id"  json:"party_id"`
	RoleCode string `bson:"role_code" json:"role_code"`
}

// Event is one immutable link in the ledger.
type Event struct {
	ID         string         `bson:"_id"         json:"id"`
	Seq        int64          `bson:"seq"         json:"seq"`
	Type       string         `bson:"type"        json:"type"`
	EntityType string         `bson:"entity_type" json:"entity_type"`
	EntityID   string         `bson:"entity_id"   json:"entity_id"`
	Refs       []Ref          `bson:"refs,omitempty"    json:"refs,omitempty"`
	Actor      ActorRef       `bson:"actor"       json:"actor"`
	OrgUnitID  string         `bson:"org_unit_id,omitempty" json:"org_unit_id,omitempty"`
	Payload    map[string]any `bson:"payload,omitempty"     json:"payload,omitempty"`
	TS         time.Time      `bson:"ts"          json:"ts"`
	PrevHash   string         `bson:"prev_hash"   json:"prev_hash"`
	Hash       string         `bson:"hash"        json:"hash"`
}

// AppendInput describes a new event to chain.
type AppendInput struct {
	Type       string
	EntityType string
	EntityID   string
	Refs       []Ref
	Actor      ActorRef
	OrgUnitID  string
	Payload    map[string]any
}

// Ledger appends to and reads the hash chain.
type Ledger struct {
	coll *mongo.Collection

	// appendMu serialises the head-read+insert of Append within this process:
	// every ledger write system-wide shares one global seq, so without local
	// serialisation concurrent handlers (e.g. a morning pour rush plus a
	// GenerateInvoices loop) burn duplicate-key retries against each other.
	// The unique seq index remains the atomic backstop across processes.
	appendMu sync.Mutex
}

// NewLedger binds the ledger to the database.
func NewLedger(db *mongo.Database) *Ledger {
	return &Ledger{coll: db.Collection(collection)}
}

// genesisHash anchors the chain before the first event.
const genesisHash = "GENESIS"

// Append chains a new event. Concurrency-safe without transactions: it reads
// the current head, computes seq+hash optimistically, and relies on the
// unique index on `seq` — a duplicate-key collision means another writer won
// the slot, so we re-read and retry.
func (l *Ledger) Append(ctx context.Context, in AppendInput) (*Event, error) {
	// Serialise appends within this process (see appendMu): local writers no
	// longer race each other for the seq slot, so under single-instance
	// deployment the retry loop only ever fires against external writers.
	l.appendMu.Lock()
	defer l.appendMu.Unlock()

	const maxRetries = 12
	for attempt := 0; attempt < maxRetries; attempt++ {
		seq, prevHash, err := l.head(ctx)
		if err != nil {
			return nil, err
		}
		ev := &Event{
			ID:         uuid.NewString(),
			Seq:        seq + 1,
			Type:       in.Type,
			EntityType: in.EntityType,
			EntityID:   in.EntityID,
			Refs:       in.Refs,
			Actor:      in.Actor,
			OrgUnitID:  in.OrgUnitID,
			Payload:    in.Payload,
			// Millisecond precision: BSON datetimes store ms, so the persisted
			// TS must equal the hashed TS or re-verification would break.
			TS:       time.Now().UTC().Truncate(time.Millisecond),
			PrevHash: prevHash,
		}
		ev.Hash = computeHash(ev)

		if _, err := l.coll.InsertOne(ctx, ev); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				continue // lost the seq race — retry against the new head
			}
			return nil, fmt.Errorf("provenance append: %w", err)
		}
		return ev, nil
	}
	return nil, fmt.Errorf("provenance append: gave up after %d seq collisions", 12)
}

// head returns the latest (seq, hash), or (0, GENESIS) for an empty chain.
func (l *Ledger) head(ctx context.Context) (int64, string, error) {
	var last Event
	err := l.coll.FindOne(ctx, bson.D{},
		options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}}),
	).Decode(&last)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, genesisHash, nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("provenance head: %w", err)
	}
	return last.Seq, last.Hash, nil
}

// computeHash covers everything that matters: position, linkage, content.
// Payload is canonicalised via encoding/json (map keys are sorted) and the
// timestamp to millisecond precision (BSON datetime resolution), so the
// digest is deterministic across a MongoDB round trip.
func computeHash(ev *Event) string {
	payloadJSON, _ := json.Marshal(ev.Payload)
	refParts := make([]string, 0, len(ev.Refs))
	for _, r := range ev.Refs {
		refParts = append(refParts, r.EntityType+":"+r.EntityID+":"+r.Relation)
	}
	material := strings.Join([]string{
		ev.PrevHash,
		strconv.FormatInt(ev.Seq, 10),
		ev.Type,
		ev.EntityType,
		ev.EntityID,
		strings.Join(refParts, ","),
		ev.Actor.PartyID,
		ev.Actor.RoleCode,
		ev.OrgUnitID,
		string(payloadJSON),
		ev.TS.UTC().Truncate(time.Millisecond).Format(time.RFC3339Nano),
	}, "|")
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// EventsForEntity returns an entity's own timeline in chain order.
func (l *Ledger) EventsForEntity(ctx context.Context, entityType, entityID string) ([]Event, error) {
	cur, err := l.coll.Find(ctx,
		bson.D{{Key: "entity_type", Value: entityType}, {Key: "entity_id", Value: entityID}},
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("provenance events for %s/%s: %w", entityType, entityID, err)
	}
	var out []Event
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Trace walks the provenance graph UPSTREAM from an entity: its events, plus
// (recursively) the events of everything those events reference. This is how
// a consumer QR resolves back to the contributing samiti set, and how a failed
// aflatoxin test walks back to source DCSes (blueprint §8.3).
func (l *Ledger) Trace(ctx context.Context, entityType, entityID string, maxDepth int) ([]Event, error) {
	return l.TraceStopAt(ctx, entityType, entityID, maxDepth, nil)
}

// TraceStopAt is Trace with a stop set: entity types in stopExpand are still
// fetched (their events appear in the result) but their refs are never
// followed. The consumer QR scan stops at the consignment/pooling boundary so
// hundreds of per-pour nodes (and cross-route trip fan-out) are never walked.
//
// Each BFS level is batched into a bounded number of $or queries instead of
// one Find per node, so a walk costs O(depth) round trips, not O(nodes).
func (l *Ledger) TraceStopAt(ctx context.Context, entityType, entityID string, maxDepth int, stopExpand map[string]bool) ([]Event, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	const hardEventCap = 2000

	type node struct{ etype, eid string }
	visited := map[node]bool{}
	frontier := []node{{entityType, entityID}}
	var all []Event

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		// Deduplicate this level against everything already visited.
		level := make([]node, 0, len(frontier))
		for _, n := range frontier {
			if visited[n] {
				continue
			}
			visited[n] = true
			level = append(level, n)
		}
		if len(level) == 0 {
			break
		}

		var next []node
		// One indexed query per chunk of the level (not per node).
		const orChunk = 200
		for start := 0; start < len(level); start += orChunk {
			end := min(start+orChunk, len(level))
			clauses := make(bson.A, 0, end-start)
			for _, n := range level[start:end] {
				clauses = append(clauses, bson.D{
					{Key: "entity_type", Value: n.etype},
					{Key: "entity_id", Value: n.eid},
				})
			}
			cur, err := l.coll.Find(ctx,
				bson.D{{Key: "$or", Value: clauses}},
				options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetLimit(hardEventCap),
			)
			if err != nil {
				return nil, fmt.Errorf("provenance trace level: %w", err)
			}
			var evs []Event
			if err := cur.All(ctx, &evs); err != nil {
				return nil, err
			}
			for _, ev := range evs {
				all = append(all, ev)
				if len(all) >= hardEventCap {
					return all, nil
				}
				if stopExpand != nil && stopExpand[ev.EntityType] {
					continue // include the node's events, never follow its refs
				}
				for _, ref := range ev.Refs {
					nn := node{ref.EntityType, ref.EntityID}
					if !visited[nn] {
						next = append(next, nn)
					}
				}
			}
		}
		frontier = next
	}
	return all, nil
}

// DownstreamRefs finds events that reference the given entity — used to walk
// FORWARD (e.g. "which batches consumed this blocked BMC lot?"). The limit is
// pushed into the query: only the LAST limit referencing events are fetched
// (ascending chain order preserved). limit <= 0 falls back to 2000.
func (l *Ledger) DownstreamRefs(ctx context.Context, entityID string, limit int64) ([]Event, error) {
	if limit <= 0 {
		limit = 2000
	}
	cur, err := l.coll.Find(ctx,
		bson.D{{Key: "refs.entity_id", Value: entityID}},
		options.Find().SetSort(bson.D{{Key: "seq", Value: -1}}).SetLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("provenance downstream of %s: %w", entityID, err)
	}
	var out []Event
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	// Restore ascending chain order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// EventsForEntityPage returns one page of an entity's own timeline in chain
// order, with skip/limit pushed into the query.
func (l *Ledger) EventsForEntityPage(ctx context.Context, entityType, entityID string, offset, limit int64) ([]Event, error) {
	cur, err := l.coll.Find(ctx,
		bson.D{{Key: "entity_type", Value: entityType}, {Key: "entity_id", Value: entityID}},
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetSkip(offset).SetLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("provenance events page for %s/%s: %w", entityType, entityID, err)
	}
	var out []Event
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CountEventsForEntity returns the total number of events on an entity's
// timeline (pagination meta companion of EventsForEntityPage).
func (l *Ledger) CountEventsForEntity(ctx context.Context, entityType, entityID string) (int64, error) {
	n, err := l.coll.CountDocuments(ctx,
		bson.D{{Key: "entity_type", Value: entityType}, {Key: "entity_id", Value: entityID}})
	if err != nil {
		return 0, fmt.Errorf("provenance count for %s/%s: %w", entityType, entityID, err)
	}
	return n, nil
}

// VerifyChain re-derives every hash in [fromSeq, toSeq] and checks linkage.
// Returns (true, 0) when intact, or (false, seq) of the first broken link.
func (l *Ledger) VerifyChain(ctx context.Context, fromSeq, toSeq int64) (bool, int64, error) {
	filter := bson.D{{Key: "seq", Value: bson.D{{Key: "$gte", Value: fromSeq}, {Key: "$lte", Value: toSeq}}}}
	cur, err := l.coll.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
	if err != nil {
		return false, 0, fmt.Errorf("provenance verify: %w", err)
	}
	defer cur.Close(ctx)

	prevHash := ""
	prevSeq := int64(0)
	first := true
	for cur.Next(ctx) {
		var ev Event
		if err := cur.Decode(&ev); err != nil {
			return false, 0, err
		}
		if computeHash(&ev) != ev.Hash {
			return false, ev.Seq, nil // content tampered
		}
		if !first {
			if ev.Seq != prevSeq+1 || ev.PrevHash != prevHash {
				return false, ev.Seq, nil // linkage broken
			}
		}
		prevHash, prevSeq, first = ev.Hash, ev.Seq, false
	}
	return true, 0, cur.Err()
}

// LatestSeq returns the current chain head sequence (0 = empty).
func (l *Ledger) LatestSeq(ctx context.Context) (int64, error) {
	seq, _, err := l.head(ctx)
	return seq, err
}
