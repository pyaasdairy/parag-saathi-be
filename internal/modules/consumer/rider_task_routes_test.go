package consumer

// The five per-task rider routes the DEPLOYED Saathi build still calls.
//
// release/26.07.03 (lib/api/rider_api.dart) sends otp/send, otp/verify and
// door-photo from proof_sheet.dart, scan and compliance from
// compliance_screen.dart. A registration that is missing answers 404 and the
// rider's proof sheet dies at the door, so this test drives every one of them
// through the real DELIVERY_RIDER mount shape (module.go) as the rider who
// holds the task and pins that each handler RUNS: never 404, never 405.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run RiderTaskRoutesAnswer -v

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

func TestRiderTaskRoutesAnswerForTheRider(t *testing.T) {
	// No SMS pipe: otp/send takes the OTP_DEV_MODE echo path the chain world
	// enables, exactly like a dev box with MSG91 unset.
	t.Setenv("MSG91_AUTHKEY", "")
	t.Setenv("MSG91_TEMPLATE_ID", "")
	w, done := newChainWorld(t)
	defer done()

	ord := chainMorningOrder(t, w, "9000006101", nil)
	task := chainTaskFor(t, w, ord.OrderID)
	chainOutForDelivery(t, w, task.ID)

	// The production mount: registerRiderOps inside the DELIVERY_RIDER group.
	r := chi.NewRouter()
	r.Group(func(dr chi.Router) {
		dr.Use(middleware.RequireRoles(domain.RoleDeliveryRider))
		registerRiderOps(dr, &handler{svc: w.svc})
	})
	call := func(method, path, body string, actor *auth.Actor) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if actor != nil {
			req = req.WithContext(auth.WithActor(req.Context(), *actor))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	base := "/delivery/tasks/" + task.ID
	cases := []struct {
		method, path, body string
		want               int
		marker             string // a substring only the real handler's answer carries
	}{
		{http.MethodPost, base + "/otp/send", `{}`, http.StatusOK, `"sent":true`},
		// A wrong code proves the verify handler ran (OTP_INVALID), which is
		// all a route check needs: the code itself never leaves the server.
		{http.MethodPost, base + "/otp/verify", `{"otp":"000000"}`, http.StatusBadRequest, "OTP_INVALID"},
		{http.MethodPost, base + "/scan", `{"codes":["CRATE-001"]}`, http.StatusOK, `"accepted":1`},
		{http.MethodPost, base + "/door-photo", `{"photo_ref":"/api/v1/uploads/view/door-photo/abc.jpg"}`, http.StatusOK, `"saved":true`},
		{http.MethodGet, base + "/compliance", "", http.StatusOK, `"PROOF"`},
	}
	for _, c := range cases {
		code, body := call(c.method, c.path, c.body, &w.rider)
		if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
			t.Fatalf("%s %s answered %d: the route is not registered", c.method, c.path, code)
		}
		if code != c.want || !strings.Contains(body, c.marker) {
			t.Fatalf("%s %s: %d %s (want %d with %q)", c.method, c.path, code, body, c.want, c.marker)
		}
	}

	// The scan bound the crate to THIS task and a re-post of the same list is
	// idempotent (the client re-sends the whole list on every close).
	if code, body := call(http.MethodPost, base+"/scan", `{"codes":["CRATE-001","crate-002"]}`, &w.rider); code != http.StatusOK {
		t.Fatalf("re-scan: %d %s", code, body)
	} else {
		var got struct {
			Data struct {
				Accepted int      `json:"accepted"`
				Rejected []string `json:"rejected"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil || got.Data.Accepted != 2 || len(got.Data.Rejected) != 0 {
			t.Fatalf("re-scan payload: %s (%v)", body, err)
		}
	}

	// Behind the gate, not merely present: no actor is 401, another rider
	// sees 404 (an unknown id and someone else's task are indistinguishable).
	if code, _ := call(http.MethodGet, base+"/compliance", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated compliance: %d want 401", code)
	}
	other := auth.Actor{PartyID: primitive.NewObjectID().Hex(), Kind: "role", RoleCode: "DELIVERY_RIDER"}
	if code, _ := call(http.MethodGet, base+"/compliance", "", &other); code != http.StatusNotFound {
		t.Fatalf("another rider's compliance read: %d want 404", code)
	}
}
