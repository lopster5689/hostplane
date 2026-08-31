package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

type N8nProvisioner struct {
	docker *client.Client
	cfg    Config
	db     *DB
}

func NewN8nProvisioner(docker *client.Client, cfg Config, db *DB) *N8nProvisioner {
	return &N8nProvisioner{docker: docker, cfg: cfg, db: db}
}

// n8nPlan maps a plan name to container resources. "starter" is the cheapest (1 GiB).
// heapMB feeds NODE_OPTIONS --max-old-space-size, kept under `memory` so V8 garbage-collects
// before the cgroup OOM-kills the container (graceful degradation on the small plan).
type n8nPlan struct {
	memory   int64 // bytes
	nanoCPUs int64
	heapMB   int
}

var n8nPlans = map[string]n8nPlan{
	"starter":  {memory: 1 << 30, nanoCPUs: 1_000_000_000, heapMB: 768},  // light use, no AI nodes
	"business": {memory: 4 << 30, nanoCPUs: 2_000_000_000, heapMB: 3072}, // AI nodes, several workflows
	"scale":    {memory: 8 << 30, nanoCPUs: 4_000_000_000, heapMB: 6144}, // heavy/AI-heavy (regular mode for now)
}

func resolveN8nPlan(plan string) n8nPlan {
	if p, ok := n8nPlans[plan]; ok {
		return p
	}
	return n8nPlans["starter"] // default to the cheapest plan
}

func (p *N8nProvisioner) Run(site, plan string) error {
	if p.cfg.N8nPGDSN == "" {
		return fmt.Errorf("N8N_PG_DSN not configured — cannot provision n8n sites")
	}

	dbName := N8nDatabaseName(site)
	dbUser := N8nDatabaseUser(site)
	volName := N8nVolumeName(site)
	containerName := N8nContainerName(site)
	domain := SiteDomain(site, p.cfg.BaseDomain)

	var encKeyStored, dbPassStored, dbCreated, volCreated, containerCreated, caddyWritten bool

	rollback := func(reason error) error {
		log.Printf("[n8n-rollback] triggered for %s: %v", site, reason)

		// Reverting the Caddy snippet is always safe — it's pure routing config.
		if caddyWritten {
			p.removeCaddyConfig(site)
			reloadCaddy(p.cfg)
		}

		// Tearing down the container, volume, and Postgres DB is ONLY safe for a
		// brand-new site. encKeyStored is true only when we minted a fresh encryption
		// key this run, which means the site had never been provisioned before and so
		// holds no customer data. On a re-provision of an existing site (key reused),
		// these must be preserved — a failed retry must never drop a live customer's
		// workflows. Note dbCreated alone is NOT proof we created the DB: createPGDatabase
		// is idempotent (CREATE DATABASE || true), so it reports success on a pre-existing
		// DB too. The encKeyStored gate is what makes the drop safe.
		if encKeyStored {
			if containerCreated {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				p.docker.ContainerStop(ctx, containerName, container.StopOptions{})
				p.docker.ContainerRemove(ctx, containerName, types.ContainerRemoveOptions{Force: true})
				cancel()
			}
			if volCreated {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				p.docker.VolumeRemove(ctx, volName, true)
				cancel()
			}
			if dbCreated {
				p.dropPGDatabase(dbName, dbUser)
			}
			p.db.SetEncryptionKey(site, "")
		}

		// Always clear a password we generated THIS run: clearing only forces a
		// regenerate on the next attempt (never destroys data), and it avoids leaving
		// a stored password that may not match a preserved Postgres role.
		if dbPassStored {
			p.db.SetDBPassword(site, "")
		}

		return fmt.Errorf("n8n provisioning failed (rolled back): %w", reason)
	}

	// Step 1: Reuse the site's persisted encryption key if one exists, else generate.
	// The key MUST be stable across the site's entire lifetime — it encrypts every
	// stored credential, so a re-provision or a restore that minted a fresh key would
	// leave all previously-saved credentials undecryptable (spec §7). Only a key we
	// generate here is eligible for rollback; a pre-existing one belongs to a prior
	// successful provision and must survive.
	encKey, err := p.db.GetEncryptionKey(site)
	if err != nil {
		return fmt.Errorf("read encryption key: %w", err)
	}
	if encKey == "" {
		encKey, err = generateEncryptionKey()
		if err != nil {
			return rollback(fmt.Errorf("generate encryption key: %w", err))
		}
		if err := p.db.SetEncryptionKey(site, encKey); err != nil {
			return rollback(fmt.Errorf("store encryption key: %w", err))
		}
		encKeyStored = true
	}

	// Step 1b: Reuse the persisted RANDOM db password if present, else generate one.
	// Random (never deterministic) because this instance holds the customer's
	// third-party credentials — a predictable DB password is exfil-prone. Sticky
	// because createPGDatabase is idempotent (CREATE USER ... || true) and does NOT
	// reset an existing role's password, so a fresh password on re-provision would
	// mismatch the real Postgres role and n8n could not connect.
	dbPass, err := p.db.GetDBPassword(site)
	if err != nil {
		return rollback(fmt.Errorf("read db password: %w", err))
	}
	if dbPass == "" {
		dbPass, err = generateDBPassword()
		if err != nil {
			return rollback(fmt.Errorf("generate db password: %w", err))
		}
		if err := p.db.SetDBPassword(site, dbPass); err != nil {
			return rollback(fmt.Errorf("store db password: %w", err))
		}
		dbPassStored = true
	}

	// Step 2: Create PostgreSQL database and user on state-01
	if err := p.createPGDatabase(dbName, dbUser, dbPass); err != nil {
		return rollback(fmt.Errorf("createPGDatabase: %w", err))
	}
	dbCreated = true

	// Step 3: Create Docker volume
	if err := p.createVolume(volName); err != nil {
		return rollback(fmt.Errorf("createVolume: %w", err))
	}
	volCreated = true

	// Step 4: Start n8n container (sized per plan)
	if err := p.createContainer(containerName, volName, dbName, dbUser, dbPass, encKey, domain, resolveN8nPlan(plan)); err != nil {
		return rollback(fmt.Errorf("createContainer: %w", err))
	}
	containerCreated = true

	// Step 4b: Health gate — wait until n8n is actually serving before we route to
	// it. n8n runs its DB migrations on first boot (~30-60s on a fresh Postgres), so
	// routing traffic before it's ready would surface 502s. Failure here rolls back;
	// the rollback only tears down container/volume/DB for a brand-new site (see the
	// encKeyStored gate above), so a re-provision that fails here preserves a live
	// customer's data, and the worker's retry re-runs idempotently.
	if err := p.pollN8nHealth(containerName, 90*time.Second); err != nil {
		return rollback(fmt.Errorf("n8n health gate: %w", err))
	}

	// Step 5: Write Caddy snippet
	if err := p.writeCaddyConfig(site, containerName, domain); err != nil {
		return rollback(fmt.Errorf("writeCaddyConfig: %w", err))
	}
	caddyWritten = true

	// Step 6: Reload Caddy
	if err := reloadCaddy(p.cfg); err != nil {
		return rollback(fmt.Errorf("reloadCaddy: %w", err))
	}

	// Step 7: Poll TLS cert (non-fatal)
	certStatus := PollCaddyCert(p.docker, p.cfg, domain, 30*time.Second)
	log.Printf("[n8n-provisioner] site=%s cert_status=%s", site, certStatus)

	// Step 8: Register the domain as a CF-for-SaaS custom hostname (non-fatal).
	registerSaaSHostname(p.cfg, domain)

	return nil
}

func generateEncryptionKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateDBPassword returns a random 48-char hex string — safe to embed in SQL and env vars
// (no quoting hazards).
func generateDBPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// pollN8nHealth waits until the n8n container reports healthy on its /healthz
// endpoint or the timeout elapses. The container publishes no host port, so the
// only path from the control plane is to exec a probe inside the container.
// n8n's own Docker images ship BusyBox wget (their documented healthcheck uses
// it), so `wget --spider` against localhost is the reliable in-container probe.
func (p *N8nProvisioner) pollN8nHealth(containerName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		exit, err := p.execHealthCheck(containerName)
		if err == nil && exit == 0 {
			return nil
		}
		// Exit 127 = the probe binary (wget) is missing from the image. Retrying can
		// never fix that, and it would otherwise burn the full timeout before a fatal
		// rollback — fail fast with a clear, distinct signal instead.
		if exit == 127 {
			return fmt.Errorf("health probe binary (wget) missing in image %s — cannot health-gate n8n", p.cfg.N8nImage)
		}
		lastErr = err
		time.Sleep(3 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("n8n not healthy within %s (last probe error: %w)", timeout, lastErr)
	}
	return fmt.Errorf("n8n not healthy within %s", timeout)
}

// execHealthCheck runs a single /healthz probe inside the container and returns the
// probe's exit code (0 = HTTP 200; 127 = wget absent). A non-nil error means the
// Docker exec itself failed, distinct from a non-zero probe exit.
func (p *N8nProvisioner) execHealthCheck(containerName string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	execResp, err := p.docker.ContainerExecCreate(ctx, containerName, types.ExecConfig{
		Cmd: []string{"wget", "-q", "--spider", "http://127.0.0.1:5678/healthz"},
	})
	if err != nil {
		return -1, err
	}
	if err := p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{}); err != nil {
		return -1, err
	}
	for {
		inspect, err := p.docker.ContainerExecInspect(ctx, execResp.ID)
		if err != nil {
			return -1, err
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// createPGDatabase creates a PostgreSQL database and user for an n8n site
// by running psql in an ephemeral postgres:16 container on the wp_backend network.
func (p *N8nProvisioner) createPGDatabase(dbName, dbUser, dbPass string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminUser, adminPass, pgHost, pgPort, _ := parsePGDSN(p.cfg.N8nPGDSN)

	containerName := fmt.Sprintf("n8n_pgsetup_%s_%d", dbName, time.Now().UnixNano())

	// Shell script that:
	// 1. Creates user if not exists (via DO block)
	// 2. Creates database if not exists (via shell conditional)
	// 3. Grants privileges
	// Create user + database via plain psql. Uses || true for idempotency
	// (CREATE USER/DATABASE don't support IF NOT EXISTS before PG 17).
	// Database is owned by admin; n8n user gets full privileges via GRANT.
	psqlCmd := fmt.Sprintf("psql -h %s -p %s -U %s -d postgres", pgHost, pgPort, adminUser)
	script := fmt.Sprintf(`%s -c "CREATE USER %s WITH PASSWORD '%s'" 2>/dev/null || true
%s -c "CREATE DATABASE %s" 2>/dev/null || true
%s -c "GRANT ALL PRIVILEGES ON DATABASE %s TO %s"
%s -d %s -c "GRANT ALL ON SCHEMA public TO %s"`,
		psqlCmd, dbUser, dbPass,
		psqlCmd, dbName,
		psqlCmd, dbName, dbUser,
		psqlCmd, dbName, dbUser,
	)

	createResp, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "postgres:16",
			Cmd:   []string{"bash", "-c", script},
			Env:   []string{"PGPASSWORD=" + adminPass},
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(p.cfg.DockerNetwork),
			AutoRemove:  false,
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create psql setup container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		p.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	if err := p.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start psql setup container: %w", err)
	}

	statusCh, errCh := p.docker.ContainerWait(ctx, createResp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return fmt.Errorf("wait for psql setup: %w", err)
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("psql setup exited with code %d", status.StatusCode)
		}
	case <-ctx.Done():
		return fmt.Errorf("psql setup timed out: %w", ctx.Err())
	}

	log.Printf("[n8n-provisioner] site=%s PG database %s and user %s created", dbName, dbName, dbUser)
	return nil
}

