package sentinel

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
)

// Pins the producer half of decision D2 ("no longer detected" notices).
//
// The bug class: an alert the owner was paged for closes on its own and nobody is told,
// so the owner keeps watching a problem that is gone. The other half of the class is a
// notice that cannot be paired with its alert (a different pipeline, link or subject),
// which the api-gateway then drops as "never pushed", and one sent for an alert nobody
// was paged for, which is noise. Whether a notice is delivered is the gateway's call
// (api-gateway/internal/notifier/resolved.go); what is decided here is WHEN one is
// produced, and that it can be paired.

const resolvedTestPID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

type sentResolved struct {
	key, pipelineID, actionURL, subject, message string
	kind                                         resolvedKind
	openedAt                                     time.Time
}

// captureResolved replaces the producer with a recorder for the test.
func captureResolved(t *testing.T) *[]sentResolved {
	t.Helper()
	var got []sentResolved
	orig := publishResolved
	publishResolved = func(_ *kafka.Manager, key string, k resolvedKind, pipelineID, actionURL, subject, message string, openedAt time.Time) {
		got = append(got, sentResolved{key, pipelineID, actionURL, subject, message, k, openedAt})
	}
	t.Cleanup(func() { publishResolved = orig })
	return &got
}

func resolvedSentinel(t *testing.T) (*CDCSentinel, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &CDCSentinel{db: db}, mock
}

var deleteReturning = regexp.QuoteMeta("DELETE FROM sentinel_active_issues WHERE id = $1 RETURNING severity, detected_at")

func expectLagDelete(mock sqlmock.Sqlmock, id string, rows *sqlmock.Rows) {
	mock.ExpectQuery(deleteReturning).WithArgs(id).WillReturnRows(rows)
}

func lagRow(severity string, detected time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"severity", "detected_at"}).AddRow(severity, detected)
}

// Exactly the four alerts the sentinel pushes and later clears by id are announced.
// The source-lag, absent-sink and wedged-sink alerts sit beside them and are not.
func TestResolvedKindForIssue_IsTheD2AllowlistByExactId(t *testing.T) {
	for _, tc := range []struct {
		name, id, pid string
		wantCode      string // "" = not announced
	}{
		{"source stalled", sourceStalledIssueID(resolvedTestPID), resolvedTestPID, "CDC_SOURCE_STREAM_STALLED_RESOLVED"},
		{"capture stopped", captureStoppedIssueID(resolvedTestPID), resolvedTestPID, "CDC_CAPTURE_STOPPED_RESOLVED"},
		{"connector down", connectorIssueID(resolvedTestPID), resolvedTestPID, "CDC_CONNECTOR_DOWN_RESOLVED"},
		{"sink not draining", sinkLagIssueID(resolvedTestPID), resolvedTestPID, "CDC_SINK_NOT_DRAINING_RESOLVED"},

		{"source lag is not in D2", "cdc-lag-" + resolvedTestPID, resolvedTestPID, ""},
		{"absent sink is not in D2", sinkAbsentIssueID(resolvedTestPID), resolvedTestPID, ""},
		{"wedged sink is not in D2", sinkWedgedEscalationIssueID(resolvedTestPID), resolvedTestPID, ""},
		{"parked rows are not in D2", rowsParkedIssueID(resolvedTestPID), resolvedTestPID, ""},
		{"another pipeline's id", sinkLagIssueID("other"), resolvedTestPID, ""},
		{"no pipeline to tell", sinkLagIssueID(""), "", ""},
		{"blank pipeline", sinkLagIssueID("  "), "  ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, ok := resolvedKindForIssue(tc.id, tc.pid)
			if tc.wantCode == "" {
				if ok {
					t.Fatalf("%q is announced as %s; D2 does not include it", tc.id, k.resolvedCode)
				}
				return
			}
			if !ok || k.resolvedCode != tc.wantCode {
				t.Fatalf("resolvedKindForIssue(%q) = (%s, %v), want %s", tc.id, k.resolvedCode, ok, tc.wantCode)
			}
		})
	}
}

