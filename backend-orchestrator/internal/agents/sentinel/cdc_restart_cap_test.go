package sentinel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Pins the restart cap (FINDING-04) against a connector whose restart looks like a
// recovery for a few minutes.
//
// Seen live on 2026-10-22: a MongoDB connector whose resume token had left the oplog
// was restarted by the Sentinel every 6 minutes, always as "attempt 1 of 3". Each
// restart put the task back in RUNNING while Debezium retried the lost token 30 times,
// 10 s apart, before the task FAILED again. The Sentinel saw that RUNNING, took it as a
// recovery and forgot the attempts, so the cap never reached 3 and the pipeline was
// never stopped or escalated.
func TestRestartCapHoldsWhenEachRestartRunsBrieflyThenFailsAgain(t *testing.T) {
	t.Setenv("CDC_SENTINEL_MAX_RESTARTS", "3")
	t.Setenv("CDC_SENTINEL_RESTART_COOLDOWN", "1ns")
	t.Setenv("CDC_SENTINEL_RESTART_STABLE_AFTER", "1h")

	const (
		pid    = "cf314b4f-0d66-4bcc-ac16-d272e3688e00"
		conn   = "cdc-cf314b4f"
		cycles = 6
	)

	var restarts int32
	connect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/connectors/"+conn+"/restart") {
			atomic.AddInt32(&restarts, 1)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer connect.Close()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cdcMode := regexp.QuoteMeta("SELECT cdc_mode FROM pipelines")
	expectCDCMode := func() {
		mock.ExpectQuery(cdcMode).WithArgs(pid).
			WillReturnRows(sqlmock.NewRows([]string{"cdc_mode"}).AddRow("initial"))
	}
	// Three FAILED polls restart the connector; the fourth finds the cap spent, stops
	// the pipeline and raises the connector-down issue; later polls stay quiet.
	for i := 0; i < 4; i++ {
		expectCDCMode()
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE pipelines")).
		WithArgs(pid, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO sentinel_active_issues")).
		WillReturnRows(detectedAtRow())
	for i := 4; i < cycles; i++ {
		expectCDCMode()
	}

	s := &CDCSentinel{
		db:           db,
		httpClient:   connect.Client(),
		connectURL:   connect.URL,
		restartState: make(map[string]*connRestartState),
	}
	ctx := context.Background()
	failed := map[string]interface{}{"state": "FAILED"}

	for i := 0; i < cycles; i++ {
		s.handleFailedConnector(ctx, pid, conn, failed)
		// The restarted task reports RUNNING while Debezium retries, then fails again.
		s.noteConnectorRunning(conn, time.Now())
	}

	if got := atomic.LoadInt32(&restarts); got != 3 {
		t.Fatalf("Connect saw %d restarts over %d failures, want 3: the cap reset on the "+
			"RUNNING that follows each restart, so it can never be reached", got, cycles)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the spent cap did not stop the pipeline and raise the issue: %v", err)
	}
}

// The other half of FINDING-04: a connector that really recovers must get its attempts
// back, or one bad week would leave it a single restart from being stopped for good.
func TestRestartStateClearsOnceTheConnectorStaysRunning(t *testing.T) {
	t.Setenv("CDC_SENTINEL_RESTART_STABLE_AFTER", "30m")

	const conn = "cdc-abc12345"
	start := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
	s := &CDCSentinel{restartState: map[string]*connRestartState{
		conn: {attempts: 2, firstAttempt: start, lastAttempt: start},
	}}

	s.noteConnectorRunning(conn, start.Add(time.Minute))
	if s.restartState[conn] == nil || s.restartState[conn].attempts != 2 {
		t.Fatalf("the first RUNNING poll after a restart cleared the attempts: %+v", s.restartState[conn])
	}
	s.noteConnectorRunning(conn, start.Add(20*time.Minute))
	if s.restartState[conn] == nil {
		t.Fatal("cleared after 19 min of RUNNING, inside the 30 min stability period")
	}
	s.noteConnectorRunning(conn, start.Add(31*time.Minute))
	if st := s.restartState[conn]; st != nil {
		t.Fatalf("still tracked after 30 min of RUNNING: %+v", st)
	}
}

func TestNoteConnectorRunningIgnoresAConnectorNeverRestarted(t *testing.T) {
	s := &CDCSentinel{restartState: make(map[string]*connRestartState)}
	s.noteConnectorRunning("cdc-healthy", time.Now())
	if len(s.restartState) != 0 {
		t.Fatalf("a healthy connector got restart bookkeeping: %+v", s.restartState)
	}
}

func TestConnectorRecoveredNeedsAnUnbrokenStreak(t *testing.T) {
	const stable = 30 * time.Minute
	t0 := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)

	st, ok := connectorRecovered(connRestartState{attempts: 1}, t0, stable)
	if ok || !st.runningSince.Equal(t0) {
		t.Fatalf("first RUNNING poll: recovered=%v runningSince=%v, want false and %v", ok, st.runningSince, t0)
	}
	if _, ok := connectorRecovered(st, t0.Add(stable-time.Second), stable); ok {
		t.Fatal("recovered before the stability period ended")
	}
	if _, ok := connectorRecovered(st, t0.Add(stable), stable); !ok {
		t.Fatal("not recovered after a full stability period of RUNNING")
	}
}
