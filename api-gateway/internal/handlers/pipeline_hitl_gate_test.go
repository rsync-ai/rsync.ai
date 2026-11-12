package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"api-gateway/internal/db"
)

// A pause answered in one tab must not be answered again from another
// (pipeline a3b07ece, 2026-10-01). The chat's transform dialog outlived the
// table-selection pause it belonged to; its Apply reached the gateway three
// minutes after the run had completed, the signal failed, and the gateway wrote
// 'failed' to the execution, the pipeline and its progress row.
//
// The bug class: a resume endpoint acting on a pause without asking whether the
// run is still at it. These tests drive every resume endpoint through a fake
// database that records each statement and a fake Temporal that answers a
// closed run the way Temporal does, so the incident's own path is replayed.

const (
	hitlTestExec      = wsScopeExecution
	hitlTestForeign   = "55555555-5555-5555-5555-555555555555"
	hitlTestConn      = "66666666-6666-6666-6666-666666666666"
	hitlTestPausedAt  = "2026-10-01T16:49:58Z"
	hitlLegacyWorkflo = "pipeline-workflow-" + wsScopePipeline
)

// ---------------------------------------------------------------------------
// a database that records what it is asked
// ---------------------------------------------------------------------------

type hitlFakeStmt struct {
	sql  string
	args []driver.Value
}

type hitlFakeRule struct {
	match *regexp.Regexp
	query func(args []driver.Value) ([]string, [][]driver.Value)
	exec  func(args []driver.Value) int64
}

type hitlFakeDB struct {
	mu    sync.Mutex
	rules []hitlFakeRule
	stmts []hitlFakeStmt
}

func (f *hitlFakeDB) onQuery(pattern string, fn func(args []driver.Value) ([]string, [][]driver.Value)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, hitlFakeRule{match: regexp.MustCompile(pattern), query: fn})
}

func (f *hitlFakeDB) onExec(pattern string, fn func(args []driver.Value) int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, hitlFakeRule{match: regexp.MustCompile(pattern), exec: fn})
}

func (f *hitlFakeDB) record(q string, args []driver.Value) []hitlFakeRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stmts = append(f.stmts, hitlFakeStmt{sql: q, args: args})
	return append([]hitlFakeRule(nil), f.rules...)
}

var hitlWriteSQL = regexp.MustCompile(`(?is)^\s*(UPDATE|INSERT|DELETE)\b`)

// writes is every statement that changed something.
func (f *hitlFakeDB) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.stmts {
		if hitlWriteSQL.MatchString(s.sql) {
			out = append(out, strings.Join(strings.Fields(s.sql), " "))
		}
	}
	return out
}

type hitlFakeConnector struct{ f *hitlFakeDB }

func (c hitlFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &hitlFakeConn{f: c.f}, nil
}
func (c hitlFakeConnector) Driver() driver.Driver { return hitlFakeDriver{} }

type hitlFakeDriver struct{}

func (hitlFakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("open through the connector")
}

type hitlFakeConn struct{ f *hitlFakeDB }

func (c *hitlFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not faked")
}
func (c *hitlFakeConn) Close() error { return nil }
func (c *hitlFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not faked")
}

func hitlArgValues(nv []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(nv))
	for i, v := range nv {
		out[i] = v.Value
	}
	return out
}

func (c *hitlFakeConn) QueryContext(_ context.Context, q string, nv []driver.NamedValue) (driver.Rows, error) {
	args := hitlArgValues(nv)
	for _, r := range c.f.record(q, args) {
		if r.query != nil && r.match.MatchString(q) {
			cols, rows := r.query(args)
			return &hitlFakeRows{cols: cols, rows: rows}, nil
		}
	}
	return &hitlFakeRows{}, nil // unscripted reads find nothing
}

func (c *hitlFakeConn) ExecContext(_ context.Context, q string, nv []driver.NamedValue) (driver.Result, error) {
	args := hitlArgValues(nv)
	for _, r := range c.f.record(q, args) {
		if r.exec != nil && r.match.MatchString(q) {
			return driver.RowsAffected(r.exec(args)), nil
		}
	}
	return driver.RowsAffected(0), nil
}

type hitlFakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *hitlFakeRows) Columns() []string { return r.cols }
func (r *hitlFakeRows) Close() error      { return nil }
func (r *hitlFakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

func hitlRows(cols []string, rows ...[]driver.Value) func([]driver.Value) ([]string, [][]driver.Value) {
	return func([]driver.Value) ([]string, [][]driver.Value) { return cols, rows }
}

// hitlRunRow is what the database says about the run being answered.
type hitlTestRow struct {
	missing  bool // the execution is not this pipeline's
	progress string
	blocking string
}

// newHITLFakeDB scripts a member's pipeline whose progress row points at
// hitlTestExec. The pause claim is a real compare-and-set, so a duplicate submit
// loses it the way it would against Postgres.
func newHITLFakeDB(t *testing.T, row hitlTestRow) *hitlFakeDB {
	t.Helper()
	f := &hitlFakeDB{}
	f.onQuery(`SELECT wm\.role\s+FROM pipelines r`, hitlRows([]string{"role"}, []driver.Value{"member"}))
	f.onQuery(`SELECT 1 FROM connections WHERE id`, hitlRows([]string{"one"}, []driver.Value{int64(1)}))
	f.onQuery(`FROM executions e\s+LEFT JOIN pipeline_progress pp`, func(args []driver.Value) ([]string, [][]driver.Value) {
		cols := []string{"status", "is_it", "progress", "blocking"}
		if row.missing || args[0] != hitlTestExec {
			return cols, nil
		}
		return cols, [][]driver.Value{{"running", true, row.progress, row.blocking}}
	})

	var claimMu sync.Mutex
	claimed := ""
	f.onExec(`'\{hitl_pause_claim\}'`, func(args []driver.Value) int64 {
		claimMu.Lock()
		defer claimMu.Unlock()
		key, _ := args[2].(string)
		if claimed == key {
			return 0
		}
		claimed = key
		return 1
	})
	f.onExec(`metadata - 'hitl_pause_claim'`, func(args []driver.Value) int64 {
		claimMu.Lock()
		defer claimMu.Unlock()
		if key, _ := args[1].(string); key == claimed {
			claimed = ""
			return 1
		}
		return 0
	})
	f.onQuery(`SELECT EXISTS \(SELECT 1 FROM pipeline_progress`, hitlRows([]string{"exists"}, []driver.Value{true}))
	f.onExec(`'\{hitl_tables_selected\}', \$1::jsonb, true\),\s+stage_last_heartbeat_at`, func([]driver.Value) int64 { return 1 })
	f.onExec(`.`, func([]driver.Value) int64 { return 1 })

	sqlDB := sql.OpenDB(hitlFakeConnector{f: f})
	prev := db.DB
	db.DB = sqlDB
	t.Cleanup(func() {
		db.DB = prev
		_ = sqlDB.Close()
	})
	return f
}

// ---------------------------------------------------------------------------
// a Temporal that answers like Temporal
// ---------------------------------------------------------------------------

type hitlFakeTemporal struct {
	client.Client // nil: anything not faked here panics, which is the point

	mu       sync.Mutex
	open     map[string]bool
	closed   map[string]bool
	attempts []string
	signals  []string

	// beforeSignal, when set, runs as a signal arrives — the hang-up tests close the
	// caller's tab there, at the moment the answer is on its way to the run.
	beforeSignal func()
}

func (f *hitlFakeTemporal) SignalWorkflow(ctx context.Context, workflowID, _ string, signalName string, _ interface{}) error {
	if f.beforeSignal != nil {
		f.beforeSignal()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, workflowID)
	// A real client gives up on a cancelled context before the signal leaves.
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case f.open[workflowID]:
		f.signals = append(f.signals, workflowID+" "+signalName)
		return nil
	case f.closed[workflowID]:
		return serviceerror.NewNotFound("workflow execution already completed")
	}
	return serviceerror.NewNotFound("workflow not found for ID: " + workflowID)
}

func (f *hitlFakeTemporal) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.signals...)
}

