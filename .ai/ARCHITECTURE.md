# Hostplane — Complete Architecture Reference

> **Last updated:** 2026-03-13 | **Branch:** `main` | **Module:** `control-plane` | **Go:** 1.24.0

---

## Table of Contents

1. [Product Overview](#1-product-overview)
2. [Physical Infrastructure](#2-physical-infrastructure)
3. [Network Architecture](#3-network-architecture)
4. [Request Path (Runtime)](#4-request-path-runtime)
5. [Control Plane Binary](#5-control-plane-binary)
6. [Per-Site Resource Model](#6-per-site-resource-model)
7. [Site Provisioning Flow (WordPress)](#7-site-provisioning-flow-wordpress)
8. [Static Site Provisioning Flow](#8-static-site-provisioning-flow)
9. [Site Destroy Flow](#9-site-destroy-flow)
10. [Custom Domain Flow](#10-custom-domain-flow)
11. [Job Queue System](#11-job-queue-system)
12. [Site Status State Machine](#12-site-status-state-machine)
13. [Backup System](#13-backup-system)
14. [API Reference](#14-api-reference)
15. [Database Schema](#15-database-schema)
16. [Configuration Reference](#16-configuration-reference)
17. [Naming Conventions](#17-naming-conventions)
18. [Docker Container Specifications](#18-docker-container-specifications)
19. [File System Layout](#19-file-system-layout)
20. [R2 Bucket Layout](#20-r2-bucket-layout)
21. [Monitoring](#21-monitoring)
22. [Operational Scripts](#22-operational-scripts)
23. [Architecture Invariants](#23-architecture-invariants)
24. [Migration History](#24-migration-history)
25. [Outstanding Work / Future](#25-outstanding-work--future)
26. [Dependencies](#26-dependencies)
27. [WP-CLI Execution](#27-wp-cli-execution-wp_execgo)

---

## 1. Product Overview

Hostplane is a WordPress and static-site hosting platform. Customers interact with a dashboard frontend at `hosto.app`, which talks to a backend API that in turn calls the Hostplane control plane to provision, manage, and destroy sites.

The system is split into two planes:

| Plane                    | Where                                | What                                                                                                                                   |
| ------------------------ | ------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------- |
| **Product plane**        | DigitalOcean droplet `***REMOVED***` | Customer dashboard (frontend), business logic API (backend), PostgreSQL (`hosto` DB for user/site records)                             |
| **Infrastructure plane** | Proxmox homelab + DigitalOcean VPS   | Control plane binary, Docker host for WordPress/static containers, MariaDB for per-site databases, Caddy reverse proxy, backup workers |

The product backend authenticates to the control plane API using a shared `X-API-Key` header and issues provisioning/destroy requests on behalf of authenticated customers.

---

## 2. Physical Infrastructure

### 2.1 Proxmox Host (`pve`)

Home server running Proxmox VE. This is the single master node today; the architecture anticipates future expansion to a multi-node topology (one strong master + multiple efficient child nodes).

| Attribute   | Value                                    |
| ----------- | ---------------------------------------- |
| **CPU**     | AMD Ryzen 7 5700G (8 cores / 16 threads) |
| **RAM**     | 27.30 GiB                                |
| **Disk**    | ~3.47 TiB total                          |
| **Boot**    | EFI (Secure Boot)                        |
| **Kernel**  | Linux 6.17.2-1-pve                       |
| **Network** | vmbr1 (private bridge, ***REMOVED***/24)     |

### 2.2 Proxmox Guests

| Guest ID | Name       | Type | Role                                                                               | CPU     | RAM     | Disk    | IP         |
| -------- | ---------- | ---- | ---------------------------------------------------------------------------------- | ------- | ------- | ------- | ---------- |
| 101      | control-01 | LXC  | Control plane binary, backup orchestrator                                          | 1 core  | 1.5 GiB | 12 GB   | 10.10.0.?  |
| 200      | app-01     | VM   | Docker host: WordPress containers, Caddy, nginx sidecars, backup helper containers | 6 cores | 16 GiB  | 1000 GB | ***REMOVED*** |
| 400      | state-01   | VM   | MariaDB: `controlplane` DB + per-site `wp_<site>` databases                        | 2 cores | 8 GiB   | 200 GB  | ***REMOVED*** |
| 500      | monitor-01 | VM   | Prometheus + Grafana monitoring stack                                              | 2 cores | 2 GiB   | 30 GB   | 10.10.0.?  |

All guests are on the **vmbr1** private bridge with static IPs. No DHCP. MariaDB on state-01 is firewall-restricted to only control-01 and app-01.

### 2.3 External Hosts

| Host                  | Provider     | IP                | Role                                           |
| --------------------- | ------------ | ----------------- | ---------------------------------------------- |
| VPS (ingress gateway) | DigitalOcean | `***REMOVED***` | nginx TCP forwarder + WireGuard peer           |
| Product droplet       | DigitalOcean | `***REMOVED***`   | Frontend, backend API, PostgreSQL (`hosto` DB) |

---

## 3. Network Architecture

### 3.1 Internal Network (vmbr1)

```
Proxmox host (pve)
├── vmbr1 (private bridge, ***REMOVED***/24, static IPs)
│   ├── control-01 (LXC)    — control plane binary
│   ├── app-01 (VM)          — Docker host, WireGuard peer
│   ├── state-01 (VM)        — MariaDB (firewall: only control-01 + app-01)
│   └── monitor-01 (VM)      — Prometheus + Grafana
```

### 3.2 WireGuard Tunnel

```
DigitalOcean VPS (***REMOVED***)  ←— WireGuard encrypted tunnel —→  app-01 (***REMOVED***)
```

The WireGuard peer runs directly on app-01. The VPS and app-01 communicate over an encrypted tunnel. The VPS never sees plaintext traffic — it forwards raw TCP bytes.

### 3.3 Docker Networks (on app-01)

| Network            | Purpose                                        | Members                                                       |
| ------------------ | ---------------------------------------------- | ------------------------------------------------------------- |
| `wp_backend`       | WordPress container communication              | Caddy, all `nginx_<site>`, all `php_<site>` containers        |
| `bridge` (default) | Outbound internet access for backup containers | Ephemeral `postgres:16` containers that reach `***REMOVED***` |

Caddy resolves `nginx_<site>` container names via Docker's built-in DNS on the `wp_backend` network.

### 3.4 Control Plane → Docker API

control-01 connects to app-01's Docker daemon over **mutual TLS** (TCP port 2376):

```
control-01 (/opt/control/certs/{ca,cert,key}.pem)
    → tcp://***REMOVED***:2376 (Docker Engine API v1.44)
```

All container lifecycle operations (create, start, stop, remove, exec, attach, copy) go through this TLS connection.

---

## 4. Request Path (Runtime)

### 4.1 WordPress Site Request

```
Customer browser
  → Cloudflare (DDoS protection, HTTPS termination, hides VPS IP)
    → VPS nginx (***REMOVED***) — dumb TCP stream forwarder, no TLS, no logic
      → WireGuard encrypted tunnel
        → app-01 Caddy (***REMOVED***, ports 80/443)
          → nginx_<site>:80 (serves static files, proxies PHP)
            → php_<site>:9000 (WordPress PHP-FPM)
              → state-01 MariaDB (***REMOVED***, database wp_<site>)
```

### 4.2 Static Site Request

```
Customer browser
  → Cloudflare → VPS nginx → WireGuard
    → app-01 Caddy
      → file_server (serves directly from /srv/sites/<site>/ on caddy_static_sites volume)
```

No nginx sidecar. No PHP. Caddy serves static files directly.

### 4.3 Control Plane API Request

```
Product backend (***REMOVED***)
  → api.cowsaidmoo.tech
    → Cloudflare → VPS → WireGuard
      → Caddy → control-01 (LXC, port 8080)
        → Gin HTTP handler (X-API-Key auth)
```

### 4.4 PostgreSQL Backup Path

```
control-01 (scheduler or API trigger)
  → Docker API (TLS) → app-01
    → ephemeral postgres:16 container (bridge network)
      → ***REMOVED***:5432 (hosto DB)
        → pg_dump stdout → stripDockerMux → gzip → R2 upload (streaming)
```

### 4.5 Prometheus Monitoring Path

```
monitor-01 (Prometheus)
  → scrapes node_exporter on app-01, state-01, control-01
```

---

## 5. Control Plane Binary

### 5.1 Overview

A single Go binary (`control-plane`) compiled from module `control-plane`, running as a systemd service on control-01. It is the sole orchestrator of all infrastructure mutations.

**Source:** `/home/oni/hosto/hostplane/` (development machine)
**Deployed to:** `/opt/control/control-plane` (control-01)

### 5.2 Startup Sequence (`main.go`)

```
1.  LoadConfig()                          — read all env vars
2.  NewDB(cfg.ControlDSN)                — open MySQL, ping, fatal on failure
3.  db.MigrateSchema()                   — ALTER TABLE sites ADD last_backup_at (non-fatal)
4.  db.MigratePGSchema()                 — CREATE TABLE pg_backup_meta (non-fatal)
5.  Docker TLS client                     — connect to app-01:2376, fatal on failure
6.  docker.Info() with 5s timeout         — verify Docker reachable, fatal on failure
7.  NewR2Client(cfg)                      — soft failure: backups disabled if R2 not configured
8.  Wire components:
    a. TunnelManager
    b. Provisioner
    c. StaticProvisioner
    d. Backupper
    e. PGBackupper
    f. Destroyer (takes Backupper for pre-destroy backup)
    g. Worker (job queue processor)
9.  go worker.Start()                     — background job processor (3s poll)
10. if r2 != nil:
    a. go BackupWorker.Start()            — daily site backups
    b. go InfraBackupWorker.Start()       — hourly DB + daily certs
    c. if PostgresDSN != "":
       go PGBackupWorker.Start()          — daily PG backups
11. Gin HTTP server on cfg.APIPort        — 10s read/write timeouts
12. api.RegisterRoutes(router)
13. Goroutine: srv.ListenAndServe()
14. Wait for SIGTERM/SIGINT
15. srv.Shutdown() with 15s timeout
```

### 5.3 Component Graph

```
main.go
├── Config (config.go)
├── DB (db.go)                         — controlplane MariaDB connection
├── Docker client                      — TLS to app-01
├── R2Client (backup.go)               — Cloudflare R2 S3-compatible client
│
├── Worker (worker.go)                 — polls job queue every 3s
│   ├── Provisioner (provisioner.go)   — WordPress site provisioning
│   ├── StaticProvisioner (static_provisioner.go) — static site provisioning
│   └── Destroyer (destroyer.go)       — site teardown
│
├── BackupWorker (backup_worker.go)    — schedules daily site backups
│   └── Backupper (backupper.go)       — site backup/restore logic
│
├── InfraBackupWorker (infra_backup_worker.go)
│   └── InfraBackupper (infra_backupper.go) — controlplane DB + Docker certs
│
├── PGBackupWorker (pg_backup_worker.go)
│   └── PGBackupper (pg_backupper.go)  — PostgreSQL hosto DB
│
├── TunnelManager (tunnel.go)          — cloudflared config (legacy, wired but unused)
│
└── API (api.go)                       — Gin HTTP handlers
    ├── DB
    ├── Docker client
    ├── TunnelManager
    ├── Backupper
    └── PGBackupper
```

---

## 6. Per-Site Resource Model

### 6.1 WordPress Site Resources

Each WordPress site consists of six resources across two hosts:

| Resource                | Name Pattern                | Location                                               | Created By                           |
| ----------------------- | --------------------------- | ------------------------------------------------------ | ------------------------------------ |
| MariaDB database        | `wp_<site>`                 | state-01 (***REMOVED***)                                  | Provisioner (SQL)                    |
| MariaDB user            | `wp_<site>` / `***REMOVED***` | state-01 (***REMOVED***)                                  | Provisioner (SQL)                    |
| Docker volume           | `wp_<site>`                 | app-01                                                 | Provisioner (Docker API)             |
| PHP-FPM container       | `php_<site>`                | app-01                                                 | Provisioner (Docker API)             |
| nginx sidecar container | `nginx_<site>`              | app-01                                                 | Provisioner (Docker API)             |
| Caddy snippet           | `<site>.caddy`              | app-01 (inside Caddy container at `/etc/caddy/sites/`) | Provisioner (Docker CopyToContainer) |

### 6.2 Static Site Resources

| Resource               | Name Pattern         | Location                           | Created By                                    |
| ---------------------- | -------------------- | ---------------------------------- | --------------------------------------------- |
| Files on shared volume | `/srv/sites/<site>/` | app-01 (caddy_static_sites volume) | StaticProvisioner (busybox + CopyToContainer) |
| Caddy snippet          | `<site>.caddy`       | app-01 (inside Caddy container)    | StaticProvisioner (Docker CopyToContainer)    |

No database. No sidecar containers. Caddy serves directly via `file_server`.

### 6.3 Runtime Limits (WordPress)

Enforced via Docker `HostConfig.Resources` at container creation:

| Container      | Memory  | CPU (NanoCPUs)           | PID Limit |
| -------------- | ------- | ------------------------ | --------- |
| `php_<site>`   | 512 MiB | 1,000,000,000 (1.0 vCPU) | 100       |
| `nginx_<site>` | 128 MiB | 500,000,000 (0.5 vCPU)   | 50        |

Static sites have no dedicated containers and therefore no per-site resource limits.

### 6.4 Why nginx Sidecar (Not Caddy-Direct FastCGI)

Caddy can proxy FastCGI to PHP-FPM, but it would need every `wp_<site>` volume mounted directly into the Caddy container. With N sites that means:

- N volume mounts on a single container
- Caddy restart required for each new mount
- Breaks per-site isolation

The nginx sidecar model gives each site its own nginx container with its own single volume mount. Caddy only does hostname-based reverse proxying and never touches PHP or WordPress files. Adding a new site never requires changes to the Caddy container itself.

---

## 7. Site Provisioning Flow (WordPress)

**Entry:** `Provisioner.Run(site string) error` (`provisioner.go`)

8-step flow with full rollback on failure:

```
Step 1: Create MySQL database + user on state-01
        → CREATE DATABASE IF NOT EXISTS wp_<site>
        → CREATE USER IF NOT EXISTS 'wp_<site>'@'%' IDENTIFIED BY '***REMOVED***'
        → GRANT ALL PRIVILEGES ON wp_<site>.* TO 'wp_<site>'@'%'
        → FLUSH PRIVILEGES

Step 2: Create Docker volume wp_<site> on app-01

Step 3: Start php_<site> container
        → Image: wordpress:php8.2-fpm
        → Volume: wp_<site> → /var/www/html
        → Network: wp_backend
        → Env: WORDPRESS_DB_HOST, WORDPRESS_DB_USER, WORDPRESS_DB_PASSWORD, WORDPRESS_DB_NAME
        → Limits: 512MB RAM, 1 CPU, 100 PIDs
        → RestartPolicy: unless-stopped
        → Idempotent: if container exists, just starts it

Step 4: Start nginx_<site> container
        → Image: nginx:alpine
        → Volume: wp_<site> → /var/www/html (read-only)
        → Network: wp_backend
        → Limits: 128MB RAM, 0.5 CPU, 50 PIDs
        → RestartPolicy: unless-stopped
        → Idempotent: if container exists, just starts it

Step 5: Write nginx config into nginx_<site> container
        → Generates server block (listen 80, root /var/www/html, fastcgi_pass php_<site>:9000)
        → Creates tar archive with /etc/nginx/conf.d/default.conf
        → CopyToContainer into nginx_<site>
        → Exec: nginx -s reload

Step 6: Write Caddy snippet into Caddy container
        → Generates: <site>.<baseDomain> { encode gzip; reverse_proxy nginx_<site>:80 }
        → Ensures /etc/caddy/sites/ dir exists (mkdir -p via exec)
        → Creates tar archive with <site>.caddy
        → CopyToContainer into Caddy

Step 7: Reload Caddy
        → docker exec caddy caddy reload --config /etc/caddy/Caddyfile
        → Zero-downtime reload

Step 8: Poll TLS certificate (up to 30s, non-fatal)
        → Checks /data/caddy/certificates/acme-v02.api.letsencrypt.org-directory/<domain>/<domain>.crt
        → Returns CertIssued or CertPending
```

### Rollback

On failure at step N, steps 1..(N-1) are undone in reverse:

```
Undo Step 6: rm -f Caddy snippet + reload Caddy
Undo Step 5: (covered by removing nginx container)
Undo Step 4: Force-remove nginx_<site> container
Undo Step 3: Force-remove php_<site> container
Undo Step 2: Force-remove wp_<site> volume
Undo Step 1: DROP DATABASE IF EXISTS wp_<site> + DROP USER IF EXISTS 'wp_<site>'
```

### DNS

No DNS changes needed. Wildcard DNS `*.cowsaidmoo.tech` is preconfigured at Cloudflare, so any new subdomain resolves instantly.

---

## 8. Static Site Provisioning Flow

**Entry:** `StaticProvisioner.Run(site, zipPath string) error` (`static_provisioner.go`)

4-step flow with rollback:

```
Step 1: Upload zip contents to caddy_static_sites volume
        → Create temporary busybox container with caddy_static_sites mounted at /data
        → Convert zip to tar (prefix all paths with <site>/)
        → CopyToContainer to /data/ → files land at /data/<site>/ = /srv/sites/<site>/ in Caddy
        → Remove temporary container

Step 2: Write Caddy config snippet
        → Generates: <site>.<baseDomain> { root * /srv/sites/<site>; file_server; encode gzip }
        → CopyToContainer into Caddy at /etc/caddy/sites/<site>.caddy

Step 3: Reload Caddy (zero-downtime)

Step 4: Poll TLS certificate (up to 30s, non-fatal)
        → Cleanup: remove /tmp/<site>.zip from control-01
```

### Rollback

```
Undo Step 2: rm -f Caddy snippet via exec
Undo Step 1: busybox container runs rm -rf /data/<site> on caddy_static_sites volume
```

### Zip-to-Tar Conversion (`zipToTar`)

The uploaded zip is converted in-memory to a tar archive with all file paths prefixed by `<site>/`. This ensures proper directory structure inside the shared volume.

---

## 9. Site Destroy Flow

**Entry:** `Destroyer.Run(site string) error` (`destroyer.go`)

6-step sequential teardown with **no rollback** (destroy is a one-way operation):

```
Step 1: Pre-destroy backup (conditional)
        → If cfg.RequireBackupBeforeDestroy == true (default):
          → Calls backupper.BackupSite(site)
          → If backup fails → ABORT entire destroy ("pre-destroy backup failed")

Step 2: Force-remove php_<site> container
        → Ignores "not found" errors

Step 3: Force-remove nginx_<site> container
        → Ignores "not found" errors

Step 4: Force-remove wp_<site> Docker volume
        → Ignores "not found" errors

Step 5: Remove Caddy snippet + reload Caddy
        → rm -f <site>.caddy via exec in Caddy container
        → Reload Caddy

Step 6: Drop MySQL database + user
        → DROP DATABASE IF EXISTS wp_<site>
        → DROP USER IF EXISTS 'wp_<site>'
```

The pre-destroy backup ensures data recovery is possible even after a full teardown.

---

## 10. Custom Domain Flow

### 10.1 Setting a Custom Domain

**Endpoint:** `POST /api/sites/:site/domain` with `{"domain": "example.com"}`

```
Validation:
  1. Domain format check (regex, length, no wildcards)
  2. Domain must not be a subdomain of BaseDomain
  3. DNS resolution check — domain must resolve to PublicIP (***REMOVED***)
  4. Domain availability — no other non-DESTROYED/FAILED site claims this domain
  5. Site must be ACTIVE or DOMAIN_ACTIVE

Infra mutations (infra first, DB last):
  1. [WordPress only] Rewrite nginx sidecar config with both domains (server_name)
     → Uses $host for HTTP_HOST when multi-domain
     → CopyToContainer + nginx -s reload
  2. Regenerate Caddy snippet with both hostnames:
     → "<site>.<baseDomain>, example.com { ... }"
     → Reload Caddy
  3. [WordPress only, non-fatal] Update wp_options siteurl/home to https://example.com
  4. Commit to DB: db.SetCustomDomain(site, domain)

Post-commit:
  → Poll Caddy cert for custom domain (up to 30s)
  → Return cert_status (issued/pending)

Rollback on partial failure:
  → If Caddy update fails: revert nginx to single-domain config
  → If nginx update fails: return error (no Caddy changes made yet)
```

### 10.2 Removing a Custom Domain

**Endpoint:** `DELETE /api/sites/:site/domain`

Reverse of set:

1. Revert nginx to single-domain config
2. Regenerate Caddy snippet with only default domain
3. Revert wp_options to default domain URL
4. Clear custom_domain in DB

### 10.3 Domain Status Check

**Endpoint:** `GET /api/sites/:site/domain/status`

Returns live checks:

- DNS resolution: does domain resolve to PublicIP?
- Cert status: does Caddy have a valid cert?
- Step indicator: `pending_dns`, `pending_cert`, or `active`

### 10.4 Cert Retry

**Endpoint:** `POST /api/sites/:site/cert-retry`

Forces a Caddy reload and polls cert for 30s. Useful when DNS propagated after initial domain setup.

---

## 11. Job Queue System

### 11.1 Overview

MySQL-backed asynchronous job queue with at-least-once delivery. The Worker polls every 3 seconds.

### 11.2 Job Types

| Type                  | Value              | Handler                                |
| --------------------- | ------------------ | -------------------------------------- |
| WordPress provision   | `PROVISION`        | `provisioner.Run(site)`                |
| Site destroy          | `DESTROY`          | `destroyer.Run(site)`                  |
| Static site provision | `STATIC_PROVISION` | `staticProvisioner.Run(site, zipPath)` |

### 11.3 Job Lifecycle

```
API handler receives request
  → Validates (no active jobs for site, valid site name, etc.)
  → Inserts job: status=PENDING, attempts=0, max_attempts=3
  → Returns 202 with job_id

Worker.processNext() [every 3s]:
  → ClaimNextJob() via SELECT ... FOR UPDATE SKIP LOCKED
    → Only selects WHERE status='PENDING' AND attempts < max_attempts
  → Atomically sets status=PROCESSING, increments attempts
  → Dispatches to appropriate handler based on job.Type
  → On success: CompleteJob() → job=COMPLETED, site=ACTIVE (or DESTROYED)
  → On failure: RetryJob() → job=PENDING (back in queue with error message)
```

### 11.4 Retry Exhaustion

Max attempts is 3. The retry exhaustion is enforced by the SQL `WHERE attempts < max_attempts` clause in `ClaimNextJob`, not by a Go-side check. After 3 failed attempts, the DB has `attempts=3` and `max_attempts=3`, so `3 < 3 = false` — the job will never be claimed again. It remains in `PENDING` status with the last error message.

There is a Go-side `job.Attempts >= job.MaxAttempts` check in `worker.go` that would call `FailJob()`, but it is effectively unreachable because `job.Attempts` holds the pre-increment value from the SELECT. This means exhausted jobs stay in PENDING (not FAILED) and the associated site also stays in its pre-failure status rather than being transitioned to FAILED.

### 11.5 Stuck Job Recovery

On Worker startup, `RecoverStuckJobs(timeoutMinutes=10)` resets any PROCESSING jobs older than 10 minutes back to PENDING. This handles cases where the control plane crashes mid-provision.

### 11.6 Concurrency Model

`SELECT ... FOR UPDATE SKIP LOCKED` ensures:

- Only one worker claims a job at a time
- No blocking on locked rows
- Safe for future multi-instance deployment

Currently single-worker (single binary), but the queue design supports horizontal scaling.

---

## 12. Site Status State Machine

**Source:** `lifecycle.go`

### 12.1 Status Values

| Status              | Meaning                                    |
| ------------------- | ------------------------------------------ |
| `CREATED`           | Initial record, no provisioning attempted  |
| `PROVISIONING`      | Job is running the provisioner             |
| `ACTIVE`            | Site is live and serving traffic           |
| `DOMAIN_PENDING`    | Custom domain requested, not yet validated |
| `DOMAIN_VALIDATING` | DNS validation in progress                 |
| `DOMAIN_ROUTING`    | Infra being updated for custom domain      |
| `DOMAIN_ACTIVE`     | Custom domain active with cert             |
| `DOMAIN_REMOVING`   | Custom domain being removed                |
| `DESTROYING`        | Destroy job is running                     |
| `DESTROYED`         | Site fully torn down                       |
| `FAILED`            | Provisioning or destroy failed             |

### 12.2 Transition Map

```
CREATED           → PROVISIONING
PROVISIONING      → ACTIVE, FAILED
ACTIVE            → DOMAIN_PENDING, DESTROYING
DOMAIN_PENDING    → DOMAIN_VALIDATING, ACTIVE
DOMAIN_VALIDATING → DOMAIN_ROUTING, DOMAIN_PENDING, ACTIVE
DOMAIN_ROUTING    → DOMAIN_ACTIVE, ACTIVE
DOMAIN_ACTIVE     → DOMAIN_REMOVING, DESTROYING
DOMAIN_REMOVING   → ACTIVE, FAILED
DESTROYING        → DESTROYED, FAILED
FAILED            → PROVISIONING, DESTROYING
```

### 12.3 Guard Methods

| Method                 | Returns true for              |
| ---------------------- | ----------------------------- |
| `IsTerminal()`         | DESTROYED, FAILED             |
| `AllowsCustomDomain()` | ACTIVE                        |
| `AllowsDestroy()`      | ACTIVE, DOMAIN_ACTIVE, FAILED |

---

## 13. Backup System

### 13.1 Overview

Three independent backup subsystems, all streaming to Cloudflare R2 (S3-compatible). No intermediate disk writes on control-01.

### 13.2 R2 Client (`backup.go`)

```go
type R2Client struct {
    s3       *s3.Client
    uploader *manager.Uploader  // 10MB multipart chunks
    bucket   string             // "hostplane-backups"
}
```

**Endpoint:** `https://<R2AccountID>.r2.cloudflarestorage.com`
**Auth:** Static credentials (access key ID + secret)
**Upload:** Streaming multipart via `manager.Uploader` (no size limit, no in-memory buffering)
**Download:** Returns `io.ReadCloser` for streaming reads

### 13.3 Subsystem A: Site Backups (WordPress)

**Code:** `backupper.go` + `backup_worker.go`

#### Backup

| Data           | Method                 | Container                                  | R2 Key                               |
| -------------- | ---------------------- | ------------------------------------------ | ------------------------------------ |
| MySQL database | `BackupDatabase(site)` | Ephemeral `mariadb:10` on `wp_backend`     | `databases/<site>/YYYY-MM-DD.sql.gz` |
| Docker volume  | `BackupVolume(site)`   | Ephemeral `alpine:latest` (vol mounted RO) | `volumes/<site>/YYYY-MM-DD.tar.gz`   |
| n8n PG database| `backupN8nDatabase(site)` | Ephemeral `postgres:16` on `wp_backend` | `databases/<site>/YYYY-MM-DD.sql.gz` |

**Database backup streaming pipeline:**

```
mysqldump (in container)
  → ContainerAttach (stdout)
    → stripDockerMux (remove 8-byte mux headers, forward only stdout frames)
      → goroutine: gzip.NewWriter + io.Pipe
        → r2.Upload (streaming multipart)
```

**Volume backup streaming pipeline:**

```
tar -czf - -C /data . (in container, already gzip)
  → ContainerAttach (stdout)
    → stripDockerMux
      → r2.Upload (direct, no extra gzip)
```

**mysqldump flags:** `--single-transaction` (consistent InnoDB snapshot), `--quick` (row-by-row, no buffering)

**Exit code verification:** After upload, checks container exit code. Non-zero → deletes the corrupt R2 object + logs up to 4KB of stderr.

#### Restore

| Data           | Method                             | Container                                            |
| -------------- | ---------------------------------- | ---------------------------------------------------- |
| MySQL database | `RestoreDatabase(ctx, site, date)` | Ephemeral `mariadb:10`, SQL piped to stdin           |
| Docker volume  | `RestoreVolume(ctx, site, date)`   | Ephemeral `alpine:latest`, tar.gz piped to stdin     |
| n8n PG database| `restoreN8nDatabase(ctx, site, date)` | Ephemeral `postgres:16`, SQL piped to psql stdin  |

**Database restore streaming:**

```
r2.Download → gzip.NewReader → io.Copy(containerStdin, decompressed) → CloseWrite (EOF)
```

**Volume restore streaming:**

```
r2.Download → io.Copy(containerStdin, raw .tar.gz) → CloseWrite (EOF)
  (tar -xzf handles decompression internally)
```

**Preflight:** `RestoreSite` checks both DB and volume objects exist in R2 before any destructive work.

**Note:** Running containers are NOT stopped before restore.

#### Scheduling

| Event                    | Frequency                             | Details                                |
| ------------------------ | ------------------------------------- | -------------------------------------- |
| Full backup of all sites | Every 24 hours + immediate on startup | Skips DESTROYED, FAILED, CREATED sites |
| Cleanup                  | Every daily tick                      | Deletes objects older than 3 days      |
| `last_backup_at` updated | After each successful site backup     | Stored in `sites` table                |

`BackupAll()`: One site failure never stops other sites. Logs final ok/failed counts.

**n8n sites:** Detected via `SiteType == "n8n"`. Uses `pg_dump` (in `postgres:16` container) against the N8n PostgreSQL DSN instead of `mysqldump`. Volume backup uses `n8n_<site>` volume name.

**Static sites:** Skipped entirely (no database or volume to back up).

### 13.4 Subsystem B: Infrastructure Backups

**Code:** `infra_backupper.go` + `infra_backup_worker.go`

| Data                                     | Schedule | Container                           | R2 Key                                           | Retention |
| ---------------------------------------- | -------- | ----------------------------------- | ------------------------------------------------ | --------- |
| Controlplane DB (`controlplane`)         | Hourly   | Ephemeral `mariadb:10` on `wp_backend` | `infra/controlplane/YYYY-MM-DDTHH-MM-SSZ.sql.gz` | 90 days   |
| Docker TLS certs (`/opt/control/certs/`) | Daily    | None (local filesystem)             | `infra/certs/YYYY-MM-DD.tar.gz`                  | 30 days   |

**DB backup credentials:** Parsed from `ControlDSN` (not `WordPressDSN`).

**Cert backup:** Reads from `DockerCertDir` (default `/opt/control/certs`). Uses `filepath.Walk` + `tar.NewWriter` + `gzip.NewWriter` → `io.Pipe` → R2. Idempotent: skips upload if today's archive already exists.

**Timestamp granularity:** Infra DB backups use full timestamps (`YYYY-MM-DDTHH-MM-SSZ`), allowing multiple backups per day. Cert backups use daily granularity.

**Cleanup:** Weekly (7-day ticker in its own select loop).

### 13.5 Subsystem C: PostgreSQL Backups (hosto DB)

**Code:** `pg_backupper.go` + `pg_backup_worker.go`

**What:** Backs up the PostgreSQL database on the product droplet (`***REMOVED***`). This is the `hosto` database containing user accounts and site metadata for the customer-facing product.

| Data                  | Schedule                     | Container                                   | R2 Key                                          | Retention |
| --------------------- | ---------------------------- | ------------------------------------------- | ----------------------------------------------- | --------- |
| `hosto` PostgreSQL DB | Daily + immediate on startup | Ephemeral `postgres:16` on `bridge` network | `postgres/<dbname>/YYYY-MM-DDTHH-MM-SSZ.sql.gz` | 30 days   |

**Why bridge network:** The PG server is external (***REMOVED***). Docker's default `bridge` network provides outbound internet access, unlike `wp_backend` which is an internal-only network.

**pg_dump flags:** `-F p` (plain SQL format), `--no-password` (password via `PGPASSWORD` env var).

**Metadata tracking:** On success, records `last_backup_at` and `last_backup_key` in the `pg_backup_meta` table (on the controlplane MariaDB, not on the PG server itself).

**Restore note:** The target database must already exist. `pg_dump -F p` does not include `CREATE DATABASE`.

**Restore key lookup:** Because keys include full timestamps, restore uses the date as a prefix and takes the first match from R2 listing.

### 13.6 Docker Mux Stripping (`stripDockerMux`)

All backup operations that attach to container stdout must strip Docker's multiplexed stream format:

```
[stream_type (1 byte)] [padding (3 bytes)] [payload_size (4 bytes, big-endian)] [payload]
```

- `stream_type == 1` (stdout): forwarded to the pipe writer
- `stream_type == 2` (stderr): discarded via `io.CopyN(io.Discard, ...)`
- Clean EOF: `pw.Close()` so downstream sees normal EOF
- Error: `pw.CloseWithError(err)`

This is a shared function in `backup.go` used by all three backup subsystems.

### 13.7 Container Lifecycle (All Backup Operations)

All ephemeral backup/restore containers follow the same pattern:

1. `AutoRemove: false` — explicit cleanup control
2. `ContainerAttach` **before** `ContainerStart` — no race on stdout
3. `defer` block: force-remove container with a fresh 15-second context
4. Exit code checked via `waitContainer` using `ContainerWait(WaitConditionNotRunning)`
5. On non-zero exit: log stderr (up to 4KB), delete any uploaded R2 object

### 13.8 Duplicated Code Note

`waitContainer` and `logContainerStderr` methods are duplicated across `Backupper`, `InfraBackupper`, and `PGBackupper` with identical logic. Only `stripDockerMux` is a shared function.

---

## 14. API Reference

**Framework:** Gin v1.11.0
**Auth:** `X-API-Key` header on all routes except `/api/health`
**Base path:** `/api`

### 14.1 Site Lifecycle

| Method   | Path                    | Auth | Description                                                                            |
| -------- | ----------------------- | ---- | -------------------------------------------------------------------------------------- |
| `POST`   | `/api/provision`        | Yes  | Queue WordPress provisioning. Body: `{"site": "..."}`. Returns 202 with job_id.        |
| `POST`   | `/api/static/provision` | Yes  | Queue static site provisioning. Multipart: `site` (field) + `zip` (file). Returns 202. |
| `POST`   | `/api/destroy`          | Yes  | Queue site destroy. Body: `{"site": "..."}`. Returns 202 with job_id.                  |
| `GET`    | `/api/jobs/:id`         | Yes  | Poll job status. Returns job record.                                                   |
| `DELETE` | `/api/jobs/:id`         | Yes  | Hard-delete a non-active job.                                                          |
| `GET`    | `/api/sites`            | Yes  | List all sites (ordered by created_at DESC).                                           |
| `GET`    | `/api/sites/:site`      | Yes  | Get site info + live cert/snippet checks for ACTIVE sites. Returns warnings array.     |
| `DELETE` | `/api/sites/:site`      | Yes  | Hard-delete a DESTROYED site record from DB.                                           |

### 14.2 Custom Domains

| Method   | Path                             | Auth | Description                                                                             |
| -------- | -------------------------------- | ---- | --------------------------------------------------------------------------------------- |
| `POST`   | `/api/sites/:site/domain`        | Yes  | Set custom domain. Body: `{"domain": "..."}`. Validates DNS, updates infra, polls cert. |
| `DELETE` | `/api/sites/:site/domain`        | Yes  | Remove custom domain. Reverts infra then clears DB.                                     |
| `GET`    | `/api/sites/:site/domain/status` | Yes  | Live DNS check + cert check. Returns step: pending_dns/pending_cert/active.             |
| `POST`   | `/api/sites/:site/cert-retry`    | Yes  | Force Caddy reload + poll cert for 30s.                                                 |

### 14.3 Site Backups

| Method   | Path                             | Auth | Description                                              |
| -------- | -------------------------------- | ---- | -------------------------------------------------------- |
| `POST`   | `/api/sites/:site/backup`        | Yes  | Trigger synchronous site backup (DB + volume).           |
| `GET`    | `/api/sites/:site/backups`       | Yes  | List R2 backup objects for site.                         |
| `POST`   | `/api/sites/:site/restore/:date` | Yes  | Restore from date backup (`YYYY-MM-DD`). Synchronous.    |
| `DELETE` | `/api/sites/:site/backups/:date` | Yes  | Delete DB + volume objects for the dated backup.         |

### 14.4 PostgreSQL Backups

| Method | Path                    | Auth | Description                    |
| ------ | ----------------------- | ---- | ------------------------------ |
| `POST` | `/api/pg/backup`        | Yes  | Trigger synchronous PG backup. |
| `GET`  | `/api/pg/backups`       | Yes  | List PG backups in R2.         |
| `POST` | `/api/pg/restore/:date` | Yes  | Restore PG from date backup.   |

### 14.5 WP-CLI Execution

| Method | Path                         | Auth | Description                                                    |
| ------ | ---------------------------- | ---- | -------------------------------------------------------------- |
| `POST` | `/api/sites/:site/wp-exec`   | Yes  | Run a structured WP-CLI tool inside the site's PHP container.  |

See Section 27 for the tool catalog and security model.

### 14.6 Health

| Method | Path          | Auth   | Description            |
| ------ | ------------- | ------ | ---------------------- |
| `GET`  | `/api/health` | **No** | Health check endpoint. |

### 14.7 Validation Rules

- Site names: lowercase alphanumeric only (`^[a-z0-9]+$`)
- Custom domains: regex validated, no wildcards, max 253 chars, must not be a subdomain of BaseDomain
- Restore dates: `YYYY-MM-DD` format validated by regex
- Provision: rejects if site has active job or is already ACTIVE
- Destroy: rejects if site is DESTROYING/DESTROYED or has active job

---

## 15. Database Schema

**Host:** state-01 (***REMOVED***), MariaDB
**Database:** `controlplane`
**Connection:** `MaxOpenConns=10, MaxIdleConns=5, ConnMaxLifetime=5min`

### 15.1 `sites` Table

| Column           | Type     | Nullable | Notes                                              |
| ---------------- | -------- | -------- | -------------------------------------------------- |
| `site`           | VARCHAR  | No (PK)  | Unique site name                                   |
| `domain`         | VARCHAR  | No       | Default subdomain (e.g., `mysite.cowsaidmoo.tech`) |
| `custom_domain`  | VARCHAR  | Yes      | Optional custom domain                             |
| `status`         | VARCHAR  | No       | See state machine (Section 12)                     |
| `job_id`         | VARCHAR  | Yes      | Last associated job ID                             |
| `last_backup_at` | DATETIME | Yes      | Last successful backup time (added via migration)  |
| `created_at`     | DATETIME | No       | Auto-set on insert                                 |
| `updated_at`     | DATETIME | No       | Auto-updated on every write                        |

**Upsert behavior:** `INSERT ... ON DUPLICATE KEY UPDATE` on site key.

### 15.2 `jobs` Table

| Column         | Type     | Nullable | Notes                                          |
| -------------- | -------- | -------- | ---------------------------------------------- |
| `id`           | VARCHAR  | No (PK)  | UUID (google/uuid)                             |
| `type`         | VARCHAR  | No       | `PROVISION`, `DESTROY`, `STATIC_PROVISION`     |
| `site`         | VARCHAR  | No       | Site name                                      |
| `status`       | VARCHAR  | No       | `PENDING`, `PROCESSING`, `COMPLETED`, `FAILED` |
| `attempts`     | INT      | No       | Current attempt count                          |
| `max_attempts` | INT      | No       | Default 3                                      |
| `error`        | VARCHAR  | Yes      | Last error message                             |
| `payload`      | VARCHAR  | Yes      | Zip path for STATIC_PROVISION                  |
| `created_at`   | DATETIME | No       | Auto-set                                       |
| `updated_at`   | DATETIME | No       | Auto-updated                                   |
| `started_at`   | DATETIME | Yes      | Set when claimed                               |
| `completed_at` | DATETIME | Yes      | Set on completion                              |

### 15.3 `pg_backup_meta` Table

| Column            | Type         | Nullable | Notes                            |
| ----------------- | ------------ | -------- | -------------------------------- |
| `name`            | VARCHAR(64)  | No (PK)  | PG database name (e.g., `hosto`) |
| `last_backup_at`  | DATETIME     | Yes      | Last successful backup time      |
| `last_backup_key` | VARCHAR(512) | Yes      | R2 object key of last backup     |

**Upsert behavior:** `INSERT ... ON DUPLICATE KEY UPDATE` on name key.

### 15.4 Per-Site WordPress Databases

Each provisioned WordPress site gets its own database and user on state-01:

```sql
CREATE DATABASE IF NOT EXISTS wp_<site>;
CREATE USER IF NOT EXISTS 'wp_<site>'@'%' IDENTIFIED BY '***REMOVED***';
GRANT ALL PRIVILEGES ON wp_<site>.* TO 'wp_<site>'@'%';
```

---

## 16. Configuration Reference

**Source:** `config.go`, loaded via `LoadConfig()` from environment variables.

| Field                        | Env Var                         | Default                                                   | Required        | Notes                                                |
| ---------------------------- | ------------------------------- | --------------------------------------------------------- | --------------- | ---------------------------------------------------- |
| `APIPort`                    | `API_PORT`                      | `"8080"`                                                  | No              | HTTP listen port                                     |
| `APIKey`                     | `API_KEY`                       | —                                                         | **Yes (fatal)** | Shared key for X-API-Key auth                        |
| `ControlDSN`                 | `CONTROL_DSN`                   | `"control:***REMOVED***@tcp(***REMOVED***:3306)/controlplane"` | No              | Controlplane MariaDB DSN                             |
| `WordPressDSN`               | `WP_DSN`                        | `"control:***REMOVED***@tcp(***REMOVED***:3306)/"`             | No              | WordPress MariaDB DSN (no DB suffix)                 |
| `DockerHost`                 | `DOCKER_HOST`                   | `"tcp://***REMOVED***:2376"`                                 | No              | Docker API endpoint on app-01                        |
| `DockerCertDir`              | `DOCKER_CERT_DIR`               | `"/opt/control/certs"`                                    | No              | TLS certs for Docker API (ca.pem, cert.pem, key.pem) |
| `CaddyConfDir`               | `CADDY_CONF_DIR`                | `"/etc/caddy/sites"`                                      | No              | Path inside Caddy container for snippets             |
| `CaddyContainer`             | `CADDY_CONTAINER`               | `"caddy"`                                                 | No              | Caddy container name                                 |
| `CaddyStaticVolume`          | `CADDY_STATIC_VOLUME`           | `"caddy_static_sites"`                                    | No              | Shared volume mounted at /srv/sites in Caddy         |
| `BaseDomain`                 | `BASE_DOMAIN`                   | `"hosto.com"`                                             | No              | Domain for subdomain generation                      |
| `AppServerIP`                | `APP_SERVER_IP`                 | `"***REMOVED***"`                                            | No              | app-01 internal IP                                   |
| `PublicIP`                   | `PUBLIC_IP`                     | `"***REMOVED***"`                                       | No              | VPS public IP (for DNS validation)                   |
| `DockerNetwork`              | `DOCKER_NETWORK`                | `"wp_backend"`                                            | No              | Docker network for WordPress containers              |
| `CloudflaredConfigPath`      | `CLOUDFLARED_CONFIG`            | `"/etc/cloudflared/config.yml"`                           | No              | Legacy cloudflared config path                       |
| `TunnelName`                 | `TUNNEL_NAME`                   | `"hosto"`                                                 | No              | Legacy tunnel name                                   |
| `ServiceTarget`              | `TUNNEL_SERVICE_TARGET`         | `"http://***REMOVED***:8080"`                                | No              | Legacy tunnel target                                 |
| `R2AccountID`                | `R2_ACCOUNT_ID`                 | `""`                                                      | No\*            | Cloudflare account ID                                |
| `R2AccessKeyID`              | `R2_ACCESS_KEY_ID`              | `""`                                                      | No\*            | R2 access key                                        |
| `R2SecretAccessKey`          | `R2_SECRET_ACCESS_KEY`          | `""`                                                      | No\*            | R2 secret key                                        |
| `R2Bucket`                   | `R2_BUCKET`                     | `"hostplane-backups"`                                     | No              | R2 bucket name                                       |
| `PostgresDSN`                | `POSTGRES_DSN`                  | `""`                                                      | No              | PG backup DSN (empty = disabled)                     |
| `RequireBackupBeforeDestroy` | `REQUIRE_BACKUP_BEFORE_DESTROY` | `true`                                                    | No              | Abort destroy if backup fails                        |

\* R2 credentials are optional — if missing, all backup subsystems are gracefully disabled (logged, not fatal).

**Hardcoded values (not configurable):**

| Setting                 | Value      |
| ----------------------- | ---------- |
| `WorkerPollInterval`    | 3 seconds  |
| `StuckJobTimeout`       | 10 minutes |
| Max job attempts        | 3          |
| HTTP read/write timeout | 10 seconds |
| Shutdown grace period   | 15 seconds |

### 16.1 Domain Strategy

| Domain                | Purpose                                                                                     |
| --------------------- | ------------------------------------------------------------------------------------------- |
| `hosto.app`           | Product domain (customer dashboard, branding)                                               |
| `cowsaidmoo.tech`     | Subdomain generation for provisioned sites (wildcard DNS `*.cowsaidmoo.tech` at Cloudflare) |
| `api.cowsaidmoo.tech` | Control plane API endpoint                                                                  |

`BaseDomain` in `.env` is set to `cowsaidmoo.tech`. The default in code (`hosto.com`) is not used in production.

---

## 17. Naming Conventions

**Source:** `naming.go`

All naming functions are pure, deterministic, package-level functions:

| Function                   | Pattern         | Example (`site = "myblog"`) |
| -------------------------- | --------------- | --------------------------- |
| `PHPContainerName(site)`   | `php_<site>`    | `php_myblog`                |
| `NginxContainerName(site)` | `nginx_<site>`  | `nginx_myblog`              |
| `VolumeName(site)`         | `wp_<site>`     | `wp_myblog`                 |
| `WPDatabaseName(site)`     | `wp_<site>`     | `wp_myblog`                 |
| `WPDatabaseUser(site)`     | `wp_<site>`     | `wp_myblog`                 |
| `WPDatabasePass(site)`     | `***REMOVED***`   | `***REMOVED***`               |
| `SiteDomain(site, base)`   | `<site>.<base>` | `myblog.cowsaidmoo.tech`    |
| `CaddyConfFile(site)`      | `<site>.caddy`  | `myblog.caddy`              |
| `NginxConfFile(site)`      | `<site>.conf`   | `myblog.conf`               |

***REMOVED***

---

## 18. Docker Container Specifications

### 18.1 Long-Running Containers (Per WordPress Site)

| Property | `php_<site>`                                  | `nginx_<site>`                     |
| -------- | --------------------------------------------- | ---------------------------------- |
| Image    | `wordpress:php8.2-fpm`                        | `nginx:alpine`                     |
| Network  | `wp_backend`                                  | `wp_backend`                       |
| Volume   | `wp_<site>` → `/var/www/html` (RW)            | `wp_<site>` → `/var/www/html` (RO) |
| Memory   | 512 MiB                                       | 128 MiB                            |
| CPU      | 1.0 vCPU (NanoCPUs)                           | 0.5 vCPU (NanoCPUs)                |
| PIDs     | 100                                           | 50                                 |
| Restart  | unless-stopped                                | unless-stopped                     |
| Env      | WORDPRESS_DB_HOST, \_USER, \_PASSWORD, \_NAME | —                                  |

### 18.2 Caddy Container (Externally Managed)

| Property       | Value                                                              |
| -------------- | ------------------------------------------------------------------ |
| Container name | `caddy`                                                            |
| Managed by     | `/opt/caddy/docker-compose.yaml` on app-01 (not by control plane)  |
| Network        | `wp_backend` (must be shared with WordPress containers)            |
| Ports          | 80, 443 (published to app-01 host)                                 |
| Caddyfile      | `/etc/caddy/Caddyfile` (imports `/etc/caddy/sites/*.caddy`)        |
| Volume mount   | `caddy_static_sites` → `/srv/sites` (for static sites)             |
| TLS            | Automatic HTTPS via Let's Encrypt ACME                             |
| Cert storage   | `/data/caddy/certificates/acme-v02.api.letsencrypt.org-directory/` |

### 18.3 Ephemeral Backup/Restore Containers

| Operation              | Image           | Network      | Volume                                  | Stdin/Stdout    |
| ---------------------- | --------------- | ------------ | --------------------------------------- | --------------- |
| Site DB backup (WP)    | `mariadb:10`    | `wp_backend` | —                                       | stdout (attach) |
| Site DB backup (n8n)   | `postgres:16`   | `wp_backend` | —                                       | stdout (attach) |
| Site DB restore (WP)   | `mariadb:10`    | `wp_backend` | —                                       | stdin (attach)  |
| Site DB restore (n8n)  | `postgres:16`   | `wp_backend` | —                                       | stdin (attach)  |
| Site volume backup     | `alpine:latest` | default      | `wp_<site>` / `n8n_<site>` → `/data` (RO) | stdout (attach) |
| Site volume restore    | `alpine:latest` | default      | `wp_<site>` / `n8n_<site>` → `/data` (RW) | stdin (attach)  |
| Infra DB backup        | `mariadb:10`    | `wp_backend` | —                                       | stdout (attach) |
| PG DB backup        | `postgres:16`   | `bridge`     | —                              | stdout (attach) |
| PG DB restore       | `postgres:16`   | `bridge`     | —                              | stdin (attach)  |
| Static site upload  | `busybox`       | —            | `caddy_static_sites` → `/data` | CopyToContainer |

All ephemeral containers: `AutoRemove: false`, explicitly removed in `defer` with `Force: true` and 15s fresh context.

---

## 19. File System Layout

### 19.1 control-01 (LXC)

```
/opt/control/
├── control-plane              # compiled Go binary (systemd service)
├── .env                       # EnvironmentFile for systemd
└── certs/                     # Docker TLS client certs (backed up to R2)
    ├── ca.pem
    ├── cert.pem
    └── key.pem

/opt/control/scripts/
├── health.sh                  # service/MySQL/Docker/Caddy/site-container smoke checks
├── cleanup.sh                 # stale job/site/container/volume/snippet cleanup
└── config.sh                  # writes /root/.my-hosto.cnf
```

### 19.2 app-01 (VM — Docker Host)

```
/opt/caddy/
├── docker-compose.yaml        # Caddy container definition (ports 80/443, wp_backend network)
├── Caddyfile                  # global opts + import /etc/caddy/sites/*.caddy
└── sites/                     # per-site Caddy snippets (written by control plane)
    └── <site>.caddy
```

Note: Per-site nginx configs are **not** stored on the app-01 filesystem. The control plane writes `default.conf` directly into each running `nginx_<site>` container via Docker `CopyToContainer` to `/etc/nginx/conf.d/default.conf`. Nginx containers are created via the Docker API, not via docker-compose.

### 19.3 Development Machine

```
/home/oni/hosto/hostplane/
├── main.go                    # wiring, workers, HTTP server, shutdown
├── api.go                     # all HTTP handlers (939 lines)
├── config.go                  # env loading
├── db.go                      # control DB, migrations, queries
├── provisioner.go             # WordPress provisioning (422 lines)
├── static_provisioner.go      # static site provisioning (265 lines)
├── destroyer.go               # site teardown (127 lines)
├── worker.go                  # job queue processor (102 lines)
├── naming.go                  # name generation functions (51 lines)
├── lifecycle.go               # state machine + domain validation (170 lines)
├── caddy.go                   # Caddy helpers (cert check, reload, etc.)
├── tunnel.go                  # cloudflared config manager (legacy, unused)
├── backup.go                  # R2Client + streaming helpers (260 lines)
├── backupper.go               # site backup/restore (598 lines)
├── backup_worker.go           # site backup scheduler (56 lines)
├── infra_backupper.go         # infra backup/restore (317 lines)
├── infra_backup_worker.go     # infra backup scheduler (89 lines)
├── pg_backupper.go            # PostgreSQL backup/restore (364 lines)
├── pg_backup_worker.go        # PostgreSQL backup scheduler (69 lines)
├── go.mod                     # module definition
├── go.sum                     # dependency checksums
└── .ai/                       # architecture and context docs
    ├── ARCHITECTURE.md         # this file
    ├── architecture_migration.md
    └── context.md
```

---

## 20. R2 Bucket Layout

**Bucket:** `hostplane-backups`

```
databases/
  <site>/
    YYYY-MM-DD.sql.gz                     # daily site MySQL dump

volumes/
  <site>/
    YYYY-MM-DD.tar.gz                     # daily site Docker volume archive

infra/
  controlplane/
    YYYY-MM-DDTHH-MM-SSZ.sql.gz           # hourly controlplane DB dump
  certs/
    YYYY-MM-DD.tar.gz                     # daily Docker TLS cert archive

postgres/
  <dbname>/
    YYYY-MM-DDTHH-MM-SSZ.sql.gz           # daily PG dump (full timestamp)
```

### Retention Policies

| Prefix                | Retention | Cleanup Frequency             |
| --------------------- | --------- | ----------------------------- |
| `databases/`          | 3 days    | Daily (every tick)            |
| `volumes/`            | 3 days    | Daily (every tick)            |
| `infra/controlplane/` | 90 days   | Weekly (7-day ticker)         |
| `infra/certs/`        | 30 days   | Weekly (7-day ticker)         |
| `postgres/<dbname>/`  | 30 days   | Weekly (every 7th daily tick) |

### Key Granularity Differences

| Subsystem       | Key includes time?       | Overwrite behavior                |
| --------------- | ------------------------ | --------------------------------- |
| Site backups    | Date only (`YYYY-MM-DD`) | One per day, overwrites same-day  |
| Controlplane DB | Full timestamp           | Multiple per day (hourly)         |
| Docker certs    | Date only                | Idempotent skip if today's exists |
| PostgreSQL      | Full timestamp           | Multiple per day possible         |

---

## 21. Monitoring

### 21.1 Prometheus + Grafana (monitor-01)

**Host:** monitor-01 (guest 500, 10.10.0.x)

Prometheus scrapes **node_exporter** on:

- app-01 (VM)
- state-01 (VM)
- control-01 (LXC)

Grafana provides dashboards for host-level metrics (CPU, RAM, disk, network).

### 21.2 What Is NOT Monitored (Currently)

- No container-level metrics (no cAdvisor)
- No custom metrics exported by the control plane binary
- No application-level metrics (request rates, provisioning duration, queue depth)
- No alerting rules documented

---

## 22. Operational Scripts

**Location:** `/opt/control/scripts/` on control-01

| Script       | Purpose                                                                                                |
| ------------ | ------------------------------------------------------------------------------------------------------ |
| `health.sh`  | Smoke checks: systemd service, MySQL connectivity, Docker API, Caddy status, per-site container health |
| `cleanup.sh` | Cleans stale jobs, sites, orphaned containers/volumes/snippets                                         |
| `config.sh`  | Writes `/root/.my-hosto.cnf` (MySQL client config for manual admin)                                    |

---

## 23. Architecture Invariants

These rules must not be violated by any code change:

1. **DB is source of truth.** Infrastructure configs (Caddy snippets, nginx configs) are derived artifacts that can be regenerated from DB state.

2. **Validate → Apply Infra → Confirm → Commit DB.** Never commit state to the database before infrastructure changes are confirmed successful.

3. **State transitions are explicit.** Use `TransitionSite()` and the lifecycle state machine. Never set status directly without validation.

4. **Everything must be idempotent.** Retries must be safe. Container creation checks for existing containers. Config writes are full regenerations, not patches.

5. **Generate, don't patch.** Rewrite config artifacts from desired state rather than trying to modify existing configs.

6. **No fire-and-forget infra mutations.** Every infrastructure change must be confirmed synchronously (exit codes checked, reloads verified).

7. **Rollback on partial failure.** When step N fails, compensate steps 1..(N-1) in reverse order.

8. **Backups must stream, not stage on disk.** Use pipe-based uploads and restores. No temporary files on control-01 for backup data.

9. **Per-site runtime limits are part of platform safety.** Any provisioning refactor must preserve or intentionally migrate container CPU/RAM/PID limits.

---

## 24. Migration History

### Old Architecture (Cloudflare Tunnel — Abandoned)

```
Browser → Cloudflare → Cloudflare Tunnel (cloudflared) → app-01 nginx → Docker containers
```

**Why abandoned:**

- Tunnel required restart for every new site route
- SSL was tunnel-dependent (no tunnel = no HTTPS)
- Custom domains required customers to be on Cloudflare
- Frequent bad gateways and tunnel instability
- Control plane tightly coupled to cloudflared lifecycle

### Current Architecture (VPS + WireGuard)

```
Browser → Cloudflare → VPS (dumb TCP forwarder) → WireGuard → Caddy → nginx sidecar → PHP-FPM
```

**Key improvements:**

- No tunnel restarts — Caddy reload is instantaneous, zero downtime
- Wildcard DNS — zero DNS changes per new site
- Custom domains need only a CNAME to `cowsaidmoo.tech`
- VPS is a disposable gateway (no state, no logic)
- nginx sidecar per site — Caddy never touches PHP or WordPress files
- Full isolation: each site's volume mounted only into its own containers
- All compute on home hardware; VPS is purely network ingress

### Legacy Code (`tunnel.go`)

The `TunnelManager` is still wired in `main.go` and injected into `API`, but it is **not called** by the current provisioning flow. It manages cloudflared YAML config and can add/remove ingress rules. `RemoveRoute` is a no-op (DNS deletion requires Cloudflare API, not implemented).

---

## 25. Outstanding Work / Future

### Infrastructure Gaps

- [ ] Define capacity model for app-01 (max sites from CPU/RAM/PID budget)
- [ ] Add per-plan resource profiles (small/medium/large) for php/nginx container limits
- [ ] Add admission control: reject new provisions when host headroom is below threshold
- [ ] Static site provisioning has no container resource limits (serves from Caddy directly)
- [ ] HTTPS health check endpoint per site

### Feature Work

- [ ] Site destroy for static sites (currently only WordPress destroy is implemented)
- [ ] Remove old cloudflared config once confirmed fully decommissioned
- [ ] Decide whether Hostplane should orchestrate lifecycle/backup for ***REMOVED*** frontend/backend containers

### Monitoring Gaps

- [ ] No container-level metrics (cAdvisor or equivalent)
- [ ] No application metrics from the control plane binary
- [ ] No alerting rules

### Code Quality

- [ ] `waitContainer` and `logContainerStderr` are duplicated across 3 backupper types — could be extracted to shared functions
- [ ] `reloadCaddy` uses `os/exec` (shell out to docker CLI) while all other Docker operations use the Go API client — inconsistency
***REMOVED***
- [ ] `FailJob` path in `worker.go` is unreachable — exhausted jobs stay PENDING instead of being marked FAILED (SQL `WHERE attempts < max_attempts` prevents re-claim, but Go-side `job.Attempts >= job.MaxAttempts` check uses pre-increment value and never fires)

### Future Architecture

- Multi-node Proxmox: one strong master + multiple efficient child nodes
- Potential site migration between nodes

---

## 26. Dependencies

**Source:** `go.mod` (Go 1.24.0)

### Direct Dependencies

| Package                                           | Version  | Purpose                                   |
| ------------------------------------------------- | -------- | ----------------------------------------- |
| `github.com/aws/aws-sdk-go-v2`                    | v1.41.2  | AWS SDK core (R2/S3 compatibility)        |
| `github.com/aws/aws-sdk-go-v2/credentials`        | v1.19.10 | Static credential provider                |
| `github.com/aws/aws-sdk-go-v2/feature/s3/manager` | v1.22.4  | Streaming multipart upload manager        |
| `github.com/aws/aws-sdk-go-v2/service/s3`         | v1.96.2  | S3-compatible API operations              |
| `github.com/docker/docker`                        | v24.0.7  | Docker Engine API client                  |
| `github.com/gin-gonic/gin`                        | v1.11.0  | HTTP router and framework                 |
| `github.com/go-sql-driver/mysql`                  | v1.9.3   | MySQL/MariaDB driver                      |
| `github.com/google/uuid`                          | v1.6.0   | UUID generation for job IDs               |
| `gopkg.in/yaml.v3`                                | v3.0.1   | YAML parsing (cloudflared config, legacy) |

---

## 27. WP-CLI Execution (`wp_exec.go`)

A single endpoint, `POST /api/sites/:site/wp-exec`, runs WP-CLI commands inside a site's PHP container. Designed for AI agent tool calls: the caller names a **tool**, not a raw command. The control plane maps the tool + params to a validated argv and execs WP-CLI; raw command strings are never accepted.

### 27.1 Request / Response

```json
// request
{ "tool": "list_posts", "params": { "post_type": "page" } }

// response (200)
{ "site": "mysite", "tool": "list_posts", "output": "[...]", "exit_code": 0 }
```

A non-zero `exit_code` returns HTTP `422` with the same body shape.

### 27.2 Tool Catalog (`buildTool`)

| Tool                | Params                                    | Notes                                                       |
| ------------------- | ----------------------------------------- | ----------------------------------------------------------- |
| `get_site_info`     | —                                         | Returns `blogname`                                          |
| `get_option`        | `key`                                     | Key must be in `safeOptionKeys` allowlist                   |
| `update_option`     | `key`, `value`                            | Same allowlist                                              |
| `list_posts`        | `post_type`, `status`                     | `post_type` ∈ {post, page}; `status` ∈ {publish, draft, any} |
| `get_post`          | `id`                                      | `id` must be a positive integer                             |
| `update_post`       | `id`, `field`, `value`                    | `field` ∈ {post_title, post_content, post_excerpt, post_status} |
| `create_post`       | `post_type`, `post_title`, `post_content` | Always inserted as `draft`                                  |
| `list_plugins`      | —                                         |                                                             |
| `install_plugin`    | `slug`                                    | Slug regex `^[a-z0-9]+(-[a-z0-9]+)*$`; auto-activates       |
| `deactivate_plugin` | `slug`                                    | Same slug regex                                             |
| `list_themes`       | —                                         |                                                             |
| `activate_theme`    | `slug`                                    | Same slug regex                                             |
| `search_replace`    | `old`, `new`                              | **Dry-run only** (`--dry-run`) — live writes deferred to V2 |
| `media_import`      | `url`, `title`                            | HTTPS only; SSRF block list for loopback + RFC1918 ranges   |

### 27.3 Security Model

- **No raw commands.** The control plane builds argv from a fixed switch; the caller cannot reach `wp eval`, `wp db`, or any unlisted subcommand.
- **`safeOptionKeys` allowlist.** Excludes `siteurl`, `home`, `admin_email`, and anything else that would let the agent break routing or hijack the install.
- **Field allowlist.** Post updates can only touch title / content / excerpt / status.
- **Slug regex.** Plugin/theme installs only accept `[a-z0-9-]+`, blocking path traversal and shell metacharacters.
- **SSRF guard on `media_import`.** Rejects HTTP, loopback, link-local, and all RFC1918 subnets.
- **`search_replace` is dry-run only.** Live execution is gated on a future pre-backup hook (V2).

### 27.4 Execution

- Exec target: `PHPContainerName(site)` (the per-site `php_<site>` container).
- User: `33:33` (`www-data`) so WP-CLI-created files have the right ownership.
- Working directory: `/var/www/html`.
- Timeout: 30 seconds (`context.WithTimeout`).
- Stdout is read through `stripDockerMux` (shared with the backup subsystem) and capped at 1 MiB; truncation appends `[output truncated at 1MB]`.
- Site must be in `ACTIVE` or `DOMAIN_ACTIVE`; otherwise `409`.
