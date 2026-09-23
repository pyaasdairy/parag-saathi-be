package consumer

import (
	"context"
	"net/http"
	"strings"

	"github.com/pyaas/saathi-backend/internal/modules/uploads"
)

// Consumer presign (contract C4): POST /consumer/uploads/presign.
//
// The member's complaint and door photos go to the same private B2 bucket the
// operator apps use, through the uploads module's presign core. The app then
// sends the bytes straight to B2 with the returned method and headers and
// stores file_url (an authenticated view path, never a file:// path) on the
// complaint / address record; proofPhotoURL and the admin CRM already resolve
// that prefix.

// presignCore is what this handler needs from the uploads seam; an interface so
// the handler shape is testable without B2 keys.
type presignCore interface {
	Configured() bool
	Presign(ctx context.Context, prefix, contentType string) (uploads.Target, error)
}

func newConsumerPresigner() presignCore { return uploads.NewPresigner() }

// consumerUploadKinds maps the wire kind to the bucket prefix. A closed set: a
// member cannot choose where in the bucket a file lands.
var consumerUploadKinds = map[string]string{
	"complaint_photo": "complaint-photo",
	"door_photo":      "door-photo",
}

type consumerPresignRequest struct {
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
}

type consumerPresignResponse struct {
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	FileURL   string            `json:"file_url"`
}

func (h *handler) consumerPresign(w http.ResponseWriter, r *http.Request) {
	if _, aerr := actorID(r); aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in consumerPresignRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	prefix, ok := consumerUploadKinds[strings.TrimSpace(in.Kind)]
	if !ok {
		writeErr(w, errUnprocessable("INVALID_KIND", "kind must be complaint_photo or door_photo"))
		return
	}
	ct := strings.ToLower(strings.TrimSpace(in.ContentType))
	if ct == "" {
		ct = "image/jpeg"
	}
	if !strings.HasPrefix(ct, "image/") {
		writeErr(w, errUnprocessable("INVALID_CONTENT_TYPE", "only images can be uploaded here"))
		return
	}
	if h.svc.presign == nil || !h.svc.presign.Configured() {
		writeErr(w, &apiError{status: http.StatusServiceUnavailable, Code: "MEDIA_UNAVAILABLE", Message: "photo upload is not available right now"})
		return
	}
	t, err := h.svc.presign.Presign(r.Context(), prefix, ct)
	if err != nil {
		h.svc.log.WarnContext(r.Context(), "consumer presign failed", "err", err)
		writeErr(w, &apiError{status: http.StatusServiceUnavailable, Code: "MEDIA_UNAVAILABLE", Message: "photo upload is not available right now - try again"})
		return
	}
	// B2's native upload endpoint (b2_upload_file) accepts only POST with these
	// headers, the same set the Saathi StorageApi sends; the app must use the
	// method and headers returned here rather than assume them.
	writeJSON(w, http.StatusOK, consumerPresignResponse{
		UploadURL: t.UploadURL,
		Method:    http.MethodPost,
		Headers: map[string]string{
			"Authorization":     t.AuthToken,
			"X-Bz-File-Name":    t.FileName,
			"Content-Type":      ct,
			"X-Bz-Content-Sha1": "do_not_verify",
		},
		FileURL: t.ViewURL,
	})
}