func installHITLTemporal(t *testing.T, open, closed []string) *hitlFakeTemporal {
	t.Helper()
	f := &hitlFakeTemporal{open: map[string]bool{}, closed: map[string]bool{}}
	for _, id := range open {
		f.open[id] = true
	}
	for _, id := range closed {
		f.closed[id] = true
	}
	temporalMu.Lock()
	prev := temporalClient
	temporalClient = f
	temporalMu.Unlock()
	t.Cleanup(func() {
		temporalMu.Lock()
		temporalClient = prev
		temporalMu.Unlock()
	})
	return f
}

// stubHITLRun stands in for the two questions the gate asks the run: describe
// answers per call (the last answer repeats), query answers every time.
func stubHITLRun(t *testing.T, describe []string, ans hitlWaitAnswer, queryErr error) *int {
	t.Helper()
	calls := 0
	prevDescribe, prevQuery := describeRunWorkflow, queryHITLWait
	describeRunWorkflow = func(context.Context, string) (bool, string) {
		how := describe[len(describe)-1]
		if calls < len(describe) {
			how = describe[calls]
		}
		calls++
		return how != "", how
	}
	queryHITLWait = func(context.Context, string) (hitlWaitAnswer, error) {
		return ans, queryErr
	}
	t.Cleanup(func() { describeRunWorkflow, queryHITLWait = prevDescribe, prevQuery })
	return &calls
}

func listeningAt(waitType string) hitlWaitAnswer {
	since, _ := time.Parse(time.RFC3339, hitlTestPausedAt)
	return hitlWaitAnswer{Type: waitType, WaitingSince: since, TimeoutAt: since.Add(24 * time.Hour), Receiving: true, ReceiveSeq: 1}
}

// ---------------------------------------------------------------------------
// the endpoints
// ---------------------------------------------------------------------------

type hitlEndpoint struct {
	name     string
	path     string
	handler  gin.HandlerFunc
	waitType string // the adapter's WaitReasonType; "" for a pause it does not report
	kind     hitlKind
	body     func(execID string) map[string]interface{}
}

var hitlEndpoints = []hitlEndpoint{
	{
		name: "tables", path: "/resume/tables", handler: ResumePipelineTables,
		waitType: "TABLE_SELECTION", kind: hitlKindTables,
		body: func(execID string) map[string]interface{} {
			return map[string]interface{}{"execution_id": execID, "selected_tables": []string{"public.orders"}}
		},
	},
	{
		name: "connections", path: "/resume/connections", handler: ResumePipelineConnections,
		waitType: "CONNECTION_CONFIGURATION", kind: hitlKindConnections,
		body: func(execID string) map[string]interface{} {
			return map[string]interface{}{"execution_id": execID, "source_connection_id": hitlTestConn}
		},
	},
	{
		name: "connectors", path: "/resume/connectors", handler: ResumePipelineConnectors,
		waitType: "CONNECTOR_GENERATION", kind: hitlKindConnectors,
		body: func(execID string) map[string]interface{} {
			return map[string]interface{}{"execution_id": execID, "connectors": []string{"postgresql"}}
		},
	},
	{
		name: "node input", path: "/node-input", handler: ResumePipelineNodeInput,
		kind: hitlKindNodeInput,
		body: func(execID string) map[string]interface{} {
			return map[string]interface{}{"execution_id": execID, "node_id": "n1", "message": "use orders"}
		},
	},
}

func (e hitlEndpoint) call(t *testing.T, execID string) *httptest.ResponseRecorder {
	t.Helper()
	return e.callWithContext(t, context.Background(), execID)
}

// callWithContext sends the answer on ctx, so a test can hang up mid-request.
func (e hitlEndpoint) callWithContext(t *testing.T, ctx context.Context, execID string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(e.body(execID))
	r := wsScopeRouterAsRole(http.MethodPost, "/p/:id"+e.path, e.handler, "member")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/p/"+wsScopePipeline+e.path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req.WithContext(ctx))
	return w
}

func hitlErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON (%d): %s", w.Code, w.Body.String())
	}
	code, _ := body["error"].(string)
	return code
}

