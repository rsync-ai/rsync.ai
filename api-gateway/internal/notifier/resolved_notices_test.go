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
// "No longer detected" notices (decision D2): delivered only for one of the six
// alerts that clear by themselves, only when that alert was pushed and stayed
// open for five minutes, on the same channels and mutes as the alert, and never
// in the way of a relapse.
// ─────────────────────────────────────────────────────────────────────────────

var resolvedNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

const resolvedGateQuery = `SELECT MIN\(created_at\), NOW\(\) FROM pipeline_notifications\s+WHERE dedup_key = \$1`

func resolvedNotice(code, pipelineID, actionURL, subject, openedAt string) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"type":        "alert_resolved",
		"pipeline_id": pipelineID,
		"message":     "The capture connector is running again.",
		"action_url":  actionURL,
		"severity":    "info",
		"opened_at":   openedAt,
		"error": map[string]string{
			"failure_type":  "system_error",
			"code":          code,
			"severity":      "info",
			"audience":      "user",
			"user_message":  "The capture connector is running again.",
			"dedup_subject": subject,
		},
	})
	return b
}

func pipelineURL() string { return "/pipelines/" + testPipeline }

// timeArg matches a timestamp argument exactly.
type timeArg struct{ want time.Time }

func (a timeArg) Match(v driver.Value) bool {
	t, ok := v.(time.Time)
	return ok && t.Equal(a.want)
}

func expectPipelineOwner(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM pipelines WHERE id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"created_by", "name"}).AddRow(testAdminA, "orders-sync"))
}

func gateRows(firstRaise interface{}) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"min", "now"}).AddRow(firstRaise, resolvedNow)
}

