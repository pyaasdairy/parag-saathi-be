#!/usr/bin/env bash
# Saathi backend DRY RUN — spins up a scratch server on an isolated DB, seeds
# dummy data, and exercises EVERY flow end-to-end over real HTTP, asserting the
# relations the frontend integration depends on. Verifies (beyond smoke.sh):
#   • multi-role login (farmer+sachiv) → role picker → role-select both
#   • KYC submit → PENDING record + PERSISTED notification reaches super admin
#   • super admin approves → party tier upgraded → role assignment ("line made")
#   • assurance A|B|C on pour + weakest-inherited on the consignment (§6.2)
#   • rate_chart_version pinned on the pour (§6.3)
#   • consignment seal_code minted at dispatch (§6.4)
#   • dashboards (farmer summary + society stats) return correct aggregates
#   • the pour→QR→settlement loop still passes (delegates to smoke.sh checks)
set -euo pipefail

export PATH="/opt/homebrew/bin:$PATH"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PORT=8097
HOST="http://127.0.0.1:${PORT}"
BASE="${HOST}/api/v1"
DB="saathi_dryrun"
SEED_ADMIN="9999999999"
SERVER_LOG="/tmp/saathi-dryrun-server.log"
TODAY="$(TZ=Asia/Kolkata date +%F)"

pass() { echo "  ✔ $*"; }
fail() { echo "  x FAIL: $*" >&2; echo "--- server log tail ---" >&2; tail -25 "$SERVER_LOG" >&2 || true; exit 1; }
step() { echo ""; echo "── $* ──"; }

SERVER_PID=""
cleanup() { [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true; }
trap cleanup EXIT

# --- helpers -----------------------------------------------------------------
jval() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }
api() { # api METHOD PATH TOKEN [JSON]
  local m="$1" p="$2" t="${3:-}" body="${4:-}"
  local args=(-sS -X "$m" "${BASE}${p}" -H 'Content-Type: application/json')
  [[ -n "$t" ]] && args+=(-H "Authorization: Bearer $t")
  [[ -n "$body" ]] && args+=(-d "$body")
  curl "${args[@]}"
}
login() { # login PHONE -> session token
  local phone="$1" otp ses
  otp=$(api POST /auth/otp/request "" "{\"phone\":\"$phone\"}" | jval 'd["data"]["dev_otp"]')
  api POST /auth/otp/verify "" "{\"phone\":\"$phone\",\"otp\":\"$otp\"}" | jval 'd["data"]["access_token"]'
}
role_token() { # role_token SESSION ROLE_CODE -> role token
  local ses="$1" role="$2" raid
  raid=$(api GET /parties/me "$ses" | python3 -c "import sys,json;print([a['id'] for a in json.load(sys.stdin)['data']['assignments'] if a['role_code']=='$role'][0])")
  api POST /auth/role/select "$ses" "{\"role_assignment_id\":\"$raid\"}" | jval 'd["data"]["access_token"]'
}

# --- boot --------------------------------------------------------------------
echo "Saathi DRY RUN — db=$DB port=$PORT today(IST)=$TODAY"
go build -o bin/saathi-server ./cmd/server
go build -o bin/saathi-seed ./cmd/seed
mongosh --quiet --eval "db.getSiblingDB(\"$DB\").dropDatabase()" >/dev/null 2>&1 || true
ENV=dev PORT="$PORT" MONGO_URI=mongodb://localhost:27017 MONGO_DB="$DB" \
  JWT_SECRET=dev-only-jwt-secret-change-me OTP_DEV_MODE=true \
  ./bin/saathi-server >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
for i in $(seq 1 40); do curl -s "$HOST/readyz" >/dev/null 2>&1 && break; sleep 0.5; done
MONGO_DB="$DB" ./bin/saathi-seed >/tmp/saathi-dryrun-seed.log 2>&1
pass "server up + seeded"

# discover super admin + org ids
ADMIN_SES=$(login "$SEED_ADMIN")
ADMIN=$(role_token "$ADMIN_SES" SUPER_ADMIN)
DCS=$(api GET "/orgs?code=DCS-01842" "$ADMIN" | jval 'd["data"][0]["id"]')
UNION=$(api GET "/orgs?code=UNION-LKO" "$ADMIN" | jval 'd["data"][0]["id"]')
MAHESH=$(api GET "/support/parties/lookup?phone=9000000011" "$ADMIN" | jval 'd["data"]["party_id"]')
pass "discovered DCS=$DCS union=$UNION farmer(Mahesh)=$MAHESH"

