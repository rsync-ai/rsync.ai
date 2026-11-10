package workflows

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// A parked run replaced by a newer one is told to stop, and its workflow's last
// writes — pipelines.status "stopped", a progress state — used to land on top of
// the run that replaced it. These tests drive the real activities through a fake
// driver that records every statement, so what a superseded run is allowed to
// write is checked statement by statement. What the superseded query itself
// selects is proven against PostgreSQL in run_superseded_pg_test.go.

const (
	supPipelineID = "33333333-0000-4000-8000-000000000001"
	supRunID      = "33333333-0000-4000-8000-0000000000a1"
)

// recordedDB is the state behind the recording driver: the answers to the
// superseded check and the execution-status read, and every statement run,
// whitespace-collapsed, in order, with its arguments.
type recordedDB struct {
	mu         sync.Mutex
	superseded bool
	failCheck  bool
	// executionStatus answers the terminal write's read of its own execution row
	// ("" reads as a run still in flight); failStatusRead makes that read error.
	executionStatus string
	failStatusRead  bool
	// runClosedFirst makes the streaming handoff's close of its own run row
	// match nothing, as when a Stop or Cancel closed that row first.
	runClosedFirst bool
	statements     []string
	args           [][]driver.Value
}

func (r *recordedDB) record(q string) { r.recordWith(q, nil) }

func (r *recordedDB) recordWith(q string, args []driver.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, strings.Join(strings.Fields(q), " "))
	r.args = append(r.args, args)
}

// argsOf returns the arguments of the first recorded statement starting with
// prefix, and whether there was one.
func (r *recordedDB) argsOf(prefix string) ([]driver.Value, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.statements {
		if strings.HasPrefix(s, prefix) {
			return r.args[i], true
		}
	}
	return nil, false
}

