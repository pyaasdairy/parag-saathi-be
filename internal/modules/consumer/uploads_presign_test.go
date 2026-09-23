package consumer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/modules/uploads"
)

type fakePresign struct {
	ok         bool
	prefix, ct string
}

func (f *fakePresign) Configured() bool { return f.ok }
func (f *fakePresign) Presign(_ context.Context, prefix, ct string) (uploads.Target, error) {
	f.prefix, f.ct = prefix, ct
	name := prefix + "/abc123.jpg"
	return uploads.Target{
		UploadURL: "https://pod-000.backblaze.test/b2api/v2/b2_upload_file/bucket/token",
		AuthToken: "one-shot-token", FileName: name, ViewURL: uploads.ViewPathPrefix + name,
	}, nil
}

// Contract C4: the handler's wire shape, without B2 keys.
func TestConsumerPresignShape(t *testing.T) {
	fp := &fakePresign{ok: true}
	h := &handler{svc: &service{presign: fp, log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	cid := primitive.NewObjectID()

	call := func(body string, withActor bool) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/uploads/presign", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withActor {
			req = req.WithContext(context.WithValue(req.Context(), consumerCtxKey, consumerActor{ID: cid.Hex()}))
		}
		rec := httptest.NewRecorder()
		h.consumerPresign(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := call(`{"kind":"complaint_photo","content_type":"image/jpeg"}`, true)
	if code != http.StatusOK {
		t.Fatalf("presign: %d %v", code, out)
	}
	if fp.prefix != "complaint-photo" || fp.ct != "image/jpeg" {
		t.Fatalf("core called with %q %q", fp.prefix, fp.ct)
	}
	if out["upload_url"] == "" || out["method"] != "POST" || out["file_url"] != "/api/v1/uploads/view/complaint-photo/abc123.jpg" {
		t.Fatalf("shape: %v", out)
	}
	hdr, _ := out["headers"].(map[string]any)
	for k, want := range map[string]string{
		"Authorization": "one-shot-token", "X-Bz-File-Name": "complaint-photo/abc123.jpg",
		"Content-Type": "image/jpeg", "X-Bz-Content-Sha1": "do_not_verify",
	} {
		if hdr[k] != want {
			t.Fatalf("header %s = %v want %s", k, hdr[k], want)
		}
	}

	if code, _ := call(`{"kind":"door_photo"}`, true); code != http.StatusOK || fp.prefix != "door-photo" || fp.ct != "image/jpeg" {
		t.Fatalf("door photo defaults: %d %q %q", code, fp.prefix, fp.ct)
	}
	if code, out := call(`{"kind":"selfie","content_type":"image/png"}`, true); code != http.StatusUnprocessableEntity || out["code"] != "INVALID_KIND" {
		t.Fatalf("unknown kind: %d %v", code, out)
	}
	if code, out := call(`{"kind":"door_photo","content_type":"application/pdf"}`, true); code != http.StatusUnprocessableEntity || out["code"] != "INVALID_CONTENT_TYPE" {
		t.Fatalf("non-image: %d %v", code, out)
	}
	if code, _ := call(`{"kind":"door_photo"}`, false); code != http.StatusUnauthorized {
		t.Fatalf("no actor: %d", code)
	}
	fp.ok = false
	if code, out := call(`{"kind":"door_photo"}`, true); code != http.StatusServiceUnavailable || out["code"] != "MEDIA_UNAVAILABLE" {
		t.Fatalf("unconfigured: %d %v", code, out)
	}
}
