# Shared Hosting Migration Proposal

> **Status:** Archived — decision is to keep container-per-tenant until ceiling is hit
> **Date:** 2026-05-26
> **Author:** Architecture review with Claude

---

## 1. Purpose

This document compares Hostplane's current **container-per-tenant** model against a traditional **shared hosting** model. It is a reference for the future — when site density on app-01 becomes a constraint, this document describes exactly what changes, what breaks, and what the migration path looks like.

---

## 2. Current Architecture (Container-Per-Tenant)

### 2.1 Infrastructure

| Host | Role | CPU | RAM | Disk |
|------|------|-----|-----|------|
| **pve** | Proxmox hypervisor | Ryzen 7 5700G (8c/16t) | 27.3 GiB | 3.47 TiB |
| **app-01** (VM 200) | Docker host for all site containers + Caddy | 6 cores | 16 GiB | 1 TB |
| **state-01** (VM 400) | MariaDB + PostgreSQL | 2 cores | 8 GiB | 200 GB |
| **control-01** (LXC 101) | Control plane binary | 1 core | 1.5 GiB | 12 GB |
| **monitor-01** (VM 500) | Prometheus + Grafana | 2 cores | 2 GiB | 30 GB |

### 2.2 Per-Site Resources (WordPress)

Each WordPress site provisions **6 infrastructure resources**:

| Resource | Name | Host | Reserved |
|----------|------|------|----------|
| PHP-FPM container | `php_<site>` | app-01 | 512 MiB RAM, 1.0 vCPU, 100 PIDs |
| nginx sidecar | `nginx_<site>` | app-01 | 128 MiB RAM, 0.5 vCPU, 50 PIDs |
| Docker volume | `wp_<site>` | app-01 | Unbounded (disk) |
| MariaDB database | `wp_<site>` | state-01 | Shared pool |
| MariaDB user | `wp_<site>` | state-01 | — |
| Caddy snippet | `<site>.caddy` | app-01 (Caddy container) | — |

**Per-site overhead: 640 MiB RAM, 1.5 vCPU reserved**

### 2.3 Request Path

```
Browser → Cloudflare → DO VPS (TCP forward) → WireGuard
  → Caddy (:443, TLS termination)
    → nginx_<site>:80 (static files + PHP proxy)
      → php_<site>:9000 (WordPress PHP-FPM)
        → state-01 MariaDB (wp_<site>)
```

### 2.4 Current Capacity

| Metric | Value |
|--------|-------|
| app-01 total RAM | 16 GiB |
| Caddy + Docker overhead | ~1.5 GiB |
| Available for site containers | ~14.5 GiB |
| Per-site reservation | 640 MiB |
| **Max WordPress sites** | **~22** |
| Current sites | 10 |
| Headroom | 12 more sites |

CPU is less constraining: 6 cores with 1.5 vCPU per site = ~4 sites at full load, but WordPress sites are idle 99%+ of the time, so CPU oversubscription is fine.

### 2.5 Why nginx Sidecar

Caddy can do FastCGI directly, but that would require mounting every `wp_<site>` volume into the single Caddy container:
- N volume mounts on one container
- Caddy restart on every new site (breaks zero-downtime)
- No per-site filesystem isolation

The sidecar model keeps Caddy clean — it only does hostname routing. Each nginx has one volume mount and can be restarted independently.

---

## 3. Proposed Architecture (Shared Hosting)

### 3.1 Overview

Replace per-site containers with a single PHP-FPM process and a single nginx instance serving all sites via virtual hosts.

```
Browser → Cloudflare → DO VPS → WireGuard
  → Caddy (:443)
    → nginx:80 (single instance, vhost routing)
      → PHP-FPM :9000 (single pool, all sites)
        → state-01 MariaDB (wp_<site>)
```

### 3.2 Per-Site Resources (Shared Model)

| Resource | Name | Host | Reserved |
|----------|------|------|----------|
| nginx vhost config | `/etc/nginx/sites/<site>.conf` | app-01 | — |
| PHP-FPM pool entry | `/etc/php-fpm.d/<site>.conf` (optional) | app-01 | — |
| Site directory | `/srv/sites/<site>/` | app-01 | Unbounded (disk) |
| MariaDB database | `wp_<site>` | state-01 | Shared pool |
| MariaDB user | `wp_<site>` | state-01 | — |
| Caddy snippet | `<site>.caddy` | app-01 | — |

**Per-site overhead: ~0 MiB RAM** (nginx vhost + directory, no dedicated processes)

### 3.3 Shared Process Allocation

| Process | Instances | RAM | CPU |
|---------|-----------|-----|-----|
| nginx (single) | 1 master + N workers | ~200 MiB | Negligible |
| PHP-FPM (single pool) | 1 master + `pm.max_children` workers | ~50 MiB per child, 20 children = 1 GiB | Burst |
| Caddy | 1 | ~100 MiB | Negligible |
| **Total** | — | **~1.3 GiB** | — |

