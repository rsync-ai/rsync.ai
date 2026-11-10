package sentinel

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
)

// Pins the producer half of "once per episode".
//
// The bug class: a sentinel re-raises an open alert every tick, and the api-gateway
// dropped a repeat only for 60 minutes, so a stall that lasted a day was pushed 24
// times. The gateway now drops a repeat for the whole episode, but only when the raise
// says when its episode opened (opened_at). That is the issue row's detected_at: set
// on insert, kept by every re-raise, and gone with the row when the alert closes, so a
// relapse opens a new episode and is delivered.

type sentAlert struct {
	key          string
	notification map[string]interface{}
}

// captureAlerts replaces the producer with a recorder for the test.
func captureAlerts(t *testing.T) *[]sentAlert {
	t.Helper()
	var got []sentAlert
	orig := emitNotification
	emitNotification = func(_ *kafka.Manager, key string, _ IssueType, n map[string]interface{}) {
		got = append(got, sentAlert{key, n})
	}
	t.Cleanup(func() { emitNotification = orig })
	return &got
}

var episodeUpsert = regexp.QuoteMeta("INSERT INTO sentinel_active_issues") + `[\s\S]*RETURNING detected_at`

// expectEpisodeUpsert scripts the issue upsert, which reports when the row's episode
// opened: detected_at is NOW() on insert and kept on conflict.
func expectEpisodeUpsert(mock sqlmock.Sqlmock, detectedAt time.Time) {
	mock.ExpectQuery(episodeUpsert).
		WillReturnRows(sqlmock.NewRows([]string{"detected_at"}).AddRow(detectedAt))
}

// detectedAtRow is the upsert's answer for a test that does not look at the episode.
func detectedAtRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"detected_at"}).AddRow(time.Now().UTC())
}

func episodeSentinels(t *testing.T) (*CDCSentinel, *BatchSentinel, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &CDCSentinel{db: db}, &BatchSentinel{db: db, stallThreshold: DefaultBatchStallThreshold}, mock
}

const episodePID = "aaaaaaaa-bbbb-4ccc-8ddd-000000000001"

// uncuratedCritical is the one raise in episodeRaises with no catalog code, on purpose:
// the control for every check that the others carry one.
const uncuratedCritical = "destination_disk_full"

