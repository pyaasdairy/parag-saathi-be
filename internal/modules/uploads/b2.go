package uploads

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// b2Client is a minimal Backblaze B2 *native-API* client covering exactly what
// the private media seam needs: authorize, ensure-bucket, per-upload URLs and
// short-lived download authorizations. The native API (not the S3-compatible
// one) is used deliberately — it accepts the master application key, which the
// S3 endpoint does not. Keys live ONLY in backend env; the app never sees them.
type b2Client struct {
	keyID  string
	appKey string
	bucket string

	httpc *http.Client

	mu         sync.Mutex
	auth       *b2Auth
	authAt     time.Time
	bucketID   string
	bucketName string
}

type b2Auth struct {
	AccountID   string `json:"accountId"`
	APIURL      string `json:"apiUrl"`
	DownloadURL string `json:"downloadUrl"`
	Token       string `json:"authorizationToken"`
}

func newB2Client(keyID, appKey, bucket string) *b2Client {
	return &b2Client{
		keyID:  keyID,
		appKey: appKey,
		bucket: bucket,
		httpc:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *b2Client) configured() bool { return c.keyID != "" && c.appKey != "" && c.bucket != "" }

// authorize returns a cached account authorization (B2 tokens last 24h; we
// refresh after 20h). Callers hold no lock.
func (c *b2Client) authorize(ctx context.Context) (*b2Auth, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.auth != nil && time.Since(c.authAt) < 20*time.Hour {
		return c.auth, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.backblazeb2.com/b2api/v2/b2_authorize_account", nil)
	if err != nil {
		return nil, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(c.keyID + ":" + c.appKey))
	req.Header.Set("Authorization", "Basic "+basic)
	res, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("b2 authorize: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("b2 authorize: HTTP %d", res.StatusCode)
	}
	var a b2Auth
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		return nil, err
	}
	c.auth = &a
	c.authAt = time.Now()
	return &a, nil
}

// call POSTs a native-API operation against the account's apiUrl.
func (c *b2Client) call(ctx context.Context, op string, body any, out any) error {
	a, err := c.authorize(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.APIURL+"/b2api/v2/"+op, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", a.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("b2 %s: %w", op, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		// Token expired early — drop the cache so the next call re-authorizes.
		c.mu.Lock()
		c.auth = nil
		c.mu.Unlock()
		return fmt.Errorf("b2 %s: unauthorized", op)
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		return fmt.Errorf("b2 %s: HTTP %d %s %s", op, res.StatusCode, e.Code, e.Message)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// ensureBucket resolves (and lazily creates, allPrivate) the media bucket.
func (c *b2Client) ensureBucket(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.bucketID != "" {
		id := c.bucketID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	a, err := c.authorize(ctx)
	if err != nil {
		return "", err
	}
	var list struct {
		Buckets []struct {
			BucketID   string `json:"bucketId"`
			BucketName string `json:"bucketName"`
		} `json:"buckets"`
	}
	if err := c.call(ctx, "b2_list_buckets", map[string]any{
		"accountId":  a.AccountID,
		"bucketName": c.bucket,
	}, &list); err != nil {
		return "", err
	}
	for _, b := range list.Buckets {
		if b.BucketName == c.bucket {
			c.mu.Lock()
			c.bucketID, c.bucketName = b.BucketID, b.BucketName
			c.mu.Unlock()
			return b.BucketID, nil
		}
	}
	// Not found — create it PRIVATE (media is KYC-adjacent; never public).
	var created struct {
		BucketID string `json:"bucketId"`
	}
	if err := c.call(ctx, "b2_create_bucket", map[string]any{
		"accountId":  a.AccountID,
		"bucketName": c.bucket,
		"bucketType": "allPrivate",
	}, &created); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.bucketID, c.bucketName = created.BucketID, c.bucket
	c.mu.Unlock()
	return created.BucketID, nil
}

// uploadTarget mints a one-shot upload URL + token for the client app. The app
// POSTs the file bytes straight to B2 (no key exposure — the token is scoped
// to this bucket and expires within 24h).
func (c *b2Client) uploadTarget(ctx context.Context) (uploadURL, token string, err error) {
	bucketID, err := c.ensureBucket(ctx)
	if err != nil {
		return "", "", err
	}
	var out struct {
		UploadURL string `json:"uploadUrl"`
		Token     string `json:"authorizationToken"`
	}
	if err := c.call(ctx, "b2_get_upload_url", map[string]any{"bucketId": bucketID}, &out); err != nil {
		return "", "", err
	}
	return out.UploadURL, out.Token, nil
}

// downloadURL returns a short-lived authorized URL for one stored file —
// the preview seam for the app (bucket stays private).
func (c *b2Client) downloadURL(ctx context.Context, fileName string, ttl time.Duration) (string, error) {
	bucketID, err := c.ensureBucket(ctx)
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"authorizationToken"`
	}
	if err := c.call(ctx, "b2_get_download_authorization", map[string]any{
		"bucketId":               bucketID,
		"fileNamePrefix":         fileName,
		"validDurationInSeconds": int(ttl.Seconds()),
	}, &out); err != nil {
		return "", err
	}
	a, err := c.authorize(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/file/%s/%s?Authorization=%s", a.DownloadURL, c.bucket, fileName, out.Token), nil
}
