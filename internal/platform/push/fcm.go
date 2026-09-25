package push

// FCM HTTP v1: the transport behind operator push (the Saathi app's store
// managers and riders, founder decision 8: "a store manager's closing alert
// only visible while the app is open is not an alert").
//
// Saathi is a Flutter app with firebase_messaging, so its devices hold FCM
// registration tokens and are reached through Firebase Cloud Messaging
// directly (the consumer app's Expo tokens stay on expo.go). Like expo.go this
// is ONLY the pipe: it knows nothing about stores, roles or the device
// registry. Sending needs an OAuth2 access token, which a Google service
// account mints for itself: we sign a JWT assertion with the account's
// private key (RS256) and trade it at the account's token_uri, then post the
// message with that token as a Bearer. The token lives an hour and is cached
// until a minute before it expires.
//
// Inert until FCM_SERVICE_ACCOUNT_JSON holds a readable service account: Send
// refuses and Status says which door is open, for the boot log. FCM_BASE_URL
// replaces FCM's origin for a dry run (the path stays, so a stub sees the
// exact request production sends); a dry-run service account points its own
// token_uri at the same stub.

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	fcmOrigin      = "https://fcm.googleapis.com"
	fcmScope       = "https://www.googleapis.com/auth/firebase.messaging"
	googleTokenURI = "https://oauth2.googleapis.com/token"
	jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

// ErrUnregistered marks a token FCM says no app instance holds any more
// (errorCode UNREGISTERED): the caller drops it from its registry. Any other
// rejection keeps the token.
var ErrUnregistered = errors.New("push: device token unregistered")

// FCMConfig is the sender's configuration, read from env by the caller.
type FCMConfig struct {
	// ServiceAccountJSON is FCM_SERVICE_ACCOUNT_JSON: the service-account key
	// file from the Firebase console (Project settings -> Service accounts ->
	// Generate new private key), raw or base64-encoded.
	ServiceAccountJSON string
	// ProjectID is FCM_PROJECT_ID; empty takes the service account's project_id.
	ProjectID string
	// BaseURL is FCM_BASE_URL, the dry-run origin; empty is FCM itself.
	BaseURL string
}

// FCMMessage is one notification to one device.
type FCMMessage struct {
	Token string
	Title string
	Body  string
	// Data travels with the notification for the app's tap handler.
	Data map[string]string
	// AndroidChannelID names the Android notification channel. A channel the
	// app never created falls back to FCM's default channel on the handset.
	AndroidChannelID string
	// Urgent wakes a dozing handset now (Android HIGH, APNs priority 10);
	// otherwise NORMAL / 5.
	Urgent bool
	// CollapseKey makes a newer alert replace an older one of the same key on
	// the device (android collapse_key + notification tag, apns-collapse-id).
	CollapseKey string
}

// FCM is a minimal FCM HTTP v1 client. Safe for concurrent use.
type FCM struct {
	status      string
	projectID   string
	clientEmail string
	keyID       string
	key         *rsa.PrivateKey
	tokenURI    string
	endpoint    string
	client      *http.Client
	now         func() time.Time

	mu          sync.Mutex // guards the cached access token; held across a renewal (single flight)
	accessToken string
	expiresAt   time.Time
}

