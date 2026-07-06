#!/usr/bin/env bash
# scripts/smoke.sh — end-to-end test of the ENTIRE Saathi loop over real HTTP.
#
# Self-contained: drops the scratch DB, builds + starts its own server on :8091
# against MONGO_DB=saathi_smoke, seeds, drives the full pour→QR→settlement loop
# plus the negative gates, and always kills the server on exit.
#
# Requires: bash, curl, python3, mongosh, go. No jq.
set -euo pipefail

export PATH="/opt/homebrew/bin:$PATH"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PORT=8091
HOST="http://127.0.0.1:${PORT}"
BASE="${HOST}/api/v1"
SMOKE_DB="saathi_smoke"
SERVER_PID=""
SERVER_LOG="$(mktemp -t saathi-smoke-server.XXXXXX.log)"

STATUS=""
BODY=""
CURRENT_STEP="setup"

# ── plumbing ────────────────────────────────────────────────────────────────

log()  { echo "$*" >&2; }
pass() { log "PASS  step $CURRENT_STEP — $*"; }

fail() {
  log "FAIL  step $CURRENT_STEP — $*"
  log "----- offending response (HTTP ${STATUS:-n/a}) -----"
  log "${BODY:-<empty body>}"
  log "---------------------------------------------------"
  exit 1
}

cleanup() {
  local code=$?
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ $code -ne 0 ]]; then
    log ""
    log "===== last 40 lines of server log ($SERVER_LOG) ====="
    tail -n 40 "$SERVER_LOG" >&2 || true
  fi
  exit $code
}
trap cleanup EXIT

step() { CURRENT_STEP="$1"; shift; log ""; log "── step $CURRENT_STEP: $* ──"; }

# req METHOD PATH TOKEN [JSON_BODY] → sets STATUS + BODY
req() {
  local method="$1" path="$2" token="${3:-}" data="${4:-}"
  local args=(-sS -X "$method" "${BASE}${path}" -H 'Content-Type: application/json')
  [[ -n "$token" ]] && args+=(-H "Authorization: Bearer $token")
  [[ -n "$data" ]] && args+=(--data "$data")
  local out
  out="$(curl "${args[@]}" -w $'\n%{http_code}')" || fail "curl error on $method $path"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
}

# raw_req METHOD URL → operational endpoints outside /api/v1
raw_req() {
  local method="$1" url="$2"
  local out
  out="$(curl -sS -X "$method" "$url" -w $'\n%{http_code}')" || fail "curl error on $url"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
}

expect_status() {
  local want="$1" what="$2"
  [[ "$STATUS" == "$want" ]] || fail "$what: expected HTTP $want, got HTTP $STATUS"
}

# jval 'PYTHON_EXPR' — evaluate over parsed BODY (as d); prints result
jval() {
  printf '%s' "$BODY" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print($1)
" 2>/dev/null || fail "could not extract: $1"
}

# jassert 'PYTHON_BOOL_EXPR' 'description'
jassert() {
  local expr="$1" what="$2"
  local res
  res="$(printf '%s' "$BODY" | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('OK' if ($expr) else 'NO')
" 2>/dev/null)" || fail "$what (assertion crashed: $expr)"
  [[ "$res" == "OK" ]] || fail "$what (assertion false: $expr)"
}

py_uuid() { python3 -c 'import uuid; print(uuid.uuid4())'; }

err_code() { jval "d['error']['code']"; }

# ── auth helpers ────────────────────────────────────────────────────────────

# login PHONE → prints session token
login() {
  local phone="$1"
  req POST /auth/otp/request "" "{\"phone\":\"$phone\"}"
  expect_status 200 "otp request for $phone"
  local otp
  otp="$(jval "d['data']['dev_otp']")"
  [[ -n "$otp" ]] || fail "no dev_otp in response for $phone (OTP_DEV_MODE off?)"
  req POST /auth/otp/verify "" "{\"phone\":\"$phone\",\"otp\":\"$otp\"}"
  expect_status 200 "otp verify for $phone"
  jval "d['data']['access_token']"
}

# role_token SESSION_TOKEN ROLE_CODE → prints role token
role_token() {
  local session="$1" role="$2"
  req GET /auth/roles "$session"
  expect_status 200 "list roles"
  local raid
  raid="$(jval "[a['id'] for a in d['data'] if a['role_code']=='$role'][0]")"
  [[ -n "$raid" ]] || fail "no $role assignment found"
  req POST /auth/role/select "$session" "{\"role_assignment_id\":\"$raid\"}"
  expect_status 200 "role select $role"
  jval "d['data']['access_token']"
}

# ── fixture constants (cmd/seed) ────────────────────────────────────────────
#
# IDs are Mongo ObjectID hex strings minted at seed time — nothing is a stable
# slug any more. The org ids, farmer party ids and animal id are DISCOVERED
# dynamically (see "id discovery" below) by their natural business keys:
# org `code`, party `phone`, animal owner. Only the natural keys are constant.

SEED_ADMIN_PHONE="9999999999"
TODAY="$(TZ=Asia/Kolkata date +%F)"
EXPIRY="$(TZ=Asia/Kolkata date -v+3d +%F 2>/dev/null || TZ=Asia/Kolkata date -d '+3 days' +%F)"

# is_hex24 STR → 0 if STR is a 24-char lowercase ObjectID hex, else 1.
is_hex24() {
  printf '%s' "$1" | python3 -c "import sys,re; sys.exit(0 if re.fullmatch(r'[0-9a-f]{24}', sys.stdin.read().strip()) else 1)"
}

# ── environment bring-up ────────────────────────────────────────────────────

log "smoke: repo=$REPO_ROOT  port=$PORT  db=$SMOKE_DB  today(IST)=$TODAY"

