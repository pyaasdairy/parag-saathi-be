package uploads

import (
	"context"
	"errors"
	"os"
)

// ViewPathPrefix is the authenticated backend path a stored file is referenced
// by on records (view_url / file_url); GET /uploads/view/{name} resolves it.
const ViewPathPrefix = "/api/v1/uploads/view/"

// ErrNotConfigured: B2_KEY_ID / B2_APP_KEY are unset, so no upload can be minted.
var ErrNotConfigured = errors.New("media storage is not configured")

// Presigner is the presign core (one-shot B2 upload target + canonical file
// name) for modules that mint uploads for their own callers, such as the
// consumer app's complaint and door photos. Same client, same naming and the
// same env as the /uploads/presign handler; nothing is duplicated.
type Presigner struct{ b2 *b2Client }

// NewPresigner builds the core from the B2_* env, like Register does.
func NewPresigner() *Presigner {
	return &Presigner{b2: newB2Client(
		os.Getenv("B2_KEY_ID"),
		os.Getenv("B2_APP_KEY"),
		envOr("B2_BUCKET", "pyaas-saathi-media"),
	)}
}

// Configured reports whether B2 keys exist; nil-safe.
func (p *Presigner) Configured() bool { return p != nil && p.b2 != nil && p.b2.configured() }

// Target is one minted upload: where to POST the bytes, the one-shot token,
// the file name B2 stores it under and the view path to keep on the record.
type Target struct {
	UploadURL string
	AuthToken string
	FileName  string
	ViewURL   string
}

// Presign mints an upload target under prefix (sanitised to [a-z0-9-]) with an
// extension derived from contentType, exactly as the handler does.
func (p *Presigner) Presign(ctx context.Context, prefix, contentType string) (Target, error) {
	if !p.Configured() {
		return Target{}, ErrNotConfigured
	}
	name := sanitizePrefix(prefix) + "/" + randomHex(16) + extFor(contentType)
	uploadURL, token, err := p.b2.uploadTarget(ctx)
	if err != nil {
		return Target{}, err
	}
	return Target{UploadURL: uploadURL, AuthToken: token, FileName: name, ViewURL: ViewPathPrefix + name}, nil
}
