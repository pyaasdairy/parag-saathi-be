# Operator push: store alerts on Saathi phones (FCM)

Founder decision 8 (25 Sep 2026): "a store manager's closing alert only visible while the
app is open is not an alert". The backend now rings the Saathi phones a store alert
concerns, through Firebase Cloud Messaging (FCM HTTP v1). Until the founder completes the
setup below, everything is inert: the app registers nothing (no Firebase config in the
build), the server sends nothing (no service account) and says so at boot, and every flow
works exactly as before with the in-app bell.

## What rings, and whom

| Alert | When | Who | Quiet hours 22:00-07:00 IST |
|---|---|---|---|
| `STORE_INSTANT_ORDER` "New instant order" | an instant order is broadcast as OFFERED | the store's ACTIVE store managers and riders | rings (order alert) |
| `STORE_INSTANT_CLOSING` "Instant delivery closes at 10:00 PM" | 15 min before instant closes (again before an extended close) | the store's managers | rings (delivery alert: the lane is running and the manager must act before it closes; extended closes run to 02:00) |
| `STORE_INSTANT_CLOSED` "Instant delivery is now closed" | at the close | the store's managers | held |
| `STORE_LOW_STOCK` "Low stock at <store>" | the store's low set changes (the same set posted again stays silent) | platform admins (SUPER_ADMIN / PCDF_ADMIN), as the inbox row | held |

"Held" means the phone does not ring; the inbox row is still written, so the Saathi bell
shows it on the next open. Nothing is queued for the morning. Recipient selection is
deliberately simple (store managers + the store's riders); it does not read attendance.

Each push carries `data.type` (the alert kind), `data.store_id` and, for an order,
`data.delivery_id` / `data.order_id`, so the app can route a tap. A newer alert of the same
kind for the same store (or the same order) replaces the older one in the tray.

## Contract

- `POST /api/v1/push/register` `{token, platform: "android"|"ios", provider: "fcm", app_version}`
  with any operator token (session or role). Answers
  `{registered: true, status: "registered", delivery: "fcm" | "pending_sender"}`.
  One row per device (`operator_push_devices`, unique on the token); the device belongs to
  whoever signed in on it last.
- `DELETE /api/v1/push/register` `{token}` at sign-out: 204, idempotent; only the caller's
  own row is removed.
- A token FCM reports `UNREGISTERED` is pruned. A party keeps at most its 10 newest rows
  (older ones go when a newer one registers) and each alert reaches at most its 5 newest
  devices, so one operator's old rows never crowd another recipient out. The consumer
  app's registry (`/consumer/push/register`, Expo) is separate and unchanged.
- A new instant order rings once per task: a duplicate create of the task (the store
  console's backfill racing the order) rings nothing.
- `POST /consumer/stores/{storeId}/low-stock` accepts the manager's own store only (403
  otherwise), so no manager can raise, clear or ring another store's low-stock alert.

Code: `internal/platform/push/fcm.go` (sender), `internal/platform/push/operator.go`
(registry, fan-out, quiet hours), `internal/modules/platformops/push_devices.go` (routes),
`internal/modules/consumer/operator_push.go` (which alert rings whom). Saathi:
`lib/services/push_service.dart`, `lib/api/push_api.dart`.

## Founder setup (one time)

1. **Firebase project.** In the Firebase console, create (or reuse) a project for PYAAS and
   add an **Android app** with package name **`in.pyaas.pyaas_saathi`** (the Saathi
   `applicationId`; not `com.pyaas.saathi`). Download **`google-services.json`**.
2. **Saathi build.** Put `google-services.json` in `pyaas-saathi/android/app/` on every
   machine that builds release APKs/AABs (it is gitignored, like `key.properties`; keep a
   copy with the signing files). The Gradle build applies the google-services plugin only
   when the file is there, so builds without it keep working, without push. Rebuild and
   ship the app.
3. **iOS (when Saathi gets an `ios/` folder).** Add an iOS app in the same Firebase project,
   put `GoogleService-Info.plist` in `ios/Runner/`, upload an **APNs authentication key**
   (.p8, Apple Developer -> Keys) under Project settings -> Cloud Messaging, and enable the
   Push Notifications capability in Xcode.
4. **Server credentials.** Firebase console -> Project settings -> Service accounts ->
   **Generate new private key**. On Render (the backend service), set
   **`FCM_SERVICE_ACCOUNT_JSON`** to the file's content (paste the JSON as is, or its base64
   if the dashboard mangles newlines: `base64 -w0 key.json`). Optionally set
   **`FCM_PROJECT_ID`** (defaults to the file's `project_id`). Redeploy. The boot log line
   `operator push (Saathi): FCM sender live for project ...` confirms it; `inert - ...`
   says what is missing.
5. **Check.** Sign in on Saathi as a store manager, allow notifications, and
   `POST /push/register` answers `delivery: "fcm"`. Place an instant order in the store's
   area: the manager's and the store's riders' phones ring "New instant order".

Keep the service-account file secret (it can send to every device of the project). Rotate
it by generating a new key, updating `FCM_SERVICE_ACCOUNT_JSON` and deleting the old key in
the Google Cloud console.

## Dry run

`FCM_BASE_URL=http://127.0.0.1:9009` sends every message to a local stub
(`POST /v1/projects/<project>/messages:send`, answer `{"name":"projects/p/messages/1"}`).
The OAuth2 token exchange goes to the service account's own `token_uri`, so a dry-run key
file points `token_uri` at the same stub (answer `{"access_token":"x","expires_in":3600}`).
Never set `FCM_BASE_URL` in production.