log "smoke: dropping $SMOKE_DB"
mongosh --quiet --eval "db.getSiblingDB(\"$SMOKE_DB\").dropDatabase()" >/dev/null

log "smoke: building server binary"
go build -o bin/saathi-server ./cmd/server

log "smoke: starting server (log: $SERVER_LOG)"
MONGO_DB="$SMOKE_DB" OTP_DEV_MODE=true PORT="$PORT" ./bin/saathi-server >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

log "smoke: waiting for /readyz"
ready=""
for _ in $(seq 1 60); do
  if curl -sf "${HOST}/readyz" >/dev/null 2>&1; then ready=1; break; fi
  kill -0 "$SERVER_PID" 2>/dev/null || { BODY="$(tail -n 20 "$SERVER_LOG")"; fail "server process died during startup"; }
  sleep 0.5
done
[[ -n "$ready" ]] || { BODY="$(tail -n 20 "$SERVER_LOG")"; fail "server did not become ready in 30s"; }

log "smoke: seeding $SMOKE_DB"
MONGO_DB="$SMOKE_DB" go run ./cmd/seed >/dev/null

# ── id discovery (ObjectID-hex contract) ────────────────────────────────────
# Everything downstream references real ObjectIDs, resolved here from the
# stable natural keys. We drive discovery as SUPER_ADMIN: org lookups by
# `?code=`, farmer party ids by `/support/parties/lookup?phone=`, the animal
# by owner within the DCS.
log "smoke: discovering seeded ObjectIDs as super admin"
DISCOVER_SESSION="$(login "$SEED_ADMIN_PHONE")"
DISCOVER_ADMIN="$(role_token "$DISCOVER_SESSION" SUPER_ADMIN)"
[[ -n "$DISCOVER_ADMIN" ]] || fail "empty super admin role token during discovery"

# org_id_by_code CODE → prints the org's ObjectID hex
org_id_by_code() {
  req GET "/orgs?code=$1" "$DISCOVER_ADMIN"
  expect_status 200 "orgs lookup code=$1"
  jassert "len(d['data'])==1 and d['data'][0]['code']=='$1'" "exactly one org for code=$1"
  jval "d['data'][0]['id']"
}
DCS="$(org_id_by_code DCS-01842)"
BMC="$(org_id_by_code BMC-LKO-007)"
PLANT="$(org_id_by_code PLANT-LKO-01)"
UNION="$(org_id_by_code UNION-LKO)"

# party_id_by_phone PHONE → prints the party's ObjectID hex (support lookup)
party_id_by_phone() {
  req GET "/support/parties/lookup?phone=$1" "$DISCOVER_ADMIN"
  expect_status 200 "party lookup phone=$1"
  jval "d['data']['party_id']"
}
FARMER_MAHESH="$(party_id_by_phone 9000000011)"
FARMER_GEETA="$(party_id_by_phone 9000000012)"

# animal owned by Mahesh within the DCS
req GET "/cattle/animals?dcs_id=$DCS" "$DISCOVER_ADMIN"
expect_status 200 "list animals in DCS"
ANIMAL="$(jval "[a['id'] for a in d['data'] if a['owner_party_id']=='$FARMER_MAHESH'][0]")"

for pair in "DCS=$DCS" "BMC=$BMC" "PLANT=$PLANT" "UNION=$UNION" \
            "FARMER_MAHESH=$FARMER_MAHESH" "FARMER_GEETA=$FARMER_GEETA" "ANIMAL=$ANIMAL"; do
  name="${pair%%=*}"; val="${pair#*=}"
  is_hex24 "$val" || fail "discovered $name is not an ObjectID hex: '$val'"
done
log "smoke: discovered DCS=$DCS BMC=$BMC PLANT=$PLANT UNION=$UNION"
log "smoke:            mahesh=$FARMER_MAHESH geeta=$FARMER_GEETA animal=$ANIMAL"

# ═══ 1. operational endpoints ═══════════════════════════════════════════════
step 1 "healthz / readyz / version"
raw_req GET "$HOST/healthz";  expect_status 200 "healthz";  jassert "d['data']['status']=='ok'" "healthz status ok"
raw_req GET "$HOST/readyz";   expect_status 200 "readyz";   jassert "d['data']['status']=='ready'" "readyz status ready"
raw_req GET "$HOST/version";  expect_status 200 "version";  jassert "d['data']['service']=='saathi-backend' and len(d['data']['version'])>0" "version body"
pass "healthz/readyz/version all OK"

# ═══ 2. sacheev login + role select ═════════════════════════════════════════
step 2 "sacheev login + role select"
SACHEEV_SESSION="$(login 9000000001)"
SACHEEV="$(role_token "$SACHEEV_SESSION" SAMITI_SACHEEV)"
[[ -n "$SACHEEV" ]] || fail "empty sacheev role token"
pass "sacheev holds a SAMITI_SACHEEV role token"

# ═══ 3. analyzer reading ════════════════════════════════════════════════════
step 3 "analyzer reading (ANALYZER_DIRECT, fat 6.5 / snf 9.0 / qty 10.5)"
NOW_UTC="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
req POST /collection/readings "$SACHEEV" "{
  \"dcs_id\": \"$DCS\",
  \"mode\": \"ANALYZER_DIRECT\",
  \"fat_pct\": 6.5, \"snf_pct\": 9.0, \"quantity_litres\": 10.5,
  \"device_id\": \"smoke-analyzer-01\",
  \"geo_lat\": 26.9124, \"geo_lng\": 80.9430,
  \"device_timestamp\": \"$NOW_UTC\"
}"
expect_status 201 "create reading"
READING_ID="$(jval "d['data']['id']")"
jassert "d['data']['plausibility_ok'] is True" "reading is plausible"
pass "reading $READING_ID recorded"

