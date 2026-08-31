package consumer

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestRiderOpsRoutesRegistered pins the rider-console wire contract.
//
// The Flutter rider console (pyaas-saathi, lib/api/rider_api.dart) was written
// and shipped against docs/RIDER_API.md before any of these routes existed —
// every one of them returned 404 in production, and the app quietly rendered
// fabricated sample data instead. This test is the guard against that ever
// recurring silently: if a route is renamed, dropped, or moved out of the
// DELIVERY_RIDER group, this fails here rather than in a rider's hands.
//
// registerRiderOps only takes method VALUES off the handler at registration
// time, so a nil *handler is safe — nothing is invoked, we are inspecting the
// routing table, not serving traffic.
func TestRiderOpsRoutesRegistered(t *testing.T) {
	want := []string{
		// Per-task extras (RIDER_API.md §2)
		"POST /delivery/tasks/{deliveryId}/otp/send",
		"POST /delivery/tasks/{deliveryId}/otp/verify",
		"POST /delivery/tasks/{deliveryId}/scan",
		"POST /delivery/tasks/{deliveryId}/door-photo",
		"POST /delivery/tasks/{deliveryId}/undo",
		"GET /delivery/tasks/{deliveryId}/compliance",
		// Profile + self-service (§3.1, §3.10-3.13, §2.6)
		"GET /delivery/rider/me",
		"GET /delivery/rider/nd-reasons",
		"GET /delivery/rider/documents/",
		"POST /delivery/rider/documents/",
		"GET /delivery/rider/support/types",
		"GET /delivery/rider/support/concerns",
		"POST /delivery/rider/support/concerns",
		"GET /delivery/rider/referral",
		"GET /delivery/rider/emergency-contacts",
		"PUT /delivery/rider/emergency-contacts",
		// Attendance (§3.2, §3.3)
		"GET /delivery/rider/attendance/today",
		"POST /delivery/rider/attendance/check-in",
		"POST /delivery/rider/attendance/check-out",
		"GET /delivery/rider/attendance/history",
		"POST /delivery/rider/face/enrol",
		// Route + inventory (§3.4, §3.6)
		"GET /delivery/rider/route/today",
		"POST /delivery/rider/route/complete",
		"GET /delivery/rider/inventory/today",
		"POST /delivery/rider/inventory/{sessionId}/verify",
		// Money (§3.5, §3.7, §3.8)
		"GET /delivery/rider/cash/",
		"POST /delivery/rider/cash/{cashId}/otp",
		"POST /delivery/rider/cash/{cashId}/collect",
		"POST /delivery/rider/cash/{cashId}/undo",
		"GET /delivery/rider/earnings",
		"GET /delivery/rider/penalties/",
		"POST /delivery/rider/penalties/{penaltyId}/concern",
		"POST /delivery/rider/penalties/{penaltyId}/waiver",
		// Performance (§3.9)
		"GET /delivery/rider/performance/",
		"GET /delivery/rider/performance/feedback",
		"GET /delivery/rider/performance/daily",
	}

	r := chi.NewRouter()
	registerRiderOps(r, (*handler)(nil))

	got := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}

	var missing []string
	for _, w := range want {
		if !got[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		have := make([]string, 0, len(got))
		for k := range got {
			have = append(have, k)
		}
		sort.Strings(have)
		t.Fatalf("rider-console routes missing (%d):\n  %s\n\nregistered:\n  %s",
			len(missing), strings.Join(missing, "\n  "), strings.Join(have, "\n  "))
	}

	if len(got) != len(want) {
		t.Errorf("route count drifted: registered %d, pinned %d — update this test deliberately, "+
			"and update pyaas-saathi/docs/RIDER_API.md with it", len(got), len(want))
	}
}
