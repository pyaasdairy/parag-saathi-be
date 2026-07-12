package consumer

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type handler struct{ svc *service }

// actorID resolves the authenticated consumer's ObjectID from context.
func actorID(r *http.Request) (primitive.ObjectID, *apiError) {
	a, ok := actorFrom(r.Context())
	if !ok {
		return primitive.NilObjectID, errUnauthorized("authentication required")
	}
	id, err := primitive.ObjectIDFromHex(a.ID)
	if err != nil {
		return primitive.NilObjectID, errUnauthorized("bad token subject")
	}
	return id, nil
}

func pathID(r *http.Request, key string) (primitive.ObjectID, *apiError) {
	id, err := primitive.ObjectIDFromHex(chi.URLParam(r, key))
	if err != nil {
		return primitive.NilObjectID, errBadRequest("invalid id")
	}
	return id, nil
}

// ── Auth ────────────────────────────────────────────────────────────────────

func (h *handler) otpRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	devOTP, expires, err := h.svc.requestOTP(r.Context(), body.Phone)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp := map[string]any{"sent": true, "expires_at": expires}
	if devOTP != "" {
		resp["dev_otp"] = devOTP
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) otpVerify(w http.ResponseWriter, r *http.Request) {
	// The FE posts {phone, code}; accept `otp` as an alias too.
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
		OTP   string `json:"otp"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	code := body.Code
	if code == "" {
		code = body.OTP
	}
	pair, err := h.svc.verifyOTP(r.Context(), body.Phone, code)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pair)
}

func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	pair, err := h.svc.refresh(r.Context(), body.RefreshToken)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pair)
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = decode(r, &body)
	_ = h.svc.logout(r.Context(), body.RefreshToken)
	writeJSON(w, http.StatusNoContent, nil)
}

// ── Profile ─────────────────────────────────────────────────────────────────

func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	acct, err := h.svc.me(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func (h *handler) patchMe(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var patch map[string]any
	if err := decode(r, &patch); err != nil {
		writeErr(w, err)
		return
	}
	acct, err := h.svc.updateMe(r.Context(), id, patch)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func (h *handler) erase(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	if err := h.svc.erase(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// ── Wallet ──────────────────────────────────────────────────────────────────

func (h *handler) getWallet(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	view, err := h.svc.wallet(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) topup(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var body struct {
		Amount float64 `json:"amount"`
		Method string  `json:"method"`
		Ref    string  `json:"ref"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	view, err := h.svc.topup(r.Context(), id, body.Amount, body.Method, body.Ref)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handler) walletTxns(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	txns, err := h.svc.walletTxns(r.Context(), id, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, txns)
}

// ── Addresses ───────────────────────────────────────────────────────────────

type addressInput struct {
	Label     string   `json:"label"`
	Line1     string   `json:"line1"`
	Line2     string   `json:"line2"`
	City      string   `json:"city"`
	Pincode   string   `json:"pincode"`
	IsDefault bool     `json:"is_default"`
	Lat       *float64 `json:"lat"`
	Lng       *float64 `json:"lng"`
}

func (h *handler) listAddresses(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	list, err := h.svc.listAddresses(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) createAddress(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var in addressInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	a, err := h.svc.createAddress(r.Context(), id, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *handler) patchAddress(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	addrID, perr := pathID(r, "id")
	if perr != nil {
		writeErr(w, perr)
		return
	}
	var body struct {
		Lat *float64 `json:"lat"`
		Lng *float64 `json:"lng"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Lat == nil || body.Lng == nil {
		writeErr(w, errBadRequest("lat and lng are required"))
		return
	}
	a, err := h.svc.setAddressGeo(r.Context(), id, addrID, *body.Lat, *body.Lng)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *handler) defaultAddress(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	addrID, perr := pathID(r, "id")
	if perr != nil {
		writeErr(w, perr)
		return
	}
	a, err := h.svc.makeDefault(r.Context(), id, addrID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *handler) deleteAddress(w http.ResponseWriter, r *http.Request) {
	id, aerr := actorID(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	addrID, perr := pathID(r, "id")
	if perr != nil {
		writeErr(w, perr)
		return
	}
	if err := h.svc.deleteAddress(r.Context(), id, addrID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
