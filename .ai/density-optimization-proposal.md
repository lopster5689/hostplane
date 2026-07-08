# Density Optimization Proposal: Scaling to 100+ Sites Without Losing Isolation

> **Date:** 2026-05-26
> **Status:** Parked — revisit when site count approaches 20. Current capacity: 10/22 sites.

---

## 1. The Problem

Each WordPress site reserves **640 MiB** (512 PHP + 128 nginx) but actually uses far less:

| Site | PHP Actual | PHP Limit | nginx Actual | nginx Limit | Wasted |
|------|-----------|-----------|-------------|-------------|--------|
| site-a | 222 MiB | 512 MiB | 6.6 MiB | 128 MiB | 411 MiB |
| site-b | 197 MiB | 512 MiB | 6.4 MiB | 128 MiB | 436 MiB |
| site-c | 136 MiB | 512 MiB | 6.4 MiB | 128 MiB | 497 MiB |
| site-d | 132 MiB | 512 MiB | 6.5 MiB | 128 MiB | 501 MiB |
| site-e | 111 MiB | 512 MiB | 6.4 MiB | 128 MiB | 522 MiB |
| site-f | 101 MiB | 512 MiB | 6.4 MiB | 128 MiB | 533 MiB |
| elearning | 75 MiB | 512 MiB | 6.2 MiB | 128 MiB | 558 MiB |
| site-g | 78 MiB | 512 MiB | 6.5 MiB | 128 MiB | 555 MiB |
| site-h | 73 MiB | 512 MiB | 6.4 MiB | 128 MiB | 560 MiB |
| site-i | 74 MiB | 512 MiB | 6.3 MiB | 128 MiB | 559 MiB |
| **Totals** | **1,199 MiB** | **5,120 MiB** | **64 MiB** | **1,280 MiB** | **5,132 MiB** |

**10 sites reserve 6,400 MiB but actually use 1,262 MiB.** That's 80% waste — 5 GiB of RAM reserved but unused.

The nginx sidecars are the worst offenders: each reserves 128 MiB but uses 6 MiB. That's **95% waste across 10 containers** for a process that just proxies static files.

### Important Caveat: These Numbers Are Idle Baselines

The figures above are **point-in-time `docker stats` snapshots** taken on 2026-05-26 when all sites have minimal real traffic. They represent idle/low-traffic baselines, NOT peaks under load.

WordPress PHP-FPM spawns child processes per concurrent request, each consuming 30-50 MiB. A WooCommerce site handling 10 concurrent checkout requests could push PHP memory to **400-500 MiB** — approaching the current 512 MiB limit. The 512 MiB reservation exists for this reason.

**Any limit reduction must be validated with load testing** (e.g., `wrk` or `k6` against WooCommerce checkout flows) before applying to production sites.

---

## 2. Three Phases (Pick Your Ceiling)

Each phase is independent. Implement only when you need the density.

| Phase | Change | Sites on 16 GiB | Code Effort | Isolation Impact |
|-------|--------|-----------------|-------------|-----------------|
| Current | 512 MiB PHP + 128 MiB nginx | ~22 | — | Full |
| **Phase 1** | Right-size limits | **~33-47** | 2 lines | None |
| **Phase 2** | Drop nginx sidecar | **~37-55** | Moderate | Minimal |
| **Phase 3** | Idle suspension (scale-to-zero) | **200+** | Significant | None |

---

## 3. Phase 1: Right-Size Container Limits

### What Changes

The nginx sidecar limit can be safely reduced — every sidecar uses 6 MiB against a 128 MiB limit. PHP limits require more care.

**Conservative option (recommended first step):**

```
PHP container:   512 MiB → 384 MiB  (keep headroom for load spikes)
nginx container: 128 MiB → 48 MiB   (safe — 8x actual usage)
Per-site total:  640 MiB → 432 MiB
```

**Aggressive option (only after load testing):**