# ═══ 4. pour for Mahesh with exact pricing ══════════════════════════════════
step 4 "pour for farmer Mahesh (rate 52.75, amount 553.88)"
CEID_MAHESH="$(py_uuid)"
POUR_MAHESH_JSON="{
  \"client_event_id\": \"$CEID_MAHESH\",
  \"farmer_party_id\": \"$FARMER_MAHESH\",
  \"dcs_id\": \"$DCS\", \"shift\": \"MORNING\",
  \"quantity_litres\": 10.5, \"fat_pct\": 6.5, \"snf_pct\": 9.0,
  \"animal_id\": \"$ANIMAL\",
  \"analyzer_reading_id\": \"$READING_ID\",
  \"source\": \"ANALYZER_DIRECT\"
}"
req POST /collection/pours "$SACHEEV" "$POUR_MAHESH_JSON"
expect_status 201 "create pour"
POUR_MAHESH_ID="$(jval "d['data']['pour']['id']")"
jassert "abs(d['data']['pour']['rate_per_litre'] - 52.75) < 0.005" "rate == 52.75 (8.0 + 5.5*6.5 + 1.0*9.0)"
jassert "abs(d['data']['pour']['amount'] - 553.88) < 0.005" "amount == 553.88 (52.75 * 10.5)"
jassert "not d['data'].get('idempotent_replay', False)" "fresh insert, not replay"
pass "pour $POUR_MAHESH_ID priced rate=52.75 amount=553.88"

# ═══ 5. same pour replay → idempotent ═══════════════════════════════════════
step 5 "SAME pour replay → idempotent (offline-first)"
req POST /collection/pours "$SACHEEV" "$POUR_MAHESH_JSON"
expect_status 200 "replayed pour"
jassert "d['data']['idempotent_replay'] is True" "idempotent_replay true"
jassert "d['data']['pour']['id'] == '$POUR_MAHESH_ID'" "same pour returned"
pass "replay returned 200 + idempotent_replay=true, same pour id"

# ═══ 6. batch-sync: Geeta new + Mahesh duplicate ════════════════════════════
step 6 "batch-sync (created / created / duplicate)"
CEID_GEETA1="$(py_uuid)"
CEID_GEETA2="$(py_uuid)"
req POST /collection/pours/batch-sync "$SACHEEV" "{
  \"pours\": [
    {\"client_event_id\": \"$CEID_GEETA1\", \"farmer_party_id\": \"$FARMER_GEETA\", \"dcs_id\": \"$DCS\",
     \"shift\": \"MORNING\", \"quantity_litres\": 8.0, \"fat_pct\": 7.0, \"snf_pct\": 9.1, \"source\": \"ANALYZER_DIRECT\"},
    {\"client_event_id\": \"$CEID_GEETA2\", \"farmer_party_id\": \"$FARMER_GEETA\", \"dcs_id\": \"$DCS\",
     \"shift\": \"MORNING\", \"quantity_litres\": 4.5, \"fat_pct\": 6.8, \"snf_pct\": 8.9, \"source\": \"ANALYZER_DIRECT\"},
    $POUR_MAHESH_JSON
  ]
}"
expect_status 200 "batch sync"
jassert "[r['status'] for r in d['data']] == ['created','created','duplicate']" "statuses created/created/duplicate"
pass "batch-sync outcomes: created, created, duplicate"

# ═══ 7. negative: farmer cannot record pours ════════════════════════════════
step 7 "negative: FARMER role token POST pour → 403"
FARMER_SESSION="$(login 9000000011)"
FARMER="$(role_token "$FARMER_SESSION" FARMER)"
req POST /collection/pours "$FARMER" "{
  \"client_event_id\": \"$(py_uuid)\", \"farmer_party_id\": \"$FARMER_MAHESH\",
  \"dcs_id\": \"$DCS\", \"shift\": \"MORNING\",
  \"quantity_litres\": 5, \"fat_pct\": 6.0, \"snf_pct\": 8.8, \"source\": \"MANUAL\"
}"
expect_status 403 "farmer pour attempt"
pass "farmer POST /collection/pours refused with 403"

# ═══ 8. negative: implausible pour ══════════════════════════════════════════
step 8 "negative: implausible pour (fat 25.0) → 422"
req POST /collection/pours "$SACHEEV" "{
  \"client_event_id\": \"$(py_uuid)\", \"farmer_party_id\": \"$FARMER_MAHESH\",
  \"dcs_id\": \"$DCS\", \"shift\": \"MORNING\",
  \"quantity_litres\": 5, \"fat_pct\": 25.0, \"snf_pct\": 9.0, \"source\": \"ANALYZER_DIRECT\"
}"
expect_status 422 "implausible pour"
[[ "$(err_code)" == "IMPLAUSIBLE_VALUES" ]] || fail "expected IMPLAUSIBLE_VALUES, got $(err_code)"
pass "implausible pour rejected 422 IMPLAUSIBLE_VALUES"

# ═══ 9. invoices ════════════════════════════════════════════════════════════
step 9 "invoice generate (created>=2) + farmer sees own invoice"
req POST /collection/invoices/generate "$SACHEEV" "{\"dcs_id\": \"$DCS\", \"date\": \"$TODAY\"}"
expect_status 200 "generate invoices"
jassert "d['data']['created'] >= 2" "created >= 2 invoices"
MAHESH_INVOICE_ID="$(jval "[i['id'] for i in d['data']['invoices'] if i['farmer_party_id']=='$FARMER_MAHESH'][0]")"
req GET "/collection/invoices?date=$TODAY" "$FARMER"
expect_status 200 "farmer lists invoices"
jassert "any(i['id']=='$MAHESH_INVOICE_ID' for i in d['data'])" "farmer sees own invoice"
jassert "all(i['farmer_party_id']=='$FARMER_MAHESH' for i in d['data'])" "farmer sees ONLY own invoices"
pass "invoices generated ($MAHESH_INVOICE_ID visible to Mahesh)"

