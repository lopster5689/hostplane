#!/bin/bash
API="http://api.cowsaidmoo.tech/api"
KEY="fbb88aabb4a9d77746df1c3a8da0ed1e68c4d9a8e805bb77c62407cb1b5eb5a1"

for SITE in nikhil nnn scalewali testsite02 testsite05 abcd 5d91w2oo 381dwe43; do
  echo "Deleting site $SITE..."
  curl -s -X DELETE "$API/sites/$SITE" -H "X-API-Key: $KEY" | jq .
  echo
done
