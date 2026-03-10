# Hostplane — Context Summary

> **Last updated: 2026-03-04** | `go build` ✅ | branch: `feat-backup`

## What This Is

Go control-plane binary (`/opt/control/control-plane`) running as a systemd service on **control-01** (LXC on Proxmox). Provisions and destroys WordPress sites on **app-01** via the Docker API over TLS. No Cloudflare Tunnel dependency.

---

## Infrastructure

| Host       | Role                                      | IP              |
| ---------- | ----------------------------------------- | --------------- |
| control-01 | control-plane binary, scripts             | LXC internal    |
| app-01     | Docker host — all site containers + Caddy | ***REMOVED***      |
| state-01   | MariaDB — one DB per site                 | ***REMOVED***      |
| VPS        | nginx TCP forwarder only (dumb, no logic) | ***REMOVED*** |

Wildcard DNS `*.cowsaidmoo.tech → ***REMOVED***` (Cloudflare, orange cloud ON, Full Strict).

---

## Request Path

```
Browser → Cloudflare → VPS nginx (TCP fwd) → WireGuard → Caddy (app-01)
  → reverse_proxy nginx_<site>:80
    → nginx_<site>  (static files from volume + fastcgi_pass to FPM)
      → php_<site>:9000  (WordPress PHP-FPM)
        → MariaDB wp_<site> (state-01)
```

---

## Per-Site Resources

| Resource                | Name                                                 | Where    |
| ----------------------- | ---------------------------------------------------- | -------- |
| MariaDB database + user | `wp_<site>`                                          | state-01 |
| Docker volume           | `wp_<site>`                                          | app-01   |
| PHP-FPM container       | `php_<site>`                                         | app-01   |
| nginx sidecar container | `nginx_<site>`                                       | app-01   |
| nginx server block      | inside `nginx_<site>:/etc/nginx/conf.d/default.conf` | app-01   |
| Caddy snippet           | inside `caddy:/etc/caddy/sites/<site>.caddy`         | app-01   |

---

## Key .env (control-01 `/opt/control/.env`)

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
```

---

## Source Files

| File                    | Role                                                                           |
| ----------------------- | ------------------------------------------------------------------------------ |
| `main.go`               | wires everything, starts worker + HTTP server                                  |
| `api.go`                | HTTP handlers (Gin)                                                            |
| `config.go`             | `LoadConfig()` from env                                                        |
| `db.go`                 | control-plane DB (jobs, sites) + `MigrateSchema`, `UpdateLastBackupAt`         |
| `naming.go`             | all resource name functions — only place names are defined                     |
| `lifecycle.go`          | `SiteStatus` enum + state machine + domain validation                          |
| `provisioner.go`        | 7-step WP site provision + rollback                                            |
| `destroyer.go`          | reverse of provision; gates on `BackupSite` before any destructive step        |
| `static_provisioner.go` | static site provision                                                          |
| `worker.go`             | job poll loop                                                                  |
| `caddy.go`              | `reloadCaddy()`, `ensureCaddyConfDir()`                                        |
| `tunnel.go`             | `TunnelManager` (custom domain routing)                                        |
| `backup.go`             | `R2Client` — Upload, List, DeleteOlderThan, Download, objectExists, helpers    |
| `backupper.go`          | `Backupper` — per-site DB + volume backup and restore via ephemeral containers |
| `backup_worker.go`      | `BackupWorker` — 24 h ticker, immediate first run, weekly cleanup              |

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

- **`health.sh`** — checks service, MySQL, Docker, Caddy, per-site containers (php* + nginx*), smoke test
- **`cleanup.sh`** — clears dead/zombie sites, stuck/pending jobs, orphaned containers/volumes/Caddy snippets
- **`config.sh`** — sets up `/root/.my-hosto.cnf`

## R2 Bucket Layout (`hostplane-backups`)

```
databases/<site>/YYYY-MM-DD.sql.gz   ← daily user DB backups
volumes/<site>/YYYY-MM-DD.tar.gz     ← daily user volume backups
infra/controlplane/YYYY-MM-DDTHH:MM:SSZ.sql.gz  ← hourly controlplane DB
infra/certs/YYYY-MM-DD.tar.gz        ← daily Docker TLS certs
```

---

## Stale `.ai/` files (superseded, kept for git history only)

- `plan.md` — old architectural principles doc
- `lifecycle-refactor.md` — 943-line audit from before nginx-sidecar architecture
- `caddy-manual-provsioning.md` — old manual guide, replaced by `detailed_runbook.md`

---

## Architecture Invariants (Do Not Violate)

1. **DB is the single source of truth.** Nginx, cloudflared, tunnel routes are derived artifacts.
2. **Validate → Apply Infra → Confirm → Commit DB.** Never write DB before infra succeeds.
3. **State transitions must be explicit.** All status changes go through `TransitionSite()` / `CanTransitionTo()`.
4. **Every operation must be idempotent.** Safe to retry without side effects.
5. **Generate, don't patch.** Config files should be fully regenerated from DB, not incrementally modified.
6. **No fire-and-forget.** Every infra mutation must confirm success synchronously.
7. **Rollback on partial failure.** If step N fails, compensate steps 1..N-1.
