package consumer

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// Rider identity + self-service — GET /me, the non-delivery reason catalog,
// KYC documents, support concerns, referral and emergency contacts.
// (RIDER_API.md §2.6, §3.1, §3.10–§3.13.)
//
// Two kinds of payload live in this file and it matters which is which:
//
//   - CONFIGURATION (the ND reason catalog, the document checklist, the support
//     concern types). These are product copy the server owns; they are static
//     by design and each mirrors the client's own offline fallback list
//     code-for-code, so a rider working through a dead spot and a rider on
//     4G never see a different set of choices — and a POST can never be
//     rejected for carrying a code the client's fallback offered.
//
//   - RECORDS (the profile, uploaded documents, concerns, referrals, contacts).
//     These are read from what is actually stored for THIS rider and nothing
//     else. Where nothing is stored yet the honest answer is an empty list or a
//     zeroed record with a truthful status — never a plausible-looking sample.
//     That is the whole reason this surface exists.
//
// Identity comes from riderActor(r).PartyID only; every query below is keyed by
// it, so one rider can never read or write another's profile, documents,
// concerns or contacts.
// ─────────────────────────────────────────────────────────────────────────────

// Input bounds. Free text a rider types reaches a human reviewer, so it is
// capped rather than trusted; a photo ref is a view URL minted by our own
// presign seam, so 512 chars is generous for one.
const (
	riderMaxRefLen     = 512
	riderMaxLabelLen   = 40
	riderMinConcernLen = 5
	riderMaxConcernLen = 1000
	riderMaxDocFields  = 8
	riderMaxFieldKey   = 64
	riderMaxFieldValue = 128
	// At most two rider-owned emergency numbers, matching the two slots the
	// app renders (Emergency Contact I / II).
	riderMaxOwnContacts = 2
)

// ── document catalog ────────────────────────────────────────────────────────

// Document statuses. PENDING is a synthetic state: it is what a type reads as
// when the rider has not uploaded it yet, so the checklist is always complete.
const (
	riderDocPending  = "PENDING"
	riderDocUploaded = "UPLOADED"
	riderDocVerified = "VERIFIED"
	riderDocRejected = "REJECTED"
)

// riderDocumentSpec is one row of the KYC checklist. Required drives
// documents_verified on the profile — SIGNATURE is a consent artefact, not an
// identity or payout document, so it does not hold a rider's verification back.
type riderDocumentSpec struct {
	Type     string
	Title    string
	Hint     string
	Required bool
}

// riderDocumentCatalog is the exact list of 7 types the client renders, in the
// order it renders them. Titles match the app's own copy so the screen does not
// visibly re-label itself the moment the endpoint goes live.
var riderDocumentCatalog = []riderDocumentSpec{
	{"AADHAAR_FRONT", "Aadhaar card (front)", "The photo side — your name must match your bank account", true},
	{"AADHAAR_BACK", "Aadhaar card (back)", "The address side, with every number readable", true},
	{"PAN", "PAN card", "Required by law before we can pay you", true},
	{"DL", "Driving licence", "Must be valid for the vehicle you ride", true},
	{"CHEQUE", "Cancelled cheque", "Needed to credit your monthly payout", true},
	{"SELFIE", "Selfie / profile photo", "Used to recognise you at the centre each morning", true},
	{"SIGNATURE", "Signature (e-verify T&C)", "Must be clearly visible on plain white paper", false},
}

// riderDocumentSpecFor resolves a submitted type against the catalog. An
// unknown type is rejected rather than stored: a document nobody reviews is
// worse than no document, and the checklist would never show it.
func riderDocumentSpecFor(t string) (riderDocumentSpec, bool) {
	t = strings.ToUpper(strings.TrimSpace(t))
	for _, spec := range riderDocumentCatalog {
		if spec.Type == t {
			return spec, true
		}
	}
	return riderDocumentSpec{}, false
}

// riderDocumentRecord is one stored KYC document. status/rejection_reason are
// written by a REVIEWER, not by the rider — the rider only ever moves a
// document to UPLOADED.
type riderDocumentRecord struct {
	RiderPartyID string            `bson:"rider_party_id"`
	Type         string            `bson:"type"`
	Status       string            `bson:"status"`
	PhotoRef     string            `bson:"photo_ref,omitempty"`
	Fields       map[string]string `bson:"fields,omitempty"`
	// RejectionReason is shown to the rider VERBATIM, so reviewers write it in
	// plain language ("The photo was blurred — please re-upload a clear image").
	RejectionReason string     `bson:"rejection_reason,omitempty"`
	UploadedAt      time.Time  `bson:"uploaded_at,omitempty"`
	ReviewedAt      *time.Time `bson:"reviewed_at,omitempty"`
	CreatedAt       time.Time  `bson:"created_at,omitempty"`
	UpdatedAt       time.Time  `bson:"updated_at,omitempty"`
}

