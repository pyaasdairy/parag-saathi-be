package push

// The FCM HTTP v1 pipe behind Saathi's operator push: inert without a service
// account, trades a signed service-account assertion for an OAuth2 access
// token (cached until it nears expiry), posts one message in FCM's v1 wire
// shape with that token as a Bearer, and tells a dead token (UNREGISTERED,
// which the caller prunes) from a transient failure and from a definitive
// rejection.
//
//	go test ./internal/platform/push/ -run FCM -v

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	fcmTestKeyOnce sync.Once
	fcmTestKey     *rsa.PrivateKey
)

func fcmKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	fcmTestKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		fcmTestKey = k
	})
	return fcmTestKey
}

// fcmServiceAccount is a service-account JSON shaped like the one the
// Firebase console downloads, with its token_uri pointed at the stub.
func fcmServiceAccount(t *testing.T, tokenURI string) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(fcmKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	b, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "pyaas-saathi-test",
		"private_key_id": "kid-1",
		"private_key":    pemKey,
		"client_email":   "fcm-sender@pyaas-saathi-test.iam.gserviceaccount.com",
		"token_uri":      tokenURI,
	})
	return string(b)
}

// fcmStub is one listener standing in for both Google's token endpoint and
// FCM: /token answers an access token, /v1/projects/<p>/messages:send answers
// what the test scripts in `send`.
type fcmStub struct {
	srv        *httptest.Server
	mu         sync.Mutex
	tokenCalls int
	assertions []string
	sends      []fcmSeen
	send       func(n int, body map[string]any) (int, string)
}

type fcmSeen struct {
	path string
	auth string
	ct   string
	body map[string]any
}

func newFCMStub(t *testing.T) *fcmStub {
	t.Helper()
	s := &fcmStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		rw.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			s.tokenCalls++
			form, _ := url.ParseQuery(string(b))
			if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				rw.WriteHeader(400)
				rw.Write([]byte(`{"error":"unsupported_grant_type"}`))
				return
			}
			s.assertions = append(s.assertions, form.Get("assertion"))
			rw.Write([]byte(`{"access_token":"ya29.token-` + string(rune('0'+s.tokenCalls)) + `","expires_in":3599,"token_type":"Bearer"}`))
			return
		}
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		s.sends = append(s.sends, fcmSeen{path: r.URL.Path, auth: r.Header.Get("Authorization"), ct: r.Header.Get("Content-Type"), body: body})
		code, answer := 200, `{"name":"projects/pyaas-saathi-test/messages/0:1"}`
		if s.send != nil {
			code, answer = s.send(len(s.sends), body)
		}
		rw.WriteHeader(code)
		rw.Write([]byte(answer))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fcmStub) newFCM(t *testing.T) *FCM {
	t.Helper()
	f := NewFCM(FCMConfig{ServiceAccountJSON: fcmServiceAccount(t, s.srv.URL+"/token"), BaseURL: s.srv.URL})
	if !f.Enabled() {
		t.Fatalf("a valid service account must enable the sender: %s", f.Status())
	}
	return f
}

func TestFCMInertWithoutAServiceAccount(t *testing.T) {
	var nilFCM *FCM
	if nilFCM.Enabled() {
		t.Fatal("a nil sender must report disabled")
	}
	off := NewFCM(FCMConfig{})
	if off.Enabled() {
		t.Fatal("no service account must leave the sender inert")
	}
	if !strings.Contains(off.Status(), "FCM_SERVICE_ACCOUNT_JSON") {
		t.Fatalf("the boot line must name the missing key, got %q", off.Status())
	}
	if _, err := off.Send(context.Background(), FCMMessage{Token: "t", Body: "b"}); err == nil {
		t.Fatal("an inert sender must refuse to send")
	}
	bad := NewFCM(FCMConfig{ServiceAccountJSON: "not json, not base64 json"})
	if bad.Enabled() || !strings.Contains(bad.Status(), "unreadable") {
		t.Fatalf("an unreadable service account must stay inert and say so, got %q", bad.Status())
	}
	noKey := NewFCM(FCMConfig{ServiceAccountJSON: `{"type":"service_account","project_id":"p","client_email":"e@x"}`})
	if noKey.Enabled() {
		t.Fatal("a service account without a private key must stay inert")
	}
}

