package logistics

import (
	"net/http"
	"testing"
	"time"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

const (
	testRider      = "rider-1"
	testOtherRider = "rider-2"
)

// tripFixture builds a two-stop trip; picked marks which consignment IDs are
// already picked up.
func tripFixture(status string, picked ...string) *domain.RouteTrip {
	pickedSet := map[string]bool{}
	for _, id := range picked {
		pickedSet[id] = true
	}
	now := time.Now().UTC()
	stops := []domain.RouteStop{
		{DCSID: "dcs-1", ConsignmentID: "con-1"},
		{DCSID: "dcs-2", ConsignmentID: "con-2"},
	}
	for i := range stops {
		if pickedSet[stops[i].ConsignmentID] {
			stops[i].PickedUpAt = &now
		}
	}
	return &domain.RouteTrip{
		ID:              "trip-1",
		UnionID:         "union-1",
		VanRiderPartyID: testRider,
		Status:          status,
		Stops:           stops,
	}
}

func assertAppError(t *testing.T, err *httpx.AppError, wantStatus int, wantCode string) {
	t.Helper()
	if wantStatus == 0 {
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected %d %s, got nil", wantStatus, wantCode)
	}
	if err.Status != wantStatus {
		t.Fatalf("status = %d, want %d (err: %v)", err.Status, wantStatus, err)
	}
	if wantCode != "" && err.Code != wantCode {
		t.Fatalf("code = %q, want %q (err: %v)", err.Code, wantCode, err)
	}
}

func TestValidatePickup(t *testing.T) {
	tests := []struct {
		name          string
		trip          *domain.RouteTrip
		rider         string
		consignmentID string
		wantStatus    int
		wantCode      string
	}{
		{
			name: "wrong rider is forbidden",
			trip: tripFixture(domain.TripStatusPlanned), rider: testOtherRider, consignmentID: "con-1",
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name: "pickup on delivered trip conflicts",
			trip: tripFixture(domain.TripStatusDelivered, "con-1", "con-2"), rider: testRider, consignmentID: "con-1",
			wantStatus: http.StatusConflict, wantCode: "TRIP_ALREADY_DELIVERED",
		},
		{
			name: "consignment not planned on this trip is not found",
			trip: tripFixture(domain.TripStatusPlanned), rider: testRider, consignmentID: "con-999",
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
		{
			name: "double pickup of the same stop conflicts",
			trip: tripFixture(domain.TripStatusInProgress, "con-1"), rider: testRider, consignmentID: "con-1",
			wantStatus: http.StatusConflict, wantCode: "STOP_ALREADY_PICKED",
		},
		{
			name: "first pickup on planned trip is allowed",
			trip: tripFixture(domain.TripStatusPlanned), rider: testRider, consignmentID: "con-1",
		},
		{
			name: "second pickup on in-progress trip is allowed",
			trip: tripFixture(domain.TripStatusInProgress, "con-1"), rider: testRider, consignmentID: "con-2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertAppError(t, validatePickup(tc.trip, tc.rider, tc.consignmentID), tc.wantStatus, tc.wantCode)
		})
	}
}

func TestValidateDeliver(t *testing.T) {
	tests := []struct {
		name       string
		trip       *domain.RouteTrip
		rider      string
		wantStatus int
		wantCode   string
	}{
		{
			name: "wrong rider is forbidden",
			trip: tripFixture(domain.TripStatusInProgress, "con-1", "con-2"), rider: testOtherRider,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name: "delivering twice conflicts",
			trip: tripFixture(domain.TripStatusDelivered, "con-1", "con-2"), rider: testRider,
			wantStatus: http.StatusConflict, wantCode: "TRIP_ALREADY_DELIVERED",
		},
		{
			name: "deliver before any pickup is unprocessable",
			trip: tripFixture(domain.TripStatusPlanned), rider: testRider,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "STOPS_NOT_PICKED",
		},
		{
			name: "deliver with one unpicked stop is unprocessable",
			trip: tripFixture(domain.TripStatusInProgress, "con-1"), rider: testRider,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "STOPS_NOT_PICKED",
		},
		{
			name: "deliver with all stops picked is allowed",
			trip: tripFixture(domain.TripStatusInProgress, "con-1", "con-2"), rider: testRider,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertAppError(t, validateDeliver(tc.trip, tc.rider), tc.wantStatus, tc.wantCode)
		})
	}
}

