package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// dateRe validates the YYYY-MM-DD format used for restore date parameters.
var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Backupper handles site backups to R2. It mirrors the Provisioner pattern:
// same Docker client, same Config, same naming helpers.
type Backupper struct {
	docker *client.Client
	cfg    Config
	r2     *R2Client // may be nil when R2 is not configured
	db     *DB       // control-plane DB — for ListSites and UpdateLastBackupAt
}

func NewBackupper(docker *client.Client, cfg Config, r2 *R2Client, db *DB) *Backupper {
	return &Backupper{docker: docker, cfg: cfg, r2: r2, db: db}
}

// resolveResourceNames returns the DB name and volume name for a site based on
// its type. Avoids repeated DB lookups by accepting the already-fetched Site.
func resolveResourceNames(s *Site) (dbName, volName string) {
	switch s.SiteType {
	case "n8n":
		return N8nDatabaseName(s.Site), N8nVolumeName(s.Site)
	default:
		return WPDatabaseName(s.Site), VolumeName(s.Site)
	}
}

// BackupSite backs up the database and volume for a single site.
// Both must succeed — the first failure aborts and returns an error.
// Called by Destroyer.Run() before any destructive step.
func (b *Backupper) BackupSite(site string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot back up site %s", site)
	}

	s, err := b.db.GetSite(site)
	if err != nil {
		return fmt.Errorf("get site for backup: %w", err)
	}

	if s.SiteType == "static" {
		log.Printf("[backupper] site=%s type=static — skipping backup (no DB/volume)", site)
		return nil
	}

	_, volName := resolveResourceNames(s)

	if s.SiteType == "n8n" {
		if err := b.backupN8nDatabase(site); err != nil {
			return fmt.Errorf("n8n database backup failed: %w", err)
		}
	} else {
		if err := b.BackupDatabase(site); err != nil {
			return fmt.Errorf("database backup failed: %w", err)
		}
	}

	if err := b.backupVolumeByName(site, volName); err != nil {
		return fmt.Errorf("volume backup failed: %w", err)
	}

	if err := b.db.UpdateLastBackupAt(site, time.Now().UTC()); err != nil {
		log.Printf("[backupper] site=%s warning: could not update last_backup_at: %v", site, err)
	}

	log.Printf("[backupper] site=%s backup complete (db + volume)", site)
	return nil
}

// backupN8nDatabase runs pg_dump for an n8n site's PostgreSQL database and
// streams the gzip-compressed output directly to R2.
func (b *Backupper) backupN8nDatabase(site string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dbName := N8nDatabaseName(site)
	dbUser := N8nDatabaseUser(site)
	dbPass, err := b.db.GetDBPassword(site)
	if err != nil {
		return fmt.Errorf("get n8n db password: %w", err)
	}
	_, _, pgHost, pgPort, _ := parsePGDSN(b.cfg.N8nPGDSN)

	containerName := fmt.Sprintf("backup_n8ndb_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "postgres:16",
			Cmd: []string{
				"pg_dump",
				"-h", pgHost,
				"-p", pgPort,
				"-U", dbUser,
				"-F", "p",
				"--no-password",
				dbName,
			},
			Env:          []string{"PGPASSWORD=" + dbPass},
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(b.cfg.DockerNetwork),
			AutoRemove:  false,
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create pg_dump container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false,
	})
	if err != nil {
		return fmt.Errorf("attach pg_dump container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start pg_dump container: %w", err)
	}

	rawStream := stripDockerMux(attachResp.Reader)

	pr, pw := io.Pipe()
	gzDone := make(chan error, 1)
	go func() {
		gz := gzip.NewWriter(pw)
		_, copyErr := io.Copy(gz, rawStream)
		gz.Close()
		pw.CloseWithError(copyErr)
		gzDone <- copyErr
	}()

	key := keyForDB(site, dateStamp())
	uploadErr := b.r2.Upload(ctx, key, pr, "application/gzip")
	<-gzDone

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		if uploadErr == nil {
			b.r2.deleteObject(context.Background(), key)
		}
		return fmt.Errorf("wait for pg_dump container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, fmt.Sprintf("pg_dump n8n site=%s", site))
		if uploadErr == nil {
			if delErr := b.r2.deleteObject(context.Background(), key); delErr != nil {
				log.Printf("[backupper] site=%s WARNING: corrupt backup object %s could not be deleted: %v", site, key, delErr)
			}
		}
		return fmt.Errorf("pg_dump exited with code %d — backup aborted", exitCode)
	}

	if uploadErr != nil {
		return fmt.Errorf("upload n8n DB backup to R2: %w", uploadErr)
	}

	log.Printf("[backupper] site=%s n8n DB backup → %s", site, key)
	return nil
}

