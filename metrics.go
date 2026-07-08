package main

import (
	"database/sql"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// hostplaneCollector is a Prometheus custom collector that queries the
// control DB on every scrape and emits gauges. This avoids instrumenting
// every operation in the codebase; the trade-off is one cheap DB query per
// scrape (default scrape interval is 15s).
type hostplaneCollector struct {
	db *DB

	jobsByStatus              *prometheus.Desc
	jobsProcessingOldestSec   *prometheus.Desc
	jobsExhaustedPending      *prometheus.Desc
	sitesByStatus             *prometheus.Desc
	sitesWithStaleBackup      *prometheus.Desc
	siteBackupOldestAgeSec    *prometheus.Desc
	pgBackupAgeSec            *prometheus.Desc
	scrapeErrorsTotal         *prometheus.Desc
}

// scrapeErrors counts query failures so a broken metrics surface is itself
// alertable. Exported once via Collect — it tracks total since process start.
var scrapeErrors float64

func newHostplaneCollector(db *DB) *hostplaneCollector {
	return &hostplaneCollector{
		db: db,
		jobsByStatus: prometheus.NewDesc(
			"hostplane_jobs_total",
			"Number of jobs currently in each status.",
			[]string{"status"}, nil,
		),
		jobsProcessingOldestSec: prometheus.NewDesc(
			"hostplane_jobs_processing_seconds_max",
			"Age in seconds of the oldest job currently in PROCESSING.",
			nil, nil,
		),
		jobsExhaustedPending: prometheus.NewDesc(
			"hostplane_jobs_exhausted_pending",
			"Jobs with attempts>=max_attempts but still in PENDING. Should always be 0; non-zero means the worker FailJob path is broken (ARCHITECTURE.md §25).",
			nil, nil,
		),
		sitesByStatus: prometheus.NewDesc(
			"hostplane_sites_total",
			"Number of sites currently in each status.",
			[]string{"status", "site_type"}, nil,
		),
		sitesWithStaleBackup: prometheus.NewDesc(
			"hostplane_sites_with_stale_backup",
			"ACTIVE/DOMAIN_ACTIVE sites whose last_backup_at is older than 36h or NULL.",
			nil, nil,
		),
		siteBackupOldestAgeSec: prometheus.NewDesc(
			"hostplane_site_backup_oldest_age_seconds",
			"Oldest last_backup_at across ACTIVE/DOMAIN_ACTIVE sites, in seconds. -1 if no sites have a backup yet.",
			nil, nil,
		),
		pgBackupAgeSec: prometheus.NewDesc(
			"hostplane_pg_backup_age_seconds",
			"Age in seconds of the most recent PostgreSQL backup. -1 if none recorded.",
			nil, nil,
		),
		scrapeErrorsTotal: prometheus.NewDesc(
			"hostplane_metrics_scrape_errors_total",
			"Total number of times a metrics-scrape DB query has failed since process start.",
			nil, nil,
		),
	}
}

func (c *hostplaneCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jobsByStatus
	ch <- c.jobsProcessingOldestSec
	ch <- c.jobsExhaustedPending
	ch <- c.sitesByStatus
	ch <- c.sitesWithStaleBackup
	ch <- c.siteBackupOldestAgeSec
	ch <- c.pgBackupAgeSec
	ch <- c.scrapeErrorsTotal
}

func (c *hostplaneCollector) Collect(ch chan<- prometheus.Metric) {
	c.collectJobs(ch)
	c.collectSites(ch)
	c.collectPGBackup(ch)
	ch <- prometheus.MustNewConstMetric(c.scrapeErrorsTotal, prometheus.CounterValue, scrapeErrors)
}

func (c *hostplaneCollector) collectJobs(ch chan<- prometheus.Metric) {
	rows, err := c.db.conn.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		scrapeErrors++
		log.Printf("[metrics] jobs by status query failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err == nil {
			ch <- prometheus.MustNewConstMetric(c.jobsByStatus, prometheus.GaugeValue, float64(count), status)
		}
	}

	var oldestSec sql.NullFloat64
	err = c.db.conn.QueryRow(`
		SELECT IFNULL(TIMESTAMPDIFF(SECOND, MIN(started_at), NOW()), 0)
		FROM jobs WHERE status='PROCESSING'
	`).Scan(&oldestSec)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(c.jobsProcessingOldestSec, prometheus.GaugeValue, oldestSec.Float64)
	} else {
		scrapeErrors++
	}

	var exhausted int
	err = c.db.conn.QueryRow(`
		SELECT COUNT(*) FROM jobs WHERE status='PENDING' AND attempts >= max_attempts
	`).Scan(&exhausted)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(c.jobsExhaustedPending, prometheus.GaugeValue, float64(exhausted))
	} else {
		scrapeErrors++
	}
}

func (c *hostplaneCollector) collectSites(ch chan<- prometheus.Metric) {
	rows, err := c.db.conn.Query(`SELECT status, COALESCE(site_type,'wordpress'), COUNT(*) FROM sites GROUP BY status, site_type`)
	if err != nil {
		scrapeErrors++
		log.Printf("[metrics] sites by status query failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status, siteType string
		var count int
		if err := rows.Scan(&status, &siteType, &count); err == nil {
			ch <- prometheus.MustNewConstMetric(c.sitesByStatus, prometheus.GaugeValue, float64(count), status, siteType)
		}
	}

	var stale int
	err = c.db.conn.QueryRow(`
		SELECT COUNT(*) FROM sites
		WHERE status IN ('ACTIVE','DOMAIN_ACTIVE')
		  AND (last_backup_at IS NULL OR last_backup_at < NOW() - INTERVAL 36 HOUR)
	`).Scan(&stale)
	if err == nil {
		ch <- prometheus.MustNewConstMetric(c.sitesWithStaleBackup, prometheus.GaugeValue, float64(stale))
	} else {
		scrapeErrors++
	}

	var oldest sql.NullFloat64
	err = c.db.conn.QueryRow(`
		SELECT IFNULL(TIMESTAMPDIFF(SECOND, MIN(last_backup_at), NOW()), -1)
		FROM sites WHERE status IN ('ACTIVE','DOMAIN_ACTIVE') AND last_backup_at IS NOT NULL
	`).Scan(&oldest)
	if err == nil {
		// If no rows match, MIN returns NULL → IFNULL forces -1.
		val := oldest.Float64
		if !oldest.Valid {
			val = -1
		}
		ch <- prometheus.MustNewConstMetric(c.siteBackupOldestAgeSec, prometheus.GaugeValue, val)
	} else {
		scrapeErrors++
	}
}

func (c *hostplaneCollector) collectPGBackup(ch chan<- prometheus.Metric) {
	var ageSec sql.NullFloat64
	err := c.db.conn.QueryRow(`
		SELECT IFNULL(TIMESTAMPDIFF(SECOND, MAX(last_backup_at), NOW()), -1)
		FROM pg_backup_meta WHERE last_backup_at IS NOT NULL
	`).Scan(&ageSec)
	if err != nil {
		// pg_backup_meta may not exist on a fresh deploy — treat as -1, no scrape error
		ch <- prometheus.MustNewConstMetric(c.pgBackupAgeSec, prometheus.GaugeValue, -1)
		return
	}
	val := ageSec.Float64
	if !ageSec.Valid {
		val = -1
	}
	ch <- prometheus.MustNewConstMetric(c.pgBackupAgeSec, prometheus.GaugeValue, val)
}

// startTime records when the process started so we can expose uptime if desired.
// Currently unused but kept for parity with common Prometheus instrumentation.
var processStart = time.Now()
