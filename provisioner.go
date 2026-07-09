package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	_ "github.com/go-sql-driver/mysql"
)

type Provisioner struct {
	docker *client.Client
	cfg    Config
	db     *DB
}

func NewProvisioner(docker *client.Client, cfg Config, db *DB) *Provisioner {
	return &Provisioner{docker: docker, cfg: cfg, db: db}
}

func (p *Provisioner) Run(site string) error {
	dbName := WPDatabaseName(site)
	dbUser := WPDatabaseUser(site)
	dbPass := WPDatabasePass(site)
	volName := VolumeName(site)
	phpName := PHPContainerName(site)
	nginxName := NginxContainerName(site)
	domain := SiteDomain(site, p.cfg.BaseDomain)

	// Track what succeeded for rollback
	var dbCreated, volCreated, phpCreated, nginxCreated, caddyWritten bool

	// Rollback in reverse order
	rollback := func(reason error) error {
		log.Printf("[rollback] triggered for %s: %v", site, reason)

		if caddyWritten {
			p.removeCaddyConfig(site)
			reloadCaddy(p.cfg)
		}
		if nginxCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.ContainerStop(ctx, nginxName, container.StopOptions{})
			p.docker.ContainerRemove(ctx, nginxName, types.ContainerRemoveOptions{Force: true})
		}
		if phpCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.ContainerStop(ctx, phpName, container.StopOptions{})
			p.docker.ContainerRemove(ctx, phpName, types.ContainerRemoveOptions{Force: true})
		}
		if volCreated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p.docker.VolumeRemove(ctx, volName, true)
		}
		if dbCreated {
			p.dropDatabase(dbName, dbUser)
		}

		return fmt.Errorf("provisioning failed (rolled back): %w", reason)
	}

	// Step 1: Create database and user on state-01
	if err := p.createDatabase(dbName, dbUser, dbPass); err != nil {
		return rollback(fmt.Errorf("createDatabase: %w", err))
	}
	dbCreated = true

	// Step 2: Create wp_<site> Docker volume
	if err := p.createVolume(volName); err != nil {
		return rollback(fmt.Errorf("createVolume: %w", err))
	}
	volCreated = true

	// Step 3: Start PHP-FPM container (wordpress:php8.2-fpm, mounts wp_<site>)
	if err := p.createContainer(phpName, volName, dbName, dbUser, dbPass); err != nil {
		return rollback(fmt.Errorf("createPhpContainer: %w", err))
	}
	phpCreated = true

	// Step 3b: Inject PHP upload limits into the running container
	if err := p.writePHPUploadsConfig(phpName); err != nil {
		return rollback(fmt.Errorf("writePHPUploadsConfig: %w", err))
	}

	// Step 4: Start nginx sidecar (mounts same volume, serves static + proxies PHP)
	if err := p.createNginxContainer(nginxName, volName); err != nil {
		return rollback(fmt.Errorf("createNginxContainer: %w", err))
	}
	nginxCreated = true

	// Step 5: Write nginx server block into the sidecar and reload nginx
	if err := p.writeNginxConfig(nginxName, phpName, domain); err != nil {
		return rollback(fmt.Errorf("writeNginxConfig: %w", err))
	}

	// Step 6: Write per-site Caddy snippet (reverse_proxy → nginx sidecar)
	if err := p.writeCaddyConfig(site, nginxName, domain); err != nil {
		return rollback(fmt.Errorf("writeCaddyConfig: %w", err))
	}
	caddyWritten = true

	// Step 7: Reload Caddy — site goes live instantly
	if err := reloadCaddy(p.cfg); err != nil {
		return rollback(fmt.Errorf("reloadCaddy: %w", err))
	}

	// Step 8: Poll for TLS cert readiness (non-fatal — Caddy retries in background).
	// This prevents the job from completing while the cert is still pending,
	// giving the caller an accurate cert_status signal via GET /api/sites/:site.
	certStatus := PollCaddyCert(p.docker, p.cfg, domain, 30*time.Second)
	log.Printf("[provisioner] site=%s cert_status=%s", site, certStatus)

	// Step 9: Install WordPress core — users must NEVER see the 5-minute
	// install screen (the "666gg" class: sites that look provisioned but have
	// no core tables; it also broke the AI editor on every fresh site,
	// observed live 2026-07-09). Credentials are generated here, stored in
	// the control DB, surfaced by the dashboard.
	adminPass, err := p.installWordPressCore(site, phpName, domain)
	if err != nil {
		return rollback(fmt.Errorf("installWordPressCore: %w", err))
	}
	if p.db != nil {
		stored := false
		for attempt := 0; attempt < 3; attempt++ {
			if err := p.db.SetWPAdminPassword(site, adminPass); err == nil {
				stored = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !stored {
			log.Printf("[provisioner] site=%s WARNING: wp admin password could not be stored — user must reset via wp-cli", site)
		}
	}

	// Step 10: Pre-install WooCommerce so store builds never pay the install
	// detour. Non-fatal: prepare_store self-heals if this misses.
	if out, code, werr := p.execWP(phpName, 3*time.Minute,
		"wp plugin install woocommerce --activate --allow-root 2>&1"); werr != nil || code != 0 {
		log.Printf("[provisioner] site=%s woocommerce pre-install skipped: %v %s", site, werr, firstLine(out))
	} else {
		log.Printf("[provisioner] site=%s woocommerce pre-installed", site)
	}

	return nil
}

// installWordPressCore waits for the WP image to finish seeding the volume,
// then runs core install with generated credentials. Idempotent: an already-
// installed site returns the empty password and no error.
func (p *Provisioner) installWordPressCore(site, phpName, domain string) (string, error) {
	// Already installed? (re-provision / retry path)
	if _, code, err := p.execWP(phpName, 30*time.Second, "wp core is-installed --allow-root 2>&1"); err == nil && code == 0 {
		return "", nil
	}
	passBytes := make([]byte, 12)
	if _, err := rand.Read(passBytes); err != nil {
		return "", fmt.Errorf("generate admin password: %w", err)
	}
	adminPass := hex.EncodeToString(passBytes) // 24 hex chars
	installCmd := fmt.Sprintf(
		"wp core install --url=https://%s --title=%s --admin_user=admin --admin_password=%s --admin_email=admin@%s --skip-email --allow-root 2>&1",
		domain, shellQuote(site), adminPass, domain)
	deadline := time.Now().Add(2 * time.Minute)
	var lastOut string
	for time.Now().Before(deadline) {
		out, code, err := p.execWP(phpName, 60*time.Second, installCmd)
		if err == nil && code == 0 {
			log.Printf("[provisioner] site=%s wordpress core installed (admin)", site)
			return adminPass, nil
		}
		if strings.Contains(out, "already installed") {
			return adminPass, nil
		}
		lastOut = firstLine(out)
		// The image entrypoint may still be copying core / generating
		// wp-config on first boot — give it a moment and retry.
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("core install did not succeed in 2m: %s", lastOut)
}

// execWP runs a shell command inside the site's PHP container as www-data.
func (p *Provisioner) execWP(phpName string, timeout time.Duration, shcmd string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	execResp, err := p.docker.ContainerExecCreate(ctx, phpName, types.ExecConfig{
		User:         "33:33",
		Cmd:          []string{"sh", "-c", shcmd},
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   "/var/www/html",
	})
	if err != nil {
		return "", -1, fmt.Errorf("exec create: %w", err)
	}
	attachResp, err := p.docker.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return "", -1, fmt.Errorf("exec attach: %w", err)
	}
	defer attachResp.Close()
	raw := stripDockerMuxAll(attachResp.Reader)
	var buf strings.Builder
	if _, err := io.Copy(&buf, io.LimitReader(raw, 256*1024)); err != nil {
		return "", -1, fmt.Errorf("read output: %w", err)
	}
	inspect, err := p.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return strings.TrimSpace(buf.String()), -1, fmt.Errorf("exec inspect: %w", err)
	}
	return strings.TrimSpace(buf.String()), inspect.ExitCode, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (p *Provisioner) dropDatabase(dbName, dbUser string) {
	db, err := sql.Open("mysql", p.cfg.WordPressDSN)
	if err != nil {
		log.Printf("[rollback] cannot open DB connection: %v", err)
		return
	}
	defer db.Close()
	db.Exec("DROP DATABASE IF EXISTS " + dbName)
	db.Exec("DROP USER IF EXISTS '" + dbUser + "'@'%'")
	log.Printf("[rollback] dropped DB %s and user %s", dbName, dbUser)
}

func (p *Provisioner) removeCaddyConfig(site string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := p.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := p.docker.ContainerExecCreate(ctx, p.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		log.Printf("[rollback] cannot create exec for caddy config removal: %v", err)
		return
	}
	p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
	log.Printf("[rollback] removed caddy config for %s", site)
}

func (p *Provisioner) createDatabase(dbName, dbUser, dbPass string) error {
	db, err := sql.Open("mysql", p.cfg.WordPressDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		return fmt.Errorf("cannot reach DB: %w", err)
	}

	stmts := []string{
		"CREATE DATABASE IF NOT EXISTS " + dbName,
		fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", dbUser, dbPass),
		fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%%'", dbName, dbUser),
		"FLUSH PRIVILEGES",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sql(%s): %w", stmt[:20], err)
		}
	}
	return nil
}

