//go:build integration

package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Integration tests for the DB layer.
//
// Required env (set by CI in .github/workflows/ci.yml):
//   CONTROL_DSN  — root@tcp(127.0.0.1:3306)/controlplane DSN
//
// Tests create the canonical sites + jobs + pg_backup_meta schema from
// hosto-infra/scripts/restore-and-recover.sh, run, and tear it down. They
// MUST be safe to re-run; each test isolates by site name.
//
// Run locally with:
//   docker run -d --rm -e MARIADB_ROOT_PASSWORD=rootpass -e MARIADB_DATABASE=controlplane -p 3306:3306 mariadb:11
//   CONTROL_DSN='root:rootpass@tcp(127.0.0.1:3306)/controlplane' go test -tags=integration -v ./...

const sitesDDL = `
CREATE TABLE IF NOT EXISTS sites (
    site            VARCHAR(255) NOT NULL PRIMARY KEY,
    domain          VARCHAR(255) NOT NULL,
    custom_domain   VARCHAR(255) NULL DEFAULT NULL,
    status          VARCHAR(64)  NOT NULL,
    job_id          VARCHAR(255) NULL DEFAULT NULL,
    last_backup_at  DATETIME     NULL DEFAULT NULL,
    user_id         VARCHAR(36)  NULL DEFAULT NULL,
    site_type       VARCHAR(20)  NOT NULL DEFAULT 'wordpress',
    encryption_key  VARCHAR(255) NULL DEFAULT NULL,
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);`

const jobsDDL = `
CREATE TABLE IF NOT EXISTS jobs (
    id           VARCHAR(255) NOT NULL PRIMARY KEY,
    type         VARCHAR(64)  NOT NULL,
    site         VARCHAR(255) NOT NULL,
    status       VARCHAR(64)  NOT NULL,
    attempts     INT          NOT NULL DEFAULT 0,
    max_attempts INT          NOT NULL DEFAULT 3,
    error        TEXT         NULL DEFAULT NULL,
    payload      TEXT         NULL DEFAULT NULL,
    created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    started_at   DATETIME     NULL DEFAULT NULL,
    completed_at DATETIME     NULL DEFAULT NULL
);`

const pgMetaDDL = `
CREATE TABLE IF NOT EXISTS pg_backup_meta (
    name            VARCHAR(64)  NOT NULL PRIMARY KEY,
    last_backup_at  DATETIME     NULL,
    last_backup_key VARCHAR(512) NULL
);`

func mustDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("CONTROL_DSN")
	if dsn == "" {
		t.Skip("CONTROL_DSN not set — set it to enable integration tests")
	}
	db, err := NewDB(dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Ensure schema
	for _, ddl := range []string{sitesDDL, jobsDDL, pgMetaDDL} {
		if _, err := db.conn.Exec(ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

// uniqSite returns a per-test site name so concurrent tests don't collide.
func uniqSite(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

// cleanupSite is a deferred teardown helper.
func cleanupSite(t *testing.T, db *DB, site string) {
	t.Helper()
	_ = db.HardDeleteSite(site)
}

func TestIntegration_TransitionSite_AllowedAndRejected(t *testing.T) {
	db := mustDB(t)
	site := uniqSite("integtran")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test.example", string(SiteProvisioning), "job1", "u1", "wordpress"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// PROVISIONING → ACTIVE is allowed.
	if err := db.TransitionSite(site, SiteActive); err != nil {
		t.Errorf("legal transition rejected: %v", err)
	}
	// ACTIVE → DESTROYED is NOT in the matrix (must go via DESTROYING).
	if err := db.TransitionSite(site, SiteDestroyed); err == nil {
		t.Error("illegal ACTIVE→DESTROYED accepted")
	}
	// ACTIVE → DOMAIN_PENDING is allowed.
	if err := db.TransitionSite(site, SiteDomainPending); err != nil {
		t.Errorf("legal ACTIVE→DOMAIN_PENDING rejected: %v", err)
	}
	// Verify the actual stored status reflects the last successful transition.
	row, err := db.GetSite(site)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	if row.Status != string(SiteDomainPending) {
		t.Errorf("stored status %q, want %q", row.Status, SiteDomainPending)
	}
}

func TestIntegration_EnsureDomainAvailable(t *testing.T) {
	db := mustDB(t)
	site1 := uniqSite("integdoma")
	site2 := uniqSite("integdomb")
	defer cleanupSite(t, db, site1)
	defer cleanupSite(t, db, site2)

	if err := db.UpsertSite(site1, site1+".test.example", string(SiteDomainActive), "j", "u", "wordpress"); err != nil {
		t.Fatalf("upsert s1: %v", err)
	}
	if err := db.SetCustomDomain(site1, "claimed.example.com"); err != nil {
		t.Fatalf("set domain: %v", err)
	}
	if err := db.UpsertSite(site2, site2+".test.example", string(SiteActive), "j", "u", "wordpress"); err != nil {
		t.Fatalf("upsert s2: %v", err)
	}

	// claimed.example.com is taken — must fail.
	if err := db.EnsureDomainAvailable("claimed.example.com", site2); err == nil {
		t.Error("EnsureDomainAvailable allowed a claimed domain")
	}
	// Free domain — must pass.
	if err := db.EnsureDomainAvailable("fresh.example.com", site2); err != nil {
		t.Errorf("free domain rejected: %v", err)
	}
	// Same site can pass when its own claim is excluded.
	if err := db.EnsureDomainAvailable("claimed.example.com", site1); err != nil {
		t.Errorf("self-exclude failed: %v", err)
	}
	// DESTROYED sites should NOT block — flip site1 to DESTROYED and recheck.
	if err := db.UpdateSiteStatus(site1, string(SiteDestroyed)); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := db.EnsureDomainAvailable("claimed.example.com", site2); err != nil {
		t.Errorf("destroyed-holder must not block: %v", err)
	}
}

func TestIntegration_CompleteJob_DrivesSiteStatus(t *testing.T) {
	db := mustDB(t)
	site := uniqSite("integcj")
	defer cleanupSite(t, db, site)

	// PROVISION job → CompleteJob sets site to ACTIVE.
	if err := db.UpsertSite(site, site+".test.example", string(SiteProvisioning), "jp", "u", "wordpress"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.InsertJob("jp", JobProvision, site); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.CompleteJob("jp", site, JobProvision); err != nil {
		t.Fatalf("complete: %v", err)
	}
	row, _ := db.GetSite(site)
	if row.Status != "ACTIVE" {
		t.Errorf("after JobProvision complete: status=%q, want ACTIVE", row.Status)
	}

	// DESTROY job → CompleteJob sets site to DESTROYED.
	// Transition via DESTROYING first (state-machine-correct), then complete.
	if err := db.UpdateSiteStatus(site, string(SiteDestroying)); err != nil {
		t.Fatalf("set destroying: %v", err)
	}
	if err := db.InsertJob("jd", JobDestroy, site); err != nil {
		t.Fatalf("insert destroy job: %v", err)
	}
	if err := db.CompleteJob("jd", site, JobDestroy); err != nil {
		t.Fatalf("complete destroy: %v", err)
	}
	row, _ = db.GetSite(site)
	if row.Status != "DESTROYED" {
		t.Errorf("after JobDestroy complete: status=%q, want DESTROYED", row.Status)
	}
}

func TestIntegration_FailJob_FailJobBug(t *testing.T) {
	// ARCHITECTURE.md §25 known bug: the worker never marks an exhausted job
	// as FAILED. RetryJob puts it back to PENDING but ClaimNextJob filters
	// `attempts < max_attempts`, so it sits forever. We exercise FailJob
	// directly here (not via worker) so this test PASSES today — it pins the
	// DB-layer behavior so when the worker is fixed later, the change is
	// observable.
	db := mustDB(t)
	site := uniqSite("integfj")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test.example", string(SiteProvisioning), "fj1", "u", "wordpress"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.InsertJob("fj1", JobProvision, site); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.FailJob("fj1", site, JobProvision, fmt.Errorf("synthetic")); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	job, _ := db.GetJob("fj1")
	if job.Status != "FAILED" {
		t.Errorf("job.Status=%q, want FAILED", job.Status)
	}
	row, _ := db.GetSite(site)
	if row.Status != "FAILED" {
		t.Errorf("site.Status=%q, want FAILED", row.Status)
	}
}

func TestIntegration_HasActiveJob(t *testing.T) {
	db := mustDB(t)
	site := uniqSite("integaj")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test.example", string(SiteProvisioning), "aj1", "u", "wordpress"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.InsertJob("aj1", JobProvision, site); err != nil {
		t.Fatalf("insert: %v", err)
	}
	active, _ := db.HasActiveJob(site)
	if !active {
		t.Error("HasActiveJob=false with PENDING job present")
	}
	// Complete it — now HasActiveJob should be false.
	if err := db.CompleteJob("aj1", site, JobProvision); err != nil {
		t.Fatalf("complete: %v", err)
	}
	active, _ = db.HasActiveJob(site)
	if active {
		t.Error("HasActiveJob=true after CompleteJob")
	}
}

func TestIntegration_ClaimNextJob_OrdersFIFO(t *testing.T) {
	db := mustDB(t)
	// Two sites with two jobs each; older one must come out first.
	siteA := uniqSite("integcA")
	siteB := uniqSite("integcB")
	defer cleanupSite(t, db, siteA)
	defer cleanupSite(t, db, siteB)

	if err := db.UpsertSite(siteA, siteA+".test", string(SiteProvisioning), "cA", "u", "wordpress"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertJob("cA", JobProvision, siteA); err != nil {
		t.Fatal(err)
	}
	// Sleep to ensure DB sees a later created_at on the second job.
	time.Sleep(1100 * time.Millisecond)
	if err := db.UpsertSite(siteB, siteB+".test", string(SiteProvisioning), "cB", "u", "wordpress"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertJob("cB", JobProvision, siteB); err != nil {
		t.Fatal(err)
	}

	first, err := db.ClaimNextJob()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first == nil || first.ID != "cA" {
		t.Errorf("first claim = %v, want cA", first)
	}
	second, _ := db.ClaimNextJob()
	if second == nil || second.ID != "cB" {
		t.Errorf("second claim = %v, want cB", second)
	}
	third, _ := db.ClaimNextJob()
	if third != nil {
		t.Errorf("third claim should be nil (queue empty), got %v", third)
	}
}

func TestIntegration_HardDeleteSite_CascadesToJobs(t *testing.T) {
	db := mustDB(t)
	site := uniqSite("integhd")

	if err := db.UpsertSite(site, site+".test", string(SiteDestroyed), "hd1", "u", "wordpress"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertJob("hd1", JobProvision, site); err != nil {
		t.Fatal(err)
	}
	if err := db.HardDeleteSite(site); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Verify both rows gone.
	if _, err := db.GetSite(site); err != sql.ErrNoRows {
		t.Errorf("GetSite after delete: err=%v, want sql.ErrNoRows", err)
	}
	if _, err := db.GetJob("hd1"); err != sql.ErrNoRows {
		t.Errorf("GetJob after delete: err=%v, want sql.ErrNoRows", err)
	}
}

func TestIntegration_SetAndRemoveCustomDomain(t *testing.T) {
	db := mustDB(t)
	site := uniqSite("integcd")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test", string(SiteActive), "j", "u", "wordpress"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCustomDomain(site, "example.com"); err != nil {
		t.Fatalf("set: %v", err)
	}
	row, _ := db.GetSite(site)
	if row.CustomDomain != "example.com" {
		t.Errorf("after set: CustomDomain=%q, want example.com", row.CustomDomain)
	}
	if err := db.RemoveCustomDomain(site); err != nil {
		t.Fatalf("remove: %v", err)
	}
	row, _ = db.GetSite(site)
	if row.CustomDomain != "" {
		t.Errorf("after remove: CustomDomain=%q, want empty", row.CustomDomain)
	}
}

func TestIntegration_MigrateSchema_Idempotent(t *testing.T) {
	db := mustDB(t)
	// Running twice in a row must not error — MigrateSchema is the only path
	// the control plane uses on startup, so re-running it (restart-after-deploy
	// for example) must be safe.
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrate 1: %v", err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
}

func TestIntegration_ListCustomDomains_FiltersTerminal(t *testing.T) {
	db := mustDB(t)
	live := uniqSite("integcdla")
	dead := uniqSite("integcdld")
	defer cleanupSite(t, db, live)
	defer cleanupSite(t, db, dead)

	if err := db.UpsertSite(live, live+".test", string(SiteDomainActive), "j", "u", "wordpress"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCustomDomain(live, "live.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSite(dead, dead+".test", string(SiteDestroyed), "j", "u", "wordpress"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCustomDomain(dead, "dead.example.com"); err != nil {
		t.Fatal(err)
	}

	domains, err := db.ListCustomDomains()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var hasLive, hasDead bool
	for _, d := range domains {
		if strings.Contains(d, "live.example.com") {
			hasLive = true
		}
		if strings.Contains(d, "dead.example.com") {
			hasDead = true
		}
	}
	if !hasLive {
		t.Error("live custom domain not in ListCustomDomains output")
	}
	if hasDead {
		t.Error("DESTROYED site's custom domain leaked into ListCustomDomains output")
	}
}

func TestIntegration_UpsertSite_N8nType(t *testing.T) {
	db := mustDB(t)
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	site := uniqSite("integn8n")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test", string(SiteProvisioning), "nj1", "u", "n8n"); err != nil {
		t.Fatalf("upsert n8n: %v", err)
	}

	row, err := db.GetSite(site)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	if row.SiteType != "n8n" {
		t.Errorf("SiteType=%q, want n8n", row.SiteType)
	}

	// Verify it appears in ListSites with correct type
	sites, err := db.ListSites()
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	var found bool
	for _, s := range sites {
		if s.Site == site {
			found = true
			if s.SiteType != "n8n" {
				t.Errorf("ListSites SiteType=%q, want n8n", s.SiteType)
			}
		}
	}
	if !found {
		t.Error("n8n site not in ListSites output")
	}
}

func TestIntegration_EncryptionKey(t *testing.T) {
	db := mustDB(t)
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	site := uniqSite("integenc")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test", string(SiteProvisioning), "ej1", "u", "n8n"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Initially empty
	key, err := db.GetEncryptionKey(site)
	if err != nil {
		t.Fatalf("get initial key: %v", err)
	}
	if key != "" {
		t.Errorf("initial key=%q, want empty", key)
	}

	// Set and retrieve
	if err := db.SetEncryptionKey(site, "abc123hex"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	key, err = db.GetEncryptionKey(site)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if key != "abc123hex" {
		t.Errorf("key=%q, want abc123hex", key)
	}

	// Also visible in GetSite
	row, _ := db.GetSite(site)
	if row.EncryptionKey != "abc123hex" {
		t.Errorf("GetSite.EncryptionKey=%q, want abc123hex", row.EncryptionKey)
	}

	// Clear
	if err := db.SetEncryptionKey(site, ""); err != nil {
		t.Fatalf("clear key: %v", err)
	}
	key, _ = db.GetEncryptionKey(site)
	if key != "" {
		t.Errorf("after clear key=%q, want empty", key)
	}
}

func TestIntegration_CompleteJob_N8nDestroy(t *testing.T) {
	db := mustDB(t)
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	site := uniqSite("integn8nd")
	defer cleanupSite(t, db, site)

	if err := db.UpsertSite(site, site+".test", string(SiteDestroying), "nd1", "u", "n8n"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.InsertJob("nd1", JobN8nDestroy, site); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.CompleteJob("nd1", site, JobN8nDestroy); err != nil {
		t.Fatalf("complete: %v", err)
	}
	row, _ := db.GetSite(site)
	if row.Status != "DESTROYED" {
		t.Errorf("after JobN8nDestroy complete: status=%q, want DESTROYED", row.Status)
	}
}

func TestIntegration_DefaultSiteType(t *testing.T) {
	db := mustDB(t)
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	site := uniqSite("integdef")
	defer cleanupSite(t, db, site)

	// Existing rows without explicit site_type should default to "wordpress"
	if err := db.UpsertSite(site, site+".test", string(SiteActive), "dj1", "u", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row, _ := db.GetSite(site)
	if row.SiteType != "wordpress" {
		t.Errorf("default SiteType=%q, want wordpress", row.SiteType)
	}
}
