# n8n Hosting — Architecture Reference

> **Last updated:** 2026-05-27 | **Status:** Deployed to production | **Module:** `control-plane`

---

## 1. Overview

Hostplane offers n8n workflow automation hosting as a **standalone product** — customers can buy n8n hosting independently of WordPress. Each customer gets a fully isolated n8n instance with its own container, PostgreSQL database, Docker volume, and encryption key.

n8n v2.0 dropped MySQL/MariaDB as a storage backend, so n8n sites use **PostgreSQL 16** on state-01 (installed alongside the existing MariaDB that serves WordPress sites). This is the only product on the platform that uses PostgreSQL.

**Source code:** `n8n_provisioner.go` (provisioner), plus modifications to `destroyer.go`, `backupper.go`, `api.go`, `worker.go`, `db.go`, `naming.go`, `config.go`, `metrics.go`.

---

## 2. Per-Site Resources

Each n8n site provisions **5 infrastructure resources**:

| Resource | Name Pattern | Location | Created By |
|----------|-------------|----------|------------|
| PostgreSQL database | `n8n_<site>` | state-01 (***REMOVED***) | `N8nProvisioner.createPGDatabase()` |
| PostgreSQL user | `n8n_<site>` / `***REMOVED***` | state-01 | `N8nProvisioner.createPGDatabase()` |
| Docker volume | `n8n_<site>` (mounted at `/home/node/.n8n`) | app-01 | `N8nProvisioner.createVolume()` |
| n8n container | `n8n_<site>` (port 5678) | app-01 | `N8nProvisioner.createContainer()` |
| Caddy snippet | `<site>.caddy` | app-01 (Caddy container at `/etc/caddy/sites/`) | `N8nProvisioner.writeCaddyConfig()` |

Additionally, a unique **encryption key** is generated per site and stored in the `sites.encryption_key` column in the control plane MariaDB. This key is set as `N8N_ENCRYPTION_KEY` on the container. If lost, all credentials stored inside n8n become unrecoverable.

No nginx sidecar is needed — Caddy reverse-proxies directly to the n8n container on port 5678.

---

## 3. Request Path

```
Customer browser
  → Cloudflare (DDoS, caching)
    → VPS nginx (***REMOVED***) — TCP stream forwarder
      → WireGuard encrypted tunnel
        → app-01 Caddy (***REMOVED***, ports 80/443)
          → n8n_<site>:5678 (n8n HTTP server)
            → state-01 PostgreSQL (***REMOVED***:5432, database n8n_<site>)
```

Webhook callbacks follow the same path. `WEBHOOK_URL` is set to `https://<site>.<baseDomain>/` so external services (Slack, Razorpay, WhatsApp) POST back through Cloudflare → Caddy → n8n.

---

## 4. Provisioning Flow

**Entry:** `N8nProvisioner.Run(site string) error` (`n8n_provisioner.go`)

**Precondition:** `N8N_PG_DSN` must be configured. Returns error immediately if empty.

7-step flow with full rollback on failure:

```
Step 1: Generate encryption key
        → crypto/rand 32-byte hex string
        → Store in sites.encryption_key via db.SetEncryptionKey()

Step 2: Create PostgreSQL database + user on state-01
        → Ephemeral postgres:16 container on wp_backend network
        → Runs bash script with 4 psql commands:
          1. CREATE USER n8n_<site> WITH PASSWORD '***REMOVED***'
          2. CREATE DATABASE n8n_<site>
          3. GRANT ALL PRIVILEGES ON DATABASE n8n_<site> TO n8n_<site>
          4. GRANT ALL ON SCHEMA public TO n8n_<site>
        → Uses || true for idempotency (PG <17 has no IF NOT EXISTS for CREATE USER/DATABASE)
        → Connects as n8n_admin (admin user from N8N_PG_DSN)

Step 3: Create Docker volume n8n_<site> on app-01

Step 4: Start n8n_<site> container
        → See Section 6 for full container specification

Step 5: Write Caddy snippet
        → <site>.<baseDomain> { encode gzip; reverse_proxy n8n_<site>:5678 }
        → tar archive + CopyToContainer into Caddy at /etc/caddy/sites/

Step 6: Reload Caddy (zero-downtime)

Step 7: Poll TLS certificate (up to 30s, non-fatal)
```

