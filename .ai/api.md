# Hostplane Control Plane — API Reference

## Base URL

```
https://api.cowsaidmoo.tech/api
```

## Authentication

Every endpoint except `GET /api/health` requires the `X-API-Key` header.

```
X-API-Key: <your-api-key>
```

Returns `401 Unauthorized` if the key is missing or wrong:

```json
{ "error": "unauthorized" }
```

---

## Site Name Rules

- Lowercase letters and numbers only (`^[a-z0-9]+$`)
- Used as the subdomain: `<site>.cowsaidmoo.tech`

---

## Endpoints

| Method   | Path                             | Description                               |
| -------- | -------------------------------- | ----------------------------------------- |
| `GET`    | `/api/health`                    | Health check (no auth)                    |
| `POST`   | `/api/provision`                 | Provision a WordPress site                |
| `POST`   | `/api/static/provision`          | Provision a static site                   |
| `POST`   | `/api/destroy`                   | Destroy a site (queues job)               |
| `GET`    | `/api/sites`                     | List all sites                            |
| `GET`    | `/api/sites/:site`               | Get site status + live infra checks       |
| `DELETE` | `/api/sites/:site`               | Hard delete a DESTROYED site record       |
| `POST`   | `/api/sites/:site/domain`        | Set a custom domain                       |
| `DELETE` | `/api/sites/:site/domain`        | Remove the custom domain                  |
| `GET`    | `/api/sites/:site/domain/status` | Live DNS + cert status (poll from UI)     |
| `POST`   | `/api/sites/:site/cert-retry`    | Force Caddy reload + poll cert            |
| `POST`   | `/api/sites/:site/backup`        | On-demand backup of a site's DB + volume  |
| `GET`    | `/api/sites/:site/backups`       | List backups for a site                   |
| `POST`   | `/api/sites/:site/restore/:date` | Restore a site from a dated backup        |
| `DELETE` | `/api/sites/:site/backups/:date` | Delete a site's backup objects            |
| `POST`   | `/api/sites/:site/wp-exec`       | Run a structured WP-CLI tool in a site    |
| `POST`   | `/api/pg/backup`                 | On-demand PostgreSQL backup               |
| `GET`    | `/api/pg/backups`                | List PostgreSQL backups                   |
| `POST`   | `/api/pg/restore/:date`          | Restore PostgreSQL from a dated backup    |
| `GET`    | `/api/jobs/:id`                  | Get job status                            |
| `DELETE` | `/api/jobs/:id`                  | Hard delete a completed/failed job record |

---

## `GET /api/health`

Health check. No authentication required.

**Response `200`**

```json
{ "status": "ok" }
```

---

## `POST /api/provision`

Queues a WordPress site provisioning job. Creates MariaDB database, Docker
volume, PHP-FPM container, nginx sidecar, and Caddy snippet. Returns
immediately with a job ID — poll `GET /api/jobs/:id` for completion.

**Request**

```json
{ "site": "mysite" }
```

**Response `202`**

```json
{
  "job_id": "63c83c3f-775d-40b5-96ad-567c3f1e10d3",
  "site": "mysite",
  "domain": "mysite.cowsaidmoo.tech",
  "status": "PENDING"
}
```

**Errors**

| Code  | Reason                                                       |
| ----- | ------------------------------------------------------------ |
| `400` | Invalid or missing site name                                 |
| `409` | Site already ACTIVE, or already has a pending/processing job |

---

## `POST /api/static/provision`

Queues a static site provisioning job. Accepts a zip file, extracts it into
the shared `caddy_static_sites` volume, and configures Caddy's `file_server`.
Returns immediately with a job ID.

**Request** — `multipart/form-data`

| Field  | Type   | Required | Description                    |
| ------ | ------ | -------- | ------------------------------ |
| `site` | string | yes      | Site name (`^[a-z0-9]+$`)      |
| `zip`  | file   | yes      | Zip archive of the static site |

**Response `202`**