type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// NewFCM builds the sender. It never fails: a missing or unreadable service
// account leaves it inert, and Status says why.
func NewFCM(c FCMConfig) *FCM {
	f := &FCM{client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
	raw := strings.TrimSpace(c.ServiceAccountJSON)
	if raw == "" {
		f.status = "operator push (Saathi): inert - FCM_SERVICE_ACCOUNT_JSON is not set; devices register at POST /push/register, nothing is sent"
		return f
	}
	sa, err := readServiceAccount(raw)
	if err != nil {
		f.status = "operator push (Saathi): inert - FCM_SERVICE_ACCOUNT_JSON is unreadable (" + err.Error() + "); devices register, nothing is sent"
		return f
	}
	project := strings.TrimSpace(c.ProjectID)
	if project == "" {
		project = sa.ProjectID
	}
	if project == "" {
		f.status = "operator push (Saathi): inert - no project id (set FCM_PROJECT_ID or use the Firebase service-account file); devices register, nothing is sent"
		return f
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(sa.PrivateKey))
	if err != nil {
		f.status = "operator push (Saathi): inert - FCM_SERVICE_ACCOUNT_JSON is unreadable (private_key: " + err.Error() + "); devices register, nothing is sent"
		return f
	}
	origin := fcmOrigin
	if o := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); o != "" {
		origin = o
	}
	f.projectID = project
	f.clientEmail = sa.ClientEmail
	f.keyID = sa.PrivateKeyID
	f.key = key
	f.tokenURI = sa.TokenURI
	if f.tokenURI == "" {
		f.tokenURI = googleTokenURI
	}
	f.endpoint = origin + "/v1/projects/" + url.PathEscape(project) + "/messages:send"
	f.status = "operator push (Saathi): FCM sender live for project " + project +
		" - store alerts reach store managers' and riders' phones"
	if origin != fcmOrigin {
		f.status += " (dry run: FCM_BASE_URL=" + origin + ")"
	}
	return f
}

// readServiceAccount accepts the key file as raw JSON or base64 of it (a
// dashboard field that mangles newlines takes the base64 form), and repairs a
// private key whose newlines arrived as a literal backslash-n.
func readServiceAccount(raw string) (*serviceAccount, error) {
	b := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		compact := strings.Join(strings.Fields(raw), "")
		var decoded []byte
		var err error
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if decoded, err = enc.DecodeString(compact); err == nil {
				break
			}
		}
		if err != nil || !bytes.HasPrefix(bytes.TrimSpace(decoded), []byte("{")) {
			return nil, errors.New("neither JSON nor base64-encoded JSON")
		}
		b = decoded
	}
	var sa serviceAccount
	if err := json.Unmarshal(b, &sa); err != nil {
		return nil, fmt.Errorf("not a service-account JSON: %v", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("client_email or private_key missing")
	}
	if !strings.Contains(sa.PrivateKey, "\n") && strings.Contains(sa.PrivateKey, `\n`) {
		sa.PrivateKey = strings.ReplaceAll(sa.PrivateKey, `\n`, "\n")
	}
	return &sa, nil
}

// Enabled reports whether real delivery is configured.
func (f *FCM) Enabled() bool { return f != nil && f.key != nil }

// Status is the one boot line saying which door is open.
func (f *FCM) Status() string {
	if f == nil {
		return "operator push (Saathi): inert - no sender"
	}
	return f.status
}

// ProjectID is the Firebase project messages go to ("" when inert).
func (f *FCM) ProjectID() string {
	if f == nil {
		return ""
	}
	return f.projectID
}

// SetClock replaces the wall clock the token cache reads (a test seam).
func (f *FCM) SetClock(now func() time.Time) {
	if now != nil {
		f.now = now
	}
}

type fcmWire struct {
	Message fcmWireMessage `json:"message"`
}