// BackupAll backs up every known active site. A single site failure is logged
// but never stops other sites from being backed up.
func (b *Backupper) BackupAll() {
	sites, err := b.db.ListSites()
	if err != nil {
		log.Printf("[backupper] BackupAll: cannot list sites: %v", err)
		return
	}

	ok, failed := 0, 0
	for _, s := range sites {
		// Only back up sites that actually have data
		if s.Status == "DESTROYED" || s.Status == "FAILED" || s.Status == "CREATED" {
			continue
		}
		if err := b.BackupSite(s.Site); err != nil {
			log.Printf("[backupper] BackupAll: site=%s FAILED: %v", s.Site, err)
			failed++
			continue // never stops the loop
		}
		ok++
	}
	log.Printf("[backupper] BackupAll complete: %d ok, %d failed", ok, failed)
}

// BackupDatabase runs mysqldump on state-01 for the site's database and streams
// the gzip-compressed output directly to R2. No disk writes on control-01.
//
// Mechanism: spawns a short-lived mysql:8 container on app-01 (which is already
// on the wp_backend network and can reach state-01). Stdout is piped via Docker
// ContainerAttach → stripDockerMux → gzip → R2.
//
// After the upload finishes, ContainerWait is used to verify the exit code.
// If mysqldump exited non-zero (e.g., wrong credentials, DB not found), any
// partial/empty object already uploaded to R2 is deleted and an error is returned.
func (b *Backupper) BackupDatabase(site string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dbName := WPDatabaseName(site)
	dbUser, dbPass := parseDSNCredentials(b.cfg.WordPressDSN)
	dbHostPort := b.cfg.DBHost() // e.g. "***REMOVED***:3306"
	dbHost, dbPort, err := net.SplitHostPort(dbHostPort)
	if err != nil {
		dbHost = dbHostPort
		dbPort = "3306"
	}

	containerName := fmt.Sprintf("backup_db_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "mariadb:10",
			Cmd: []string{
				"mysqldump",
				"-h", dbHost,
				"-P", dbPort,
				"-u", dbUser,
				fmt.Sprintf("-p%s", dbPass),
				"--single-transaction", // consistent snapshot for InnoDB (WordPress default)
				"--quick",
				dbName,
			},
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(b.cfg.DockerNetwork),
			AutoRemove:  false, // removed explicitly in defer so we can call ContainerWait first
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create mysqldump container: %w", err)
	}

	// Ensure cleanup regardless of outcome
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	// Attach BEFORE start to capture all output from the beginning
	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false, // captured separately via ContainerLogs on failure
	})
	if err != nil {
		return fmt.Errorf("attach mysqldump container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start mysqldump container: %w", err)
	}

	// Strip Docker mux headers to get raw stdout SQL text
	rawStream := stripDockerMux(attachResp.Reader)

	// Gzip on-the-fly: rawStream → gzip.Writer → pr (pipe reader sent to R2)
	pr, pw := io.Pipe()
	gzDone := make(chan error, 1)
	go func() {
		gz := gzip.NewWriter(pw)
		_, copyErr := io.Copy(gz, rawStream)
		gz.Close()
		pw.CloseWithError(copyErr)
		gzDone <- copyErr
	}()

	key := keyForDB(site, dateStamp())
	uploadErr := b.r2.Upload(ctx, key, pr, "application/gzip")
	<-gzDone // wait for gzip goroutine to finish before checking results

	// By this point the container has exited (stdout stream closure = container done).
	// Check the exit code to detect silent mysqldump failures.
	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		// Cannot confirm success — treat as failure and clean up any upload
		if uploadErr == nil {
			b.r2.deleteObject(context.Background(), key) //nolint
		}
		return fmt.Errorf("wait for mysqldump container: %w", waitErr)
	}
	if exitCode != 0 {
		dumpStderr := b.logContainerStderr(createResp.ID, fmt.Sprintf("mysqldump site=%s", site))
		if uploadErr == nil {
			// The partial/empty object was successfully stored — delete it so R2
			// never contains a corrupt backup that looks valid.
			if delErr := b.r2.deleteObject(context.Background(), key); delErr != nil {
				log.Printf("[backupper] site=%s WARNING: corrupt backup object %s could not be deleted: %v", site, key, delErr)
			} else {
				log.Printf("[backupper] site=%s corrupt backup object %s deleted from R2", site, key)
			}
		}
		return dumpFailureError(exitCode, dumpStderr)
	}

	if uploadErr != nil {
		return fmt.Errorf("upload DB backup to R2: %w", uploadErr)
	}

	log.Printf("[backupper] site=%s DB backup → %s", site, key)
	return nil
}

