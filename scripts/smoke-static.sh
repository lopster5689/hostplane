#!/bin/bash
# Pre-deploy smoke test: static-site lifecycle (multipart provision -> reachable -> destroy).
# Site naming: smoke<unix_ts>. Cleans up on exit. Run on control-01.

set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; FAILED=1; }
warn() { echo -e "${YELLOW}!${NC} $1"; }
step() { echo ""; echo -e "${YELLOW}── Step $1 ──${NC} $2"; }

FAILED=0
SITE=""
TMP_DIR=""

API_URL="${API_URL:-http://localhost:8080}"
BASE_DOMAIN="${BASE_DOMAIN:-cowsaidmoo.tech}"
POLL_TIMEOUT="${POLL_TIMEOUT:-90}"
HTTP_RETRY_TIMEOUT="${HTTP_RETRY_TIMEOUT:-60}"

if [ -z "${API_KEY:-}" ]; then
  if [ -r /opt/control/.env ]; then
    API_KEY=$(grep '^API_KEY' /opt/control/.env | cut -d= -f2-)
  else
    echo "ERROR: cannot read /opt/control/.env and \$API_KEY not set" >&2; exit 2
  fi
fi

api() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" -H "X-API-Key: $API_KEY")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  curl "${args[@]}" "${API_URL}${path}"
}
api_code() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -o /dev/null -w "%{http_code}" -X "$method" -H "X-API-Key: $API_KEY")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  curl "${args[@]}" "${API_URL}${path}"
}
poll_job() {
  local job_id="$1" timeout="$2" elapsed=0 status
  while [ "$elapsed" -lt "$timeout" ]; do
    status=$(api GET "/api/jobs/$job_id" | jq -r '.status // "?"')
    case "$status" in COMPLETED) return 0;; FAILED) return 1;; esac
    sleep 3; elapsed=$((elapsed + 3))
  done
  return 2
}

cleanup() {
  local exit_code=$?
  [ -n "$TMP_DIR" ] && rm -rf "$TMP_DIR"
  [ -z "$SITE" ] && exit "$exit_code"
  echo ""; echo "── Cleanup ──"
  local resp http_status body job
  resp=$(curl -sS -w "\n%{http_code}" -X POST \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
    -d "{\"site\":\"$SITE\"}" \
    "${API_URL}/api/destroy" 2>/dev/null || true)
  http_status=$(printf '%s' "$resp" | tail -n1)
  body=$(printf '%s' "$resp" | sed '$d')
  case "$http_status" in
    202)
      job=$(printf '%s' "$body" | jq -r '.job_id // empty')
      [ -n "$job" ] && poll_job "$job" 60 >/dev/null 2>&1 || true
      api_code DELETE "/api/sites/$SITE" >/dev/null 2>&1 || true
      echo "  destroyed $SITE"
      ;;
    404) echo "  no site record for $SITE";;
    409)
      api_code DELETE "/api/sites/$SITE" >/dev/null 2>&1 || true
      echo "  $SITE was already DESTROYING/DESTROYED"
      ;;
    *) warn "destroy returned $http_status — sweeper will reap";;
  esac
  exit "$exit_code"
}
trap cleanup EXIT

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTPLANE — STATIC-SITE SMOKE TEST"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

command -v jq >/dev/null 2>&1 || { echo "jq required" >&2; exit 2; }
command -v zip >/dev/null 2>&1 || { echo "zip required" >&2; exit 2; }

if [ "$(api_code GET /api/health)" != "200" ]; then
  echo "ERROR: control plane unreachable at $API_URL" >&2; exit 2
fi

SITE="smoke$(date +%s)"
TMP_DIR=$(mktemp -d)
echo "Site name: $SITE"
echo "Tmp dir:   $TMP_DIR"

# Build a tiny static zip with a marker string we can grep for
cat > "$TMP_DIR/index.html" <<HTML
<!DOCTYPE html><html><body><h1>SMOKE_STATIC_MARKER</h1></body></html>
HTML
(cd "$TMP_DIR" && zip -q site.zip index.html)

# ── 1. Multipart provision ────────────────────────────────────
step 1 "POST /api/static/provision (multipart)"
RESP=$(curl -sS -X POST "$API_URL/api/static/provision" \
  -H "X-API-Key: $API_KEY" \
  -F "site=$SITE" \
  -F "zip=@$TMP_DIR/site.zip")
JOB=$(echo "$RESP" | jq -r '.job_id // empty')
if [ -n "$JOB" ]; then pass "static provision queued (job $JOB)"; else fail "response: $RESP"; exit 1; fi

# ── 2. Poll ───────────────────────────────────────────────────
step 2 "Poll provision job"
poll_job "$JOB" "$POLL_TIMEOUT"
case $? in
  0) pass "completed";;
  1) fail "FAILED"; exit 1;;
  2) fail "timeout"; exit 1;;
esac

# ── 3. Site status ────────────────────────────────────────────
step 3 "GET /api/sites/$SITE"
SR=$(api GET "/api/sites/$SITE")
SS=$(echo "$SR" | jq -r '.status'); CS=$(echo "$SR" | jq -r '.cert_status'); WC=$(echo "$SR" | jq -r '.warnings|length')
[ "$SS" = "ACTIVE" ] && pass "status=ACTIVE" || fail "status=$SS"
[ "$CS" = "issued" ] && pass "cert_status=issued" || fail "cert_status=$CS"
[ "$WC" = "0" ] && pass "no warnings" || fail "$WC warnings"

# ── 4. Public reachability ────────────────────────────────────
step 4 "curl https://${SITE}.${BASE_DOMAIN}/"
URL="https://${SITE}.${BASE_DOMAIN}/"
elapsed=0; ok=0
while [ "$elapsed" -lt "$HTTP_RETRY_TIMEOUT" ]; do
  body=$(curl -sS --max-time 10 "$URL" 2>/dev/null || true)
  if echo "$body" | grep -q "SMOKE_STATIC_MARKER"; then ok=1; break; fi
  sleep 4; elapsed=$((elapsed + 4))
done
[ "$ok" = "1" ] && pass "marker visible" || fail "marker not visible within ${HTTP_RETRY_TIMEOUT}s"

# ── 5. Destroy ────────────────────────────────────────────────
# NOTE: ARCHITECTURE.md §25 lists static-site destroy as outstanding. If the destroy
# returns 4xx because static destroy is not yet implemented, we mark a WARN and still
# run cleanup — this script does not regress when static-destroy ships.
step 5 "POST /api/destroy"
DR=$(api POST /api/destroy "{\"site\":\"$SITE\"}")
DJ=$(echo "$DR" | jq -r '.job_id // empty')
if [ -n "$DJ" ]; then
  pass "destroy queued (job $DJ)"
  poll_job "$DJ" "$POLL_TIMEOUT"
  case $? in
    0) pass "destroy completed"
       api_code DELETE "/api/sites/$SITE" >/dev/null 2>&1 || true
       GC=$(api_code GET "/api/sites/$SITE")
       [ "$GC" = "404" ] && pass "site returns 404" || fail "GET returns $GC"
       SITE=""  # disarm trap
       ;;
    *) fail "destroy did not complete";;
  esac
else
  warn "static destroy not implemented yet — sweeper will reap (ARCHITECTURE.md §25)"
fi

echo ""
if [ "$FAILED" = "0" ]; then echo -e "${GREEN}SMOKE PASS${NC}"; exit 0
else echo -e "${RED}SMOKE FAIL${NC}"; exit 1; fi