// episodeRaises are the alerts that re-raise every tick while their problem lasts:
// each is pushed once per episode, not once an hour.
var episodeRaises = []struct {
	name  string
	code  string // what the api-gateway dedups on
	raise func(ctx context.Context, c *CDCSentinel, b *BatchSentinel)
}{
	{"source stream stalled", "CDC_SOURCE_STREAM_STALLED", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitCDCIssue(ctx, sourceStalledIssueID(episodePID), IssueTypeSourceStreamStalled, IssueSeverityCritical,
			"source_stream_stalled", episodePID, "orders", "postgres", "d", nil)
	}},
	{"capture stopped", "CDC_CAPTURE_STOPPED", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitCDCIssue(ctx, captureStoppedIssueID(episodePID), IssueTypeCaptureStopped, IssueSeverityCritical,
			"capture_stopped", episodePID, "orders", "postgres", "d", nil)
	}},
	{"sink not draining", "CDC_SINK_NOT_DRAINING", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitCDCIssue(ctx, sinkLagIssueID(episodePID), IssueTypeHighLag, IssueSeverityCritical,
			"sink_not_draining", episodePID, "orders", "postgres", "d", nil)
	}},
	{"source lag sustained", "CDC_SOURCE_LAG_HIGH", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitCDCIssue(ctx, sourceLagIssueID(episodePID), IssueTypeHighLag, IssueSeverityCritical,
			"source_lag_sustained", episodePID, "orders", "postgres", "d", nil)
	}},
	{"sink worker absent", "SINK_WORKER_ABSENT", func(ctx context.Context, _ *CDCSentinel, b *BatchSentinel) {
		b.emitBatchIssue(ctx, "batch-sink-absent-x", IssueTypeSinkWorkerAbsent, IssueSeverityCritical,
			episodePID, "orders", "exec-1", "d", nil)
	}},
	{"sink write rejected", "SINK_WRITE_REJECTED", func(ctx context.Context, _ *CDCSentinel, b *BatchSentinel) {
		b.emitBatchIssue(ctx, "batch-write-rejected-x", IssueTypeSinkWriteRejected, IssueSeverityCritical,
			episodePID, "orders", "exec-1", "d", map[string]interface{}{"table_name": "loans"})
	}},
	{"sink absent", "CDC_SINK_ABSENT", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitLagIssue(ctx, sinkAbsentIssueID(episodePID), "sink_absent", episodePID, "orders", "postgres", "d",
			map[string]interface{}{"consumer_group": "sink-aaaaaaaa", "attempts": 3, "terminal": true})
	}},
	{"WAL pressure", "CDC_WAL_PRESSURE", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitWALAlert(ctx, walSlotRow{slotName: "rsync_slot_x", connectionID: "conn-1",
			pipelineID: sql.NullString{String: episodePID, Valid: true}, pipelineState: "running", pipelineName: "orders"},
			walAlert{retained: 5 << 30, sev: IssueSeverityWarning, disposition: "running", remedy: walRemedyAlertOnly})
	}},
	{"position retention", "CDC_POSITION_RETENTION", func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitHorizonAlert(ctx, horizonAlert{
			row: horizonRow{pipelineID: episodePID, pipelineName: "orders", pipelineState: "stopped", connectionID: "conn-1", family: "mysql"},
			sev: IssueSeverityCritical, disposition: "stopped", remedy: walRemedyAlertOnly})
	}},
	// An uncurated critical is pushed under its issue type; it re-raises like the rest.
	{"an uncurated critical", uncuratedCritical, func(ctx context.Context, c *CDCSentinel, _ *BatchSentinel) {
		c.emitCDCIssue(ctx, "cdc-disk-x", IssueType(uncuratedCritical), IssueSeverityCritical,
			"destination_disk_full", episodePID, "orders", "postgres", "d", nil)
	}},
}

// alertCode is what the api-gateway dedups the notification on: its catalog code, or
// its event type when it has none.
func alertCode(n map[string]interface{}) string {
	if c, _ := n["_code"].(string); c != "" {
		return c
	}
	s, _ := n["type"].(string)
	return s
}

func TestSentinelAlert_SaysWhenItsEpisodeOpened(t *testing.T) {
	// A TIMESTAMP column: the server's clock, read back without a zone.
	detected := time.Date(2026, 10, 24, 6, 30, 0, 0, time.UTC)
	for _, tc := range episodeRaises {
		t.Run(tc.name, func(t *testing.T) {
			c, b, mock := episodeSentinels(t)
			sent := captureAlerts(t)
			expectEpisodeUpsert(mock, detected)

			tc.raise(context.Background(), c, b)

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
			if len(*sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(*sent))
			}
			n := (*sent)[0].notification
			if got := alertCode(n); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
			if got, want := n["opened_at"], detected.Format(time.RFC3339); got != want {
				t.Errorf("opened_at = %v, want %v (the row's detected_at)", got, want)
			}
		})
	}
}

// Control: when the row could not be written, nothing says when the episode opened,
// and the raise keeps the 60-minute repeat rather than claiming an episode.
func TestSentinelAlert_AnUnwrittenRowSaysNothingAboutItsEpisode(t *testing.T) {
	for _, tc := range episodeRaises {
		t.Run(tc.name, func(t *testing.T) {
			c, b, mock := episodeSentinels(t)
			sent := captureAlerts(t)
			mock.ExpectQuery(episodeUpsert).WillReturnError(errors.New("connection reset"))

			tc.raise(context.Background(), c, b)

			if len(*sent) != 1 {
				t.Fatalf("sent %d notifications, want 1: a failed write must not stop the alert", len(*sent))
			}
			if v, ok := (*sent)[0].notification["opened_at"]; ok {
				t.Errorf("opened_at = %v, want none", v)
			}
		})
	}
}