// A clear that removes a row the owner was paged for publishes one notice addressed the
// way its alert was: same pipeline, same link, no subject, carrying when the episode began.
func TestResolveLagIssue_AnnouncesAClearedAlert(t *testing.T) {
	opened := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		id       string
		severity string
		want     string
	}{
		{"source stalled", sourceStalledIssueID(resolvedTestPID), "critical", "CDC_SOURCE_STREAM_STALLED_RESOLVED"},
		{"capture stopped", captureStoppedIssueID(resolvedTestPID), "critical", "CDC_CAPTURE_STOPPED_RESOLVED"},
		{"connector down", connectorIssueID(resolvedTestPID), "critical", "CDC_CONNECTOR_DOWN_RESOLVED"},
		{"sink not draining", sinkLagIssueID(resolvedTestPID), "critical", "CDC_SINK_NOT_DRAINING_RESOLVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := captureResolved(t)
			s, mock := resolvedSentinel(t)
			expectLagDelete(mock, tc.id, lagRow(tc.severity, opened))

			s.resolveLagIssue(context.Background(), tc.id, resolvedTestPID)

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			if len(*sent) != 1 {
				t.Fatalf("%d notices, want 1", len(*sent))
			}
			n := (*sent)[0]
			if n.kind.resolvedCode != tc.want {
				t.Errorf("code = %s, want %s", n.kind.resolvedCode, tc.want)
			}
			if n.key != resolvedTestPID || n.pipelineID != resolvedTestPID {
				t.Errorf("key/pipeline = %q/%q, want the pipeline id", n.key, n.pipelineID)
			}
			if n.actionURL != "/pipelines/"+resolvedTestPID {
				t.Errorf("action_url = %q, want the pipeline's page", n.actionURL)
			}
			if n.subject != "" {
				t.Errorf("subject = %q; a pipeline alert has none, so a subject here would never pair", n.subject)
			}
			if !n.openedAt.Equal(opened) {
				t.Errorf("openedAt = %v, want the row's detected_at %v", n.openedAt, opened)
			}
		})
	}
}

// The sink-drain alert is pushed only once it turns critical. A row cleared while it was
// still a warning reached nobody, so there is nothing to retract. Control: the same row
// critical does publish (above), so this cannot pass by publishing nothing at all.
func TestResolveLagIssue_AWarningThatNeverPushedIsNotAnnounced(t *testing.T) {
	for _, id := range []string{
		sinkLagIssueID(resolvedTestPID),
		sourceStalledIssueID(resolvedTestPID),
		captureStoppedIssueID(resolvedTestPID),
		connectorIssueID(resolvedTestPID),
	} {
		sent := captureResolved(t)
		s, mock := resolvedSentinel(t)
		expectLagDelete(mock, id, lagRow("warning", time.Now().Add(-time.Hour)))
		s.resolveLagIssue(context.Background(), id, resolvedTestPID)
		if len(*sent) != 0 {
			t.Errorf("%s cleared at warning severity announced %s", id, (*sent)[0].kind.resolvedCode)
		}
	}
}

// Nothing was open: a tick that re-checks a healthy pipeline must stay silent, or every
// poll would announce a recovery.
func TestResolveLagIssue_NothingOpenAnnouncesNothing(t *testing.T) {
	sent := captureResolved(t)
	s, mock := resolvedSentinel(t)
	id := sinkLagIssueID(resolvedTestPID)
	expectLagDelete(mock, id, sqlmock.NewRows([]string{"severity", "detected_at"}))
	s.resolveLagIssue(context.Background(), id, resolvedTestPID)
	if len(*sent) != 0 {
		t.Fatalf("a delete that removed no row announced %s", (*sent)[0].kind.resolvedCode)
	}
}

