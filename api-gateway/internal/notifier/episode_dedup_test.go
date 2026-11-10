package notifier

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ─────────────────────────────────────────────────────────────────────────────
// Once per episode. A sentinel raise that says when its problem opened
// (opened_at) is a repeat of any alert stored since then, not only of one
// stored in the last 60 minutes. Without it, a stall that lasted a day was
// pushed 24 times. A raise counts an earlier row of the episode only at the
// same or a higher severity, so a WAL warning that turns critical is still
// delivered.
// ─────────────────────────────────────────────────────────────────────────────

const episodeDedupQuery = `FROM pipeline_notifications\s+WHERE dedup_key = \$1 AND \(created_at > NOW\(\) - INTERVAL '60 minutes'` +
	`\s+OR \(created_at > \$(\d)::timestamptz AND ` + `CASE metadata->>'raw_severity' WHEN 'critical' THEN 2 WHEN 'warning' THEN 1 ELSE 0 END >= \$(\d)\)\)`

func episodeRaise(t *testing.T, fields map[string]interface{}) []byte {
	t.Helper()
	body := map[string]interface{}{"pipeline_id": testPipeline, "action_url": pipelineURL(), "message": "x"}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sentinelRaise(t *testing.T, code, openedAt string) []byte {
	return episodeRaise(t, map[string]interface{}{
		"type": "source_stream_stalled", "severity": "critical", "opened_at": openedAt,
		"error": map[string]string{"code": code, "severity": "error", "audience": "user", "user_message": "x"},
	})
}

// walRaise is the WAL watchdog's alert as cdc_wal_watchdog.go emitWALAlert sends it: its
// level at the top (the episode ranks rows by it) and in its structured error, where a
// critical is "error".
func walRaise(t *testing.T, severity, openedAt string) []byte {
	envelope := severity
	if severity == "critical" {
		envelope = "error"
	}
	return episodeRaise(t, map[string]interface{}{
		"type": "cdc_wal_pressure", "severity": severity, "opened_at": openedAt,
		"error": map[string]string{"code": "CDC_WAL_PRESSURE", "severity": envelope, "audience": "user", "user_message": "x"},
	})
}

// positionRaise is the position-horizon alert as the sentinel now sends it.
func positionRaise(t *testing.T, severity, openedAt string) []byte {
	envelope := severity
	if severity == "critical" {
		envelope = "error"
	}
	return episodeRaise(t, map[string]interface{}{
		"type": "cdc_position_retention", "severity": severity, "opened_at": openedAt,
		"error": map[string]string{"code": "CDC_POSITION_RETENTION", "severity": envelope, "audience": "user", "user_message": "x"},
	})
}

func TestAlertDedup_AnEpisodeLooksBackToWhenItOpened(t *testing.T) {
	opened := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	openedAt := opened.Format(time.RFC3339)
	since := timeArg{opened.Add(-resolvedClockSlack)}

	for _, tc := range []struct {
		name string
		raw  []byte
		args []driver.Value
	}{
		{"stall, which has a resolved notice", sentinelRaise(t, "CDC_SOURCE_STREAM_STALLED", openedAt), []driver.Value{
			makeDedupKey(testPipeline, "CDC_SOURCE_STREAM_STALLED", pipelineURL(), ""),
			makeDedupKey(testPipeline, "CDC_SOURCE_STREAM_STALLED_RESOLVED", pipelineURL(), ""),
			since, 2,
		}},
		{"source lag, which has none", sentinelRaise(t, "CDC_SOURCE_LAG_HIGH", openedAt), []driver.Value{
			makeDedupKey(testPipeline, "CDC_SOURCE_LAG_HIGH", pipelineURL(), ""),
			since, 2,
		}},
		// A warning ranks below a critical: an episode's critical row repeats it, and
		// its own warning row does not repeat a critical.
		{"WAL pressure at warning", walRaise(t, "warning", openedAt), []driver.Value{
			makeDedupKey(testPipeline, "CDC_WAL_PRESSURE", pipelineURL(), ""),
			makeDedupKey(testPipeline, CodeWALPressureResolved, pipelineURL(), ""),
			since, 1,
		}},
		{"position retention at critical", episodeRaise(t, map[string]interface{}{
			"type": "cdc_position_retention", "severity": "critical", "opened_at": openedAt,
		}), []driver.Value{
			makeDedupKey(testPipeline, "cdc_position_retention", pipelineURL(), ""),
			since, 2,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, mock := newResolvedMock(t)
			expectPipelineOwner(mock)
			// A hit ends handleMessage here, so nothing else is expected.
			mock.ExpectQuery(episodeDedupQuery).WithArgs(tc.args...).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("11111111-0000-0000-0000-000000000000"))
			if err := n.handleMessage(context.Background(), n.topics.notify, tc.raw); err != nil {
				t.Fatalf("handleMessage: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

// Control: an opened_at that does not parse is no episode, and the raise keeps
// the plain window (TestAlertDedup_LooksBackOnlyToTheLastResolvedNotice pins the
// raise that sends none).
func TestAlertDedup_AnUnreadableOpenedAtKeepsTheWindow(t *testing.T) {
	n, mock := newResolvedMock(t)
	expectPipelineOwner(mock)
	mock.ExpectQuery(`FROM pipeline_notifications\s+WHERE dedup_key = \$1 AND created_at > NOW\(\) - INTERVAL '60 minutes'\s+ORDER BY`).
		WithArgs(makeDedupKey(testPipeline, "CDC_SOURCE_LAG_HIGH", pipelineURL(), "")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("11111111-0000-0000-0000-000000000000"))
	if err := n.handleMessage(context.Background(), n.topics.notify, sentinelRaise(t, "CDC_SOURCE_LAG_HIGH", "yesterday")); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// The severity the producer sent is what the next raise of the episode ranks this
// row by, so it is stored with the row.
func TestAlertDedup_StoresTheSeverityTheProducerSent(t *testing.T) {
	n, mock := newResolvedMock(t)
	expectPipelineOwner(mock)
	key := makeDedupKey(testPipeline, "CDC_WAL_PRESSURE", pipelineURL(), "")
	mock.ExpectQuery(episodeDedupQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	var meta map[string]interface{}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO pipeline_notifications`).
		WithArgs(sqlmock.AnyArg(), testPipeline, testAdminA, "cdc_wal_pressure", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), pipelineURL(), jsonCapture{into: &meta}, key).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
	mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))

	raw := episodeRaise(t, map[string]interface{}{
		"type": "cdc_wal_pressure", "severity": "Critical", "opened_at": "2026-10-24T08:00:00Z",
		"error": map[string]string{"code": "CDC_WAL_PRESSURE", "severity": "error", "audience": "user", "user_message": "x"},
	})
	if err := n.handleMessage(context.Background(), n.topics.notify, raw); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if got := meta["raw_severity"]; got != "critical" {
		t.Errorf("metadata raw_severity = %v, want critical", got)
	}
}

func TestSeverityRank_OrdersWarningBelowCritical(t *testing.T) {
	for sev, want := range map[string]int{"critical": 2, " CRITICAL ": 2, "warning": 1, "info": 0, "": 0, "error": 0} {
		if got := severityRank(sev); got != want {
			t.Errorf("severityRank(%q) = %d, want %d", sev, got, want)
		}
	}
}

// strArg captures a string argument.
type strArg struct{ into *string }

func (a strArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	*a.into = s
	return ok
}

// The WAL alert is stored at the level the watchdog raised it, under its own headline.
// It carried no code, so the bell humanized its event type into the headline ("Cdc Wal
// Pressure") and gave it the type's default severity, info, which is below the delivery
// threshold for email and Slack: a slot about to fill the source's disk read as a notice
// (KI-CDC-WAL-PRESSURE-NOTICE-RENDERS-AS-INFO).
func TestWALPressure_IsStoredAtItsLevelUnderItsOwnHeadline(t *testing.T) {
	for _, level := range []string{"warning", "critical"} {
		t.Run(level, func(t *testing.T) {
			n, mock := newResolvedMock(t)
			expectPipelineOwner(mock)
			mock.ExpectQuery(episodeDedupQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			var severity, title string
			var meta map[string]interface{}
			mock.ExpectBegin()
			mock.ExpectExec(`INSERT INTO pipeline_notifications`).
				WithArgs(sqlmock.AnyArg(), testPipeline, testAdminA, "cdc_wal_pressure", strArg{&severity}, strArg{&title},
					sqlmock.AnyArg(), pipelineURL(), jsonCapture{into: &meta},
					makeDedupKey(testPipeline, "CDC_WAL_PRESSURE", pipelineURL(), "")).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
			mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))

			if err := n.handleMessage(context.Background(), n.topics.notify, walRaise(t, level, "2026-10-24T08:00:00Z")); err != nil {
				t.Fatalf("handleMessage: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
			if severity != level {
				t.Errorf("stored severity = %q, want %q", severity, level)
			}
			if want := "The change log kept for orders-sync is growing on your source database"; title != want {
				t.Errorf("stored title = %q, want %q", title, want)
			}
			if got := meta["raw_severity"]; got != level {
				t.Errorf("metadata raw_severity = %v, want %s", got, level)
			}
		})
	}
}

// The position-horizon alert is the WAL alert's twin for MySQL/MariaDB and SQL Server and
// had the same fault: no code, so a humanized type for a headline ("Cdc Position
// Retention") and the info default, below the delivery threshold for email and Slack
// (KI-CDC-POSITION-RETENTION-NOTICE-RENDERS-AS-INFO). It is filed under data loss either
// way, through its code now and its type before.
func TestPositionRetention_IsStoredAtItsLevelUnderItsOwnHeadline(t *testing.T) {
	for _, level := range []string{"warning", "critical"} {
		t.Run(level, func(t *testing.T) {
			n, mock := newResolvedMock(t)
			expectPipelineOwner(mock)
			mock.ExpectQuery(episodeDedupQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			var severity, title string
			var meta map[string]interface{}
			mock.ExpectBegin()
			mock.ExpectExec(`INSERT INTO pipeline_notifications`).
				WithArgs(sqlmock.AnyArg(), testPipeline, testAdminA, "cdc_position_retention", strArg{&severity}, strArg{&title},
					sqlmock.AnyArg(), pipelineURL(), jsonCapture{into: &meta},
					makeDedupKey(testPipeline, "CDC_POSITION_RETENTION", pipelineURL(), "")).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
			mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))

			if err := n.handleMessage(context.Background(), n.topics.notify, positionRaise(t, level, "2026-10-24T08:00:00Z")); err != nil {
				t.Fatalf("handleMessage: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
			if severity != level {
				t.Errorf("stored severity = %q, want %q", severity, level)
			}
			if want := "orders-sync may not be able to resume from where it stopped"; title != want {
				t.Errorf("stored title = %q, want %q", title, want)
			}
			if got := meta["raw_severity"]; got != level {
				t.Errorf("metadata raw_severity = %v, want %s", got, level)
			}
			if got := CategoryFor("CDC_POSITION_RETENTION", "cdc_position_retention"); got != CategoryDataLoss {
				t.Errorf("category = %q, want %q", got, CategoryDataLoss)
			}
		})
	}
}
