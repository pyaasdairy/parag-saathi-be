package publictrace

// Public per-samiti BATCH quality report (F8): GET /public/qr/{code} first
// tries to resolve the code as a consignment batch QR (by batch_code OR by
// its short HMAC token — batch codes are "PARAG-…", product QRs "PRG-…", so
// the two namespaces never collide) and renders the batch's quality report;
// otherwise the scan falls through to the existing product-lot resolution
// unchanged.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ---------------------------------------------------------------------------
// Wire models — the F8 quality-report shape
// ---------------------------------------------------------------------------

// BatchSamitiInfo names the batch's single source society. Unlike the pooled
// product view, a per-samiti batch honestly traces to exactly one samiti.
type BatchSamitiInfo struct {
	Name     string `json:"name"`
	NameHi   string `json:"name_hi,omitempty"`
	Code     string `json:"code,omitempty"`
	Village  string `json:"village,omitempty"`
	District string `json:"district,omitempty"`
	// Society coordinates (when registered on the org) so the public report
	// can show the samiti on a map — society granularity only, never a farm.
	GeoLat *float64 `json:"geo_lat,omitempty"`
	GeoLng *float64 `json:"geo_lng,omitempty"`
}

// BatchCollectionInfo is the journey timeline of the batch.
type BatchCollectionInfo struct {
	PickedUpAt  *time.Time `json:"picked_up_at,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	AcceptedAt  *time.Time `json:"accepted_at,omitempty"`
}

// BatchVolumeInfo pairs the rider-measured volume with the pooled pour total.
type BatchVolumeInfo struct {
	MeasuredLitres      *float64 `json:"measured_litres,omitempty"`
	TotalQuantityLitres float64  `json:"total_quantity_litres"`
}

// BatchTestInfo is one QC test with its limit (where one is defined) and the
// recorded verdict.
type BatchTestInfo struct {
	Parameter string   `json:"parameter"`
	Value     *float64 `json:"value,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Limit     *float64 `json:"limit,omitempty"`
	LimitUnit string   `json:"limit_unit,omitempty"`
	Pass      bool     `json:"pass"`
}

// BatchQualityInfo is the batch's QC panel and overall verdict.
type BatchQualityInfo struct {
	Tests       []BatchTestInfo `json:"tests"`
	OverallPass bool            `json:"overall_pass"`
	TestedAt    time.Time       `json:"tested_at"`
}

// BatchFarmerInfo is one contributing farmer on the batch roster. The owner
// explicitly wants the FULL journey roster for a per-samiti batch.
type BatchFarmerInfo struct {
	Name    string `json:"name"`
	NameHi  string `json:"name_hi,omitempty"`
	Village string `json:"village,omitempty"`
}

// BatchFarmersInfo is the contributing-farmer summary + roster.
type BatchFarmersInfo struct {
	Total  int               `json:"total"`
	Roster []BatchFarmerInfo `json:"roster"`
}

// BatchVanInfo names the van rider who carried the batch.
type BatchVanInfo struct {
	RiderName   string `json:"rider_name"`
	RiderNameHi string `json:"rider_name_hi,omitempty"`
}

// BatchPlantInfo names the plant that accepted the batch.
type BatchPlantInfo struct {
	Name   string `json:"name"`
	NameHi string `json:"name_hi,omitempty"`
}

// BatchQualityReport is the F8 public quality report of one per-samiti batch.
// Blocks that cannot be resolved are omitted — never fabricated.
type BatchQualityReport struct {
	BatchCode  string              `json:"batch_code"`
	Samiti     BatchSamitiInfo     `json:"samiti"`
	Collection BatchCollectionInfo `json:"collection"`
	Volume     BatchVolumeInfo     `json:"volume"`
	Quality    *BatchQualityInfo   `json:"quality,omitempty"`
	Farmers    BatchFarmersInfo    `json:"farmers"`
	Van        *BatchVanInfo       `json:"van,omitempty"`
	Plant      *BatchPlantInfo     `json:"plant,omitempty"`
}

// ---------------------------------------------------------------------------
// Repo reads (all read-only)
// ---------------------------------------------------------------------------