// expectDelivered is the rest of handleMessage for one recipient: insert, then the
// channel settings, then the delivery status.
func expectDelivered(mock sqlmock.Sqlmock, dedupKey string, meta *map[string]interface{}) {
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO pipeline_notifications`).
		WithArgs(sqlmock.AnyArg(), testPipeline, testAdminA, "alert_resolved", "info",
			"Change capture has resumed on orders-sync", sqlmock.AnyArg(), pipelineURL(),
			jsonCapture{into: meta}, dedupKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
	mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))
}

func newResolvedMock(t *testing.T) (*Notifier, sqlmock.Sqlmock) {
	t.Helper()
	clearNotifierEnv(t)
	InvalidateChannelCache()
	t.Cleanup(InvalidateChannelCache)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mockDB.Close() })
	return NewEmitter(mockDB), mock
}

func connectorKeys() (raiseKey, resolvedKey string) {
	return makeDedupKey(testPipeline, "CDC_CONNECTOR_DOWN", pipelineURL(), ""),
		makeDedupKey(testPipeline, CodeConnectorDownResolved, pipelineURL(), "")
}

func TestResolvedNotice_ForAnAlertNobodyWasToldAboutIsDropped(t *testing.T) {
	n, mock := newResolvedMock(t)
	raiseKey, resolvedKey := connectorKeys()
	expectPipelineOwner(mock)
	// No opened_at: any alert row since the last resolved notice counts.
	mock.ExpectQuery(resolvedGateQuery).WithArgs(raiseKey, resolvedKey, nil).WillReturnRows(gateRows(nil))

	raw := resolvedNotice(CodeConnectorDownResolved, testPipeline, pipelineURL(), "", "")
	if err := n.handleMessage(context.Background(), n.topics.notify, raw); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestResolvedNotice_ForAnAlertOpenUnderFiveMinutesIsDropped(t *testing.T) {
	n, mock := newResolvedMock(t)
	raiseKey, resolvedKey := connectorKeys()
	expectPipelineOwner(mock)
	mock.ExpectQuery(resolvedGateQuery).WithArgs(raiseKey, resolvedKey, nil).
		WillReturnRows(gateRows(resolvedNow.Add(-resolvedMinOpen + time.Second)))

	raw := resolvedNotice(CodeConnectorDownResolved, testPipeline, pipelineURL(), "", "")
	if err := n.handleMessage(context.Background(), n.topics.notify, raw); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestResolvedNotice_ForAnAlertOpenFiveMinutesIsDeliveredLikeTheAlert(t *testing.T) {
	n, mock := newResolvedMock(t)
	raiseKey, resolvedKey := connectorKeys()
	expectPipelineOwner(mock)
	mock.ExpectQuery(resolvedGateQuery).WithArgs(raiseKey, resolvedKey, nil).
		WillReturnRows(gateRows(resolvedNow.Add(-resolvedMinOpen)))
	var meta map[string]interface{}
	expectDelivered(mock, resolvedKey, &meta)

	raw := resolvedNotice(CodeConnectorDownResolved, testPipeline, pipelineURL(), "", "")
	if err := n.handleMessage(context.Background(), n.topics.notify, raw); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	// The alert's category, so the alert's mutes (planDelivery) apply to it too.
	if meta["category"] != CategoryHealth || meta["impact"] != resolvedImpact || meta["error_code"] != CodeConnectorDownResolved {
		t.Errorf("metadata = %v", meta)
	}
}

// Five minutes count from when this episode opened, not from an older alert row
// the episode's alert was deduped against, and a row from before the dedup window
// that precedes the episode is never paired with it.
func TestResolvedNotice_CountsFromWhenTheEpisodeOpened(t *testing.T) {
	for _, tc := range []struct {
		name      string
		openedAgo time.Duration
		delivered bool
	}{
		{"opened two minutes ago", 2 * time.Minute, false},
		{"opened ten minutes ago", 10 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, mock := newResolvedMock(t)
			raiseKey, resolvedKey := connectorKeys()
			opened := resolvedNow.Add(-tc.openedAgo)
			since := opened.Add(-time.Duration(dedupWindowMinutes)*time.Minute - resolvedClockSlack)
			expectPipelineOwner(mock)
			// The alert row is 50 minutes old: the episode's alert was deduped against it.
			mock.ExpectQuery(resolvedGateQuery).WithArgs(raiseKey, resolvedKey, timeArg{since}).
				WillReturnRows(gateRows(resolvedNow.Add(-50 * time.Minute)))
			var meta map[string]interface{}
			if tc.delivered {
				expectDelivered(mock, resolvedKey, &meta)
			}

			raw := resolvedNotice(CodeConnectorDownResolved, testPipeline, pipelineURL(), "", opened.Format(time.RFC3339))
			if err := n.handleMessage(context.Background(), n.topics.notify, raw); err != nil {
				t.Fatalf("handleMessage: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

// An instance-wide resolved notice reaches every admin, paired with the instance
// alert for the same service (scope "", the same action_url and subject).
func TestResolvedNotice_ForAnInstanceAlertFansOutToEveryAdmin(t *testing.T) {
	n, mock := newResolvedMock(t)
	mock.ExpectQuery(`FROM users\s+WHERE role = 'admin'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(testAdminA).AddRow(testAdminB))
	mock.ExpectQuery(resolvedGateQuery).
		WithArgs(makeDedupKey("", "INFRASTRUCTURE_DOWN", "/admin/health", "kafka"),
			makeDedupKey("", CodeInfrastructureDownResolved, "/admin/health", "kafka"), nil).
		WillReturnRows(gateRows(resolvedNow.Add(-10 * time.Minute)))
	mock.ExpectBegin()
	for _, admin := range []string{testAdminA, testAdminB} {
		mock.ExpectExec(`INSERT INTO pipeline_notifications`).
			WithArgs(sqlmock.AnyArg(), nil, admin, sqlmock.AnyArg(), "info", "A core service is responding again",
				sqlmock.AnyArg(), "/admin/health", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()
	mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
	mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))

	raw := resolvedNotice(CodeInfrastructureDownResolved, "instance", "/admin/health", "kafka", "")
	if err := n.handleMessage(context.Background(), n.topics.notify, raw); err != nil {
		t.Fatalf("handleMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// An alert that has a resolved notice dedups only back to its last one, so a
// relapse right after "no longer detected" is delivered. Every other alert keeps
// the plain window (one argument).
func TestAlertDedup_LooksBackOnlyToTheLastResolvedNotice(t *testing.T) {
	sentinelAlert := func(code string) []byte {
		b, _ := json.Marshal(map[string]interface{}{
			"type": "system_error", "pipeline_id": testPipeline, "action_url": pipelineURL(),
			"message": "x", "error": map[string]string{"code": code, "severity": "error", "user_message": "x"},
		})
		return b
	}

	for _, tc := range []struct {
		name string
		raw  []byte
		args []driver.Value
	}{
		{"coded alert with a resolved notice", sentinelAlert("CDC_CONNECTOR_DOWN"), []driver.Value{
			makeDedupKey(testPipeline, "CDC_CONNECTOR_DOWN", pipelineURL(), ""),
			makeDedupKey(testPipeline, CodeConnectorDownResolved, pipelineURL(), ""),
		}},
		// The WAL alert pairs by its code, not by its event type.
		{"WAL alert", walRaise(t, "warning", ""), []driver.Value{
			makeDedupKey(testPipeline, "CDC_WAL_PRESSURE", pipelineURL(), ""),
			makeDedupKey(testPipeline, CodeWALPressureResolved, pipelineURL(), ""),
		}},
		{"alert with no resolved notice", sentinelAlert("CDC_SOURCE_LAG_HIGH"), []driver.Value{
			makeDedupKey(testPipeline, "CDC_SOURCE_LAG_HIGH", pipelineURL(), ""),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, mock := newResolvedMock(t)
			expectPipelineOwner(mock)
			q := `FROM pipeline_notifications\s+WHERE dedup_key = \$1 AND created_at > NOW\(\) - INTERVAL '60 minutes'`
			if len(tc.args) == 2 {
				q += `\s+AND created_at > COALESCE\(\(SELECT MAX\(created_at\) FROM pipeline_notifications WHERE dedup_key = \$2\)`
			}
			// A hit ends handleMessage here, so nothing else is expected.
			mock.ExpectQuery(q).WithArgs(tc.args...).
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

// D2 is an allowlist: exactly these six alerts get a resolved notice.
func TestResolvedNotices_AreTheD2AllowlistOnly(t *testing.T) {
	want := map[string]string{
		"CDC_SOURCE_STREAM_STALLED_RESOLVED": "CDC_SOURCE_STREAM_STALLED",
		"CDC_CAPTURE_STOPPED_RESOLVED":       "CDC_CAPTURE_STOPPED",
		"CDC_CONNECTOR_DOWN_RESOLVED":        "CDC_CONNECTOR_DOWN",
		"CDC_SINK_NOT_DRAINING_RESOLVED":     "CDC_SINK_NOT_DRAINING",
		"CDC_WAL_PRESSURE_RESOLVED":          "CDC_WAL_PRESSURE",
		"INFRASTRUCTURE_DOWN_RESOLVED":       "INFRASTRUCTURE_DOWN",
	}
	if len(resolvedPairs) != len(want) {
		t.Fatalf("resolvedPairs has %d entries, want %d: %v", len(resolvedPairs), len(want), resolvedPairs)
	}
	for resolved, raise := range want {
		if resolvedPairs[resolved] != raise {
			t.Errorf("resolvedPairs[%s] = %q, want %q", resolved, resolvedPairs[resolved], raise)
		}
		if resolvedCodeFor[raise] != resolved {
			t.Errorf("resolvedCodeFor[%s] = %q, want %q", raise, resolvedCodeFor[raise], resolved)
		}
	}
}

// A resolved notice is info, carries the D6 line, links where its alert linked,
// and sits in its alert's category so the alert's mutes silence it too.
func TestEveryResolvedCodeSharesItsAlertsCategoryAndLink(t *testing.T) {
	for resolved, raise := range resolvedPairs {
		entry, ok := catalog[resolved]
		if !ok {
			t.Errorf("%s has no catalog entry", resolved)
			continue
		}
		if entry.Severity != severityInfo || entry.Impact != resolvedImpact || entry.Title == "" {
			t.Errorf("%s entry = %+v; want info severity, the resolved impact and a title", resolved, entry)
		}
		raiseLabel := defaultActionLabel
		if e, ok := catalog[raise]; ok {
			raiseLabel = e.ActionLabel
		}
		if entry.ActionLabel != raiseLabel {
			t.Errorf("%s action label = %q, its alert's is %q", resolved, entry.ActionLabel, raiseLabel)
		}
		// The raise identity is a code, or (WAL) an event type: CategoryFor tries both.
		if got, want := CategoryFor(resolved, ""), CategoryFor(raise, raise); got != want || got == CategoryOther {
			t.Errorf("%s category = %q, its alert's is %q", resolved, got, want)
		}
	}
}

func TestParseOpenedAt(t *testing.T) {
	for in, ok := range map[string]bool{
		"2026-10-16T11:55:00Z":           true,
		"2026-10-16T11:55:00.123456789Z": true,
		"2026-10-16T13:55:00+02:00":      true,
		"":                               false,
		"yesterday":                      false,
	} {
		if _, got := parseOpenedAt(in); got != ok {
			t.Errorf("parseOpenedAt(%q) ok = %v, want %v", in, got, ok)
		}
	}
}