type riderDocumentView struct {
	Type            string `json:"type"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	RejectionReason string `json:"rejection_reason,omitempty"`
	UploadedAt      string `json:"uploaded_at,omitempty"`
	PhotoRef        string `json:"photo_ref,omitempty"`
	Hint            string `json:"hint,omitempty"`
}

type riderDocumentUploadRequest struct {
	Type     string         `json:"type"`
	PhotoRef string         `json:"photo_ref"`
	Fields   map[string]any `json:"fields"`
}

// ── support catalog ─────────────────────────────────────────────────────────

// Concern lifecycle. A rider only ever creates OPEN; support moves it on.
const (
	riderConcernOpen       = "OPEN"
	riderConcernInProgress = "IN_PROGRESS"
	riderConcernResolved   = "RESOLVED"
	riderConcernClosed     = "CLOSED"
)

type riderSupportTypeSpec struct {
	Code       string
	Label      string
	NeedsPhoto bool
}

// riderSupportTypeCatalog carries the six categories the brief names plus
// INVENTORY, because the client's offline fallback list offers INVENTORY too —
// a code the app can hand a rider must be a code this server accepts, or the
// concern raised on a bad connection dies with a 400 the rider cannot fix.
//
// NeedsPhoto is a CLIENT HINT ("attach a photo, it will be resolved faster").
// It is deliberately NOT enforced on POST: the app does not send photo_ref at
// all today, so enforcing it here would reject every inventory and safety
// concern — the two we least want a rider to give up on.
var riderSupportTypeCatalog = []riderSupportTypeSpec{
	{"PENALTY", "Penalty dispute", false},
	{"PAYMENT", "Payment / per-diem issue", false},
	{"INVENTORY", "Inventory shortage", true},
	{"APP", "App not working", false},
	{"CUSTOMER", "Customer behaviour", false},
	{"SAFETY", "Safety or accident", true},
	{"OTHER", "Something else", false},
}

func riderSupportTypeFor(code string) (riderSupportTypeSpec, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, s := range riderSupportTypeCatalog {
		if s.Code == code {
			return s, true
		}
	}
	return riderSupportTypeSpec{}, false
}

type riderSupportTypeView struct {
	Code       string `json:"code"`
	Label      string `json:"label"`
	NeedsPhoto bool   `json:"needs_photo"`
}

// riderSupportRecord is one concern the rider raised.
//
// `type` carries the CODE, matching riderConcernDoc in rider_ops_money.go: a
// penalty dispute lands in this same collection and must render on this same
// screen, so the two writers have to agree on the field. TypeLabel is stored
// beside it so a later re-labelling of the catalog cannot silently rewrite the
// history of a ticket support has already answered; a record without one (a
// penalty dispute) falls back to the catalog at read time.
type riderSupportRecord struct {
	ConcernID    string    `bson:"concern_id"`
	RiderPartyID string    `bson:"rider_party_id"`
	TypeCode     string    `bson:"type"`
	TypeLabel    string    `bson:"type_label,omitempty"`
	Title        string    `bson:"title"`
	Description  string    `bson:"description"`
	PhotoRef     string    `bson:"photo_ref,omitempty"`
	DeliveryID   string    `bson:"delivery_id,omitempty"`
	Status       string    `bson:"status"`
	RaisedAt     time.Time `bson:"raised_at"`
	LastMessage  string    `bson:"last_message,omitempty"`
	ClosingNote  string    `bson:"closing_note,omitempty"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

type riderSupportConcernView struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	RaisedAt    string `json:"raised_at"`
	LastMessage string `json:"last_message,omitempty"`
	ClosingNote string `json:"closing_note,omitempty"`
}

type riderConcernRequest struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	PhotoRef    string `json:"photo_ref"`
	TaskID      string `json:"task_id"`
}

type riderConcernCreatedResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// ── non-delivery reasons ────────────────────────────────────────────────────

type riderNDReasonView struct {
	Code        string `json:"code"`
	Label       string `json:"label"`
	NeedsCall   bool   `json:"needs_call"`
	NeedsPhoto  bool   `json:"needs_photo"`
	NeedsRemark bool   `json:"needs_remark"`
}

// riderNDReasonCatalog mirrors kDefaultNdReasons in lib/api/rider_api.dart
// CODE-FOR-CODE and flag-for-flag. The client ships that list as its offline
// fallback and gates the "Mark not delivered" button on these three flags; if
// the two lists drifted, the evidence a rider is forced to collect would depend
// on whether the config fetch happened to succeed. Change one, change both.
var riderNDReasonCatalog = []riderNDReasonView{
	{Code: "CUSTOMER_UNAVAILABLE", Label: "Customer not available", NeedsCall: true, NeedsPhoto: true},
	{Code: "ADDRESS_NOT_FOUND", Label: "Address not found", NeedsCall: true, NeedsPhoto: true},
	{Code: "CUSTOMER_REFUSED", Label: "Customer refused the order", NeedsRemark: true},
	{Code: "GATE_CLOSED", Label: "Society gate / lift closed", NeedsPhoto: true},
	{Code: "CUSTOMER_ON_HOLD", Label: "Customer wants to hold today", NeedsRemark: true},
	{Code: "NO_CHANGE", Label: "Customer had no change", NeedsRemark: true},
	{Code: "PRODUCT_SHORTAGE", Label: "Product shortage at the centre", NeedsRemark: true},
	{Code: "DAMAGED", Label: "Product damaged in transit", NeedsPhoto: true, NeedsRemark: true},
}

// ── profile ─────────────────────────────────────────────────────────────────

// Referral programme terms. Product config, stamped onto the rider's profile
// the first time it is read so a later change to the programme cannot
// retroactively rewrite what an individual rider was promised.
const (
	riderReferralRewardRupees   = 1500.0
	riderReferralMinPresentDays = 15
)

// riderProfileRecord is the rider-owned side of the profile: the few fields
// that have no home in the shared Saathi identity collections. Everything else
// on GET /me (name, phone, photo, centre, active) is read live from parties /
// role_assignments / org_units, which the operator console owns.
//
// The face_* fields are written by the enrolment endpoint in
// rider_ops_attendance.go; this file only READS them, to answer
// face_registered. They are decoded (never re-written) here, and every write
// below is a targeted $set, so the two files cannot clobber each other.
type riderProfileRecord struct {
	RiderPartyID           string     `bson:"rider_party_id"`
	PartnerCode            string     `bson:"partner_code,omitempty"`
	VehicleNumber          string     `bson:"vehicle_number,omitempty"`
	ReferralCode           string     `bson:"referral_code,omitempty"`
	ReferralRewardRupees   float64    `bson:"referral_reward_rupees,omitempty"`
	ReferralMinPresentDays int        `bson:"referral_min_present_days,omitempty"`
	JoinedAt               time.Time  `bson:"joined_at,omitempty"`
	FaceRegistered         bool       `bson:"face_registered,omitempty"`
	FacePhotoRef           string     `bson:"face_photo_ref,omitempty"`
	FaceEnrolledAt         *time.Time `bson:"face_enrolled_at,omitempty"`
	FaceEmbedding          []float64  `bson:"face_embedding,omitempty"`
	CreatedAt              time.Time  `bson:"created_at,omitempty"`
	UpdatedAt              time.Time  `bson:"updated_at,omitempty"`
}

// faceRegistered reports whether a reference selfie is actually on file — the
// flag that decides whether the app opens the ENROL flow or the VERIFY flow at
// the centre gate. face_photo_ref is the field riderEnrolFacePhoto
// (rider_ops_attendance.go) guards its first-enrolment-only rule on, so it is
// the authoritative one here; the others are accepted so a profile written by
// an earlier or later shape of that record still reads true.
//
// It is deliberately independent of the SELFIE KYC document: a KYC selfie
// carries no enrolment, so treating it as one would send the rider into a
// verification that can only fail.
func (p riderProfileRecord) faceRegistered() bool {
	return strings.TrimSpace(p.FacePhotoRef) != "" || p.FaceRegistered ||
		len(p.FaceEmbedding) > 0 ||
		(p.FaceEnrolledAt != nil && !p.FaceEnrolledAt.IsZero())
}

