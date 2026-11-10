package workflows

// Real-PostgreSQL coverage for reopenCDCStreamRowSQL: which rows the streaming
// handoff reopens, which a fake driver cannot show because it never runs the SQL.
// Uses the scratch database and seed of run_superseded_pg_test.go (same
// ADAPTER_PG_DSN; see the top of that file):
//
//	ADAPTER_PG_DSN='postgres://postgres:verify@localhost:55451/pipeline_db?sslmode=disable' \
//	    go test ./internal/workflows/ -run PG_CDCStreamRow -v

import (
	"database/sql"
	"testing"
)

const (
	pgStreamRunID = "44444444-0000-4000-8000-0000000000c1"
	pgStreamOldID = "44444444-0000-4000-8000-0000000000c2"
)

type pgStreamRow struct {
	exists     bool
	status     string
	ended      bool
	errMsg     string
	ageSeconds float64
}

func pgStreamRead(t *testing.T, db *sql.DB, id string) pgStreamRow {
	t.Helper()
	var r pgStreamRow
	var errMsg sql.NullString
	err := db.QueryRow(`
		SELECT status, end_time IS NOT NULL, error_message,
		       EXTRACT(EPOCH FROM (NOW() - start_time))::float8
		FROM executions WHERE id = $1`, id).Scan(&r.status, &r.ended, &errMsg, &r.ageSeconds)
	if err == sql.ErrNoRows {
		return r
	}
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	r.exists, r.errMsg = true, errMsg.String
	return r
}

// pgStreamStopped seeds a CDC pipeline that a Stop closed a week into
// streaming: the stream row cancelled, and a resume run in flight.
func pgStreamStopped(t *testing.T, db *sql.DB) {
	t.Helper()
	pgSupSeed(t, db)
	pgSupRun(t, db, pgSupPipelineID, "cancelled", "cdc", "Cancelled by user", 7*86400)
	pgSupExec(t, db, `UPDATE executions SET end_time = NOW() - INTERVAL '1 minute' WHERE id = $1`, pgSupPipelineID)
	pgSupRun(t, db, pgStreamOldID, "cancelled", "manual", "Cancelled by user", 8*86400)
	pgSupRun(t, db, pgStreamRunID, "running", "manual", "", 20)
}

func TestPG_CDCStreamRow_AResumeReopensIt(t *testing.T) {
	db := pgSupDB(t)
	pgStreamStopped(t, db)

	pgSupFinish(t, pgStreamRunID, "streaming_active", "")

	s := pgStreamRead(t, db, pgSupPipelineID)
	if s.status != "running" || s.ended || s.errMsg != "" || s.ageSeconds > 60 {
		t.Fatalf("stream row after resume = %+v, want running, no end_time, no error, started just now", s)
	}
	if run := pgStreamRead(t, db, pgStreamRunID); run.status != "completed" {
		t.Fatalf("the resume run's own row = %q, want completed", run.status)
	}
	if old := pgStreamRead(t, db, pgStreamOldID); old.status != "cancelled" {
		t.Fatalf("an older cancelled run of the pipeline was reopened (%q): only the stream row may be", old.status)
	}
}

func TestPG_CDCStreamRow_AStoppedPipelineKeepsItClosed(t *testing.T) {
	db := pgSupDB(t)
	pgStreamStopped(t, db)
	// Stop's first write: the pipeline row, before it closes the executions.
	pgSupExec(t, db, `UPDATE pipelines SET status = 'stopped' WHERE id = $1`, pgSupPipelineID)

	pgSupFinish(t, pgStreamRunID, "streaming_active", "")

	if s := pgStreamRead(t, db, pgSupPipelineID); s.status != "cancelled" {
		t.Fatalf("the stream row of a pipeline marked stopped was reopened: %+v", s)
	}
}

func TestPG_CDCStreamRow_ARunStoppedFirstKeepsItClosed(t *testing.T) {
	db := pgSupDB(t)
	pgStreamStopped(t, db)
	pgSupExec(t, db, `UPDATE executions SET status = 'cancelled', end_time = NOW() WHERE id = $1`, pgStreamRunID)

	pgSupFinish(t, pgStreamRunID, "streaming_active", "")

	if s := pgStreamRead(t, db, pgSupPipelineID); s.status != "cancelled" {
		t.Fatalf("a run a Cancel closed first reopened the stream row: %+v", s)
	}
}

func TestPG_CDCStreamRow_AFirstStartLeavesItToTheSink(t *testing.T) {
	db := pgSupDB(t)
	pgSupSeed(t, db)
	pgSupRun(t, db, pgStreamRunID, "running", "manual", "", 20)

	pgSupFinish(t, pgStreamRunID, "streaming_active", "")

	if s := pgStreamRead(t, db, pgSupPipelineID); s.exists {
		t.Fatalf("the handoff created a stream row (%+v); the sink creates it on its first write", s)
	}
}

func TestPG_CDCStreamRow_AnOpenRowKeepsItsStart(t *testing.T) {
	db := pgSupDB(t)
	pgSupSeed(t, db)
	pgSupRun(t, db, pgSupPipelineID, "running", "cdc", "", 2*86400)
	pgSupRun(t, db, pgStreamRunID, "running", "manual", "", 20)

	pgSupFinish(t, pgStreamRunID, "streaming_active", "")

	if s := pgStreamRead(t, db, pgSupPipelineID); s.status != "running" || s.ageSeconds < 86400 {
		t.Fatalf("an open stream row was rewritten (%+v); its start must stay two days back", s)
	}
}
