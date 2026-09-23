package consumer

// Contract C2: DELETE /consumer/push/register {"token"} removes the token
// when it belongs to the caller, answers 204 whether or not a row existed,
// and never touches another member's device.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://localhost:27017 \
//	  go test ./internal/modules/consumer/ -run PushUnregister -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestPushUnregisterContractC2(t *testing.T) {
	w, done := newChainWorld(t)
	defer done()
	ctx := context.Background()
	a := w.customer(t, "9000007108", 0)
	b := w.customer(t, "9000007109", 0)
	const tokA = "ExponentPushToken[aaaaaaaaaaaaaaaaaaaaaa]"
	const tokB = "ExponentPushToken[bbbbbbbbbbbbbbbbbbbbbb]"
	if err := w.svc.registerPushDevice(ctx, a, pushRegisterInput{Token: tokA, Platform: "android", Provider: "expo"}); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := w.svc.registerPushDevice(ctx, b, pushRegisterInput{Token: tokB, Platform: "ios", Provider: "expo"}); err != nil {
		t.Fatalf("register B: %v", err)
	}
	count := func(tok string) int64 {
		n, err := w.db.Collection(collPushDevices).CountDocuments(ctx, bson.D{{Key: "token", Value: tok}})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	h := &handler{svc: w.svc}
	del := func(as primitive.ObjectID, body string, authed bool) int {
		req := httptest.NewRequest(http.MethodDelete, "/push/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authed {
			req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: as.Hex()}))
		}
		rec := httptest.NewRecorder()
		h.unregisterPushDevice(rec, req)
		return rec.Code
	}

	if code := del(a, `{"token":"`+tokB+`"}`, true); code != http.StatusNoContent {
		t.Fatalf("another member's token: %d", code)
	}
	if count(tokB) != 1 {
		t.Fatal("A must not be able to unbind B's device")
	}
	if code := del(a, `{"token":"`+tokA+`"}`, true); code != http.StatusNoContent {
		t.Fatalf("own token: %d", code)
	}
	if count(tokA) != 0 {
		t.Fatal("A's token must be gone")
	}
	if code := del(a, `{"token":"`+tokA+`"}`, true); code != http.StatusNoContent {
		t.Fatalf("absent token must still be 204 (idempotent): %d", code)
	}
	if code := del(a, `{"token":""}`, true); code != http.StatusBadRequest {
		t.Fatalf("empty token: %d", code)
	}
	if code := del(a, `{"token":"`+tokB+`"}`, false); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", code)
	}
	if count(tokB) != 1 {
		t.Fatal("B's device must survive everything above")
	}
}