// BackupVolume creates a tar.gz of the wp_<site> Docker volume and streams it
// directly to R2. No disk writes on control-01.
//
// Mechanism: spawns a short-lived alpine container on app-01 with the site
// volume mounted read-only. Runs: tar -czf - -C /data . so stdout is the
// complete tar.gz. Streamed via ContainerAttach → stripDockerMux → R2.
//
// ContainerWait is used after upload to verify tar exited cleanly.
// If non-zero, the uploaded object is deleted from R2 before returning error.
func (b *Backupper) BackupVolume(site string) error {
	return b.backupVolumeByName(site, VolumeName(site))
}

func (b *Backupper) backupVolumeByName(site, volumeName string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	containerName := fmt.Sprintf("backup_vol_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image:        "alpine:latest",
			Cmd:          []string{"tar", "-czf", "-", "-C", "/data", "."},
			AttachStdout: true,
			AttachStderr: false,
		},
		&container.HostConfig{
			AutoRemove: false,
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   volumeName,
					Target:   "/data",
					ReadOnly: true,
				},
			},
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create volume backup container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false,
	})
	if err != nil {
		return fmt.Errorf("attach volume backup container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start volume backup container: %w", err)
	}

	// tar -czf already produces gzip output — stream directly to R2
	rawStream := stripDockerMux(attachResp.Reader)

	key := keyForVolume(site, dateStamp())
	uploadErr := b.r2.Upload(ctx, key, rawStream, "application/x-tar")

	// Verify tar exited cleanly
	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		if uploadErr == nil {
			b.r2.deleteObject(context.Background(), key) //nolint
		}
		return fmt.Errorf("wait for volume backup container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, fmt.Sprintf("tar site=%s", site))
		if uploadErr == nil {
			if delErr := b.r2.deleteObject(context.Background(), key); delErr != nil {
				log.Printf("[backupper] site=%s WARNING: corrupt volume object %s could not be deleted: %v", site, key, delErr)
			} else {
				log.Printf("[backupper] site=%s corrupt volume object %s deleted from R2", site, key)
			}
		}
		return fmt.Errorf("tar exited with code %d — backup aborted", exitCode)
	}

	if uploadErr != nil {
		return fmt.Errorf("upload volume backup to R2: %w", uploadErr)
	}

	log.Printf("[backupper] site=%s volume backup → %s", site, key)
	return nil
}

// RestoreSite downloads and restores both the database and volume from the
// backup taken on :date (YYYY-MM-DD).
//
// Flow:
//  1. Validate date format
//  2. Preflight: verify both backup objects exist in R2 before any data is touched
//  3. RestoreDatabase — download .sql.gz → decompress → pipe to mysql:8 container stdin
//  4. RestoreVolume   — download .tar.gz → pipe to alpine tar container stdin
//
// NOTE: the site's running containers are NOT stopped before restore. For a
// safe production restore, quiesce writes beforehand. For WordPress sites
// this is typically acceptable — mysqldump includes DROP TABLE IF EXISTS so
// the import is idempotent at the table level.
func (b *Backupper) RestoreSite(site, date string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot restore site %s", site)
	}
	if !dateRe.MatchString(date) {
		return fmt.Errorf("invalid date format %q — expected YYYY-MM-DD", date)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Preflight: verify both objects exist before any destructive work
	dbKey := keyForDB(site, date)
	volKey := keyForVolume(site, date)
	if err := b.r2.objectExists(ctx, dbKey); err != nil {
		return fmt.Errorf("DB backup not found in R2 (%s): %w", dbKey, err)
	}
	if err := b.r2.objectExists(ctx, volKey); err != nil {
		return fmt.Errorf("volume backup not found in R2 (%s): %w", volKey, err)
	}

	s, err := b.db.GetSite(site)
	if err != nil {
		return fmt.Errorf("get site for restore: %w", err)
	}

	if s.SiteType == "n8n" {
		if err := b.restoreN8nDatabase(ctx, site, date); err != nil {
			return fmt.Errorf("n8n database restore: %w", err)
		}
	} else {
		if err := b.RestoreDatabase(ctx, site, date); err != nil {
			return fmt.Errorf("database restore: %w", err)
		}
	}

	_, volName := resolveResourceNames(s)
	if err := b.restoreVolumeByName(ctx, site, date, volName); err != nil {
		return fmt.Errorf("volume restore: %w", err)
	}

	log.Printf("[backupper] site=%s restored from %s (db + volume)", site, date)
	return nil
}