func TestFCMReadsRawOrBase64ServiceAccountAndProjectOverride(t *testing.T) {
	raw := fcmServiceAccount(t, "https://oauth2.googleapis.com/token")
	for name, in := range map[string]string{
		"raw":        raw,
		"base64":     base64.StdEncoding.EncodeToString([]byte(raw)),
		"base64 url": base64.URLEncoding.EncodeToString([]byte(raw)),
		"spaced":     "\n  " + base64.StdEncoding.EncodeToString([]byte(raw)) + "  \n",
	} {
		f := NewFCM(FCMConfig{ServiceAccountJSON: in})
		if !f.Enabled() {
			t.Fatalf("%s: sender not enabled: %s", name, f.Status())
		}
		if f.ProjectID() != "pyaas-saathi-test" {
			t.Fatalf("%s: project %q, want the service account's", name, f.ProjectID())
		}
		if !strings.Contains(f.Status(), "pyaas-saathi-test") {
			t.Fatalf("%s: the boot line must name the project, got %q", name, f.Status())
		}
	}
	over := NewFCM(FCMConfig{ServiceAccountJSON: raw, ProjectID: "pyaas-prod"})
	if over.ProjectID() != "pyaas-prod" {
		t.Fatalf("FCM_PROJECT_ID must win over the service account's project, got %q", over.ProjectID())
	}
}