// A failed delete may not have closed anything, so it announces nothing.
func TestResolveLagIssue_AFailedDeleteAnnouncesNothing(t *testing.T) {
	sent := captureResolved(t)
	s, mock := resolvedSentinel(t)
	id := sinkLagIssueID(resolvedTestPID)
	mock.ExpectQuery(deleteReturning).WithArgs(id).WillReturnError(errors.New("connection reset"))
	s.resolveLagIssue(context.Background(), id, resolvedTestPID)
	if len(*sent) != 0 {
		t.Fatalf("a failed delete announced %s", (*sent)[0].kind.resolvedCode)
	}
}

// An id outside the allowlist keeps the plain delete: no RETURNING, no notice.
func TestResolveLagIssue_AnUnlistedAlertIsClearedSilently(t *testing.T) {
	sent := captureResolved(t)
	s, mock := resolvedSentinel(t)
	id := "cdc-lag-" + resolvedTestPID
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sentinel_active_issues WHERE id = $1")).
		WithArgs(id).WillReturnResult(driver.RowsAffected(1))
	s.resolveLagIssue(context.Background(), id, resolvedTestPID)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 0 {
		t.Fatalf("source lag announced %s; it is not in D2", (*sent)[0].kind.resolvedCode)
	}
}

// The WAL alert is pushed at warning too, so a warning that clears is announced.
func TestResolveWALAlert_AnnouncesOnlyAPipelineOwnedSlot(t *testing.T) {
	const slot = "rsync_slot_aaaaaaaa"
	del := regexp.QuoteMeta("DELETE FROM sentinel_active_issues WHERE id = $1 RETURNING component_id, detected_at")
	opened := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	walRow := func(component string) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"component_id", "detected_at"}).AddRow(component, opened)
	}
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
		err  error
		want bool
	}{
		{"owned by a pipeline", walRow(resolvedTestPID), nil, true},
		{"an orphan slot files under its own name: nobody to tell", walRow(slot), nil, false},
		{"no component recorded", walRow(""), nil, false},
		{"nothing was open", sqlmock.NewRows([]string{"component_id", "detected_at"}), nil, false},
		{"delete failed", nil, errors.New("connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := captureResolved(t)
			s, mock := resolvedSentinel(t)
			q := mock.ExpectQuery(del).WithArgs("cdc-wal-" + slot)
			if tc.err != nil {
				q.WillReturnError(tc.err)
			} else {
				q.WillReturnRows(tc.rows)
			}

			s.resolveWALAlert(context.Background(), slot)

			if got := len(*sent) == 1; got != tc.want {
				t.Fatalf("announced = %v (%d notices), want %v", got, len(*sent), tc.want)
			}
			if !tc.want {
				return
			}
			n := (*sent)[0]
			if n.kind.resolvedCode != "CDC_WAL_PRESSURE_RESOLVED" {
				t.Errorf("code = %s", n.kind.resolvedCode)
			}
			// The alert is keyed by slot name on the topic and addressed to the pipeline.
			if n.key != slot || n.pipelineID != resolvedTestPID || n.actionURL != "/pipelines/"+resolvedTestPID || n.subject != "" {
				t.Errorf("addressed %+v; want key=slot, the pipeline, its page, no subject", n)
			}
			if !n.openedAt.Equal(opened) {
				t.Errorf("openedAt = %v, want %v", n.openedAt, opened)
			}
		})
	}
}

