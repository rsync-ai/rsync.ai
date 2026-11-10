package heal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// recordingProducer stands in for kafka.Manager and keeps what the sweep sends.
type recordingProducer struct {
	topics []string
	keys   []string
	bodies [][]byte
	err    error
}

func (p *recordingProducer) Produce(topic string, key, value []byte) error {
	p.topics = append(p.topics, topic)
	p.keys = append(p.keys, string(key))
	p.bodies = append(p.bodies, value)
	return p.err
}

// zombieSweepRows is the statement's result shape: one row per reaped execution, the two
// repair counts, and whether the reap may tell the pipeline's owner.
func zombieSweepRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "pipeline_id", "reconciled", "pipelines_closed", "notify_eligible"})
}

func expectZombieHealEvent(mock sqlmock.Sqlmock, pipelineID, execID string) {
	mock.ExpectExec(`INSERT INTO pipeline_run_events`).
		WithArgs(pipelineID, execID, sqlmock.AnyArg(), "healer_zombie_swept", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

type sentRunFailure struct {
	Type       string `json:"type"`
	PipelineID string `json:"pipeline_id"`
	Message    string `json:"message"`
	ActionURL  string `json:"action_url"`
	Error      struct {
		Code         string `json:"code"`
		Severity     string `json:"severity"`
		Audience     string `json:"audience"`
		FailureType  string `json:"failure_type"`
		UserMessage  string `json:"user_message"`
		PipelineID   string `json:"pipeline_id"`
		ExecutionID  string `json:"execution_id"`
		DedupSubject string `json:"dedup_subject"`
	} `json:"error"`
}

func decodeRunFailures(t *testing.T, p *recordingProducer) []sentRunFailure {
	t.Helper()
	out := make([]sentRunFailure, 0, len(p.bodies))
	for i, b := range p.bodies {
		var n sentRunFailure
		if err := json.Unmarshal(b, &n); err != nil {
			t.Fatalf("notice %d is not JSON: %v (%s)", i, err, b)
		}
		out = append(out, n)
	}
	return out
}

// A run the sweep reaps has failed as far as every screen is concerned, and nobody watching
// a scheduled pipeline is looking at it when that happens. The temporal adapter's terminal
// alert cannot cover it: a zombie is precisely a run whose workflow never reached the defer
// that sends that alert. So the sweep sends the same PIPELINE_RUN_FAILED, once per reaped
// run, scoped to the execution so two reaped runs of one pipeline are two notices.
func TestSweepZombies_NotifiesEachEligibleReapedRunOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("WITH swept AS").
		WithArgs(ZombieTimeout.String(), AbandonedParkTimeout.String()).
		WillReturnRows(zombieSweepRows().
			AddRow("exec-1", "pipe-1", 2, 1, true).
			AddRow("exec-2", "pipe-1", 2, 1, true).
			AddRow("exec-3", "pipe-3", 2, 1, false))
	expectZombieHealEvent(mock, "pipe-1", "exec-1")
	expectZombieHealEvent(mock, "pipe-1", "exec-2")
	expectZombieHealEvent(mock, "pipe-3", "exec-3")

	sent := &recordingProducer{}
	w := &HealWorker{DB: db, Notifications: sent}
	w.sweepZombies(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sweep statements: %v", err)
	}
	got := decodeRunFailures(t, sent)
	if len(got) != 2 {
		t.Fatalf("sent %d notices, want 2 (one per eligible reaped run): %+v", len(got), got)
	}
	for i, wantExec := range []string{"exec-1", "exec-2"} {
		n := got[i]
		if sent.topics[i] != "rsync.notifications" {
			t.Errorf("notice %d topic = %q, want rsync.notifications", i, sent.topics[i])
		}
		if sent.keys[i] != "pipe-1" {
			t.Errorf("notice %d key = %q, want the pipeline id, so one pipeline's notices stay in order", i, sent.keys[i])
		}
		if n.Type != "structured_error_notification" || n.PipelineID != "pipe-1" || n.ActionURL != "/pipelines/pipe-1" {
			t.Errorf("notice %d envelope = %+v", i, n)
		}
		if n.Error.Code != "PIPELINE_RUN_FAILED" || n.Error.Severity != "error" ||
			n.Error.Audience != "developer" || n.Error.FailureType != "system_error" {
			t.Errorf("notice %d error = %+v, want the temporal adapter's PIPELINE_RUN_FAILED envelope", i, n.Error)
		}
		if n.Error.DedupSubject != wantExec || n.Error.ExecutionID != wantExec {
			t.Errorf("notice %d dedup_subject/execution_id = %q/%q, want %q: without it the notifier's "+
				"60-minute window folds the second reaped run into the first", i, n.Error.DedupSubject, n.Error.ExecutionID, wantExec)
		}
		if !strings.Contains(n.Message, "4 hours") || n.Message != n.Error.UserMessage {
			t.Errorf("notice %d message = %q, want the user-facing sentence naming the 4 hours", i, n.Message)
		}
		if strings.Contains(n.Message, "zombie") || strings.Contains(n.Message, "end_time") {
			t.Errorf("notice %d message leaks the sweep's internal wording: %q", i, n.Message)
		}
	}
}

// A CDC stream's setup execution is reaped while the stream keeps running, and a run the
// user paused, or whose pipeline they paused or stopped, ended by their choice. None of those
// is a failure to tell anybody about; the statement says so in notify_eligible.
func TestSweepZombies_DoesNotNotifyForCDCOrPausedRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("WITH swept AS").
		WillReturnRows(zombieSweepRows().
			AddRow("exec-cdc", "pipe-cdc", 0, 0, false).
			AddRow("exec-paused", "pipe-paused", 0, 0, false))
	expectZombieHealEvent(mock, "pipe-cdc", "exec-cdc")
	expectZombieHealEvent(mock, "pipe-paused", "exec-paused")

	sent := &recordingProducer{}
	(&HealWorker{DB: db, Notifications: sent}).sweepZombies(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the heal events must still be written for rows that do not notify: %v", err)
	}
	if len(sent.bodies) != 0 {
		t.Errorf("sent %d notices for reaps that must not notify: %s", len(sent.bodies), sent.bodies)
	}
}

