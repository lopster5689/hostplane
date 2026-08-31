package main

import (
	"log"
	"os"
	"strings"
)

type Config struct {
	// API
	APIPort string
	APIKey  string

	// Databases
	ControlDSN   string // controlplane DB (jobs, sites)
	WordPressDSN string // root-level DSN to create wp_ databases

	// Docker
	DockerHost    string
	DockerCertDir string // path to TLS certs for app-01

	// Caddy
	CaddyConfDir      string // path to per-site snippet dir inside Caddy container
	CaddyContainer    string // Docker container name for Caddy
	CaddyStaticVolume string // shared Docker volume name mounted at /srv/sites in Caddy

	// WordPress
	WordPressImage string // Docker image for PHP containers (default: hostplane/wordpress:php8.2-fpm)

	// Domain
	BaseDomain string

	// Infrastructure
	AppServerIP           string // IP of the app server (containers + caddy)
	PublicIP              string // Public VPS IP — custom domain A records must point here
	DockerNetwork         string // Docker network for site containers
	CloudflaredConfigPath string // path to cloudflared config.yml
	TunnelName            string // Cloudflare tunnel name
	ServiceTarget         string // upstream service URL for tunnel ingress

	// Worker
	WorkerPollInterval int // seconds
	StuckJobTimeout    int // minutes

	// Backup (R2 / Cloudflare)
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string

	// PostgreSQL backup (hosto DB on ***REMOVED***)
	PostgresDSN string // postgres://user:pass@host:port/dbname

	// RequireBackupBeforeDestroy gates destroy jobs on a successful pre-destroy
	// backup. Defaults to true. Set REQUIRE_BACKUP_BEFORE_DESTROY=false to
	// skip during development / debugging when R2 is not yet configured.
	RequireBackupBeforeDestroy bool

	// n8n
	N8nImage         string // Docker image for n8n containers
	N8nPGDSN         string // PostgreSQL admin DSN on state-01 for creating per-site n8n databases
	N8nDockerNetwork string // Docker network for n8n containers (separate from wp_backend); Caddy must also join it

	// Cloudflare-for-SaaS custom hostname registration (public reachability of
	// <site>.<BaseDomain> through the customers.hosto.app wildcard). Empty
	// token/zone disables it. Origin empty = the zone's fallback origin.
	CFSaaSAPIToken     string
	CFSaaSZoneID       string
	CFSaaSOriginServer string
}

func LoadConfig() Config {
	return Config{
		APIPort:                    getEnv("API_PORT", "8080"),
		APIKey:                     mustEnv("API_KEY"),
		ControlDSN:                 getEnv("CONTROL_DSN", "control:***REMOVED***@tcp(***REMOVED***:3306)/controlplane"),
		WordPressDSN:               getEnv("WP_DSN", "control:***REMOVED***@tcp(***REMOVED***:3306)/"),
		DockerHost:                 getEnv("DOCKER_HOST", "tcp://***REMOVED***:2376"),
		DockerCertDir:              getEnv("DOCKER_CERT_DIR", "/opt/control/certs"),
		CaddyConfDir:               getEnv("CADDY_CONF_DIR", "/etc/caddy/sites"),
		CaddyContainer:             getEnv("CADDY_CONTAINER", "caddy"),
		CaddyStaticVolume:          getEnv("CADDY_STATIC_VOLUME", "caddy_static_sites"),
		WordPressImage:             getEnv("WORDPRESS_IMAGE", "hostplane/wordpress:php8.2-fpm"),
		BaseDomain:                 getEnv("BASE_DOMAIN", "hosto.com"),
		AppServerIP:                getEnv("APP_SERVER_IP", "***REMOVED***"),
		PublicIP:                   getEnv("PUBLIC_IP", "***REMOVED***"),
		DockerNetwork:              getEnv("DOCKER_NETWORK", "wp_backend"),
		CloudflaredConfigPath:      getEnv("CLOUDFLARED_CONFIG", "/etc/cloudflared/config.yml"),
		TunnelName:                 getEnv("TUNNEL_NAME", "hosto"),
		ServiceTarget:              getEnv("TUNNEL_SERVICE_TARGET", "http://***REMOVED***:8080"),
		WorkerPollInterval:         3,
		StuckJobTimeout:            10,
		R2AccountID:                getEnv("R2_ACCOUNT_ID", ""),
		R2AccessKeyID:              getEnv("R2_ACCESS_KEY_ID", ""),
		R2SecretAccessKey:          getEnv("R2_SECRET_ACCESS_KEY", ""),
		R2Bucket:                   getEnv("R2_BUCKET", "hostplane-backups"),
		RequireBackupBeforeDestroy: getEnvBool("REQUIRE_BACKUP_BEFORE_DESTROY", true),
		PostgresDSN:                getEnv("POSTGRES_DSN", ""),
		// Pin a concrete version — never :latest. n8n runs DB migrations on boot,
		// so an unpinned tag can silently jump a major (e.g. 1.x→2.x) on container
		// recreate and irreversibly migrate a customer's DB. Override per instance.
		N8nImage:           getEnv("N8N_IMAGE", "n8nio/n8n:2.27.4"),
		N8nPGDSN:           getEnv("N8N_PG_DSN", ""),
		N8nDockerNetwork:   getEnv("N8N_DOCKER_NETWORK", "n8n_backend"),
		CFSaaSAPIToken:     getEnv("CF_SAAS_API_TOKEN", ""),
		CFSaaSZoneID:       getEnv("CF_SAAS_ZONE_ID", ""),
		CFSaaSOriginServer: getEnv("CF_SAAS_ORIGIN", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required env var %s is not set", key)
	}
	return v
}

func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "true" || v == "1" || v == "yes"
}

func (c Config) DBHost() string {
	// Extract just host:port from the full DSN
	// e.g. "control:pass@tcp(***REMOVED***:3306)/" → "***REMOVED***:3306"
	start := strings.Index(c.WordPressDSN, "tcp(") + 4
	end := strings.Index(c.WordPressDSN, ")/")
	if start < 4 || end < 0 {
		return "***REMOVED***:3306"
	}
	return c.WordPressDSN[start:end]
}
