package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// PGBackupWorker runs a daily PostgreSQL backup with weekly retention cleanup.
// Same pattern as BackupWorker and InfraBackupWorker: immediate first run,
// 24h ticker, cleanup every 7 days.
type PGBackupWorker struct {
	backupper *PGBackupper
}

func NewPGBackupWorker(backupper *PGBackupper) *PGBackupWorker {
	return &PGBackupWorker{backupper: backupper}
}

// Start begins the daily backup loop. Blocks forever — call via go.
// Runs an immediate backup on startup so we never wait a full day.
func (w *PGBackupWorker) Start() {
	log.Println("[pg-backup-worker] starting — 24h interval")

	// Immediate run on startup
	w.runBackup()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	day := 0
	for range ticker.C {
		day++
		log.Printf("[pg-backup-worker] daily backup run #%d", day)
		w.runBackup()

		// Weekly cleanup — purge backups older than 30 days
		if day%7 == 0 {
			w.runCleanup()
		}
	}
}

func (w *PGBackupWorker) runBackup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := w.backupper.BackupDatabase(ctx); err != nil {
		log.Printf("[pg-backup-worker] backup FAILED: %v", err)
		return
	}
	log.Println("[pg-backup-worker] backup OK")
}

func (w *PGBackupWorker) runCleanup() {
	log.Println("[pg-backup-worker] running retention cleanup (>30 days)")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, _, _, _, pgDB := parsePGDSN(w.backupper.cfg.PostgresDSN)
	prefix := fmt.Sprintf("postgres/%s/", pgDB)

	if err := w.backupper.r2.DeleteOlderThan(ctx, prefix, 30); err != nil {
		log.Printf("[pg-backup-worker] cleanup failed: %v", err)
		return
	}
	log.Println("[pg-backup-worker] cleanup done")
}
