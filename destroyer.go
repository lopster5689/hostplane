package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	_ "github.com/go-sql-driver/mysql"
)

type Destroyer struct {
	docker    *client.Client
	cfg       Config
	backupper *Backupper
}

func NewDestroyer(docker *client.Client, cfg Config, backupper *Backupper) *Destroyer {
	return &Destroyer{docker: docker, cfg: cfg, backupper: backupper}
}

func (d *Destroyer) Run(site, siteType string) error {
	// ── Pre-destroy safety backup ─────────────────────────────────
	// Enabled by default. Set REQUIRE_BACKUP_BEFORE_DESTROY=false to skip
	// during development / debugging when R2 is not yet configured.
	if d.cfg.RequireBackupBeforeDestroy {
		if err := d.backupper.BackupSite(site); err != nil {
			if errors.Is(err, errBackupSourceDBMissing) {
				log.Printf("[destroyer] site=%s database does not exist — nothing to back up, proceeding with destroy", site)
			} else {
				return fmt.Errorf("pre-destroy backup failed, aborting destroy: %w", err)
			}
		}
	} else {
		log.Printf("[destroyer] site=%s REQUIRE_BACKUP_BEFORE_DESTROY=false — skipping pre-destroy backup", site)
	}

	switch siteType {
	case "n8n":
		return d.destroyN8n(site)
	case "static":
		return d.destroyStatic(site)
	default:
		return d.destroyWordPress(site)
	}
}

func (d *Destroyer) destroyWordPress(site string) error {
	dbName := WPDatabaseName(site)
	dbUser := WPDatabaseUser(site)
	volumeName := VolumeName(site)
	phpName := PHPContainerName(site)
	nginxName := NginxContainerName(site)

	if err := d.removeContainer(phpName); err != nil {
		return fmt.Errorf("removePhpContainer: %w", err)
	}
	if err := d.removeContainer(nginxName); err != nil {
		return fmt.Errorf("removeNginxContainer: %w", err)
	}
	if err := d.removeVolume(volumeName); err != nil {
		return fmt.Errorf("removeVolume: %w", err)
	}
	if err := d.removeCaddyConfig(site); err != nil {
		return fmt.Errorf("removeCaddyConfig: %w", err)
	}
	if err := reloadCaddy(d.cfg); err != nil {
		return fmt.Errorf("reloadCaddy: %w", err)
	}
	if err := d.dropDatabase(dbName, dbUser); err != nil {
		return fmt.Errorf("dropDatabase: %w", err)
	}
	return nil
}

func (d *Destroyer) destroyN8n(site string) error {
	containerName := N8nContainerName(site)
	volumeName := N8nVolumeName(site)
	dbName := N8nDatabaseName(site)
	dbUser := N8nDatabaseUser(site)

	if err := d.removeContainer(containerName); err != nil {
		return fmt.Errorf("removeN8nContainer: %w", err)
	}
	if err := d.removeVolume(volumeName); err != nil {
		return fmt.Errorf("removeVolume: %w", err)
	}
	if err := d.removeCaddyConfig(site); err != nil {
		return fmt.Errorf("removeCaddyConfig: %w", err)
	}
	if err := reloadCaddy(d.cfg); err != nil {
		return fmt.Errorf("reloadCaddy: %w", err)
	}
	// n8n uses PostgreSQL — drop via ephemeral psql container
	np := NewN8nProvisioner(d.docker, d.cfg, nil)
	np.dropPGDatabase(dbName, dbUser)
	return nil
}

func (d *Destroyer) destroyStatic(site string) error {
	sp := NewStaticProvisioner(d.docker, d.cfg)
	sp.removeStaticSiteFiles(site)
	if err := d.removeCaddyConfig(site); err != nil {
		return fmt.Errorf("removeCaddyConfig: %w", err)
	}
	return reloadCaddy(d.cfg)
}

func (d *Destroyer) removeContainer(phpName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := d.docker.ContainerRemove(ctx, phpName, types.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: false,
	})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (d *Destroyer) removeVolume(volumeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := d.docker.VolumeRemove(ctx, volumeName, true)
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (d *Destroyer) removeCaddyConfig(site string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confPath := d.cfg.CaddyConfDir + "/" + CaddyConfFile(site)
	execResp, err := d.docker.ContainerExecCreate(ctx, d.cfg.CaddyContainer, types.ExecConfig{
		Cmd: []string{"rm", "-f", confPath},
	})
	if err != nil {
		return err
	}

	return d.docker.ContainerExecStart(ctx, execResp.ID, types.ExecStartCheck{})
}

func (d *Destroyer) dropDatabase(dbName, dbUser string) error {
	db, err := sql.Open("mysql", d.cfg.WordPressDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		return fmt.Errorf("cannot reach DB: %w", err)
	}

	stmts := []string{
		"DROP DATABASE IF EXISTS " + dbName,
		fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", dbUser),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sql: %w", err)
		}
	}
	return nil
}