# ═══ 10. consignment create + dispatch ══════════════════════════════════════
step 10 "consignment create + dispatch (sacheev)"
req POST /logistics/consignments "$SACHEEV" "{\"dcs_id\": \"$DCS\", \"date\": \"$TODAY\", \"shift\": \"MORNING\"}"
expect_status 201 "create consignment"
CONSIGNMENT_ID="$(jval "d['data']['id']")"
jassert "d['data']['status']=='OPEN' and len(d['data']['pour_ids'])>=3" "OPEN consignment pooling >=3 pours"
req POST "/logistics/consignments/$CONSIGNMENT_ID/dispatch" "$SACHEEV"
expect_status 200 "dispatch consignment"
jassert "d['data']['status']=='DISPATCHED'" "consignment DISPATCHED"
pass "consignment $CONSIGNMENT_ID OPEN→DISPATCHED"

# ═══ 11. rider trip: pickup → cold chain → deliver ══════════════════════════
step 11 "rider trip: create → pickup 4.2°C → cold-chain → deliver to BMC"
RIDER_SESSION="$(login 9000000021)"
RIDER="$(role_token "$RIDER_SESSION" VAN_RIDER)"
req POST /logistics/trips "$RIDER" "{
  \"route_name\": \"Smoke Route A\", \"union_id\": \"$UNION\",
  \"date\": \"$TODAY\", \"shift\": \"MORNING\",
  \"stops\": [{\"dcs_id\": \"$DCS\", \"consignment_id\": \"$CONSIGNMENT_ID\"}]
}"
expect_status 201 "create trip"
TRIP_ID="$(jval "d['data']['id']")"
jassert "d['data']['status']=='PLANNED'" "trip PLANNED"
req POST "/logistics/trips/$TRIP_ID/stops/$CONSIGNMENT_ID/pickup" "$RIDER" '{"temp_c": 4.2}'
expect_status 200 "pickup"
jassert "d['data']['status']=='IN_PROGRESS'" "trip IN_PROGRESS after pickup"
jassert "abs(d['data']['stops'][0]['temp_c'] - 4.2) < 0.001" "pickup temp recorded"
req POST "/logistics/trips/$TRIP_ID/cold-chain" "$RIDER" '{"temp_c": 4.4, "geo_lat": 26.93, "geo_lng": 80.95}'
expect_status 200 "cold-chain log"
jassert "len(d['data']['cold_chain'])>=1" "cold-chain entry stored"
req POST "/logistics/trips/$TRIP_ID/deliver" "$RIDER" "{\"bmc_id\": \"$BMC\"}"
expect_status 200 "deliver"
jassert "d['data']['status']=='DELIVERED' and d['data']['delivered_to_bmc_id']=='$BMC'" "trip DELIVERED to BMC"
pass "trip $TRIP_ID PLANNED→IN_PROGRESS→DELIVERED"

# ═══ 12. BMC lot create + close ═════════════════════════════════════════════
step 12 "bmc-op: lot create from delivered consignment + close at 3.8°C"
BMCOP_SESSION="$(login 9000000031)"
BMCOP="$(role_token "$BMCOP_SESSION" BMC_OPERATOR)"
req POST /plant/bmc-lots "$BMCOP" "{
  \"bmc_id\": \"$BMC\", \"date\": \"$TODAY\", \"shift\": \"MORNING\",
  \"consignment_ids\": [\"$CONSIGNMENT_ID\"]
}"
expect_status 201 "create bmc lot"
LOT_ID="$(jval "d['data']['id']")"
jassert "d['data']['status']=='OPEN'" "lot OPEN"
req POST "/plant/bmc-lots/$LOT_ID/close" "$BMCOP" '{"chilling_temp_c": 3.8}'
expect_status 200 "close lot"
jassert "d['data']['status']=='QC_PENDING' and abs(d['data']['chilling_temp_c']-3.8)<0.001" "lot QC_PENDING at 3.8°C"
pass "bmc lot $LOT_ID OPEN→QC_PENDING"

# ═══ 13. QC pass + dispatch lot ═════════════════════════════════════════════
step 13 "bmc-op QC BMC_RAPID PASS (AFM1 0.3, coliform 5) → PASSED → dispatch"
req POST /quality/qc-results "$BMCOP" "{
  \"subject_type\": \"BMC_LOT\", \"subject_id\": \"$LOT_ID\", \"stage\": \"BMC_RAPID\",
  \"tests\": [
    {\"name\": \"AFLATOXIN_M1\", \"value\": 0.3, \"unit\": \"µg/kg\"},
    {\"name\": \"COLIFORM\", \"value\": 5, \"unit\": \"CFU/ml\"}
  ]
}"
expect_status 201 "record BMC_RAPID QC"
jassert "d['data']['overall_pass'] is True and len(d['data']['certificate_number'])>0" "QC passed with certificate"
req GET "/plant/bmc-lots?bmc_id=$BMC&date=$TODAY" "$BMCOP"
expect_status 200 "list lots"
jassert "[l['status'] for l in d['data'] if l['id']=='$LOT_ID'] == ['PASSED']" "lot status PASSED"
req POST "/plant/bmc-lots/$LOT_ID/dispatch" "$BMCOP"
expect_status 200 "dispatch lot"
jassert "d['data']['status']=='DISPATCHED'" "lot DISPATCHED"
pass "lot $LOT_ID QC PASSED → DISPATCHED"

