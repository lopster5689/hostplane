package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// InfraBackupper backs up infrastructure-level data to R2:
//   - The controlplane MariaDB database (hourly, 90-day retention)
//   - The Docker TLS certificates from the control server local filesystem
//     (daily, 30-day retention)
//
// No site data is touched; this is purely about the control-plane itself.
type InfraBackupper struct {
	docker *client.Client
	cfg    Config
	r2     *R2Client
}

func NewInfraBackupper(docker *client.Client, cfg Config, r2 *R2Client) *InfraBackupper {
	return &InfraBackupper{docker: docker, cfg: cfg, r2: r2}
}

// BackupControlplaneDB dumps the controlplane MariaDB database and streams the
// gzip-compressed output directly to R2 under the "infra/controlplane/" prefix.
//
// Uses the same ephemeral-container pattern as Backupper.BackupDatabase:
// mysql:8 on wp_backend → ContainerAttach → stripDockerMux → gzip → R2.
// Exit-code is checked after upload; a non-zero result deletes the uploaded
// object so R2 never holds a corrupt snapshot.
func (b *InfraBackupper) BackupControlplaneDB(ctx context.Context) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot backup controlplane DB")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	dbUser, dbPass := parseDSNCredentials(b.cfg.ControlDSN)
	dbName := parseDSNDatabase(b.cfg.ControlDSN)
	if dbName == "" {
		dbName = "controlplane"
	}
	dbHostPort := b.cfg.DBHost()
	dbHost, dbPort, err := net.SplitHostPort(dbHostPort)
	if err != nil {
		dbHost = dbHostPort
		dbPort = "3306"
	}

	ts := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	key := fmt.Sprintf("infra/controlplane/%s.sql.gz", ts)
	containerName := fmt.Sprintf("backup_infra_db_%d", time.Now().UnixNano())

	createResp, err := b.docker.ContainerCreate(ctx,
		&container.Config{
			Image: "mysql:8",
			Cmd: []string{
				"mysqldump",
				"-h", dbHost,
				"-P", dbPort,
				"-u", dbUser,
				fmt.Sprintf("-p%s", dbPass),
				"--single-transaction",
				"--quick",
				"--set-gtid-purged=OFF",
				dbName,
			},
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
		return fmt.Errorf("create infra DB backup container: %w", err)
	}

	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		b.docker.ContainerRemove(cleanCtx, createResp.ID, types.ContainerRemoveOptions{Force: true}) //nolint
	}()

	// Attach before start so we capture all output from the beginning.
	attachResp, err := b.docker.ContainerAttach(ctx, createResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: false, // captured on failure via logContainerStderr
	})
	if err != nil {
		return fmt.Errorf("attach infra DB backup container: %w", err)
	}
	defer attachResp.Close()

	if err := b.docker.ContainerStart(ctx, createResp.ID, types.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start infra DB backup container: %w", err)
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

	uploadErr := b.r2.Upload(ctx, key, pr, "application/gzip")
	<-gzDone

	exitCode, waitErr := b.waitContainer(ctx, createResp.ID)
	if waitErr != nil {
		if uploadErr == nil {
			b.r2.deleteObject(context.Background(), key) //nolint
		}
		return fmt.Errorf("wait for infra DB backup container: %w", waitErr)
	}
	if exitCode != 0 {
		b.logContainerStderr(createResp.ID, "mysqldump controlplane")
		if uploadErr == nil {
			if delErr := b.r2.deleteObject(context.Background(), key); delErr != nil {
				log.Printf("[infra-backup] WARNING: corrupt controlplane backup %s could not be deleted: %v", key, delErr)
			} else {
				log.Printf("[infra-backup] corrupt controlplane backup %s deleted from R2", key)
			}
		}
		return fmt.Errorf("mysqldump exited with code %d — controlplane backup aborted", exitCode)
	}
	if uploadErr != nil {
		return fmt.Errorf("upload controlplane DB backup to R2: %w", uploadErr)
	}

	log.Printf("[infra-backup] controlplane DB backup → %s", key)
	return nil
}

// BackupDockerCerts tars the Docker TLS certificate directory from the local
// filesystem and streams the gzip-compressed archive to R2 under the
// "infra/certs/" prefix. No Docker container is needed — the certs live on
// control-01 itself at cfg.DockerCertDir.
func (b *InfraBackupper) BackupDockerCerts(ctx context.Context) error {
	if b.r2 == nil {
		return fmt.Errorf("R2 not configured — cannot backup Docker certs")
	}

	certDir := b.cfg.DockerCertDir
	if certDir == "" {
		certDir = "/opt/control/certs"
	}

	key := fmt.Sprintf("infra/certs/%s.tar.gz", dateStamp())

	// Check if an archive for today already exists — avoid re-uploading.
	if err := b.r2.objectExists(ctx, key); err == nil {
		log.Printf("[infra-backup] certs backup already exists for today, skipping: %s", key)
		return nil
	}

	pr, pw := io.Pipe()
	archiveDone := make(chan error, 1)

	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		archiveDone <- tarDir(tw, certDir)
		tw.Close()
		gz.Close()
		pw.Close()
	}()

	uploadErr := b.r2.Upload(ctx, key, pr, "application/gzip")
	archiveErr := <-archiveDone

	if archiveErr != nil {
		// Clean up any partial upload
		if uploadErr == nil {
			b.r2.deleteObject(context.Background(), key) //nolint
		}
		return fmt.Errorf("archive cert dir %s: %w", certDir, archiveErr)
	}
	if uploadErr != nil {
		return fmt.Errorf("upload Docker certs backup to R2: %w", uploadErr)
	}

	log.Printf("[infra-backup] Docker certs backup → %s", key)
	return nil
}

// tarDir walks dir and writes every regular file into the tar.Writer using
// paths relative to dir. Symlinks and directories are skipped (Docker TLS
// certs are all plain files).
func tarDir(tw *tar.Writer, dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil // skip dirs, symlinks
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		hdr := &tar.Header{
			Name:    rel,
			Mode:    int64(info.Mode()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})
}

// waitContainer blocks until the container exits and returns its exit code.
// Mirrors the helper in backupper.go.
func (b *InfraBackupper) waitContainer(ctx context.Context, id string) (int64, error) {
	statusCh, errCh := b.docker.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return -1, err
	case status := <-statusCh:
		return status.StatusCode, nil
	}
}

// logContainerStderr prints the last 4 KB of stderr for a container to the
// control-plane log. Mirrors the helper in backupper.go.
func (b *InfraBackupper) logContainerStderr(id, label string) {
	logCtx, logCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer logCancel()
	rc, err := b.docker.ContainerLogs(logCtx, id, types.ContainerLogsOptions{
		ShowStderr: true,
		Tail:       "50",
	})
	if err != nil {
		log.Printf("[infra-backup] %s: could not retrieve container stderr: %v", label, err)
		return
	}
	defer rc.Close()
	buf := &strings.Builder{}
	io.Copy(buf, rc) //nolint
	log.Printf("[infra-backup] %s stderr:\n%s", label, buf.String())
}

// parseDSNDatabase extracts the database name from a DSN of the form:
// user:pass@tcp(host:port)/dbname[?params]
func parseDSNDatabase(dsn string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return ""
	}
	db := dsn[slash+1:]
	if q := strings.Index(db, "?"); q >= 0 {
		db = db[:q]
	}
	return db
}
