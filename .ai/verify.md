# Hostplane — Deployment & Feature Verification Playbook

> Run every step from **control-01** unless noted otherwise.
> Set `KEY` and `BASE` once at the top and all curl commands work as-is.

```bash
export KEY=<your-api-key>
export BASE=https://api.cowsaidmoo.tech/api
export SITE=testsite   # change as needed
```

---

## Part 1 — Deploy the feat-backup branch

### 1.1 Pull and build on control-01

```bash
cd /opt/control/hostplane          # or wherever your git checkout lives
git fetch origin
git checkout feat-backup
go build -o /opt/control/control-plane .
```

Expected: no output, exit 0. If it fails, run `go vet ./...` first.

### 1.2 Add R2 env vars to `.env`

```bash
# /opt/control/.env  — append these four lines
R2_ACCOUNT_ID=<cloudflare-account-id>
R2_ACCESS_KEY_ID=<r2-r2-token-id>
R2_SECRET_ACCESS_KEY=<r2-token-secret>
R2_BUCKET=hostplane-backups
REQUIRE_BACKUP_BEFORE_DESTROY=true   # default; set false to skip during debug
```

Get `R2_ACCOUNT_ID` from Cloudflare dashboard → R2 → "Account ID" (top right).
Create an R2 API token at: Cloudflare → R2 → "Manage R2 API tokens" — needs
`Object Read & Write` on `hostplane-backups`.

### 1.3 Restart the service

```bash
systemctl restart control-plane
journalctl -u control-plane -f -n 50
```

**Expected log lines on startup:**

```
[db] MigrateSchema: applied last_backup_at column (or already exists)
[main] R2 configured — backup worker starting
[backupper] BackupAll: ...   ← first run fires immediately
```

If R2 credentials are wrong/missing you'll see:
```
[main] R2 not configured (...) — backups disabled
```
The binary still starts; only backup endpoints will return 503.

---

## Part 2 — Health check

```bash
curl -s $BASE/health
```

**Expected:** `{"status":"ok"}`

No auth required. If this fails the binary isn't running or the port is wrong.

---

## Part 3 — Provision a WordPress site

```bash
# 3.1 Queue provisioning
RESP=$(curl -s -X POST $BASE/provision \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"site\":\"$SITE\"}")
JOB_ID=$(echo $RESP | jq -r .job_id)
echo "Job: $JOB_ID"
```

**Expected `202`:**
```json
{"job_id":"...","site":"testsite","domain":"testsite.cowsaidmoo.tech","status":"PENDING"}
```

```bash
# 3.2 Poll until COMPLETED (takes ~30-60s)
watch -n 5 "curl -s -H 'X-API-Key: $KEY' $BASE/jobs/$JOB_ID | jq '{status,error}'"
```

**Expected final state:** `"status": "COMPLETED"`, `"error": ""`

```bash
# 3.3 Verify site infra is healthy
curl -s -H "X-API-Key: $KEY" $BASE/sites/$SITE | jq '{status,cert_status,warnings}'
```

**Expected:**
```json
{"status":"ACTIVE","cert_status":"issued","warnings":[]}
```

If `cert_status` is `"pending"`, wait ~2 min or go to Part 6 (cert-retry).

```bash
# 3.4 Confirm the site loads in a browser or via curl
curl -sI https://$SITE.cowsaidmoo.tech | head -5
```

**Expected:** `HTTP/2 200` (or `301` redirect to WordPress install page).

---

## Part 4 — Custom domain

> Skip if you don't have a domain to test with.

```bash
DOMAIN=example.com   # must have A record already pointing to ***REMOVED***

# 4.1 Set the domain (validates DNS before accepting)
curl -s -X POST $BASE/sites/$SITE/domain \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"domain\":\"$DOMAIN\"}" | jq '{custom_domain,cert_status}'
```

**Expected:** `"cert_status": "issued"` if DNS was already propagated.

```bash
# 4.2 Poll domain/status until ready=true
watch -n 5 "curl -s -H 'X-API-Key: $KEY' $BASE/sites/$SITE/domain/status \
  | jq '{step, dns_ok: .dns.ok, cert_status, ready}'"
```

**Expected progression:** `pending_dns` → `pending_cert` → `active`

```bash
# 4.3 Remove the custom domain
curl -s -X DELETE $BASE/sites/$SITE/domain \
  -H "X-API-Key: $KEY" | jq .
```