type riderProfileView struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	PartnerCode       string `json:"partner_code"`
	Phone             string `json:"phone"`
	PhotoRef          string `json:"photo_ref,omitempty"`
	CenterName        string `json:"center_name,omitempty"`
	CenterCode        string `json:"center_code,omitempty"`
	VehicleNumber     string `json:"vehicle_number,omitempty"`
	JoinedAt          string `json:"joined_at,omitempty"`
	ReferralCode      string `json:"referral_code,omitempty"`
	FaceRegistered    bool   `json:"face_registered"`
	DocumentsVerified bool   `json:"documents_verified"`
	AccountActive     bool   `json:"account_active"`
}

// riderIdentityRecord is the operator-side truth about one rider, assembled
// from the three shared collections the repository already holds. Nothing here
// is derived from the request — every field traces to a stored record.
type riderIdentityRecord struct {
	partyID    primitive.ObjectID
	name       string
	phone      string
	photoRef   string
	suspended  bool
	centreID   primitive.ObjectID
	centreName string
	centreCode string
	// dutyActive is the rider's ACTIVE, in-window STORE role assignment. The
	// app puts a blocking banner over the whole console when this is false.
	dutyActive bool
	joinedAt   time.Time
}

// ── referral ────────────────────────────────────────────────────────────────

// Referral states the client renders. EARNED is the only one that has actually
// paid out — it is the only one summed into `earned`.
const (
	riderReferralPending    = "PENDING"
	riderReferralSuccessful = "SUCCESSFUL"
	riderReferralEarned     = "EARNED"
)

// riderReferralRecord is one person this rider brought onto the platform.
// Written by whoever completes a referred rider's onboarding — never here.
type riderReferralRecord struct {
	ReferrerPartyID string     `bson:"referrer_party_id"`
	Name            string     `bson:"name"`
	Status          string     `bson:"status"`
	JoinedAt        *time.Time `bson:"joined_at,omitempty"`
	Amount          float64    `bson:"amount"`
}

type riderReferralEntryView struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	JoinedAt string  `json:"joined_at,omitempty"`
	Amount   float64 `json:"amount"`
}

type riderReferralView struct {
	Code           string                   `json:"code"`
	RewardAmount   float64                  `json:"reward_amount"`
	MinPresentDays int                      `json:"min_present_days"`
	Earned         float64                  `json:"earned"`
	Referrals      []riderReferralEntryView `json:"referrals"`
}

// ── emergency contacts ──────────────────────────────────────────────────────

// riderRelationSelf marks a contact the RIDER owns and may edit. Every other
// relation is a PYAAS-side number: read-only, with a call button.
const (
	riderRelationSelf   = "SELF"
	riderRelationCentre = "Centre"
)

type riderEmergencyContactDoc struct {
	Label string `bson:"label"`
	Phone string `bson:"phone"`
}

type riderEmergencyRecord struct {
	RiderPartyID string                     `bson:"rider_party_id"`
	Contacts     []riderEmergencyContactDoc `bson:"contacts"`
	UpdatedAt    time.Time                  `bson:"updated_at"`
}

type riderEmergencyContactView struct {
	Label    string `json:"label"`
	Phone    string `json:"phone"`
	Relation string `json:"relation"`
}