func TestValidateColdChain(t *testing.T) {
	tests := []struct {
		name       string
		trip       *domain.RouteTrip
		rider      string
		wantStatus int
		wantCode   string
	}{
		{
			name: "wrong rider is forbidden",
			trip: tripFixture(domain.TripStatusInProgress), rider: testOtherRider,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name: "logging after delivery conflicts",
			trip: tripFixture(domain.TripStatusDelivered, "con-1", "con-2"), rider: testRider,
			wantStatus: http.StatusConflict, wantCode: "TRIP_ALREADY_DELIVERED",
		},
		{
			name: "logging while planned is allowed",
			trip: tripFixture(domain.TripStatusPlanned), rider: testRider,
		},
		{
			name: "logging while in progress is allowed",
			trip: tripFixture(domain.TripStatusInProgress, "con-1"), rider: testRider,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertAppError(t, validateColdChain(tc.trip, tc.rider), tc.wantStatus, tc.wantCode)
		})
	}
}

func TestValidateStopConsignment(t *testing.T) {
	consignment := func(status, dcsID string) *domain.DCSConsignment {
		return &domain.DCSConsignment{ID: "con-1", DCSID: dcsID, Status: status}
	}
	tests := []struct {
		name        string
		consignment *domain.DCSConsignment
		stopDCSID   string
		wantStatus  int
		wantCode    string
	}{
		{
			name:        "planning an OPEN consignment conflicts (pickup before dispatch)",
			consignment: consignment(domain.ConsignmentStatusOpen, "dcs-1"), stopDCSID: "dcs-1",
			wantStatus: http.StatusConflict, wantCode: "CONSIGNMENT_NOT_DISPATCHED",
		},
		{
			name:        "planning an already picked-up consignment conflicts",
			consignment: consignment(domain.ConsignmentStatusPickedUp, "dcs-1"), stopDCSID: "dcs-1",
			wantStatus: http.StatusConflict, wantCode: "CONSIGNMENT_NOT_DISPATCHED",
		},
		{
			name:        "consignment from another DCS is unprocessable",
			consignment: consignment(domain.ConsignmentStatusDispatch, "dcs-1"), stopDCSID: "dcs-2",
			wantStatus: http.StatusUnprocessableEntity, wantCode: "CONSIGNMENT_DCS_MISMATCH",
		},
		{
			name:        "dispatched consignment at its own DCS is allowed",
			consignment: consignment(domain.ConsignmentStatusDispatch, "dcs-1"), stopDCSID: "dcs-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertAppError(t, validateStopConsignment(tc.consignment, tc.stopDCSID), tc.wantStatus, tc.wantCode)
		})
	}
}

func TestAggregatePours(t *testing.T) {
	rows := []pourRow{
		{ID: "p1", QuantityLitres: 10, FatPct: 4.0, SNFPct: 8.0},
		{ID: "p2", QuantityLitres: 30, FatPct: 6.0, SNFPct: 9.0},
	}
	ids, total, fat, snf := aggregatePours(rows)
	if len(ids) != 2 || ids[0] != "p1" || ids[1] != "p2" {
		t.Fatalf("ids = %v", ids)
	}
	if total != 40 {
		t.Fatalf("total = %v, want 40", total)
	}
	if fat != 5.5 { // (4*10 + 6*30) / 40
		t.Fatalf("weighted fat = %v, want 5.5", fat)
	}
	if snf != 8.75 { // (8*10 + 9*30) / 40
		t.Fatalf("weighted snf = %v, want 8.75", snf)
	}
}