```json
{
  "job_id": "abc123",
  "site": "mystaticsite",
  "domain": "mystaticsite.cowsaidmoo.tech",
  "status": "PENDING"
}
```

**Errors**

| Code  | Reason                                                       |
| ----- | ------------------------------------------------------------ |
| `400` | Missing site name, invalid name, or missing zip file         |
| `409` | Site already ACTIVE, or already has a pending/processing job |

---

## `POST /api/destroy`

Queues a destroy job. Tears down all Docker containers, volumes, Caddy
snippet, and database for the site. Returns immediately — poll
`GET /api/jobs/:id` for completion.

**Request**

```json
{ "site": "mysite" }
```

**Response `202`**

```json
{
  "job_id": "abc123",
  "site": "mysite",
  "status": "PENDING"
}
```

**Errors**

| Code  | Reason                                                        |
| ----- | ------------------------------------------------------------- |
| `400` | Invalid or missing site name                                  |
| `404` | Site not found                                                |
| `409` | Site is already DESTROYING or DESTROYED, or has an active job |

---

## `GET /api/sites`

Returns all site records.

**Response `200`**

```json
{
  "sites": [
    {
      "site": "mysite",
      "domain": "mysite.cowsaidmoo.tech",
      "custom_domain": "example.com",
      "status": "ACTIVE",
      "job_id": "63c83c3f-...",
      "created_at": "2026-02-25T02:56:20Z",
      "updated_at": "2026-02-25T02:56:52Z"
    }
  ]
}
```

---

## `GET /api/sites/:site`

Returns site status with live infra checks against the Caddy container.
For `ACTIVE` and `DOMAIN_ACTIVE` sites, verifies:

1. TLS cert is present on Caddy's disk
2. Caddy snippet file exists inside the container
3. Snippet contains the active domain (catches stale config after domain moves)

**Response `200`**

```json
{
  "site": "mysite",
  "domain": "mysite.cowsaidmoo.tech",
  "custom_domain": "example.com",
  "status": "ACTIVE",
  "cert_status": "issued",
  "warnings": [],
  "job_id": "63c83c3f-...",
  "created_at": "2026-02-25T02:56:20Z",
  "updated_at": "2026-02-25T02:56:52Z"
}
```

**`cert_status` values**

| Value       | Meaning                                                |
| ----------- | ------------------------------------------------------ |
| `"issued"`  | TLS cert file present in Caddy's ACME storage          |
| `"pending"` | Cert not yet on disk — Caddy is retrying in background |
| `""`        | Site is not ACTIVE/DOMAIN_ACTIVE — not checked         |

**`warnings` examples**

```json
"warnings": [
  "TLS cert not yet issued — Caddy is retrying ACME in background; call /cert-retry to force"
]
```

```json
"warnings": [
  "Caddy config snippet missing — site will not be routed; re-provision or call /cert-retry"
]
```

```json
"warnings": [
  "Caddy snippet exists but does not route example.com — reload may be needed; call /cert-retry"
]
```

An empty array (`[]`) means all infra checks passed.

**Errors**

| Code  | Reason         |
| ----- | -------------- |
| `404` | Site not found |

---

## `DELETE /api/sites/:site`

Hard deletes the site record from the database. The site **must** be in
`DESTROYED` status. Use `POST /api/destroy` first to destroy infrastructure.

**Response `200`**

```json
{ "deleted": "mysite" }
```

**Errors**

| Code  | Reason                          |
| ----- | ------------------------------- |
| `404` | Site not found                  |
| `409` | Site is not in DESTROYED status |

---

## `POST /api/sites/:site/domain`

Sets a custom domain for an existing ACTIVE site. Validates that:

- Domain format is valid and is not a subdomain of the base domain
- DNS A record for the domain resolves to the VPS public IP
- Domain is not already claimed by another site

For WordPress sites, also updates the nginx `server_name` and `wp_options`
`siteurl` / `home` values. Caddy snippet is rewritten and reloaded atomically.
Polls for cert issuance for up to 30 s before returning.

**Request**

```json
{ "domain": "example.com" }
```

**Response `200`**