// batchQRByCodeOrToken resolves a scanned code against the consignment batch
// QRs — by the human-readable batch_code OR the short signed token.
func (r *repo) batchQRByCodeOrToken(ctx context.Context, code string) (*domain.ConsignmentBatchQR, error) {
	var qr domain.ConsignmentBatchQR
	err := r.consignmentBatchQRs.FindOne(ctx, bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "batch_code", Value: code}},
		bson.D{{Key: "token", Value: code}},
	}}}).Decode(&qr)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("batch qr")
	}
	if err != nil {
		return nil, err
	}
	return &qr, nil
}

// consignmentByID loads one consignment document.
func (r *repo) consignmentByID(ctx context.Context, id primitive.ObjectID) (*domain.DCSConsignment, error) {
	var c domain.DCSConsignment
	err := r.consignments.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, httpx.NotFound("consignment")
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// consignmentQCByConsignment loads the batch QC result, nil when none exists.
func (r *repo) consignmentQCByConsignment(ctx context.Context, consignmentID primitive.ObjectID) (*domain.ConsignmentQC, error) {
	var qc domain.ConsignmentQC
	err := r.consignmentQC.FindOne(ctx, bson.D{{Key: "consignment_id", Value: consignmentID}}).Decode(&qc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &qc, nil
}

// tripByID loads one route trip, nil when it no longer resolves.
func (r *repo) tripByID(ctx context.Context, id primitive.ObjectID) (*domain.RouteTrip, error) {
	var t domain.RouteTrip
	err := r.routeTrips.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// partiesByIDs loads parties by id (name + village display fields). Used for
// the batch roster — the owner explicitly wants the full journey roster for a
// per-samiti batch, so no consent filter applies here (unlike the pooled
// product roster).
func (r *repo) partiesByIDs(ctx context.Context, ids []primitive.ObjectID) ([]domain.Party, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cur, err := r.parties.Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	if err != nil {
		return nil, err
	}
	var out []domain.Party
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// partyByID loads one party, nil when it no longer resolves.
func (r *repo) partyByID(ctx context.Context, id primitive.ObjectID) (*domain.Party, error) {
	var p domain.Party
	err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// ResolvePublicQR dispatches one public scan: consignment batch QRs first
// (batch_code / token), then the existing product-lot resolution unchanged.
func (s *Service) ResolvePublicQR(ctx context.Context, code string) (any, error) {
	report, err := s.scanBatchQR(ctx, code)
	if err == nil {
		return report, nil
	}
	var appErr *httpx.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusNotFound {
		return nil, err
	}
	return s.ScanQR(ctx, code)
}

// scanBatchQR resolves a code against the consignment batch QRs and builds
// the F8 quality report. Returns NotFound when the code is not a batch QR.
func (s *Service) scanBatchQR(ctx context.Context, code string) (*BatchQualityReport, error) {
	qr, err := s.repo.batchQRByCodeOrToken(ctx, code)
	if err != nil {
		var appErr *httpx.AppError
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, s.internalErr(ctx, "batch qr scan: load qr", err, slog.String("code", code))
	}

	// Integrity gate: the stored token must equal the HMAC of the batch code
	// under the QR secret — a mismatch means the record was tampered/forged.
	expected := auth.HMACHash(s.qrSecret, qr.BatchCode)[:len(qr.Token)]
	if qr.Token == "" || !auth.ConstantTimeEqual(expected, qr.Token) {
		s.log.WarnContext(ctx, "batch QR token integrity check failed — possible forgery",
			slog.String("batch_code", qr.BatchCode), slog.String("qr_id", qr.ID.Hex()))
		return nil, httpx.Conflict("QR_INTEGRITY_FAILED",
			"this QR code failed integrity verification and may be counterfeit")
	}

	c, err := s.repo.consignmentByID(ctx, qr.ConsignmentID)
	if err != nil {
		var appErr *httpx.AppError
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, s.internalErr(ctx, "batch qr scan: load consignment", err,
			slog.String("consignment_id", qr.ConsignmentID.Hex()))
	}

	report := &BatchQualityReport{
		BatchCode: qr.BatchCode,
		Collection: BatchCollectionInfo{
			PickedUpAt:  c.PickedUpAt,
			DeliveredAt: c.DeliveredAt,
			AcceptedAt:  c.AcceptedAt,
		},
		Volume: BatchVolumeInfo{
			MeasuredLitres:      c.MeasuredVolumeLitres,
			TotalQuantityLitres: c.TotalQuantityLitres,
		},
		Farmers: BatchFarmersInfo{Roster: []BatchFarmerInfo{}},
	}

	// Samiti block (best-effort — a missing org leaves the block empty rather
	// than failing a consumer scan or fabricating names).
	if org, oerr := s.orgs.Get(ctx, c.DCSID); oerr == nil && org != nil {
		report.Samiti = BatchSamitiInfo{
			Name: org.Name, NameHi: org.NameHi, Code: org.Code,
			Village: org.Village, District: org.District,
		}
		// Only emit coordinates the org actually carries — never fabricate.
		if org.GeoLat != 0 || org.GeoLng != 0 {
			lat, lng := org.GeoLat, org.GeoLng
			report.Samiti.GeoLat, report.Samiti.GeoLng = &lat, &lng
		}
	} else {
		s.log.WarnContext(ctx, "batch qr scan: samiti org not resolvable",
			slog.String("dcs_id", c.DCSID.Hex()))
	}

	// Quality block: the stored QC panel with limits + verdicts.
	if qc, qerr := s.repo.consignmentQCByConsignment(ctx, c.ID); qerr != nil {
		return nil, s.internalErr(ctx, "batch qr scan: load qc", qerr,
			slog.String("consignment_id", c.ID.Hex()))
	} else if qc != nil {
		tests := make([]BatchTestInfo, 0, len(qc.Tests))
		for _, t := range qc.Tests {
			ti := BatchTestInfo{Parameter: t.Parameter, Value: t.Value, Unit: t.Unit, Pass: t.Pass}
			if t.Parameter == domain.BatchTestAflatoxinM1 {
				limit := domain.FSSAIAflatoxinM1MaxMicrogramPerKg
				ti.Limit = &limit
				ti.LimitUnit = "µg/kg"
			}
			tests = append(tests, ti)
		}
		report.Quality = &BatchQualityInfo{Tests: tests, OverallPass: qc.OverallPass, TestedAt: qc.TestedAt}
	}

	// Farmer roster: ALL contributors of this samiti's pours (owner-specified
	// full journey roster for the per-samiti batch).
	farmerIDs, err := s.repo.distinctFarmerPartyIDs(ctx, c.PourIDs)
	if err != nil {
		return nil, s.internalErr(ctx, "batch qr scan: distinct farmers", err)
	}
	report.Farmers.Total = len(farmerIDs)
	if parties, perr := s.repo.partiesByIDs(ctx, farmerIDs); perr != nil {
		return nil, s.internalErr(ctx, "batch qr scan: load farmer roster", perr)
	} else {
		for _, p := range parties {
			if p.FullName == "" && p.FullNameHi == "" {
				continue // never fabricate a nameless roster row
			}
			report.Farmers.Roster = append(report.Farmers.Roster, BatchFarmerInfo{
				Name: p.FullName, NameHi: p.FullNameHi, Village: p.Village,
			})
		}
	}

	// Van block via the carrying trip (best-effort).
	if c.RouteTripID != nil {
		if trip, terr := s.repo.tripByID(ctx, *c.RouteTripID); terr == nil && trip != nil {
			if rider, rerr := s.repo.partyByID(ctx, trip.VanRiderPartyID); rerr == nil && rider != nil && rider.FullName != "" {
				report.Van = &BatchVanInfo{RiderName: rider.FullName, RiderNameHi: rider.FullNameHi}
			}
		}
	}

	// Plant block from the intake stamp (best-effort).
	if c.AcceptedPlantID != nil {
		if org, oerr := s.orgs.Get(ctx, *c.AcceptedPlantID); oerr == nil && org != nil {
			report.Plant = &BatchPlantInfo{Name: org.Name, NameHi: org.NameHi}
		}
	}

	s.log.InfoContext(ctx, "batch qr scan resolved",
		slog.String("batch_code", qr.BatchCode),
		slog.String("consignment_id", c.ID.Hex()),
		slog.Int("farmers_total", report.Farmers.Total),
		slog.Bool("quality_present", report.Quality != nil))
	return report, nil
}