### 3.4 Capacity (Shared Model)

| Metric | Value |
|--------|-------|
| Fixed process overhead | ~1.3 GiB |
| Per-site overhead | ~0 (just disk + DB) |
| **Max WordPress sites** | **200+** (disk/DB limited) |
| Limiting factor | Disk I/O, MariaDB connections, PHP-FPM child pool |

---

## 4. Side-by-Side Comparison

### 4.1 Capacity & Density

| Metric | Container-Per-Tenant | Shared Hosting |
|--------|---------------------|----------------|
| Max sites on app-01 (16 GiB) | ~22 | 200+ |
| RAM per site | 640 MiB reserved | ~0 (shared pool) |
| CPU per site | 1.5 vCPU reserved | Shared (no guarantee) |
| Provisioning time | ~7 seconds (image pull cached) | <1 second (file copy + reload) |
| Docker containers per site | 2 (php + nginx) | 0 |
| Total containers at 10 sites | 21 (20 site + 1 Caddy) | 3 (nginx + PHP-FPM + Caddy) |

### 4.2 Isolation & Security

| Property | Container-Per-Tenant | Shared Hosting |
|----------|---------------------|----------------|
| Memory isolation | Hard limit per site (OOM kills one site only) | None — one runaway site starves all |
| CPU isolation | NanoCPU limit per site | None — one site can peg all cores |
| PID isolation | Per-container PID limit (100/50) | None — fork bombs affect all |
| Filesystem isolation | Separate Docker volume per site | Shared filesystem (Linux user separation only) |
| PHP process isolation | Separate FPM pool per site | Shared FPM pool — crash affects all |
| PHP version flexibility | Per-site image (can mix 8.1/8.2/8.3) | One version for all sites |
| Cross-site attack surface | Container boundary (high bar) | OS user boundary (lower bar) |
| Independent restart | Restart one site's PHP without touching others | PHP-FPM restart bounces all sites |

### 4.3 Operations

| Operation | Container-Per-Tenant | Shared Hosting |
|-----------|---------------------|----------------|
| Provisioning | Docker create + start (2 containers, 1 volume) | mkdir + copy files + nginx vhost + reload |
| Destroy | Docker rm (2 containers + volume) + DB drop | rm -rf directory + nginx vhost + DB drop |
| Backup (DB) | Ephemeral container runs mysqldump, streams to R2 | Direct mysqldump from state-01, stream to R2 |
| Backup (files) | Ephemeral container tars Docker volume to R2 | Direct tar of site directory to R2 |
| WP-CLI | `docker exec php_<site> wp ...` | `sudo -u <site-user> wp --path=/srv/sites/<site> ...` |
| Debug one site | `docker logs php_<site>` | Grep shared PHP-FPM/nginx logs |
| PHP config change | Restart one container | Restart shared FPM (all sites bounce) |
| Scale up | Add app-02 VM, point control plane at it | Same, but also need shared filesystem (NFS/GlusterFS) |

### 4.4 Failure Modes

| Scenario | Container-Per-Tenant | Shared Hosting |
|----------|---------------------|----------------|
| PHP fatal error | One site's container crashes, restarts via Docker policy. Others unaffected. | Depends on FPM pool config. `pm = ondemand` recovers; `pm = static` may lose a child from the shared pool. |
| Memory exhaustion | Docker OOM-kills the offending container (512 MiB limit). Other sites unaffected. | OOM-killer picks a process — could be any site's request or even nginx/FPM master. Catastrophic. |
| Compromised plugin | Attacker has access inside one container. Cannot reach other containers or host. | Attacker has access to the shared PHP process. Can read/modify ALL sites' files. Must rely on OS-level user separation. |
| Traffic spike on one site | That site hits its CPU/PID limit and slows down. Other sites unaffected. | Saturates shared FPM pool. All sites slow down or queue. |
| Disk full | Docker volume is per-site — could fill the shared storage. Same impact in both models. | Same. |

---

## 5. Codebase Impact

### 5.1 Files That Change

