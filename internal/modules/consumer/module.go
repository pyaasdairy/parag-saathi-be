package consumer

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pyaas/saathi-backend/internal/domain"
	"github.com/pyaas/saathi-backend/internal/platform/deps"
	"github.com/pyaas/saathi-backend/internal/platform/middleware"
)

// Register mounts the consumer backend under /consumer (→ /api/v1/consumer).
// Everything here is ADD-ONLY and isolated from the operator side. The consumer
// app points EXPO_PUBLIC_API_URL at .../api/v1/consumer so its bare paths
// (/auth/..., /users/me, /wallet, /addresses) compose onto this subtree.
func Register(r chi.Router, d *deps.Deps) {
	log := d.Log.With(slog.String("module", "consumer"))
	repo := newRepository(d.DB)
	svc := newService(d, repo, log)
	h := &handler{svc: svc}

	// Own indexes at startup (never touches shared Saathi index setup). This is
	// FATAL on failure: the unique (consumer, ref, type) index IS the wallet's
	// exactly-once money gate — booting without it would let a duplicate webhook
	// double-credit. Fail fast rather than serve money endpoints unguarded.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := repo.ensureIndexes(ctx); err != nil {
		log.Error("consumer index setup failed — refusing to boot", slog.Any("err", err))
		panic("consumer: index setup failed: " + err.Error())
	}

	// Repair QR integrity tokens signed under an older QR_SIGNING_SECRET so
	// already-printed QRs keep resolving after a secret rotation (qrresign.go).
	// Best-effort: runs in the background, never blocks or fails boot.
	go resignQRTokens(d, log)

	// Server-owned subscription scheduler (subscriptions.go): every 15 min,
	// turn today's DUE subscriptions (daily/alternate/weekly, minus vacations)
	// into morning-lane orders + store delivery tasks — so the store manager's
	// queue fills even when no consumer opens the app. Exactly-once per
	// (subscription, IST day); money still settles on delivery.
	go svc.subscriptionOrderWorker(context.Background())

	r.Route("/consumer", func(cr chi.Router) {
		// Raw-JSON 404/405 so the FE apiClient reads {message}, not the
		// operator envelope, on unknown consumer routes.
		cr.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 404, &apiError{Code: "NOT_FOUND", Message: "not found"})
		})
		cr.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 405, &apiError{Code: "METHOD_NOT_ALLOWED", Message: "method not allowed"})
		})

		// ── Public (no auth) ──
		cr.Route("/auth", func(ar chi.Router) {
			ar.Post("/otp/request", h.otpRequest)
			ar.Post("/otp/verify", h.otpVerify)
			ar.Post("/refresh", h.refresh)
			ar.Post("/logout", h.logout)
		})

		// Traceability bridge — consumer-app-gated (X-Parag-App-Key when
		// CONSUMER_APP_KEY is set). Resolves a scanned pack QR to provenance JSON,
		// and renders a printable/downloadable HTML label (all values + QR) that the
		// app turns into a PDF via expo-print. Reuses the operator's public QR
		// resolver read-only.
		cr.Get("/traceability/{code}", h.traceByCode)
		cr.Get("/traceability/{code}/label", h.traceLabel)

		// Consumer catalog OVERLAY — consumer-app-gated (same X-Parag-App-Key as
		// traceability). The store-manager-owned overrides + additions the app
		// merges onto its shipped milk baseline (catalog.go).
		cr.Get("/catalog", h.getCatalog)

		// Geofence serviceability — consumer-app-gated (same X-Parag-App-Key).
		// The app asks "can we deliver here, how fast?" before checkout, and lets
		// an out-of-area shopper join the waitlist (upsert by phone). (geofence.go)
		cr.Get("/serviceability", h.serviceability)
		cr.Post("/waitlist", h.joinWaitlist)

		// ── Authenticated (consumer JWT) ──
		cr.Group(func(pr chi.Router) {
			pr.Use(svc.authenticate)

			// Profile — support the FE's /users/me and the note's /me alias.
			for _, base := range []string{"/users/me", "/me"} {
				pr.Get(base, h.me)
				pr.Patch(base, h.patchMe)
				pr.Put(base, h.patchMe)
				pr.Delete(base, h.erase)
			}
			pr.Post("/me/erasure", h.erase)

			// Wallet — canonical + FE-alias paths.
			pr.Get("/wallet", h.getWallet)
			pr.Get("/wallet/txns", h.walletTxns)
			pr.Get("/wallet/transactions", h.walletTxns)
			pr.Post("/wallet/topup", h.topup)
			pr.Post("/wallet/recharge", h.topup)
			// Marketing grant seam (free-pack funnel): REWARDS-only credit,
			// idempotent by ref, dev-gated like /wallet/topup.
			pr.Post("/wallet/promo", h.promo)
			// Real money-in path (Razorpay): create amount-bound order → verify
			// signature server-side → credit exactly once.
			pr.Post("/wallet/order", h.walletOrder)
			pr.Post("/wallet/verify", h.walletVerify)
			// Server-authoritative spend/refund (promo-first, idempotent by ref).
			pr.Post("/wallet/debit", h.walletDebit)
			pr.Post("/wallet/refund", h.walletRefund)

			// UPI-AutoPay / e-mandate — subscription auto-renewal (mandate.go).
			// Create a recurring authorization (mock token in the dev seam), verify
			// its registration payment, and drive the pause/resume/cancel state
			// machine. Daily EXECUTIONS charge via the SAME exactly-once wallet
			// settle path, idempotent by (mandateId, day). GET /me lists the
			// caller's mandates.
			pr.Post("/mandate/create", h.createMandate)
			pr.Post("/mandate/verify", h.verifyMandate)
			pr.Get("/mandate/me", h.listMandates)
			pr.Post("/mandate/{id}/pause", h.mandateAction("pause"))
			pr.Post("/mandate/{id}/resume", h.mandateAction("resume"))
			pr.Post("/mandate/{id}/cancel", h.mandateAction("cancel"))
			pr.Post("/mandate/{id}/execute", h.executeMandate) // dev-only manual tick

			// Server-owned subscriptions (subscriptions.go) — the backend twin of
			// the app's local subscription rows. The worker turns these into the
			// daily/alternate/weekly MORNING orders (store-routed via the default
			// saved address), so delivery no longer depends on the app being open.
			pr.Post("/subscriptions", h.createSubscription)
			pr.Get("/subscriptions", h.listSubscriptions)
			pr.Get("/subscriptions/me", h.listSubscriptions)
			pr.Patch("/subscriptions/{id}", h.patchSubscription)
			pr.Post("/subscriptions/{id}/pause", h.subscriptionAction("pause"))
			pr.Post("/subscriptions/{id}/resume", h.subscriptionAction("resume"))
			pr.Post("/subscriptions/{id}/cancel", h.subscriptionAction("cancel"))
			pr.Post("/subscriptions/sweep", h.sweepSubscriptions) // dev-only manual tick

			// Subscription welcome trial ("3 PAID then 3 FREE"): the caller's trial
			// standing (phase + paid/free days remaining). The window is spent on
			// DELIVERED days by the delivery settle path (trial.go), never here.
			pr.Get("/trial/me", h.trialMe)

			// Addresses.
			pr.Get("/addresses", h.listAddresses)
			pr.Post("/addresses", h.createAddress)
			pr.Patch("/addresses/{id}", h.patchAddress)
			pr.Post("/addresses/{id}/default", h.defaultAddress)
			pr.Delete("/addresses/{id}", h.deleteAddress)

			// Orders — the backend owns the order; money is debited on delivery
			// via the server wallet. Scoped to the authenticated shopper.
			pr.Get("/orders", h.listOrders)
			pr.Post("/orders", h.createOrder)
			pr.Get("/orders/{id}", h.getOrder)
			pr.Post("/orders/{id}/cancel", h.cancelOrder)
			pr.Post("/orders/{id}/review", h.reviewOrder)
			pr.Post("/orders/{id}/advance", h.advanceOrder) // dev-only status transition
			// Pay an order directly via the gateway (seam): create an amount-bound
			// Razorpay order for the order total. Default payment mode is 'wallet'
			// (settled on delivery); this lets the FE offer a direct-pay path later.
			// Dev-gated until the order-pay verify/capture flow lands.
			pr.Post("/orders/{id}/pay", h.payOrder)
		})

		// ── Operator surfaces (SAATHI operator token + role) ──
		// The store manager and delivery rider are Saathi operators; these routes
		// reuse Saathi's auth + wire format ({data} envelope), consumed by the
		// Saathi FE (store.ts / delivery.ts, service:'consumer').
		cr.Group(func(op chi.Router) {
			op.Use(middleware.Authenticate(d.JWT))

			// Store manager (STORE_MANAGER): orders in the store's vicinity, its
			// rider roster (with distance tiers), and rider assignment.
			op.Group(func(sm chi.Router) {
				sm.Use(middleware.RequireRoles(domain.RoleStoreManager))
				sm.Get("/stores/{storeId}/orders", h.storeOrders)
				sm.Get("/stores/{storeId}/riders", h.storeRiders)
				sm.Post("/stores/{storeId}/orders/{deliveryId}/assign", h.assignRider)
				// Order surgery at handover: cancel (crate damaged) or reduce
				// quantities (deliver 2 of 3) — the bill re-computes and the
				// customer is charged only for what actually ships.
				sm.Post("/stores/{storeId}/orders/{deliveryId}/cancel", h.storeCancelDelivery)
				sm.Patch("/stores/{storeId}/orders/{deliveryId}/items", h.storeAdjustDelivery)
				sm.Post("/stores/{storeId}/low-stock", h.lowStock)

				// Consumer catalog overlay console (catalog.go): view the milk
				// baseline + this store's overrides/additions, override a SKU's
				// price/stock/visibility, and add or remove store SKUs.
				sm.Get("/stores/{storeId}/skus", h.listSkus)
				sm.Post("/stores/{storeId}/skus", h.addSku)
				sm.Patch("/stores/{storeId}/skus/{skuId}", h.patchSku)
				sm.Delete("/stores/{storeId}/skus/{skuId}", h.deleteSkuHandler)

				// Serviceability zone (geofence.go): view / draw the store's serving
				// area (instant + standard circles, include/exclude pincodes + polygons).
				sm.Get("/stores/{storeId}/zone", h.getZone)
				sm.Put("/stores/{storeId}/zone", h.putZone)
			})

			// Delivery rider (DELIVERY_RIDER): the last-mile task lifecycle.
			op.Group(func(dr chi.Router) {
				dr.Use(middleware.RequireRoles(domain.RoleDeliveryRider))
				dr.Get("/delivery/tasks", h.riderDeliveries)
				dr.Get("/delivery/tasks/available", h.riderAvailable) // OFFERED broadcast pool
				dr.Get("/delivery/tasks/{deliveryId}", h.riderGetDelivery)
				dr.Post("/delivery/tasks/{deliveryId}/accept", h.riderAccept) // manager-assigned
				dr.Post("/delivery/tasks/{deliveryId}/claim", h.riderClaim)   // first-accept-wins
				dr.Post("/delivery/tasks/{deliveryId}/reject", h.riderReject) // decline → re-broadcast
				dr.Post("/delivery/tasks/{deliveryId}/pickup", h.riderPickup)
				dr.Post("/delivery/tasks/{deliveryId}/location", h.riderLocation)
				dr.Post("/delivery/tasks/{deliveryId}/deliver", h.riderDeliver)
				dr.Post("/delivery/tasks/{deliveryId}/fail", h.riderFail)
			})
		})
	})

	log.Info("consumer backend mounted at /api/v1/consumer")
}
