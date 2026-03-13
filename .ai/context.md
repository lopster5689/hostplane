# Hostplane — Context Summary

> **Last updated: 2026-03-13** | `go build` ✅ | branch: `main`

## What This Is

Go control-plane binary (`/opt/control/control-plane`) running as a systemd service on **control-01** (LXC on Proxmox). It provisions and destroys WordPress sites on **app-01** via Docker API over TLS, stores per-site data in **state-01** MariaDB, and runs backup pipelines to Cloudflare R2 for:

1. Site MariaDB DB + Docker volume backups
2. Infra backups (controlplane DB + Docker certs)
3. PostgreSQL backup for external droplet `***REMOVED***` (`hosto` DB)

---

## Proxmox Architecture (Detailed)

### Node

- **Proxmox node:** `pve`

### Guests and Sizing

| Guest ID | Name       | Type | Role                                                          | CPU     | RAM     | Disk  | Network |
| -------- | ---------- | ---- | ------------------------------------------------------------- | ------- | ------- | ----- | ------- |
| 101      | control-01 | LXC  | control-plane binary, cron/scripts, backup orchestrator       | 1 core  | 1.5 GiB | 12G   | vmbr1   |
| 200      | app-01     | VM   | Docker host for WordPress containers + Caddy + backup helpers | 6 cores | 16 GiB  | 1000G | vmbr1   |
| 400      | state-01   | VM   | MariaDB host (`controlplane` + per-site `wp_<site>`)          | 2 cores | 8 GiB   | 200G  | vmbr1   |
| 500      | monitor-01 | VM   | monitoring/support host                                       | 2 cores | 2 GiB   | 30G   | vmbr1   |

### External Product Droplet

| Host IP         | Role                            | Containerized services      |
| --------------- | ------------------------------- | --------------------------- |
| `***REMOVED***` | customer-facing product droplet | frontend, backend, postgres |

This droplet is not only a DB host. It runs three separate service containers:

1. customer dashboard frontend container
2. customer dashboard backend API container
3. PostgreSQL container (`hosto` database for user/site data)

Hostplane currently integrates with this droplet through PostgreSQL backup/restore only.

---

## Data/Request Paths

### WordPress Runtime Path

```
Browser
  → Cloudflare
    → VPS nginx TCP forwarder (***REMOVED***)
      → WireGuard
        → Caddy on app-01
          → nginx_<site>:80
            → php_<site>:9000
              → MariaDB on state-01 (wp_<site>)
```

### PostgreSQL Backup Path

```
control-01 (scheduler/API)
  → Docker API TLS to app-01
    → ephemeral postgres:16 container (pg_dump / psql)
      → network (bridge) → ***REMOVED***:5432 (hosto)
        → stream gzip SQL to/from R2
```

---

## Per-Site Resources

| Resource                | Name                                          | Where    |
| ----------------------- | --------------------------------------------- | -------- |
| MariaDB database + user | `wp_<site>`                                   | state-01 |
| Docker volume           | `wp_<site>`                                   | app-01   |
| PHP-FPM container       | `php_<site>`                                  | app-01   |
| nginx sidecar container | `nginx_<site>`                                | app-01   |
| nginx server block      | `nginx_<site>:/etc/nginx/conf.d/default.conf` | app-01   |
| Caddy snippet           | `caddy:/etc/caddy/sites/<site>.caddy`         | app-01   |

---

## Backup Architecture (Current)

### A) Site Backups (WordPress)

- **Code:** `backupper.go` + `backup_worker.go`
- **Frequency:** every 24h (plus manual API trigger)
- **Data:** MariaDB `wp_<site>` + Docker volume `wp_<site>`
- **Restore:** manual API restore by date (`YYYY-MM-DD`)
- **Retention:** cleanup weekly, delete older than 30 days
- **Last backup tracking:** `sites.last_backup_at` via `UpdateLastBackupAt`

### B) Infra Backups

- **Code:** `infra_backupper.go` + `infra_backup_worker.go`
- **Data 1:** `controlplane` DB dump hourly
- **Data 2:** Docker TLS cert directory tar daily
- **Retention:** `infra/controlplane/` 90 days, `infra/certs/` 30 days

### C) PostgreSQL Backups (`hosto`)

- **Code:** `pg_backupper.go` + `pg_backup_worker.go`
- **Source DB:** PostgreSQL container on product droplet `***REMOVED***`, DB `hosto`
- **Manual API:** backup/list/restore endpoints under `/api/pg/*`
- **Auto schedule:** every 24h + immediate run on startup
- **Retention:** weekly cleanup, delete older than 30 days
- **Metadata tracking:** `pg_backup_meta.last_backup_at` + `last_backup_key`

### D) Runtime Limits (Current, Implemented)

Current per-site WordPress runtime limits are implemented in code (`provisioner.go`) at container creation time:

| Container      | Memory limit | CPU limit           | PID limit |
| -------------- | ------------ | ------------------- | --------- |
| `php_<site>`   | 512 MiB      | 1.0 vCPU (NanoCPUs) | 100       |
| `nginx_<site>` | 128 MiB      | 0.5 vCPU (NanoCPUs) | 50        |