```json
{
  "site": "mysite",
  "default_domain": "mysite.cowsaidmoo.tech",
  "custom_domain": "example.com",
  "cert_status": "issued",
  "status": "active"
}
```

**Idempotent:** If the same domain is already set, returns `200` immediately
with `"message": "domain already set"`.

**Errors**

| Code  | Reason                                                                        |
| ----- | ----------------------------------------------------------------------------- |
| `400` | Invalid domain format, domain is a base subdomain, or DNS not pointing to VPS |
| `404` | Site not found                                                                |
| `409` | Site not ACTIVE/DOMAIN_ACTIVE, or domain already claimed by another site      |
| `500` | nginx/Caddy config failed (infra rolled back)                                 |

---

## `DELETE /api/sites/:site/domain`

Removes the custom domain from a site. Reverts Caddy snippet to default
subdomain only, reloads Caddy, and for WordPress sites reverts nginx
`server_name` and `wp_options` URLs.

**Response `200`**

```json
{
  "site": "mysite",
  "domain": "mysite.cowsaidmoo.tech",
  "status": "custom domain removed"
}
```

**Errors**

| Code  | Reason                         |
| ----- | ------------------------------ |
| `400` | No custom domain currently set |
| `404` | Site not found                 |
| `500` | nginx/Caddy revert failed      |

---

## `GET /api/sites/:site/domain/status`

Returns a live snapshot of the custom domain's DNS propagation and TLS cert
state. **No infra changes are made** — safe to poll every few seconds from the
UI during the "add domain" setup flow.

Designed to power a setup checklist like:

```
[ ] Point phoenixdns.app → ***REMOVED***   ← dns.ok
[ ] TLS certificate issued                  ← cert_status == "issued"
```

**Response `200`**

```json
{
  "domain": "phoenixdns.app",
  "expected_ip": "***REMOVED***",
  "dns": {
    "ok": true,
    "resolved": ["***REMOVED***"]
  },
  "cert_status": "issued",
  "ready": true,
  "step": "active"
}
```

**`step` values**

| Value          | Meaning                                          |
| -------------- | ------------------------------------------------ |
| `pending_dns`  | A record not yet pointing to the VPS IP          |
| `pending_cert` | DNS is correct but TLS cert not issued yet       |
| `active`       | DNS correct + cert issued — domain is fully live |

**DNS not yet propagated example**

```json
{
  "domain": "phoenixdns.app",
  "expected_ip": "***REMOVED***",
  "dns": {
    "ok": false,
    "resolved": ["***REMOVED***"]
  },
  "cert_status": "pending",
  "ready": false,
  "step": "pending_dns"
}
```

**DNS correct, cert pending example**

```json
{
  "domain": "phoenixdns.app",
  "expected_ip": "***REMOVED***",
  "dns": {
    "ok": true,
    "resolved": ["***REMOVED***"]
  },
  "cert_status": "pending",
  "ready": false,
  "step": "pending_cert"
}
```

**Errors**

| Code  | Reason                            |
| ----- | --------------------------------- |
| `400` | No custom domain set on this site |
| `404` | Site not found                    |

**Typical UI polling loop**

```js
// Poll every 5s until ready
const poll = setInterval(async () => {
  const res = await fetch(`/api/sites/${site}/domain/status`, {
    headers: { "X-API-Key": key },
  });
  const data = await res.json();
  updateUI(data.step, data.dns, data.cert_status);
  if (data.ready) clearInterval(poll);
}, 5000);
```

---

## `POST /api/sites/:site/cert-retry`

Forces a Caddy reload to re-queue ACME certificate issuance for the site's
active domain, then polls for up to 30 s and returns the current cert status.

Use this when:

- `cert_status` has been `"pending"` for more than ~10 minutes
- A `warnings` entry says the snippet is stale or the domain is missing
- After fixing a DNS or port 80 issue

**Response `200`**

```json
{
  "site": "mysite",
  "domain": "example.com",
  "cert_status": "issued"
}
```

**Errors**

