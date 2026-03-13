package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// PGBackupper handles backup and restore for the PostgreSQL "hosto" database
// on server ***REMOVED***. It runs pg_dump / psql inside ephemeral postgres:16
// containers on app-01's Docker daemon, which routes to the PG server over the
// internet. Data streams directly to/from R2 — no disk writes on control-01.
type PGBackupper struct {
	docker *client.Client // app-01 Docker daemon — bridge mode reaches external IPs
	cfg    Config
	r2     *R2Client
	db     *DB
}

func NewPGBackupper(docker *client.Client, cfg Config, r2 *R2Client, db *DB) *PGBackupper {
	return &PGBackupper{docker: docker, cfg: cfg, r2: r2, db: db}
}

// BackupDatabase dumps the configured PostgreSQL database via an ephemeral
// postgres:16 container and streams the gzip-compressed output directly to R2.
//
// Flow: pg_dump stdout → ContainerAttach → stripDockerMux → gzip → R2
// R2 key: postgres/<dbname>/YYYY-MM-DDTHH-MM-SSZ.sql.gz
//
// Exit code is checked after upload; non-zero deletes the uploaded object so
// R2 never holds a corrupt backup.
func (b *PGBackupper) BackupDatabase(ctx context.Context) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot backup PostgreSQL database")
	}
	if b.cfg.PostgresDSN == "" {
		return fmt.Errorf("POSTGRES_DSN not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	pgUser, pgPass, pgHost, pgPort, pgDB := parsePGDSN(b.cfg.PostgresDSN)

	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	key := fmt.Sprintf("postgres/%s/%s.sql.gz", pgDB, ts)
	containerName := fmt.Sprintf("backup_pg_%d", time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "postgres:16",
			Cmd: []string{
				"pg_dump",
				"-h", pgHost,
				"-p", pgPort,
				"-U", pgUser,
				"-d", pgDB,
				"-F", "p", // plain SQL — directly pipeable to psql for restore
				"--no-password", // password supplied via PGPASSWORD env var
			},
			Env:          []string{"PGPASSWORD=" + pgPass},
			AttachStdout: true,
			AttachStderr: true,
		},
		&container.HostConfig{
			NetworkMode: "bridge", // bridge routes to external IPs (***REMOVED***)
			AutoRemove:  false,    // explicit remove after ContainerWait
		},
		nil, nil, containerName,
	)
	if err != nil {
		return fmt.Errorf("create pg_dump container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true}) //nolint
	}()

	// Attach BEFORE start so we capture all output from the beginning.
	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false, // captured on failure via logContainerStderr
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
		pw.CloseWithError(copyErr) // propagate error to Upload reader, not pw.Close()
		gzDone <- copyErr
	}()

	uploadErr := b.r2.Upload(ctx, key, pr, "application/gzip")
	<-gzDone

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		if uploadErr == nil {
			b.r2.deleteObject(context.Background(), key) //nolint
		}
		return fmt.Errorf("wait for pg_dump container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, "pg_dump "+pgDB)
		if uploadErr == nil {
			if delErr := b.r2.deleteObject(context.Background(), key); delErr != nil {
				log.Printf("[pg-backup] WARNING: corrupt backup %s could not be deleted: %v", key, delErr)
			} else {
				log.Printf("[pg-backup] corrupt backup %s deleted from R2", key)
			}
		}
		return fmt.Errorf("pg_dump exited with code %d — backup aborted", exitCode)
	}
	if uploadErr != nil {
		return fmt.Errorf("upload pg backup to R2: %w", uploadErr)
	}

	// Record successful backup time in the control DB
	if err := b.db.UpdatePGLastBackupAt(pgDB, time.Now().UTC(), key); err != nil {
		log.Printf("[pg-backup] WARNING: could not update last_backup_at: %v", err)
		// non-fatal — backup is in R2 regardless
	}

	log.Printf("[pg-backup] database=%s backup → %s", pgDB, key)
	return nil
}

