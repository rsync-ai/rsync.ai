package workflows

// Real-PostgreSQL coverage for runSupersededSQL and the writes it guards. The
// recording-driver tests in run_superseded_test.go prove which statements a
// superseded run skips; these prove the query picks the right runs, which a fake
// driver cannot: it never executes the SQL.
//
// Skipped unless ADAPTER_PG_DSN points at a scratch database with the gateway's
// migrations applied (it deletes and re-seeds its own pipeline):
//
//	docker run -d --name sup-pg -e POSTGRES_PASSWORD=verify \
//	    -e POSTGRES_DB=pipeline_db -p 55451:5432 postgres:16
//	for m in api-gateway/migrations/*.sql; do
//	    docker exec -i sup-pg psql -U postgres -d pipeline_db -q < "$m"
//	done
//	ADAPTER_PG_DSN='postgres://postgres:verify@localhost:55451/pipeline_db?sslmode=disable' \
//	    go test ./internal/workflows/ -run PG_RunSuperseded -v
//	docker rm -f sup-pg

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/rsync-ai/shared/pgdriver"
	"go.temporal.io/sdk/testsuite"
)

const (
	pgSupUserID      = "44444444-0000-4000-8000-000000000001"
	pgSupWorkspaceID = "44444444-0000-4000-8000-000000000002"
	pgSupPipelineID  = "44444444-0000-4000-8000-000000000003"
	pgSupOldRun      = "44444444-0000-4000-8000-0000000000a1"
	pgSupNewRun      = "44444444-0000-4000-8000-0000000000b1"
)

func pgSupDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ADAPTER_PG_DSN")
	if dsn == "" {
		t.Skip("ADAPTER_PG_DSN not set; see the top of run_superseded_pg_test.go")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	prev := activityCtx
	activityCtx = &ActivityContext{DB: db}
	t.Cleanup(func() { activityCtx = prev })
	return db
}

func pgSupExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

// pgSupSeed resets the pipeline to "running", owned by whatever run the test
// then inserts. The pipeline's progress row points at progressRun.
func pgSupSeed(t *testing.T, db *sql.DB) {
	t.Helper()
	pgSupExec(t, db, `DELETE FROM pipeline_progress WHERE pipeline_id = $1`, pgSupPipelineID)
	pgSupExec(t, db, `DELETE FROM executions WHERE pipeline_id = $1`, pgSupPipelineID)
	pgSupExec(t, db, `DELETE FROM pipelines WHERE id = $1`, pgSupPipelineID)
	pgSupExec(t, db, `
		INSERT INTO users (id, email, password_hash, name)
		VALUES ($1, 'run-superseded-probe@example.invalid', 'x', 'run superseded probe')
		ON CONFLICT (id) DO NOTHING`, pgSupUserID)
	pgSupExec(t, db, `
		INSERT INTO workspaces (id, name, slug, owner_id)
		VALUES ($1, 'run superseded probe', 'run-superseded-probe', $2)
		ON CONFLICT (id) DO NOTHING`, pgSupWorkspaceID, pgSupUserID)
	pgSupExec(t, db, `
		INSERT INTO pipelines (id, name, natural_language_request, status, workspace_id)
		VALUES ($1, 'run-superseded-probe', 'probe', 'running', $2)`, pgSupPipelineID, pgSupWorkspaceID)
}

// pgSupRun inserts an execution that started secondsAgo seconds ago.
func pgSupRun(t *testing.T, db *sql.DB, id, status, trigger, errMsg string, secondsAgo int) {
	t.Helper()
	pgSupExec(t, db, `
		INSERT INTO executions (id, pipeline_id, status, trigger_source, error_message, start_time)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NOW() - ($6 * INTERVAL '1 second'))`,
		id, pgSupPipelineID, status, trigger, errMsg, secondsAgo)
}

func pgSupProgress(t *testing.T, db *sql.DB, execID, status string) {
	t.Helper()
	pgSupExec(t, db, `
		INSERT INTO pipeline_progress (pipeline_id, execution_id, status, message, version)
		VALUES ($1, $2, $3, 'copying orders', 4)`, pgSupPipelineID, execID, status)
}

type pgSupState struct {
	pipelineStatus string
	completedSet   bool
	oldRunStatus   string
	oldRunError    string
	progressRun    string
	progressStatus string
}

func pgSupRead(t *testing.T, db *sql.DB) pgSupState {
	t.Helper()
	var s pgSupState
	var oldErr sql.NullString
	var progressRun, progressStatus sql.NullString
	if err := db.QueryRow(`
		SELECT p.status, p.completed_at IS NOT NULL, e.status, e.error_message,
		       pp.execution_id::text, pp.status
		FROM pipelines p
		JOIN executions e ON e.id = $2
		LEFT JOIN pipeline_progress pp ON pp.pipeline_id = p.id
		WHERE p.id = $1`, pgSupPipelineID, pgSupOldRun).
		Scan(&s.pipelineStatus, &s.completedSet, &s.oldRunStatus, &oldErr, &progressRun, &progressStatus); err != nil {
		t.Fatalf("read state: %v", err)
	}
	s.oldRunError, s.progressRun, s.progressStatus = oldErr.String, progressRun.String, progressStatus.String
	return s
}

func pgSupFinish(t *testing.T, execID, status, errMsg string) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(UpdatePipelineStatusActivity)
	if _, err := env.ExecuteActivity(UpdatePipelineStatusActivity, pgSupPipelineID, execID, status, errMsg); err != nil {
		t.Fatalf("UpdatePipelineStatusActivity(%s): %v", status, err)
	}
}