# ═══ 14. negative gate: full second path → BLOCKED lot ══════════════════════
step 14 "negative gate: 2nd path (evening) → QC fail AFM1 0.9 → BLOCKED → dispatch 422 → batch 422"
# second pour (evening shift keeps the consignment unique-index happy)
CEID_EVE="$(py_uuid)"
req POST /collection/pours "$SACHEEV" "{
  \"client_event_id\": \"$CEID_EVE\", \"farmer_party_id\": \"$FARMER_MAHESH\",
  \"dcs_id\": \"$DCS\", \"shift\": \"EVENING\",
  \"quantity_litres\": 9.0, \"fat_pct\": 6.2, \"snf_pct\": 8.9, \"source\": \"ANALYZER_DIRECT\"
}"
expect_status 201 "evening pour"
req POST /logistics/consignments "$SACHEEV" "{\"dcs_id\": \"$DCS\", \"date\": \"$TODAY\", \"shift\": \"EVENING\"}"
expect_status 201 "evening consignment"
CONSIGNMENT2_ID="$(jval "d['data']['id']")"
req POST "/logistics/consignments/$CONSIGNMENT2_ID/dispatch" "$SACHEEV"
expect_status 200 "dispatch evening consignment"
req POST /logistics/trips "$RIDER" "{
  \"route_name\": \"Smoke Route B\", \"union_id\": \"$UNION\",
  \"date\": \"$TODAY\", \"shift\": \"EVENING\",
  \"stops\": [{\"dcs_id\": \"$DCS\", \"consignment_id\": \"$CONSIGNMENT2_ID\"}]
}"
expect_status 201 "evening trip"
TRIP2_ID="$(jval "d['data']['id']")"
req POST "/logistics/trips/$TRIP2_ID/stops/$CONSIGNMENT2_ID/pickup" "$RIDER" '{"temp_c": 4.0}'
expect_status 200 "evening pickup"
req POST "/logistics/trips/$TRIP2_ID/deliver" "$RIDER" "{\"bmc_id\": \"$BMC\"}"
expect_status 200 "evening deliver"
req POST /plant/bmc-lots "$BMCOP" "{
  \"bmc_id\": \"$BMC\", \"date\": \"$TODAY\", \"shift\": \"EVENING\",
  \"consignment_ids\": [\"$CONSIGNMENT2_ID\"]
}"
expect_status 201 "evening bmc lot"
LOT2_ID="$(jval "d['data']['id']")"
req POST "/plant/bmc-lots/$LOT2_ID/close" "$BMCOP" '{"chilling_temp_c": 3.9}'
expect_status 200 "close evening lot"
req POST /quality/qc-results "$BMCOP" "{
  \"subject_type\": \"BMC_LOT\", \"subject_id\": \"$LOT2_ID\", \"stage\": \"BMC_RAPID\",
  \"tests\": [{\"name\": \"AFLATOXIN_M1\", \"value\": 0.9, \"unit\": \"µg/kg\"}]
}"
expect_status 201 "record failing QC"
jassert "d['data']['overall_pass'] is False and len(d['data']['failure_reasons'])>=1" "QC failed with reasons"
req GET "/plant/bmc-lots?bmc_id=$BMC&date=$TODAY" "$BMCOP"
expect_status 200 "list lots after block"
jassert "[l['status'] for l in d['data'] if l['id']=='$LOT2_ID'] == ['BLOCKED']" "lot2 BLOCKED"
# dispatch of blocked lot must refuse
req POST "/plant/bmc-lots/$LOT2_ID/dispatch" "$BMCOP"
expect_status 422 "dispatch of BLOCKED lot"
[[ "$(err_code)" == "SAFETY_GATE_BLOCKED" ]] || fail "expected SAFETY_GATE_BLOCKED on dispatch, got $(err_code)"
# batching the blocked lot must refuse too
PLANTOP_SESSION="$(login 9000000041)"
PLANTOP="$(role_token "$PLANTOP_SESSION" PLANT_OPERATOR)"
req POST /plant/batches "$PLANTOP" "{
  \"plant_id\": \"$PLANT\", \"bmc_lot_ids\": [\"$LOT2_ID\"], \"product_type\": \"TONED_MILK\"
}"
expect_status 422 "batch with BLOCKED lot"
[[ "$(err_code)" == "SAFETY_GATE_BLOCKED" ]] || fail "expected SAFETY_GATE_BLOCKED on batch, got $(err_code)"
pass "blocked lot $LOT2_ID can neither dispatch nor enter a batch (both 422 SAFETY_GATE_BLOCKED)"

