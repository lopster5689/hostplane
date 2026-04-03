#!/bin/bash
API="http://api.cowsaidmoo.tech/api"
KEY="fbb88aabb4a9d77746df1c3a8da0ed1e68c4d9a8e805bb77c62407cb1b5eb5a1"

delete_backup() {
  local site=$1 date=$2
  echo "Deleting $site / $date..."
  curl -s -X DELETE "$API/sites/$site/backups/$date" -H "X-API-Key: $KEY" | jq .
  echo
}

# site-a — keep only 2026-04-03
delete_backup site-a 2026-03-14
delete_backup site-a 2026-03-15

# site-d — only has 2026-04-03, nothing to delete

# site-c — keep only 2026-04-03
for DATE in 2026-03-04 2026-03-05 2026-03-06 2026-03-07 2026-03-08 2026-03-09 2026-03-10 2026-03-11 2026-03-12 2026-03-13 2026-03-14 2026-03-15; do
  delete_backup site-c "$DATE"
done
