package dashboards

import (
	"context"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

const trendDays = 12 // the farmer home-screen shows a 12-day trend

type service struct {
	deps *deps.Deps
	repo *repo
	log  *slog.Logger
}

func newService(d *deps.Deps, r *repo, log *slog.Logger) *service {
	return &service{deps: d, repo: r, log: log}
}

// farmerSummary builds the farmer home aggregate. A farmer may only read their
// own summary; scoped roles (sachiv/adhyaksh/admin) may read any farmer in
// their org scope — but the farmer's DCS isn't on the summary, so we gate by
// "self, or a role that can see the DCS" at the handler via RequireRoles and,
// for FARMER, force self.
func (s *service) farmerSummary(ctx context.Context, actor auth.Actor, farmerID primitive.ObjectID) (*FarmerSummary, error) {
	// A FARMER may only ever see their own summary.
	if actor.RoleCode == domain.RoleFarmer {
		self, err := httpx.ParseID(actor.PartyID, "actor")
		if err != nil {
			return nil, err
		}
		if self != farmerID {
			return nil, httpx.Forbidden("a farmer may only view their own summary")
		}
	}

	now := time.Now().UTC()
	today := domain.DateKeyIST(now)
	monthStart := monthStartIST(now)

	days, err := s.repo.farmerPourDays(ctx, farmerID, minDate(monthStart, trendSince(now)))
	if err != nil {
		return nil, httpx.Internal(err)
	}
	pendCount, pendAmt, err := s.repo.pendingInvoices(ctx, farmerID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	animals, err := s.repo.animalCount(ctx, farmerID)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	sum := &FarmerSummary{
		FarmerPartyID:   farmerID.Hex(),
		Today:           dayTotalsFor(days, today),
		Month:           periodTotalsSince(days, monthStart),
		PendingAmount:   round2(pendAmt),
		PendingInvoices: pendCount,
		AnimalCount:     animals,
		Trend:           trend(days, now),
	}
	s.log.InfoContext(ctx, "farmer summary served",
		slog.String("farmer_party_id", farmerID.Hex()),
		slog.Float64("today_qty", sum.Today.QuantityLitres),
		slog.Float64("pending_amount", sum.PendingAmount))
	return sum, nil
}

// societyStats builds the DCS console aggregate. Caller must be in the DCS's
// org scope (enforced here).
func (s *service) societyStats(ctx context.Context, actor auth.Actor, dcsID primitive.ObjectID) (*SocietyStats, error) {
	if err := s.deps.Orgs.RequireInScope(ctx, actor, dcsID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	today := domain.DateKeyIST(now)
	monthStart := monthStartIST(now)

	days, err := s.repo.dcsPourDays(ctx, dcsID, monthStart)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	activeFarmers, err := s.repo.activeFarmersToday(ctx, dcsID, today)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	members, err := s.repo.memberCount(ctx, dcsID)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	stats := &SocietyStats{
		DCSID:         dcsID.Hex(),
		Date:          today,
		Today:         dayTotalsFor(days, today),
		Month:         periodTotalsSince(days, monthStart),
		ActiveFarmers: activeFarmers,
		MemberCount:   members,
	}
	s.log.InfoContext(ctx, "society stats served",
		slog.String("dcs_id", dcsID.Hex()), slog.Int("active_farmers", activeFarmers))
	return stats, nil
}

// --- pure helpers over the day rollups ---

func dayTotalsFor(days []pourRollup, date string) DayTotals {
	for _, d := range days {
		if d.Date == date {
			return DayTotals{Date: date, QuantityLitres: round2(d.Qty), Amount: round2(d.Amt), Pours: d.Count}
		}
	}
	return DayTotals{Date: date}
}

func periodTotalsSince(days []pourRollup, since string) PeriodTotals {
	var t PeriodTotals
	for _, d := range days {
		if d.Date >= since {
			t.QuantityLitres += d.Qty
			t.Amount += d.Amt
			t.Pours += d.Count
		}
	}
	t.QuantityLitres = round2(t.QuantityLitres)
	t.Amount = round2(t.Amount)
	return t
}

// trend returns the last trendDays days (most recent first), zero-filling gaps.
func trend(days []pourRollup, now time.Time) []DayTotals {
	ist := time.FixedZone("IST", 5*3600+1800)
	byDate := make(map[string]pourRollup, len(days))
	for _, d := range days {
		byDate[d.Date] = d
	}
	out := make([]DayTotals, 0, trendDays)
	for i := 0; i < trendDays; i++ {
		key := now.In(ist).AddDate(0, 0, -i).Format("2006-01-02")
		if d, ok := byDate[key]; ok {
			out = append(out, DayTotals{Date: key, QuantityLitres: round2(d.Qty), Amount: round2(d.Amt), Pours: d.Count})
		} else {
			out = append(out, DayTotals{Date: key})
		}
	}
	return out
}

func trendSince(now time.Time) string {
	ist := time.FixedZone("IST", 5*3600+1800)
	return now.In(ist).AddDate(0, 0, -(trendDays - 1)).Format("2006-01-02")
}

func minDate(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