# ═══ 15. plant batch: create → lab QC pass → complete ═══════════════════════
step 15 "plant batch from PASSED+DISPATCHED lot → PLANT_LAB pass → COMPLETED"
req POST /plant/batches "$PLANTOP" "{
  \"plant_id\": \"$PLANT\", \"bmc_lot_ids\": [\"$LOT_ID\"], \"product_type\": \"TONED_MILK\"
}"
expect_status 201 "create batch"
BATCH_ID="$(jval "d['data']['id']")"
BATCH_NUMBER="$(jval "d['data']['batch_number']")"
jassert "d['data']['status']=='QC_PENDING'" "batch QC_PENDING"
LAB_SESSION="$(login 9000000042)"
LAB="$(role_token "$LAB_SESSION" PLANT_LAB_ANALYST)"
req POST /quality/qc-results "$LAB" "{
  \"subject_type\": \"PROCESSING_BATCH\", \"subject_id\": \"$BATCH_ID\", \"stage\": \"PLANT_LAB\",
  \"tests\": [
    {\"name\": \"AFLATOXIN_M1\", \"value\": 0.2, \"unit\": \"µg/kg\"},
    {\"name\": \"COLIFORM\", \"value\": 3, \"unit\": \"CFU/ml\"},
    {\"name\": \"PHOSPHATASE\", \"value\": 0, \"unit\": \"0=negative\"}
  ]
}"
expect_status 201 "record PLANT_LAB QC"
jassert "d['data']['overall_pass'] is True" "plant lab QC passed"
req GET "/plant/batches/$BATCH_ID" "$PLANTOP"
expect_status 200 "get batch"
jassert "d['data']['status']=='PASSED'" "batch PASSED"
req POST "/plant/batches/$BATCH_ID/complete" "$PLANTOP"
expect_status 200 "complete batch"
jassert "d['data']['status']=='COMPLETED'" "batch COMPLETED"
pass "batch $BATCH_NUMBER QC_PENDING→PASSED→COMPLETED"

# ═══ 16. product lot + QR ═══════════════════════════════════════════════════
step 16 "product lot (PARAG-TM-500, expiry +3d) → QR issue"
req POST /plant/product-lots "$PLANTOP" "{
  \"batch_id\": \"$BATCH_ID\", \"sku\": \"PARAG-TM-500\",
  \"product_name\": \"Parag Toned Milk\", \"units\": 2000, \"unit_size\": \"500ml\",
  \"mrp\": 27.0, \"expiry_date\": \"$EXPIRY\"
}"
expect_status 201 "create product lot"
PRODUCT_LOT_ID="$(jval "d['data']['id']")"
jassert "d['data']['status']=='ACTIVE'" "product lot ACTIVE"
req POST /plant/qrs "$PLANTOP" "{\"product_lot_id\": \"$PRODUCT_LOT_ID\"}"
expect_status 201 "issue QR"
QR_CODE="$(jval "d['data']['qr_code']")"
[[ -n "$QR_CODE" ]] || fail "no qr_code in issue response"
pass "product lot $PRODUCT_LOT_ID labelled with QR $QR_CODE"

# ═══ 17. PUBLIC scan without any token ══════════════════════════════════════
step 17 "public QR scan (no token): samiti set + certificate + intact ledger"
STATUS=""; BODY=""
out="$(curl -sS "${BASE}/public/qr/${QR_CODE}" -w $'\n%{http_code}')" || fail "curl error on public scan"
STATUS="${out##*$'\n'}"; BODY="${out%$'\n'*}"
expect_status 200 "public scan"
jassert "'DCS-01842' in [s['code'] for s in d['data']['sourcing']['samitis']]" "sourcing.samitis contains DCS-01842"
jassert "len(d['data']['quality']['certificate_number'])>0" "quality.certificate_number present"
jassert "d['data']['ledger']['intact'] is True" "ledger.intact true"
jassert "d['data']['product']['sku']=='PARAG-TM-500'" "product SKU on scan"
pass "public scan shows honest provenance + intact chain"

# ═══ 18. public ledger verify ═══════════════════════════════════════════════
step 18 "public ledger verify from=1 → intact"
req GET "/public/ledger/verify?from=1&to=100000" ""
expect_status 200 "ledger verify"
jassert "d['data']['intact'] is True and d['data']['from']==1 and d['data']['to']>=10" "full chain intact"
pass "hash chain verified intact from=1 to=$(jval "d['data']['to']")"

# ═══ 19. settlement: initiate → dual control → execute ══════════════════════
step 19 "settlement: initiate → (self-approve 403) → adhyaksh approve → execute → farmer paid"
req POST /settlements "$SACHEEV" "{\"dcs_id\": \"$DCS\", \"date\": \"$TODAY\"}"
expect_status 201 "initiate settlement"
SETTLEMENT_ID="$(jval "d['data']['id']")"
jassert "d['data']['status']=='PENDING_APPROVAL' and len(d['data']['invoice_ids'])>=2" "batch PENDING_APPROVAL with >=2 invoices"
# dual control: initiator (sacheev) must NOT be able to approve own batch
req POST "/settlements/$SETTLEMENT_ID/approve" "$SACHEEV"
expect_status 403 "sacheev approving own batch"
ADHYAKSH_SESSION="$(login 9000000002)"
ADHYAKSH="$(role_token "$ADHYAKSH_SESSION" SAMITI_ADHYAKSH)"
req POST "/settlements/$SETTLEMENT_ID/approve" "$ADHYAKSH"
expect_status 200 "adhyaksh approve"
jassert "d['data']['status']=='APPROVED'" "batch APPROVED"
req POST "/settlements/$SETTLEMENT_ID/execute" "$ADHYAKSH"
expect_status 200 "execute settlement"
jassert "d['data']['batch']['status']=='EXECUTED' and len(d['data']['payout_instructions'])>=2" "EXECUTED with payouts"
jassert "all(p['status']=='SUCCESS' and p['utr'] for p in d['data']['payout_instructions'])" "all payouts SUCCESS with UTR"
# farmer sees the payout + PAID invoice
req GET /settlements/payouts "$FARMER"
expect_status 200 "farmer payout history"
jassert "any(p['farmer_party_id']=='$FARMER_MAHESH' and p['status']=='SUCCESS' for p in d['data'])" "farmer payout visible"
req GET "/collection/invoices/$MAHESH_INVOICE_ID" "$FARMER"
expect_status 200 "farmer fetches invoice"
jassert "d['data']['status']=='PAID'" "invoice PAID"
pass "settlement $SETTLEMENT_ID dual-controlled and executed; Mahesh paid, invoice PAID"

