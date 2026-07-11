package mongodb

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Collection names — single source of truth so modules never typo a name.
const (
	CollParties          = "parties"
	CollOTPChallenges    = "otp_challenges"
	CollRefreshTokens    = "refresh_tokens"
	CollRoleAssignments  = "role_assignments"
	CollOrgUnits         = "org_units"
	CollKYCRecords       = "kyc_records"
	CollConsents         = "consents"
	CollAnimals          = "animals"
	CollHealthEvents     = "health_events"
	CollMVUCases         = "mvu_cases"
	CollEducation        = "education_content"
	CollRateCharts       = "rate_charts"
	CollAnalyzerReadings = "analyzer_readings"
	CollMilkPours        = "milk_pours"
	CollInvoices         = "invoices"
	CollConsignments     = "dcs_consignments"
	CollRouteTrips       = "route_trips"
	CollBMCLots          = "bmc_lots"
	CollBatches          = "processing_batches"
	CollQCResults        = "qc_results"
	CollProductLots      = "product_lots"
	CollBatchQRs         = "batch_qrs"
	CollSettlements      = "settlement_batches"
	CollPayouts          = "payout_instructions"
	CollDBTRequests      = "dbt_requests"
	CollNotifications    = "notifications"
	CollProvenance       = "provenance_events"
	CollAuditLogs        = "audit_logs"
	CollFeatureFlags     = "feature_flags"
	CollCounters         = "counters"
	CollProducts         = "products"
	CollOnboarding       = "onboarding_requests"
	CollCMSContent       = "cms_content"
	CollQCCertificates   = "qc_certificates"
	CollSettings         = "app_settings"
	// Per-samiti batch flow (F7/F8): the batch QC results and the auto-minted
	// public batch QRs (distinct from product-lot batch_qrs).
	CollConsignmentQC       = "consignment_qc"
	CollConsignmentBatchQRs = "consignment_batch_qrs"
)

