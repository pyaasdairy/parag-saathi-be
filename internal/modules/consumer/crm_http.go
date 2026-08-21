// CRM — HTTP surface.
//
// CONSUMER (JWT): the in-app inbox the Phase A channel delivers into.
//
//	GET  /crm/inbox            — newest 50 messages (en + hi bodies)
//	POST /crm/inbox/{id}/read  — mark one read
//	GET  /crm/offer            — my offer state + entitled free deliveries
//	                             (the wallet screen's third line reads this)
//
// OPERATOR (Saathi auth, STORE_MANAGER or SUPER_ADMIN — the "handover
// profile"): the campaign console the founder asked for. This profile can
// enrol a household, push a manual message (through the SAME guard chain as
// every automated trigger), inspect and override offer state with a mandatory
// reason, read the dispatch log, and flip per-trigger kill switches.
//
//	POST /crm/enrol
//	POST /crm/message
//	GET  /crm/offers/{phone}
//	POST /crm/offers/{phone}/override
//	GET  /crm/dispatch-log/{phone}
//	POST /crm/flags            — {"trigger_id":"W-03b","enabled":false}
//
// Every route hard-gates on CRM_ENABLED so the deployed binary exposes
// nothing until the founder flips the env.
package consumer

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ── Consumer inbox ──────────────────────────────────────────────────────────

