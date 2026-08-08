package consumer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// CONSUMER CATALOG IMAGES — a PUBLIC, read-only proxy in front of the PRIVATE
// Backblaze B2 media bucket, scoped to product art only.
//
// Why a proxy and not a public bucket / a baked signed URL:
//   - The consumer app renders a product photo as <Image source={{uri}}> — a
//     plain GET with NO auth header — so the URL must load unauthenticated.
//   - The media bucket stays allPrivate (it also holds KYC/field photos), so we
//     never expose a public bucket.
//   - A signed URL baked into the seed would EXPIRE (B2 download auth ≤ 7 days),
//     so seeded data can't carry one. Instead the seed stores a STABLE path
//     ("catalog/img/<file>") and THIS route mints a fresh short-lived B2 token
//     per request and 302-redirects to it — the public URL never expires.
//
// Safety: the route is unauthenticated (it has to be), but it is hard-scoped to
// the catalogImagePrefix at BOTH layers — the handler refuses any nested/rooted
// name, and the B2 download authorization itself is issued for that prefix only.
// So it can serve product art and NOTHING else (never profile/ or KYC files),
// no matter what {name} a caller supplies.

// catalogImagePrefix is the ONLY key prefix this route serves from the bucket.
const catalogImagePrefix = "catalog/"

// b2DownloadClient is a tiny, download-only Backblaze B2 native-API client. It
// mirrors the private media seam (internal/modules/uploads/b2.go) but needs only
// authorize + a prefix-scoped download authorization — never upload/create.
// Same env as uploads (B2_KEY_ID / B2_APP_KEY / B2_BUCKET); keys stay
// server-side and the bucket stays allPrivate.
type b2DownloadClient struct {
	keyID, appKey, bucket string
	httpc                 *http.Client

	mu     sync.Mutex
	apiURL string
	dlURL  string
	token  string // account authorization (broad) — NEVER handed to a client
	authAt time.Time

	bucketID  string
	prefixTok string // download auth SCOPED to catalogImagePrefix — safe for clients
	prefixAt  time.Time
}

func newB2DownloadClient() *b2DownloadClient {
	bucket := os.Getenv("B2_BUCKET")
	if bucket == "" {
		bucket = "pyaas-saathi-media"
	}
	return &b2DownloadClient{
		keyID:  os.Getenv("B2_KEY_ID"),
		appKey: os.Getenv("B2_APP_KEY"),
		bucket: bucket,
		httpc:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *b2DownloadClient) configured() bool {
	return c.keyID != "" && c.appKey != "" && c.bucket != ""
}

// authorize refreshes the account token (B2 tokens last 24h; refresh at 20h) and
// resolves the bucket id once. Caller holds no lock.
func (c *b2DownloadClient) authorize(ctx context.Context) error {
	c.mu.Lock()
	fresh := c.token != "" && time.Since(c.authAt) < 20*time.Hour && c.bucketID != ""
	c.mu.Unlock()
	if fresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.backblazeb2.com/b2api/v2/b2_authorize_account", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.keyID+":"+c.appKey)))
	res, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("b2 authorize: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("b2 authorize: HTTP %d", res.StatusCode)
	}
	var a struct {
		AccountID   string `json:"accountId"`
		APIURL      string `json:"apiUrl"`
		DownloadURL string `json:"downloadUrl"`
		Token       string `json:"authorizationToken"`
	}
	if err := json.NewDecoder(res.Body).Decode(&a); err != nil {
		return err
	}
	// Resolve the bucket id (needed by b2_get_download_authorization).
	bucketID, err := c.resolveBucketID(ctx, a.APIURL, a.Token, a.AccountID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.apiURL, c.dlURL, c.token, c.authAt = a.APIURL, a.DownloadURL, a.Token, time.Now()
	c.bucketID = bucketID
	c.prefixTok = "" // force a fresh prefix token under the new account auth
	c.mu.Unlock()
	return nil
}

func (c *b2DownloadClient) resolveBucketID(ctx context.Context, apiURL, token, accountID string) (string, error) {
	body, _ := json.Marshal(map[string]any{"accountId": accountID, "bucketName": c.bucket})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/b2api/v2/b2_list_buckets", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("b2 list_buckets: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("b2 list_buckets: HTTP %d", res.StatusCode)
	}
	var out struct {
		Buckets []struct {
			BucketID   string `json:"bucketId"`
			BucketName string `json:"bucketName"`
		} `json:"buckets"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	for _, b := range out.Buckets {
		if b.BucketName == c.bucket {
			return b.BucketID, nil
		}
	}
	return "", fmt.Errorf("b2 bucket %q not found", c.bucket)
}

// prefixToken returns a download authorization SCOPED to catalogImagePrefix
// (cached ~20h). Because it is prefix-scoped, it is safe to place in a
// client-facing URL: it can read product art and nothing else.
func (c *b2DownloadClient) prefixToken(ctx context.Context) (string, error) {
	if err := c.authorize(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.prefixTok != "" && time.Since(c.prefixAt) < 20*time.Hour {
		tok := c.prefixTok
		c.mu.Unlock()
		return tok, nil
	}
	apiURL, token, bucketID := c.apiURL, c.token, c.bucketID
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"bucketId":               bucketID,
		"fileNamePrefix":         catalogImagePrefix,
		"validDurationInSeconds": 86400, // 24h; refreshed at 20h
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/b2api/v2/b2_get_download_authorization", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("b2 download_authorization: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("b2 download_authorization: HTTP %d", res.StatusCode)
	}
	var out struct {
		Token string `json:"authorizationToken"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.prefixTok, c.prefixAt = out.Token, time.Now()
	c.mu.Unlock()
	return out.Token, nil
}

// signedURL builds a client-usable, prefix-scoped download URL for one catalog
// file (name is a bare filename under catalogImagePrefix).
func (c *b2DownloadClient) signedURL(ctx context.Context, name string) (string, error) {
	tok, err := c.prefixToken(ctx)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	dlURL, bucket := c.dlURL, c.bucket
	c.mu.Unlock()
	return fmt.Sprintf("%s/file/%s/%s%s?Authorization=%s", dlURL, bucket, catalogImagePrefix, name, tok), nil
}

// catalogImage is the PUBLIC image proxy: GET /consumer/catalog/img/{name} →
// 302 to a short-lived B2 download URL for catalog/{name}. Unauthenticated by
// necessity (<Image> sends no headers); scoped to product art by the flat-name
// guard here and the prefix-scoped B2 token.
func (h *handler) catalogImage(w http.ResponseWriter, r *http.Request) {
	if h.svc.b2img == nil || !h.svc.b2img.configured() {
		writeJSON(w, http.StatusServiceUnavailable, &apiError{Code: "STORAGE_UNAVAILABLE", Message: "image storage not configured"})
		return
	}
	name := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	// Only a bare filename directly under catalog/ — no traversal, no nesting.
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
		writeJSON(w, http.StatusBadRequest, &apiError{Code: "INVALID_NAME", Message: "invalid image name"})
		return
	}
	url, err := h.svc.b2img.signedURL(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, &apiError{Code: "IMAGE_UNAVAILABLE", Message: "image unavailable"})
		return
	}
	// The bytes are immutable per file name; let clients + CDNs cache the redirect.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.Redirect(w, r, url, http.StatusFound)
}