```
PHP container:   512 MiB → 256 MiB
nginx container: 128 MiB → 48 MiB
Per-site total:  640 MiB → 304 MiB
```

### Why nginx at 48 MiB Is Safe

Every single nginx sidecar uses 6 MiB at idle. Under heavy traffic, nginx rarely exceeds 20-30 MiB. 48 MiB is 8x current usage.

### Why PHP Needs Caution

The idle-state numbers (75-222 MiB) are misleading. PHP-FPM spawns child processes per concurrent request, each consuming 30-50 MiB. A WooCommerce site handling 10 concurrent checkout/cart operations could push PHP to 400-500 MiB. The 512 MiB limit is not as generous as the idle numbers suggest.

**Before reducing PHP below 384 MiB**, run a load test:
```bash
# Install k6 or wrk, simulate 20 concurrent users on WooCommerce checkout
k6 run --vus 20 --duration 60s woocommerce-checkout.js
# Monitor: docker stats php_<site> during the test
```

### Capacity

| Option | Per-Site | Max Sites on 16 GiB |
|--------|----------|---------------------|
| Current | 640 MiB | ~22 |
| Conservative (384 + 48) | 432 MiB | ~33 |
| Aggressive (256 + 48) | 304 MiB | ~47 |

### Escape hatch for heavy sites

Add a `plan` column to the sites table later. Heavy sites get 512 MiB PHP. Light sites get 384 or 256 MiB. The provisioner reads the plan and sets limits accordingly. This is a product feature, not an architecture change.

### Code Changes

| File | Change |
|------|--------|
| `provisioner.go` | Change `Memory: 512 * 1024 * 1024` → `384 * 1024 * 1024` and `Memory: 128 * 1024 * 1024` → `48 * 1024 * 1024` |

Existing sites keep their current limits (container limits are set at creation time). New sites get the lower limits. To resize existing sites, recreate the container (stop → remove → create with new limits → start).

### Risk

| Scenario | Likelihood | Impact | Mitigation |
|----------|-----------|--------|------------|
| PHP OOM at 384 MiB under load | Low-Medium | One site's PHP process killed, auto-restarts | Monitor with `docker stats`, bump to 512 for that site |
| PHP OOM at 256 MiB under load | Medium (not load-tested) | Same | **Must load test before applying** |
| nginx OOM at 48 MiB | Near zero (6/128 MiB actual) | One site's static serving restarts | 48 MiB is 8x actual usage |

---

## 4. Phase 2: Drop the nginx Sidecar

### Background: Why the nginx Sidecar Exists

The nginx sidecar is an **architectural design decision**, not a bug workaround. It was introduced because Caddy can only serve WordPress static files if the site's Docker volume is mounted directly into the Caddy container. With N sites, that means:

1. N volume mounts on a single Caddy container
2. Caddy container restart required for each new site (Docker doesn't support adding volume mounts to running containers)
3. All sites' files accessible inside one container (breaks isolation)

The sidecar model gives each site its own nginx with its own single volume mount. Caddy only does hostname-based reverse proxying and never touches PHP or WordPress files. Adding a new site never requires changes to the Caddy container.

This rationale is documented in `architecture_migration.md` (lines 59-67) and `ARCHITECTURE.md` (section 6.4).

### What Changes

Replace per-site nginx containers with a **single shared nginx** serving all sites. Keep per-site PHP containers for isolation. This trades the per-site nginx isolation for density.

```
Current:  Caddy → nginx_<site>:80 → php_<site>:9000    (2 containers/site)
Phase 2:  Caddy → shared-nginx:80 → php_<site>:9000    (1 container/site)
```

### How It Works

1. All WordPress files live in `/srv/wordpress/<site>/` on app-01's host filesystem (bind mounts, not Docker volumes)
2. One nginx container mounts the parent directory: `/srv/wordpress:/srv/wordpress:ro`
3. Per-site vhost configs route `<site>.cowsaidmoo.tech` → serve static from `/srv/wordpress/<site>/`, proxy PHP to `php_<site>:9000`
4. Adding a new site: create directory, start PHP container, write vhost config, `nginx -s reload` — **no nginx container restart needed** (unlike Caddy, nginx supports config reload without adding volume mounts)

### Capacity

```
Shared nginx:   ~200 MiB (one instance for all sites)

With 384 MiB PHP (conservative, no load test needed):
Max sites:      (14,500 - 200) / 384 ≈ ~37

With 256 MiB PHP (requires load testing first):
Max sites:      (14,500 - 200) / 256 ≈ ~55
```

### What's Preserved

- Per-site PHP process isolation (hard memory/CPU/PID limits)
- Per-site PHP version (each container can run a different image)
- Independent PHP restarts (restart one site's PHP container, others unaffected)
- Separate databases per site
- Individual backup/restore per site

### What's Lost

- Per-site nginx isolation (shared nginx crash affects all static file serving — but nginx almost never crashes)
- Independent nginx restarts (reload bounces briefly for all sites — but nginx reload is near-instant, <100ms)
- Docker volume isolation for WordPress files (now on host filesystem with Linux permissions instead of separate volumes)

### Code Changes

| File | Change | Effort |
|------|--------|--------|
| `provisioner.go` | Stop creating nginx container. Write vhost config to shared nginx via CopyToContainer. Use bind mount for PHP container instead of Docker volume. | High |
| `destroyer.go` | Stop removing nginx container. Remove vhost from shared nginx. Remove site directory. | Medium |
| `backupper.go` | Tar site directory directly instead of via ephemeral alpine container (simpler). | Medium |
| `naming.go` | Remove `NginxContainerName`. Add `SiteDir(site)` for `/srv/wordpress/<site>/`. | Low |
| `caddy.go` | `reverse_proxy nginx_<site>:80` → `reverse_proxy shared-nginx:80` (or Caddy handles vhost routing directly and shared nginx is the single upstream). | Low |
| `n8n_provisioner.go` | No change (n8n has no nginx sidecar already). | None |
| Docker Compose on app-01 | Add shared nginx container definition with parent directory mount. | Low |

### Risk

| Scenario | Likelihood | Impact | Mitigation |
|----------|-----------|--------|------------|
| Shared nginx crashes | Very low (nginx is extremely stable) | All sites lose static file serving until restart | `restart: unless-stopped` policy, auto-recovers in seconds |
| nginx reload hiccup | Low | Sub-100ms blip across all sites | nginx reload is designed for zero-downtime |
| Cross-site file read via PHP | Low (requires exploit) | Attacker reads another site's files | PHP `open_basedir` per site restricts filesystem access |

---

## 5. Phase 3: Idle Suspension (Scale-to-Zero)

### The Insight

Most sites are idle most of the time. A portfolio site gets maybe 10 visits/day. It doesn't need 256 MiB of PHP-FPM sitting in RAM 24/7. If you could stop idle containers and start them on demand, you'd decouple density from RAM entirely.

This is what Fly.io, Railway, and Cloudflare Workers do — **scale to zero**.

### How It Works

```
Request arrives at Caddy for idle-site.cowsaidmoo.tech
  → Caddy checks: is php_<site> running?
  → No → return "Loading..." page (HTTP 503 with Retry-After: 3)
  → Trigger control plane: POST /api/sites/<site>/wake
  → Control plane: docker start php_<site> (1-3 seconds)
  → Client retries after 3 seconds → site is live

Background sweeper (every 30 minutes):
  → For each ACTIVE site, check last request time (via Caddy access logs or metric)
  → If no traffic in 30 minutes → docker stop php_<site>
  → Container stays, volume stays, just not running
```

### Capacity

```
200 total sites, ~20 active at any time
Active sites:  20 × 256 MiB = 5,120 MiB
Idle sites:    180 × 0 MiB  = 0
Total:         5,120 MiB (well within 14.5 GiB)
Max sites:     Effectively unlimited by RAM (limited by disk/DB)
```

### What's Preserved

**Everything.** Each site still has its own container with hard resource limits. Full isolation. Independent restarts. Per-site PHP version. Nothing changes architecturally — sites just get paused when nobody is using them.

### What's Lost

**Cold start latency**: First request after idle takes 1-3 seconds (Docker container start). Subsequent requests are instant. Users see a brief loading state.

### Code Changes

| Component | Change | Effort |
|-----------|--------|--------|
| Caddy config | Add a fallback route that returns 503 + "Loading" page when upstream is down | Low |
| New: `waker.go` | API endpoint `POST /api/sites/:site/wake` that starts the PHP container | Low |
| New: `sweeper.go` | Background goroutine that stops idle containers every 30 minutes | Medium |
| `provisioner.go` | No change — containers are created exactly as today | None |
| `destroyer.go` | No change | None |
| `backupper.go` | Must start container before volume backup (or backup from host path if Phase 2 done) | Low |
| Monitoring | Track wake latency, idle-to-active transitions, container state | Medium |

### Caddy "Loading" Behavior

Caddy's `reverse_proxy` directive already handles this natively:

```
<site>.<baseDomain> {
    reverse_proxy php_<site>:9000 {
        @unavailable status 502 503
        handle_response @unavailable {
            header Retry-After "3"
            respond "Site is waking up, please refresh in a moment." 503
        }
    }
}
```

When the PHP container is stopped, Caddy gets a connection refused, returns the loading page, and the client retries.

### Risk

| Scenario | Likelihood | Impact | Mitigation |
|----------|-----------|--------|------------|
| Cold start latency annoying for users | Medium | 1-3 second delay on first visit | Loading page with auto-refresh. Warm up known-active sites. |
| Webhook misses (WooCommerce, Razorpay) | Medium | Payment webhook arrives while site is asleep | Wake site on ANY request including webhooks. 3s start is within most webhook retry windows. |
| Backup runs on stopped container | Certain | Volume backup fails | Start container before backup, or tar host directory directly |
| Sweep race with active request | Low | Container stopped while request in-flight | Check last-request timestamp with grace period |

---

## 6. Combining Phases

The phases stack. You can mix and match:

| Combination | Max Sites | Per-Site RAM | Isolation |
|-------------|-----------|-------------|-----------|
| Current | ~22 | 640 MiB reserved | Full container |
| Phase 1 conservative | ~33 | 432 MiB reserved | Full container |
| Phase 1 aggressive (load-tested) | ~47 | 304 MiB reserved | Full container |
| Phase 1 + 2 | ~37-55 | 256-384 MiB reserved | Container PHP, shared nginx |
| Phase 1 + 3 | 200+ | 384 MiB active, 0 idle | Full container |
| Phase 1 + 2 + 3 | 500+ | 256 MiB active, 0 idle | Container PHP, shared nginx |

---

## 7. Recommendation

**Parked until 20 customers.** At 10 sites on a 22-site capacity, there's no immediate density pressure.

When the time comes, the progression is:
- **~18-20 sites**: Evaluate Phase 1. Start with the nginx-only reduction (128 → 48 MiB, zero risk). Load test before touching PHP limits.
- **20-35 sites**: Phase 1 conservative (384 MiB PHP + 48 MiB nginx) buys time.
- **35-55 sites**: Phase 2 (drop nginx sidecar) OR add a second app-02 VM (simpler, ~11 GiB spare RAM on pve).
- **55+ sites**: Phase 3 (idle suspension) — the only option that scales to 200+ without adding hardware.

Each phase is a ~1 week increment. They don't need to be done together.

**Alternative to any of this:** Add `app-02` VM on the Proxmox host. You have 11 GiB unused RAM and idle CPU. That doubles capacity with zero code changes — just point the control plane at a second Docker host. This is likely cheaper than any code refactor until you're past 40+ sites.