### Rollback

Each step sets a boolean flag after success. On failure at step N, the rollback function undoes steps 1..(N-1) in reverse:

```
Undo Step 5: rm -f Caddy snippet + reload Caddy
Undo Step 4: docker stop + docker rm n8n_<site>
Undo Step 3: docker volume rm n8n_<site>
Undo Step 2: DROP DATABASE IF EXISTS + DROP USER IF EXISTS (via ephemeral psql container)
Undo Step 1: Clear encryption_key in DB
```

---

## 5. Destroy Flow

**Entry:** `Destroyer.destroyN8n(site string) error` (`destroyer.go`)

Triggered when `POST /api/destroy` is called for a site with `site_type == "n8n"`. The worker dispatches a `JobN8nDestroy` job.

```
Step 1: Pre-destroy backup (conditional, cfg.RequireBackupBeforeDestroy)
        → Calls backupper.BackupSite(site) which routes to backupN8nDatabase() for n8n
        → If backup fails → ABORT entire destroy

Step 2: Force-remove n8n_<site> container (ignores "not found")

Step 3: Force-remove n8n_<site> volume (ignores "not found")

Step 4: Remove Caddy snippet + reload Caddy

Step 5: Drop PostgreSQL database + user
        → Ephemeral postgres:16 container runs:
          DROP DATABASE IF EXISTS n8n_<site>; DROP USER IF EXISTS n8n_<site>;
        → Connects as n8n_admin via N8N_PG_DSN
```

No rollback — destroy is a one-way operation. The pre-destroy backup ensures data recovery is possible.

---

## 6. Container Specification

| Property | Value |
|----------|-------|
| Image | `cfg.N8nImage` (env `N8N_IMAGE`, default `n8nio/n8n:latest`) |
| Container name | `n8n_<site>` |
| Network | `wp_backend` (shared with WordPress containers and Caddy) |
| Volume | `n8n_<site>` → `/home/node/.n8n` |
| Memory | 1 GiB (1024 * 1024 * 1024 bytes) |
| CPU | 1.0 vCPU (1,000,000,000 NanoCPUs) |
| PID limit | 100 |
| Restart policy | `unless-stopped` |

### Environment Variables

| Variable | Value | Purpose |
|----------|-------|---------|
| `DB_TYPE` | `postgresdb` | n8n v2+ only supports `postgresdb` or `sqlite` |
| `DB_POSTGRESDB_HOST` | `***REMOVED***` (from `N8N_PG_DSN`) | PostgreSQL host on state-01 |
| `DB_POSTGRESDB_PORT` | `5432` (from `N8N_PG_DSN`) | PostgreSQL port |
| `DB_POSTGRESDB_DATABASE` | `n8n_<site>` | Per-site database |
| `DB_POSTGRESDB_USER` | `n8n_<site>` | Per-site database user |
| `DB_POSTGRESDB_PASSWORD` | `***REMOVED***` | Per-site database password |
| `N8N_ENCRYPTION_KEY` | 64-char hex (unique per site) | Encrypts stored credentials. Persisted in `sites.encryption_key`. |
| `N8N_HOST` | `0.0.0.0` | Listen on all interfaces inside container |
| `N8N_PORT` | `5678` | n8n default HTTP port |
| `N8N_PROTOCOL` | `https` | Behind Caddy TLS termination |
| `N8N_PROXY_HOPS` | `1` | Tells n8n to trust X-Forwarded-* from Caddy |
| `WEBHOOK_URL` | `https://<site>.<baseDomain>/` | Public URL for webhook callbacks |
| `N8N_EDITOR_BASE_URL` | `https://<site>.<baseDomain>/` | Public URL for the n8n editor UI and emails |

### Caddy Snippet

```
<site>.cowsaidmoo.tech {
    encode gzip
    reverse_proxy n8n_<site>:5678
}
```

With custom domain:

```
<site>.cowsaidmoo.tech, example.com, www.example.com {
    encode gzip
    reverse_proxy n8n_<site>:5678
}
```

---

## 7. Database Setup (state-01)

