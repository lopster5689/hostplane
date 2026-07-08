//go:build integration

package main

import (
	"os"
	"testing"
)

// Phase 7 v2 — Docker-bound integration tests.
//
// These tests exercise provisioner.go, destroyer.go, static_provisioner.go,
// backupper.go, and the full job worker against real Docker + MariaDB.
//
// Today they SKIP because the CI environment in .github/workflows/ci.yml
// only spins up MariaDB. To make these run, the workflow must additionally:
//
//   1. Mount the host Docker socket  (-v /var/run/docker.sock:/var/run/docker.sock)
//   2. Pull a slim wordpress image and tag it as hostplane/wordpress:php8.2-fpm
//   3. Create the wp_backend Docker network
//   4. Create a test Caddy container with a /etc/caddy/sites volume
//
// The CI step block to add is roughly:
//
//   - name: prepare docker fixtures
//     run: |
//       docker network create wp_backend
//       docker pull wordpress:php8.2-fpm
//       docker tag wordpress:php8.2-fpm hostplane/wordpress:php8.2-fpm
//       docker run -d --name caddy --network wp_backend -v caddy-sites:/etc/caddy/sites caddy:latest
//
// And the test invocation must set HOSTPLANE_DOCKER_E2E=1 to opt in.
//
// Test plan (already enumerated in baseline-research/testing-strategy-2026-05-15.md §6.1):
//   - TestIntegration_Provisioner_Run             — provision, assert containers + volume + caddy snippet
//   - TestIntegration_Provisioner_Idempotent      — second Run on same site is a no-op
//   - TestIntegration_Provisioner_Rollback        — broken nginx step rolls everything back
//   - TestIntegration_Destroyer_Run               — destroy removes all artifacts
//   - TestIntegration_Destroyer_Idempotent
//   - TestIntegration_StaticProvisioner_Run
//   - TestIntegration_Backupper_Roundtrip         — backup → list → restore → diff
//   - TestIntegration_Backupper_PreflightExistence — restore aborts when objects missing
//   - TestIntegration_SetDomain_DNSMismatch       — 400, no infra change
//   - TestIntegration_SetDomain_Happy             — Caddy snippet acquires server block
//   - TestIntegration_SetDomain_RemoveReverts
//   - TestIntegration_JobQueue_FailJob            — exhausted job → FAILED (fails today, see §25)
//
// Until the CI fixtures land, this file's tests skip but the test names exist
// in the binary so a future contributor can grep, fill in the body, and remove
// the skip.

func requireDockerE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("HOSTPLANE_DOCKER_E2E") != "1" {
		t.Skip("set HOSTPLANE_DOCKER_E2E=1 with CI fixtures to enable Docker-bound integration tests")
	}
}

func TestIntegration_Provisioner_Run(t *testing.T)              { requireDockerE2E(t); t.Fatal("not yet implemented — see file header") }
func TestIntegration_Provisioner_Idempotent(t *testing.T)       { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_Provisioner_Rollback(t *testing.T)         { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_Destroyer_Run(t *testing.T)                { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_Destroyer_Idempotent(t *testing.T)         { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_StaticProvisioner_Run(t *testing.T)        { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_Backupper_Roundtrip(t *testing.T)          { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_Backupper_PreflightExistence(t *testing.T) { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_SetDomain_DNSMismatch(t *testing.T)        { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_SetDomain_Happy(t *testing.T)              { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_SetDomain_RemoveReverts(t *testing.T)      { requireDockerE2E(t); t.Fatal("not yet implemented") }
func TestIntegration_JobQueue_FailJob(t *testing.T)             { requireDockerE2E(t); t.Fatal("not yet implemented") }
