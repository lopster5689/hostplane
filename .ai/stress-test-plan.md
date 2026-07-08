# Plan: WordPress Hosting Stress Test

## Context

The density optimization proposal (`hostplane/.ai/density-optimization-proposal.md`) recommends reducing PHP container memory limits from 512 MiB to save RAM and fit more sites on app-01. However, the numbers it's based on are **idle-state `docker stats` snapshots** — not peaks under load. The heaviest site (site-a) idles at 222 MiB, but with concurrent WooCommerce checkout requests, PHP-FPM spawns children that could push memory to 400-500 MiB.

We need real load-test data before making any limit changes. This plan sets up monitoring, creates a test site, runs 4 progressive stress scenarios, and produces a results table that maps directly to the density proposal decisions.

---

## Step 1: Install Monitoring (cAdvisor + mysqld_exporter)

Currently there are no per-container metrics (ARCHITECTURE.md section 21 gap). Host-level node_exporter exists on all hosts.

### cAdvisor on app-01
- Run `gcr.io/cadvisor/cadvisor:v0.49.1` on port 8081 (80/443 taken by Caddy, 8080 by control plane)
- Provides `container_memory_usage_bytes`, `container_memory_working_set_bytes`, `container_memory_max_usage_bytes`, `container_cpu_usage_seconds_total`, `container_oom_events_total` per container
- Add scrape target to Prometheus on monitor-01 (`***REMOVED***:8081`, 10s interval during tests)

### mysqld_exporter on state-01
- Create `exporter` MariaDB user with PROCESS + REPLICATION CLIENT + SELECT grants
- Run `mysqld_exporter` on port 9104
- Provides `mysql_global_status_threads_connected`, `mysql_global_status_threads_running`, query rates, slow queries, buffer pool metrics
- Add scrape target to Prometheus (`***REMOVED***:9104`)

### k6 load testing tool
- Install on the DO VPS (`***REMOVED***`) for through-tunnel tests
- Install on app-01 (`***REMOVED***`) for direct-to-Caddy tests (bypasses tunnel, isolates app-01 capacity)

## Step 2: Provision Test Site

Provision a dedicated `loadtest` WordPress + WooCommerce site via the existing control plane API. Never test against customer sites.

- `POST /api/provision {"site": "loadtest", "user_id": "stress-test"}`
- Install WooCommerce via `POST /api/sites/loadtest/wp-exec {"tool": "install_plugin", "params": {"slug": "woocommerce"}}`
- Seed 20 products, enable COD payment, install Storefront theme via `docker exec` into `php_loadtest`
- Add `/etc/hosts` entry on VPS and app-01 to bypass Cloudflare DNS: `127.0.0.1 loadtest.cowsaidmoo.tech`

## Step 3: k6 Scripts

Three scripts targeting different workload intensities:

### `baseline.js` — Homepage + shop page
Light read-only workload. Measures baseline per-request PHP memory.

### `woocommerce-checkout.js` — Full checkout flow (the critical test)
Browse shop → add 2-3 products to cart → view cart → load checkout → submit order (COD). This is the heaviest realistic WordPress workload: multiple PHP-FPM children, DB reads (products, cart), DB writes (order creation, inventory), session handling, nonce validation.

### `wp-admin.js` — Admin panel
Login → dashboard → WooCommerce orders page → plugins page. DB-heavy read workload.

All scripts use `insecureSkipTLSVerify: true` (self-signed dev cert) and pass `BASE_URL` via `-e` flag.

## Step 4: Test Scenarios

### Scenario 1: Single Site Ramp (find per-site ceiling)
Ramp from 1 to 50 concurrent users over ~3 minutes using `woocommerce-checkout.js`. Run twice: once from VPS (through WireGuard tunnel) and once from app-01 (direct to Caddy). Comparing the two reveals whether the tunnel is a bottleneck.

**Key metric:** At what VU count does p95 latency exceed 5 seconds? That's the per-site ceiling.

### Scenario 2: Sustained Load (15 minutes at 80% ceiling)
Run at 80% of the ceiling found in Scenario 1 for 15 minutes. PHP memory can creep upward as FPM children accumulate — a 30-second ramp test won't catch this. Record memory at 1-minute intervals to confirm it plateaus.

**Key metric:** PHP `container_memory_max_usage_bytes` after 15 minutes — this is the true peak for the density proposal.

### Scenario 3: Cross-Site Interference
Run heavy load on `loadtest` site while probing 5 real customer sites at 1 request per 5 seconds (negligible load). Compare observer p95 latency with and without the loadtest load running.

**Key metric:** Do other sites degrade when one site is hammered? If observer p95 increases >50%, the container isolation has a shared-resource bottleneck (likely CPU or MariaDB).

### Scenario 4: Host Saturation
Progressive overload: 2 minutes each at 10, 25, 50, 75, 100, 150, 200 VUs. Watch app-01 CPU, memory, state-01 CPU, MariaDB connections. Stop when any resource hits 90%+ or error rate exceeds 30%.

**Key metric:** Which resource saturates first — app-01 CPU, app-01 RAM, state-01 CPU, or MariaDB connections? This determines what to scale when you hit the ceiling.

## Step 5: Results Tables

### Table 1: Per-Site Memory Under Load

| Metric | Idle | 10 VUs | 20 VUs | 30 VUs | 40 VUs | 50 VUs | Sustained 15m |
|--------|------|--------|--------|--------|--------|--------|---------------|
| PHP memory_working_set (MiB) | | | | | | | |
| PHP memory_max_usage (MiB) | | | | | | | |
| nginx memory (MiB) | | | | | | | |
| k6 p95 latency (ms) | — | | | | | | |
| k6 error rate (%) | — | | | | | | |

### Table 2: Limit Decision (fill from Table 1)

| PHP Peak Under Load | Safe Limit (peak + 25%) | Max Sites on 16 GiB | nginx Safe Limit |
|---------------------|------------------------|---------------------|------------------|
| ___ MiB | ___ MiB | ~___ | ___ MiB |

### Table 3: Cross-Site Interference

| Observer Site | Baseline p95 (ms) | Under-Load p95 (ms) | Degradation % | Pass (<50%)? |
|---------------|--------------------|----------------------|---------------|--------------|

### Table 4: Saturation Point

| Resource | VU Count at 80% | Value at Saturation | Bottleneck? |
|----------|-----------------|---------------------|-------------|
| app-01 CPU | | | |
| app-01 RAM | | | |
| state-01 CPU | | | |
| MariaDB connections | | | |

### Table 5: WireGuard Tunnel Overhead

| VUs | p95 via VPS (ms) | p95 direct (ms) | Overhead | Bottleneck? |
|-----|------------------|-----------------|----------|-------------|

## Step 6: Cleanup

- Destroy `loadtest` site via `POST /api/destroy`
- **Keep** cAdvisor and mysqld_exporter running permanently (fills ARCHITECTURE.md section 21 monitoring gap)
- Revert Prometheus scrape interval from 10s to 15s
- Remove `/etc/hosts` entries and k6 output files
- Update `density-optimization-proposal.md` with actual numbers from Tables 1-5

## Verification

After all scenarios complete:
1. All 5 results tables filled with real numbers
2. Table 2 produces a concrete limit recommendation backed by data
3. density-optimization-proposal.md updated with measured (not estimated) values
4. cAdvisor + mysqld_exporter remain as permanent monitoring additions
5. `loadtest` site fully destroyed, no artifacts left
