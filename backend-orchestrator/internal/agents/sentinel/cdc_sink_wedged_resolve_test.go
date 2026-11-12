package sentinel

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The sink-wedged escalation is raised once the auto-restart rung runs out of restarts, and
// nothing closed it: the row stayed open after the sink drained again, whoever fixed it. It
// now closes once the sink has applied a change after the alert was raised.

const wedgedTestPID = "77777777-8888-4999-8aaa-bbbbbbbbbbbb"

func newWedgedMock(t *testing.T) (*CDCSentinel, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &CDCSentinel{db: db, sinkRestartState: map[string]*connRestartState{}}, mock
}

func expectWedgedClose(mock sqlmock.Sqlmock) *sqlmock.ExpectedExec {
	return mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sentinel_active_issues si")).
		WithArgs(sinkWedgedEscalationIssueID(wedgedTestPID), wedgedTestPID)
}

// TestResolveSinkWedged_AnApplyAfterTheAlertClosesItAndReArmsTheEscalation: the row is gone,
// and the next wedge in the same window escalates again instead of staying silent. The
// attempts are kept, so the rung does not restart the sink more often than its cap allows.
func TestResolveSinkWedged_AnApplyAfterTheAlertClosesItAndReArmsTheEscalation(t *testing.T) {
	s, mock := newWedgedMock(t)
	first := time.Now().Add(-time.Hour)
	s.sinkRestartState[wedgedTestPID] = &connRestartState{attempts: 3, firstAttempt: first, escalated: true}
	expectWedgedClose(mock).WillReturnResult(sqlmock.NewResult(0, 1))

	s.resolveSinkWedged(context.Background(), wedgedTestPID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	st := s.sinkRestartState[wedgedTestPID]
	if st.escalated {
		t.Fatal("the alert closed but the escalation stayed spent: a second wedge in this window would raise nothing")
	}
	if st.attempts != 3 || !st.firstAttempt.Equal(first) {
		t.Fatalf("closing the alert changed the restart budget: attempts=%d firstAttempt=%v", st.attempts, st.firstAttempt)
	}

	// The second wedge, a minute later, inside the same window.
	decision, _ := decideSinkRestart(sinkRestartInputs{
		enabled: true, lagging: true, sinkStale: true, progressFlat: true,
		now: time.Now(), st: *st, maxAttempts: 3, window: 24 * time.Hour, cooldown: 5 * time.Minute,
	})
	if decision != sinkRestartEscalate {
		t.Fatalf("a second wedge after the alert closed decided %v, want an escalation", decision)
	}
}

// TestResolveSinkWedged_NoApplySinceTheAlertKeepsIt: zero rows deleted means the sink has
// applied nothing since the alert, so the escalation stays spent and nothing re-raises.
func TestResolveSinkWedged_NoApplySinceTheAlertKeepsIt(t *testing.T) {
	s, mock := newWedgedMock(t)
	s.sinkRestartState[wedgedTestPID] = &connRestartState{attempts: 3, escalated: true}
	expectWedgedClose(mock).WillReturnResult(sqlmock.NewResult(0, 0))

	s.resolveSinkWedged(context.Background(), wedgedTestPID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if !s.sinkRestartState[wedgedTestPID].escalated {
		t.Fatal("nothing was closed, yet the escalation was re-armed: the next tick would raise the same alert again")
	}
}

// TestResolveSinkWedged_AFailedDeleteChangesNothing: the next tick tries again.
func TestResolveSinkWedged_AFailedDeleteChangesNothing(t *testing.T) {
	s, mock := newWedgedMock(t)
	s.sinkRestartState[wedgedTestPID] = &connRestartState{attempts: 3, escalated: true}
	expectWedgedClose(mock).WillReturnError(errors.New("connection reset"))

	s.resolveSinkWedged(context.Background(), wedgedTestPID)

	if !s.sinkRestartState[wedgedTestPID].escalated {
		t.Fatal("a failed DELETE re-armed the escalation")
	}
}

// TestResolveSinkWedged_ProofIsAnApplyAfterTheAlert: the evidence is the sink's own apply
// time for this pipeline's CDC rows, later than the alert's last occurrence. An apply from
// before the alert, or a batch run's row, proves nothing about the stream.
func TestResolveSinkWedged_ProofIsAnApplyAfterTheAlert(t *testing.T) {
	for _, want := range []string{
		`si\.id = \$1`,
		`t\.pipeline_id = \$2`,
		`t\.mode = 'cdc'`,
		`t\.last_applied_ts > si\.last_occurrence`,
	} {
		if !regexp.MustCompile(want).MatchString(resolveSinkWedgedSQL) {
			t.Errorf("resolveSinkWedgedSQL lacks %s", want)
		}
	}
}

// TestCheckSinkConsumerLag_TriesToCloseTheWedgedAlert: the close runs on every sink check,
// above the Kafka guard (it reads only the control DB), and whether or not the auto-restart
// rung is armed: switching the flag off after an escalation must not strand the row.
func TestCheckSinkConsumerLag_TriesToCloseTheWedgedAlert(t *testing.T) {
	t.Setenv("CDC_SINK_AUTORESTART_ENABLED", "false")
	s, mock := newWedgedMock(t)
	mock.ExpectQuery(`FROM pipeline_run_table_stats`).WithArgs(wedgedTestPID).
		WillReturnError(errors.New("parked rows unreadable in this test"))
	expectWedgedClose(mock).WillReturnResult(sqlmock.NewResult(0, 0))

	s.checkSinkConsumerLag(context.Background(), wedgedTestPID, "wedged-cdc", "postgresql")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the sink check never tried to close the wedged alert: %v", err)
	}
}