// The notice can be paired with its alert only if it is addressed the same way. Build
// the alert with the sentinel's own builder and compare, for every CDC kind.
func TestAResolvedNoticeIsAddressedLikeItsAlert(t *testing.T) {
	for _, tc := range []struct {
		name      string
		issueType IssueType
		alertType string
		kind      resolvedKind
	}{
		{"source stalled", IssueTypeSourceStreamStalled, "", resolvedStalled},
		{"capture stopped", IssueTypeCaptureStopped, "", resolvedCapture},
		{"connector down", IssueTypeConnectorDown, "", resolvedConnector},
		{"sink not draining", IssueTypeHighLag, "sink_not_draining", resolvedSinkDrain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alert, ok := buildSentinelNotification(tc.issueType, IssueSeverityCritical, tc.alertType, resolvedTestPID, "", nil)
			if !ok {
				t.Fatal("the alert is not pushed at all")
			}
			resolved := buildResolvedNotification(tc.kind, "", resolvedTestPID, notificationActionURL(resolvedTestPID), "", time.Now())

			if alert["pipeline_id"] != resolved["pipeline_id"] {
				t.Errorf("pipeline_id: alert %v, resolved %v", alert["pipeline_id"], resolved["pipeline_id"])
			}
			if alert["action_url"] != resolved["action_url"] {
				t.Errorf("action_url: alert %v, resolved %v", alert["action_url"], resolved["action_url"])
			}
			// The gateway pairs by code: the resolved code is the alert's plus _RESOLVED.
			if got, want := resolved["_code"], alert["_code"].(string)+"_RESOLVED"; got != want {
				t.Errorf("resolved code %v does not answer the alert's %v", got, alert["_code"])
			}
		})
	}
}

// The wire shape: info, an envelope the gateway reads the code from, nothing sensitive,
// and an opened_at only when there is one.
func TestBuildResolvedNotification_Shape(t *testing.T) {
	opened := time.Date(2026, 10, 18, 9, 30, 0, 0, time.FixedZone("x", 5*3600+1800))
	n := buildResolvedNotification(resolvedStalled, "", resolvedTestPID, "/pipelines/"+resolvedTestPID, "svc", opened)

	if n["_code"] != "CDC_SOURCE_STREAM_STALLED_RESOLVED" {
		t.Errorf("_code = %v", n["_code"])
	}
	delete(n, "_code") // emitNotification strips it before marshalling
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type, PipelineID, ActionURL, Severity, Message, OpenedAt string
		Error                                                    struct {
			Code, Severity, Audience string
			UserMessage              string `json:"user_message"`
			DedupSubject             string `json:"dedup_subject"`
		}
	}
	// The gateway's field names, not Go's.
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wire)
	str := func(k string) string { var v string; _ = json.Unmarshal(wire[k], &v); return v }
	got.Type, got.PipelineID, got.ActionURL = str("type"), str("pipeline_id"), str("action_url")
	got.Severity, got.Message, got.OpenedAt = str("severity"), str("message"), str("opened_at")
	if err := json.Unmarshal(wire["error"], &got.Error); err != nil {
		t.Fatalf("error envelope: %v", err)
	}

	if got.Type != resolvedEventType || got.Severity != "info" || got.Error.Severity != "info" {
		t.Errorf("type/severity = %s/%s/%s, want %s/info/info", got.Type, got.Severity, got.Error.Severity, resolvedEventType)
	}
	if got.Error.Code != "CDC_SOURCE_STREAM_STALLED_RESOLVED" || got.Error.DedupSubject != "svc" {
		t.Errorf("envelope = %+v", got.Error)
	}
	if got.Message != resolvedStalled.message || got.Error.UserMessage != resolvedStalled.message {
		t.Errorf("message = %q / %q, want the kind's own", got.Message, got.Error.UserMessage)
	}
	if got.OpenedAt != "2026-10-18T04:00:00Z" {
		t.Errorf("opened_at = %q, want RFC 3339 in UTC", got.OpenedAt)
	}

	// No opened_at when the time is unknown, rather than the zero date.
	if _, has := buildResolvedNotification(resolvedStalled, "", resolvedTestPID, "/x", "", time.Time{})["opened_at"]; has {
		t.Error("a zero opened_at was sent")
	}
}