// The hourly ticker runs the same statement, so it must send the same notices: whichever of
// the two sweepers reaches a row first is the only one that ever sees it.
func TestTheHourlyZombieSweeperNotifiesToo(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("WITH swept AS").
		WillReturnRows(zombieSweepRows().AddRow("exec-9", "pipe-9", 1, 1, true))
	expectZombieHealEvent(mock, "pipe-9", "exec-9")

	w := NewHealWorker(db, "")
	sent := &recordingProducer{}
	w.NotifyRunFailuresVia(sent)
	if err := w.ZombieSweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got := decodeRunFailures(t, sent)
	if len(got) != 1 || got[0].Error.DedupSubject != "exec-9" {
		t.Errorf("the hourly sweeper sent %+v, want one PIPELINE_RUN_FAILED for exec-9", got)
	}
}

// A broker outage must not fail the sweep or skip the rest of the notices: the reaps have
// already committed.
func TestAFailedNoticeDoesNotStopTheSweep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("WITH swept AS").
		WillReturnRows(zombieSweepRows().
			AddRow("exec-1", "pipe-1", 1, 1, true).
			AddRow("exec-2", "pipe-2", 1, 1, true))
	expectZombieHealEvent(mock, "pipe-1", "exec-1")
	expectZombieHealEvent(mock, "pipe-2", "exec-2")

	sent := &recordingProducer{err: errors.New("broker down")}
	if err := runZombieSweep(context.Background(), db, ZombieTimeout, sent); err != nil {
		t.Fatalf("runZombieSweep returned %v; a failed notice must not fail the sweep", err)
	}
	if len(sent.bodies) != 2 {
		t.Errorf("tried %d notices, want 2: the first failure must not skip the second", len(sent.bodies))
	}
}

// finalZombieSelect returns the statement's final SELECT, where notify_eligible is computed,
// so the assertions below cannot be satisfied by the same predicate in pipelines_closed.
func finalZombieSelect(t *testing.T) string {
	t.Helper()
	start := strings.LastIndex(sweepZombiesQuery, "RETURNING p.id")
	if start < 0 {
		t.Fatal("sweepZombiesQuery no longer ends pipelines_closed with `RETURNING p.id`")
	}
	return sweepZombiesQuery[start:]
}

// The flag's predicates, against the query text. zombie_sweep_pg_test.go runs them against
// PostgreSQL; this keeps them from being edited away in the default suite.
func TestSweepZombiesQuery_NotifyFlagExcludesStreamsAndUserPauses(t *testing.T) {
	sel := finalZombieSelect(t)
	for _, want := range []string{
		"AS notify_eligible",
		"LEFT JOIN pipelines p ON p.id = s.pipeline_id",
		"p.id IS NOT NULL",
		"p.sync_mode IS DISTINCT FROM 'cdc'",
		"p.cdc_mode IS NULL",
		"p.status NOT IN ('paused', 'stopped')",
		"pp.blocking_reason_type = 'paused_by_user'",
	} {
		if !strings.Contains(sel, want) {
			t.Errorf("the final SELECT no longer contains %q:\n%s", want, sel)
		}
	}
}
