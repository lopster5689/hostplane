#!/bin/bash
# Pre-deploy smoke test: WordPress lifecycle (provision -> domain -> backup -> destroy).
# Run on control-01 against the current control-plane.
# Fails non-zero on any check failure. Always cleans up the smoke site on exit.
#
# Site naming: smoke<unix_ts> (regex ^[a-z0-9]+$ — no hyphens allowed).
# A separate sweeper (smoke-sweeper.sh) reaps any smoke* site older than 1h
# so a crashed smoke run never leaves a permanent ghost.

set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; FAILED=1; }
warn() { echo -e "${YELLOW}!${NC} $1"; }
step() { echo ""; echo -e "${YELLOW}── Step $1 ──${NC} $2"; }

FAILED=0
SITE=""
PROVISION_JOB=""
DESTROY_JOB=""
BACKUP_DATE=""

API_URL="${API_URL:-http://localhost:8080}"
BASE_DOMAIN="${BASE_DOMAIN:-cowsaidmoo.tech}"
POLL_TIMEOUT="${POLL_TIMEOUT:-120}"     # provision/destroy job timeout (s)
HTTP_RETRY_TIMEOUT="${HTTP_RETRY_TIMEOUT:-60}"  # public curl retry budget (s)

# Load API key from /opt/control/.env (same pattern as health.sh)
if [ -z "${API_KEY:-}" ]; then
  if [ -r /opt/control/.env ]; then
    API_KEY=$(grep '^API_KEY' /opt/control/.env | cut -d= -f2-)
  else
    echo "ERROR: cannot read /opt/control/.env and \$API_KEY not set" >&2
    exit 2
  fi
fi

MYSQL="${MYSQL:-mysql --defaults-file=/root/.my-hosto.cnf -se}"
DOCKER="${DOCKER:-docker --host tcp://***REMOVED***:2376 --tlsverify \
  --tlscacert /opt/control/certs/ca.pem \
  --tlscert /opt/control/certs/cert.pem \
  --tlskey /opt/control/certs/key.pem}"

# ── API helpers ───────────────────────────────────────────────
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
    case "$status" in
      COMPLETED) return 0 ;;
      FAILED)    return 1 ;;
    esac
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 2
}

# ── Cleanup trap ──────────────────────────────────────────────
cleanup() {
  local exit_code=$?
  [ -z "$SITE" ] && exit "$exit_code"
  echo ""
  echo "── Cleanup ──"
  # Best-effort destroy + record delete in one shot. We capture body and HTTP status
  # from the SAME request so we don't accidentally call /api/destroy twice and lose
  # the job_id on the second call's 409.
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
    404)
      echo "  no site record for $SITE (already gone)"
      ;;
    409)
      api_code DELETE "/api/sites/$SITE" >/dev/null 2>&1 || true
      echo "  $SITE was already DESTROYING/DESTROYED"
      ;;
    *)
      warn "destroy returned $http_status for $SITE — sweeper will reap"
      ;;
  esac
  exit "$exit_code"
}
trap cleanup EXIT

# ── Preflight ─────────────────────────────────────────────────
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTPLANE — WORDPRESS SMOKE TEST"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

command -v jq >/dev/null 2>&1 || { echo "jq required" >&2; exit 2; }
command -v curl >/dev/null 2>&1 || { echo "curl required" >&2; exit 2; }

if [ "$(api_code GET /api/health)" != "200" ]; then
  echo "ERROR: control plane is not reachable at $API_URL — refusing to smoke" >&2
  exit 2
fi

SITE="smoke$(date +%s)"
echo "Site name: $SITE"

# ── 1. Provision ──────────────────────────────────────────────
step 1 "POST /api/provision"
RESP=$(api POST /api/provision "{\"site\":\"$SITE\"}")
PROVISION_JOB=$(echo "$RESP" | jq -r '.job_id // empty')
STATUS=$(echo "$RESP" | jq -r '.status // empty')
if [ -n "$PROVISION_JOB" ] && [ "$STATUS" = "PENDING" ]; then
  pass "provision queued (job $PROVISION_JOB)"
