# Deploy the Saathi backend on Render (free) + build the APK against it

The repo ships a `Dockerfile` (multi-stage, distroless-ish Alpine) and a
`render.yaml` blueprint. The database is MongoDB Atlas (already provisioned:
`saathi_dev`, minimal-seeded with SUPER_ADMIN `9999999999` and
ONBOARDING_EXECUTIVE `9876500014` — both with the fixture profile photo).

## 1 · Deploy the API (≈5 minutes)

1. Push this repo (`parag-saathi-be`) to GitHub — already at
   `github.com/pyaasdairy/parag-saathi-be`.
2. [dashboard.render.com](https://dashboard.render.com) → **New → Blueprint** →
   select `pyaasdairy/parag-saathi-be`. Render reads `render.yaml`.
3. When prompted for **MONGO_URI** (it is `sync: false`, never in git), paste
   the Atlas string:
   `mongodb+srv://<user>:<password>@cluster0.gqgvsoc.mongodb.net`
   (credentials are in the local `backend/.env`; `MONGO_DB` is already
   `saathi_dev`.)
   ⚠️ Atlas → Network Access → add `0.0.0.0/0` (or Render's egress IPs) so the
   service can reach the cluster.
4. Deploy. Render builds the Dockerfile and starts `saathi-server`; the
   health check is `GET /healthz`.
5. Verify: `curl https://saathi-backend.onrender.com/healthz` →
   `{"data":{"status":"ok"}}` (your exact URL is shown on the service page).

Notes
- **Free plan spins down** after ~15 min idle; the first request after that
  takes ~30–60 s. Upgrade to Starter for always-on.
- OTP_DEV_MODE=true is intentional for this test phase: the OTP comes back in
  the login response (no SMS provider needed). Flip to `false` + wire an SMS
  provider (env seam documented in `.env.example`) before production.
- The DB was seeded from a laptop; to re-provision, run locally:
  `MONGO_URI=<atlas> MONGO_DB=saathi_dev SEED_PROFILE_PHOTO_URL=<img-url> ./bin/saathi-seed -minimal`
  (never seed from Render itself).

## 2 · Point the app at the deployed API and build the APK

In `parag-saathi-fe`:

1. Edit `.env`:
   ```
   EXPO_PUBLIC_API_MODE=http
   EXPO_PUBLIC_API_BASE_URL=https://saathi-backend.onrender.com/api/v1
   ```
   (everything is env-driven — no code change; maps stay on the free keyless
   OSM template, the test-photo seam stays on EXPO_PUBLIC_TEST_PHOTO_URL.)
2. Build the release APK (pick one):
   - **Local Gradle** (no account needed):
     `cd android && ./gradlew assembleRelease`
     → `android/app/build/outputs/apk/release/app-release.apk`
     (if `android/` is absent run `npx expo prebuild -p android` once).
   - **EAS cloud build** (needs a free Expo account):
     `npx eas build -p android --profile preview`
     (add `"preview": {"android": {"buildType": "apk"}}` under `build` in
     `eas.json` if not present).
3. Install on any phone: `adb install app-release.apk` (or share the file).
4. Smoke: log in as `9999999999` (Super Admin) — the OTP shows on-screen
   (dev mode). Onboard everything else through the app as
   `9876500014` (Onboarding Executive, Neha Tripathi).

## 3 · What is intentionally left as env seams (add later, zero code changes)
- Real SMS / push provider (`SMS_PROVIDER`, `PUSH_PROVIDER=expo` is free)
- S3 presigned uploads (`EXPO_PUBLIC_S3_*`) — test photo seam meanwhile
- Paid maps key (`EXPO_PUBLIC_MAPS_PROVIDER=google` + key) — OSM free now
- Redis for multi-replica rate limiting (`REDIS_URL`)