// EnsureIndexes creates every index the query paths rely on. Idempotent —
// safe to run on every boot. Each hot read path below has a covering or
// prefix index so no collection scan sits on a user-facing request.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	asc, desc := int32(1), int32(-1)
	_ = desc

	specs := map[string][]mongo.IndexModel{
		CollParties: {
			idx(bson.D{{Key: "phone", Value: asc}}, options.Index().SetUnique(true)),
			// People directory: newest-first paging (GET /support/parties).
			idx(bson.D{{Key: "created_at", Value: desc}}, nil),
		},
		CollOTPChallenges: {
			idx(bson.D{{Key: "phone", Value: asc}}, nil),
			idx(bson.D{{Key: "expires_at", Value: asc}}, options.Index().SetExpireAfterSeconds(0)),
		},
		CollRefreshTokens: {
			idx(bson.D{{Key: "token_hash", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "party_id", Value: asc}}, nil),
			idx(bson.D{{Key: "expires_at", Value: asc}}, options.Index().SetExpireAfterSeconds(0)),
		},
		CollRoleAssignments: {
			idx(bson.D{{Key: "party_id", Value: asc}, {Key: "status", Value: asc}}, nil),
			idx(bson.D{{Key: "org_unit_id", Value: asc}, {Key: "role_code", Value: asc}, {Key: "status", Value: asc}}, nil),
		},
		CollOrgUnits: {
			idx(bson.D{{Key: "code", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "parent_id", Value: asc}}, nil),
			idx(bson.D{{Key: "type", Value: asc}}, nil),
			idx(bson.D{{Key: "path", Value: asc}}, nil),
		},
		CollKYCRecords: {
			idx(bson.D{{Key: "party_id", Value: asc}, {Key: "status", Value: asc}}, nil),
		},
		CollConsents: {
			idx(bson.D{{Key: "party_id", Value: asc}, {Key: "purpose", Value: asc}}, nil),
		},
		CollAnimals: {
			idx(bson.D{{Key: "pashu_aadhaar", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "owner_party_id", Value: asc}}, nil),
			idx(bson.D{{Key: "dcs_id", Value: asc}}, nil),
		},
		CollHealthEvents: {
			idx(bson.D{{Key: "animal_id", Value: asc}, {Key: "occurred_at", Value: desc}}, nil),
			idx(bson.D{{Key: "bp_sync_status", Value: asc}}, nil),
		},
		CollMVUCases: {
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "status", Value: asc}}, nil),
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "requested_at", Value: desc}}, nil),
		},
		CollEducation: {
			idx(bson.D{{Key: "topic", Value: asc}, {Key: "language", Value: asc}, {Key: "published", Value: asc}}, nil),
		},
		CollRateCharts: {
			idx(bson.D{{Key: "org_unit_id", Value: asc}, {Key: "active", Value: asc}, {Key: "effective_from", Value: desc}}, nil),
		},
		CollAnalyzerReadings: {
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "created_at", Value: desc}}, nil),
		},
		CollMilkPours: {
			// Offline-first idempotency: device-generated event IDs are unique.
			idx(bson.D{{Key: "client_event_id", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "pour_date", Value: asc}, {Key: "shift", Value: asc}}, nil),
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "pour_date", Value: desc}}, nil),
			// Sort-covering indexes for the pour listings (poured_at sort).
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "poured_at", Value: desc}}, nil),
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "poured_at", Value: desc}}, nil),
		},
		CollInvoices: {
			idx(bson.D{{Key: "invoice_number", Value: asc}}, options.Index().SetUnique(true)),
			// One invoice per farmer per DCS per day.
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "dcs_id", Value: asc}, {Key: "invoice_date", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "invoice_date", Value: asc}, {Key: "status", Value: asc}}, nil),
			// Settlement lifecycle reads (initiate/execute/reject) filter on
			// the claimed batch — partial: only claimed invoices carry it.
			idx(bson.D{{Key: "settlement_batch_id", Value: asc}},
				options.Index().SetPartialFilterExpression(bson.D{
					{Key: "settlement_batch_id", Value: bson.D{{Key: "$exists", Value: true}}},
				})),
			// Multikey: pour freeze checks (supersede) look up the invoice by
			// contained pour ID.
			idx(bson.D{{Key: "pour_ids", Value: asc}}, nil),
			// Sort-covering indexes for the invoice listings (issued_at sort).
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "issued_at", Value: desc}}, nil),
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "issued_at", Value: desc}}, nil),
		},
		CollConsignments: {
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "date", Value: asc}, {Key: "shift", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "status", Value: asc}, {Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "created_at", Value: desc}}, nil),
			// The per-samiti batch code minted at pickup — unique where present
			// (partial: pre-pickup consignments carry no batch_code).
			idx(bson.D{{Key: "batch_code", Value: asc}},
				options.Index().SetUnique(true).SetPartialFilterExpression(bson.D{
					{Key: "batch_code", Value: bson.D{{Key: "$exists", Value: true}}},
				})),
		},
		CollRouteTrips: {
			idx(bson.D{{Key: "van_rider_party_id", Value: asc}, {Key: "date", Value: desc}}, nil),
			idx(bson.D{{Key: "union_id", Value: asc}, {Key: "date", Value: desc}}, nil),
			idx(bson.D{{Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "union_id", Value: asc}, {Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "van_rider_party_id", Value: asc}, {Key: "created_at", Value: desc}}, nil),
		},
		CollBMCLots: {
			idx(bson.D{{Key: "bmc_id", Value: asc}, {Key: "date", Value: desc}, {Key: "shift", Value: asc}}, nil),
			// Listing sort (date desc, created_at desc), bare and per-status.
			idx(bson.D{{Key: "date", Value: desc}, {Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "status", Value: asc}, {Key: "date", Value: desc}, {Key: "created_at", Value: desc}}, nil),
		},
		CollBatches: {
			idx(bson.D{{Key: "batch_number", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "plant_id", Value: asc}, {Key: "started_at", Value: desc}}, nil),
			idx(bson.D{{Key: "status", Value: asc}}, nil),
		},
		CollQCResults: {
			idx(bson.D{{Key: "subject_type", Value: asc}, {Key: "subject_id", Value: asc}, {Key: "recorded_at", Value: desc}}, nil),
		},
		CollProductLots: {
			idx(bson.D{{Key: "batch_id", Value: asc}}, nil),
			idx(bson.D{{Key: "sku", Value: asc}, {Key: "mfg_date", Value: desc}}, nil),
		},
		CollBatchQRs: {
			idx(bson.D{{Key: "qr_code", Value: asc}}, options.Index().SetUnique(true)),
			// QR listing sorts on issued_at, optionally narrowed to one lot.
			idx(bson.D{{Key: "product_lot_id", Value: asc}, {Key: "issued_at", Value: desc}}, nil),
			idx(bson.D{{Key: "issued_at", Value: desc}}, nil),
		},
		CollSettlements: {
			idx(bson.D{{Key: "dcs_id", Value: asc}, {Key: "date", Value: desc}}, nil),
			idx(bson.D{{Key: "status", Value: asc}}, nil),
		},
		CollPayouts: {
			idx(bson.D{{Key: "settlement_batch_id", Value: asc}}, nil),
			idx(bson.D{{Key: "invoice_id", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "created_at", Value: desc}}, nil),
		},
		CollDBTRequests: {
			idx(bson.D{{Key: "farmer_party_id", Value: asc}, {Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "status", Value: asc}}, nil),
		},
		CollNotifications: {
			idx(bson.D{{Key: "status", Value: asc}, {Key: "queued_at", Value: asc}}, nil),
			idx(bson.D{{Key: "phone", Value: asc}, {Key: "queued_at", Value: desc}}, nil),
			// The party-scoped inbox read (GET /notifications/me), newest first.
			idx(bson.D{{Key: "party_id", Value: asc}, {Key: "queued_at", Value: desc}}, nil),
		},
		CollProvenance: {
			// The hash chain: strictly monotonic sequence.
			idx(bson.D{{Key: "seq", Value: asc}}, options.Index().SetUnique(true)),
			// Entity timeline + graph traversal by references. The seq suffix
			// on the refs index serves the bounded (sort seq, limit) reads.
			idx(bson.D{{Key: "entity_type", Value: asc}, {Key: "entity_id", Value: asc}, {Key: "seq", Value: asc}}, nil),
			idx(bson.D{{Key: "refs.entity_id", Value: asc}, {Key: "seq", Value: asc}}, nil),
		},
		CollAuditLogs: {
			idx(bson.D{{Key: "ts", Value: desc}}, nil),
			idx(bson.D{{Key: "actor_party_id", Value: asc}, {Key: "ts", Value: desc}}, nil),
			idx(bson.D{{Key: "target_type", Value: asc}, {Key: "target_id", Value: asc}}, nil),
		},
		CollFeatureFlags: {
			// `_id` is a generated ObjectID; `key` is the unique business key.
			idx(bson.D{{Key: "key", Value: asc}}, options.Index().SetUnique(true)),
		},
		CollProducts: {
			idx(bson.D{{Key: "sku", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "active", Value: asc}}, nil),
		},
		CollOnboarding: {
			idx(bson.D{{Key: "status", Value: asc}, {Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "submitted_by", Value: asc}, {Key: "created_at", Value: desc}}, nil),
			idx(bson.D{{Key: "org_unit_id", Value: asc}, {Key: "status", Value: asc}}, nil),
		},
		CollCMSContent: {
			// Delta pull (§6.1): monotonic version cursor + type/region filters.
			idx(bson.D{{Key: "version", Value: asc}}, nil),
			idx(bson.D{{Key: "type", Value: asc}, {Key: "published", Value: asc}}, nil),
			idx(bson.D{{Key: "region_scope", Value: asc}}, nil),
		},
		CollQCCertificates: {
			// One regulatory certificate per batch is enforced in the service
			// (findCertificateByBatch → 409 ALREADY_ISSUED before insert). The
			// batch_id index stays non-unique so a boot over historical data that
			// predates the guard (possible duplicate certs) never fails index
			// creation; certificate_number remains globally unique.
			idx(bson.D{{Key: "batch_id", Value: asc}}, nil),
			idx(bson.D{{Key: "certificate_number", Value: asc}}, options.Index().SetUnique(true)),
		},
		CollSettings: {
			// `_id` is the setting key (e.g. "sachiv_cap") — a small keyed store
			// for governance knobs that are neither boolean flags nor counters.
			idx(bson.D{{Key: "key", Value: asc}}, options.Index().SetUnique(true)),
		},
		CollConsignmentQC: {
			// One QC record per per-samiti batch (consignment).
			idx(bson.D{{Key: "consignment_id", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "batch_code", Value: asc}}, nil),
		},
		CollConsignmentBatchQRs: {
			// The public scan resolves by batch_code OR token.
			idx(bson.D{{Key: "batch_code", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "consignment_id", Value: asc}}, options.Index().SetUnique(true)),
			idx(bson.D{{Key: "token", Value: asc}}, nil),
		},
	}

	for coll, models := range specs {
		if _, err := db.Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
			return fmt.Errorf("ensure indexes on %s: %w", coll, err)
		}
	}
	return nil
}

func idx(keys bson.D, opts *options.IndexOptions) mongo.IndexModel {
	m := mongo.IndexModel{Keys: keys}
	if opts != nil {
		m.Options = opts
	}
	return m
}