func TestFCMSendShapeAuthAndTokenCache(t *testing.T) {
	stub := newFCMStub(t)
	f := stub.newFCM(t)
	now := time.Date(2026, 9, 26, 21, 0, 0, 0, time.UTC)
	f.SetClock(func() time.Time { return now })

	name, err := f.Send(context.Background(), FCMMessage{
		Token: "fcm-token-a", Title: "New instant order", Body: "Order ORD1 - 2 items - Rs 114",
		Data:             map[string]string{"type": "STORE_INSTANT_ORDER", "delivery_id": "DLV1"},
		AndroidChannelID: "store_alerts", Urgent: true, CollapseKey: "order-DLV1",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if name != "projects/pyaas-saathi-test/messages/0:1" {
		t.Fatalf("message name %q", name)
	}
	if len(stub.sends) != 1 || stub.tokenCalls != 1 {
		t.Fatalf("sends %d token calls %d, want 1 and 1", len(stub.sends), stub.tokenCalls)
	}
	got := stub.sends[0]
	if got.path != "/v1/projects/pyaas-saathi-test/messages:send" {
		t.Fatalf("path %q", got.path)
	}
	if got.auth != "Bearer ya29.token-1" || !strings.HasPrefix(got.ct, "application/json") {
		t.Fatalf("headers auth=%q ct=%q", got.auth, got.ct)
	}
	msg, _ := got.body["message"].(map[string]any)
	if msg == nil || msg["token"] != "fcm-token-a" {
		t.Fatalf("message.token missing: %v", got.body)
	}
	note, _ := msg["notification"].(map[string]any)
	if note["title"] != "New instant order" || note["body"] != "Order ORD1 - 2 items - Rs 114" {
		t.Fatalf("notification %v", note)
	}
	data, _ := msg["data"].(map[string]any)
	if data["type"] != "STORE_INSTANT_ORDER" || data["delivery_id"] != "DLV1" {
		t.Fatalf("data %v", data)
	}
	android, _ := msg["android"].(map[string]any)
	if android["priority"] != "HIGH" || android["collapse_key"] != "order-DLV1" {
		t.Fatalf("android %v", android)
	}
	an, _ := android["notification"].(map[string]any)
	if an["channel_id"] != "store_alerts" || an["sound"] != "default" || an["tag"] != "order-DLV1" {
		t.Fatalf("android.notification %v", an)
	}
	apns, _ := msg["apns"].(map[string]any)
	hdr, _ := apns["headers"].(map[string]any)
	if hdr["apns-priority"] != "10" || hdr["apns-collapse-id"] != "order-DLV1" {
		t.Fatalf("apns headers %v", hdr)
	}

	// The assertion is a service-account JWT: RS256, signed by the account's
	// key, for the FCM scope, addressed to the token endpoint, one hour long.
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(stub.assertions[0], claims, func(*jwt.Token) (any, error) { return &fcmKey(t).PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || !tok.Valid {
		t.Fatalf("assertion does not verify with the account key: %v", err)
	}
	if tok.Header["kid"] != "kid-1" {
		t.Fatalf("assertion kid %v", tok.Header["kid"])
	}
	if claims["iss"] != "fcm-sender@pyaas-saathi-test.iam.gserviceaccount.com" ||
		claims["scope"] != "https://www.googleapis.com/auth/firebase.messaging" ||
		claims["aud"] != stub.srv.URL+"/token" {
		t.Fatalf("assertion claims %v", claims)
	}
	if iat, exp := claims["iat"].(float64), claims["exp"].(float64); int64(iat) != now.Unix() || int64(exp) != now.Add(time.Hour).Unix() {
		t.Fatalf("assertion window iat=%v exp=%v", iat, exp)
	}

	// A normal-priority alert: no collapse key, NORMAL / 5.
	if _, err := f.Send(context.Background(), FCMMessage{Token: "fcm-token-b", Title: "Low stock", Body: "Toned 500ml (4)"}); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if stub.tokenCalls != 1 {
		t.Fatalf("a fresh access token must be reused, token calls %d", stub.tokenCalls)
	}
	msg2, _ := stub.sends[1].body["message"].(map[string]any)
	a2, _ := msg2["android"].(map[string]any)
	h2, _ := msg2["apns"].(map[string]any)["headers"].(map[string]any)
	if a2["priority"] != "NORMAL" || h2["apns-priority"] != "5" {
		t.Fatalf("normal priority: android %v apns %v", a2, h2)
	}
	if _, has := a2["collapse_key"]; has {
		t.Fatalf("no collapse key must be sent when none is set: %v", a2)
	}

	// Near expiry the token is traded again.
	now = now.Add(59 * time.Minute)
	if _, err := f.Send(context.Background(), FCMMessage{Token: "fcm-token-c", Body: "x"}); err != nil {
		t.Fatalf("third send: %v", err)
	}
	if stub.tokenCalls != 2 || stub.sends[2].auth != "Bearer ya29.token-2" {
		t.Fatalf("an access token about to expire must be renewed: calls %d auth %q", stub.tokenCalls, stub.sends[2].auth)
	}
}

func TestFCMSendErrorsSplitDeadTokenTransientAndRejected(t *testing.T) {
	stub := newFCMStub(t)
	f := stub.newFCM(t)
	ctx := context.Background()

	stub.send = func(int, map[string]any) (int, string) {
		return 404, `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`
	}
	if _, err := f.Send(ctx, FCMMessage{Token: "dead", Body: "x"}); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("UNREGISTERED must be ErrUnregistered so the caller prunes the token, got %v", err)
	}

	stub.send = func(int, map[string]any) (int, string) {
		return 503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`
	}
	if _, err := f.Send(ctx, FCMMessage{Token: "t", Body: "x"}); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnregistered) {
		t.Fatalf("a 5xx is transient, got %v", err)
	}
	stub.send = func(int, map[string]any) (int, string) {
		return 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"errorCode":"QUOTA_EXCEEDED"}]}}`
	}
	if _, err := f.Send(ctx, FCMMessage{Token: "t", Body: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a 429 is transient, got %v", err)
	}

	stub.send = func(int, map[string]any) (int, string) {
		return 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"INVALID_ARGUMENT"}]}}`
	}
	_, err := f.Send(ctx, FCMMessage{Token: "t", Body: "x"})
	if err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnregistered) {
		t.Fatalf("a 400 is a definitive rejection that keeps the token, got %v", err)
	}

	// A 401 means the cached access token went bad: renew once and retry.
	stub.send = func(n int, _ map[string]any) (int, string) {
		if n == 5 {
			return 401, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`
		}
		return 200, `{"name":"projects/p/messages/2"}`
	}
	calls := stub.tokenCalls
	if _, err := f.Send(ctx, FCMMessage{Token: "t", Body: "x"}); err != nil {
		t.Fatalf("a 401 must renew the access token and retry once, got %v", err)
	}
	if stub.tokenCalls != calls+1 {
		t.Fatalf("token calls %d, want %d", stub.tokenCalls, calls+1)
	}

	if _, err := f.Send(ctx, FCMMessage{Body: "x"}); err == nil {
		t.Fatal("a message without a device token must be refused")
	}
}

func TestFCMTokenEndpointFailureIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(502)
	}))
	defer srv.Close()
	f := NewFCM(FCMConfig{ServiceAccountJSON: fcmServiceAccount(t, srv.URL+"/token"), BaseURL: srv.URL})
	if _, err := f.Send(context.Background(), FCMMessage{Token: "t", Body: "x"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a failed token exchange is transient, got %v", err)
	}
}
