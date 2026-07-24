// Package uploads implements the private media seam (photos captured in the
// field: van pickup, plant intake, KYC/profile). Storage is a PRIVATE
// Backblaze B2 bucket; the B2 keys live only in backend env
// (B2_KEY_ID / B2_APP_KEY / B2_BUCKET). The app:
//
//  1. POST /uploads/presign {prefix, content_type}
//     → {upload_url, auth_token, file_name, view_url}
//     then POSTs the bytes STRAIGHT to B2 with the one-shot token.
//  2. Stores view_url (an authenticated backend path) on the record.
//  3. GET /uploads/view/{name...} → 302 redirect to a short-lived (10 min)
//     authorized B2 download URL — so previews render in-app while the
//     bucket itself stays private.
package uploads

import (
	"crypto/rand"
	"errors"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

type handler struct {
	b2  *b2Client
	log *slog.Logger
}

// Register mounts the uploads module under /uploads on the /api/v1 subtree.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "uploads"))
	h := &handler{
		b2: newB2Client(
			os.Getenv("B2_KEY_ID"),
			os.Getenv("B2_APP_KEY"),
			envOr("B2_BUCKET", "pyaas-saathi-media"),
		),
		log: log,
	}
	if !h.b2.configured() {
		log.Warn("B2 media storage not configured (B2_KEY_ID/B2_APP_KEY unset) — /uploads will 503")
	}

	r.Route("/uploads", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		r.Use(middleware.RequireSession)
		r.Post("/presign", h.presign)
		r.Get("/view/*", h.view)
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type presignRequest struct {
	Prefix      string `json:"prefix"`
	ContentType string `json:"content_type"`
}

type presignResponse struct {
	UploadURL string `json:"upload_url"`
	AuthToken string `json:"auth_token"`
	FileName  string `json:"file_name"`
	ViewURL   string `json:"view_url"`
}

// presign mints a one-shot B2 upload target + the canonical file name.
func (h *handler) presign(w http.ResponseWriter, r *http.Request) {
	if !h.b2.configured() {
		httpx.Error(w, r, httpx.Internal(errors.New("media storage is not configured")))
		return
	}
	var req presignRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	prefix := sanitizePrefix(req.Prefix)
	name := prefix + "/" + randomHex(16) + extFor(req.ContentType)

	uploadURL, token, err := h.b2.uploadTarget(r.Context())
	if err != nil {
		h.log.Error("b2 presign failed", slog.String("err", err.Error()))
		httpx.Error(w, r, httpx.Internal(errors.New("media storage unavailable — try again")))
		return
	}
	httpx.JSON(w, http.StatusOK, presignResponse{
		UploadURL: uploadURL,
		AuthToken: token,
		FileName:  name,
		ViewURL:   "/api/v1/uploads/view/" + name,
	})
}

// view redirects to a short-lived authorized download URL for one file.
func (h *handler) view(w http.ResponseWriter, r *http.Request) {
	if !h.b2.configured() {
		httpx.Error(w, r, httpx.Internal(errors.New("media storage is not configured")))
		return
	}
	name := chi.URLParam(r, "*")
	if name == "" || strings.Contains(name, "..") {
		httpx.Error(w, r, httpx.BadRequest("INVALID_NAME", "file name required"))
		return
	}
	url, err := h.b2.downloadURL(r.Context(), name, 10*time.Minute)
	if err != nil {
		h.log.Error("b2 view failed", slog.String("err", err.Error()))
		httpx.Error(w, r, httpx.Internal(errors.New("media storage unavailable — try again")))
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// sanitizePrefix keeps prefixes to a safe, flat vocabulary.
func sanitizePrefix(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	var b strings.Builder
	for _, c := range p {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "media"
	}
	return b.String()
}

func extFor(contentType string) string {
	switch contentType {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/heic", "image/heif":
		return ".heic"
	default:
		return ".jpg"
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000")))
	}
	return hex.EncodeToString(b)
}