// These re-raise on purpose, and keep the 60-minute repeat (episodeExempt says why).
func TestSentinelAlert_ARepeatThatMeansSomethingSaysNothingAboutItsEpisode(t *testing.T) {
	for _, tc := range []struct {
		code  string
		raise func(ctx context.Context, c *CDCSentinel)
	}{
		{"CDC_ROWS_PARKED", func(ctx context.Context, c *CDCSentinel) {
			c.emitCDCIssue(ctx, rowsParkedIssueID(episodePID), IssueTypeRowsParked, IssueSeverityCritical,
				"cdc_rows_parked", episodePID, "orders", "postgres", "d", nil)
		}},
		{"CDC_CONNECTOR_DOWN", func(ctx context.Context, c *CDCSentinel) {
			c.emitCDCIssue(ctx, connectorIssueID(episodePID), IssueTypeConnectorDown, IssueSeverityCritical,
				"connector_down", episodePID, "orders", "postgres", "d", nil)
		}},
		{"CDC_SINK_WEDGED", func(ctx context.Context, c *CDCSentinel) {
			c.emitCDCIssue(ctx, sinkWedgedEscalationIssueID(episodePID), IssueTypeHighLag, IssueSeverityWarning,
				"sink_wedged_escalation", episodePID, "orders", "postgres", "d", nil)
		}},
	} {
		t.Run(tc.code, func(t *testing.T) {
			c, _, mock := episodeSentinels(t)
			sent := captureAlerts(t)
			expectEpisodeUpsert(mock, time.Date(2026, 10, 24, 6, 30, 0, 0, time.UTC))

			tc.raise(context.Background(), c)

			if len(*sent) != 1 || alertCode((*sent)[0].notification) != tc.code {
				t.Fatalf("sent %+v, want one %s", *sent, tc.code)
			}
			if v, ok := (*sent)[0].notification["opened_at"]; ok {
				t.Errorf("opened_at = %v, want none: each repeat of %s is news", v, tc.code)
			}
		})
	}
}

// An instance alert has no issue row of its own to date it, so it keeps the window.
func TestInstanceAlert_SaysNothingAboutAnEpisode(t *testing.T) {
	sent := captureAlerts(t)
	publishInstanceAlert(nil, &Issue{Type: IssueTypeInfrastructureDown, Severity: IssueSeverityCritical, ComponentID: "postgres"})
	if len(*sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(*sent))
	}
	if v, ok := (*sent)[0].notification["opened_at"]; ok {
		t.Errorf("opened_at = %v, want none", v)
	}
}

// Every exempt alert is one the catalog raises, with a reason, and every other
// curated alert says when its episode opened: a code curated later is pushed once
// per episode unless somebody writes down why each of its repeats is news.
func TestEpisodeExemptAlertsAreCuratedAndSayWhy(t *testing.T) {
	sent := captureAlerts(t)
	opened := time.Date(2026, 10, 24, 6, 30, 0, 0, time.UTC)
	curated := map[string]bool{}
	check := func(issueType IssueType, alertType, want string) {
		t.Helper()
		*sent = (*sent)[:0]
		publishSentinelAlert(nil, issueType, IssueSeverityCritical, alertType, episodePID, "d", nil, opened)
		if len(*sent) != 1 || alertCode((*sent)[0].notification) != want {
			t.Fatalf("%s/%s: sent %+v, want one %s", issueType, alertType, *sent, want)
		}
		curated[want] = true
		_, stamped := (*sent)[0].notification["opened_at"]
		if _, exempt := episodeExempt[want]; stamped == exempt {
			t.Errorf("%s: opened_at sent = %v, exempt = %v: exactly one of them must hold", want, stamped, exempt)
		}
	}
	for issueType, a := range notifiableIssueTypes {
		check(issueType, "", a.code)
	}
	for alertType, a := range notifiableAlertTypes {
		check(IssueTypeHighLag, alertType, a.code)
	}

	for code, why := range episodeExempt {
		if !curated[code] {
			t.Errorf("episodeExempt[%q]: no curated alert raises it", code)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("episodeExempt[%q] does not say why each repeat is news", code)
		}
	}
}
