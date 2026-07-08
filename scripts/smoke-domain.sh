#!/bin/bash
# Pre-deploy smoke test: custom-domain set + remove flow.
#
# Pre-requisite: a throwaway A record pointing at the VPS public IP. Default is
#   smoke.test.cowsaidmoo.tech  →  $PUBLIC_IP
# Override with $SMOKE_DOMAIN if you keep the throwaway A record on a different name.
#
# This script provisions a fresh WordPress site, attaches the domain, verifies cert
# issuance, removes the domain, and tears the site down.

set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; FAILED=1; }
warn() { echo -e "${YELLOW}!${NC} $1"; }
step() { echo ""; echo -e "${YELLOW}── Step $1 ──${NC} $2"; }

FAILED=0
SITE=""

API_URL="${API_URL:-http://localhost:8080}"
BASE_DOMAIN="${BASE_DOMAIN:-cowsaidmoo.tech}"
SMOKE_DOMAIN="${SMOKE_DOMAIN:-smoke.test.cowsaidmoo.tech}"
POLL_TIMEOUT="${POLL_TIMEOUT:-120}"
DOMAIN_TIMEOUT="${DOMAIN_TIMEOUT:-90}"   # seconds to wait for cert after set-domain

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
  [ -z "$SITE" ] && exit "$exit_code"
  echo ""; echo "── Cleanup ──"
  # best effort: remove domain (ignore failure), destroy (capture body+code in one call), hard-delete
  api_code DELETE "/api/sites/$SITE/domain" >/dev/null 2>&1 || true
  local resp http_status body job
  resp=$(curl -sS -w "\n%{http_code}" -X POST \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
    -d "{\"site\":\"$SITE\"}" \
    "${API_URL}/api/destroy" 2>/dev/null || true)
  http_status=$(printf '%s' "$resp" | tail -n1)
  body=$(printf '%s' "$resp" | sed '$d')
  if [ "$http_status" = "202" ]; then
    job=$(printf '%s' "$body" | jq -r '.job_id // empty')
    [ -n "$job" ] && poll_job "$job" 60 >/dev/null 2>&1 || true
  fi
  api_code DELETE "/api/sites/$SITE" >/dev/null 2>&1 || true
  echo "  destroyed $SITE"
  exit "$exit_code"
}
trap cleanup EXIT

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  HOSTPLANE — CUSTOM DOMAIN SMOKE TEST"
echo "  $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

command -v jq >/dev/null 2>&1 || { echo "jq required" >&2; exit 2; }

# Preflight: DNS must point to the VPS public IP. Without this the API will reject the domain.
PUBLIC_IP="${PUBLIC_IP:-$(grep '^PUBLIC_IP' /opt/control/.env 2>/dev/null | cut -d= -f2-)}"
PUBLIC_IP="${PUBLIC_IP:-***REMOVED***}"
RESOLVED=$(dig +short "$SMOKE_DOMAIN" A | tail -1)
if [ "$RESOLVED" != "$PUBLIC_IP" ]; then
  fail "DNS preflight: $SMOKE_DOMAIN resolves to '$RESOLVED' but expected $PUBLIC_IP"
  fail "set the throwaway A record and re-run, or override \$SMOKE_DOMAIN"
  exit 2
fi
pass "DNS preflight: $SMOKE_DOMAIN → $PUBLIC_IP"

if [ "$(api_code GET /api/health)" != "200" ]; then
  echo "ERROR: control plane unreachable at $API_URL" >&2; exit 2
fi

SITE="smoke$(date +%s)"
echo "Site name: $SITE"

# ── 1. Provision a fresh site ─────────────────────────────────
step 1 "POST /api/provision"
RESP=$(api POST /api/provision "{\"site\":\"$SITE\"}")
JOB=$(echo "$RESP" | jq -r '.job_id // empty')
[ -n "$JOB" ] || { fail "provision response: $RESP"; exit 1; }
poll_job "$JOB" "$POLL_TIMEOUT"
case $? in 0) pass "provisioned";; *) fail "provision did not complete"; exit 1;; esac

# ── 2. Set custom domain ──────────────────────────────────────
step 2 "POST /api/sites/$SITE/domain"
DR=$(api POST "/api/sites/$SITE/domain" "{\"domain\":\"$SMOKE_DOMAIN\"}")
SET_STATUS=$(echo "$DR" | jq -r '.status // empty')
SET_CERT=$(echo "$DR" | jq -r '.cert_status // empty')
if [ "$SET_STATUS" = "active" ]; then pass "domain set, status=active"; else fail "domain set status=$SET_STATUS body=$DR"; fi
if [ "$SET_CERT" = "issued" ]; then pass "cert_status=issued"; else warn "cert_status=$SET_CERT — will poll"; fi

# ── 3. Poll domain/status until ready or timeout ──────────────
step 3 "GET /api/sites/$SITE/domain/status (poll)"
elapsed=0; ready=0
while [ "$elapsed" -lt "$DOMAIN_TIMEOUT" ]; do
  DS=$(api GET "/api/sites/$SITE/domain/status")
  R=$(echo "$DS" | jq -r '.ready')
  STEP=$(echo "$DS" | jq -r '.step')
  if [ "$R" = "true" ]; then ready=1; break; fi
  echo "    step=$STEP elapsed=${elapsed}s — still waiting"
  sleep 5; elapsed=$((elapsed + 5))
done
[ "$ready" = "1" ] && pass "ready=true within ${elapsed}s" || fail "not ready within ${DOMAIN_TIMEOUT}s"

# ── 4. Public reachability over custom domain ─────────────────
step 4 "curl https://$SMOKE_DOMAIN/"
body=$(curl -sS --max-time 10 "https://$SMOKE_DOMAIN/" 2>/dev/null || true)
if echo "$body" | grep -qi "wordpress"; then pass "custom domain serves WordPress"; else fail "custom domain did not serve WordPress"; fi

# ── 5. Remove custom domain ───────────────────────────────────
step 5 "DELETE /api/sites/$SITE/domain"
RM_CODE=$(api_code DELETE "/api/sites/$SITE/domain")
[ "$RM_CODE" = "200" ] && pass "domain removed" || fail "DELETE returned $RM_CODE"

# Verify site still resolves on default subdomain
DEFAULT_URL="https://${SITE}.${BASE_DOMAIN}/"
elapsed=0; ok=0
while [ "$elapsed" -lt 30 ]; do
  body=$(curl -sS --max-time 10 "$DEFAULT_URL" 2>/dev/null || true)
  if echo "$body" | grep -qi "wordpress"; then ok=1; break; fi
  sleep 3; elapsed=$((elapsed + 3))
done
[ "$ok" = "1" ] && pass "default subdomain still serves WordPress" || fail "default subdomain unreachable after domain remove"

# Verify custom domain status now reports no custom domain
ST=$(api_code GET "/api/sites/$SITE/domain/status")
[ "$ST" = "400" ] && pass "domain/status returns 400 (no domain set)" || fail "domain/status returns $ST"

echo ""
if [ "$FAILED" = "0" ]; then echo -e "${GREEN}SMOKE PASS${NC}"; exit 0
else echo -e "${RED}SMOKE FAIL${NC}"; exit 1; fi