These limits are active in Docker HostConfig `Resources` for each newly provisioned WordPress site.

What is **not** covered yet:

- no dynamic per-plan quota model (same limits for all sites)
- no documented hard cap for total WordPress site count on app-01
- no scheduler-level admission control based on host free RAM/CPU
- static site provisioning path does not currently define equivalent container resource limits

---

## Key `.env` (control-01 `/opt/control/.env`)

```
DOCKER_HOST=tcp://***REMOVED***:2376
DOCKER_CERT_DIR=/opt/control/certs
CADDY_CONTAINER=caddy
CADDY_CONF_DIR=/etc/caddy/sites
BASE_DOMAIN=cowsaidmoo.tech
APP_SERVER_IP=***REMOVED***
DOCKER_NETWORK=wp_backend
WP_DSN=control:***REMOVED***@tcp(***REMOVED***:3306)/

R2_ACCOUNT_ID=<cloudflare-account-id>
R2_ACCESS_KEY_ID=<r2-key-id>
R2_SECRET_ACCESS_KEY=<r2-secret>
R2_BUCKET=hostplane-backups
REQUIRE_BACKUP_BEFORE_DESTROY=true

POSTGRES_DSN=postgres://<user>:<pass>@***REMOVED***:5432/hosto
```

---

## Active Source Files (Backup + Core)

| File                     | Role                                                                            |
| ------------------------ | ------------------------------------------------------------------------------- |
| `main.go`                | wiring, workers, HTTP server                                                    |
| `api.go`                 | all HTTP handlers including site backup and `/api/pg/*` endpoints               |
| `config.go`              | env loading (`LoadConfig`) including `POSTGRES_DSN`                             |
| `db.go`                  | control DB + migrations + backup metadata (`last_backup_at`, `pg_backup_meta`)  |
| `backup.go`              | `R2Client` (Upload/List/DeleteOlderThan/Download/objectExists) + stream helpers |
| `backupper.go`           | site backup/restore for MySQL + volumes                                         |
| `backup_worker.go`       | daily site backup worker + weekly cleanup                                       |
| `infra_backupper.go`     | controlplane DB + certs backup/restore helpers                                  |
| `infra_backup_worker.go` | hourly/daily/weekly infra backup scheduler                                      |
| `pg_backupper.go`        | PostgreSQL `hosto` backup + restore to/from R2                                  |
| `pg_backup_worker.go`    | PostgreSQL daily auto-backup + weekly cleanup                                   |

---

## R2 Bucket Layout (`hostplane-backups`)

```
databases/<site>/YYYY-MM-DD.sql.gz
volumes/<site>/YYYY-MM-DD.tar.gz

infra/controlplane/YYYY-MM-DDTHH-MM-SSZ.sql.gz
infra/certs/YYYY-MM-DD.tar.gz

postgres/hosto/YYYY-MM-DDTHH-MM-SSZ.sql.gz
```

---

## Naming Conventions (from `naming.go`)

```
PHPContainerName(site)   → php_<site>
NginxContainerName(site) → nginx_<site>
VolumeName(site)         → wp_<site>
WPDatabaseName(site)     → wp_<site>
WPDatabaseUser(site)     → wp_<site>
WPDatabasePass(site)     → ***REMOVED***
CaddyConfFile(site)      → <site>.caddy
NginxConfFile(site)      → <site>.conf
SiteDomain(site, base)   → <site>.<base>
```

---

## Site Status State Machine

```
CREATED → PROVISIONING → ACTIVE → DESTROYING → DESTROYED
                       ↘ FAILED
ACTIVE → DOMAIN_PENDING → DOMAIN_VALIDATING → DOMAIN_ROUTING → DOMAIN_ACTIVE
```

---

## Deploy

```bash
cd /home/oni/hostplane
go build -o control-plane .
cp control-plane /opt/control/control-plane
systemctl restart control-plane
```

---

## Scripts (`/opt/control/scripts/`)

- `health.sh` — service/MySQL/Docker/Caddy/site-container smoke checks
- `cleanup.sh` — stale job/site/container/volume/snippet cleanup
- `config.sh` — writes `/root/.my-hosto.cnf`

---

## Architecture Invariants (Do Not Violate)

1. **DB is source of truth.** Infra configs are derived artifacts.
2. **Validate → Apply Infra → Confirm → Commit DB.** Never commit state early.
3. **State transitions are explicit.** Use `TransitionSite()` / lifecycle rules.
4. **Everything must be idempotent.** Retries are safe.
5. **Generate, don’t patch.** Rewrite config artifacts from desired state.
6. **No fire-and-forget infra mutations.** Confirm success synchronously.
7. **Rollback on partial failure.** Compensate steps `1..N-1` when step `N` fails.
8. **Backups must stream, not stage on disk.** Use pipe-based uploads/restores.
9. **Per-site runtime limits are part of platform safety.** Any provisioning refactor must preserve or intentionally migrate container CPU/RAM/PID limits.
