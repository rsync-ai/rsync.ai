package executor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/backend-orchestrator/internal/runcancel"
)

// KI-STOP-DURING-LANDING-CHECK-REPORTED-AS-DROP. After dispatching a batch the
// executor waits for the sink to confirm every row (reconcileLandedRows). A Stop
// during that wait ended the run's context, the ack-ledger read failed under it
// and read as "no acks", and the run was judged a total drop: a "🚨 Silent drop
// detected" warning, then a checkpoint rewind that also died under the ended
// context and logged an ERROR telling ops to run a Reload. Nothing was lost —
// the rows produced but not yet written stay in Kafka for the next run.
//
// Bug class: a verdict drawn from reads that the end of the run's context cut
// short. The landing check now reports such a run as cut short ("cancelled",
// settled by the worker into "stopped by user"), and only a negative ack read
// before the end still counts as evidence.

func stoppedCtx() (context.Context, context.CancelCauseFunc) {
	return context.WithCancelCause(context.Background())
}

func ackRow(landed, received, acks int64, lastErr interface{}) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"landed", "received", "acks", "last_error"}).AddRow(landed, received, acks, lastErr)
}

// A Stop that came before the landing check: every ledger read fails.
func TestALandingCheckStartedAfterAStopIsNotADrop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &Agent{db: db}
	ctx, stop := stoppedCtx()
	stop(runcancel.ErrStopped)

	d, _, _ := a.checkLanding(ctx, "p1", "e1", 500, 5, 0, 5, time.Minute)
	if d.AckEvidencedDrop || d.UnverifiedCompletion || !d.CutShort {
		t.Fatalf("a check whose reads the Stop cut short must be CutShort, not a verdict: %+v", d)
	}
	if !strings.Contains(d.Reason, "pipeline stopped by user") {
		t.Errorf("the reason must name why the run ended, got %q", d.Reason)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A Stop that came while the check waited for the sink to drain.
func TestAStopDuringTheLandingWaitIsNotADrop(t *testing.T) {
	cases := []struct {
		name                   string
		landed, received, acks int64
	}{
		{"no acks yet", 0, 0, 0},             // was: "destination write unconfirmed … total drop"
		{"sink still draining", 200, 200, 2}, // was: "landing unconfirmed" (receipt shortfall)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectQuery(`FROM pipeline_batch_acks`).WillReturnRows(ackRow(tc.landed, tc.received, tc.acks, nil))
			a := &Agent{db: db}
			ctx, stop := stoppedCtx()
			time.AfterFunc(50*time.Millisecond, func() { stop(runcancel.ErrStopped) })

			d, _, _ := a.checkLanding(ctx, "p1", "e1", 500, 5, 0, 5, time.Minute)
			if d.AckEvidencedDrop || d.UnverifiedCompletion || !d.CutShort {
				t.Fatalf("a Stop during the wait must leave the run cut short, not judged: %+v", d)
			}
		})
	}
}

// Control: a negative ack read before the Stop is evidence of a lost batch.
func TestANegativeAckReadBeforeAStopIsStillADrop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM pipeline_batch_acks`).WillReturnRows(ackRow(200, 250, 3, "poison batch"))
	a := &Agent{db: db}
	ctx, stop := stoppedCtx()
	time.AfterFunc(50*time.Millisecond, func() { stop(runcancel.ErrStopped) })

	d, _, _ := a.checkLanding(ctx, "p1", "e1", 500, 5, 0, 5, time.Minute)
	if !d.AckEvidencedDrop || d.CutShort || d.Status != "silent_partial_drop_detected" {
		t.Fatalf("a DLQ'd batch is lost whether or not the run was stopped: %+v", d)
	}
}

// Control: with the context alive, zero acks by the deadline still fails closed.
func TestZeroAcksByTheDeadlineStillFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM pipeline_batch_acks`).WillReturnRows(ackRow(0, 0, 0, nil))
	a := &Agent{db: db}

	d, _, _ := a.checkLanding(context.Background(), "p1", "e1", 500, 5, 0, 5, 0)
	if !d.AckEvidencedDrop || d.CutShort {
		t.Fatalf("zero acks by the deadline is a drop when nothing ended the run: %+v", d)
	}
}

// Control: a run whose rows all landed is not cut short by a late Stop.
func TestADrainedRunIsNotCutShort(t *testing.T) {
	ctx, stop := stoppedCtx()
	stop(runcancel.ErrStopped)
	if landingCheckCutShort(ctx, 500, 500, "") {
		t.Fatal("every row landed; the Stop cut nothing short")
	}
	if landingCheckCutShort(context.Background(), 0, 500, "") {
		t.Fatal("a live context never cuts the check short")
	}
}

// The blob lane waits on the same ledger and had the same hole: a Stop read as
// "ledger lag" and the run was marked unverified.
func TestABlobLandingCheckAStopCutShortIsNotUnverified(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &Agent{db: db}
	ctx, stop := stoppedCtx()
	stop(runcancel.ErrStopped)
	task := ExecutorTask{TaskID: "t1", PipelineID: "p1"}

	status, failed := a.verifyBlobLanding(ctx, task, "e1", 10, map[string]interface{}{}, time.Minute)
	if failed == nil || failed.Status != "cancelled" {
		t.Fatalf("a Stop during the blob landing check must end the run cancelled, got status %q, response %+v", status, failed)
	}
}

// Control: with the context alive, zero blob acks stays unverified (fail-soft).
func TestZeroBlobAcksByTheDeadlineStaysUnverified(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM pipeline_batch_acks`).WillReturnRows(ackRow(0, 0, 0, nil))
	a := &Agent{db: db}
	task := ExecutorTask{TaskID: "t1", PipelineID: "p1"}

	if status, failed := a.verifyBlobLanding(context.Background(), task, "e1", 10, map[string]interface{}{}, 0); failed != nil || status != "unverified_completion" {
		t.Fatalf("got status %q, response %+v", status, failed)
	}
}

// A rewind that real evidence called for must survive the run's context ending:
// under a Stop it died on "context canceled" and told ops to run a Reload.
func TestARewindCalledForByEvidenceSurvivesAStop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`^SAVEPOINT record_rewound_tables$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO pipeline_run_rewinds`).WithArgs("p1", "e1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`^RELEASE SAVEPOINT record_rewound_tables$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE pipeline_checkpoints`).WithArgs("p1", "e1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM pipeline_checkpoints`).WithArgs("p1", "e1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	ctx, stop := stoppedCtx()
	stop(runcancel.ErrStopped)

	got := rewindAfterLostBatches(ctx, db, "p1", "e1")
	if !strings.Contains(got, "1 table(s) were rewound") {
		t.Fatalf("the rewind must run under a stopped run's context, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The structured lane acts on a cut-short check before any verdict, and never
// rewinds for it.
func TestTheStructuredLaneEndsACutShortRunBeforeAnyVerdict(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	cut := strings.Index(s, "case decision.CutShort:")
	drop := strings.Index(s, "case decision.AckEvidencedDrop:")
	if cut < 0 || drop < 0 || cut > drop {
		t.Fatal("the landing switch must handle decision.CutShort before decision.AckEvidencedDrop")
	}
	if strings.Contains(s[cut:drop], "rewindAfterLostBatches(") {
		t.Error("a cut-short run lost nothing; it must not rewind its checkpoints")
	}
}