// ensureN8nNetwork makes sure the shared n8n Docker network exists. It is normally
// created by the docker Ansible role (and Caddy joins it via its compose file), but
// we create it idempotently here so a provision never fails on a host where that
// step has not run yet. The control plane deliberately does NOT attach Caddy to the
// network — Caddy joins it statically via compose so the attachment survives a Caddy
// recreate. If Caddy is not on this network, reverse_proxy to n8n_<site> won't
// resolve, so that infra step is a hard dependency.
func (p *N8nProvisioner) ensureN8nNetwork() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := p.docker.NetworkInspect(ctx, p.cfg.N8nDockerNetwork, types.NetworkInspectOptions{}); err == nil {
		return nil
	}
	if _, err := p.docker.NetworkCreate(ctx, p.cfg.N8nDockerNetwork, types.NetworkCreate{Driver: "bridge"}); err != nil {
		return fmt.Errorf("create n8n network %s: %w", p.cfg.N8nDockerNetwork, err)
	}
	log.Printf("[n8n-provisioner] created docker network %s", p.cfg.N8nDockerNetwork)
	return nil
}

func (p *N8nProvisioner) createVolume(volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := p.docker.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName})
	return err
}

func (p *N8nProvisioner) createContainer(containerName, volumeName, dbName, dbUser, dbPass, encKey, domain string, tier n8nPlan) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// n8n instances live on their own network, isolated from the WordPress site
	// containers on wp_backend (n8n holds customer API creds + makes arbitrary
	// outbound HTTP). Caddy joins this network statically via its compose file.
	// Ensure it exists before BOTH the create and the idempotent restart path — a
	// pruned network would otherwise fail the start of an existing container.
	if err := p.ensureN8nNetwork(); err != nil {
		return err
	}

	// Idempotent — if container exists, just start it
	_, err := p.docker.ContainerInspect(ctx, containerName)
	if err == nil {
		return p.docker.ContainerStart(ctx, containerName, types.ContainerStartOptions{})
	}

	_, _, pgHost, pgPort, _ := parsePGDSN(p.cfg.N8nPGDSN)

	pids := int64(200)

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: p.cfg.N8nImage,
			Env: []string{
				"DB_TYPE=postgresdb",
				"DB_POSTGRESDB_HOST=" + pgHost,
				"DB_POSTGRESDB_PORT=" + pgPort,
				"DB_POSTGRESDB_DATABASE=" + dbName,
				"DB_POSTGRESDB_USER=" + dbUser,
				"DB_POSTGRESDB_PASSWORD=" + dbPass,
				"N8N_ENCRYPTION_KEY=" + encKey,
				"N8N_HOST=" + domain,
				"N8N_PORT=5678",
				"N8N_PROTOCOL=https",
				"N8N_PROXY_HOPS=1",
				"N8N_SECURE_COOKIE=true",
				"WEBHOOK_URL=https://" + domain + "/",
				"N8N_EDITOR_BASE_URL=https://" + domain + "/",
				"GENERIC_TIMEZONE=Asia/Kolkata",
				"N8N_DIAGNOSTICS_ENABLED=false",
				// keep the DB bounded + cap the V8 heap so the small (1 GiB) plan degrades
				// gracefully instead of getting OOM-killed under a heavy workflow
				"EXECUTIONS_DATA_PRUNE=true",
				"EXECUTIONS_DATA_MAX_AGE=336",
				fmt.Sprintf("NODE_OPTIONS=--max-old-space-size=%d", tier.heapMB),
			},
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: volumeName,
					Target: "/home/node/.n8n",
				},
			},
			Resources: container.Resources{
				Memory:    tier.memory,
				NanoCPUs:  tier.nanoCPUs,
				PidsLimit: &pids,
			},
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				p.cfg.N8nDockerNetwork: {},
			},
		},
		nil,
		containerName,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	return p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
}