var (
	hitlOutcomeWrite = regexp.MustCompile(`(?i)^UPDATE executions\b|^UPDATE pipelines SET status\b`)
	hitlEndedClear   = regexp.MustCompile(`(?i)^UPDATE pipeline_progress SET status = \$3, .* WHERE pipeline_id = \$1 AND execution_id = \$2 AND status = 'waiting_for_user'$`)
)

// assertNoOutcomeWritten: the gateway did not see how the run ended, so it must
// not record an outcome — the bug that showed a completed run as failed.
func assertNoOutcomeWritten(t *testing.T, f *hitlFakeDB) {
	t.Helper()
	for _, w := range f.writes() {
		if hitlOutcomeWrite.MatchString(w) {
			t.Errorf("the gateway recorded an outcome it did not see: %s", w)
		}
	}
}

// ---------------------------------------------------------------------------
// the tests
// ---------------------------------------------------------------------------

// The incident: a second tab answers after the run has completed. Nothing may be
// signalled or written except clearing the pause the run left on its own row.
func TestHITLResume_ALateAnswerToAFinishedRunChangesNothing(t *testing.T) {
	for _, e := range hitlEndpoints {
		t.Run(e.name, func(t *testing.T) {
			f := newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
			tc := installHITLTemporal(t, nil, []string{hitlTestExec})
			stubHITLRun(t, []string{"completed"}, hitlWaitAnswer{}, errHITLRunNotOpen)

			w := e.call(t, hitlTestExec)

			if w.Code != http.StatusConflict || hitlErrorCode(t, w) != "run_finished" {
				t.Fatalf("want 409 run_finished, got %d: %s", w.Code, w.Body.String())
			}
			if got := tc.delivered(); len(got) != 0 {
				t.Fatalf("a finished run was signalled: %v", got)
			}
			writes := f.writes()
			if len(writes) == 0 {
				t.Fatal("the run's stale pause was left on its progress row, so every open tab keeps showing a dialog nothing can answer")
			}
			for _, w := range writes {
				if !hitlEndedClear.MatchString(w) {
					t.Errorf("only the ended run's own pause may be cleared; also wrote: %s", w)
				}
			}
		})
	}
}

// The run is still going but has moved past this pause (answered in another
// tab). The late answer would otherwise wait in the signal channel and be taken
// by the run's NEXT pause of the same kind.
func TestHITLResume_AnAnsweredPauseIsNotSignalledAgain(t *testing.T) {
	for _, e := range hitlEndpoints {
		if e.waitType == "" {
			continue
		}
		t.Run(e.name, func(t *testing.T) {
			f := newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
			tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
			stubHITLRun(t, []string{""}, listeningAt("APPROVAL"), nil)

			w := e.call(t, hitlTestExec)

			if w.Code != http.StatusConflict || hitlErrorCode(t, w) != "pause_already_answered" {
				t.Fatalf("want 409 pause_already_answered, got %d: %s", w.Code, w.Body.String())
			}
			if got := tc.delivered(); len(got) != 0 {
				t.Fatalf("an answered pause was signalled again: %v", got)
			}
			if got := f.writes(); len(got) != 0 {
				t.Fatalf("a refused answer changed the database: %v", got)
			}
		})
	}
}

