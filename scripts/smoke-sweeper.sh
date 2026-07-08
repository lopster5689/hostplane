#!/bin/bash
# Sweep stale smoke-test sites left over from crashed smoke runs.
#
# Site naming convention for smoke tests: smoke<unix_ts> (e.g. smoke1715784000).
# This sweeper finds any `smoke*` site whose `created_at` is older than the TTL
# and tears it down via the control-plane API. Intended to run from cron.
#
# Default TTL: 3600 seconds (1 hour). Override via $SMOKE_TTL_SECONDS.

set -uo pipefail

API_URL="${API_URL:-http://localhost:8080}"
SMOKE_TTL_SECONDS="${SMOKE_TTL_SECONDS:-3600}"

if [ -z "${API_KEY:-}" ]; then
  if [ -r /opt/control/.env ]; then
    API_KEY=$(grep '^API_KEY' /opt/control/.env | cut -d= -f2-)
  else
    echo "ERROR: cannot read /opt/control/.env and \$API_KEY not set" >&2; exit 2
  fi
fi
MYSQL="${MYSQL:-mysql --defaults-file=/root/.my-hosto.cnf -se}"

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

ts=$(date -u '+%Y-%m-%d %H:%M:%S UTC')
echo "[$ts] smoke-sweeper start (TTL=${SMOKE_TTL_SECONDS}s)"

# Sites that look like smoke runs (prefix `smoke`, fully ^smoke[0-9]+$) older than TTL.
STALE=$($MYSQL "
  SELECT site FROM sites
  WHERE site REGEXP '^smoke[0-9]+\$'
    AND created_at < NOW() - INTERVAL $SMOKE_TTL_SECONDS SECOND
")

if [ -z "$STALE" ]; then
  echo "  no stale smoke sites"
  exit 0
fi

while IFS= read -r site; do
  [ -z "$site" ] && continue
  echo "  reaping $site"
  status_row=$($MYSQL "SELECT status FROM sites WHERE site='$site';")
  case "$status_row" in
    DESTROYED|FAILED)
      api_code DELETE "/api/sites/$site" >/dev/null 2>&1 || true
      echo "    record deleted ($status_row)"
      ;;
    *)
      job=$(api POST /api/destroy "{\"site\":\"$site\"}" | jq -r '.job_id // empty')
      if [ -n "$job" ]; then
        # Poll up to 90s for the destroy to complete, then hard-delete
        for _ in $(seq 1 30); do
          s=$(api GET "/api/jobs/$job" | jq -r '.status // "?"')
          [ "$s" = "COMPLETED" ] && break
          [ "$s" = "FAILED" ] && { echo "    destroy job FAILED"; break; }
          sleep 3
        done
        api_code DELETE "/api/sites/$site" >/dev/null 2>&1 || true
        echo "    destroy completed"
      else
        echo "    could not queue destroy for $site"
      fi
      ;;
  esac
done <<< "$STALE"

echo "[$ts] smoke-sweeper done"