// Every message states only what is true once the alert has cleared, never an error,
// a host or a count: it is an email.
func TestResolvedMessagesAreSafeToEmail(t *testing.T) {
	for _, k := range []resolvedKind{resolvedStalled, resolvedCapture, resolvedConnector, resolvedSinkDrain, resolvedWAL, resolvedInfrastructure} {
		if strings.TrimSpace(k.message) == "" {
			t.Errorf("%s has no message", k.resolvedCode)
		}
		for _, bad := range []string{"error", "failed", "kafka", "debezium", "slot_", "://", "%d"} {
			if strings.Contains(strings.ToLower(k.message), bad) {
				t.Errorf("%s message %q mentions %q", k.resolvedCode, k.message, bad)
			}
		}
	}
}

func infraIssue(sev IssueSeverity, typ IssueType, component string) *Issue {
	return &Issue{
		ID: "i1", Type: typ, Severity: sev, ComponentID: component,
		DetectedAt: time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC),
	}
}

// An INFRASTRUCTURE_DOWN finding that was pushed and has closed is announced for that
// service alone: same instance scope, same link, and the component id as the subject the
// alert carried, which is what keeps one service's notice from answering another's alert.
func TestPublishInstanceResolved_PairsWithTheServicesAlert(t *testing.T) {
	sent := captureResolved(t)
	issue := infraIssue(IssueSeverityCritical, IssueTypeInfrastructureDown, "infrastructure:orchestrator")

	publishInstanceResolved(nil, issue)

	if len(*sent) != 1 {
		t.Fatalf("%d notices, want 1", len(*sent))
	}
	n := (*sent)[0]
	if n.kind.resolvedCode != "INFRASTRUCTURE_DOWN_RESOLVED" || n.key != "instance" || n.pipelineID != "instance" ||
		n.actionURL != "/admin/health" || n.subject != "infrastructure:orchestrator" {
		t.Errorf("addressed %+v", n)
	}
	if !n.openedAt.Equal(issue.DetectedAt) {
		t.Errorf("openedAt = %v, want %v", n.openedAt, issue.DetectedAt)
	}
	if !strings.Contains(n.message, "responding again") {
		t.Errorf("message = %q", n.message)
	}

	// The alert's side of the same pairing, from the sentinel's own builder.
	alert, _ := buildSentinelNotification(issue.Type, issue.Severity, "", instanceScopeID, "", map[string]interface{}{"component_id": issue.ComponentID})
	if alert["pipeline_id"] != n.pipelineID {
		t.Errorf("alert scope %v, resolved %v", alert["pipeline_id"], n.pipelineID)
	}
}

// Findings that were never pushed, or are about something else, announce nothing. The
// first case is the control: the same finding critical does announce.
func TestPublishInstanceResolved_OnlyAPushedInfrastructureDownIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue *Issue
		want  int
	}{
		{"critical infrastructure down", infraIssue(IssueSeverityCritical, IssueTypeInfrastructureDown, "infrastructure:kafka"), 1},
		{"warning infrastructure down was never pushed", infraIssue(IssueSeverityWarning, IssueTypeInfrastructureDown, "infrastructure:kafka"), 0},
		{"a lost worker heartbeat is not in D2", infraIssue(IssueSeverityCritical, IssueTypeMissingHeartbeat, "worker:abc"), 0},
		{"a closed queue reader is not in D2", infraIssue(IssueSeverityCritical, IssueTypeConsumerGroupClosed, "consumer:abc"), 0},
		{"nil issue", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := captureResolved(t)
			publishInstanceResolved(nil, tc.issue)
			if len(*sent) != tc.want {
				t.Fatalf("%d notices, want %d", len(*sent), tc.want)
			}
		})
	}
}

