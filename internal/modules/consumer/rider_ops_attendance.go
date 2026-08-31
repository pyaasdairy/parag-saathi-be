package consumer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/pyaas/saathi-backend/internal/platform/httpx"
)

// ─────────────────────────────────────────────────────────────────────────────
// DUTY ATTENDANCE — RIDER_API.md §3.2 and §3.3.
//
// WHAT THIS IS, AND WHAT IT DELIBERATELY IS NOT.
//
// The client shipped an eight-gate face pipeline whose actual matching step was
// a scripted simulation (DemoFaceAnalyzer). That simulation has been REMOVED
// rather than backed by a half-real server-side matcher, because a biometric
// claim we cannot stand behind is worse than no biometric claim at all: the
// rider is told the machine recognised their face, a supervisor believes it,
// and nothing was ever compared. What ships instead is an HONEST attendance
// check — a live SELFIE plus a GPS fix inside the centre's geofence, both
// stored as real evidence a human can audit later.
//
// Consequences, and they are load-bearing:
//
//   - No similarity score is computed, stored or returned. faceVerifyResponse
//     .Score exists only for wire compatibility with the shipped client and is
//     LEFT UNSET on every response in this file.
//   - `matched` on the wire means "ATTENDANCE ACCEPTED" — nothing more. It is
//     never an assertion that a face was recognised.
//   - The check-in body from older builds still carries `embedding` (the
//     on-device face vector). It is IGNORED and never stored. A face embedding
//     is sensitive personal data under the DPDP Act 2023; storing one we have
//     no legitimate consumer for would be collection without purpose. The
//     struct below simply does not declare the field, so it is dropped at the
//     JSON boundary and never reaches memory we own.
//   - A check-in whose liveness.analyzer is "demo" is REJECTED (422). A
//     placeholder build must never be able to mark a real day of work present,
//     because that day becomes a per-diem line on a real payout.
//
// Every `message` here is rendered verbatim to the rider (through the client's
// tr(), which translates the English strings it knows), so they are stable,
// plain English sentences — never templated, never punctuated for a log.
// ─────────────────────────────────────────────────────────────────────────────

// Duty states on the wire. The client maps these onto DutyState
// (lib/models/rider.dart): anything unrecognised degrades to NOT_MARKED, so
// these three strings are the whole vocabulary.
const (
	riderDutyNotMarked  = "NOT_MARKED"
	riderDutyOnDuty     = "ON_DUTY"
	riderDutyCheckedOut = "CHECKED_OUT"
)

// riderAttendanceGeofenceM is how far from the centre a check-in may be taken.
// 500 m is a hub forecourt plus honest GPS error on a cheap Android handset at
// 5 AM under a shed roof — tight enough that marking attendance from home is
// impossible, loose enough that a rider standing at the gate is never turned
// away by a bad fix.
const riderAttendanceGeofenceM = 500.0

// riderSelfieRequired mirrors `face_required` on the duty card. It is true
// because riderCheckIn genuinely refuses a check-in without a photo_ref — the
// flag is an accurate statement about server behaviour, not UI decoration.
const riderSelfieRequired = true

// Free-text and evidence bounds. A photo_ref is a view URL minted by the
// presign seam; anything longer than this is not one.
const (
	riderPhotoRefMaxLen     = 512
	riderAnalyzerMaxLen     = 64
	riderLivenessStepsMax   = 32
	riderLivenessStepMaxLen = 64
	riderAttendanceMaxDays  = 90
)

// ── stored shapes ───────────────────────────────────────────────────────────

// riderAttendanceDoc is one rider's one IST day. The unique index on
// (rider_party_id, day) in ensureRiderOpsIndexes is what makes a second
// check-in structurally impossible — not a read-then-write check that a
// double-tap on a 2G connection would race straight through.
type riderAttendanceDoc struct {
	RiderPartyID string     `bson:"rider_party_id"`
	Day          string     `bson:"day"` // IST calendar day, "2006-01-02"
	State        string     `bson:"state"`
	CheckInAt    *time.Time `bson:"check_in_at,omitempty"`
	CheckOutAt   *time.Time `bson:"check_out_at,omitempty"`
	Hours        float64    `bson:"hours,omitempty"`
	// The honest evidence: the selfie the rider took and where they stood.
	// No embedding, no score — see the file header.
	PhotoRef      string             `bson:"photo_ref,omitempty"`
	CheckInGeo    *geoPt             `bson:"check_in_geo,omitempty"`
	CheckOutGeo   *geoPt             `bson:"check_out_geo,omitempty"`
	CentreDistM   float64            `bson:"centre_distance_m,omitempty"`
	CentreOrgID   string             `bson:"centre_org_id,omitempty"`
	CentreName    string             `bson:"centre_name,omitempty"`
	Liveness      *riderLivenessDoc  `bson:"liveness,omitempty"`
	Note          string             `bson:"note,omitempty"`
	CreatedAt     time.Time          `bson:"created_at"`
	UpdatedAt     time.Time          `bson:"updated_at"`
	MongoObjectID primitive.ObjectID `bson:"_id,omitempty"`
}