# ═══ 20. platformops: worker, audit, flags, dormant telemetry ═══════════════
step 20 "platformops: SMS worker, audit logs, flags, dormant collar telemetry"
ADMIN_SESSION="$(login 9999999999)"
ADMIN="$(role_token "$ADMIN_SESSION" SUPER_ADMIN)"
req POST /notifications/worker/run "$ADMIN"
expect_status 200 "run notifications worker"
jassert "d['data']['sent'] > 0" "worker sent > 0 SMS"
WORKER_SENT="$(jval "d['data']['sent']")"
req GET "/audit/logs?limit=5" "$ADMIN"
expect_status 200 "list audit logs"
jassert "d['meta']['total'] > 0 and len(d['data']) > 0" "audit entries exist"
req GET /admin/flags "$ADMIN"
expect_status 200 "list flags"
jassert "[f['enabled'] for f in d['data'] if f['key']=='collar_telemetry_enabled'] == [False]" "collar_telemetry_enabled is false"
req POST /cattle/telemetry "$ADMIN" '{"pashu_aadhaar": "356729481027", "metrics": {"temp_c": 38.6, "steps": 4211}}'
expect_status 403 "telemetry while flag off"
[[ "$(err_code)" == "FEATURE_DISABLED" ]] || fail "expected FEATURE_DISABLED, got $(err_code)"
pass "worker sent=$WORKER_SENT; audit trail live; collar flag off; telemetry 403 FEATURE_DISABLED"

# ═══ 21. KYC approval gate (the new onboarding flow) ════════════════════════
step 21 "kyc gate: fresh party submits KYC → granted FARMER but role select 403 → org-manager approves → role select succeeds"

# fresh phone: verify auto-creates a MINIMAL-tier party.
NEWPHONE="9000000099"
NEW_SESSION="$(login "$NEWPHONE")"
req GET /parties/me "$NEW_SESSION"
expect_status 200 "new party /parties/me"
NEW_PARTY_ID="$(jval "d['data']['party']['id']")"
is_hex24 "$NEW_PARTY_ID" || fail "new party id is not an ObjectID hex: '$NEW_PARTY_ID'"
jassert "d['data']['party']['kyc_tier']=='MINIMAL'" "fresh party auto-created at MINIMAL tier"

# submit Aadhaar KYC at the FARMER tier → PENDING (no auto-verify).
req POST /kyc/aadhaar "$NEW_SESSION" '{"aadhaar_number":"123412341234","consent":true,"requested_tier":"FARMER"}'
expect_status 201 "submit aadhaar KYC"
KYC_ID="$(jval "d['data']['record']['id']")"
is_hex24 "$KYC_ID" || fail "kyc record id is not an ObjectID hex: '$KYC_ID'"
jassert "d['data']['status']=='PENDING'" "KYC status PENDING"
jassert "d['data']['record']['requested_tier']=='FARMER'" "KYC requested_tier FARMER"

# adhyaksh grants the FARMER role at the DCS to this fresh party.
KYC_ADHYAKSH_SESSION="$(login 9000000002)"
KYC_ADHYAKSH="$(role_token "$KYC_ADHYAKSH_SESSION" SAMITI_ADHYAKSH)"
req POST /roles/assignments "$KYC_ADHYAKSH" "{\"party_id\": \"$NEW_PARTY_ID\", \"role_code\": \"FARMER\", \"org_unit_id\": \"$DCS\"}"
expect_status 201 "adhyaksh grants FARMER at DCS"
NEW_FARMER_RA_ID="$(jval "d['data']['id']")"
is_hex24 "$NEW_FARMER_RA_ID" || fail "role assignment id is not an ObjectID hex: '$NEW_FARMER_RA_ID'"

# role select MUST be refused: KYC still PENDING, party only MINIMAL tier.
req POST /auth/role/select "$NEW_SESSION" "{\"role_assignment_id\": \"$NEW_FARMER_RA_ID\"}"
expect_status 403 "role select before KYC approval"
[[ "$(err_code)" == "KYC_TIER_INSUFFICIENT" ]] || fail "expected KYC_TIER_INSUFFICIENT, got $(err_code)"

# organising manager sees it in the review queue and approves it.
OM_SESSION="$(login 9000000071)"
OM="$(role_token "$OM_SESSION" ORGANISING_MANAGER)"
req GET /kyc/pending "$OM"
expect_status 200 "org-manager lists pending KYC"
jassert "any(r['id']=='$KYC_ID' and r.get('party',{}).get('id')=='$NEW_PARTY_ID' for r in d['data'])" "pending queue shows our record"
req POST "/kyc/$KYC_ID/approve" "$OM"
expect_status 200 "org-manager approves KYC"
jassert "d['data']['kyc_tier']=='FARMER'" "party upgraded to FARMER tier on approval"
jassert "d['data']['record']['status']=='VERIFIED'" "KYC record now VERIFIED"

# with the tier unlocked, the SAME role select now succeeds.
req POST /auth/role/select "$NEW_SESSION" "{\"role_assignment_id\": \"$NEW_FARMER_RA_ID\"}"
expect_status 200 "role select after KYC approval"
jassert "d['data']['role_code']=='FARMER'" "FARMER role token issued"
NEW_FARMER_TOKEN="$(jval "d['data']['access_token']")"
[[ -n "$NEW_FARMER_TOKEN" ]] || fail "empty FARMER role token after approval"

# ObjectID-hex contract: dump a couple of response bodies to /tmp and confirm
# the ids they carry are 24-hex ObjectIDs (one org id + one pour id).
ORG_DUMP="/tmp/saathi-smoke-org.json"
POUR_DUMP="/tmp/saathi-smoke-pour.json"
req GET "/orgs?code=DCS-01842" "$DISCOVER_ADMIN"; printf '%s' "$BODY" >"$ORG_DUMP"
req GET "/collection/pours?dcs_id=$DCS&farmer_party_id=$FARMER_MAHESH" "$SACHEEV"
expect_status 200 "list Mahesh pours for hex dump"
printf '%s' "$BODY" >"$POUR_DUMP"
python3 -c "
import json,re
org=json.load(open('$ORG_DUMP'))
oid=org['data'][0]['id']
assert re.fullmatch(r'[0-9a-f]{24}', oid), 'org id not hex24: '+repr(oid)
pours=json.load(open('$POUR_DUMP'))['data']
pid=pours[0]['id']
assert re.fullmatch(r'[0-9a-f]{24}', pid), 'pour id not hex24: '+repr(pid)
assert any(p['id']=='$POUR_MAHESH_ID' for p in pours), 'seeded pour id missing from dump'
" >/dev/null 2>&1 || fail "ObjectID-hex regex check failed (org=$ORG_DUMP pours=$POUR_DUMP)"
is_hex24 "$POUR_MAHESH_ID" || fail "pour id not ObjectID hex: '$POUR_MAHESH_ID'"

