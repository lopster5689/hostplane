package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type JobType string
type JobStatus string

const (
	JobProvision       JobType   = "PROVISION"
	JobDestroy         JobType   = "DESTROY"
	JobStaticProvision JobType   = "STATIC_PROVISION"
	JobStaticUpdate    JobType   = "STATIC_UPDATE"
	JobN8nProvision    JobType   = "N8N_PROVISION"
	JobN8nDestroy      JobType   = "N8N_DESTROY"
	StatusPending      JobStatus = "PENDING"
	StatusProcessing   JobStatus = "PROCESSING"
	StatusCompleted    JobStatus = "COMPLETED"
	StatusFailed       JobStatus = "FAILED"
)

type Site struct {
	Site          string
	Domain        string
	Status        string
	JobID         string
	UserID        string
	SiteType      string // "wordpress", "static", "n8n"
	EncryptionKey string // n8n only — persists N8N_ENCRYPTION_KEY across container recreations
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CustomDomain  string
	LastBackupAt  *time.Time // nullable — nil until first backup
}

func (d *DB) SetCustomDomain(site, domain string) error {
	_, err := d.conn.Exec(`
        UPDATE sites SET custom_domain=?, updated_at=NOW() WHERE site=?
    `, domain, site)
	return err
}

func (d *DB) RemoveCustomDomain(site string) error {
	_, err := d.conn.Exec(`
        UPDATE sites SET custom_domain=NULL, updated_at=NOW() WHERE site=?
    `, site)
	return err
}

func (d *DB) FailJob(jobID, site string, jobType JobType, jobErr error) error {
	msg := jobErr.Error()
	_, err := d.conn.Exec(`
        UPDATE jobs SET status='FAILED', error=?, updated_at=NOW() WHERE id=?
    `, msg, jobID)
	if err != nil {
		return err
	}
	// A failed STATIC_UPDATE must NOT downgrade the site: the existing files are
	// untouched (RunUpdate swaps in place only on success) so the site is still
	// live and ACTIVE. Downgrading to FAILED would wedge future updates, which
	// require the site to be ACTIVE.
	if jobType == JobStaticUpdate {
		return nil
	}
	// Mark site as FAILED so it's visible and cleanable
	return d.UpdateSiteStatus(site, "FAILED")
}

func (d *DB) SetJobPayload(jobID, payload string) error {
	_, err := d.conn.Exec(`UPDATE jobs SET payload=? WHERE id=?`, payload, jobID)
	return err
}

func (d *DB) GetJobPayload(jobID string) (string, error) {
	var val sql.NullString
	err := d.conn.QueryRow(`SELECT payload FROM jobs WHERE id=?`, jobID).Scan(&val)
	if err != nil {
		return "", err
	}
	if val.Valid {
		return val.String, nil
	}
	return "", nil
}

func (d *DB) HardDeleteSite(site string) error {
	_, err := d.conn.Exec(`
		DELETE FROM jobs WHERE site=?;
	`, site)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`
		DELETE FROM sites WHERE site=?;
	`, site)
	return err
}

func (d *DB) HardDeleteJob(id string) error {
	_, err := d.conn.Exec(`DELETE FROM jobs WHERE id=?`, id)
	return err
}

func (d *DB) MigrateSchema() error {
	// Idempotent — ADD COLUMN IF NOT EXISTS requires MySQL 8.0.3+
	// For older versions we use a conditional approach.
	_, err := d.conn.Exec(`
		ALTER TABLE sites
		ADD COLUMN IF NOT EXISTS last_backup_at DATETIME NULL DEFAULT NULL
	`)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`
		ALTER TABLE sites
		ADD COLUMN IF NOT EXISTS user_id VARCHAR(36) NULL DEFAULT NULL
	`)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`
		ALTER TABLE sites
		ADD COLUMN IF NOT EXISTS site_type VARCHAR(20) NOT NULL DEFAULT 'wordpress'
	`)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`
		ALTER TABLE sites
		ADD COLUMN IF NOT EXISTS encryption_key VARCHAR(255) NULL DEFAULT NULL
	`)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`
		ALTER TABLE sites
		ADD COLUMN IF NOT EXISTS db_password VARCHAR(255) NULL DEFAULT NULL
	`)
	return err
}