func (p *Provisioner) createVolume(volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := p.docker.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName})
	return err
}

func (p *Provisioner) createContainer(phpName, volumeName, dbName, dbUser, dbPass string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Idempotent — container already exists, just ensure it's running
	_, err := p.docker.ContainerInspect(ctx, phpName)
	if err == nil {
		return p.docker.ContainerStart(ctx, phpName, types.ContainerStartOptions{})
	}

	pids := int64(100)

	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: p.cfg.WordPressImage,
			Env: []string{
				"WORDPRESS_DB_HOST=" + p.cfg.DBHost(),
				"WORDPRESS_DB_USER=" + dbUser,
				"WORDPRESS_DB_PASSWORD=" + dbPass,
				"WORDPRESS_DB_NAME=" + dbName,
			},
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeVolume,
					Source: volumeName,
					Target: "/var/www/html",
				},
			},
			Resources: container.Resources{
				Memory:    512 * 1024 * 1024,
				NanoCPUs:  1_000_000_000,
				PidsLimit: &pids,
			},
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				p.cfg.DockerNetwork: {},
			},
		},
		nil,
		phpName,
	)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	return p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
}

// writeCaddyConfig writes a per-site Caddy snippet into the CaddyConfDir inside
// the Caddy container. Caddy simply reverse-proxies by hostname to the site's
// nginx sidecar — no FastCGI from Caddy's side.
func (p *Provisioner) writeCaddyConfig(site, nginxName, defaultDomain string, customDomain ...string) error {
	hosts := defaultDomain
	if len(customDomain) > 0 && customDomain[0] != "" {
		hosts = defaultDomain + ", " + customDomain[0] + ", www." + customDomain[0]
	}

	conf := fmt.Sprintf("%s {\n    encode gzip\n    reverse_proxy %s:80\n}\n", hosts, nginxName)

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

// createNginxContainer starts an nginx:alpine sidecar for a site.
// It shares the same wp_<site> volume as the PHP-FPM container so nginx can
// serve static assets directly. The server block is written separately via
// writeNginxConfig after the container is running.
func (p *Provisioner) createNginxContainer(nginxName, volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Idempotent — container already exists, just ensure it's running
	_, err := p.docker.ContainerInspect(ctx, nginxName)
	if err == nil {
		return p.docker.ContainerStart(ctx, nginxName, types.ContainerStartOptions{})
	}

	pids := int64(50)
	resp, err := p.docker.ContainerCreate(
		ctx,
		&container.Config{
			Image: "nginx:alpine",
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   volumeName,
					Target:   "/var/www/html",
					ReadOnly: true,
				},
			},
			Resources: container.Resources{
				Memory:    128 * 1024 * 1024,
				NanoCPUs:  500_000_000,
				PidsLimit: &pids,
			},
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				p.cfg.DockerNetwork: {},
			},
		},
		nil,
		nginxName,
	)
	if err != nil {
		return fmt.Errorf("nginx container create: %w", err)
	}

	return p.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
}

