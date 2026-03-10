# Infrastructure Backup Plan

> **Date: 2026-03-04** | Separate from site-level user backups

---

## Should this be separate from user backups?

**Yes — separate implementation, same R2 bucket, different prefix.**

| Dimension            | User backups                     | Infra backups                              |
| -------------------- | -------------------------------- | ------------------------------------------ |
| What                 | `wp_<site>` DB + volume per site | `controlplane` DB + Docker TLS certs       |
| Cadence              | Daily                            | Hourly (DB), daily (certs)                 |
| Retention            | 30 days                          | 90 days                                    |
| Size                 | MBs–GBs per site                 | < 1 MB total                               |
| Restore path         | API-driven, self-service         | Manual emergency procedure                 |
| Who triggers restore | Operator (site issue)            | Operator (control-01 catastrophic failure) |
| Pre-destroy gate     | Yes                              | No                                         |

Combining them into the same code path would couple the cadences and blur the
failure semantics. A separate `InfraBackupper` and `InfraBackupWorker` are clean
and mirror the existing separation between `Backupper` and `BackupWorker`.

---

## What to back up

### 1. `controlplane` database — **critical**

The `sites` and `jobs` tables on `state-01`. This is the complete state of the
control plane: every site, its domain, status, custom domain, last backup
timestamp. Without it, after a control-01 rebuild you have no record of what
sites exist and the provisioner/destroyer have no source of truth.

**Cadence:** hourly — the `jobs` table changes with every provision/destroy and
is cheap to dump (< 100 KB typically).

**Key:** `infra/controlplane/2026-03-04T14:00:00Z.sql.gz`

**Method:** same ephemeral `mysql:8` container pattern already used for user DB
backups. Uses the existing `ControlDSN` from config rather than `WordPressDSN`.

### 2. Docker TLS certs — **critical**

`/opt/control/certs/` on control-01 contains the client certificate, key, and
CA cert used to authenticate to `app-01`'s Docker daemon over TLS. Without
these after a control-01 rebuild, you cannot issue any Docker commands to app-01
until you regenerate and re-trust them — which requires console access to app-01.

**Cadence:** daily — certs don't change unless manually rotated.

**Key:** `infra/certs/2026-03-04.tar.gz`

**Method:** read local directory directly from control-01 filesystem (no
container needed), tar + gzip in memory, upload to R2.

### 3. `.env` — ⚠️ special case, NOT to R2

`/opt/control/.env` contains `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY`.
Storing it in R2 creates a circular dependency: you need the credentials to
access R2, but the credentials are in R2.

**Solution — two copies:**

1. **Proxmox host filesystem** — write a copy to `/root/hostplane-env-backup.env`
   on the Proxmox host (not the LXC guest). The LXC guest can die; the Proxmox
   host persists.
2. **Password manager** — keep a current copy in 1Password / Bitwarden under a
   "hostplane infrastructure" entry. Update it whenever `.env` changes.

The binary can emit a startup warning if `.env` was last modified more than N
days ago without a copy being saved, but this is low priority.

---

## R2 Bucket Layout (additions)

```
hostplane-backups/
├── databases/          ← existing: user DB backups (daily)
├── volumes/            ← existing: user volume backups (daily)
└── infra/              ← new
    ├── controlplane/
    │   ├── 2026-03-04T14:00:00Z.sql.gz
    │   ├── 2026-03-04T15:00:00Z.sql.gz
    │   └── 2026-03-04T16:00:00Z.sql.gz
    └── certs/
        └── 2026-03-04.tar.gz
```

---

## Implementation

### New files

#### `infra_backupper.go`

```go
type InfraBackupper struct {
    docker *client.Client
    cfg    Config
    r2     *R2Client
}

func NewInfraBackupper(docker *client.Client, cfg Config, r2 *R2Client) *InfraBackupper

// BackupControlplaneDB dumps the controlplane database to R2.
// Key: infra/controlplane/<RFC3339-UTC>.sql.gz
func (b *InfraBackupper) BackupControlplaneDB(ctx context.Context) error

// BackupDockerCerts tars /opt/control/certs and uploads to R2.
// Key: infra/certs/<YYYY-MM-DD>.tar.gz
func (b *InfraBackupper) BackupDockerCerts(ctx context.Context) error
```

**`BackupControlplaneDB` mechanics:**

Spawn ephemeral `mysql:8` container on app-01 (same pattern as user DB backup),
but pointed at the `controlplane` database using `ControlDSN` credentials:

```go
dbName  := "controlplane"
dbUser, dbPass := parseDSNCredentials(b.cfg.ControlDSN)
dbHost, dbPort, _ := net.SplitHostPort(b.cfg.DBHost())
// cmd: mysqldump -h <host> -P <port> -u <user> -p<pass>
//      --single-transaction --set-gtid-purged=OFF controlplane
```

Key uses RFC3339 UTC timestamp (not just a date) so hourly runs don't overwrite
each other:

```go
func infraDBKey() string {
    return "infra/controlplane/" + time.Now().UTC().Format("2006-01-02T15:04:05Z") + ".sql.gz"
}
```

**`BackupDockerCerts` mechanics:**

Read the cert directory directly from the local filesystem on control-01 — no
Docker container needed. Use `archive/tar` + `compress/gzip` to stream the
directory into an `io.Pipe`, upload the pipe reader to R2:

```go
certDir := b.cfg.DockerCertDir  // "/opt/control/certs"
// Walk certDir, write each file into a tar.gz pipe → r2.Upload()
```

This is simpler than a container: the files are local, small, and don't require
Docker to access.

#### `infra_backup_worker.go`

```go
type InfraBackupWorker struct {
    backupper *InfraBackupper
}

func (w *InfraBackupWorker) Start() {
    // Immediate run on startup
    w.runAll()

    // DB: every hour
    dbTicker := time.NewTicker(1 * time.Hour)

    // Certs: every 24h
    certTicker := time.NewTicker(24 * time.Hour)

    // Cleanup: every 7 days
    cleanTicker := time.NewTicker(7 * 24 * time.Hour)

    for {
        select {
        case <-dbTicker.C:
            w.backupper.BackupControlplaneDB(context.Background())
        case <-certTicker.C:
            w.backupper.BackupDockerCerts(context.Background())
        case <-cleanTicker.C:
            // Keep 90 days of controlplane DB dumps, 30 days of certs
            w.backupper.r2.DeleteOlderThan(ctx, "infra/controlplane/", 90)
            w.backupper.r2.DeleteOlderThan(ctx, "infra/certs/", 30)
        }
    }
}

func (w *InfraBackupWorker) runAll() {
    ctx := context.Background()
    w.backupper.BackupControlplaneDB(ctx)
    w.backupper.BackupDockerCerts(ctx)
}
```

### `main.go` additions

```go
if r2 != nil {
    // existing user backup worker
    go NewBackupWorker(backupper, cfg).Start()

    // new infra backup worker
    go NewInfraBackupWorker(NewInfraBackupper(docker, cfg, r2)).Start()
}
```

### No new API endpoints needed

Infra backup is fully automated — no on-demand API. Restore is a manual
emergency procedure (see Restore Runbook below).

---

## Restore Runbook (manual — break-glass)

This is for the scenario where control-01 is completely lost.

### Step 1 — Recover `.env`

Get the `.env` from Proxmox host or password manager. You need at minimum:

- `API_KEY`
- `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET`
- `DOCKER_HOST`, `DOCKER_CERT_DIR`
- `CONTROL_DSN`, `WP_DSN`

### Step 2 — Rebuild control-01 LXC

Provision a new LXC on Proxmox, install Go, copy the binary or rebuild from
git, place `.env` at `/opt/control/.env`.

### Step 3 — Restore Docker TLS certs

```bash
# Find the latest cert backup
aws s3 ls s3://hostplane-backups/infra/certs/ \
  --endpoint-url https://<ACCOUNT_ID>.r2.cloudflarestorage.com | tail -1

# Download and extract to cert dir
aws s3 cp s3://hostplane-backups/infra/certs/2026-03-04.tar.gz - \
  --endpoint-url https://<ACCOUNT_ID>.r2.cloudflarestorage.com \
  | tar -xzf - -C /opt/control/certs/
```

### Step 4 — Restore `controlplane` database

```bash
# Find the latest controlplane DB dump
aws s3 ls s3://hostplane-backups/infra/controlplane/ \
  --endpoint-url https://<ACCOUNT_ID>.r2.cloudflarestorage.com | tail -1

# Download, decompress, and pipe into MariaDB
aws s3 cp s3://hostplane-backups/infra/controlplane/2026-03-04T15:00:00Z.sql.gz - \
  --endpoint-url https://<ACCOUNT_ID>.r2.cloudflarestorage.com \
  | gunzip | mysql -h ***REMOVED*** -u control -p controlplane
```

### Step 5 — Start the service

```bash
systemctl start control-plane
# Verify health
curl -s https://api.cowsaidmoo.tech/api/health
# Verify sites are visible
curl -s -H "X-Api-Key: $KEY" https://api.cowsaidmoo.tech/api/sites | jq '.sites | length'
```

After this, all site records are restored. The actual site containers and
volumes on app-01 were never touched — they kept running through the control-01
outage.

---

## Key Risks

| Risk                                         | Mitigation                                                                                             |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `.env` lost with control-01                  | Copy to Proxmox host + password manager — do this now                                                  |
| R2 credentials in `.env` circular dependency | Never store `.env` in R2 — it must live elsewhere                                                      |
| controlplane DB on state-01 also dies        | state-01 itself should have MariaDB-level replication or Proxmox snapshot — out of scope for this plan |
| Hourly DB dumps accumulate fast              | 90-day retention + < 100 KB per dump = ~130 MB max — negligible R2 cost                                |
| Cert backup contains private key             | R2 bucket is private, access is via token — acceptable for LXC-contained infra                         |

---

## Implementation Order

1. `infra_backupper.go` — `BackupControlplaneDB` (uses existing container pattern, ~1hr)
2. `infra_backupper.go` — `BackupDockerCerts` (local file tar, ~30min)
3. `infra_backup_worker.go` — multi-ticker scheduler (~20min)
4. `main.go` — wire up alongside existing backup worker (~5min)
5. **Do immediately (no code)** — copy `.env` to Proxmox host and password manager

Total: ~2 hours. The `.env` copy takes 2 minutes and should be done before
writing any code.