else
  fail "unexpected provision response: $RESP"
  exit 1
fi

# ── 2. Poll job ───────────────────────────────────────────────
step 2 "Poll /api/jobs/$PROVISION_JOB"
poll_job "$PROVISION_JOB" "$POLL_TIMEOUT"
case $? in
  0) pass "provision completed within ${POLL_TIMEOUT}s" ;;
  1) fail "provision job FAILED — see control-plane logs"; exit 1 ;;
  2) fail "provision did not finish in ${POLL_TIMEOUT}s"; exit 1 ;;
esac

# ── 3. Site status ────────────────────────────────────────────
step 3 "GET /api/sites/$SITE"
SITE_RESP=$(api GET "/api/sites/$SITE")
SITE_STATUS=$(echo "$SITE_RESP" | jq -r '.status // empty')
CERT_STATUS=$(echo "$SITE_RESP" | jq -r '.cert_status // empty')
WARN_COUNT=$(echo "$SITE_RESP" | jq -r '.warnings | length')
if [ "$SITE_STATUS" = "ACTIVE" ]; then pass "status=ACTIVE"; else fail "status=$SITE_STATUS (want ACTIVE)"; fi
if [ "$CERT_STATUS" = "issued" ]; then pass "cert_status=issued"; else fail "cert_status=$CERT_STATUS (want issued)"; fi
if [ "$WARN_COUNT" = "0" ]; then pass "no warnings"; else fail "$WARN_COUNT warnings: $(echo "$SITE_RESP" | jq -c .warnings)"; fi

# ── 4. Public HTTPS reachability ──────────────────────────────
step 4 "curl https://${SITE}.${BASE_DOMAIN}/"
SITE_URL="https://${SITE}.${BASE_DOMAIN}/"
elapsed=0
ok=0
while [ "$elapsed" -lt "$HTTP_RETRY_TIMEOUT" ]; do
  body=$(curl -sS --max-time 10 "$SITE_URL" 2>/dev/null || true)
  if echo "$body" | grep -qi "wordpress"; then
    ok=1; break
  fi
  sleep 4
  elapsed=$((elapsed + 4))
done
if [ "$ok" = "1" ]; then
  pass "public URL serves WordPress"
else
  fail "public URL did not return WordPress within ${HTTP_RETRY_TIMEOUT}s"
fi

# ── 5. WP-CLI exec ────────────────────────────────────────────
step 5 "POST /api/sites/$SITE/wp-exec (list_plugins)"
WPEXEC=$(api POST "/api/sites/$SITE/wp-exec" '{"tool":"list_plugins","params":{}}')
WP_EXIT=$(echo "$WPEXEC" | jq -r '.exit_code // -1')
if [ "$WP_EXIT" = "0" ]; then pass "wp-exec exit_code=0"; else fail "wp-exec exit_code=$WP_EXIT body=$WPEXEC"; fi

# ── 6. Backup ─────────────────────────────────────────────────
step 6 "POST /api/sites/$SITE/backup"
BACKUP_RESP=$(api POST "/api/sites/$SITE/backup")
BACKUP_STATUS=$(echo "$BACKUP_RESP" | jq -r '.status // empty')
BACKUP_DATE=$(echo "$BACKUP_RESP" | jq -r '.date // empty')
if [ "$BACKUP_STATUS" = "backed_up" ] && [ -n "$BACKUP_DATE" ]; then
  pass "backup ok (date=$BACKUP_DATE)"
else
  fail "backup response: $BACKUP_RESP"
fi

# ── 7. List backups ───────────────────────────────────────────
step 7 "GET /api/sites/$SITE/backups"
LIST_RESP=$(api GET "/api/sites/$SITE/backups")
LIST_COUNT=$(echo "$LIST_RESP" | jq -r '.backups | length')
if [ "$LIST_COUNT" -ge 2 ] 2>/dev/null; then
  pass "backup list has $LIST_COUNT entries (>=2 expected: db + volume)"
else
  fail "backup list count=$LIST_COUNT — want >=2"