// The connector-down verdict used to have no way out: once raised, nothing ever cleared
// it. A poll that finds the connector RUNNING again now closes it, and that close is
// announced. Drives the real poll against a Connect that reports RUNNING; the control is
// the same poll with no connector-down row, which must stay silent.
func TestAConnectorThatIsRunningAgainClosesAndAnnouncesTheConnectorDownVerdict(t *testing.T) {
	const connector = "cdc-11111111"
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
		want int
	}{
		{"connector-down was open and critical", lagRow("critical", time.Now().Add(-20*time.Minute)), 1},
		{"nothing was open", sqlmock.NewRows([]string{"severity", "detected_at"}), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := captureResolved(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/connectors" {
					_, _ = w.Write([]byte(`{"` + connector + `":{"status":{"connector":{"state":"RUNNING"},"tasks":[]}}}`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(srv.Close)

			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			t.Cleanup(func() { db.Close() })
			mock.ExpectQuery("SELECT id").
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(captureTestPID))
			mock.ExpectQuery(regexp.QuoteMeta("FROM pipeline_dependencies d")).
				WillReturnRows(captureRows()) // the capture probe has nothing to say
			mock.ExpectQuery(regexp.QuoteMeta("FROM cdc_resources")).
				WillReturnRows(sqlmock.NewRows([]string{"pipeline_id"}).AddRow(captureTestPID))
			expectLagDelete(mock, connectorIssueID(captureTestPID), tc.rows)

			s := &CDCSentinel{db: db, httpClient: srv.Client(), connectURL: srv.URL}
			s.checkActivePipelines(context.Background())

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("the RUNNING poll did not clear the connector-down verdict: %v", err)
			}
			if len(*sent) != tc.want {
				t.Fatalf("%d notices, want %d", len(*sent), tc.want)
			}
			if tc.want == 1 {
				n := (*sent)[0]
				if n.kind.resolvedCode != "CDC_CONNECTOR_DOWN_RESOLVED" || n.pipelineID != captureTestPID {
					t.Errorf("announced %+v", n)
				}
			}
		})
	}
}

// The call site: resolveRecoveredIssues is what closes a core service's finding when its
// health row goes healthy again, and it is the only caller of publishInstanceResolved.
// A pushed (critical) outage is announced once, for that service. A finding that was
// never pushed, one for a service that is only degraded (still open), and a healthy
// service with no finding open all announce nothing.
func TestAHealthyComponentsPushedOutageIsAnnouncedWhenItsFindingCloses(t *testing.T) {
	sent := captureResolved(t)

	redis := &ComponentHealth{ComponentID: "infrastructure:redis", Status: HealthStatusHealthy}
	kafkaC := &ComponentHealth{ComponentID: "infrastructure:kafka", Status: HealthStatusHealthy}
	temporal := &ComponentHealth{ComponentID: "infrastructure:temporal", Status: HealthStatusDegraded}
	minio := &ComponentHealth{ComponentID: "infrastructure:minio", Status: HealthStatusHealthy}

	issue := func(c *ComponentHealth, sev IssueSeverity) *Issue {
		i := infraIssue(sev, IssueTypeInfrastructureDown, c.ComponentID)
		i.ID = generateIssueID(c.ComponentID, IssueTypeInfrastructureDown)
		return i
	}
	redisIssue := issue(redis, IssueSeverityCritical)
	kafkaIssue := issue(kafkaC, IssueSeverityWarning) // never pushed
	temporalIssue := issue(temporal, IssueSeverityCritical)

	a := &Agent{activeIssues: map[string]*Issue{
		redisIssue.ID:    redisIssue,
		kafkaIssue.ID:    kafkaIssue,
		temporalIssue.ID: temporalIssue,
	}}

	a.resolveRecoveredIssues(context.Background(), []*ComponentHealth{redis, kafkaC, temporal, minio})

	if len(*sent) != 1 {
		t.Fatalf("%d notices, want exactly 1 (the pushed outage of the service that is healthy again): %+v", len(*sent), *sent)
	}
	n := (*sent)[0]
	if n.kind.resolvedCode != "INFRASTRUCTURE_DOWN_RESOLVED" || n.subject != "infrastructure:redis" || n.pipelineID != "instance" {
		t.Errorf("announced %+v, want INFRASTRUCTURE_DOWN_RESOLVED for infrastructure:redis", n)
	}
	if _, open := a.activeIssues[temporalIssue.ID]; !open {
		t.Error("a degraded service's finding was closed")
	}
}