// writeNginxConfig injects the nginx server block into the running nginx_<site>
// sidecar container and reloads nginx. The config routes static file requests
// directly from the WordPress volume and proxies PHP to the FPM container.
func (p *Provisioner) writeNginxConfig(nginxName, phpName, domain string) error {
	return p.writeNginxConfigWithDomains(nginxName, phpName, domain, "")
}

// writeNginxConfigWithDomains writes a multi-domain nginx server block.
// When customDomain is non-empty, server_name includes both domains and
// HTTP_HOST becomes $host so WordPress receives the correct hostname per request.
// When customDomain is empty, the config is a single-domain block with a
// hardcoded HTTP_HOST — used for initial provisioning and domain removal.
func (p *Provisioner) writeNginxConfigWithDomains(nginxName, phpName, defaultDomain, customDomain string) error {
	serverName := defaultDomain
	httpHost := defaultDomain
	if customDomain != "" {
		serverName = defaultDomain + " " + customDomain + " www." + customDomain
		httpHost = "$host"
	}

	conf := fmt.Sprintf(`server {
    listen 80;
    root /var/www/html;
    index index.php;

    server_name %s;

    client_max_body_size 500M;

    location / {
        try_files $uri $uri/ /index.php?$args;
    }

    location ~ \.php$ {
        fastcgi_pass %s:9000;
        fastcgi_index index.php;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param HTTPS on;
        fastcgi_param HTTP_HOST %s;
    }
}
`, serverName, phpName, httpHost)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte(conf)
	tw.WriteHeader(&tar.Header{
		Name:    "default.conf",
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	})
	tw.Write(content)
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := p.docker.CopyToContainer(ctx, nginxName,
		"/etc/nginx/conf.d/", &buf, types.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy nginx config: %w", err)
	}

	// Reload nginx to apply the new server block
	execResp, err := p.docker.ContainerExecCreate(ctx, nginxName, types.ExecConfig{
		Cmd: []string{"nginx", "-s", "reload"},
	})
	if err != nil {
		return fmt.Errorf("nginx reload exec create: %w", err)
	}
	return p.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
}

