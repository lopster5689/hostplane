package main

import (
	"fmt"
	"log"
	"time"
)

type Worker struct {
    db                *DB
    provisioner       *Provisioner
    destroyer         *Destroyer
    staticProvisioner *StaticProvisioner
    n8nProvisioner    *N8nProvisioner
    cfg               Config
}

func NewWorker(db *DB, provisioner *Provisioner, destroyer *Destroyer, staticProvisioner *StaticProvisioner, n8nProvisioner *N8nProvisioner, cfg Config) *Worker {
    return &Worker{
        db:                db,
        provisioner:       provisioner,
        destroyer:         destroyer,
        staticProvisioner: staticProvisioner,
        n8nProvisioner:    n8nProvisioner,
        cfg:               cfg,
    }
}

func (w *Worker) Start() {
	log.Println("[worker] starting")

	// On startup, recover any jobs that were stuck mid-flight
	// when control-01 last crashed or restarted
	recovered, err := w.db.RecoverStuckJobs(w.cfg.StuckJobTimeout)
	if err != nil {
		log.Printf("[worker] stuck job recovery error: %v", err)
	} else if recovered > 0 {
		log.Printf("[worker] recovered %d stuck jobs back to PENDING", recovered)
	}

	ticker := time.NewTicker(time.Duration(w.cfg.WorkerPollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		w.processNext()
	}
}

func (w *Worker) processNext() {
	job, err := w.db.ClaimNextJob()
	if err != nil {
		log.Printf("[worker] error claiming job: %v", err)
		return
	}
	if job == nil {
		return // nothing pending, silent
	}

	log.Printf("[worker] claimed job %s | type=%s site=%s attempt=%d/%d",
		job.ID, job.Type, job.Site, job.Attempts, job.MaxAttempts)

	var jobErr error

	switch job.Type {
	case JobProvision:
		jobErr = w.provisioner.Run(job.Site)
	case JobDestroy:
		siteType := "wordpress"
		if s, err := w.db.GetSite(job.Site); err == nil && s.SiteType != "" {
			siteType = s.SiteType
		}
		jobErr = w.destroyer.Run(job.Site, siteType)
	case JobStaticProvision:
		payload, err := w.db.GetJobPayload(job.ID)
		if err != nil || payload == "" {
			jobErr = fmt.Errorf("missing zip payload for job")
		} else {
			jobErr = w.staticProvisioner.Run(job.Site, payload)
		}
	case JobStaticUpdate:
		payload, err := w.db.GetJobPayload(job.ID)
		if err != nil || payload == "" {
			jobErr = fmt.Errorf("missing zip payload for job")
		} else {
			jobErr = w.staticProvisioner.RunUpdate(job.Site, payload)
		}
	case JobN8nProvision:
		plan, _ := w.db.GetJobPayload(job.ID) // plan tier ("starter"/"business"/"scale"); empty -> starter
		jobErr = w.n8nProvisioner.Run(job.Site, plan)
	case JobN8nDestroy:
		jobErr = w.destroyer.Run(job.Site, "n8n")
	default:
		jobErr = fmt.Errorf("unknown job type: %s", job.Type)
	}

	if jobErr != nil {
		log.Printf("[worker] job %s FAILED (attempt %d): %v", job.ID, job.Attempts, jobErr)

		// If we've hit max attempts, mark FAILED permanently
		// If not, mark PENDING again so the next poll retries it.
		// ClaimNextJob already incremented the row's attempts to job.Attempts+1,
		// so compare against that — otherwise the last attempt is retried once
		// more and then stranded at PENDING (attempts==max_attempts can never be
		// re-claimed by ClaimNextJob's `attempts < max_attempts` filter).
		if job.Attempts+1 >= job.MaxAttempts {
			log.Printf("[worker] job %s exhausted all %d attempts, marking FAILED", job.ID, job.MaxAttempts)
			if err := w.db.FailJob(job.ID, job.Site, job.Type, jobErr); err != nil {
				log.Printf("[worker] error marking job failed: %v", err)
			}
		} else {
			log.Printf("[worker] job %s will retry (%d attempts remaining)", job.ID, job.MaxAttempts-job.Attempts)
			if err := w.db.RetryJob(job.ID, jobErr); err != nil {
				log.Printf("[worker] error scheduling retry: %v", err)
			}
		}
		return
	}

	log.Printf("[worker] job %s COMPLETED | site=%s", job.ID, job.Site)
	if err := w.db.CompleteJob(job.ID, job.Site, job.Type); err != nil {
		log.Printf("[worker] error marking job complete: %v", err)
	}
}