// restoreN8nDatabase downloads a .sql.gz from R2 and pipes it into an
// ephemeral psql container to restore an n8n site's PostgreSQL database.
func (b *Backupper) restoreN8nDatabase(ctx context.Context, site, date string) error {
	key := keyForDB(site, date)

	rc, err := b.r2.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("download n8n DB backup %s: %w", key, err)
	}
	defer rc.Close()

	gzReader, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("decompress n8n DB backup: %w", err)
	}
	defer gzReader.Close()

	dbName := N8nDatabaseName(site)
	dbUser := N8nDatabaseUser(site)
	dbPass, err := b.db.GetDBPassword(site)
	if err != nil {
		return fmt.Errorf("get n8n db password: %w", err)
	}
	_, _, pgHost, pgPort, _ := parsePGDSN(b.cfg.N8nPGDSN)

	containerName := fmt.Sprintf("restore_n8ndb_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "postgres:16",
			Cmd: []string{
				"psql",
				"-h", pgHost,
				"-p", pgPort,
				"-U", dbUser,
				"-d", dbName,
			},
			Env:         []string{"PGPASSWORD=" + dbPass},
			AttachStdin: true,
			OpenStdin:   true,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(b.cfg.DockerNetwork),
			AutoRemove:  false,
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create psql restore container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
	})
	if err != nil {
		return fmt.Errorf("attach psql restore container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start psql restore container: %w", err)
	}

	if _, err := io.Copy(attachResp.Conn, gzReader); err != nil {
		return fmt.Errorf("stream SQL to psql: %w", err)
	}
	attachResp.CloseWrite()

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		return fmt.Errorf("wait for psql restore: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, fmt.Sprintf("psql restore n8n site=%s", site))
		return fmt.Errorf("psql restore exited with code %d", exitCode)
	}

	log.Printf("[backupper] site=%s n8n DB restored from %s", site, key)
	return nil
}

// RestoreDatabase downloads the .sql.gz from R2 for the given site/date,
// decompresses it, and pipes the SQL into an ephemeral mysql:8 container
// running on app-01 (same network as BackupDatabase). The container has stdin
// open; we stream the SQL then close stdin to signal EOF to mysql.
func (b *Backupper) RestoreDatabase(ctx context.Context, site, date string) error {
	key := keyForDB(site, date)

	rc, err := b.r2.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("download DB backup %s: %w", key, err)
	}
	defer rc.Close()

	gzReader, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("decompress DB backup: %w", err)
	}
	defer gzReader.Close()

	dbName := WPDatabaseName(site)
	dbUser, dbPass := parseDSNCredentials(b.cfg.WordPressDSN)
	dbHostPort := b.cfg.DBHost()
	dbHost, dbPort, splitErr := net.SplitHostPort(dbHostPort)
	if splitErr != nil {
		dbHost = dbHostPort
		dbPort = "3306"
	}

	containerName := fmt.Sprintf("restore_db_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "mariadb:10",
			Cmd: []string{
				"mysql",
				"-h", dbHost,
				"-P", dbPort,
				"-u", dbUser,
				fmt.Sprintf("-p%s", dbPass),
				dbName,
			},
			AttachStdin:  true,
			AttachStdout: false,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    true, // close stdin when the attached client disconnects
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(b.cfg.DockerNetwork),
			AutoRemove:  false,
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create mysql restore container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: false,
		Stderr: false, // captured via logContainerStderr on failure
	})
	if err != nil {
		return fmt.Errorf("attach mysql restore container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start mysql restore container: %w", err)
	}

	// Stream decompressed SQL into the container's stdin
	if _, err := io.Copy(attachResp.Conn, gzReader); err != nil {
		return fmt.Errorf("stream SQL to restore container: %w", err)
	}
	// Signal EOF to mysql by closing the write side of the connection
	if err := attachResp.CloseWrite(); err != nil {
		log.Printf("[backupper] site=%s db restore: CloseWrite warning: %v", site, err)
	}

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		return fmt.Errorf("wait for mysql restore container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, fmt.Sprintf("mysql restore site=%s", site))
		return fmt.Errorf("mysql restore exited with code %d", exitCode)
	}

	log.Printf("[backupper] site=%s DB restored from R2 key %s", site, key)
	return nil
}

