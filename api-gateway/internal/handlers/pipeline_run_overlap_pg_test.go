package handlers

// Real-PostgreSQL coverage for what the run-overlap check reads and writes. The
// sqlmock tests in pipeline_run_overlap_test.go match statements as strings and
// never execute them, so they cannot show which executions openRunsSQL returns
// or that applyRunOverlap leaves a finished run alone.
//
// Skipped unless OVERLAP_PG_DSN points at a scratch database with the migrations
// applied (it deletes and re-seeds its own pipeline):
//
//	docker run -d --name sup-pg -e POSTGRES_PASSWORD=verify \
//	    -e POSTGRES_DB=pipeline_db -p 55451:5432 postgres:16
//	for m in api-gateway/migrations/*.sql; do
//	    docker exec -i sup-pg psql -U postgres -d pipeline_db -q < "$m"
//	done
//	OVERLAP_PG_DSN='postgres://postgres:verify@localhost:55451/pipeline_db?sslmode=disable' \
//	    go test ./internal/handlers/ -run PG_RunOverlap -v
//	docker rm -f sup-pg

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgOvUserID      = "55555555-0000-4000-8000-000000000001"
	pgOvWorkspaceID = "55555555-0000-4000-8000-000000000002"
	pgOvPipelineID  = "55555555-0000-4000-8000-000000000003"
	pgOvParked      = "55555555-0000-4000-8000-0000000000a1"
	pgOvOldOpen     = "55555555-0000-4000-8000-0000000000a2"
	pgOvJustStarted = "55555555-0000-4000-8000-0000000000a3"
	pgOvFinished    = "55555555-0000-4000-8000-0000000000a4"
	pgOvTaggedCDC   = "55555555-0000-4000-8000-0000000000a5"
	pgOvNewRun      = "55555555-0000-4000-8000-0000000000b1"
)

func pgOvDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("OVERLAP_PG_DSN")
	if dsn == "" {
		t.Skip("OVERLAP_PG_DSN not set; see the top of pipeline_run_overlap_pg_test.go")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

func pgOvExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

// pgOvSeed builds one pipeline holding every kind of execution the check has to
// tell apart, with pipeline_progress parked on pgOvParked.
func pgOvSeed(t *testing.T, db *sql.DB) {
	t.Helper()
	pgOvExec(t, db, `DELETE FROM pipeline_progress WHERE pipeline_id = $1`, pgOvPipelineID)
	pgOvExec(t, db, `DELETE FROM executions WHERE pipeline_id = $1`, pgOvPipelineID)
	pgOvExec(t, db, `DELETE FROM pipelines WHERE id = $1`, pgOvPipelineID)
	pgOvExec(t, db, `
		INSERT INTO users (id, email, password_hash, name)
		VALUES ($1, 'run-overlap-probe@example.invalid', 'x', 'run overlap probe')
		ON CONFLICT (id) DO NOTHING`, pgOvUserID)
	pgOvExec(t, db, `
		INSERT INTO workspaces (id, name, slug, owner_id)
		VALUES ($1, 'run overlap probe', 'run-overlap-probe', $2)
		ON CONFLICT (id) DO NOTHING`, pgOvWorkspaceID, pgOvUserID)
	pgOvExec(t, db, `
		INSERT INTO pipelines (id, name, natural_language_request, status, workspace_id)
		VALUES ($1, 'run-overlap-probe', 'probe', 'running', $2)`, pgOvPipelineID, pgOvWorkspaceID)

	run := func(id, status, trigger string, secondsAgo int) {
		pgOvExec(t, db, `
			INSERT INTO executions (id, pipeline_id, status, trigger_source, start_time)
			VALUES ($1, $2, $3, NULLIF($4, ''), NOW() - ($5 * INTERVAL '1 second'))`,
			id, pgOvPipelineID, status, trigger, secondsAgo)
	}
	run(pgOvParked, "running", "manual", 3600)
	run(pgOvOldOpen, "pending", "scheduled", 1800)
	run(pgOvJustStarted, "pending", "scheduled", 10)
	run(pgOvFinished, "completed", "manual", 900)
	run(pgOvTaggedCDC, "running", "cdc", 60)
	// The CDC stream's own row: id = pipeline id, and no trigger tag here, so only
	// the id exclusion keeps it out.
	pgOvExec(t, db, `
		INSERT INTO executions (id, pipeline_id, status, start_time)
		VALUES ($1, $1, 'running', NOW() - INTERVAL '2 hours')`, pgOvPipelineID)

	pgOvExec(t, db, `
		INSERT INTO pipeline_progress (pipeline_id, execution_id, status, message, version)
		VALUES ($1, $2, 'waiting_for_user', 'Which tables?', 3)`, pgOvPipelineID, pgOvParked)
}

func TestPG_RunOverlap_OpenRunsQuery(t *testing.T) {
	db := pgOvDB(t)
	pgOvSeed(t, db)

	rows, err := db.Query(openRunsSQL, pgOvPipelineID, 120)
	if err != nil {
		t.Fatalf("openRunsSQL: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		var parked, recent bool
		if err := rows.Scan(&id, &parked, &recent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		flags := ""
		if parked {
			flags += " parked"
		}
		if recent {
			flags += " recent"
		}
		got = append(got, id+flags)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// Newest first; the finished run and both CDC stream shapes are left out.
	want := []string{
		pgOvJustStarted + " recent",
		pgOvOldOpen,
		pgOvParked + " parked",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("open runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPG_RunOverlap_ApplyClosesOnlyWhatIsStillOpen(t *testing.T) {
	db := pgOvDB(t)
	pgOvSeed(t, db)
	_, cancelled := stubRunWorkflows(t, nil)

	// pgOvFinished is listed as stale on purpose: a run that finished between the
	// check and the close must keep the result it recorded.
	applyRunOverlap(context.Background(), db, pgOvPipelineID, pgOvNewRun, runOverlap{
		Parked: []string{pgOvParked},
		Stale:  []staleRun{{ID: pgOvOldOpen, HowEnded: workflowNotFound}, {ID: pgOvFinished, HowEnded: "completed"}},
	})

	status := func(id string) (string, string, bool) {
		var st string
		var msg sql.NullString
		var ended bool
		if err := db.QueryRow(`SELECT status, error_message, end_time IS NOT NULL FROM executions WHERE id = $1`, id).
			Scan(&st, &msg, &ended); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return st, msg.String, ended
	}

	if st, msg, ended := status(pgOvParked); st != "cancelled" || !ended ||
		msg != "Replaced by run "+pgOvNewRun+" while waiting for input." {
		t.Errorf("parked run = %q %q (ended %v), want cancelled and replaced", st, msg, ended)
	}
	if st, msg, ended := status(pgOvOldOpen); st != "failed" || !ended ||
		msg != "This run stopped without recording a result (workflow not found). Closed when run "+pgOvNewRun+" started." {
		t.Errorf("stale run = %q %q (ended %v), want failed with how it ended", st, msg, ended)
	}
	if st, msg, _ := status(pgOvFinished); st != "completed" || msg != "" {
		t.Errorf("finished run = %q %q, want its own completed result kept", st, msg)
	}
	for _, id := range []string{pgOvJustStarted, pgOvTaggedCDC, pgOvPipelineID} {
		if st, _, _ := status(id); st == "failed" || st == "cancelled" {
			t.Errorf("%s was closed (%s) though nothing listed it", id, st)
		}
	}
	if strings.Join(*cancelled, ",") != pgOvParked {
		t.Errorf("cancelled %v, want only the parked run", *cancelled)
	}
}