| Code  | Reason                              |
| ----- | ----------------------------------- |
| `404` | Site not found                      |
| `409` | Site is not ACTIVE or DOMAIN_ACTIVE |
| `500` | Caddy reload failed                 |

---

## `GET /api/jobs/:id`

Returns the current state of a provisioning or destroy job.

**Response `200`**

```json
{
  "job_id": "63c83c3f-775d-40b5-96ad-567c3f1e10d3",
  "type": "provision",
  "site": "mysite",
  "status": "COMPLETED",
  "attempts": 1,
  "max_attempts": 3,
  "error": "",
  "created_at": "2026-02-25T02:56:20Z",
  "started_at": "2026-02-25T02:56:21Z",
  "completed_at": "2026-02-25T02:56:52Z"
}
```

**`type` values**

| Value              | Triggered by                 |
| ------------------ | ---------------------------- |
| `provision`        | `POST /api/provision`        |
| `static_provision` | `POST /api/static/provision` |
| `destroy`          | `POST /api/destroy`          |

**`status` values**

| Value        | Meaning                             |
| ------------ | ----------------------------------- |
| `PENDING`    | Queued, not yet picked up by worker |
| `PROCESSING` | Worker is actively running the job  |
| `COMPLETED`  | Job finished successfully           |
| `FAILED`     | All retry attempts exhausted        |

**Errors**

| Code  | Reason        |
| ----- | ------------- |
| `404` | Job not found |

---

## `DELETE /api/jobs/:id`

Hard deletes a job record from the database. Job must be in `COMPLETED` or
`FAILED` status.

**Response `200`**

```json
{ "deleted": "63c83c3f-775d-40b5-96ad-567c3f1e10d3" }
```

**Errors**

| Code  | Reason                                      |
| ----- | ------------------------------------------- |
| `404` | Job not found                               |
| `409` | Job is PENDING or PROCESSING (still active) |

---

## `POST /api/sites/:site/backup`

Triggers an on-demand backup of the site's MariaDB database and Docker volume.
Streams both to R2 under `wp/<site>/db/` and `wp/<site>/vol/`. Runs
synchronously — response returns after the upload completes.

**Response `200`**

```json
{ "site": "mysite", "status": "backed_up", "date": "2026-05-15" }
```

**Errors**

| Code  | Reason                                            |
| ----- | ------------------------------------------------- |
| `404` | Site not found                                    |
| `500` | Backup pipeline failed (dump, tar, or R2 upload)  |
| `503` | Backup not configured — R2 credentials missing    |

---

## `GET /api/sites/:site/backups`

Lists every backup object in R2 for the site, with type, date, and size. Both
database and volume entries are returned together.

**Response `200`**

```json
{
  "site": "mysite",
  "backups": [
    { "date": "2026-05-15", "type": "database", "key": "wp/mysite/db/2026-05-15.sql.gz", "size_bytes": 482113 },
    { "date": "2026-05-15", "type": "volume",   "key": "wp/mysite/vol/2026-05-15.tar.gz", "size_bytes": 39812455 }
  ]
}
```

An empty `"backups": []` means no objects exist for the site.

**Errors**

| Code  | Reason                                         |
| ----- | ---------------------------------------------- |
| `404` | Site not found                                 |
| `500` | R2 list failed                                 |
| `503` | Backup not configured — R2 credentials missing |

---

## `POST /api/sites/:site/restore/:date`

Restores both the database and volume from the dated backup. The `:date` path
parameter must match the `YYYY-MM-DD` form. Runs synchronously.

> Running containers are **not** stopped automatically. Quiesce writes (stop
> PHP-FPM or put the site in maintenance mode) before restoring for a fully
> consistent state.

**Response `200`**

```json
{ "site": "mysite", "date": "2026-05-15", "status": "restored" }
```

**Errors**

| Code  | Reason                                         |
| ----- | ---------------------------------------------- |
| `400` | Invalid date format                            |
| `404` | Site not found                                 |
| `500` | Restore failed (download, untar, or DB import) |
| `503` | Backup not configured — R2 credentials missing |