// RestoreVolume downloads the .tar.gz from R2 for the given site/date and
// pipes it into an ephemeral alpine container that extracts it into the site's
// Docker volume (mounted read-write at /data).
func (b *Backupper) RestoreVolume(ctx context.Context, site, date string) error {
	return b.restoreVolumeByName(ctx, site, date, VolumeName(site))
}

func (b *Backupper) restoreVolumeByName(ctx context.Context, site, date, volumeName string) error {
	key := keyForVolume(site, date)

	rc, err := b.r2.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("download volume backup %s: %w", key, err)
	}
	defer rc.Close()
	containerName := fmt.Sprintf("restore_vol_%s_%d", site, time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image:        "alpine:latest",
			Cmd:          []string{"tar", "-xzf", "-", "-C", "/data"},
			AttachStdin:  true,
			AttachStdout: false,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    true,
		},
		&container.HostConfig{
			AutoRemove: false,
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   volumeName,
					Target:   "/data",
					ReadOnly: false, // writable for restore
				},
			},
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create volume restore container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true})
	}()

	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: false,
		Stderr: false,
	})
	if err != nil {
		return fmt.Errorf("attach volume restore container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start volume restore container: %w", err)
	}

	// Stream the tar.gz directly — tar -xzf handles decompression
	if _, err := io.Copy(attachResp.Conn, rc); err != nil {
		return fmt.Errorf("stream tar to restore container: %w", err)
	}
	if err := attachResp.CloseWrite(); err != nil {
		log.Printf("[backupper] site=%s volume restore: CloseWrite warning: %v", site, err)
	}

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		return fmt.Errorf("wait for volume restore container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, fmt.Sprintf("tar restore site=%s", site))
		return fmt.Errorf("tar restore exited with code %d", exitCode)
	}

	log.Printf("[backupper] site=%s volume restored from R2 key %s", site, key)
	return nil
}

// waitContainer waits for a container to stop and returns its exit code.
// Safe to call after the container has already exited — returns immediately.
func (b *Backupper) waitContainer(ctx context.Context, containerID string) (int64, error) {
	statusCh, errCh := b.docker.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return -1, err
	case status := <-statusCh:
		if status.Error != nil {
			return -1, fmt.Errorf("%s", status.Error.Message)
		}
		return status.StatusCode, nil
	}
}

// logContainerStderr fetches and logs up to 4 KB of stderr from an exited
// container. Used for post-mortem diagnostics when a backup container fails.
// Reads the Docker multiplexed log stream directly — cannot reuse stripDockerMux
// because that function filters to stdout (type 1) only.
// errBackupSourceDBMissing marks a dump failure caused by the site database
// not existing at all — the destroyer treats this as "nothing to back up"
// instead of aborting (zombie sites are exactly the ones we most need gone).
var errBackupSourceDBMissing = errors.New("site database does not exist")

// dumpFailureError classifies a mysqldump failure by its stderr.
func dumpFailureError(exitCode int64, stderr string) error {
	if strings.Contains(stderr, "Unknown database") {
		return fmt.Errorf("mysqldump exited with code %d (%s): %w", exitCode, stderr, errBackupSourceDBMissing)
	}
	return fmt.Errorf("mysqldump exited with code %d — backup aborted", exitCode)
}

func (b *Backupper) logContainerStderr(containerID, label string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logsReader, err := b.docker.ContainerLogs(ctx, containerID, types.ContainerLogsOptions{
		ShowStdout: false,
		ShowStderr: true,
	})
	if err != nil {
		log.Printf("[backupper] %s: could not fetch stderr: %v", label, err)
		return ""
	}
	defer logsReader.Close()

	// Docker mux format: [stream_type(1)] [pad(3)] [size(4)] [data...]
	// Since ShowStdout=false, every frame here is type 2 (stderr).
	var buf strings.Builder
	header := make([]byte, 8)
	for buf.Len() < 4096 {
		if _, err := io.ReadFull(logsReader, header); err != nil {
			break // EOF or connection drop — stop reading
		}
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		remaining := int64(4096 - buf.Len())
		if size <= remaining {
			data := make([]byte, size)
			io.ReadFull(logsReader, data) //nolint
			buf.Write(data)
		} else {
			data := make([]byte, remaining)
			io.ReadFull(logsReader, data) //nolint
			buf.Write(data)
			io.CopyN(io.Discard, logsReader, size-remaining) //nolint
		}
	}
	if buf.Len() > 0 {
		log.Printf("[backupper] %s stderr: %s", label, strings.TrimSpace(buf.String()))
	}
	return strings.TrimSpace(buf.String())
}