// writePHPUploadsConfig injects a custom uploads.ini into the running PHP-FPM
// container to raise PHP upload limits from the stock 2 MB default.
func (p *Provisioner) writePHPUploadsConfig(phpName string) error {
	conf := []byte("upload_max_filesize = 500M\npost_max_size = 500M\nmemory_limit = 512M\n")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{
		Name:    "uploads.ini",
		Mode:    0644,
		Size:    int64(len(conf)),
		ModTime: time.Now(),
	})
	tw.Write(conf)
	tw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return p.docker.CopyToContainer(ctx, phpName,
		"/usr/local/etc/php/conf.d/", &buf, types.CopyToContainerOptions{})
}

// updateWordPressURLs sets siteurl and home in wp_options so WordPress serves
// on the given URL. Call with "https://<customDomain>" when adding a custom
// domain and "https://<defaultDomain>" when removing one.
// Non-fatal if WordPress tables don't exist yet (site not installed).
func (p *Provisioner) updateWordPressURLs(site, url string) error {
	dsn := p.cfg.WordPressDSN + WPDatabaseName(site)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("open site DB: %w", err)
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		return fmt.Errorf("cannot reach site DB: %w", err)
	}

	for _, opt := range []string{"siteurl", "home"} {
		if _, err := db.Exec(
			"UPDATE wp_options SET option_value=? WHERE option_name=?",
			url, opt,
		); err != nil {
			return fmt.Errorf("update %s: %w", opt, err)
		}
	}

	log.Printf("[provisioner] site=%s wordpress URLs set to %s", site, url)
	return nil
}