PostgreSQL 16 was installed on state-01 alongside the existing MariaDB. Both run on the same VM.

| Component | Detail |
|-----------|--------|
| Package | `postgresql` (16.14, Ubuntu 24.04 repo) |
| Data directory | `/var/lib/postgresql/16/main/` |
| Listen address | `localhost,***REMOVED***` (private network only) |
| Port | `5432` |
| Admin user | `n8n_admin` (CREATEDB, CREATEROLE privileges) |
| Admin password | Set via `N8N_PG_DSN` on control-01 |
| pg_hba.conf | `host all all ***REMOVED***/24 scram-sha-256` |

### Firewall Rules (iptables on state-01)

```
ACCEPT tcp ***REMOVED***  → :5432   (control-01 / pve)
ACCEPT tcp ***REMOVED*** → :5432   (app-01 — ephemeral containers reach PG through wp_backend)
ACCEPT tcp ***REMOVED*** → :5432   (localhost via network interface)
```

Rules persisted via `iptables-save > /etc/iptables/rules.v4`.

### Control-01 Configuration

Added to `/opt/control/.env`:

```
N8N_PG_DSN=postgres://n8n_admin:<password>@***REMOVED***:5432/postgres
N8N_IMAGE=n8nio/n8n:latest
```

The DSN points to the `postgres` default database — individual site databases (`n8n_<site>`) are created by the provisioner using this admin connection.

---

## 8. Backup & Restore

### Database Backup

**Function:** `Backupper.backupN8nDatabase(site string)` in `backupper.go`

```
pg_dump (in ephemeral postgres:16 container on wp_backend)
  → ContainerAttach (stdout)
    → stripDockerMux
      → gzip.NewWriter + io.Pipe
        → r2.Upload (streaming multipart)
```

- Connects as per-site user (`n8n_<site>` / `***REMOVED***`), not the admin user — the per-site user owns the tables n8n created
- R2 key: `databases/<site>/YYYY-MM-DD.sql.gz` (same structure as WordPress)
- Exit code verified; corrupt uploads deleted from R2

### Database Restore

**Function:** `Backupper.restoreN8nDatabase(ctx, site, date string)` in `backupper.go`

```
r2.Download → gzip.NewReader → io.Copy to psql container stdin → CloseWrite (EOF)
```

- Connects as per-site user to the per-site database
- Ephemeral `postgres:16` container on `wp_backend`

### Volume Backup & Restore

Uses the same `backupVolumeByName()` / `restoreVolumeByName()` as WordPress, with `N8nVolumeName(site)` resolved by `resolveResourceNames()`. The n8n volume at `/home/node/.n8n` contains custom nodes, local files, and configuration.

- R2 key: `volumes/<site>/YYYY-MM-DD.tar.gz`

### Scheduling

n8n sites are included in the daily `BackupAll()` sweep alongside WordPress sites. `BackupSite()` checks `s.SiteType == "n8n"` and routes to `backupN8nDatabase()` instead of `BackupDatabase()` (MySQL).

---

## 9. Custom Domains

Custom domains work for n8n sites with the same API (`POST /api/sites/:site/domain`) and validation flow as WordPress. The key difference: n8n has no nginx sidecar or wp_options to update.

When `regenerateCaddy()` is called for an n8n site (detected via `existing.SiteType == "n8n"` in `api.go`), it generates the Caddy snippet with `reverse_proxy n8n_<site>:5678` instead of `reverse_proxy nginx_<site>:80`.

Steps skipped for n8n (compared to WordPress custom domain flow):
- No nginx sidecar config update (no nginx sidecar exists)
- No wp_options siteurl/home update (not WordPress)

---

## 10. Naming Conventions

**Source:** `naming.go`

| Function | Pattern | Example (`site = "myflow"`) |
|----------|---------|---------------------------|
| `N8nContainerName(site)` | `n8n_<site>` | `n8n_myflow` |
| `N8nVolumeName(site)` | `n8n_<site>` | `n8n_myflow` |
| `N8nDatabaseName(site)` | `n8n_<site>` | `n8n_myflow` |
| `N8nDatabaseUser(site)` | `n8n_<site>` | `n8n_myflow` |
| `N8nDatabasePass(site)` | `***REMOVED***` | `***REMOVED***` |
| `SiteDomain(site, base)` | `<site>.<base>` | `myflow.cowsaidmoo.tech` |
| `CaddyConfFile(site)` | `<site>.caddy` | `myflow.caddy` |

