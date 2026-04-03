package main

import (
	"context"
	"log"
	"time"
)

// BackupWorker runs a daily backup tick, mirroring the Worker pattern exactly.
type BackupWorker struct {
	backupper *Backupper
	cfg       Config
}

func NewBackupWorker(backupper *Backupper, cfg Config) *BackupWorker {
	return &BackupWorker{backupper: backupper, cfg: cfg}
}

// Start begins the daily backup loop. Blocks forever — call via go.
// Runs an immediate backup on first start, then every 24 hours.
// Single-site failures never crash the worker.
func (bw *BackupWorker) Start() {
	log.Println("[backup-worker] starting — daily interval")

	// Immediate run on startup so we don't wait 24 h for the first backup.
	bw.backupper.BackupAll()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	day := 0
	for range ticker.C {
		day++
		log.Printf("[backup-worker] daily backup run #%d starting", day)
		bw.backupper.BackupAll()

		// Daily cleanup — purge backups older than 3 days
		log.Printf("[backup-worker] running cleanup (>3 days)")
		bw.runCleanup()
	}
}

func (bw *BackupWorker) runCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := bw.backupper.r2.DeleteOlderThan(ctx, "databases/", 3); err != nil {
		log.Printf("[backup-worker] cleanup databases/: %v", err)
	}
	if err := bw.backupper.r2.DeleteOlderThan(ctx, "volumes/", 3); err != nil {
		log.Printf("[backup-worker] cleanup volumes/: %v", err)
	}
}