type fcmWireMessage struct {
	Token        string            `json:"token"`
	Notification *fcmNotification  `json:"notification,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
	Android      fcmAndroid        `json:"android"`
	APNS         fcmAPNS           `json:"apns"`
}

type fcmNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type fcmAndroid struct {
	Priority     string                  `json:"priority"`
	CollapseKey  string                  `json:"collapse_key,omitempty"`
	Notification *fcmAndroidNotification `json:"notification,omitempty"`
}

type fcmAndroidNotification struct {
	ChannelID string `json:"channel_id,omitempty"`
	Sound     string `json:"sound,omitempty"`
	Tag       string `json:"tag,omitempty"`
}

type fcmAPNS struct {
	Headers map[string]string `json:"headers"`
	Payload map[string]any    `json:"payload"`
}

func (m FCMMessage) wire() fcmWire {
	w := fcmWireMessage{Token: m.Token, Data: m.Data}
	if m.Title != "" || m.Body != "" {
		w.Notification = &fcmNotification{Title: m.Title, Body: m.Body}
	}
	w.Android = fcmAndroid{Priority: "NORMAL", CollapseKey: m.CollapseKey,
		Notification: &fcmAndroidNotification{ChannelID: m.AndroidChannelID, Sound: "default", Tag: m.CollapseKey}}
	w.APNS = fcmAPNS{Headers: map[string]string{"apns-priority": "5"}, Payload: map[string]any{"aps": map[string]any{"sound": "default"}}}
	if m.Urgent {
		w.Android.Priority = "HIGH"
		w.APNS.Headers["apns-priority"] = "10"
	}
	if m.CollapseKey != "" {
		w.APNS.Headers["apns-collapse-id"] = m.CollapseKey
	}
	return fcmWire{Message: w}
}

// Send posts one message and returns FCM's message name. Errors split three
// ways: ErrUnregistered (the token is dead; prune it), ErrUnavailable (a
// transport error, a 5xx or a 429: transient, try again later) and anything
// else (a definitive rejection; the token stays).
func (f *FCM) Send(ctx context.Context, m FCMMessage) (string, error) {
	if !f.Enabled() {
		return "", fmt.Errorf("push: FCM not configured")
	}
	if strings.TrimSpace(m.Token) == "" {
		return "", fmt.Errorf("push: no device token")
	}
	body, err := json.Marshal(m.wire())
	if err != nil {
		return "", fmt.Errorf("push: marshal: %w", err)
	}
	for attempt := 0; ; attempt++ {
		token, err := f.bearer(ctx)
		if err != nil {
			return "", err
		}
		name, status, err := f.post(ctx, token, body)
		if status == http.StatusUnauthorized && attempt == 0 {
			f.forget(token) // the cached token went bad: renew once and retry
			continue
		}
		return name, err
	}
}

func (f *FCM) post(ctx context.Context, token string, body []byte) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("push: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("push: FCM request failed: %v: %w", err, ErrUnavailable)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(rb, &ok)
		return ok.Name, resp.StatusCode, nil
	}
	var fail struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rb, &fail)
	code := ""
	for _, d := range fail.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
			break
		}
	}
	switch {
	case code == "UNREGISTERED":
		return "", resp.StatusCode, fmt.Errorf("push: FCM %d %s: %w", resp.StatusCode, code, ErrUnregistered)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return "", resp.StatusCode, fmt.Errorf("push: FCM status %d %s %s: %w", resp.StatusCode, fail.Error.Status, code, ErrUnavailable)
	}
	return "", resp.StatusCode, fmt.Errorf("push: FCM rejected (status %d %s %s): %s", resp.StatusCode, fail.Error.Status, code, fail.Error.Message)
}

// bearer returns a live access token, trading a fresh assertion when the
// cached one is missing or within a minute of expiring.
func (f *FCM) bearer(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if f.accessToken != "" && now.Before(f.expiresAt.Add(-time.Minute)) {
		return f.accessToken, nil
	}
	assertion := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   f.clientEmail,
		"scope": fcmScope,
		"aud":   f.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if f.keyID != "" {
		assertion.Header["kid"] = f.keyID
	}
	signed, err := assertion.SignedString(f.key)
	if err != nil {
		return "", fmt.Errorf("push: sign FCM assertion: %w", err)
	}
	form := url.Values{"grant_type": {jwtBearerGrant}, "assertion": {signed}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("push: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("push: token exchange failed: %v: %w", err, ErrUnavailable)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("push: token exchange status %d: %w", resp.StatusCode, ErrUnavailable)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(rb, &tok); err != nil || resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return "", fmt.Errorf("push: token exchange refused (status %d): %s", resp.StatusCode, tok.Error)
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	f.accessToken = tok.AccessToken
	f.expiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	return f.accessToken, nil
}

// forget drops a cached access token FCM refused, unless another caller has
// already replaced it.
func (f *FCM) forget(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accessToken == token {
		f.accessToken = ""
	}
}
