// Lead capture — the two consumer-app forms whose submissions must reach the
// founder, not a device-local table nobody reads:
//
//	POST /consumer/partner-leads   — "Partner with us" (bulk order / franchise /
//	  vendor) enquiry. App-key-gated like /waitlist: a prospective partner is
//	  often NOT signed in.
//	POST /consumer/wishlist/leads  — "Notify me" restock demand on an
//	  out-of-stock SKU. Upserted per (user, product) with a tap counter so
//	  repeat taps read as intensity, not duplicates.
//
// Both existed in the FE first (lib/leads.ts posted here and 404'd, silently
// falling back to the on-device table) — these routes close that gap.
package consumer

import (
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	collPartnerLeads  = "consumer_partner_leads"
	collWishlistLeads = "consumer_wishlist_leads"
)

// createPartnerLead — POST /consumer/partner-leads (app-key-gated).
func (h *handler) createPartnerLead(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("partner leads are accepted from the PYAAS app only"))
		return
	}
	var body struct {
		Kind         string         `json:"kind"`
		Name         string         `json:"name"`
		Phone        string         `json:"phone"`
		Email        *string        `json:"email"`
		BusinessName *string        `json:"business_name"`
		City         *string        `json:"city"`
		Message      *string        `json:"message"`
		Details      map[string]any `json:"details"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	name, phone := strings.TrimSpace(body.Name), strings.TrimSpace(body.Phone)
	if name == "" || phone == "" {
		writeErr(w, errBadRequest("name and phone are required"))
		return
	}
	doc := bson.D{
		{Key: "kind", Value: strings.TrimSpace(body.Kind)},
		{Key: "name", Value: name},
		{Key: "phone", Value: phone},
		{Key: "email", Value: body.Email},
		{Key: "business_name", Value: body.BusinessName},
		{Key: "city", Value: body.City},
		{Key: "message", Value: body.Message},
		{Key: "details", Value: body.Details},
		{Key: "created_at", Value: time.Now().UTC()},
	}
	if _, err := h.svc.deps.DB.Collection(collPartnerLeads).InsertOne(r.Context(), doc); err != nil {
		writeErr(w, errInternal("lead store failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// createWishlistLead — POST /consumer/wishlist/leads (app-key-gated). Upsert
// per (user, product); `taps` counts repeat interest.
func (h *handler) createWishlistLead(w http.ResponseWriter, r *http.Request) {
	if !h.svc.appKeyOK(r) {
		writeErr(w, errForbidden("wishlist leads are accepted from the PYAAS app only"))
		return
	}
	var body struct {
		UserID      string `json:"user_id"`
		ProductID   string `json:"product_id"`
		ProductName string `json:"product_name"`
		Variant     string `json:"variant"`
		Source      string `json:"source"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	pid := strings.TrimSpace(body.ProductID)
	if pid == "" {
		writeErr(w, errBadRequest("product_id is required"))
		return
	}
	uid := strings.TrimSpace(body.UserID)
	if uid == "" {
		uid = "anon"
	}
	now := time.Now().UTC()
	_, err := h.svc.deps.DB.Collection(collWishlistLeads).UpdateOne(r.Context(),
		bson.D{{Key: "user_id", Value: uid}, {Key: "product_id", Value: pid}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "product_name", Value: strings.TrimSpace(body.ProductName)},
				{Key: "variant", Value: strings.TrimSpace(body.Variant)},
				{Key: "source", Value: strings.TrimSpace(body.Source)},
				{Key: "updated_at", Value: now},
			}},
			{Key: "$inc", Value: bson.D{{Key: "taps", Value: 1}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: now}}},
		}, options.Update().SetUpsert(true))
	if err != nil {
		writeErr(w, errInternal("wishlist lead store failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// dolibarrWebhook — POST /consumer/dolibarr/webhook?token=… (also accepts the
// X-Webhook-Token header). Wired to the DoliCloud Webhook module so a product
// edit / stock movement in the ERP kicks the catalog sync IMMEDIATELY instead
// of waiting for the next poll tick. The payload body is ignored on purpose:
// the sync always re-reads the ERP as the source of truth, so a forged body
// can never inject data — the token only gates who may spend our sync cycles.
func (h *handler) dolibarrWebhook(w http.ResponseWriter, r *http.Request) {
	want := h.svc.deps.Cfg.DolibarrWebhookToken
	if want == "" {
		want = h.svc.deps.Cfg.DolibarrAPIKey // sensible default: the DOLAPIKEY
	}
	got := r.URL.Query().Get("token")
	if got == "" {
		got = r.Header.Get("X-Webhook-Token")
	}
	if want == "" || got != want {
		writeErr(w, errForbidden("bad webhook token"))
		return
	}
	h.svc.KickDolibarrSync()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "kicked": true})
}