// RestoreDatabase downloads the backup for the given date (YYYY-MM-DD) and
// pipes the decompressed SQL into an ephemeral psql container.
//
// Because backup keys include a full timestamp (YYYY-MM-DDTHH-MM-SSZ), `date`
// is used as a key prefix to find the first matching object in R2.
//
// NOTE: The target database must already exist on the PostgreSQL server.
// pg_dump -F p (plain SQL) does not include CREATE DATABASE — it only
// contains the schema and data. Run `createdb hosto` on pg-01 if needed.
func (b *PGBackupper) RestoreDatabase(ctx context.Context, date string) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot restore PostgreSQL database")
	}
	if b.cfg.PostgresDSN == "" {
		return fmt.Errorf("POSTGRES_DSN not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	pgUser, pgPass, pgHost, pgPort, pgDB := parsePGDSN(b.cfg.PostgresDSN)

	// Find the timestamped key for this date by listing with a date-prefix.
	prefix := fmt.Sprintf("postgres/%s/%s", pgDB, date)
	entries, err := b.r2.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list backups for date %s: %w", date, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no postgres backup found for date %s", date)
	}
	key := entries[0].Key

	rc, err := b.r2.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("download backup %s: %w", key, err)
	}
	defer rc.Close()

	gzReader, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("decompress backup: %w", err)
	}
	defer gzReader.Close()

	containerName := fmt.Sprintf("restore_pg_%d", time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "postgres:16",
			Cmd: []string{
				"psql",
				"-h", pgHost,
				"-p", pgPort,
				"-U", pgUser,
				"-d", pgDB,
				"--no-password",
			},
			Env:          []string{"PGPASSWORD=" + pgPass},
			AttachStdin:  true,
			AttachStdout: false,
			AttachStderr: true,
			OpenStdin:    true,
			StdinOnce:    true, // close stdin when the attached client disconnects
		},
		&container.HostConfig{
			NetworkMode: "bridge",
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
		Stdout: false,
		Stderr: false, // captured on failure via logContainerStderr
	})
	if err != nil {
		return fmt.Errorf("attach psql restore container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start psql restore container: %w", err)
	}

	// Stream decompressed SQL into psql stdin
	if _, err := io.Copy(attachResp.Conn, gzReader); err != nil {
		return fmt.Errorf("stream SQL to psql container: %w", err)
	}
	// Signal EOF so psql knows the SQL stream is complete
	if err := attachResp.CloseWrite(); err != nil {
		log.Printf("[pg-backup] psql restore: CloseWrite warning: %v", err)
	}

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		return fmt.Errorf("wait for psql restore container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, "psql restore "+pgDB)
		return fmt.Errorf("psql restore exited with code %d", exitCode)
	}

	log.Printf("[pg-backup] database=%s restored from %s", pgDB, key)
	return nil
}

// waitContainer blocks until the container exits and returns its exit code.
// Includes the status.Error check — Docker can report container-level errors
// with StatusCode 0.
func (b *PGBackupper) waitContainer(ctx context.Context, id string) (int64, error) {
	statusCh, errCh := b.docker.ContainerWait(ctx, id, container.WaitConditionNotRunning)
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

// logContainerStderr fetches and logs up to 4 KB of stderr, stripping Docker
// mux frame headers (8-byte prefix per chunk) so the output is plain text.
func (b *PGBackupper) logContainerStderr(id, label string) {
	logCtx, logCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer logCancel()
	rc, err := b.docker.ContainerLogs(logCtx, id, types.ContainerLogsOptions{
		ShowStdout: false,
		ShowStderr: true,
	})
	if err != nil {
		log.Printf("[pg-backup] %s: could not retrieve container stderr: %v", label, err)
		return
	}
	defer rc.Close()

	// Docker mux format: [stream_type(1)] [pad(3)] [size(4)] [data...]
	var buf strings.Builder
	header := make([]byte, 8)
	for buf.Len() < 4096 {
		if _, err := io.ReadFull(rc, header); err != nil {
			break
		}
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		remaining := int64(4096 - buf.Len())
		if size <= remaining {
			data := make([]byte, size)
			io.ReadFull(rc, data) //nolint
			buf.Write(data)
		} else {
			data := make([]byte, remaining)
			io.ReadFull(rc, data) //nolint
			buf.Write(data)
			io.CopyN(io.Discard, rc, size-remaining) //nolint
		}
	}
	if buf.Len() > 0 {
		log.Printf("[pg-backup] %s stderr: %s", label, strings.TrimSpace(buf.String()))
	}
}

// parsePGDSN extracts credentials and connection info from a postgres:// URL.
// Returns safe defaults if parsing fails.
func parsePGDSN(dsn string) (user, pass, host, port, dbname string) {
	u, err := url.Parse(dsn)
	if err != nil || u == nil || u.User == nil {
		return "pguser", "", "***REMOVED***", "5432", "hosto"
	}
	user = u.User.Username()
	pass, _ = u.User.Password()
	host = u.Hostname()
	if host == "" {
		host = "***REMOVED***"
	}
	port = u.Port()
	if port == "" {
		port = "5432"
	}
	dbname = strings.TrimPrefix(u.Path, "/")
	if dbname == "" {
		dbname = "hosto"
	}
	return
}

// pgDateFromKey extracts the YYYY-MM-DD portion from a postgres backup key.
// e.g. "postgres/hosto/2026-03-10T14-30-00Z.sql.gz" → "2026-03-10"
func pgDateFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	name = strings.TrimSuffix(name, ".sql.gz")
	if len(name) >= 10 {
		return name[:10]
	}
	return name
}