// mentioned reports whether any recorded statement contains fragment.
func (r *recordedDB) mentioned(fragment string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.statements {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

// ran reports whether any recorded statement starts with prefix.
func (r *recordedDB) ran(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.statements {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func (r *recordedDB) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.statements, "\n")
}

var (
	recordedRegistry     sync.Map // dsn -> *recordedDB
	registerRecordedOnce sync.Once
)

const recordedDriverName = "workflows-run-superseded-recorder"

func openRecordedDB(t *testing.T, state *recordedDB) *sql.DB {
	t.Helper()
	registerRecordedOnce.Do(func() { sql.Register(recordedDriverName, recordedDriver{}) })
	dsn := t.Name()
	recordedRegistry.Store(dsn, state)
	t.Cleanup(func() { recordedRegistry.Delete(dsn) })
	db, err := sql.Open(recordedDriverName, dsn)
	if err != nil {
		t.Fatalf("open recorded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type recordedDriver struct{}

func (recordedDriver) Open(dsn string) (driver.Conn, error) {
	v, ok := recordedRegistry.Load(dsn)
	if !ok {
		return nil, errors.New("recorded db not registered: " + dsn)
	}
	return &recordedConn{state: v.(*recordedDB)}, nil
}

type recordedConn struct{ state *recordedDB }

func (c *recordedConn) Prepare(q string) (driver.Stmt, error) {
	return &recordedStmt{state: c.state, query: q}, nil
}
func (c *recordedConn) Close() error { return nil }
func (c *recordedConn) Begin() (driver.Tx, error) {
	c.state.record("BEGIN")
	return recordedTx{state: c.state}, nil
}

type recordedTx struct{ state *recordedDB }

func (t recordedTx) Commit() error   { t.state.record("COMMIT"); return nil }
func (t recordedTx) Rollback() error { t.state.record("ROLLBACK"); return nil }

type recordedStmt struct {
	state *recordedDB
	query string
}

func (s *recordedStmt) Close() error  { return nil }
func (s *recordedStmt) NumInput() int { return -1 }
func (s *recordedStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.state.recordWith(s.query, args)
	q := strings.Join(strings.Fields(s.query), " ")
	if s.state.runClosedFirst && strings.HasPrefix(q, "UPDATE executions SET status = 'completed'") {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}

func (s *recordedStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.state.recordWith(s.query, args)
	switch {
	case strings.Contains(s.query, "FROM executions WHERE id"):
		if s.state.failStatusRead {
			return nil, errors.New("connection reset")
		}
		st := s.state.executionStatus
		if st == "" {
			st = "running"
		}
		return &fakeRows{cols: []string{"status"}, data: [][]driver.Value{{st}}}, nil
	case strings.Contains(s.query, "SELECT EXISTS"):
		if s.state.failCheck {
			return nil, errors.New("connection reset")
		}
		return &fakeRows{cols: []string{"exists"}, data: [][]driver.Value{{s.state.superseded}}}, nil
	case strings.Contains(s.query, "FROM pipeline_progress WHERE pipeline_id"):
		// No progress row yet: the state write inserts version 1.
		return &fakeRows{cols: []string{"version"}}, nil
	}
	return nil, errors.New("unexpected query in recorder: " + s.query)
}

// withRecordedDB installs the recorder and a capturing producer into the
// package-global activity context for one test.
func withRecordedDB(t *testing.T, state *recordedDB) *capturingProducer {
	t.Helper()
	prev := activityCtx
	p := &capturingProducer{}
	activityCtx = &ActivityContext{KafkaProducer: p, DB: openRecordedDB(t, state)}
	t.Cleanup(func() { activityCtx = prev })
	return p
}

func runStatusActivity(t *testing.T, status string) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(UpdatePipelineStatusActivity)
	if _, err := env.ExecuteActivity(UpdatePipelineStatusActivity, supPipelineID, supRunID, status, ""); err != nil {
		t.Fatalf("UpdatePipelineStatusActivity(%s): %v", status, err)
	}
}

func TestSupersededRunLeavesThePipelineRowToTheNewerRun(t *testing.T) {
	state := &recordedDB{superseded: true}
	producer := withRecordedDB(t, state)

	runStatusActivity(t, "stopped")

	if state.ran("UPDATE pipelines") {
		t.Fatalf("a replaced run wrote the pipeline row the newer run owns:\n%s", state.all())
	}
	// Its own execution is still recorded, keeping the reason it was closed with.
	if !state.ran("UPDATE executions SET status = $2, end_time = NOW(), error_message = CASE WHEN $4 THEN COALESCE($3, error_message)") {
		t.Fatalf("the replaced run's own execution was not recorded:\n%s", state.all())
	}
	if !state.ran("COMMIT") {
		t.Fatalf("the terminal write did not commit:\n%s", state.all())
	}
	if len(producer.sent) != 0 {
		t.Fatalf("a run stopped by its replacement raised %d alert(s)", len(producer.sent))
	}
}

// The control: the same stop with no newer run is an ordinary stop.
func TestAStopWithNoNewerRunStillStopsThePipeline(t *testing.T) {
	state := &recordedDB{superseded: false}
	producer := withRecordedDB(t, state)

	runStatusActivity(t, "stopped")

	if !state.ran("UPDATE pipelines SET status = $2, updated_at = NOW(), completed_at = NOW()") {
		t.Fatalf("an ordinary stop did not write the pipeline row:\n%s", state.all())
	}
	if !state.ran("UPDATE executions") {
		t.Fatalf("an ordinary stop did not record its execution:\n%s", state.all())
	}
	if len(producer.sent) != 1 {
		t.Fatalf("an ordinary stop raised %d alert(s), want 1", len(producer.sent))
	}
}

func TestSupersededRunSkipsANonTerminalPipelineWrite(t *testing.T) {
	state := &recordedDB{superseded: true}
	withRecordedDB(t, state)

	runStatusActivity(t, "running")

	if state.ran("UPDATE pipelines") || state.ran("UPDATE executions") {
		t.Fatalf("a replaced run wrote after it was replaced:\n%s", state.all())
	}
}

func TestSupersededRunSkipsItsProgressWrite(t *testing.T) {
	for _, superseded := range []bool{true, false} {
		state := &recordedDB{superseded: superseded}
		t.Run(map[bool]string{true: "superseded", false: "current"}[superseded], func(t *testing.T) {
			withRecordedDB(t, state)
			err := stateUpdateActivity(context.Background(), StateUpdateInput{
				PipelineID:  supPipelineID,
				ExecutionID: supRunID,
				Status:      "cancelled",
				EventType:   "PIPELINE_CANCELLED",
				Summary:     "Pipeline cancelled",
			})
			if err != nil {
				t.Fatalf("stateUpdateActivity: %v", err)
			}
			wrote := state.ran("INSERT INTO pipeline_progress")
			if superseded && wrote {
				t.Fatalf("a replaced run overwrote the newer run's progress:\n%s", state.all())
			}
			if !superseded && !wrote {
				t.Fatalf("the current run's progress was not written:\n%s", state.all())
			}
		})
	}
}

// When the check cannot answer, the write goes ahead exactly as it did before
// the check existed: a run that cannot prove it was replaced is not.
func TestRunSupersededFailsOpen(t *testing.T) {
	cases := []struct {
		name      string
		execID    string
		failCheck bool
		wantQuery bool
	}{
		{"no execution id", "", false, false},
		{"an id that is not a run", "pipeline-legacy-id", false, false},
		{"the query fails", supRunID, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &recordedDB{superseded: true, failCheck: tc.failCheck}
			db := openRecordedDB(t, state)
			if runSuperseded(context.Background(), db, tc.execID) {
				t.Fatal("runSuperseded = true, want false")
			}
			if got := state.ran("SELECT EXISTS"); got != tc.wantQuery {
				t.Fatalf("queried = %v, want %v", got, tc.wantQuery)
			}
		})
	}
	if runSuperseded(context.Background(), nil, supRunID) {
		t.Fatal("runSuperseded with no DB = true, want false")
	}
}