| File | Current Role | Shared Model Change | Effort |
|------|-------------|---------------------|--------|
| `provisioner.go` (422 lines) | Creates 2 Docker containers, volume, writes nginx config into container, writes Caddy snippet | **Full rewrite**: create directory, copy WP files, write nginx vhost to disk, write PHP-FPM pool config, reload nginx + FPM | High |
| `static_provisioner.go` (265 lines) | Uploads zip to Caddy volume via ephemeral busybox | Minor change: write to `/srv/sites/<site>/` directly instead of via container | Low |
| `destroyer.go` (127 lines) | Force-removes 2 containers + volume, drops DB | **Rewrite**: `rm -rf /srv/sites/<site>/`, remove nginx vhost + FPM pool, reload, drop DB | Medium |
| `backupper.go` (598 lines) | Tars Docker volume via ephemeral alpine container, mysqldump via ephemeral mysql container | **Simplify**: direct tar of `/srv/sites/<site>/` (no ephemeral container needed). mysqldump unchanged. | Medium |
| `naming.go` (51 lines) | Container names (`php_<site>`, `nginx_<site>`), volume names | Remove container/volume naming. Add site directory path, nginx vhost path, FPM pool path. | Low |
| `caddy.go` | Caddy snippets with `reverse_proxy nginx_<site>:80` | Change to `reverse_proxy localhost:80` or `reverse_proxy unix//run/php-fpm/<site>.sock` | Low |
| `api.go` | `handleWPExec` uses `docker exec php_<site>` | Change to `sudo -u <site-user> wp --path=...` (or SSH, or local exec) | Medium |
| `config.go` | Docker-focused config (DOCKER_HOST, DOCKER_CERT_DIR) | Add nginx/FPM config paths. Docker config still needed for Caddy + backup containers. | Low |
| `main.go` | Wires Docker client, provisioners | Remove Docker dependency from provisioner (keep for Caddy and backups) | Low |
| `worker.go` | Dispatches to provisioner/destroyer | No change (just calls different methods) | None |
| `lifecycle.go` | State machine | No change | None |
| `db.go` | Schema, queries | No change | None |
| `metrics.go` | Prometheus gauges | No change | None |

**Estimated rewrite: ~60-70% of provisioner + destroyer + backupper**. API, job queue, lifecycle, and database layers are unchanged.

### 5.2 New Infrastructure Requirements

| Requirement | Why | Complexity |
|-------------|-----|------------|
| nginx installed on app-01 (not in container) | Single nginx serving all vhosts | Low — apt install |
| PHP-FPM installed on app-01 (not in container) | Single FPM pool for all sites | Low — apt install |
| Per-site OS users (optional) | Filesystem isolation between sites | Medium — user management in provisioner |
| Shared site directory `/srv/sites/` | All WordPress installations | Already exists for static sites |
| Config reload mechanism | Control plane must reload nginx/FPM on app-01 | Medium — SSH or remote exec from control-01 |
| Log aggregation per site | Debugging without per-container logs | Medium — nginx access log per vhost, PHP error log per pool |

### 5.3 Lost Capabilities

| Capability | Container Model | Shared Model |
|------------|----------------|--------------|
| Per-site PHP version | Different Docker image per site | Not possible without multiple FPM installs |
| Per-site memory hard limit | Docker `--memory 512m` | FPM `memory_limit` (per-request, not per-site total) |
| Per-site PID limit | Docker `--pids-limit 100` | Not possible |
| Per-site CPU limit | Docker `--cpus 1.0` | Not possible (cgroups v2 manual setup required) |
| Independent site restart | `docker restart php_<site>` | FPM reload bounces all |
| Container-level monitoring | Per-container CPU/mem via Docker stats | Need per-vhost metrics (more complex) |

---

## 6. Migration Path (If/When Needed)

### Phase 1: Preparation (no user impact)
1. Install nginx + PHP-FPM on app-01 alongside Docker
2. Create `/srv/wordpress/` directory structure
3. Build new `SharedProvisioner` alongside existing `Provisioner`
4. Add `HOSTING_MODEL=container|shared` config toggle

### Phase 2: New sites on shared model
5. New provisions use `SharedProvisioner` when `HOSTING_MODEL=shared`
6. Existing container sites continue running unchanged
7. Both models coexist — mixed fleet

### Phase 3: Gradual migration
8. For each existing site: backup → provision on shared → restore → swap Caddy target → destroy old containers
9. Per-site migration, no big-bang cutover
10. Rollback: re-provision container from backup if shared model has issues

### Phase 4: Cleanup
11. Remove container provisioner code paths
12. Reclaim Docker resources (images, stopped containers)
13. Update monitoring dashboards

**Estimated timeline: 2-3 weeks of focused work, including testing.**

---

## 7. Decision Criteria

Switch to shared hosting **when**:
- Site count exceeds ~20 on app-01 AND adding a second app node is not viable
- Per-site resource isolation is not a selling point (price-competitive market)
- All customers are on the same PHP version and don't need independent restarts

Stay on container-per-tenant **when**:
- Site count is under ~20 (current: 10)
- Selling managed/premium hosting where isolation is the differentiator
- Per-site debugging, restarts, and resource limits are operationally valuable
- Adding a second app node is simpler than rewriting the provisioner

---

## 8. Recommendation

**Keep the container-per-tenant model.** At 10 sites on a 22-site capacity, there is no density pressure. The current model provides:

- Hard resource isolation (the strongest customer protection you can offer)
- Per-site debugging and independent restarts
- Clean security boundary against cross-site attacks
- Already built and working in production

When you hit ~18-20 sites, the cheaper path is adding a second `app-02` VM on the Proxmox host (you have 11 GiB unused RAM and idle CPU cores) rather than rewriting 60-70% of the control plane.

The shared hosting model becomes worth revisiting only if:
- You need 50+ sites on a single node
- You're competing on price ($3-5/mo tier)
- You're willing to trade isolation for density

This document will be here when that day comes.