**Expected:** `{"site":"testsite","domain":"testsite.cowsaidmoo.tech","status":"custom domain removed"}`

---

## Part 5 — Backup feature

> Requires R2 configured (Part 1.2). If not, all backup endpoints return 503.

```bash
# 5.1 Trigger an on-demand backup
curl -s -X POST $BASE/sites/$SITE/backup \
  -H "X-API-Key: $KEY" | jq .
```

**Expected `200`** (takes 10–60s depending on DB/volume size):
```json
{"site":"testsite","status":"backed_up","date":"2026-03-04"}
```

While this runs, watch the logs:
```bash
journalctl -u control-plane -f | grep backupper
```
You should see the mysqldump container start, the tar container start, and
`backup complete (db + volume)`.

```bash
# 5.2 List backups — confirm both objects are in R2
curl -s -H "X-API-Key: $KEY" $BASE/sites/$SITE/backups | jq '.backups[] | {date,type,size_bytes}'
```

**Expected:** Two entries for today — one `"type":"database"`, one `"type":"volume"`. Both
`size_bytes` must be > 0. A 0-byte entry means the backup container failed silently — check logs.

```bash
# 5.3 Verify last_backup_at is set on the site record
curl -s -H "X-API-Key: $KEY" $BASE/sites/$SITE | jq .last_backup_at
```

**Expected:** ISO timestamp from today, e.g. `"2026-03-04T02:00:31Z"`. Not null.

```bash
# 5.4 Verify the objects exist in R2 directly (optional — via wrangler or dashboard)
# In Cloudflare R2 dashboard → hostplane-backups — you should see:
#   databases/testsite/2026-03-04.sql.gz
#   volumes/testsite/2026-03-04.tar.gz
```

---

## Part 6 — Restore feature (end-to-end drill)

> This is the most critical verification — backups are only valuable if you can
> restore from them. Do this before shipping to real customers.

```bash
# 6.1 Note the current date of the backup you want to restore from
DATE=$(curl -s -H "X-API-Key: $KEY" $BASE/sites/$SITE/backups \
  | jq -r '[.backups[].date] | unique | last')
echo "Will restore from: $DATE"
```

```bash
# 6.2 Corrupt the database so you can verify the restore actually worked
# Connect to state-01 and drop a table:
#   mysql -h ***REMOVED*** -u root -p wp_testsite -e "DROP TABLE wp_options;"
# Or via the control-01 WP_DSN credentials:
mysql -h ***REMOVED*** -u control -p***REMOVED*** wp_$SITE \
  -e "DROP TABLE wp_options;"
```

```bash
# 6.3 Confirm the site is broken
curl -sI https://$SITE.cowsaidmoo.tech
# Expected: 500 or WordPress DB error page
```

```bash
# 6.4 Restore from the backup
curl -s -X POST $BASE/sites/$SITE/restore/$DATE \
  -H "X-API-Key: $KEY" | jq .
```

**Expected `200`** (takes ~10–60s):
```json
{"site":"testsite","date":"2026-03-04","status":"restored"}
```

Watch logs during this:
```bash
journalctl -u control-plane -f | grep -E 'backupper|restore'
```
You should see the mysql restore container start and exit 0, then the tar
restore container start and exit 0.

```bash
# 6.5 Confirm the site is working again
curl -sI https://$SITE.cowsaidmoo.tech
# Expected: HTTP/2 200
```

```bash
# 6.6 Verify wp_options is back
mysql -h ***REMOVED*** -u control -p***REMOVED*** wp_$SITE \
  -e "SELECT option_value FROM wp_options WHERE option_name='siteurl';"
# Expected: https://testsite.cowsaidmoo.tech
```

---

## Part 7 — Pre-destroy backup gate

```bash
# 7.1 Queue a destroy — this will trigger BackupSite first (blocks until done)
JOB_ID=$(curl -s -X POST $BASE/destroy \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"site\":\"$SITE\"}" | jq -r .job_id)
echo "Destroy job: $JOB_ID"
```

```bash
# 7.2 Poll job status
watch -n 5 "curl -s -H 'X-API-Key: $KEY' $BASE/jobs/$JOB_ID | jq '{status,error}'"
```

**Expected:** Job goes `PENDING → PROCESSING → COMPLETED`.