func (d *DB) UpdateLastBackupAt(site string, t time.Time) error {
	_, err := d.conn.Exec(`
		UPDATE sites SET last_backup_at=?, updated_at=NOW() WHERE site=?
	`, t.UTC(), site)
	return err
}

func (d *DB) GetSite(site string) (*Site, error) {
	var s Site
	var lastBackup sql.NullTime
	err := d.conn.QueryRow(`
        SELECT site, domain, COALESCE(custom_domain,''), status, COALESCE(job_id,''), COALESCE(user_id,''),
               COALESCE(site_type,'wordpress'), COALESCE(encryption_key,''),
               created_at, updated_at, last_backup_at
        FROM sites WHERE site=?
    `, site).Scan(&s.Site, &s.Domain, &s.CustomDomain, &s.Status, &s.JobID, &s.UserID,
		&s.SiteType, &s.EncryptionKey,
		&s.CreatedAt, &s.UpdatedAt, &lastBackup)
	if err != nil {
		return nil, err
	}
	if lastBackup.Valid {
		s.LastBackupAt = &lastBackup.Time
	}
	return &s, nil
}

func (d *DB) ListSites() ([]Site, error) {
	rows, err := d.conn.Query(`
        SELECT site, domain, COALESCE(custom_domain,''), status, COALESCE(job_id,''), COALESCE(user_id,''),
               COALESCE(site_type,'wordpress'), COALESCE(encryption_key,''),
               created_at, updated_at, last_backup_at
        FROM sites ORDER BY created_at DESC
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sites []Site
	for rows.Next() {
		var s Site
		var lastBackup sql.NullTime
		if err := rows.Scan(&s.Site, &s.Domain, &s.CustomDomain, &s.Status, &s.JobID, &s.UserID,
			&s.SiteType, &s.EncryptionKey,
			&s.CreatedAt, &s.UpdatedAt, &lastBackup); err != nil {
			return nil, err
		}
		if lastBackup.Valid {
			s.LastBackupAt = &lastBackup.Time
		}
		sites = append(sites, s)
	}
	return sites, nil
}

type Job struct {
	ID          string
	Type        JobType
	Site        string
	Status      JobStatus
	Attempts    int
	MaxAttempts int
	Error       *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

type DB struct {
	conn *sql.DB
}

func NewDB(dsn string) (*DB, error) {
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(5)
	conn.SetConnMaxLifetime(5 * time.Minute)
	if err = conn.Ping(); err != nil {
		return nil, fmt.Errorf("cannot reach control DB: %w", err)
	}
	return &DB{conn: conn}, nil
}

// InsertJob writes a new PENDING job and returns its ID
func (d *DB) InsertJob(id string, jobType JobType, site string) error {
	_, err := d.conn.Exec(`
        INSERT INTO jobs (id, type, site, status, attempts, max_attempts)
        VALUES (?, ?, ?, 'PENDING', 0, 3)
    `, id, jobType, site)
	return err
}

// UpsertSite creates or updates the site record.
// siteType should be "wordpress", "static", or "n8n".
func (d *DB) UpsertSite(site, domain, status, jobID, userID, siteType string) error {
	if siteType == "" {
		siteType = "wordpress"
	}
	_, err := d.conn.Exec(`
        INSERT INTO sites (site, domain, status, job_id, user_id, site_type)
        VALUES (?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE status=VALUES(status), job_id=VALUES(job_id), user_id=VALUES(user_id), site_type=VALUES(site_type), updated_at=NOW()
    `, site, domain, status, jobID, userID, siteType)
	return err
}

// UpdateSiteStatus updates just the status of a site
func (d *DB) UpdateSiteStatus(site, status string) error {
	_, err := d.conn.Exec(`
        UPDATE sites SET status=?, updated_at=NOW() WHERE site=?
    `, status, site)
	return err
}

// HasActiveJob checks if site already has a PENDING or PROCESSING job
func (d *DB) HasActiveJob(site string) (bool, error) {
	var count int
	err := d.conn.QueryRow(`
        SELECT COUNT(*) FROM jobs
        WHERE site=? AND status IN ('PENDING','PROCESSING')
    `, site).Scan(&count)
	return count > 0, err
}

// ClaimNextJob atomically claims the next PENDING job using FOR UPDATE SKIP LOCKED
func (d *DB) ClaimNextJob() (*Job, error) {
	tx, err := d.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRow(`
        SELECT id, type, site, attempts, max_attempts
        FROM jobs
        WHERE status='PENDING' AND attempts < max_attempts
        ORDER BY created_at ASC
        LIMIT 1
        FOR UPDATE SKIP LOCKED
    `)

	var job Job
	err = row.Scan(&job.ID, &job.Type, &job.Site, &job.Attempts, &job.MaxAttempts)
	if err == sql.ErrNoRows {
		return nil, nil // nothing to do
	}
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(`
        UPDATE jobs
        SET status='PROCESSING', attempts=attempts+1, started_at=NOW(), updated_at=NOW()
        WHERE id=?
    `, job.ID)
	if err != nil {
		return nil, err
	}

	return &job, tx.Commit()
}

// CompleteJob marks job and site as done
func (d *DB) CompleteJob(jobID, site string, jobType JobType) error {
	_, err := d.conn.Exec(`
        UPDATE jobs
        SET status='COMPLETED', completed_at=NOW(), updated_at=NOW(), error=NULL
        WHERE id=?
    `, jobID)
	if err != nil {
		return err
	}

	finalSiteStatus := "ACTIVE"
	if jobType == JobDestroy || jobType == JobN8nDestroy {
		finalSiteStatus = "DESTROYED"
	}
	return d.UpdateSiteStatus(site, finalSiteStatus)
}

// GetJob fetches a job by ID for status polling
func (d *DB) GetJob(id string) (*Job, error) {
	var job Job
	var errStr sql.NullString
	var startedAt, completedAt sql.NullTime

	err := d.conn.QueryRow(`
        SELECT id, type, site, status, attempts, max_attempts, error, created_at, updated_at, started_at, completed_at
        FROM jobs WHERE id=?
    `, id).Scan(
		&job.ID, &job.Type, &job.Site, &job.Status,
		&job.Attempts, &job.MaxAttempts, &errStr,
		&job.CreatedAt, &job.UpdatedAt, &startedAt, &completedAt,
	)
	if err != nil {
		return nil, err
	}

	if errStr.Valid {
		job.Error = &errStr.String
	}
	if startedAt.Valid {
		job.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}

	return &job, nil
}

// RecoverStuckJobs resets PROCESSING jobs that have been running too long (called on startup)
func (d *DB) RecoverStuckJobs(timeoutMinutes int) (int64, error) {
	res, err := d.conn.Exec(`
        UPDATE jobs
        SET status='PENDING', error='recovered: was stuck in PROCESSING', updated_at=NOW()
        WHERE status='PROCESSING'
        AND started_at < NOW() - INTERVAL ? MINUTE
    `, timeoutMinutes)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetryJob puts a PROCESSING job back to PENDING for the next poll cycle to pick up
func (d *DB) RetryJob(jobID string, jobErr error) error {
	msg := fmt.Sprintf("attempt failed: %s", jobErr.Error())
	_, err := d.conn.Exec(`
		UPDATE jobs
		SET status='PENDING', error=?, updated_at=NOW()
		WHERE id=?
	`, msg, jobID)
	return err
}

// TransitionSite validates and performs a state transition using the lifecycle state machine.
// Returns an error if the transition is not allowed.
func (d *DB) TransitionSite(site string, to SiteStatus) error {
	s, err := d.GetSite(site)
	if err != nil {
		return fmt.Errorf("get site %s: %w", site, err)
	}
	from := SiteStatus(s.Status)
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("invalid transition: %s → %s for site %s", from, to, site)
	}
	return d.UpdateSiteStatus(site, string(to))
}

// EnsureDomainAvailable checks that no other active site has claimed this custom domain.
func (d *DB) EnsureDomainAvailable(domain, excludeSite string) error {
	var count int
	err := d.conn.QueryRow(`
		SELECT COUNT(*) FROM sites
		WHERE custom_domain=? AND site!=? AND status NOT IN ('DESTROYED','FAILED')
	`, domain, excludeSite).Scan(&count)
	if err != nil {
		return fmt.Errorf("check domain availability: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("domain %s is already claimed by another site", domain)
	}
	return nil
}

// MigratePGSchema creates the pg_backup_meta table used to track last
// successful backup time and key for each PostgreSQL database.
func (d *DB) MigratePGSchema() error {
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS pg_backup_meta (
			name            VARCHAR(64)   NOT NULL PRIMARY KEY,
			last_backup_at  DATETIME      NULL DEFAULT NULL,
			last_backup_key VARCHAR(512)  NULL DEFAULT NULL
		)
	`)
	return err
}

// UpdatePGLastBackupAt records the time and R2 key of the most recent
// successful backup for a PostgreSQL database.
func (d *DB) UpdatePGLastBackupAt(name string, t time.Time, key string) error {
	_, err := d.conn.Exec(`
		INSERT INTO pg_backup_meta (name, last_backup_at, last_backup_key)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE
			last_backup_at  = VALUES(last_backup_at),
			last_backup_key = VALUES(last_backup_key)
	`, name, t.UTC(), key)
	return err
}

// GetPGLastBackupAt returns the last backup time and R2 key for a PostgreSQL
// database. Returns nil time and empty key if no backup has been recorded yet.
func (d *DB) GetPGLastBackupAt(name string) (*time.Time, string, error) {
	var lastAt sql.NullTime
	var lastKey sql.NullString
	err := d.conn.QueryRow(
		`SELECT last_backup_at, last_backup_key FROM pg_backup_meta WHERE name=?`,
		name,
	).Scan(&lastAt, &lastKey)
	if err == sql.ErrNoRows {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var t *time.Time
	if lastAt.Valid {
		t = &lastAt.Time
	}
	key := ""
	if lastKey.Valid {
		key = lastKey.String
	}
	return t, key, nil
}

func (d *DB) SetEncryptionKey(site, key string) error {
	_, err := d.conn.Exec(`
		UPDATE sites SET encryption_key=?, updated_at=NOW() WHERE site=?
	`, key, site)
	return err
}

func (d *DB) GetEncryptionKey(site string) (string, error) {
	var key sql.NullString
	err := d.conn.QueryRow(`SELECT encryption_key FROM sites WHERE site=?`, site).Scan(&key)
	if err != nil {
		return "", err
	}
	if key.Valid {
		return key.String, nil
	}
	return "", nil
}

// SetDBPassword persists the random per-site n8n Postgres password (n8n only).
func (d *DB) SetDBPassword(site, pass string) error {
	_, err := d.conn.Exec(`
		UPDATE sites SET db_password=?, updated_at=NOW() WHERE site=?
	`, pass, site)
	return err
}

func (d *DB) GetDBPassword(site string) (string, error) {
	var pass sql.NullString
	err := d.conn.QueryRow(`SELECT db_password FROM sites WHERE site=?`, site).Scan(&pass)
	if err != nil {
		return "", err
	}
	if pass.Valid {
		return pass.String, nil
	}
	return "", nil
}

// ListCustomDomains returns all active custom domain values from the sites table.
func (d *DB) ListCustomDomains() ([]string, error) {
	rows, err := d.conn.Query(`
		SELECT custom_domain FROM sites
		WHERE custom_domain IS NOT NULL AND custom_domain != ''
		AND status NOT IN ('DESTROYED','FAILED')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var domains []string
	for rows.Next() {
		var dom string
		if err := rows.Scan(&dom); err != nil {
			return nil, err
		}
		domains = append(domains, dom)
	}
	return domains, nil
}
