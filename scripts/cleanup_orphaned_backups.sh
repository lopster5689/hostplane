#!/bin/bash
API="http://api.cowsaidmoo.tech/api"
KEY="fbb88aabb4a9d77746df1c3a8da0ed1e68c4d9a8e805bb77c62407cb1b5eb5a1"
SITE="site-a"

for DATE in 2026-03-07 2026-03-08 2026-03-09 2026-03-10 2026-03-11 2026-03-12 2026-03-13; do
  echo "Deleting $SITE backups for $DATE..."
  curl -s -X DELETE "$API/sites/$SITE/backups/$DATE" -H "X-API-Key: $KEY" | jq .
  echo
done