// At the pause but not listening (a connection fix still being validated): an
// answer now would be buffered for the next receive, so the caller retries.
func TestHITLResume_ABusyPauseAsksTheCallerToRetry(t *testing.T) {
	for _, e := range hitlEndpoints {
		if e.waitType == "" {
			continue
		}
		t.Run(e.name, func(t *testing.T) {
			f := newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
			tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
			ans := listeningAt(e.waitType)
			ans.Receiving = false
			stubHITLRun(t, []string{""}, ans, nil)

			w := e.call(t, hitlTestExec)

			if w.Code != http.StatusConflict || hitlErrorCode(t, w) != "pause_busy" {
				t.Fatalf("want 409 pause_busy, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"retryable":true`) {
				t.Fatalf("pause_busy must say it is retryable: %s", w.Body.String())
			}
			if got := tc.delivered(); len(got) != 0 {
				t.Fatalf("a busy pause was signalled: %v", got)
			}
			if got := f.writes(); len(got) != 0 {
				t.Fatalf("a refused answer changed the database: %v", got)
			}
		})
	}
}

// The run is the witness, not the row: the progress row lags the workflow, and
// a pause the run says it is listening at must be answerable.
func TestHITLResume_TheRunIsAskedNotTheLaggingRow(t *testing.T) {
	for _, e := range hitlEndpoints {
		t.Run(e.name, func(t *testing.T) {
			f := newHITLFakeDB(t, hitlTestRow{progress: "processing"})
			tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
			stubHITLRun(t, []string{""}, listeningAt(e.waitType), nil)

			w := e.call(t, hitlTestExec)

			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
			}
			if got := tc.delivered(); len(got) != 1 || !strings.HasPrefix(got[0], hitlTestExec+" ") {
				t.Fatalf("want exactly one signal to the run, got %v", got)
			}
			assertNoOutcomeWritten(t, f)
		})
	}
}

// Two tabs that both pass the run check answer the same receive: one signal.
func TestHITLResume_TwoAnswersToOneReceiveSignalOnce(t *testing.T) {
	for _, e := range hitlEndpoints {
		if e.kind != hitlKindConnections && e.kind != hitlKindConnectors {
			continue // the table claim and node input have their own guards
		}
		t.Run(e.name, func(t *testing.T) {
			newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
			tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
			stubHITLRun(t, []string{""}, listeningAt(e.waitType), nil)

			first := e.call(t, hitlTestExec)
			second := e.call(t, hitlTestExec)

			if first.Code != http.StatusOK {
				t.Fatalf("first answer: want 200, got %d: %s", first.Code, first.Body.String())
			}
			if second.Code != http.StatusConflict || hitlErrorCode(t, second) != "hitl_answer_in_flight" {
				t.Fatalf("second answer: want 409 hitl_answer_in_flight, got %d: %s", second.Code, second.Body.String())
			}
			if got := tc.delivered(); len(got) != 1 {
				t.Fatalf("want one signal for one receive, got %v", got)
			}
		})
	}
}

// A retry after a failed validation is a new receive of the same pause; the
// first receive's claim must not block it.
func TestHITLResume_ARetryReceiveIsNotBlockedByTheFirstAnswer(t *testing.T) {
	newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(hitlKindConnections)})
	tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
	e := hitlEndpoints[1]

	stubHITLRun(t, []string{""}, listeningAt(e.waitType), nil)
	if w := e.call(t, hitlTestExec); w.Code != http.StatusOK {
		t.Fatalf("first receive: want 200, got %d: %s", w.Code, w.Body.String())
	}
	retry := listeningAt(e.waitType)
	retry.ReceiveSeq = 2
	stubHITLRun(t, []string{""}, retry, nil)
	if w := e.call(t, hitlTestExec); w.Code != http.StatusOK {
		t.Fatalf("retry receive: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := tc.delivered(); len(got) != 2 {
		t.Fatalf("want one signal per receive, got %v", got)
	}
}

// A client-supplied execution_id must belong to the pipeline the caller holds a
// role on.
func TestHITLResume_AnotherPipelinesRunIsNotFound(t *testing.T) {
	for _, e := range hitlEndpoints {
		for _, execID := range []string{hitlTestForeign, "not-a-uuid"} {
			t.Run(e.name+"/"+execID, func(t *testing.T) {
				f := newHITLFakeDB(t, hitlTestRow{missing: true})
				tc := installHITLTemporal(t, []string{hitlTestForeign, "not-a-uuid"}, nil)
				stubHITLRun(t, []string{""}, listeningAt(e.waitType), nil)

				w := e.call(t, execID)

				if w.Code != http.StatusNotFound || hitlErrorCode(t, w) != "execution_not_found" {
					t.Fatalf("want 404 execution_not_found, got %d: %s", w.Code, w.Body.String())
				}
				if got := tc.delivered(); len(got) != 0 {
					t.Fatalf("another pipeline's run was signalled: %v", got)
				}
				if got := f.writes(); len(got) != 0 {
					t.Fatalf("a refused answer changed the database: %v", got)
				}
			})
		}
	}
}

