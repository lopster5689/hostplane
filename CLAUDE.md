# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Hostplane is a WordPress and static site hosting control plane written in Go. It provisions, manages, backs up, and destroys sites on a distributed Proxmox infrastructure (control-01 LXC → app-01 Docker host + state-01 MariaDB).

## Build & Run

```bash
# Build
go build -o control-plane .

# Lint (no other linter configured)
go vet ./...

# Deploy to control-01
cp control-plane /opt/control/control-plane
systemctl restart control-plane

# No unit tests exist. Smoke tests are in scripts/:
#   scripts/health.sh   — service/MySQL/Docker/Caddy/site-container checks
#   scripts/cleanup.sh  — stale job/site/container/volume cleanup
```

## Architecture

**Single Go binary, single package (no subdirectories).** All `.go` files are in the root `main` package.

### Component Map

| Layer | Files | Role |
|-------|-------|------|
| Entry & config | `main.go`, `config.go` | Startup sequence, env vars, component wiring |
| HTTP API | `api.go` | All Gin REST handlers (API key auth via middleware) |
| Database | `db.go` | MySQL connection, schema migrations, job queue, site/domain CRUD |
| Provisioning | `provisioner.go`, `static_provisioner.go`, `n8n_provisioner.go`, `destroyer.go` | Create/destroy WordPress, static, and n8n sites via Docker API |
| Job queue | `worker.go` | Background FIFO processor polling every 3s, max 3 attempts |
| Site backups | `backup.go`, `backupper.go`, `backup_worker.go` | Daily + manual DB/volume backup to R2, restore |
| Infra backups | `infra_backupper.go`, `infra_backup_worker.go` | Hourly control DB + daily Docker cert backups |
| PG backups | `pg_backupper.go`, `pg_backup_worker.go` | Daily external hosto PostgreSQL backup |
| Domain mgmt | `lifecycle.go`, `caddy.go` | Site status state machine, custom domain Caddy config |
| Naming | `naming.go` | Site/container/volume naming conventions |

### Key Patterns

- **API → Job Queue → Worker**: Provision/destroy requests create a job row; the worker picks it up and runs the provisioner/destroyer with rollback on failure.
- **Streaming backups**: DB dumps and volume tars stream directly from Docker containers through gzip to R2 multipart upload — no temp files on disk.
- **Per-site resources (WordPress)**: Each site gets a MariaDB database (`wp_<site>`), a Docker volume (`wp_<site>`), a PHP-FPM container (`php_<site>`), an nginx sidecar (`nginx_<site>`), and a Caddy snippet (`<site>.caddy`).
- **Per-site resources (n8n)**: Each site gets a PostgreSQL database (`n8n_<site>`), a Docker volume (`n8n_<site>`), a single n8n container (`n8n_<site>` on port 5678) on the dedicated `n8n_backend` Docker network, and a Caddy snippet. No nginx sidecar needed. Per-site encryption key + random DB password stored in control DB. Requires `N8N_PG_DSN` pointing at a PostgreSQL server. ⚠️ **BLOCKER (2026-06-25): no PostgreSQL server is provisioned yet.** state-01 runs MariaDB; the Postgres tier is still an open infra decision (dedicated `pgstate-01` guest vs co-locate on state-01 vs container on app-01 — see `hosto-infra/N8N_PROVISIONER_SPEC.md` §2/§12). Until it exists and `N8N_PG_DSN` is set in the control-plane env, every n8n provision fails fast at `Run` with "N8N_PG_DSN not configured". Caddy must also be on `n8n_backend` (handled by the caddy role compose) or `reverse_proxy n8n_<site>:5678` won't resolve.
- **Static sites**: Files on shared Caddy volume at `/srv/sites/<site>/`, Caddy snippet only — no containers or databases per site.

### Infrastructure

| Host | Role | IP |
|------|------|----|
| control-01 (LXC) | This binary runs here | — |
| app-01 (VM) | Docker host for site containers + Caddy | ***REMOVED*** |
| state-01 (VM) | MariaDB for control DB + per-site DBs | ***REMOVED*** |

Docker connection uses TLS (certs in `DOCKER_CERT_DIR`). Backups go to Cloudflare R2.

## Configuration

All config is via environment variables (loaded from `.env` or systemd `EnvironmentFile`). See `config.go` for the full list. Key vars: `API_PORT`, `API_KEY`, `CONTROL_DSN`, `WP_DSN`, `DOCKER_HOST`, `DOCKER_CERT_DIR`, `BASE_DOMAIN`, `R2_*`.

## Documentation

Detailed architecture docs, API specs, and implementation plans live in `.ai/`. The definitive reference is `.ai/ARCHITECTURE.md`.