func (p *N8nProvisioner) writeCaddyConfig(site, containerName, defaultDomain string, customDomain ...string) error {
	hosts := defaultDomain
	if len(customDomain) > 0 && customDomain[0] != "" {
		hosts = defaultDomain + ", " + customDomain[0] + ", www." + customDomain[0]
	}

	conf := fmt.Sprintf("%s {\n    encode gzip\n    reverse_proxy %s:5678\n}\n", hosts, containerName)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte(conf)
	tw.WriteHeader(&tar.Header{
		Name:    CaddyConfFile(site),
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	})
	tw.Write(content)
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureCaddyConfDir(ctx, p.docker, p.cfg.CaddyContainer, p.cfg.CaddyConfDir); err != nil {
		return err
	}

	return p.docker.CopyToContainer(ctx, p.cfg.CaddyContainer,
		p.cfg.CaddyConfDir, &buf, types.CopyToContainerOptions{})
}

// dropPGDatabase removes a PostgreSQL database and user via an ephemeral psql container.
func (p *N8nProvisioner) dropPGDatabase(dbName, dbUser string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminUser, adminPass, pgHost, pgPort, _ := parsePGDSN(p.cfg.N8nPGDSN)

	// Two SEPARATE -c flags, NOT one combined string: psql wraps a single multi-statement
	// -c in one implicit transaction, and DROP DATABASE cannot run inside a transaction
	// block (it would error and orphan both the DB and the role). Repeated -c options run
	// as separate transactions. WITH (FORCE) (PG 13+, supported by postgres:16) terminates
	// any lingering n8n session so the drop can't hang on an open connection.
	dropDB := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName)
	dropUser := fmt.Sprintf("DROP USER IF EXISTS %s", dbUser)
	containerName := fmt.Sprintf("n8n_pgdrop_%s_%d", dbName, time.Now().UnixNano())

	createResp, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "postgres:16",
			Cmd:   []string{"psql", "-h", pgHost, "-p", pgPort, "-U", adminUser, "-d", "postgres", "-c", dropDB, "-c", dropUser},
			Env:   []string{"PGPASSWORD=" + adminPass},
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(p.cfg.DockerNetwork),
			AutoRemove:  false,
		},
		nil, nil, containerName,
	)
	if err != nil {
		log.Printf("[n8n-rollback] cannot create psql drop container: %v", err)
		return
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		p.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	if err := p.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		log.Printf("[n8n-rollback] cannot start psql drop container: %v", err)
		return
	}

	statusCh, errCh := p.docker.ContainerWait(ctx, createResp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		log.Printf("[n8n-rollback] psql drop wait error for %s: %v — DB/user may be orphaned, check manually", dbName, err)
	case status := <-statusCh:
		if status.StatusCode != 0 {
			log.Printf("[n8n-rollback] WARNING: psql drop for %s exited %d — DB/user may be orphaned, check manually", dbName, status.StatusCode)
		} else {
			log.Printf("[n8n-rollback] dropped PG database %s and user %s", dbName, dbUser)
		}
	case <-ctx.Done():
		log.Printf("[n8n-rollback] psql drop for %s timed out — DB/user may be orphaned, check manually", dbName)
	}
}

func (p *N8nProvisioner) removeCaddyConfig(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := p.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := p.docker.ContainerExecCreate(ctx, p.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		log.Printf("[n8n-rollback] cannot create exec for caddy config removal: %v", err)
		return
	}
	p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
	log.Printf("[n8n-rollback] removed caddy config for %s", site)
}