// The run ends between the check and the signal. The signal fails the way
// Temporal fails it; the answer is "finished", and no outcome is recorded.
func TestHITLResume_ARunThatEndsDuringTheAnswerIsNotMarkedFailed(t *testing.T) {
	for _, e := range hitlEndpoints {
		t.Run(e.name, func(t *testing.T) {
			f := newHITLFakeDB(t, hitlTestRow{progress: "waiting_for_user", blocking: string(e.kind)})
			installHITLTemporal(t, nil, []string{hitlTestExec})
			stubHITLRun(t, []string{"", "completed"}, listeningAt(e.waitType), nil)

			w := e.call(t, hitlTestExec)

			if w.Code != http.StatusConflict || hitlErrorCode(t, w) != "run_finished" {
				t.Fatalf("want 409 run_finished, got %d: %s", w.Code, w.Body.String())
			}
			assertNoOutcomeWritten(t, f)
		})
	}
}

// A worker that cannot answer hitl_wait (still on older code, or slow) leaves
// the progress row as the witness.
func TestHITLResume_WhenTheRunCannotBeAskedTheRowDecides(t *testing.T) {
	queryFailed := serviceerror.NewQueryFailed("unknown queryType hitl_wait")
	cases := []struct {
		name string
		row  hitlTestRow
		code int
		err  string
	}{
		{"row shows this pause", hitlTestRow{progress: "waiting_for_user", blocking: "table_selection"}, http.StatusOK, ""},
		{"row shows the pause answered", hitlTestRow{progress: "processing"}, http.StatusConflict, "pause_already_answered"},
		{"row shows another pause", hitlTestRow{progress: "waiting_for_user", blocking: "connection_config"}, http.StatusConflict, "pause_already_answered"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			newHITLFakeDB(t, tt.row)
			tc := installHITLTemporal(t, []string{hitlTestExec}, nil)
			stubHITLRun(t, []string{""}, hitlWaitAnswer{}, queryFailed)

			w := hitlEndpoints[0].call(t, hitlTestExec)

			if w.Code != tt.code {
				t.Fatalf("want %d, got %d: %s", tt.code, w.Code, w.Body.String())
			}
			if tt.err != "" && hitlErrorCode(t, w) != tt.err {
				t.Fatalf("want %s, got %s", tt.err, w.Body.String())
			}
			wantSignals := 0
			if tt.code == http.StatusOK {
				wantSignals = 1
			}
			if got := tc.delivered(); len(got) != wantSignals {
				t.Fatalf("want %d signals, got %v", wantSignals, got)
			}
		})
	}
}

// The helper behind every resume reported the LEGACY workflow's "not found"
// when the run's own workflow said "already completed" — which is how a
// completed run came to be written down as failed.
func TestSignalPipelineWorkflowWithFallback_ReportsTheRunsOwnError(t *testing.T) {
	t.Run("closed run", func(t *testing.T) {
		installHITLTemporal(t, nil, []string{hitlTestExec})
		err := signalPipelineWorkflowWithFallback(context.Background(), wsScopePipeline, hitlTestExec, "tables_selected", nil)
		if !isWorkflowAlreadyCompletedError(err) {
			t.Fatalf("want the run's own 'already completed', got %v", err)
		}
	})
	t.Run("legacy workflow still answers", func(t *testing.T) {
		tc := installHITLTemporal(t, []string{hitlLegacyWorkflo}, nil)
		if err := signalPipelineWorkflowWithFallback(context.Background(), wsScopePipeline, hitlTestExec, "pause", nil); err != nil {
			t.Fatalf("a pre-execution-id workflow must still be reachable: %v", err)
		}
		if got := tc.delivered(); len(got) != 1 || !strings.HasPrefix(got[0], hitlLegacyWorkflo) {
			t.Fatalf("want one signal to the legacy workflow, got %v", got)
		}
	})
	t.Run("a closed run is not answered by the legacy workflow", func(t *testing.T) {
		tc := installHITLTemporal(t, []string{hitlLegacyWorkflo}, []string{hitlTestExec})
		err := signalPipelineWorkflowWithFallback(context.Background(), wsScopePipeline, hitlTestExec, "tables_selected", nil)
		if !isWorkflowAlreadyCompletedError(err) {
			t.Fatalf("want 'already completed', got %v", err)
		}
		if got := tc.delivered(); len(got) != 0 {
			t.Fatalf("the legacy workflow took an answer meant for a finished run: %v", got)
		}
	})
	t.Run("nothing to signal", func(t *testing.T) {
		installHITLTemporal(t, nil, nil)
		if err := signalPipelineWorkflowWithFallback(context.Background(), "", "", "pause", nil); err == nil {
			t.Fatal("signalling no workflow at all must not report success")
		}
	})
}

