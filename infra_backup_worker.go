package main

import (
	"context"
	"log"
	"time"
)

// InfraBackupWorker runs scheduled infrastructure backups:
//   - Controlplane DB dump  — every hour   (90-day retention)
//   - Docker TLS certs      — every 24 h   (30-day retention)
//   - Retention cleanup     — every 7 days
//
// Start blocks forever; call it via go.
type InfraBackupWorker struct {
	backupper *InfraBackupper
}

func NewInfraBackupWorker(backupper *InfraBackupper) *InfraBackupWorker {
	return &InfraBackupWorker{backupper: backupper}
}

// Start begins the infra-backup loop. Blocks forever — call via go.
// Runs both jobs immediately on first start so we never wait up to an hour
// for the first backup.
func (w *InfraBackupWorker) Start() {
	log.Println("[infra-backup-worker] starting — DB hourly, certs daily")

	// Immediate first run.
	w.runDB()
	w.runCerts()

	dbTicker := time.NewTicker(1 * time.Hour)
	defer dbTicker.Stop()

	certTicker := time.NewTicker(24 * time.Hour)
	defer certTicker.Stop()

	cleanTicker := time.NewTicker(7 * 24 * time.Hour)
	defer cleanTicker.Stop()

	for {
		select {
		case <-dbTicker.C:
			w.runDB()
		case <-certTicker.C:
			w.runCerts()
		case <-cleanTicker.C:
			w.runCleanup()
		}
	}
}

func (w *InfraBackupWorker) runDB() {
	log.Println("[infra-backup-worker] starting controlplane DB backup")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := w.backupper.BackupControlplaneDB(ctx); err != nil {
		log.Printf("[infra-backup-worker] controlplane DB backup FAILED: %v", err)
		return
	}
	log.Println("[infra-backup-worker] controlplane DB backup OK")
}

func (w *InfraBackupWorker) runCerts() {
	log.Println("[infra-backup-worker] starting Docker certs backup")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := w.backupper.BackupDockerCerts(ctx); err != nil {
		log.Printf("[infra-backup-worker] Docker certs backup FAILED: %v", err)
		return
	}
	log.Println("[infra-backup-worker] Docker certs backup OK")
}

func (w *InfraBackupWorker) runCleanup() {
	log.Println("[infra-backup-worker] running retention cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := w.backupper.r2.DeleteOlderThan(ctx, "infra/controlplane/", 90); err != nil {
		log.Printf("[infra-backup-worker] cleanup infra/controlplane/: %v", err)
	}
	if err := w.backupper.r2.DeleteOlderThan(ctx, "infra/certs/", 30); err != nil {
		log.Printf("[infra-backup-worker] cleanup infra/certs/: %v", err)
	}
	log.Println("[infra-backup-worker] retention cleanup done")
}