type inboxItemView struct {
	ID        string     `json:"id"`
	TriggerID string     `json:"trigger_id"`
	Category  string     `json:"category"`
	BodyEN    string     `json:"body_en"`
	BodyHI    string     `json:"body_hi"`
	CTA       string     `json:"cta,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

func (h *handler) crmInbox(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeJSON(w, http.StatusOK, []inboxItemView{})
		return
	}
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	cur, err := h.svc.repo.accounts.Database().Collection(collConsumerInbox).Find(r.Context(),
		bson.D{{Key: "consumer_id", Value: id}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(50))
	if err != nil {
		writeErr(w, errInternal("inbox read failed"))
		return
	}
	var rows []struct {
		ID        primitive.ObjectID `bson:"_id"`
		TriggerID string             `bson:"trigger_id"`
		Category  string             `bson:"category"`
		BodyEN    string             `bson:"body_en"`
		BodyHI    string             `bson:"body_hi"`
		CTA       string             `bson:"cta"`
		CreatedAt time.Time          `bson:"created_at"`
		ReadAt    *time.Time         `bson:"read_at"`
	}
	if err := cur.All(r.Context(), &rows); err != nil {
		writeErr(w, errInternal("inbox decode failed"))
		return
	}
	out := make([]inboxItemView, 0, len(rows))
	for _, m := range rows {
		out = append(out, inboxItemView{
			ID: m.ID.Hex(), TriggerID: m.TriggerID, Category: m.Category,
			BodyEN: m.BodyEN, BodyHI: m.BodyHI, CTA: m.CTA,
			CreatedAt: m.CreatedAt, ReadAt: m.ReadAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) crmInboxRead(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	oid, err := primitive.ObjectIDFromHex(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, errBadRequest("bad message id"))
		return
	}
	_, _ = h.svc.repo.accounts.Database().Collection(collConsumerInbox).UpdateOne(r.Context(),
		bson.D{{Key: "_id", Value: oid}, {Key: "consumer_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "read_at", Value: time.Now().UTC()}}}})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// crmMyOffer — the additive surface vc35's wallet third line and offer card
// read. Absent offer → zeros, so the response is always well-formed.
func (h *handler) crmMyOffer(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if !crmEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enrolled": false, "entitled_free_deliveries": 0})
		return
	}
	o, err := h.svc.repo.findOffer(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if o == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enrolled": false, "entitled_free_deliveries": 0})
		return
	}
	// WHITELISTED view — the raw doc carries abuse_flagged, operator party ids
	// in the transition log, and promoter attribution: fraud-relevant internals
	// a customer must never read off their own wire traffic. The operator
	// route (crmOfferByPhone) keeps the full document.
	writeJSON(w, http.StatusOK, map[string]any{
		"enrolled": true,
		"offer": map[string]any{
			"offer_id":        o.OfferID,
			"enrolled_at":     o.EnrolledAt,
			"pack1_state":     o.Pack1State,
			"pack2_state":     o.Pack2State,
			"subscription_id": o.SubscriptionID,
		},
		"entitled_free_deliveries": entitledFreeDeliveries(o),
	})
}

// ── Operator console (the handover profile) ────────────────────────────────

func (h *handler) crmEnrolHandler(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeErr(w, errForbidden("CRM is not enabled"))
		return
	}
	actor, _ := operatorActor(r)
	var in crmEnrolInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := h.svc.crmEnrol(r.Context(), actor.PartyID, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// crmComposeMessage lets the handover profile push a manual message to one
// customer. It flows through crmDispatch's guard chain via a synthetic
// trigger, so quiet hours / caps / dedup apply to humans exactly as to code.
func (h *handler) crmComposeMessage(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeErr(w, errForbidden("CRM is not enabled"))
		return
	}
	actor, _ := operatorActor(r)
	var in struct {
		Phone    string `json:"phone"`
		BodyEN   string `json:"body_en"`
		BodyHI   string `json:"body_hi"`
		Category string `json:"category"` // service_implicit (default) | promotional
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.BodyEN) == "" && strings.TrimSpace(in.BodyHI) == "" {
		writeErr(w, errBadRequest("a message body is required"))
		return
	}
	acct, err := h.svc.repo.findAccountByPhone(r.Context(), crmCanonicalPhone(normalizePhone(in.Phone)))
	if err != nil || acct == nil {
		writeErr(w, errNotFound("no customer with that phone"))
		return
	}
	// Category decides which guards apply, so it is a CLOSED enum: an operator
	// send is promotional (full consent/opt-out/quiet-hours/caps chain) unless
	// explicitly declared service_implicit. Unknown values fail CLOSED into
	// promotional — never into a guard-free lane.
	cat := "promotional"
	if in.Category == "service_implicit" || in.Category == "service" {
		cat = "service_implicit"
	}
	t := crmTrigger{
		ID: "MANUAL-" + time.Now().UTC().Format("20060102-150405"), Category: cat,
		Kind: "manual", Section: "M",
	}
	now := time.Now().UTC()
	row, won := h.svc.crmClaimDispatch(r.Context(), t, acct.ID, istDay(now))
	if !won {
		writeErr(w, errConflict("DUPLICATE", "an identical manual send was just made"))
		return
	}
	if guard, pass := h.svc.crmGuardCheck(r.Context(), t, acct.ID, now); !pass {
		h.svc.crmFinishDispatch(r.Context(), row, "SUPPRESSED", guard, "")
		writeJSON(w, http.StatusOK, map[string]any{"sent": false, "suppressed_by": guard})
		return
	}
	tpl := crmTemplate{EN: in.BodyEN, HI: in.BodyHI}
	if tpl.HI == "" {
		tpl.HI = in.BodyEN
	}
	if err := (inappChannel{}).Send(r.Context(), h.svc, acct.ID, t, tpl, map[string]string{}); err != nil {
		h.svc.crmFinishDispatch(r.Context(), row, "SUPPRESSED", "G9_channel_availability", "inapp")
		writeErr(w, errInternal("send failed"))
		return
	}
	h.svc.crmFinishDispatch(r.Context(), row, "SENT", "", "inapp")
	h.svc.deps.Log.Info("crm: manual message sent", "by", actor.PartyID, "to", acct.ID.Hex())
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}

func (h *handler) crmOfferByPhone(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeErr(w, errForbidden("CRM is not enabled"))
		return
	}
	acct, err := h.svc.repo.findAccountByPhone(r.Context(), crmCanonicalPhone(normalizePhone(chi.URLParam(r, "phone"))))
	if err != nil || acct == nil {
		writeErr(w, errNotFound("no customer with that phone"))
		return
	}
	o, err := h.svc.repo.findOffer(r.Context(), acct.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"consumer_id": acct.ID.Hex(), "has_paid_order": acct.HasPaidOrder,
		"offer": o, "entitled_free_deliveries": entitledFreeDeliveries(o),
	})
}

// crmOverrideOffer — support moves a pack state with a MANDATORY reason
// (spec: "Delivery exceptions happen and support must be able to honour a
// pack without a developer"). Every override lands in the transition log.
func (h *handler) crmOverrideOffer(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeErr(w, errForbidden("CRM is not enabled"))
		return
	}
	actor, _ := operatorActor(r)
	var in struct {
		PackNo int    `json:"pack_no"`
		From   string `json:"from"`
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Reason) == "" {
		writeErr(w, errBadRequest("a reason is mandatory for an override"))
		return
	}
	if in.PackNo != 1 && in.PackNo != 2 {
		writeErr(w, errBadRequest("pack_no must be 1 or 2"))
		return
	}
	acct, err := h.svc.repo.findAccountByPhone(r.Context(), crmCanonicalPhone(normalizePhone(chi.URLParam(r, "phone"))))
	if err != nil || acct == nil {
		writeErr(w, errNotFound("no customer with that phone"))
		return
	}
	moved, err := h.svc.repo.transitionPack(r.Context(), acct.ID, in.PackNo, in.From, in.To,
		"OVERRIDE by "+actor.PartyID+": "+in.Reason, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !moved {
		writeErr(w, errConflict("STATE_MISMATCH", "the pack is not in the expected 'from' state"))
		return
	}
	h.svc.emitCRMEvent(r.Context(), "offer_pack_state_change", acct.ID,
		map[string]any{"pack_no": in.PackNo, "from": in.From, "to": in.To, "reason": "override"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handler) crmDispatchLog(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeErr(w, errForbidden("CRM is not enabled"))
		return
	}
	acct, err := h.svc.repo.findAccountByPhone(r.Context(), crmCanonicalPhone(normalizePhone(chi.URLParam(r, "phone"))))
	if err != nil || acct == nil {
		writeErr(w, errNotFound("no customer with that phone"))
		return
	}
	cur, err := h.svc.repo.crmDispatchCol().Find(r.Context(),
		bson.D{{Key: "consumer_id", Value: acct.ID}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100))
	if err != nil {
		writeErr(w, errInternal("dispatch log read failed"))
		return
	}
	var rows []crmDispatchRow
	if err := cur.All(r.Context(), &rows); err != nil {
		writeErr(w, errInternal("dispatch log decode failed"))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, map[string]any{
			"trigger_id": d.TriggerID, "ist_day": d.ISTDay, "category": d.Category,
			"template": d.Template, "status": d.Status, "guard": d.Guard,
			"channel": d.Channel, "at": d.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// crmSetFlag flips a per-trigger kill switch (G1) — admin-togglable without a
// deploy, restricted to the crm.* namespace so this route can never touch
// platform flags.
func (h *handler) crmSetFlag(w http.ResponseWriter, r *http.Request) {
	if !crmEnabled() {
		writeErr(w, errForbidden("CRM is not enabled"))
		return
	}
	actor, _ := operatorActor(r)
	var in struct {
		TriggerID string `json:"trigger_id"`
		Enabled   bool   `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.TriggerID == "" {
		writeErr(w, errBadRequest("trigger_id is required"))
		return
	}
	if h.svc.deps.Flags == nil {
		writeErr(w, errInternal("flags service unavailable"))
		return
	}
	if err := h.svc.deps.Flags.Set(r.Context(), crmFlagPrefix+in.TriggerID, in.Enabled, actor.PartyID); err != nil {
		writeErr(w, errInternal("flag write failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flag": crmFlagPrefix + in.TriggerID, "enabled": in.Enabled})
}