---

## 11. Configuration

| Field | Env Var | Default | Required |
|-------|---------|---------|----------|
| `N8nImage` | `N8N_IMAGE` | `n8nio/n8n:latest` | No |
| `N8nPGDSN` | `N8N_PG_DSN` | `""` (empty = n8n provisioning disabled) | Yes, for n8n |

The `N8N_PG_DSN` format is `postgres://user:password@host:port/dbname`. It is parsed by `parsePGDSN()` in `pg_backupper.go` (shared utility).

---

## 12. API Endpoints

### Provision

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `POST` | `/api/n8n/provision` | `{"site": "myflow", "user_id": "..."}` | Queue n8n provisioning. Returns 202 with job_id. |

Validation: same as WordPress — site name regex (`^[a-z0-9]+$`), active job check, existing active site check.

### Destroy

| Method | Path | Body | Description |
|--------|------|------|-------------|
| `POST` | `/api/destroy` | `{"site": "myflow"}` | Queue destroy. Auto-detects site type from DB and creates `N8N_DESTROY` job for n8n sites. |

### Other Endpoints (shared with WordPress)

All existing endpoints work for n8n sites:
- `GET /api/sites/:site` — includes `site_type: "n8n"` in response
- `GET /api/sites` — lists all sites with their types
- `GET /api/jobs/:id` — job status polling
- `POST /api/sites/:site/domain` — set custom domain
- `DELETE /api/sites/:site/domain` — remove custom domain
- `GET /api/sites/:site/domain/status` — DNS/cert check
- `POST /api/sites/:site/backup` — manual backup
- `GET /api/sites/:site/backups` — list backups
- `POST /api/sites/:site/restore/:date` — restore from backup

---

## 13. Differences from WordPress Hosting

| Aspect | WordPress | n8n |
|--------|-----------|-----|
| Containers per site | 2 (php + nginx) | 1 (n8n) |
| Database engine | MariaDB on state-01 | PostgreSQL on state-01 |
| Database creation | Direct SQL via Go MySQL driver | Ephemeral `postgres:16` container running `psql` |
| Backup method | `mysqldump` via ephemeral `mariadb:10` | `pg_dump` via ephemeral `postgres:16` |
| Backup credentials | WordPress DSN admin user | Per-site n8n user (owns the tables) |
| Reverse proxy target | `nginx_<site>:80` | `n8n_<site>:5678` |
| nginx sidecar | Yes (serves static files, proxies PHP) | No (n8n serves HTTP directly) |
| Volume mount point | `/var/www/html` | `/home/node/.n8n` |
| Memory limit | 512 MiB (PHP) + 128 MiB (nginx) = 640 MiB | 1 GiB |
| CPU limit | 1.0 (PHP) + 0.5 (nginx) = 1.5 vCPU | 1.0 vCPU |
| Encryption key | None | Per-site, stored in `sites.encryption_key` |
| Custom domain extras | Rewrite nginx config + wp_options | Caddy snippet only |
| Job types | `PROVISION` / `DESTROY` | `N8N_PROVISION` / `N8N_DESTROY` |

---

## 14. Known Issues

1. **PG database not always cleaned up on destroy.** The `dropPGDatabase()` function runs an ephemeral `postgres:16` container to execute `DROP DATABASE`. If a prior failed provisioning attempt left a stale container with a colliding name, the drop container fails to create. The site is still marked DESTROYED and all Docker resources are cleaned up — only the PG database/user may persist. Manual cleanup: `sudo -u postgres dropdb n8n_<site> && sudo -u postgres psql -c "DROP USER IF EXISTS n8n_<site>"`.

***REMOVED***

3. **Encryption key in container env vars.** Anyone who can `docker inspect` the n8n container can see `N8N_ENCRYPTION_KEY`. The Docker API is TLS-protected and only accessible from control-01. The key is also stored in the control plane DB's `sites.encryption_key` column as a recovery path.