---

## `DELETE /api/sites/:site/backups/:date`

Deletes the database and volume backup objects for the site on the given
date. If only one of the two exists, it is still deleted; the response lists
which keys were removed.

**Response `200`**

```json
{
  "site": "mysite",
  "date": "2026-05-15",
  "deleted": ["wp/mysite/db/2026-05-15.sql.gz", "wp/mysite/vol/2026-05-15.tar.gz"],
  "errors": []
}
```

**Errors**

| Code  | Reason                                         |
| ----- | ---------------------------------------------- |
| `400` | Invalid date format                            |
| `404` | Site not found                                 |
| `500` | Both DB and volume deletes failed              |
| `503` | Backup not configured — R2 credentials missing |

---

## `POST /api/sites/:site/wp-exec`

Runs a **structured** WP-CLI tool inside the site's PHP container. The control
plane never passes raw command strings — the request names a tool, the
control plane builds the WP-CLI argv from validated parameters. Designed to
be driven by an AI agent making tool calls.

**Request**

```json
{ "tool": "list_posts", "params": { "post_type": "page", "status": "publish" } }
```

**Response `200`**

```json
{
  "site": "mysite",
  "tool": "list_posts",
  "output": "[ ... wp output ... ]",
  "exit_code": 0
}
```

**Available tools**

| Tool                | Params                                    | Notes                                              |
| ------------------- | ----------------------------------------- | -------------------------------------------------- |
| `get_site_info`     | —                                         | Returns blogname                                   |
| `get_option`        | `key`                                     | Only allowlisted keys (no `siteurl`, `home`, etc.) |
| `update_option`     | `key`, `value`                            | Allowlist applies                                  |
| `list_posts`        | `post_type` (`post`/`page`), `status`     | JSON output                                        |
| `get_post`          | `id`                                      | Positive integer                                   |
| `update_post`       | `id`, `field`, `value`                    | `field` ∈ {title, content, excerpt, status}        |
| `create_post`       | `post_type`, `post_title`, `post_content` | Always created as `draft`                          |
| `list_plugins`      | —                                         |                                                    |
| `install_plugin`    | `slug`                                    | Validates wordpress.org slug; auto-activates       |
| `deactivate_plugin` | `slug`                                    |                                                    |
| `list_themes`       | —                                         |                                                    |
| `activate_theme`    | `slug`                                    |                                                    |
| `search_replace`    | `old`, `new`                              | **Dry-run only** in current build                  |
| `media_import`      | `url`, `title`                            | HTTPS only, SSRF guard against RFC1918 + loopback  |

**Limits**

- 30 s exec timeout
- Output capped at 1 MB (truncation marker appended)
- Container runs as `www-data:www-data` (uid/gid 33)

**Errors**

| Code  | Reason                                                            |
| ----- | ----------------------------------------------------------------- |
| `400` | Missing `tool`, unknown tool, or invalid params                   |
| `404` | Site not found                                                    |
| `409` | Site is not ACTIVE or DOMAIN_ACTIVE                               |
| `422` | Tool ran but `exit_code != 0` — response body still has `output`  |
| `500` | Docker exec failed (container missing, attach failed, etc.)       |

---

## `POST /api/pg/backup`

On-demand backup of the external PostgreSQL `hosto` database (the dashboard's
data store). Streams `pg_dump` → gzip → R2 under `postgres/<db>/`.

**Response `200`**

```json
{ "database": "hosto", "status": "backed_up", "date": "2026-05-15" }
```

**Errors**

| Code  | Reason                                            |
| ----- | ------------------------------------------------- |
| `500` | Backup pipeline failed                            |
| `503` | PG backup not configured — R2 or `POSTGRES_DSN` missing |

---

## `GET /api/pg/backups`

Lists PG backup objects in R2 along with the last recorded backup time
from the control DB.

**Response `200`**