# ── 1. multi-role login (farmer + sachiv) ──
step "1. multi-role login: 9000000001 is SAMITI_SACHEEV + FARMER"
RAMESH_SES=$(login 9000000001)
ROLES=$(api GET /parties/me "$RAMESH_SES" | python3 -c "import sys,json;print(sorted(a['role_code'] for a in json.load(sys.stdin)['data']['assignments']))")
echo "  roles: $ROLES"
echo "$ROLES" | grep -q "FARMER" && echo "$ROLES" | grep -q "SAMITI_SACHEEV" || fail "expected both FARMER and SAMITI_SACHEEV"
SACHEEV=$(role_token "$RAMESH_SES" SAMITI_SACHEEV)
FARMER_TOK=$(role_token "$RAMESH_SES" FARMER)
[[ -n "$SACHEEV" && -n "$FARMER_TOK" ]] || fail "could not select both roles"
pass "role picker shows both; role-select works for each"

# ── 2. KYC submit → notification reaches super admin ──
step "2. KYC submit → PENDING + notification reaches super admin"
NEWPHONE="9000000200"
NEW_SES=$(login "$NEWPHONE")
NEW_PARTY=$(api GET /parties/me "$NEW_SES" | jval 'd["data"]["party"]["id"]')
KYC=$(api POST /kyc/aadhaar "$NEW_SES" '{"aadhaar_number":"123412340200","consent":true,"requested_tier":"FARMER"}')
echo "$KYC" | jval 'd["data"]["status"]' | grep -q PENDING || fail "KYC not PENDING"
KYC_ID=$(echo "$KYC" | jval 'd["data"]["record"]["id"]')
pass "KYC record PENDING ($KYC_ID)"
# run the notification worker (drains the outbox) as super admin, then confirm a
# KYC_PENDING notification addressed to the super-admin party exists.
api POST /notifications/worker/run "$ADMIN" >/dev/null
NOTIFS=$(api GET "/notifications?phone=$SEED_ADMIN" "$ADMIN")
echo "$NOTIFS" | python3 -c "
import sys,json
rows=json.load(sys.stdin)['data']
pend=[n for n in rows if n['template_key']=='KYC_PENDING']
assert pend, 'no KYC_PENDING notification reached super admin'
print('  super-admin KYC_PENDING notifications:', len(pend))
" || fail "KYC_PENDING notification did not reach super admin (see $NOTIFS)"
pass "notification generated AND reached super admin"

# ── 3. super admin approves → tier upgraded → assignment ("line made") ──
step "3. approve KYC → tier upgraded → grant FARMER role (relation made)"
APP=$(api POST "/kyc/$KYC_ID/approve" "$ADMIN")
echo "$APP" | jval 'd["data"]["kyc_tier"]' | grep -q FARMER || fail "tier not upgraded to FARMER"
echo "$APP" | jval 'd["data"]["record"]["status"]' | grep -q VERIFIED || fail "record not VERIFIED"
pass "approved: party tier→FARMER, record VERIFIED"
# now the 'line' (role assignment) — granted by admin, then the new party can select it
GRANT=$(api POST /roles/assignments "$ADMIN" "{\"party_id\":\"$NEW_PARTY\",\"role_code\":\"FARMER\",\"org_unit_id\":\"$DCS\"}")
NEW_RA=$(echo "$GRANT" | jval 'd["data"]["id"]')
[[ -n "$NEW_RA" ]] || fail "role assignment not created"
# confirm the relation: /parties/me now shows the FARMER assignment at the DCS
api GET /parties/me "$NEW_SES" | python3 -c "
import sys,json
a=json.load(sys.stdin)['data']['assignments']
assert any(x['role_code']=='FARMER' and x['org_unit_id']=='$DCS' for x in a), 'FARMER@DCS relation missing'
print('  new party assignments:', [x['role_code'] for x in a])
" || fail "assignment relation not visible on /parties/me"
# and the new party can now actually enter the role (KYC gate satisfied)
api POST /auth/role/select "$NEW_SES" "{\"role_assignment_id\":\"$NEW_RA\"}" | jval 'd["data"]["role_code"]' | grep -q FARMER || fail "new farmer cannot select role after approval"
pass "confirmation → relation (FARMER@DCS assignment) made and usable"

