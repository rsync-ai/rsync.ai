//go:build integration_pg

// Real-PostgreSQL coverage for the orphaned-issue reaper.
//
// This lives beside issue_sweep_pg_test.go and reuses its fixture on purpose:
// that fixture already seeds the one row that separates a correct reaper from a
// destructive one — a cdc_pipeline-typed issue whose component_id is a
// replication SLOT NAME, not a UUID (cdc_wal_watchdog.go:572-576).
//
// A reaper written as "close every pipeline issue whose component_id matches no
// pipeline" passes the happy-path test and silently closes that alarm too, because
// a slot name matches no pipeline BY CONSTRUCTION — it never referred to one. The
// orphaned slot it reports is a real, ongoing problem (an unattached slot pins WAL
// and can fill the source disk), so closing it is worse than the leak this fix is
// for: it turns a stuck-open alarm into a disappearing one.
//
// sqlmock cannot settle this. It matches statements as strings and never runs
// them, so the regex shape-guard would "pass" there whether or not PostgreSQL
// agrees with it. Only a real planner over a real mixed-content VARCHAR column
// does. Same reason issue_sweep_pg_test.go exists — see #723's `text = uuid`.
//
// Server setup: see the header of issue_sweep_pg_test.go.
package heal

import (
	"context"
	"database/sql"
	"testing"
)

// pgIssueState reports whether the row is still present and whether it has been
// closed out. Both halves matter: the reaper must set resolved_at, NOT DELETE.
// The table is the healer's audit trail of what it was told and when — a DELETE
// erases the evidence that the alarm ever fired, which is precisely what makes
// the current leak hard to reason about after the fact.
func pgIssueState(t *testing.T, db *sql.DB, issueID string) (exists, resolved bool) {
	t.Helper()
	err := db.QueryRow(
		`SELECT resolved_at IS NOT NULL FROM sentinel_active_issues WHERE id = $1`,
		issueID).Scan(&resolved)
	if err == sql.ErrNoRows {
		return false, false
	}
	if err != nil {
		t.Fatalf("read resolved_at for %q: %v", issueID, err)
	}
	return true, resolved
}

// TestPGReapClosesIssuesWhosePipelineWasDeleted is the leak itself.
//
// Every resolver the Sentinel has is a RE-OBSERVATION resolver: batch_sentinel.go
// resolveStaleIssues, cdc_sentinel.go resolveLagIssue, cdc_wal_watchdog.go
// resolveWALAlert all close a row on the tick that finds the component healthy
// again. Once the pipeline is deleted there is no next observation, so the row is
// unreachable by every one of them and stays open forever. Measured on production
// 2026-08-03: 9 of 10 open issues pointed at deleted pipelines, and 0 of 10 rows
// had ever been resolved by anything.
func TestPGReapClosesIssuesWhosePipelineWasDeleted(t *testing.T) {
	db := pgHealDB(t)
	issueID := pgSeedOpenIssue(t, db)
	slotIssueID := "cdc-wal-" + pgHealOrphanSlotComponentID

	// The event no resolver survives.
	if _, err := db.Exec(`DELETE FROM pipelines WHERE id = $1::uuid`, pgHealPipelineID); err != nil {
		t.Fatalf("delete pipeline: %v", err)
	}

	n, err := pgHealWorker(db).reapOrphanedIssues(context.Background())
	if err != nil {
		t.Fatalf("reapOrphanedIssues: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d issues, want exactly 1 (the orphan, and only the orphan)", n)
	}

	exists, resolved := pgIssueState(t, db, issueID)
	if !exists {
		t.Fatal("reaper DELETEd the orphaned issue; it must set resolved_at and keep the audit row")
	}
	if !resolved {
		t.Fatal("orphaned issue still has resolved_at IS NULL — the leak is not closed")
	}

	// The discriminator. This row's component_id never referred to a pipeline, so
	// "no matching pipeline" is its normal state, not evidence of an orphan.
	exists, resolved = pgIssueState(t, db, slotIssueID)
	if !exists {
		t.Fatal("reaper DELETEd the WAL-watchdog orphan-slot alarm")
	}
	if resolved {
		t.Fatal("reaper closed the WAL-watchdog orphan-slot alarm: a slot name matches no " +
			"pipeline by construction, so matching on that alone silences a live problem")
	}
}

// TestPGReapLeavesIssuesWithALivePipelineAlone is the guard against the reaper
// becoming a second, unattended way to silence alarms. An issue whose pipeline
// still exists is the healer's actual work queue (issue_sweep.go) — reaping it
// would delete that queue.
func TestPGReapLeavesIssuesWithALivePipelineAlone(t *testing.T) {
	db := pgHealDB(t)
	issueID := pgSeedOpenIssue(t, db) // pipeline deliberately left alive
	slotIssueID := "cdc-wal-" + pgHealOrphanSlotComponentID

	n, err := pgHealWorker(db).reapOrphanedIssues(context.Background())
	if err != nil {
		t.Fatalf("reapOrphanedIssues: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaped %d issues, want 0 — nothing here is orphaned", n)
	}

	for _, id := range []string{issueID, slotIssueID} {
		exists, resolved := pgIssueState(t, db, id)
		if !exists || resolved {
			t.Fatalf("issue %q: exists=%v resolved=%v, want still present and open", id, exists, resolved)
		}
	}
}