func TestIsWorkflowNotFoundError_MatchesOnlyTemporalsNotFound(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{sql.ErrNoRows, false},
		{errors.New("connection not found"), false},
		{errors.New("table not found in source"), false},
		{serviceerror.NewNotFound("workflow not found for ID: x"), true},
		{fmt.Errorf("signal: %w", serviceerror.NewNotFound("sql: no rows in result set")), true},
		{errors.New("workflow execution not found"), true},
	}
	for _, tt := range cases {
		if got := isWorkflowNotFoundError(tt.err); got != tt.want {
			t.Errorf("isWorkflowNotFoundError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// the cross-module wire contract
// ---------------------------------------------------------------------------

const hitlAdapterQueryFile = "../../../backend-temporal-adapter/internal/workflows/nl_pipeline_v2_hitl_query.go"

func parseHITLAdapterFile(t *testing.T) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), hitlAdapterQueryFile, nil, 0)
	if err != nil {
		t.Fatalf("could not read the adapter's hitl_wait contract at %s: %v\n\n"+
			"This test is the only thing holding the two sides equal; it must fail "+
			"loudly rather than skip when it cannot see the other side.", hitlAdapterQueryFile, err)
	}
	return f
}

// A misspelled query name comes back as QueryFailed, which the gate reads as
// "could not ask" and answers from the lagging row — the bug this fixes, back.
func TestTheGatewaySpellsTheHITLWaitQueryTheAdapterRegisters(t *testing.T) {
	var adapter string
	ast.Inspect(parseHITLAdapterFile(t), func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "HITLWaitQuery" || len(vs.Values) != 1 {
			return true
		}
		if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			adapter, _ = strconv.Unquote(lit.Value)
		}
		return false
	})
	if adapter == "" {
		t.Fatalf("no HITLWaitQuery string constant in %s", hitlAdapterQueryFile)
	}
	if adapter != hitlWaitQuery {
		t.Fatalf("the adapter registers %q, the gateway asks %q", adapter, hitlWaitQuery)
	}
}

// A renamed field decodes as its zero value: receiving=false reads as "busy"
// for every pause, forever.
func TestTheGatewayDecodesEveryHITLWaitFieldTheAdapterSends(t *testing.T) {
	adapter := map[string]bool{}
	ast.Inspect(parseHITLAdapterFile(t), func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "HITLWaitState" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil {
				continue
			}
			raw, _ := strconv.Unquote(field.Tag.Value)
			name := strings.Split(reflect.StructTag(raw).Get("json"), ",")[0]
			if name != "" && name != "-" {
				adapter[name] = true
			}
		}
		return false
	})
	if len(adapter) < 6 {
		t.Fatalf("found %d json fields on the adapter's HITLWaitState (%v), want 6", len(adapter), adapter)
	}

	gateway := map[string]bool{}
	rt := reflect.TypeOf(hitlWaitAnswer{})
	for i := 0; i < rt.NumField(); i++ {
		gateway[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	if !reflect.DeepEqual(adapter, gateway) {
		t.Fatalf("adapter sends %v, gateway decodes %v", adapter, gateway)
	}
}