pass "KYC gate holds: PENDING→403 KYC_TIER_INSUFFICIENT→approve→role select OK; ids are ObjectID hex"

# ── step 22: concurrency proof — two simultaneous approvals, only ONE completes ──
# This is the "duplicacy" guarantee: MongoDB's atomic status-guarded update
# (filter includes status:PENDING) lets exactly one writer win; the loser gets
# 409. No Redis lock — the DB itself is the single source of truth.
step 22 "concurrency: two simultaneous KYC approvals on the SAME record → exactly one 200, one 409"
DUP_PHONE="9000000096"
DUP_SESSION="$(login "$DUP_PHONE")"
req POST /kyc/aadhaar "$DUP_SESSION" '{"aadhaar_number":"123412341296","consent":true,"requested_tier":"FARMER"}'
expect_status 201 "fresh party submits KYC for concurrency test"
DUP_KYC_ID="$(jval "d['data']['record']['id']")"
is_hex24 "$DUP_KYC_ID" || fail "no KYC id for concurrency test: '$DUP_KYC_ID'"

# Fire two approvals in parallel (SUPER_ADMIN sees all scopes). Capture each
# HTTP status to its own file so the shared req/BODY globals never collide.
C1="/tmp/saathi-dup-1.code"; C2="/tmp/saathi-dup-2.code"
curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -X POST "${BASE}/kyc/${DUP_KYC_ID}/approve" \
  -H "Authorization: Bearer $DISCOVER_ADMIN" >"$C1" &
P1=$!
curl -sS --max-time 10 -o /dev/null -w '%{http_code}' -X POST "${BASE}/kyc/${DUP_KYC_ID}/approve" \
  -H "Authorization: Bearer $DISCOVER_ADMIN" >"$C2" &
P2=$!
# Wait ONLY on the two curls (bare `wait` would also block on the background
# server process started earlier in this script and never return).
wait "$P1" "$P2"
CODE1="$(cat "$C1")"; CODE2="$(cat "$C2")"
log "      parallel approval results: $CODE1 and $CODE2"
python3 -c "
codes = sorted(['$CODE1', '$CODE2'])
assert codes == ['200','409'], 'expected exactly one 200 and one 409, got '+repr(codes)
" || fail "concurrency guard failed — got $CODE1 and $CODE2 (expected one 200, one 409)"
# And the record is VERIFIED exactly once, not double-processed.
req GET /kyc/pending "$DISCOVER_ADMIN"
jassert "not any(r['id']=='$DUP_KYC_ID' for r in d['data'])" "approved record left the pending queue"
pass "duplicacy prevented atomically: two concurrent approvals → exactly one completed ($CODE1/$CODE2)"

# ── step 23: live badge — pending count endpoint + SSE nudge on new submission ──
step 23 "live badge: GET /kyc/pending/count + SSE /events/stream nudge on submit"
req GET /kyc/pending/count "$DISCOVER_ADMIN"
expect_status 200 "pending count endpoint"
jassert "isinstance(d['data']['count'], int) and d['data']['count']>=0" "count is a non-negative integer"
BADGE_BEFORE="$(jval "d['data']['count']")"

# Open an SSE stream as an admin dashboard, then trigger a submission and
# assert the live nudge arrives without any refresh.
SSE_OUT="/tmp/saathi-sse.txt"; : >"$SSE_OUT"
curl -N -sS --max-time 8 -H "Authorization: Bearer $DISCOVER_ADMIN" "${BASE}/events/stream" >"$SSE_OUT" 2>/dev/null &
SSE_PID=$!
sleep 1
SSE_PHONE="9000000097"
SSE_SESSION="$(login "$SSE_PHONE")"
req POST /kyc/aadhaar "$SSE_SESSION" '{"aadhaar_number":"123412341297","consent":true,"requested_tier":"FARMER"}'
expect_status 201 "submit KYC to fire the SSE nudge"
sleep 2
kill "$SSE_PID" 2>/dev/null || true
wait "$SSE_PID" 2>/dev/null || true
LC_ALL=C grep -q 'event: ready' "$SSE_OUT"        || fail "SSE stream did not open with a ready event (see $SSE_OUT)"
LC_ALL=C grep -q 'kyc.pending.changed' "$SSE_OUT" || fail "SSE did not deliver the kyc.pending.changed nudge (see $SSE_OUT)"
req GET /kyc/pending/count "$DISCOVER_ADMIN"
expect_status 200 "pending count endpoint (after submit)"
BADGE_AFTER="$(jval "d['data']['count']")"
# Integer compare in bash (avoids fragile python-string interpolation of vars).
[[ "$BADGE_BEFORE" =~ ^[0-9]+$ && "$BADGE_AFTER" =~ ^[0-9]+$ ]] \
  || fail "badge values not integers: before='$BADGE_BEFORE' after='$BADGE_AFTER'"
(( BADGE_AFTER >= BADGE_BEFORE + 1 )) \
  || fail "pending count did not rise after submission (before=$BADGE_BEFORE after=$BADGE_AFTER)"
pass "live badge works: count endpoint + SSE nudge on submit (badge ${BADGE_BEFORE} to ${BADGE_AFTER})"

log ""
log "SMOKE PASSED — all 23 steps green (db=$SMOKE_DB, port=$PORT)"