// Fixture for a slot that outlives its pipeline. pgHealPurge needs no change: the
// connection hangs off the fixture workspace, so deleting that cascades to it and
// from it to cdc_resources, and the issue row carries pgHealPipelineID.
const (
	pgHealConnectionID  = "bbbbbbbb-0000-4000-8000-000000000004"
	pgHealOutlivingSlot = "rsync_slot_outlived_probe"
)

// TestPGReapLeavesAWALAlarmWhoseSlotOutlivedItsPipeline — the WAL watchdog keys its
// row by slot and writes the pipeline's id as component_id while the pipeline exists.
// Deleting the pipeline does not delete the slot: cdc_resources.pipeline_id is ON
// DELETE SET NULL (migration 064), a failed drop leaves it 'failed' (postgresql.go
// MarkResourceFailed), and the watchdog goes on reading the slot's retained WAL every
// tick. But its upsert never clears resolved_at, so once the reaper closed the row on
// "no such pipeline" the alarm stayed hidden for as long as the slot pinned WAL —
// forever, for a WARNING slot, which nothing drops.
//
// Whether the watchdog still watches the slot is the rule, so the test runs over
// every status its listing reads, and the control is the one it doesn't: a slot
// dropped and marked 'deleted' is never observed again, and only the reaper can
// close its row.
func TestPGReapLeavesAWALAlarmWhoseSlotOutlivedItsPipeline(t *testing.T) {
	for _, tc := range []struct {
		slotStatus string
		wantReaped bool
	}{
		{"active", false},
		{"inactive", false},
		{"failed", false},
		{"orphaned", false},
		{"deleted", true}, // control: the watchdog no longer lists it
	} {
		t.Run(tc.slotStatus, func(t *testing.T) {
			db := pgHealDB(t)
			issueID := pgSeedOpenIssue(t, db)
			walIssueID := "cdc-wal-" + pgHealOutlivingSlot

			mustExec := func(q string, args ...interface{}) {
				t.Helper()
				if _, err := db.Exec(q, args...); err != nil {
					t.Fatalf("%.60q: %v", q, err)
				}
			}
			mustExec(`INSERT INTO connections (id, user_id, workspace_id, name, type, connector_type, config)
			          VALUES ($1::uuid, $2::uuid, $3::uuid, 'reaper-verify-source', 'source', 'postgresql', 'x')`,
				pgHealConnectionID, pgHealUserID, pgHealWorkspaceID)
			mustExec(`INSERT INTO cdc_resources (pipeline_id, connection_id, resource_type, resource_name, status, database_type)
			          VALUES ($1::uuid, $2::uuid, 'replication_slot', $3, 'active', 'postgresql')`,
				pgHealPipelineID, pgHealConnectionID, pgHealOutlivingSlot)
			// The row as the watchdog wrote it while the pipeline still existed.
			mustExec(`INSERT INTO sentinel_active_issues
			          (id, type, severity, component_id, component_type, description, detected_at, last_occurrence)
			          VALUES ($1, 'high_lag', 'warning', $2, 'cdc_pipeline',
			                  'replication slot is retaining WAL', NOW(), NOW())`,
				walIssueID, pgHealPipelineID)

			mustExec(`DELETE FROM pipelines WHERE id = $1::uuid`, pgHealPipelineID)
			var orphaned bool
			if err := db.QueryRow(`SELECT pipeline_id IS NULL FROM cdc_resources WHERE resource_name = $1`,
				pgHealOutlivingSlot).Scan(&orphaned); err != nil {
				t.Fatalf("the slot row did not survive the pipeline delete: %v", err)
			}
			if !orphaned {
				t.Fatal("the slot row still names the deleted pipeline; the fixture is not the state prod reaches")
			}
			mustExec(`UPDATE cdc_resources SET status = $1 WHERE resource_name = $2`, tc.slotStatus, pgHealOutlivingSlot)

			n, err := pgHealWorker(db).reapOrphanedIssues(context.Background())
			if err != nil {
				t.Fatalf("reapOrphanedIssues: %v", err)
			}
			want := 1 // the connector-down issue, whose pipeline is gone and nothing watches
			if tc.wantReaped {
				want = 2
			}
			if n != want {
				t.Errorf("reaped %d issues, want %d", n, want)
			}
			if _, resolved := pgIssueState(t, db, issueID); !resolved {
				t.Error("the connector-down issue of the deleted pipeline was left open")
			}
			exists, resolved := pgIssueState(t, db, walIssueID)
			if !exists {
				t.Fatal("reaper DELETEd the WAL alarm")
			}
			if resolved != tc.wantReaped {
				if tc.wantReaped {
					t.Errorf("a WAL alarm whose slot was dropped stayed open: nothing else will ever close it")
				} else {
					t.Errorf("reaper closed the WAL alarm of a slot the watchdog still watches (status %q): "+
						"its upsert never reopens a row, so the alarm is hidden while the slot pins WAL", tc.slotStatus)
				}
			}
		})
	}
}