// The case the guard exists for: a parked run replaced by a newer one, whose
// workflow then exits with its "stopped" write and a last progress state.
func TestPG_RunSuperseded_ReplacedParkedRunLeavesThePipelineAlone(t *testing.T) {
	db := pgSupDB(t)
	pgSupSeed(t, db)
	const replaced = "Replaced by run " + pgSupNewRun + " while waiting for input."
	pgSupRun(t, db, pgSupOldRun, "cancelled", "manual", replaced, 600)
	pgSupRun(t, db, pgSupNewRun, "running", "manual", "", 5)
	pgSupProgress(t, db, pgSupNewRun, "processing")

	if !runSuperseded(context.Background(), db, pgSupOldRun) {
		t.Fatal("the replaced run is not seen as superseded")
	}
	if runSuperseded(context.Background(), db, pgSupNewRun) {
		t.Fatal("the run that replaced it is seen as superseded")
	}

	pgSupFinish(t, pgSupOldRun, "stopped", "")
	if err := stateUpdateActivity(context.Background(), StateUpdateInput{
		PipelineID: pgSupPipelineID, ExecutionID: pgSupOldRun,
		Status: "cancelled", EventType: "PIPELINE_CANCELLED", Summary: "Pipeline cancelled",
	}); err != nil {
		t.Fatalf("stateUpdateActivity: %v", err)
	}

	got := pgSupRead(t, db)
	if got.pipelineStatus != "running" || got.completedSet {
		t.Errorf("pipeline = %q (completed_at set: %v), want the newer run's running", got.pipelineStatus, got.completedSet)
	}
	if got.progressRun != pgSupNewRun || got.progressStatus != "processing" {
		t.Errorf("progress = %s/%s, want the newer run's %s/processing", got.progressRun, got.progressStatus, pgSupNewRun)
	}
	if got.oldRunStatus != "cancelled" || got.oldRunError != replaced {
		t.Errorf("replaced run = %q %q, want cancelled with %q", got.oldRunStatus, got.oldRunError, replaced)
	}
}

// Stop with no newer run: an ordinary stop, which now also keeps the reason
// StopPipeline recorded instead of blanking it.
func TestPG_RunSuperseded_AnOrdinaryStopKeepsItsReason(t *testing.T) {
	db := pgSupDB(t)
	pgSupSeed(t, db)
	pgSupRun(t, db, pgSupOldRun, "cancelled", "manual", "Cancelled by user", 60)
	pgSupProgress(t, db, pgSupOldRun, "processing")

	if runSuperseded(context.Background(), db, pgSupOldRun) {
		t.Fatal("a stopped run with no newer run is seen as superseded")
	}
	pgSupFinish(t, pgSupOldRun, "stopped", "")

	got := pgSupRead(t, db)
	if got.pipelineStatus != "stopped" || !got.completedSet {
		t.Errorf("pipeline = %q (completed_at set: %v), want stopped", got.pipelineStatus, got.completedSet)
	}
	if got.oldRunStatus != "cancelled" || got.oldRunError != "Cancelled by user" {
		t.Errorf("run = %q %q, want cancelled with its reason kept", got.oldRunStatus, got.oldRunError)
	}
}

// A newer run that failed to start leaves the older run open. The older run
// still owns the pipeline and records its result.
func TestPG_RunSuperseded_AnOpenRunIsNeverSuperseded(t *testing.T) {
	db := pgSupDB(t)
	pgSupSeed(t, db)
	pgSupRun(t, db, pgSupOldRun, "running", "scheduled", "", 600)
	pgSupRun(t, db, pgSupNewRun, "failed", "manual", "failed to start workflow", 5)
	pgSupProgress(t, db, pgSupOldRun, "processing")

	if runSuperseded(context.Background(), db, pgSupOldRun) {
		t.Fatal("an open run is seen as superseded")
	}
	pgSupFinish(t, pgSupOldRun, "failed", "source unreachable")

	got := pgSupRead(t, db)
	if got.pipelineStatus != "failed" || got.oldRunStatus != "failed" || got.oldRunError != "source unreachable" {
		t.Errorf("pipeline %q, run %q %q; want both failed with the run's error", got.pipelineStatus, got.oldRunStatus, got.oldRunError)
	}
	if got.progressStatus != "failed" {
		t.Errorf("progress = %q, want the run's own failure reconciled", got.progressStatus)
	}
}

// The CDC stream's bookkeeping row (id = pipeline id) starts with every stream
// and is not a run; it must never count as the newer run.
func TestPG_RunSuperseded_TheCDCStreamRowIsNotANewerRun(t *testing.T) {
	db := pgSupDB(t)
	pgSupSeed(t, db)
	pgSupRun(t, db, pgSupOldRun, "cancelled", "manual", "Cancelled by user", 600)
	// Each exclusion on its own: the stream row's id without its trigger tag...
	pgSupExec(t, db, `
		INSERT INTO executions (id, pipeline_id, status, start_time)
		VALUES ($1, $1, 'running', NOW())`, pgSupPipelineID)
	// ...and the trigger tag on a row with an id of its own.
	const taggedStream = "44444444-0000-4000-8000-0000000000c1"
	pgSupRun(t, db, taggedStream, "running", "cdc", "", 1)

	if runSuperseded(context.Background(), db, pgSupOldRun) {
		t.Fatal("the CDC stream row made a run superseded")
	}

	// The control: the same run with a real newer run is superseded.
	pgSupRun(t, db, pgSupNewRun, "running", "manual", "", 1)
	if !runSuperseded(context.Background(), db, pgSupOldRun) {
		t.Fatal("a newer manual run did not supersede the stopped run")
	}
}