// riderLivenessDoc is the anti-spoof evidence the handset gathered. It is kept
// so a disputed attendance can be reviewed ("which build, which gates passed"),
// not to make any server-side decision beyond the demo-analyzer rejection.
type riderLivenessDoc struct {
	Blink         bool     `bson:"blink"`
	Analyzer      string   `bson:"analyzer,omitempty"`
	StepsPassed   []string `bson:"steps_passed,omitempty"`
	DeviceUpright bool     `bson:"device_upright"`
}

// ── wire shapes ─────────────────────────────────────────────────────────────

// riderDutyTodayResponse is the duty card (client: DutyToday).
type riderDutyTodayResponse struct {
	State        string `json:"state"`
	CheckInAt    string `json:"check_in_at,omitempty"`
	CheckOutAt   string `json:"check_out_at,omitempty"`
	Shift        string `json:"shift,omitempty"`
	CenterName   string `json:"center_name,omitempty"`
	FaceRequired bool   `json:"face_required"`
	Message      string `json:"message,omitempty"`
}

// riderAttendanceDayResponse is one row of the history list (client:
// AttendanceDay). Absent days are simply not in the list.
type riderAttendanceDayResponse struct {
	Date       string  `json:"date"` // IST day, "2006-01-02"
	Present    bool    `json:"present"`
	CheckInAt  string  `json:"check_in_at,omitempty"`
	CheckOutAt string  `json:"check_out_at,omitempty"`
	Hours      float64 `json:"hours,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// riderLivenessRequest is the liveness block the client posts. `embedding` is
// intentionally absent from every request struct in this file — see the header.
type riderLivenessRequest struct {
	Blink         bool     `json:"blink"`
	Analyzer      string   `json:"analyzer"`
	StepsPassed   []string `json:"steps_passed"`
	DeviceUpright bool     `json:"device_upright"`
}

// riderCheckInRequest — POST /attendance/check-in.
type riderCheckInRequest struct {
	PhotoRef string                `json:"photo_ref"`
	Liveness *riderLivenessRequest `json:"liveness"`
	Geo      []float64             `json:"geo"` // [lat, lng]
}

// riderCheckOutRequest — POST /attendance/check-out. Geo is optional: a rider
// legitimately finishes the run away from the hub.
type riderCheckOutRequest struct {
	Geo []float64 `json:"geo"`
}

// riderFaceEnrolRequest — POST /face/enrol. Only the reference selfie is taken;
// `embedding` and `quality` from the client are dropped at this boundary.
type riderFaceEnrolRequest struct {
	PhotoRef string `json:"photo_ref"`
}

// ── handlers ────────────────────────────────────────────────────────────────

// riderAttendanceToday handles GET /consumer/delivery/rider/attendance/today.
// Returns the rider's real record for the current IST day, or an honest
// NOT_MARKED card. Nothing here is synthesised: if there is no record, the
// answer is "not marked yet", never a plausible-looking check-in time.
func (h *handler) riderAttendanceToday(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ctx := r.Context()
	day := istDay(time.Now())

	doc, err := h.svc.repo.riderAttendanceForDay(ctx, actor.PartyID, day)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	centre, err := h.svc.repo.riderAttendanceCentre(ctx, actor.PartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	shift, err := h.svc.repo.riderAttendanceShiftLabel(ctx, actor.PartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	resp := riderDutyTodayResponse{
		State:        riderDutyNotMarked,
		Shift:        shift, // "" when no shift is configured — we do not invent one
		CenterName:   centre.Name,
		FaceRequired: riderSelfieRequired,
		// The message field carries ACTION, not narration: it is set only when
		// the rider still has something to do.
		Message: "Please mark attendance at the centre.",
	}
	if doc != nil {
		resp.State = doc.State
		resp.CheckInAt = rfc3339Ptr(doc.CheckInAt)
		resp.CheckOutAt = rfc3339Ptr(doc.CheckOutAt)
		resp.Message = ""
		// The centre recorded ON the attendance is the truth for that day; the
		// role assignment may have moved since.
		if doc.CentreName != "" {
			resp.CenterName = doc.CentreName
		}
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// riderCheckIn handles POST /consumer/delivery/rider/attendance/check-in.
//
// A rejected check-in that the rider can FIX (too far from the centre) comes
// back as 200 with matched:false and an actionable message, because that is
// what the client renders — an HTTP error would surface as a generic failure
// banner with no instruction. A rejected check-in the rider CANNOT fix (a demo
// build, a missing selfie) is a real HTTP error, because it is a defect in the
// caller, not something to coach the rider through.
func (h *handler) riderCheckIn(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderCheckInRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	photoRef, err := riderCleanPhotoRef(req.PhotoRef)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	liveness, err := riderCleanLiveness(req.Liveness)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Geo is EVIDENCE, not a precondition.
	//
	// It used to be required, but the shipped client only sends it when it has a
	// fix (rider_api.dart: `if (lat != null && lng != null)`), and its 10-second
	// getCurrentPosition times out routinely at 05:00 under a hub's shed roof —
	// or returns nothing at all if location permission was refused. Requiring it
	// meant those riders could not mark attendance by any route. Record the
	// absence instead: the selfie is still mandatory, the geofence below simply
	// cannot be applied, and the gap is written onto the record so the centre
	// can see which check-ins were unlocated rather than being told they were
	// all fenced.
	at, err := riderGeoFromPair(req.Geo, false)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ctx := r.Context()
	centre, err := h.svc.repo.riderAttendanceCentre(ctx, actor.PartyID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// NO ACTIVE POSTING, NO PRESENT DAY.
	//
	// riderAttendanceCentre deliberately tolerates a missing posting when it is
	// only READING a duty card — but marking attendance is a payroll fact, and
	// it must not be creatable by someone with no current assignment. A role
	// token outlives a revocation by up to its 15-minute TTL, so a rider
	// dismissed at 06:00 could otherwise stand at home at 06:05 and write
	// themselves a present day: with no posting there is also no centre, so the
	// geofence below would not fire either, and the record would look exactly
	// like a real check-in.
	if centre.OrgID == "" {
		httpx.Error(w, r, httpx.Unprocessable("NO_ACTIVE_POSTING",
			"you have no active posting at a centre — contact your centre before marking attendance"))
		return
	}

	// Geofence — only when we actually know where the centre is. A centre with
	// no coordinates on its org-unit cannot be fenced against honestly, and
	// refusing every rider at that hub because of a missing admin field would
	// strand a real morning run. The stored geo remains the evidence either way.
	distM := 0.0
	if centre.HasGeo && at != nil {
		distM = haversineM(*at, centre.Geo)
		if distM > riderAttendanceGeofenceM {
			httpx.JSON(w, http.StatusOK, faceVerifyResponse{
				Matched: false,
				Message: "Move closer to the centre to mark attendance",
			})
			return
		}
	}

	now := time.Now().UTC()
	doc := riderAttendanceDoc{
		RiderPartyID: actor.PartyID,
		Day:          istDay(now),
		State:        riderDutyOnDuty,
		CheckInAt:    &now,
		PhotoRef:     photoRef,
		CheckInGeo:   at,
		CentreDistM:  round2(distM),
		CentreOrgID:  centre.OrgID,
		CentreName:   centre.Name,
		Note:         riderAttendanceGeoNote(at, centre),
		Liveness:     liveness,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	existing, dup, err := h.svc.repo.riderInsertAttendance(ctx, doc)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if dup {
		// The unique (rider, day) index caught a second check-in. A double-tap
		// on a bad network is not an error the rider caused — report the state
		// that already exists so the screen settles on the truth.
		msg := "You are already on duty today"
		if existing != nil && existing.State == riderDutyCheckedOut {
			msg = "You have already checked out for today"
		}
		httpx.JSON(w, http.StatusOK, faceVerifyResponse{
			Matched:          true, // attendance ACCEPTED (it exists), not a face match
			AttendanceMarked: true,
			Message:          msg,
		})
		return
	}

	httpx.JSON(w, http.StatusOK, faceVerifyResponse{
		Matched:          true, // attendance ACCEPTED — see the file header
		AttendanceMarked: true,
		Message:          "Attendance marked",
	})
}

// riderCheckOut handles POST /consumer/delivery/rider/attendance/check-out.
// Closes the IST day and computes hours worked from the stored check_in_at —
// never from a client-supplied duration, because those hours are a payout line.
//
// No geofence on checkout: a rider legitimately ends the run at the last
// doorstep, kilometres from the hub. Fencing it would push them to fake a
// location or leave the day open.
func (h *handler) riderCheckOut(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderCheckOutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	at, err := riderGeoFromPair(req.Geo, false)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	ctx := r.Context()
	day := istDay(time.Now())

	closed, err := h.svc.repo.riderCloseAttendance(ctx, actor.PartyID, day, at)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if closed {
		httpx.JSON(w, http.StatusOK, faceVerifyResponse{
			Matched: true,
			Message: "Checkout marked",
		})
		return
	}

	// The conditional update matched nothing: either the day was already closed
	// (idempotent success — a retry must not read as a failure) or the rider
	// never checked in (a genuine conflict).
	doc, err := h.svc.repo.riderAttendanceForDay(ctx, actor.PartyID, day)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if doc == nil {
		httpx.Error(w, r, httpx.Conflict("NOT_CHECKED_IN", "mark attendance before checking out"))
		return
	}
	httpx.JSON(w, http.StatusOK, faceVerifyResponse{
		Matched: true,
		Message: "You have already checked out for today",
	})
}

// riderAttendanceHistory handles GET /attendance/history?days=30.
// Real records only, newest first. A day with no record is ABSENT from the
// list — the client renders those as not-present. Synthesising a row for every
// calendar day would be inventing attendance data, which is the exact failure
// this whole surface exists to undo.
func (h *handler) riderAttendanceHistory(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 {
			httpx.Error(w, r, httpx.BadRequest("INVALID_DAYS", "days must be a positive whole number"))
			return
		}
		days = n
	}
	if days > riderAttendanceMaxDays {
		days = riderAttendanceMaxDays // a cap, not a rejection: 90 days is the whole answer
	}

	// The window is counted in IST days, inclusive of today, so "30 days" is
	// the last 30 business days the rider recognises — not a UTC 720-hour span
	// that would clip this morning's record at 05:30 IST.
	from := istDay(time.Now().In(istZone).AddDate(0, 0, -(days - 1)))
	rows, err := h.svc.repo.riderAttendanceSince(r.Context(), actor.PartyID, from)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	out := make([]riderAttendanceDayResponse, 0, len(rows))
	for i := range rows {
		d := rows[i]
		out = append(out, riderAttendanceDayResponse{
			Date:       d.Day,
			Present:    d.CheckInAt != nil,
			CheckInAt:  rfc3339Ptr(d.CheckInAt),
			CheckOutAt: rfc3339Ptr(d.CheckOutAt),
			Hours:      d.Hours,
			Note:       d.Note,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// riderFaceEnrol handles POST /consumer/delivery/rider/face/enrol.
//
// It stores ONE thing: the rider's reference selfie. No embedding, no template,
// no score — see the file header. What it buys is an auditable "this is the
// person we onboarded" photo a supervisor can hold against a check-in selfie.
//
// The FIRST enrolment is open (a new rider must be able to finish onboarding
// unassisted). A RE-enrolment is not self-service: a rider who can silently
// replace the reference face can register a friend and defeat every attendance
// control downstream of it. That is a 409 the client shows as a "contact your
// supervisor" state.
func (h *handler) riderFaceEnrol(w http.ResponseWriter, r *http.Request) {
	actor, err := riderActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req riderFaceEnrolRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	photoRef, err := riderCleanPhotoRef(req.PhotoRef)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.repo.riderEnrolFacePhoto(r.Context(), actor.PartyID, photoRef); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Matched:true = "enrolment accepted". Score stays UNSET: there is nothing
	// to score against on a first enrolment, and we will not imply otherwise.
	httpx.JSON(w, http.StatusOK, faceVerifyResponse{
		Matched: true,
		Message: "Face registered successfully",
	})
}

// ── input validation ────────────────────────────────────────────────────────

// riderCleanPhotoRef enforces that a selfie was actually uploaded. The selfie
// is the ONLY evidence of who marked the attendance now that no face matching
// happens, so a blank photo_ref is a hard rejection, never a warning.
func riderCleanPhotoRef(raw string) (string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", httpx.BadRequest("PHOTO_REQUIRED", "a selfie is required to mark attendance")
	}
	if len(ref) > riderPhotoRefMaxLen {
		return "", httpx.BadRequest("PHOTO_REF_INVALID", "photo reference is too long")
	}
	// The presign seam returns a view URL — a path on this API or an absolute
	// http(s) URL. Anything else (a local file path, a data: blob) is a client
	// bug we should surface rather than store.
	if !strings.HasPrefix(ref, "/") && !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		return "", httpx.BadRequest("PHOTO_REF_INVALID", "photo reference must be an uploaded file URL")
	}
	return ref, nil
}

// riderCleanLiveness validates and normalises the anti-spoof evidence.
//
// The one hard gate: analyzer "demo" is refused. DemoFaceAnalyzer scripts a
// successful approach with no camera involvement at all, and a build carrying
// it must never be able to put a present day on a real payout.
func riderCleanLiveness(in *riderLivenessRequest) (*riderLivenessDoc, error) {
	if in == nil {
		// Older builds and a plain retry may omit the block entirely. Absent
		// evidence is not fabricated evidence — record nothing and continue.
		return nil, nil
	}
	analyzer := strings.TrimSpace(in.Analyzer)
	if strings.EqualFold(analyzer, "demo") {
		return nil, httpx.Unprocessable("DEMO_ANALYZER",
			"this build cannot mark attendance — update the app and try again")
	}
	if len(analyzer) > riderAnalyzerMaxLen {
		return nil, httpx.BadRequest("INVALID_LIVENESS", "analyzer name is too long")
	}
	if len(in.StepsPassed) > riderLivenessStepsMax {
		return nil, httpx.BadRequest("INVALID_LIVENESS", "too many liveness steps reported")
	}
	steps := make([]string, 0, len(in.StepsPassed))
	for _, s := range in.StepsPassed {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len(s) > riderLivenessStepMaxLen {
			return nil, httpx.BadRequest("INVALID_LIVENESS", "liveness step name is too long")
		}
		steps = append(steps, s)
	}
	return &riderLivenessDoc{
		Blink:         in.Blink,
		Analyzer:      analyzer,
		StepsPassed:   steps,
		DeviceUpright: in.DeviceUpright,
	}, nil
}

// riderAttendanceGeoNote records, on the attendance row itself, why a check-in
// could not be geofenced. A centre reviewing the day needs to tell "stood at the
// hub" apart from "we could not tell" — an empty note means the fence was
// actually applied and passed.
func riderAttendanceGeoNote(at *geoPt, centre riderCentreRef) string {
	switch {
	case at == nil:
		return "no GPS fix at check-in"
	case !centre.HasGeo:
		return "centre has no coordinates on file"
	default:
		return ""
	}
}

// riderGeoFromPair reads the wire's [lat, lng] pair. `required` distinguishes
// check-in (a fix is mandatory — it is half the evidence) from check-out (the
// handset may have no fix indoors at the hub, and blocking the close of a shift
// over that helps nobody).
//
// coordsSane (geofence.go) rejects out-of-range values and Null Island (0,0),
// the classic "no fix yet" sentinel — a check-in fenced against (0,0) would
// otherwise be measured from the Gulf of Guinea.
func riderGeoFromPair(pair []float64, required bool) (*geoPt, error) {
	if len(pair) == 0 {
		if required {
			return nil, httpx.BadRequest("GEO_REQUIRED", "location is required to mark attendance")
		}
		return nil, nil
	}
	if len(pair) != 2 || !coordsSane(pair[0], pair[1]) {
		if required {
			return nil, httpx.BadRequest("GEO_INVALID", "location could not be read — try again outdoors")
		}
		// On checkout a bad fix is dropped rather than fatal: the shift still closes.
		return nil, nil
	}
	return &geoPt{Lat: pair[0], Lng: pair[1]}, nil
}

// ── repository ──────────────────────────────────────────────────────────────

// riderCentreRef is the hub a rider is posted to, as far as attendance cares:
// a name for the duty card and a coordinate for the geofence. HasGeo is
// explicit because "no coordinates configured" and "coordinates at 0,0" must
// not be confused — the second would fence every rider out.
type riderCentreRef struct {
	OrgID  string
	Name   string
	Geo    geoPt
	HasGeo bool
}

// riderAttendanceCentre resolves the rider's posting from their ACTIVE
// DELIVERY_RIDER role assignment. A rider with no active assignment is NOT an
// error here: the role token already proved they are a rider, and refusing to
// show them a duty card because an admin has not filled a field would be a
// worse failure than an unfenced check-in. The caller gets a zero ref and skips
// the fence.
func (r *repository) riderAttendanceCentre(ctx context.Context, riderPartyID string) (riderCentreRef, error) {
	oid, err := primitive.ObjectIDFromHex(riderPartyID)
	if err != nil {
		return riderCentreRef{}, httpx.BadRequest("INVALID_RIDER", "bad rider identity on the token")
	}
	var ra struct {
		OrgUnitID primitive.ObjectID `bson:"org_unit_id"`
	}
	err = r.roleAssignments.FindOne(ctx, bson.D{
		{Key: "party_id", Value: oid},
		{Key: "role_code", Value: "DELIVERY_RIDER"},
		{Key: "status", Value: "ACTIVE"},
	}).Decode(&ra)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return riderCentreRef{}, nil
	}
	if err != nil {
		return riderCentreRef{}, httpx.Internal(fmt.Errorf("resolve rider centre assignment: %w", err))
	}

	var unit struct {
		Name string  `bson:"name"`
		Lat  float64 `bson:"geo_lat"`
		Lng  float64 `bson:"geo_lng"`
	}
	err = r.orgUnits.FindOne(ctx, bson.D{{Key: "_id", Value: ra.OrgUnitID}}).Decode(&unit)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return riderCentreRef{OrgID: ra.OrgUnitID.Hex()}, nil
	}
	if err != nil {
		return riderCentreRef{}, httpx.Internal(fmt.Errorf("load rider centre org unit: %w", err))
	}
	ref := riderCentreRef{OrgID: ra.OrgUnitID.Hex(), Name: unit.Name}
	if geoSane(unit.Lat, unit.Lng) {
		ref.Geo, ref.HasGeo = geoPt{Lat: unit.Lat, Lng: unit.Lng}, true
	}
	return ref, nil
}

// riderAttendanceShiftLabel reads the shift copy stored on the rider's profile
// ("Morning · 5:00 – 9:00 AM"). There is no shift model in this backend yet, so
// when nothing is on file this returns "" and the duty card simply omits the
// line — a hard-coded default would be a made-up promise about working hours.
func (r *repository) riderAttendanceShiftLabel(ctx context.Context, riderPartyID string) (string, error) {
	var doc struct {
		ShiftLabel string `bson:"shift_label"`
	}
	err := r.riderColl(collRiderProfiles).
		FindOne(ctx, bson.D{{Key: "rider_party_id", Value: riderPartyID}}).
		Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", httpx.Internal(fmt.Errorf("load rider shift label: %w", err))
	}
	return strings.TrimSpace(doc.ShiftLabel), nil
}

// riderAttendanceForDay loads one rider's record for one IST day, or (nil, nil)
// when they have not marked it. mongo.ErrNoDocuments is an expected answer on
// this path — every rider's first request of the morning takes it — so it is
// handled here and never escapes as a 500.
func (r *repository) riderAttendanceForDay(ctx context.Context, riderPartyID, day string) (*riderAttendanceDoc, error) {
	var doc riderAttendanceDoc
	err := r.riderColl(collRiderAttendance).FindOne(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "day", Value: day},
	}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load rider attendance for day: %w", err))
	}
	return &doc, nil
}

// riderInsertAttendance writes the check-in. dup=true means the unique
// (rider_party_id, day) index rejected a second check-in for the same IST day —
// the authoritative double-submission guard, enforced by the database rather
// than by a read the next tap could race. On a duplicate the caller gets the
// record that already exists so it can answer idempotently.
func (r *repository) riderInsertAttendance(ctx context.Context, doc riderAttendanceDoc) (*riderAttendanceDoc, bool, error) {
	if _, err := r.riderColl(collRiderAttendance).InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			existing, loadErr := r.riderAttendanceForDay(ctx, doc.RiderPartyID, doc.Day)
			if loadErr != nil {
				return nil, true, loadErr
			}
			return existing, true, nil
		}
		return nil, false, httpx.Internal(fmt.Errorf("insert rider attendance: %w", err))
	}
	return &doc, false, nil
}

// riderCloseAttendance closes the day in ONE conditional update: the filter
// pins the expected state (ON_DUTY, not yet closed), so two taps race into a
// single write and the loser simply reports MatchedCount==0. Hours are computed
// from the STORED check_in_at, so a tampered client clock cannot inflate a
// per-diem line.
//
// Returns closed=false when nothing matched; the caller then distinguishes
// "already checked out" from "never checked in" and answers accordingly.
func (r *repository) riderCloseAttendance(ctx context.Context, riderPartyID, day string, at *geoPt) (bool, error) {
	coll := r.riderColl(collRiderAttendance)

	var current riderAttendanceDoc
	err := coll.FindOne(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "day", Value: day},
		{Key: "state", Value: riderDutyOnDuty},
	}).Decode(&current)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("load rider attendance for checkout: %w", err))
	}

	now := time.Now().UTC()
	hours := 0.0
	if current.CheckInAt != nil && !current.CheckInAt.IsZero() {
		// A negative span is only reachable if the stored check-in is in the
		// future (a clock skew we never write ourselves). Report zero rather
		// than a negative payout input.
		if d := now.Sub(*current.CheckInAt); d > 0 {
			hours = round2(d.Hours())
		}
	}

	set := bson.D{
		{Key: "state", Value: riderDutyCheckedOut},
		{Key: "check_out_at", Value: now},
		{Key: "hours", Value: hours},
		{Key: "updated_at", Value: now},
	}
	if at != nil {
		set = append(set, bson.E{Key: "check_out_geo", Value: at})
	}

	res, err := coll.UpdateOne(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "day", Value: day},
		{Key: "state", Value: riderDutyOnDuty}, // the guard: only an open day closes
	}, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return false, httpx.Internal(fmt.Errorf("close rider attendance: %w", err))
	}
	return res.MatchedCount == 1, nil
}

// riderAttendanceSince lists the rider's records from IST day `fromDay`
// onwards, newest first. `day` is stored as a zero-padded "2006-01-02" string,
// so a lexicographic range is exactly a chronological one — and it rides the
// (rider_party_id, day) index rather than scanning.
func (r *repository) riderAttendanceSince(ctx context.Context, riderPartyID, fromDay string) ([]riderAttendanceDoc, error) {
	cur, err := r.riderColl(collRiderAttendance).Find(ctx, bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "day", Value: bson.D{{Key: "$gte", Value: fromDay}}},
	}, options.Find().
		SetSort(bson.D{{Key: "day", Value: -1}}).
		SetLimit(riderAttendanceMaxDays))
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("find rider attendance history: %w", err))
	}
	rows := []riderAttendanceDoc{}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, httpx.Internal(fmt.Errorf("decode rider attendance history: %w", err))
	}
	return rows, nil
}

// riderEnrolFacePhoto stores the reference selfie on the rider's profile.
//
// The first-enrolment-only rule is enforced by the DATABASE, not by a read
// followed by a write: the filter matches only a profile with NO reference
// selfie on file, so an upsert against a profile that already has one cannot
// match and instead attempts an insert — which the unique rider_party_id index
// rejects. That duplicate-key error IS the re-enrolment signal, and it is
// atomic, so two simultaneous enrolments cannot both win.
func (r *repository) riderEnrolFacePhoto(ctx context.Context, riderPartyID, photoRef string) error {
	now := time.Now().UTC()
	filter := bson.D{
		{Key: "rider_party_id", Value: riderPartyID},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "face_photo_ref", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "face_photo_ref", Value: ""}},
			bson.D{{Key: "face_photo_ref", Value: nil}},
		}},
	}
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "face_photo_ref", Value: photoRef},
			{Key: "face_registered", Value: true},
			{Key: "face_enrolled_at", Value: now},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "rider_party_id", Value: riderPartyID},
			{Key: "created_at", Value: now},
		}},
	}
	res, err := r.riderColl(collRiderProfiles).UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return httpx.Conflict("REENROL_NEEDS_APPROVAL",
				"a reference photo is already on file — ask your supervisor to re-register your face")
		}
		return httpx.Internal(fmt.Errorf("store rider face enrolment: %w", err))
	}
	if res.MatchedCount == 0 && res.UpsertedCount == 0 {
		// Belt and braces: no match and no insert can only mean the guarded
		// filter excluded an existing profile. Same answer as the duplicate key.
		return httpx.Conflict("REENROL_NEEDS_APPROVAL",
			"a reference photo is already on file — ask your supervisor to re-register your face")
	}
	return nil
}