type riderEmergencyPutRequest struct {
	Contacts []struct {
		Label    string `json:"label"`
		Phone    string `json:"phone"`
		Relation string `json:"relation"`
	} `json:"contacts"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Handlers
// ─────────────────────────────────────────────────────────────────────────────

// riderMe — GET /consumer/delivery/rider/me. The rider's own ID card.
//
// Assembled live from real records on every call rather than cached on the
// profile doc: a revoked role assignment or a rejected document must show up on
// the very next screen open, not after a re-login.
func (h *handler) riderMe(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()
	ident, err := h.svc.repo.riderLoadIdentity(ctx, actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prof, err := h.svc.repo.riderEnsureProfile(ctx, ident)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	docs, err := h.svc.repo.riderDocumentsByType(ctx, ident.partyID.Hex())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	view := riderProfileView{
		ID:            ident.partyID.Hex(),
		Name:          ident.name,
		PartnerCode:   prof.PartnerCode,
		Phone:         ident.phone,
		PhotoRef:      ident.photoRef,
		CenterName:    ident.centreName,
		CenterCode:    ident.centreCode,
		VehicleNumber: prof.VehicleNumber,
		ReferralCode:  prof.ReferralCode,
		// documents_verified is an ALL-of over the REQUIRED types: a payout
		// runs on the weakest document, not the strongest.
		DocumentsVerified: riderDocumentsVerified(docs),
		FaceRegistered:    prof.faceRegistered(),
		// The one flag the operator actually controls. A rejected document is
		// surfaced through the documents list (with its reason, in the rider's
		// own words) — it does not silently ground a rider mid-shift; only
		// revoking the role assignment or suspending the party does that.
		AccountActive: ident.dutyActive && !ident.suspended,
	}
	if !prof.JoinedAt.IsZero() {
		view.JoinedAt = rfc3339(prof.JoinedAt)
	}
	httpx.JSON(w, http.StatusOK, view)
}

// riderNDReasons — GET /consumer/delivery/rider/nd-reasons. Server-owned
// configuration; see riderNDReasonCatalog for why it must not drift from the
// client's fallback list.
func (h *handler) riderNDReasons(w http.ResponseWriter, r *http.Request) {
	if _, err := riderActor(r); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderNDReasonCatalog)
}

// riderDocuments — GET /consumer/delivery/rider/documents. Always returns ALL
// seven types: the checklist is the point of the screen, so a type with nothing
// stored comes back as PENDING with its hint rather than being omitted.
func (h *handler) riderDocuments(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	stored, err := h.svc.repo.riderDocumentsByType(r.Context(), actor.PartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]riderDocumentView, 0, len(riderDocumentCatalog))
	for _, spec := range riderDocumentCatalog {
		v := riderDocumentView{Type: spec.Type, Title: spec.Title, Status: riderDocPending, Hint: spec.Hint}
		if rec, ok := stored[spec.Type]; ok {
			v.Status = riderDocStatus(rec.Status)
			v.PhotoRef = rec.PhotoRef
			v.RejectionReason = rec.RejectionReason
			at := rec.UploadedAt
			v.UploadedAt = rfc3339Ptr(&at)
		}
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderUploadDocument — POST /consumer/delivery/rider/documents.
//
// The rider may (re-)submit anything that is not already VERIFIED — that is
// exactly the loop a REJECTED document needs. Overwriting a VERIFIED document
// is refused: a verified KYC record is evidence a reviewer signed off on, and
// swapping the image under it after the fact would erase that.
func (h *handler) riderUploadDocument(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderDocumentUploadRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	spec, ok := riderDocumentSpecFor(req.Type)
	if !ok {
		httpx.Error(w, r, httpx.BadRequest("UNKNOWN_DOCUMENT_TYPE", "that document type is not on the checklist"))
		return
	}
	photoRef := strings.TrimSpace(req.PhotoRef)
	if photoRef == "" {
		httpx.Error(w, r, httpx.BadRequest("PHOTO_REQUIRED", "upload the photo first, then submit the document"))
		return
	}
	if len(photoRef) > riderMaxRefLen {
		httpx.Error(w, r, httpx.BadRequest("PHOTO_REF_TOO_LONG", "that photo reference is not valid"))
		return
	}
	fields, err := riderNormaliseDocFields(req.Fields)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.repo.riderStoreDocument(r.Context(), actor.PartyID, spec.Type, photoRef, fields); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, statusResponse{Status: riderDocUploaded})
}

// riderSupportTypes — GET /consumer/delivery/rider/support/types.
func (h *handler) riderSupportTypes(w http.ResponseWriter, r *http.Request) {
	if _, err := riderActor(r); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]riderSupportTypeView, 0, len(riderSupportTypeCatalog))
	for _, s := range riderSupportTypeCatalog {
		out = append(out, riderSupportTypeView{Code: s.Code, Label: s.Label, NeedsPhoto: s.NeedsPhoto})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderSupportConcerns — GET /consumer/delivery/rider/support/concerns. The
// rider's own tickets, newest first. A rider who has never raised one gets [],
// and the app renders its empty state — that is the correct answer, not a
// specimen ticket.
func (h *handler) riderSupportConcerns(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.repo.riderListConcerns(r.Context(), actor.PartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderRaiseConcern — POST /consumer/delivery/rider/support/concerns.
func (h *handler) riderRaiseConcern(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderConcernRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	spec, ok := riderSupportTypeFor(req.Type)
	if !ok {
		httpx.Error(w, r, httpx.BadRequest("UNKNOWN_CONCERN_TYPE", "pick one of the listed concern types"))
		return
	}
	desc := strings.TrimSpace(req.Description)
	if n := len([]rune(desc)); n < riderMinConcernLen {
		httpx.Error(w, r, httpx.BadRequest("DESCRIPTION_REQUIRED", "tell us what happened, in a line or two"))
		return
	} else if n > riderMaxConcernLen {
		httpx.Error(w, r, httpx.BadRequest("DESCRIPTION_TOO_LONG",
			fmt.Sprintf("keep the description under %d characters", riderMaxConcernLen)))
		return
	}
	photoRef := strings.TrimSpace(req.PhotoRef)
	if len(photoRef) > riderMaxRefLen {
		httpx.Error(w, r, httpx.BadRequest("PHOTO_REF_TOO_LONG", "that photo reference is not valid"))
		return
	}
	ctx := r.Context()
	// A concern may be filed AGAINST one of the rider's tasks. Confirm the task
	// is theirs before storing the link — 404, not 403, so a probe cannot learn
	// that someone else's delivery id exists.
	taskID := strings.TrimSpace(req.TaskID)
	if taskID != "" {
		owns, err := h.svc.repo.riderOwnsDelivery(ctx, actor.PartyID, taskID)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if !owns {
			httpx.Error(w, r, httpx.NotFound("delivery"))
			return
		}
	}
	now := time.Now().UTC()
	rec := riderSupportRecord{
		ConcernID:    newRiderOpsID("con"),
		RiderPartyID: actor.PartyID,
		TypeCode:     spec.Code,
		TypeLabel:    spec.Label,
		Title:        riderConcernTitle(desc),
		Description:  desc,
		PhotoRef:     photoRef,
		DeliveryID:   taskID,
		Status:       riderConcernOpen,
		RaisedAt:     now,
		UpdatedAt:    now,
	}
	if err := h.svc.repo.riderInsertSupportConcern(ctx, rec); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderConcernCreatedResponse{ID: rec.ConcernID, Status: rec.Status})
}

// riderReferral — GET /consumer/delivery/rider/referral. The rider's own code
// and terms, plus the people they actually brought in. No referrals stored →
// an empty list and earned 0, which is the truth for almost every rider on day
// one and renders as the app's "invite your first friend" state.
func (h *handler) riderReferral(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()
	ident, err := h.svc.repo.riderLoadIdentity(ctx, actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	prof, err := h.svc.repo.riderEnsureProfile(ctx, ident)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	entries, earned, err := h.svc.repo.riderListReferrals(ctx, ident.partyID.Hex())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, riderReferralView{
		Code:           prof.ReferralCode,
		RewardAmount:   round2(prof.ReferralRewardRupees),
		MinPresentDays: prof.ReferralMinPresentDays,
		Earned:         round2(earned),
		Referrals:      entries,
	})
}

// riderEmergencyContacts — GET /consumer/delivery/rider/emergency-contacts.
// The rider's own (editable) numbers first, then the PYAAS-side numbers for
// their centre, which are read-only.
func (h *handler) riderEmergencyContacts(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()
	ident, err := h.svc.repo.riderLoadIdentity(ctx, actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.repo.riderContactList(ctx, ident)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderSetEmergencyContacts — PUT /consumer/delivery/rider/emergency-contacts.
//
// Replaces ONLY the rider's own (relation SELF) numbers. The centre numbers are
// never stored on the rider's document — they are derived on read — so no
// payload, however it is relabelled, can overwrite or delete the number a rider
// calls after an accident.
func (h *handler) riderSetEmergencyContacts(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderEmergencyPutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	own := make([]riderEmergencyContactDoc, 0, riderMaxOwnContacts)
	seen := map[string]struct{}{}
	for _, c := range req.Contacts {
		rel := strings.ToUpper(strings.TrimSpace(c.Relation))
		if rel != "" && rel != riderRelationSelf {
			// A PYAAS-side row the client echoed back from the GET. Ignore it
			// rather than 400 — it is not an error, it is just not the rider's
			// to save.
			continue
		}
		phone, err := riderNormaliseMobile(c.Phone)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if _, dup := seen[phone]; dup {
			httpx.Error(w, r, httpx.BadRequest("DUPLICATE_CONTACT", "both emergency contacts should be different numbers"))
			return
		}
		seen[phone] = struct{}{}
		label := strings.TrimSpace(c.Label)
		if len([]rune(label)) > riderMaxLabelLen {
			httpx.Error(w, r, httpx.BadRequest("LABEL_TOO_LONG",
				fmt.Sprintf("keep the name under %d characters", riderMaxLabelLen)))
			return
		}
		if label == "" {
			label = "Emergency contact"
		}
		if len(own) == riderMaxOwnContacts {
			httpx.Error(w, r, httpx.BadRequest("TOO_MANY_CONTACTS",
				fmt.Sprintf("you can save at most %d emergency contacts", riderMaxOwnContacts)))
			return
		}
		own = append(own, riderEmergencyContactDoc{Label: label, Phone: phone})
	}
	ctx := r.Context()
	ident, err := h.svc.repo.riderLoadIdentity(ctx, actor)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.repo.riderSaveOwnContacts(ctx, ident.partyID.Hex(), own); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Echo the same shape GET returns (own + centre) so the screen can
	// re-render from the response without a second round trip.
	out, err := h.svc.repo.riderContactList(ctx, ident)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// Repository
// ─────────────────────────────────────────────────────────────────────────────

// riderProfilePartyOID turns the token's party id into the ObjectID the shared
// identity collections are keyed by. A token whose subject is not an ObjectID
// cannot address a party at all, so it is an authentication failure, not a 404.
func riderProfilePartyOID(actor auth.Actor) (primitive.ObjectID, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(actor.PartyID))
	if err != nil {
		return primitive.NilObjectID, httpx.Unauthorized("bad token subject")
	}
	return oid, nil
}

// riderLoadIdentity assembles the rider from parties + role_assignments +
// org_units. Read fresh on every call — a revoke must take effect immediately.
func (r *repository) riderLoadIdentity(ctx context.Context, actor auth.Actor) (riderIdentityRecord, error) {
	oid, err := riderProfilePartyOID(actor)
	if err != nil {
		return riderIdentityRecord{}, err
	}
	var p struct {
		FullName        string    `bson:"full_name"`
		Phone           string    `bson:"phone"`
		ProfilePhotoURL string    `bson:"profile_photo_url"`
		Status          string    `bson:"status"`
		CreatedAt       time.Time `bson:"created_at"`
	}
	if err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: oid}}).Decode(&p); err != nil {
		if isNoDocs(err) {
			return riderIdentityRecord{}, httpx.NotFound("rider")
		}
		return riderIdentityRecord{}, httpx.Internal(fmt.Errorf("load rider party: %w", err))
	}
	ident := riderIdentityRecord{
		partyID:   oid,
		name:      strings.TrimSpace(p.FullName),
		phone:     p.Phone,
		photoRef:  p.ProfilePhotoURL,
		suspended: p.Status == domain.PartyStatusSuspended,
	}

	// Oldest assignment first: the earliest valid_from is the day this person
	// became a rider, which is what "joined_at" means on their ID card.
	cur, err := r.roleAssignments.Find(ctx,
		bson.D{{Key: "party_id", Value: oid}, {Key: "role_code", Value: domain.RoleDeliveryRider}},
		options.Find().SetSort(bson.D{{Key: "valid_from", Value: 1}}).SetLimit(50))
	if err != nil {
		return riderIdentityRecord{}, httpx.Internal(fmt.Errorf("find rider assignments: %w", err))
	}
	var ras []domain.RoleAssignment
	if err := cur.All(ctx, &ras); err != nil {
		return riderIdentityRecord{}, httpx.Internal(fmt.Errorf("decode rider assignments: %w", err))
	}
	now := time.Now().UTC()
	var current *domain.RoleAssignment
	for i := range ras {
		ra := ras[i]
		if ident.joinedAt.IsZero() && !ra.ValidFrom.IsZero() {
			ident.joinedAt = ra.ValidFrom
		}
		if current == nil && ra.UsableAt(now) {
			current = &ras[i]
		}
	}
	if ident.joinedAt.IsZero() {
		// No dated assignment (legacy seed): the party record is still real
		// evidence of when this person was onboarded.
		ident.joinedAt = p.CreatedAt
	}
	centreID := primitive.NilObjectID
	switch {
	case current != nil:
		ident.dutyActive = true
		centreID = current.OrgUnitID
	case len(ras) > 0:
		// Revoked or expired: the console still needs to name the centre so the
		// blocking banner says WHERE to go and sort it out.
		centreID = ras[len(ras)-1].OrgUnitID
	default:
		// No rider assignment on record at all (a token outliving a deleted
		// grant): the org unit the token was minted against is the last honest
		// hint at which centre this is.
		if orgOID, e := primitive.ObjectIDFromHex(strings.TrimSpace(actor.OrgUnitID)); e == nil {
			centreID = orgOID
		}
	}
	if !centreID.IsZero() {
		var ou struct {
			Name string `bson:"name"`
			Code string `bson:"code"`
		}
		if err := r.orgUnits.FindOne(ctx, bson.D{{Key: "_id", Value: centreID}}).Decode(&ou); err != nil {
			if !isNoDocs(err) {
				return riderIdentityRecord{}, httpx.Internal(fmt.Errorf("load rider centre: %w", err))
			}
			// A dangling org reference must not blank the whole profile; the
			// centre fields simply stay empty.
		}
		ident.centreID, ident.centreName, ident.centreCode = centreID, ou.Name, ou.Code
	}
	return ident, nil
}

// riderEnsureProfile reads (creating on first touch) the rider-owned profile
// doc. Created lazily rather than at onboarding so this surface owns its own
// storage end to end and no rider can be missing one.
func (r *repository) riderEnsureProfile(ctx context.Context, ident riderIdentityRecord) (riderProfileRecord, error) {
	coll := r.riderColl(collRiderProfiles)
	key := ident.partyID.Hex()
	filter := bson.D{{Key: "rider_party_id", Value: key}}
	now := time.Now().UTC()

	// $setOnInsert-only upsert: two concurrent screen opens cannot create two
	// profiles (the unique rider_party_id index settles the race), and an
	// existing doc — including one the face-enrolment endpoint created first —
	// is left completely untouched.
	_, err := coll.UpdateOne(ctx, filter,
		bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}}},
		options.Update().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return riderProfileRecord{}, httpx.Internal(fmt.Errorf("create rider profile: %w", err))
	}
	var rec riderProfileRecord
	if err := coll.FindOne(ctx, filter).Decode(&rec); err != nil {
		return riderProfileRecord{}, httpx.Internal(fmt.Errorf("load rider profile: %w", err))
	}

	// Backfill only what is actually absent — never overwrite a value an
	// operator (or another endpoint) has already written.
	set := bson.D{}
	if strings.TrimSpace(rec.PartnerCode) == "" {
		rec.PartnerCode = riderPartnerCode(ident)
		set = append(set, bson.E{Key: "partner_code", Value: rec.PartnerCode})
	}
	if rec.JoinedAt.IsZero() && !ident.joinedAt.IsZero() {
		rec.JoinedAt = ident.joinedAt.UTC()
		set = append(set, bson.E{Key: "joined_at", Value: rec.JoinedAt})
	}
	if rec.ReferralRewardRupees <= 0 {
		rec.ReferralRewardRupees = riderReferralRewardRupees
		set = append(set, bson.E{Key: "referral_reward_rupees", Value: rec.ReferralRewardRupees})
	}
	if rec.ReferralMinPresentDays <= 0 {
		rec.ReferralMinPresentDays = riderReferralMinPresentDays
		set = append(set, bson.E{Key: "referral_min_present_days", Value: rec.ReferralMinPresentDays})
	}
	if len(set) > 0 {
		set = append(set, bson.E{Key: "updated_at", Value: now})
		if _, err := coll.UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: set}}); err != nil {
			return riderProfileRecord{}, httpx.Internal(fmt.Errorf("backfill rider profile: %w", err))
		}
	}
	if strings.TrimSpace(rec.ReferralCode) == "" {
		code, err := r.riderClaimReferralCode(ctx, ident)
		if err != nil {
			return riderProfileRecord{}, err
		}
		rec.ReferralCode = code
	}
	return rec, nil
}

// riderPartnerCode derives the human partner id printed on the rider's ID card:
// their centre's business code plus four stable characters of their party id.
// Deterministic and persisted on first read, so it never changes under a rider
// who has already shared it — and an operator-written partner_code always wins,
// because this only ever fills an empty field.
func riderPartnerCode(ident riderIdentityRecord) string {
	prefix := strings.ToUpper(strings.TrimSpace(ident.centreCode))
	if prefix == "" {
		prefix = "PYS"
	}
	hex := ident.partyID.Hex()
	return prefix + "-" + strings.ToUpper(hex[len(hex)-4:])
}

// riderReferralAlphabet drops the characters riders misread when they say a
// code out loud or copy it off a screen (0/O, 1/I).
const riderReferralAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// riderReferralSuffix mints the random tail of a referral code.
func riderReferralSuffix() string {
	b := make([]byte, 4)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(riderReferralAlphabet))))
		if err != nil {
			// A failed CSPRNG read must not block a rider's profile; the
			// unique index below is what actually guarantees distinctness.
			b[i] = riderReferralAlphabet[(time.Now().UTC().UnixNano()>>uint(8*i))%int64(len(riderReferralAlphabet))]
			continue
		}
		b[i] = riderReferralAlphabet[n.Int64()]
	}
	return string(b)
}

// riderReferralStem is the readable half of the code: the rider's own name, so
// the friend they hand it to recognises it. Names in Devanagari (or any script
// with no A–Z letters) fall back to the brand.
func riderReferralStem(name string) string {
	var b strings.Builder
	for _, ru := range strings.ToUpper(name) {
		if ru >= 'A' && ru <= 'Z' {
			b.WriteRune(ru)
		}
		if b.Len() == 6 {
			break
		}
	}
	if b.Len() < 3 {
		return "PYAAS"
	}
	return b.String()
}

// riderClaimReferralCode mints codes until one lands. The sparse unique index
// on referral_code is the arbiter — checking for a free code first and writing
// it second would hand the same code to two riders under a race.
func (r *repository) riderClaimReferralCode(ctx context.Context, ident riderIdentityRecord) (string, error) {
	coll := r.riderColl(collRiderProfiles)
	key := ident.partyID.Hex()
	// Match only while the code is still unset, so a concurrent claim that got
	// there first is never overwritten.
	filter := bson.D{
		{Key: "rider_party_id", Value: key},
		{Key: "referral_code", Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}},
	}
	for attempt := 0; attempt < 5; attempt++ {
		code := riderReferralStem(ident.name) + riderReferralSuffix()
		res, err := coll.UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: bson.D{
			{Key: "referral_code", Value: code},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}})
		if err != nil {
			if mongo.IsDuplicateKeyError(err) {
				continue // that code is taken; mint another
			}
			return "", httpx.Internal(fmt.Errorf("claim referral code: %w", err))
		}
		if res.MatchedCount == 0 {
			// Someone else claimed it between our read and this write — read
			// back the winner rather than fighting for it.
			var rec riderProfileRecord
			if err := coll.FindOne(ctx, bson.D{{Key: "rider_party_id", Value: key}}).Decode(&rec); err != nil {
				return "", httpx.Internal(fmt.Errorf("reload rider profile: %w", err))
			}
			return rec.ReferralCode, nil
		}
		return code, nil
	}
	return "", httpx.Internal(errors.New("could not mint a unique referral code"))
}

// riderDocumentsByType returns the rider's stored KYC documents keyed by type.
// An empty map is a perfectly good answer — the handler still renders all seven.
func (r *repository) riderDocumentsByType(ctx context.Context, riderPartyID string) (map[string]riderDocumentRecord, error) {
	cur, err := r.riderColl(collRiderDocuments).Find(ctx, bson.D{{Key: "rider_party_id", Value: riderPartyID}})
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider documents: %w", err))
	}
	var rows []riderDocumentRecord
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider documents: %w", err))
	}
	out := make(map[string]riderDocumentRecord, len(rows))
	for _, row := range rows {
		out[strings.ToUpper(strings.TrimSpace(row.Type))] = row
	}
	return out, nil
}

// riderDocStatus normalises a stored status onto the closed set the client
// switches on. An unrecognised value reads as UPLOADED — a record exists, so
// PENDING ("you have not sent this yet") would be a lie to the rider.
func riderDocStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case riderDocPending:
		return riderDocPending
	case riderDocVerified:
		return riderDocVerified
	case riderDocRejected:
		return riderDocRejected
	default:
		return riderDocUploaded
	}
}

// riderDocumentsVerified is the profile's documents_verified flag: every
// REQUIRED type present AND verified.
func riderDocumentsVerified(stored map[string]riderDocumentRecord) bool {
	for _, spec := range riderDocumentCatalog {
		if !spec.Required {
			continue
		}
		rec, ok := stored[spec.Type]
		if !ok || riderDocStatus(rec.Status) != riderDocVerified {
			return false
		}
	}
	return true
}

// riderNormaliseDocFields bounds the optional structured fields that ride along
// with a document (e.g. the licence number). Values are stringified because
// they are transcription aids for a reviewer, never anything we compute on.
func riderNormaliseDocFields(in map[string]any) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > riderMaxDocFields {
		return nil, httpx.BadRequest("TOO_MANY_FIELDS", "too many fields on this document")
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > riderMaxFieldKey {
			return nil, httpx.BadRequest("INVALID_FIELD", "one of the document fields is not valid")
		}
		if v == nil {
			continue
		}
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		if len(s) > riderMaxFieldValue {
			return nil, httpx.BadRequest("FIELD_TOO_LONG", "one of the document fields is too long")
		}
		out[k] = s
	}
	return out, nil
}

// riderStoreDocument records an upload as UPLOADED, for any state EXCEPT
// VERIFIED.
//
// The `status != VERIFIED` clause sits in the FILTER of an upserting update, so
// the check and the write are one atomic operation: when a VERIFIED record
// exists the filter misses, Mongo attempts an insert, and the unique
// (rider_party_id, type) index rejects it — which is precisely the conflict we
// want to report. A read-then-write here would let a double-tap slip a new
// image under a document a reviewer had just approved.
func (r *repository) riderStoreDocument(ctx context.Context, riderPartyID, docType, photoRef string, fields map[string]string) error {
	now := time.Now().UTC()
	set := bson.D{
		{Key: "status", Value: riderDocUploaded},
		{Key: "photo_ref", Value: photoRef},
		// A fresh submission clears the previous reviewer verdict — leaving a
		// stale "the photo was blurred" under a new, sharp photo reads as the
		// upload having failed.
		{Key: "rejection_reason", Value: ""},
		{Key: "reviewed_at", Value: nil},
		{Key: "uploaded_at", Value: now},
		{Key: "updated_at", Value: now},
	}
	if len(fields) > 0 {
		set = append(set, bson.E{Key: "fields", Value: fields})
	}
	filter := bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "type", Value: docType},
		{Key: "status", Value: bson.D{{Key: "$ne", Value: riderDocVerified}}},
	}
	update := bson.D{
		{Key: "$set", Value: set},
		{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
	}
	if _, err := r.riderColl(collRiderDocuments).UpdateOne(ctx, filter, update, options.Update().SetUpsert(true)); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return httpx.Conflict("DOCUMENT_ALREADY_VERIFIED",
				"this document is already verified — contact your centre to change it")
		}
		return httpx.Internal(fmt.Errorf("store rider document: %w", err))
	}
	return nil
}

// riderConcernTitle derives the one-line headline the concern list shows from
// the rider's own first line. Derived, not invented — it is their words.
func riderConcernTitle(desc string) string {
	line := desc
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) > 70 {
		return strings.TrimSpace(string(runes[:70])) + "…"
	}
	return line
}

// riderListConcerns returns the rider's own tickets, newest first.
func (r *repository) riderListConcerns(ctx context.Context, riderPartyID string) ([]riderSupportConcernView, error) {
	cur, err := r.riderColl(collRiderSupport).Find(ctx,
		bson.D{{Key: "rider_party_id", Value: riderPartyID}},
		options.Find().SetSort(bson.D{{Key: "raised_at", Value: -1}}).SetLimit(100))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider concerns: %w", err))
	}
	var rows []riderSupportRecord
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider concerns: %w", err))
	}
	out := make([]riderSupportConcernView, 0, len(rows))
	for _, row := range rows {
		label := row.TypeLabel
		if label == "" {
			// Pre-label records (or an operator-created ticket): fall back to
			// the catalog, then to the raw code — never to a guess.
			if spec, ok := riderSupportTypeFor(row.TypeCode); ok {
				label = spec.Label
			} else {
				label = row.TypeCode
			}
		}
		at := row.RaisedAt
		out = append(out, riderSupportConcernView{
			ID:          row.ConcernID,
			Type:        label,
			Title:       row.Title,
			Status:      riderConcernStatus(row.Status),
			RaisedAt:    rfc3339Ptr(&at),
			LastMessage: row.LastMessage,
			ClosingNote: row.ClosingNote,
		})
	}
	return out, nil
}

// riderConcernStatus normalises onto the closed set the client renders.
func riderConcernStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case riderConcernInProgress:
		return riderConcernInProgress
	case riderConcernResolved:
		return riderConcernResolved
	case riderConcernClosed:
		return riderConcernClosed
	default:
		return riderConcernOpen
	}
}

func (r *repository) riderInsertSupportConcern(ctx context.Context, rec riderSupportRecord) error {
	if _, err := r.riderColl(collRiderSupport).InsertOne(ctx, rec); err != nil {
		return httpx.Internal(fmt.Errorf("insert rider concern: %w", err))
	}
	return nil
}

// riderOwnsDelivery reports whether a delivery is currently this rider's. The
// rider id comes from the token, so this is the ownership gate for anything a
// rider attaches to one of their tasks.
func (r *repository) riderOwnsDelivery(ctx context.Context, riderPartyID, deliveryID string) (bool, error) {
	n, err := r.deliveries.CountDocuments(ctx, bson.D{
		{Key: "delivery_id", Value: deliveryID},
		{Key: "rider_party_id", Value: riderPartyID},
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("check delivery ownership: %w", err))
	}
	return n > 0, nil
}

// riderListReferrals returns the people this rider actually brought in, plus
// the total that has genuinely been earned (EARNED rows only — a PENDING or
// SUCCESSFUL referral has not paid, and showing it as earned would be a
// promise the payout run does not keep).
func (r *repository) riderListReferrals(ctx context.Context, riderPartyID string) ([]riderReferralEntryView, float64, error) {
	cur, err := r.riderColl(collRiderReferrals).Find(ctx,
		bson.D{{Key: "referrer_party_id", Value: riderPartyID}},
		options.Find().SetSort(bson.D{{Key: "joined_at", Value: -1}}).SetLimit(200))
	if err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("find rider referrals: %w", err))
	}
	var rows []riderReferralRecord
	if err := cur.All(ctx, &rows); err != nil {
		return nil, 0, httpx.Internal(fmt.Errorf("decode rider referrals: %w", err))
	}
	out := make([]riderReferralEntryView, 0, len(rows))
	earned := 0.0
	for _, row := range rows {
		status := riderReferralStatus(row.Status)
		if status == riderReferralEarned {
			earned += row.Amount
		}
		out = append(out, riderReferralEntryView{
			Name:     strings.TrimSpace(row.Name),
			Status:   status,
			JoinedAt: rfc3339Ptr(row.JoinedAt),
			Amount:   round2(row.Amount),
		})
	}
	return out, earned, nil
}

func riderReferralStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case riderReferralEarned:
		return riderReferralEarned
	case riderReferralSuccessful:
		return riderReferralSuccessful
	default:
		return riderReferralPending
	}
}

// riderContactList is the emergency screen's whole payload: the rider's own
// editable numbers, then the read-only PYAAS-side numbers for their centre.
func (r *repository) riderContactList(ctx context.Context, ident riderIdentityRecord) ([]riderEmergencyContactView, error) {
	own, err := r.riderLoadOwnContacts(ctx, ident.partyID.Hex())
	if err != nil {
		return nil, err
	}
	out := make([]riderEmergencyContactView, 0, len(own)+1)
	for _, c := range own {
		out = append(out, riderEmergencyContactView{Label: c.Label, Phone: c.Phone, Relation: riderRelationSelf})
	}
	centre, err := r.riderCentreContacts(ctx, ident)
	if err != nil {
		return nil, err
	}
	return append(out, centre...), nil
}

func (r *repository) riderLoadOwnContacts(ctx context.Context, riderPartyID string) ([]riderEmergencyContactDoc, error) {
	var rec riderEmergencyRecord
	err := r.riderColl(collRiderEmergency).
		FindOne(ctx, bson.D{{Key: "rider_party_id", Value: riderPartyID}}).Decode(&rec)
	if err != nil {
		if isNoDocs(err) {
			// Nothing saved yet — the screen shows two empty fields, which is
			// the truth.
			return []riderEmergencyContactDoc{}, nil
		}
		return nil, httpx.Internal(fmt.Errorf("load emergency contacts: %w", err))
	}
	if rec.Contacts == nil {
		return []riderEmergencyContactDoc{}, nil
	}
	return rec.Contacts, nil
}

// riderSaveOwnContacts replaces the rider's own contacts wholesale. Only the
// `contacts` array is ever written here, and the centre numbers are not part of
// this document at all, so a rider cannot reach them.
func (r *repository) riderSaveOwnContacts(ctx context.Context, riderPartyID string, contacts []riderEmergencyContactDoc) error {
	if contacts == nil {
		contacts = []riderEmergencyContactDoc{}
	}
	_, err := r.riderColl(collRiderEmergency).UpdateOne(ctx,
		bson.D{{Key: "rider_party_id", Value: riderPartyID}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "contacts", Value: contacts},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}},
		options.Update().SetUpsert(true))
	if err != nil {
		return httpx.Internal(fmt.Errorf("save emergency contacts: %w", err))
	}
	return nil
}

// riderCentreContacts derives the PYAAS-side numbers for the rider's centre.
//
// Preference order, both of them REAL records: the centre's own contact number
// if the org unit carries one, otherwise the phone of the centre's active store
// manager — the person a rider in trouble is actually meant to call. If neither
// exists we return nothing at all rather than a helpline that would not answer.
func (r *repository) riderCentreContacts(ctx context.Context, ident riderIdentityRecord) ([]riderEmergencyContactView, error) {
	out := []riderEmergencyContactView{}
	if ident.centreID.IsZero() {
		return out, nil
	}
	centre := strings.TrimSpace(ident.centreName)
	if centre == "" {
		centre = "Your centre"
	}
	var ou struct {
		ContactPhone string `bson:"contact_phone"`
		Phone        string `bson:"phone"`
	}
	if err := r.orgUnits.FindOne(ctx, bson.D{{Key: "_id", Value: ident.centreID}}).Decode(&ou); err != nil && !isNoDocs(err) {
		return nil, httpx.Internal(fmt.Errorf("load centre contact: %w", err))
	}
	if phone := strings.TrimSpace(riderFirstNonEmpty(ou.ContactPhone, ou.Phone)); phone != "" {
		out = append(out, riderEmergencyContactView{Label: centre, Phone: phone, Relation: riderRelationCentre})
		return out, nil
	}

	var ra struct {
		PartyID primitive.ObjectID `bson:"party_id"`
	}
	err := r.roleAssignments.FindOne(ctx, bson.D{
		{Key: "org_unit_id", Value: ident.centreID},
		{Key: "role_code", Value: domain.RoleStoreManager},
		{Key: "status", Value: domain.RoleAssignmentActive},
	}).Decode(&ra)
	if err != nil {
		if isNoDocs(err) {
			return out, nil // no manager on record — say nothing rather than invent one
		}
		return nil, httpx.Internal(fmt.Errorf("find centre manager: %w", err))
	}
	var p struct {
		Phone string `bson:"phone"`
	}
	if err := r.parties.FindOne(ctx, bson.D{{Key: "_id", Value: ra.PartyID}}).Decode(&p); err != nil {
		if isNoDocs(err) {
			return out, nil
		}
		return nil, httpx.Internal(fmt.Errorf("load centre manager: %w", err))
	}
	if strings.TrimSpace(p.Phone) == "" {
		return out, nil
	}
	return append(out, riderEmergencyContactView{
		Label:    "Hub manager · " + centre,
		Phone:    p.Phone,
		Relation: riderRelationCentre,
	}), nil
}

func riderFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// riderNormaliseMobile validates an Indian mobile number and stores it in one
// canonical form (+91XXXXXXXXXX), so the app's dialler always gets a number it
// can call. Accepts what riders actually type — 10 digits, a 0 or +91 prefix,
// spaces and dashes — and rejects anything that is not a real mobile series.
// A wrong emergency number is worse than a missing one, so this rejects rather
// than trims something plausible out of the input.
func riderNormaliseMobile(raw string) (string, error) {
	var digits strings.Builder
	for _, ru := range raw {
		if ru >= '0' && ru <= '9' {
			digits.WriteRune(ru)
		}
	}
	d := digits.String()
	switch {
	case len(d) == 12 && strings.HasPrefix(d, "91"):
		d = d[2:]
	case len(d) == 11 && strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	// Indian mobile series start 6–9; a landline or a truncated number would
	// never connect in an emergency.
	if len(d) != 10 || d[0] < '6' || d[0] > '9' {
		return "", httpx.BadRequest("INVALID_PHONE", "enter a 10-digit mobile number")
	}
	return "+91" + d, nil
}