```json
{
  "database": "hosto",
  "backups": [
    {
      "key": "postgres/hosto/2026-05-15.sql.gz",
      "date": "2026-05-15",
      "size_bytes": 18203112,
      "last_modified": "2026-05-15T03:00:11Z"
    }
  ],
  "last_backup_at": "2026-05-15T03:00:11Z",
  "last_backup_key": "postgres/hosto/2026-05-15.sql.gz"
}
```

**Errors**

| Code  | Reason                                                  |
| ----- | ------------------------------------------------------- |
| `500` | R2 list failed                                          |
| `503` | PG backup not configured — R2 or `POSTGRES_DSN` missing |

---

## `POST /api/pg/restore/:date`

Restores the PostgreSQL database from the dated backup. Runs synchronously.
The target database must already exist on the PG server.

**Response `200`**

```json
{ "database": "hosto", "date": "2026-05-15", "status": "restored" }
```

**Errors**

| Code  | Reason                                                  |
| ----- | ------------------------------------------------------- |
| `400` | Invalid date format                                     |
| `500` | Restore failed (download or `psql` import)              |
| `503` | PG backup not configured — R2 or `POSTGRES_DSN` missing |

---

## Site Status Lifecycle

```
                  POST /provision
                        │
                        ▼
                  PROVISIONING ──── job FAILED ──▶ FAILED (manual cleanup needed)
                        │
                   job COMPLETED
                        │
                        ▼
                     ACTIVE ◀──── DELETE /api/sites/:site/domain
                        │
          POST /api/sites/:site/domain
                        │
                        ▼
                 DOMAIN_ACTIVE
                        │
            POST /api/destroy (either status)
                        │
                        ▼
                  DESTROYING ──── job FAILED ──▶ FAILED
                        │
                   job COMPLETED
                        │
                        ▼
                  DESTROYED
                        │
          DELETE /api/sites/:site (hard delete)
                        │
                        ▼
                   (record gone)
```

---

## Common Workflows

### Provision a WordPress site and wait for it to be ready

```bash
# 1. Queue provisioning
RESP=$(curl -s -X POST https://api.cowsaidmoo.tech/api/provision \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"site":"mysite"}')
JOB_ID=$(echo $RESP | jq -r .job_id)

# 2. Poll until COMPLETED
watch -n 5 curl -s -H "X-API-Key: $KEY" \
  https://api.cowsaidmoo.tech/api/jobs/$JOB_ID | jq .status

# 3. Verify infra is healthy
curl -s -H "X-API-Key: $KEY" \
  https://api.cowsaidmoo.tech/api/sites/mysite | jq '{cert_status,warnings}'
```

### Set a custom domain and watch it go live

```bash
# Step 1 — tell the user to set their A record, then POST the domain
# (the API validates DNS is already pointing before accepting)
curl -s -X POST https://api.cowsaidmoo.tech/api/sites/mysite/domain \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"domain":"example.com"}' | jq '{custom_domain,cert_status}'

# Step 2 — poll domain/status from the UI until ready=true
# step: pending_dns → pending_cert → active
watch -n 5 'curl -s -H "X-API-Key: $KEY" \
  https://api.cowsaidmoo.tech/api/sites/mysite/domain/status \
  | jq "{step,dns_ok: .dns.ok, cert_status, ready}"'
```

**UI response progression:**

```
step=pending_dns  → show "Point example.com → ***REMOVED***"
step=pending_cert → show "DNS verified ✓  Waiting for TLS cert..."
step=active       → show "Domain live ✓"
```

### Force cert issuance if stuck

```bash
curl -s -X POST https://api.cowsaidmoo.tech/api/sites/mysite/cert-retry \
  -H "X-API-Key: $KEY" | jq .
```

### Destroy a site completely

```bash
# 1. Queue destroy
JOB_ID=$(curl -s -X POST https://api.cowsaidmoo.tech/api/destroy \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"site":"mysite"}' | jq -r .job_id)

# 2. Wait for COMPLETED, then hard delete the record
curl -s -X DELETE https://api.cowsaidmoo.tech/api/sites/mysite \
  -H "X-API-Key: $KEY"
```