fi

# ── 8. Destroy ────────────────────────────────────────────────
step 8 "POST /api/destroy"
DEST_RESP=$(api POST /api/destroy "{\"site\":\"$SITE\"}")
DESTROY_JOB=$(echo "$DEST_RESP" | jq -r '.job_id // empty')
if [ -n "$DESTROY_JOB" ]; then
  pass "destroy queued (job $DESTROY_JOB)"
else
  fail "destroy response: $DEST_RESP"
  exit 1
fi

# ── 9. Poll destroy ───────────────────────────────────────────
step 9 "Poll destroy job"
poll_job "$DESTROY_JOB" "$POLL_TIMEOUT"
case $? in
  0) pass "destroy completed" ;;
  1) fail "destroy FAILED"; exit 1 ;;
  2) fail "destroy did not finish in ${POLL_TIMEOUT}s"; exit 1 ;;
esac

# ── 10. Verify 404 after hard delete ──────────────────────────
step 10 "DELETE record then verify 404"
DEL_CODE=$(api_code DELETE "/api/sites/$SITE")
if [ "$DEL_CODE" = "200" ]; then pass "site record deleted"; else fail "DELETE returned $DEL_CODE"; fi

GET_CODE=$(api_code GET "/api/sites/$SITE")
if [ "$GET_CODE" = "404" ]; then pass "site returns 404"; else fail "GET returns $GET_CODE (want 404)"; fi

# ── 11. Delete backup objects ─────────────────────────────────
step 11 "DELETE /api/sites/$SITE/backups/$BACKUP_DATE"
DEL_BK=$(api_code DELETE "/api/sites/$SITE/backups/$BACKUP_DATE")
# Note: at this point GetSite returns 404, so the backup delete handler also returns 404.
# That is correct behavior. We accept 200 (if site still on record) OR 404 (already gone) here.
if [ "$DEL_BK" = "200" ] || [ "$DEL_BK" = "404" ]; then
  pass "backup delete returned $DEL_BK"
else
  fail "backup delete returned $DEL_BK"
fi

# ── 12. Orphan check on Docker + MariaDB ──────────────────────
step 12 "Orphan check (containers, volume, DB)"
# These commands run on control-01; need the Docker TLS env to reach app-01,
# and MYSQL to reach state-01. We skip the check (with a warn, not a fail)
# if either is unavailable so smoke can still be run from a thin operator host.
if $DOCKER ps -a --format '{{.Names}}' >/dev/null 2>&1; then
  ORPH_PHP=$($DOCKER ps -a --filter "name=php_$SITE$" -q | wc -l)
  ORPH_NGX=$($DOCKER ps -a --filter "name=nginx_$SITE$" -q | wc -l)
  ORPH_VOL=$($DOCKER volume ls --filter "name=wp_$SITE$" -q | wc -l)
  [ "$ORPH_PHP" = "0" ] && pass "no php_$SITE container" || fail "php_$SITE container still present"
  [ "$ORPH_NGX" = "0" ] && pass "no nginx_$SITE container" || fail "nginx_$SITE container still present"
  [ "$ORPH_VOL" = "0" ] && pass "no wp_$SITE volume" || fail "wp_$SITE volume still present"
else
  warn "Docker check skipped (no Docker TLS reachable from this host)"
fi

if echo "SELECT 1;" | $MYSQL >/dev/null 2>&1; then
  ORPH_DB=$($MYSQL "SHOW DATABASES LIKE 'wp_$SITE';" | wc -l)
  [ "$ORPH_DB" = "0" ] && pass "no wp_$SITE database" || fail "wp_$SITE database still present"
else
  warn "MariaDB check skipped (no MariaDB reachable from this host)"
fi

# SITE already cleaned this way — clear so cleanup trap is a no-op
SITE=""
echo ""
if [ "$FAILED" = "0" ]; then
  echo -e "${GREEN}SMOKE PASS${NC}"
  exit 0
else
  echo -e "${RED}SMOKE FAIL${NC}"
  exit 1
fi
