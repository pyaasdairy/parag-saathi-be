package platformops

// POST / DELETE /api/v1/push/register: the Saathi app hands the backend the
// FCM token of the handset an operator is signed in on, and takes it back at
// sign-out. Any operator token (session or role) may register; a consumer
// token is refused (it is signed for another issuer). The answer says whether
// anything can be sent yet.
//
//	CONSUMER_MONGO_TEST_URI=mongodb://127.0.0.1:27017 \
//	  go test ./internal/modules/platformops/ -run PushRegister -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/push"
)

func pushRegisterWorld(t *testing.T) (*mongo.Database, http.Handler, *auth.JWTManager) {
	t.Helper()
	uri := os.Getenv("CONSUMER_MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set CONSUMER_MONGO_TEST_URI to run the push register integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	db := client.Database("platformops_push_register_test")
	_ = db.Drop(ctx)
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	if err := push.NewOperatorRegistry(db).EnsureIndexes(ctx); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	jwtm := auth.NewJWTManager("push-register-test", 15*time.Minute)
	d := &deps.Deps{Log: log, DB: db, JWT: jwtm}
	h := newHandler(newService(d, newRepository(db), log))
	r := chi.NewRouter()
	mountPushRoutes(r, d, h)
	return db, r, jwtm
}

func pushCall(t *testing.T, srv http.Handler, method, token string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/push/register", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)
	var out map[string]any
	_ = json.Unmarshal(rw.Body.Bytes(), &out)
	return rw.Code, out
}

func TestPushRegisterOperatorDevice(t *testing.T) {
	db, srv, jwtm := pushRegisterWorld(t)
	ctx := context.Background()
	mgr := domain.Party{ID: primitive.NewObjectID(), Phone: "+919900000071"}
	session, err := jwtm.IssueSessionToken(mgr)
	if err != nil {
		t.Fatal(err)
	}
	role, err := jwtm.IssueRoleToken(mgr, domain.RoleAssignment{ID: primitive.NewObjectID(), RoleCode: domain.RoleStoreManager, OrgUnitID: primitive.NewObjectID()}, "STORE")
	if err != nil {
		t.Fatal(err)
	}

	if code, _ := pushCall(t, srv, http.MethodPost, "", map[string]string{"token": "fcm-a", "platform": "android"}); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	// A token from another issuer (the consumer app's) is not an operator's.
	foreign, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"pid": mgr.ID.Hex(), "iss": "saathi-consumer", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("push-register-test"))
	if code, _ := pushCall(t, srv, http.MethodPost, foreign, map[string]string{"token": "fcm-a", "platform": "android"}); code != http.StatusUnauthorized {
		t.Fatalf("consumer-issuer token: %d", code)
	}

	code, out := pushCall(t, srv, http.MethodPost, session, map[string]string{"token": "fcm-a", "platform": "android", "provider": "fcm", "app_version": "1.7.4"})
	if code != http.StatusOK {
		t.Fatalf("session register: %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["registered"] != true || data["delivery"] != "pending_sender" {
		t.Fatalf("answer %v, want registered with delivery pending_sender while FCM is not configured", out)
	}
	// The role token re-registers the same device: still one row, now tagged
	// with the role it was signed in under.
	if code, out := pushCall(t, srv, http.MethodPost, role, map[string]string{"token": "fcm-a", "platform": "android"}); code != http.StatusOK {
		t.Fatalf("role register: %d %v", code, out)
	}
	var rows []push.OperatorDevice
	cur, _ := db.Collection(push.CollOperatorPushDevices).Find(ctx, bson.D{})
	_ = cur.All(ctx, &rows)
	if len(rows) != 1 || rows[0].PartyID != mgr.ID || rows[0].RoleCode != domain.RoleStoreManager || rows[0].Provider != "fcm" {
		t.Fatalf("rows %+v", rows)
	}

	if code, out := pushCall(t, srv, http.MethodPost, role, map[string]string{"token": "fcm-b", "platform": "web"}); code != http.StatusBadRequest {
		t.Fatalf("a web platform must be a 400, got %d %v", code, out)
	}
	if code, _ := pushCall(t, srv, http.MethodPost, role, map[string]string{"platform": "android"}); code != http.StatusBadRequest {
		t.Fatalf("a missing token must be a 400, got %d", code)
	}

	// Sign-out: 204, and again 204 (idempotent).
	for i := 0; i < 2; i++ {
		if code, _ := pushCall(t, srv, http.MethodDelete, role, map[string]string{"token": "fcm-a"}); code != http.StatusNoContent {
			t.Fatalf("unregister %d: %d", i, code)
		}
	}
	if n, _ := db.Collection(push.CollOperatorPushDevices).CountDocuments(ctx, bson.D{}); n != 0 {
		t.Fatalf("rows after sign-out: %d", n)
	}
}
