//go:build integration_pg

// Real-PostgreSQL coverage for when an alert's episode opened. The sqlmock tests pin
// that the raise carries whatever the upsert returns; only a real server proves the
// upsert's RETURNING detected_at executes, that a re-raise keeps it, and that a
// relapse after the alert closed gets a new one. Run it as batch_sentinel_pg_test.go's
// header says.
package sentinel

import (
	"context"
	"testing"
	"time"
)

func TestPGEpisodeOpensOnceAndAgainAfterAClose(t *testing.T) {
	db := pgTestDB(t)
	pgSeedBatchRun(t, db, 90*time.Minute, "extracting")
	s := pgSentinel(db)
	ctx := context.Background()
	sent := captureAlerts(t)
	ackID := ackIssueID(pgTestPipelineID, "orders")

	raise := func() time.Time {
		t.Helper()
		*sent = (*sent)[:0]
		s.emitBatchIssue(ctx, ackID, IssueTypeSinkWriteRejected, IssueSeverityCritical,
			pgTestPipelineID, "episode verify", pgTestExecutionID, "seeded", map[string]interface{}{"table_name": "orders"})
		if len(*sent) != 1 {
			t.Fatalf("sent %d notifications, want 1", len(*sent))
		}
		v, _ := (*sent)[0].notification["opened_at"].(string)
		opened, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatalf("opened_at = %q: %v", v, err)
		}
		return opened
	}

	first := raise()
	// The api-gateway compares opened_at with its own timestamptz rows, so a session
	// that is not on UTC would date the episode hours away from now.
	if d := time.Since(first); d < -time.Minute || d > time.Minute {
		t.Fatalf("opened_at = %s, %s from now: the column is not being read as UTC", first, d)
	}

	time.Sleep(1100 * time.Millisecond) // opened_at has whole seconds
	if repeat := raise(); !repeat.Equal(first) {
		t.Fatalf("a re-raise moved opened_at from %s to %s: every tick would be a new episode", first, repeat)
	}

	s.resolveStaleIssues(ctx, batchAckIssuePrefix, map[string]bool{})
	if relapse := raise(); !relapse.After(first) {
		t.Fatalf("a relapse after the alert closed kept opened_at %s (was %s): it would never be delivered", relapse, first)
	}
}
