package platformops

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/pyaas/saathi-backend/internal/platform/auth"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/httpx"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
	"github.com/pyaas/saathi-backend/internal/platform/push"
)

// OPERATOR PUSH DEVICES - POST and DELETE /api/v1/push/register, the contract
// the Saathi app calls after sign-in (and on every FCM token refresh) and at
// sign-out. The rows live in operator_push_devices (internal/platform/push);
// the consumer module's store alerts read them to ring store managers' and
// riders' phones over FCM. The consumer app keeps its own registry at
// /consumer/push/register (Expo tokens under the consumer JWT).
//
// Any operator token may register, session or role: the device is bound to
// the PARTY, and which alerts reach it follows that party's ACTIVE role
// assignments at send time (a revoked store manager stops hearing the store
// without the handset doing anything). A consumer token fails Authenticate
// (another issuer).
func mountPushRoutes(r chi.Router, d *deps.Deps, h *handler) {
	r.Route("/push", func(r chi.Router) {
		r.Use(middleware.Authenticate(d.JWT))
		r.Post("/register", h.registerPushDevice)
		r.Delete("/register", h.unregisterPushDevice)
	})
}

func (h *handler) pushDevices() *push.OperatorRegistry {
	return push.NewOperatorRegistry(h.svc.deps.DB)
}

func pushActor(r *http.Request) (auth.Actor, primitive.ObjectID, error) {
	actor, ok := auth.ActorFrom(r.Context())
	if !ok {
		return actor, primitive.NilObjectID, httpx.Unauthorized("authentication required")
	}
	pid, err := primitive.ObjectIDFromHex(actor.PartyID)
	if err != nil {
		return actor, primitive.NilObjectID, httpx.Unauthorized("token carries no party")
	}
	return actor, pid, nil
}

func pushDeviceErr(err error) error {
	var bad push.ErrBadDevice
	if errors.As(err, &bad) {
		return httpx.BadRequest("INVALID_DEVICE", bad.Reason)
	}
	return httpx.Internal(err)
}

// registerPushDevice handles POST /push/register
// {"token","platform":"android|ios","provider":"fcm","app_version"} -> 200
// {"registered":true,"status":"registered","delivery":"fcm"|"pending_sender"}.
// delivery says plainly whether anything can be sent to the token yet.
func (h *handler) registerPushDevice(w http.ResponseWriter, r *http.Request) {
	actor, pid, err := pushActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in push.RegisterInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.pushDevices().Register(r.Context(), pid, actor.RoleCode, in, time.Now()); err != nil {
		httpx.Error(w, r, pushDeviceErr(err))
		return
	}
	delivery := "pending_sender"
	if h.svc.deps.OperatorPush.Enabled() {
		delivery = "fcm"
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"registered": true, "status": "registered", "delivery": delivery})
}

// unregisterPushDevice handles DELETE /push/register {"token"} -> 204, also
// when the row is absent or has moved to another party. The app calls it
// before it clears the session.
func (h *handler) unregisterPushDevice(w http.ResponseWriter, r *http.Request) {
	_, pid, err := pushActor(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.pushDevices().Unregister(r.Context(), pid, in.Token); err != nil {
		httpx.Error(w, r, pushDeviceErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