In the logs you should see a pre-destroy backup fire before any containers are
removed:
```
[backupper] site=testsite backup complete (db + volume)
[destroyer] site=testsite removing container php_testsite ...
```

```bash
# 7.3 Verify the destroy backup object landed in R2
curl -s -H "X-API-Key: $KEY" $BASE/sites/$SITE/backups \
  | jq '[.backups[].date] | unique'
# Should include today's date
```

> **To test with REQUIRE_BACKUP_BEFORE_DESTROY=false:** add it to `.env`,
> restart, destroy a site — the backup step is skipped and a warning is logged.
> Re-enable and restart before going to production.

---

## Part 8 — Error path verification

```bash
# 8.1 Bad date format → should return 400
curl -s -X POST $BASE/sites/$SITE/restore/not-a-date \
  -H "X-API-Key: $KEY" | jq .
# Expected: {"error":"invalid date format — expected YYYY-MM-DD"}, HTTP 400

# 8.2 Date with no backup → should return 500 (backup not found in R2)
curl -s -X POST $BASE/sites/$SITE/restore/1999-01-01 \
  -H "X-API-Key: $KEY" | jq .
# Expected: {"error":"DB backup not found in R2 (...): ..."}, HTTP 500

# 8.3 Unknown site → 404
curl -s -X POST $BASE/sites/doesnotexist/backup \
  -H "X-API-Key: $KEY" | jq .
# Expected: {"error":"site not found"}, HTTP 404

# 8.4 No R2 credentials (test by temporarily unsetting in .env, restarting)
curl -s -X POST $BASE/sites/$SITE/backup \
  -H "X-API-Key: $KEY" | jq .
# Expected: {"error":"backup not configured (R2 credentials missing)"}, HTTP 503
```

---

## Part 9 — Automatic backup scheduler

The backup worker fires immediately on startup, then every 24 h. Every 7th tick
it also deletes objects older than 30 days.

```bash
# Verify first run fired on startup
journalctl -u control-plane --since "5 minutes ago" | grep BackupAll
# Expected: "[backupper] BackupAll complete: N ok, 0 failed"

# To force a re-run without waiting 24h, restart the service
systemctl restart control-plane
journalctl -u control-plane -f | grep BackupAll
```

---

## Part 10 — Hard delete after destroy

```bash
# After destroy job COMPLETED:
curl -s -X DELETE $BASE/sites/$SITE \
  -H "X-API-Key: $KEY" | jq .
# Expected: {"deleted":"testsite"}

# Confirm it's gone
curl -s -H "X-API-Key: $KEY" $BASE/sites/$SITE | jq .
# Expected: {"error":"site not found"}, HTTP 404
```

---

## Quick smoke-test sequence (all features, ~5 min)

```bash
export KEY=... BASE=https://api.cowsaidmoo.tech/api SITE=smoke01

# Health
curl -sf $BASE/health

# Provision
JOB=$(curl -sf -X POST $BASE/provision -H "X-API-Key: $KEY" \
  -H "Content-Type: application/json" -d '{"site":"'$SITE'"}' | jq -r .job_id)
until [ "$(curl -sf -H "X-API-Key: $KEY" $BASE/jobs/$JOB | jq -r .status)" = "COMPLETED" ]; do
  sleep 5; echo -n "."; done; echo " PROVISIONED"

# Backup
curl -sf -X POST $BASE/sites/$SITE/backup -H "X-API-Key: $KEY" | jq .status

# List
curl -sf -H "X-API-Key: $KEY" $BASE/sites/$SITE/backups | jq '.backups | length'
# Expected: 2

# Restore
DATE=$(date -u +%Y-%m-%d)
curl -sf -X POST $BASE/sites/$SITE/restore/$DATE -H "X-API-Key: $KEY" | jq .status

# Destroy
JOB=$(curl -sf -X POST $BASE/destroy -H "X-API-Key: $KEY" \
  -H "Content-Type: application/json" -d '{"site":"'$SITE'"}' | jq -r .job_id)
until [ "$(curl -sf -H "X-API-Key: $KEY" $BASE/jobs/$JOB | jq -r .status)" = "COMPLETED" ]; do
  sleep 5; echo -n "."; done; echo " DESTROYED"

# Hard delete
curl -sf -X DELETE $BASE/sites/$SITE -H "X-API-Key: $KEY"

echo "All done."
```