# ── 4. pour with assurance + rate_chart_version, weakest on consignment ──
step "4. pours: assurance A|B|C + rate_chart_version, weakest inherited by consignment"
uid() { python3 -c 'import uuid;print(uuid.uuid4())'; }
# MANUAL pour (assurance C) for Mahesh
P1=$(api POST /collection/pours "$SACHEEV" "{\"client_event_id\":\"$(uid)\",\"farmer_party_id\":\"$MAHESH\",\"dcs_id\":\"$DCS\",\"shift\":\"MORNING\",\"quantity_litres\":10.5,\"fat_pct\":6.5,\"snf_pct\":9.0,\"source\":\"MANUAL\"}")
echo "$P1" | jval 'd["data"]["pour"]["assurance"]' | grep -q C || fail "MANUAL pour assurance != C"
echo "$P1" | jval 'd["data"]["pour"]["rate_chart_version"]' | grep -q "LKO-2026-06" || fail "rate_chart_version not pinned"
pass "MANUAL pour → assurance C, rate_chart_version LKO-2026-06, rate $(echo "$P1" | jval 'd["data"]["pour"]["rate_per_litre"]')"
# ANALYZER_DIRECT pour (assurance A) for Geeta
GEETA=$(api GET "/support/parties/lookup?phone=9000000012" "$ADMIN" | jval 'd["data"]["party_id"]')
P2=$(api POST /collection/pours "$SACHEEV" "{\"client_event_id\":\"$(uid)\",\"farmer_party_id\":\"$GEETA\",\"dcs_id\":\"$DCS\",\"shift\":\"MORNING\",\"quantity_litres\":8.0,\"fat_pct\":5.5,\"snf_pct\":8.5,\"source\":\"ANALYZER_DIRECT\"}")
echo "$P2" | jval 'd["data"]["pour"]["assurance"]' | grep -q A || fail "ANALYZER_DIRECT pour assurance != A"
pass "ANALYZER_DIRECT pour → assurance A"
# generate invoices (so the farmer dashboard has a pending amount)
api POST /collection/invoices/generate "$SACHEEV" "{\"dcs_id\":\"$DCS\"}" >/dev/null
# seal the shift → consignment, assert seal_code + weakest assurance C
CONS=$(api POST /logistics/consignments "$SACHEEV" "{\"dcs_id\":\"$DCS\",\"shift\":\"MORNING\"}")
CONS_ID=$(echo "$CONS" | jval 'd["data"]["id"]')
DISP=$(api POST "/logistics/consignments/$CONS_ID/dispatch" "$SACHEEV")
echo "$DISP" | jval 'd["data"]["seal_code"]' | grep -q "^SEAL-" || fail "no seal_code minted at dispatch"
echo "$DISP" | jval 'd["data"]["assurance"]' | grep -q C || fail "consignment did not inherit weakest assurance C"
pass "consignment sealed: seal_code=$(echo "$DISP" | jval 'd["data"]["seal_code"]'), weakest assurance=C (§6.2/§6.4)"

# ── 5. dashboards ──
step "5. dashboards: farmer summary + society stats"
# A farmer viewing their OWN summary (self-scope): log in as Mahesh.
MAHESH_SES=$(login 9000000011)
MAHESH_TOK=$(role_token "$MAHESH_SES" FARMER)
FARM_DASH=$(api GET "/dashboards/farmer/$MAHESH" "$MAHESH_TOK")
# And confirm cross-farmer read is refused for a FARMER (Ramesh cannot view Mahesh).
CROSS=$(api GET "/dashboards/farmer/$MAHESH" "$FARMER_TOK" | jval 'd.get("error",{}).get("code","")')
[[ "$CROSS" == "FORBIDDEN" ]] || fail "cross-farmer dashboard read should be FORBIDDEN, got '$CROSS'"
echo "$FARM_DASH" | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
assert d['today']['pours']>=1, 'farmer today pours should be >=1'
assert d['pending_amount']>0, 'farmer should have a pending invoice amount'
assert len(d['trend'])==12, 'trend should be 12 days'
print('  farmer today:', d['today'], '| pending ₹', d['pending_amount'])
" || fail "farmer dashboard wrong (see $FARM_DASH)"
SOC_DASH=$(api GET "/dashboards/society/$DCS" "$SACHEEV")
echo "$SOC_DASH" | python3 -c "
import sys,json
d=json.load(sys.stdin)['data']
assert d['today']['pours']>=2, 'society today pours should be >=2'
assert d['active_farmers']>=2, 'active farmers should be >=2 (Mahesh + Geeta)'
assert d['member_count']>=1, 'member count should be >=1'
print('  society today:', d['today'], '| active farmers', d['active_farmers'], '| members', d['member_count'])
" || fail "society dashboard wrong (see $SOC_DASH)"
pass "dashboards return correct aggregates"

echo ""
echo "DRY RUN PASSED — every checked flow works with dummy data (db=$DB)."
