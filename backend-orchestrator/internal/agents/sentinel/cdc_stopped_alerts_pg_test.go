//go:build integration_pg

// Real-PostgreSQL coverage for closing a stopped CDC pipeline's alerts. The sqlmock
// test pins the statement and its arguments; only a real server proves the
// DELETE … USING executes (pipelines.id is a uuid, component_id is text) and deletes
// exactly the five running-only alerts. Run it as batch_sentinel_pg_test.go's header says.
package sentinel

import (
	"context"
	"database/sql"
	"sort"
	"testing"
)

const (
	pgStopUserID      = "bbbbbbbb-0000-4000-8000-000000000001"
	pgStopWorkspaceID = "bbbbbbbb-0000-4000-8000-000000000002"
	pgStopPipelineID  = "bbbbbbbb-0000-4000-8000-000000000003"
	pgRunPipelineID   = "bbbbbbbb-0000-4000-8000-000000000004"
)

func pgStopPurge(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM sentinel_active_issues WHERE component_id IN ($1, $2)`,
		`DELETE FROM pipelines WHERE id IN ($1::uuid, $2::uuid)`,
	} {
		if _, err := db.Exec(q, pgStopPipelineID, pgRunPipelineID); err != nil {
			t.Fatalf("purge %q: %v", q, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM workspaces WHERE id = $1::uuid`, pgStopWorkspaceID); err != nil {
		t.Fatalf("purge workspace: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1::uuid`, pgStopUserID); err != nil {
		t.Fatalf("purge user: %v", err)
	}
}

func pgStopSeed(t *testing.T, db *sql.DB, stoppedStatus string) {
	t.Helper()
	pgStopPurge(t, db)
	t.Cleanup(func() { pgStopPurge(t, db) })
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %.60q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, email, password_hash) VALUES ($1::uuid, $2, 'x')`,
		pgStopUserID, "cdc-stop-verify@example.invalid")
	mustExec(`INSERT INTO workspaces (id, name, slug, owner_id)
	          VALUES ($1::uuid, 'cdc-stop-verify', 'cdc-stop-verify', $2::uuid)`,
		pgStopWorkspaceID, pgStopUserID)
	for id, status := range map[string]string{pgStopPipelineID: stoppedStatus, pgRunPipelineID: "running"} {
		mustExec(`INSERT INTO pipelines
		          (id, name, natural_language_request, workspace_id, status, sync_mode, cdc_mode, created_by)
		          VALUES ($1::uuid, 'cdc stop verify', 'verify', $2::uuid, $3, 'cdc', 'initial', $4::uuid)`,
			id, pgStopWorkspaceID, status, pgStopUserID)
	}
	issue := func(id, pipelineID string) {
		mustExec(`INSERT INTO sentinel_active_issues
		          (id, type, severity, component_id, component_type, description, detected_at, last_occurrence)
		          VALUES ($1, 'high_lag', 'critical', $2, 'cdc_pipeline', 'verify', NOW(), NOW())`,
			id, pipelineID)
	}
	for _, issueID := range alertsClosedOnStop {
		issue(issueID(pgStopPipelineID), pgStopPipelineID)
	}
	for _, kept := range pgKeptIDs(pgStopPipelineID) {
		issue(kept, pgStopPipelineID)
	}
	issue(captureStoppedIssueID(pgRunPipelineID), pgRunPipelineID)
}

func pgKeptIDs(pipelineID string) []string {
	return []string{
		connectorIssueID(pipelineID),
		sinkWedgedEscalationIssueID(pipelineID),
		rowsParkedIssueID(pipelineID),
		"cdc-position-" + pipelineID,
		"cdc-wal-rsync_cdc_" + pipelineID[:8],
	}
}

func pgOpenIssues(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM sentinel_active_issues WHERE component_id IN ($1, $2)`,
		pgStopPipelineID, pgRunPipelineID)
	if err != nil {
		t.Fatalf("read issues: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestPGStoppedPipelineKeepsOnlyItsStandingAlerts: for every stopped status, the five
// running-only alerts go, the five standing ones stay, and a running pipeline's
// capture-stopped alert is untouched.
func TestPGStoppedPipelineKeepsOnlyItsStandingAlerts(t *testing.T) {
	db := pgTestDB(t)
	for _, status := range stoppedPipelineStatuses {
		t.Run(status, func(t *testing.T) {
			pgStopSeed(t, db, status)
			if got := len(pgOpenIssues(t, db)); got != 11 {
				t.Fatalf("seeded %d issues, want 11", got)
			}

			(&CDCSentinel{db: db}).closeAlertsOfStoppedPipelines(context.Background())

			want := append(pgKeptIDs(pgStopPipelineID), captureStoppedIssueID(pgRunPipelineID))
			sort.Strings(want)
			got := pgOpenIssues(t, db)
			if len(got) != len(want) {
				t.Fatalf("open after the close:\n got  %v\n want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("open after the close:\n got  %v\n want %v", got, want)
				}
			}
		})
	}
}

// TestPGRunningPipelineKeepsItsAlerts is the control: the same seed with the pipeline
// still running (or in a transient status) closes nothing.
func TestPGRunningPipelineKeepsItsAlerts(t *testing.T) {
	db := pgTestDB(t)
	for _, status := range []string{"running", "starting"} {
		t.Run(status, func(t *testing.T) {
			pgStopSeed(t, db, status)
			(&CDCSentinel{db: db}).closeAlertsOfStoppedPipelines(context.Background())
			if got := len(pgOpenIssues(t, db)); got != 11 {
				t.Fatalf("a %s pipeline lost alerts: %d of 11 open", status, got)
			}
		})
	}
}
