package sentinel

// Coverage for the sustained source-lag escalation. A source-side lag reading (PostgreSQL
// slot lag, MySQL binlog lag) was only ever a warning on the pipeline page: it was written
// as high_lag/warning however long it lasted, and a warning is never pushed (notify.go), so
// a source that stopped being read for a day told nobody. These tests lock when it becomes
// a critical, pushed alert and, as importantly, when it must not.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestSourceLagAlert_EscalatesOnlyWhenOverFrozenAndSilent: every condition is needed. Lag
// over the threshold alone is a busy source catching up; a frozen position alone is a quiet
// one; a reading that is not source-side (the MySQL consumer proxy) measures the sink. And a
// frozen position with heartbeats still arriving is a reader that is alive with nothing to
// commit: a PostgreSQL or MySQL heartbeat does not move the position of a connector whose
// tables are quiet, while the server's log grows from everything else written to it.
func TestSourceLagAlert_EscalatesOnlyWhenOverFrozenAndSilent(t *testing.T) {
	notify := DefaultSourceLagNotifyAfter
	cases := []struct {
		name       string
		over       time.Duration
		frozen     time.Duration
		silent     time.Duration
		sourceSide bool
		wantSev    IssueSeverity
		wantType   string
	}{
		{"just over the threshold", 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, true, IssueSeverityWarning, "source_lag"},
		{"over, frozen and silent at the bound", notify, notify, notify, true, IssueSeverityCritical, "source_lag_sustained"},
		{"over, frozen and silent for hours", 3 * time.Hour, 3 * time.Hour, 3 * time.Hour, true, IssueSeverityCritical, "source_lag_sustained"},
		{"over for hours but the position moves", 3 * time.Hour, notify - time.Second, 3 * time.Hour, true, IssueSeverityWarning, "source_lag"},
		{"frozen but only just over", notify - time.Second, 3 * time.Hour, 3 * time.Hour, true, IssueSeverityWarning, "source_lag"},
		{"over and frozen for hours but heartbeats arrive", 3 * time.Hour, 3 * time.Hour, notify - time.Second, true, IssueSeverityWarning, "source_lag"},
		{"the consumer proxy never escalates", 3 * time.Hour, 3 * time.Hour, 3 * time.Hour, false, IssueSeverityWarning, "source_lag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sev, alertType := sourceLagAlert(tc.over, tc.frozen, tc.silent, notify, tc.sourceSide)
			if sev != tc.wantSev || alertType != tc.wantType {
				t.Fatalf("got %s/%s, want %s/%s", sev, alertType, tc.wantSev, tc.wantType)
			}
		})
	}
}

// expectSourceLagUpsert scripts the one upsert emitSourceLagAlert makes, on the source-lag
// issue id whatever the severity — the page shows one alert that escalates, not two.
func expectSourceLagUpsert(mock sqlmock.Sqlmock, pid string, sev IssueSeverity) {
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO sentinel_active_issues")).
		WithArgs("cdc-lag-"+pid, string(IssueTypeHighLag), string(sev),
			pid, string(ComponentTypeCDCPipeline), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(detectedAtRow())
}

// rewindSourceLag moves a pipeline's remembered lag clock back by d, as if the readings that
// produced it had been taken d earlier.
func rewindSourceLag(t *testing.T, s *CDCSentinel, pid string, d time.Duration) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sourceLag[pid]
	if !ok {
		t.Fatalf("an over-threshold source-side reading for %s started no lag clock", pid)
	}
	st.since = st.since.Add(-d)
	st.posMovedAt = st.posMovedAt.Add(-d)
	st.beatsMovedAt = st.beatsMovedAt.Add(-d)
	s.sourceLag[pid] = st
}

// stoppedReader is a source-side reading whose position and heartbeat count repeat from one
// tick to the next: nothing is reading the source.
var stoppedReader = sourceLagProbe{sourceSide: true, position: "0/16B3748", heartbeats: 3871, slot: "rsync_slot_p1"}

// TestSustainedFrozenSourceLagEscalatesTheSameRow: lag that stays over the threshold with the
// committed position frozen for the notify bound becomes a critical source_lag_sustained on
// the same row, so it is pushed (CDC_SOURCE_LAG_HIGH) instead of sitting on the page. Red
// before the fix: every reading was written as a warning.
func TestSustainedFrozenSourceLagEscalatesTheSameRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}
	ctx := context.Background()

	// First reading: the clock starts, nothing is known about how long it has lasted.
	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, stoppedReader)

	// The same lag and the same position, a notify bound later.
	rewindSourceLag(t, s, "p1", sourceLagNotifyAfter()+time.Minute)
	mock.ExpectQuery("FROM sentinel_active_issues").
		WillReturnRows(sqlmock.NewRows([]string{"x"}))
	expectSourceLagUpsert(mock, "p1", IssueSeverityCritical)
	metadata := map[string]interface{}{}
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", metadata, stoppedReader)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sustained frozen lag was not escalated on the same row: %v", err)
	}
	if got, _ := metadata["lagging_for_seconds"].(int64); got < int64(sourceLagNotifyAfter().Seconds()) {
		t.Fatalf("lagging_for_seconds = %v, want at least the notify bound", metadata["lagging_for_seconds"])
	}
	if got, _ := metadata["frozen_for_seconds"].(int64); got < int64(sourceLagNotifyAfter().Seconds()) {
		t.Fatalf("frozen_for_seconds = %v, want at least the notify bound", metadata["frozen_for_seconds"])
	}
	if got, _ := metadata["heartbeat_silent_for_seconds"].(int64); got < int64(sourceLagNotifyAfter().Seconds()) {
		t.Fatalf("heartbeat_silent_for_seconds = %v, want at least the notify bound", metadata["heartbeat_silent_for_seconds"])
	}
}

// TestASourceLagRelapseIsItsOwnEpisode is the CDC_SOURCE_LAG_HIGH that never reached the
// owner on prod (2026-11-02, KI-CDC-SOURCE-LAG-RELAPSE-SILENT): Card Transactions' heartbeats
// stopped for half an hour, the alert went critical, and the api-gateway dropped every push
// as a repeat of a page from three days before. Its cdc-lag row had stayed open since then, a
// warning while the quiet source sat over the lag threshold, and each push named that row's
// detected_at as when its episode opened, so the old page counted. The class: an alert
// pushed only at its higher severity, whose row outlives the pushed state at the lower one,
// never closes its episode. So each stall is its own episode, opened when it turned
// critical: two readings of one stall name the same opening, and a stall after the reader
// came back names a later one.
func TestASourceLagRelapseIsItsOwnEpisode(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s := &CDCSentinel{db: db, sourceLagNow: func() time.Time { return now }}
	ctx := context.Background()
	sent := captureAlerts(t)
	// The row has been open since a page three days ago: detected_at is kept on conflict.
	detected := now.Add(-72 * time.Hour)

	type push struct{ at, opened time.Time }
	var pushes []push
	read := func(probe sourceLagProbe, sev IssueSeverity) {
		t.Helper()
		if sev == IssueSeverityCritical {
			mock.ExpectQuery("FROM sentinel_active_issues").
				WillReturnRows(sqlmock.NewRows([]string{"x"}))
		}
		mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO sentinel_active_issues")).
			WithArgs("cdc-lag-p1", string(IssueTypeHighLag), string(sev),
				"p1", string(ComponentTypeCDCPipeline), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows([]string{"detected_at"}).AddRow(detected))
		before := len(*sent)
		s.emitSourceLagAlert(ctx, "p1", "card-transactions", "postgresql", "lag", map[string]interface{}{}, probe)
		for _, a := range (*sent)[before:] {
			if alertCode(a.notification) != "CDC_SOURCE_LAG_HIGH" {
				continue
			}
			stamp, _ := a.notification["opened_at"].(string)
			opened, err := time.Parse(time.RFC3339, stamp)
			if err != nil {
				t.Fatalf("a CDC_SOURCE_LAG_HIGH push says no time its episode opened: %q", stamp)
			}
			pushes = append(pushes, push{at: now, opened: opened})
		}
	}

	reader := stoppedReader
	stall1 := now
	// The first stall: over the threshold, frozen and silent for the notify bound, and
	// still so five minutes on.
	read(reader, IssueSeverityWarning)
	now = now.Add(sourceLagNotifyAfter() + time.Minute)
	read(reader, IssueSeverityCritical)
	now = now.Add(5 * time.Minute)
	read(reader, IssueSeverityCritical)
	// The reader is back: a heartbeat arrives while the quiet source stays over the
	// threshold, so the row stays open, as a warning.
	now = now.Add(5 * time.Minute)
	reader.heartbeats++
	read(reader, IssueSeverityWarning)
	// Hours later the last heartbeat arrives, and none follows it for the notify bound.
	now = now.Add(2 * time.Hour)
	reader.heartbeats++
	stall2 := now
	read(reader, IssueSeverityWarning)
	now = now.Add(sourceLagNotifyAfter() + time.Minute)
	read(reader, IssueSeverityCritical)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if len(pushes) != 3 {
		t.Fatalf("CDC_SOURCE_LAG_HIGH was pushed %d times, want 3: two readings of the first stall, one of the second", len(pushes))
	}
	if want := stall1.Add(sourceLagNotifyAfter()); !pushes[0].opened.Equal(want) || !pushes[1].opened.Equal(want) {
		t.Errorf("the first stall's readings say it opened at %v and %v, want %v: when it turned critical, the same on every reading",
			pushes[0].opened, pushes[1].opened, want)
	}
	if want := stall2.Add(sourceLagNotifyAfter()); !pushes[2].opened.Equal(want) {
		t.Errorf("the relapse says it opened at %v, want %v: when the second stall turned critical", pushes[2].opened, want)
	}
	// The api-gateway counts a stored push as a repeat when it is newer than opened_at less a
	// minute of clock slack (notifier.go recentDuplicate).
	for _, p := range pushes[:2] {
		if !p.at.Before(pushes[2].opened.Add(-time.Minute)) {
			t.Errorf("the relapse opened at %v, before the first stall's push at %v: the api-gateway drops it as a repeat of that page",
				pushes[2].opened, p.at)
		}
	}
}

// TestAQuietSourceWhoseHeartbeatsArriveIsNotEscalated is the false CDC_SOURCE_LAG_HIGH on
// prod (2026-10-30, KI-CDC-SOURCE-LAG-CRITICAL-ON-IDLE-SOURCE): a PostgreSQL pipeline whose
// four tables had no writes held its slot at one confirmed_flush_lsn while the server's WAL
// grew ~142 MB from other databases, and its heartbeat topic kept advancing every five
// minutes. Lag over the threshold and a frozen position for half an hour paged the owner
// that rsync.ai had stopped reading. A reader that keeps writing heartbeats has not stopped.
func TestAQuietSourceWhoseHeartbeatsArriveIsNotEscalated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}
	ctx := context.Background()

	quiet := stoppedReader
	quiet.heartbeats = 3870
	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "card-transactions", "postgresql", "lag", map[string]interface{}{}, quiet)

	// Same position, an hour later, one more heartbeat on the topic.
	rewindSourceLag(t, s, "p1", time.Hour)
	quiet.heartbeats = 3871
	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "card-transactions", "postgresql", "lag", map[string]interface{}{}, quiet)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a quiet source whose heartbeats arrive was escalated: %v", err)
	}
}

// TestASourceWhoseHeartbeatsCannotBeCountedNeverEscalates: a connector with heartbeats off,
// or whose heartbeat topic cannot be read, gives no count (0), and without one a frozen
// position cannot tell a stopped reader from a quiet one. The class: the alarm takes "the
// reader stopped" from the configuration instead of from the heartbeats the connector wrote.
func TestASourceWhoseHeartbeatsCannotBeCountedNeverEscalates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}
	ctx := context.Background()

	uncounted := stoppedReader
	uncounted.heartbeats = 0
	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, uncounted)
	rewindSourceLag(t, s, "p1", 3*time.Hour)
	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, uncounted)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a source with no heartbeat count was escalated: %v", err)
	}
}

// TestHeartbeatsWrittenReadsTheConnectorsHeartbeatTopic: the count both lag probes hand the
// alarm is the end offset of the topic the connector config names, and 0 whenever that
// says nothing (heartbeats off, no topic.prefix, a read error, no such topic yet).
func TestHeartbeatsWrittenReadsTheConnectorsHeartbeatTopic(t *testing.T) {
	prodShape := map[string]interface{}{
		"heartbeat.interval.ms":  "300000",
		"topic.heartbeat.prefix": "rsync.heartbeat",
		"topic.prefix":           "rsync.cdc-76dc7ef5",
	}
	without := func(key string) map[string]interface{} {
		cfg := map[string]interface{}{}
		for k, v := range prodShape {
			if k != key {
				cfg[k] = v
			}
		}
		return cfg
	}
	for _, tc := range []struct {
		name   string
		config map[string]interface{}
		end    int64
		exists bool
		err    error
		want   int64
	}{
		{"the connector's heartbeat topic", prodShape, 3871, true, nil, 3871},
		{"heartbeats off", without("heartbeat.interval.ms"), 3871, true, nil, 0},
		{"no topic.prefix", without("topic.prefix"), 3871, true, nil, 0},
		{"the topic cannot be read", prodShape, 3871, true, context.DeadlineExceeded, 0},
		{"no such topic", prodShape, 0, false, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked string
			s := &CDCSentinel{heartbeatEndOffset: func(topic string) (int64, bool, error) {
				asked = topic
				return tc.end, tc.exists, tc.err
			}}
			if got := s.heartbeatsWritten(tc.config); got != tc.want {
				t.Fatalf("heartbeatsWritten = %d, want %d", got, tc.want)
			}
			if tc.want > 0 && asked != "rsync.heartbeat.rsync.cdc-76dc7ef5" {
				t.Fatalf("read topic %q, want the connector's heartbeat topic", asked)
			}
		})
	}
}

// TestASourceWhosePositionMovesIsNotEscalated: a busy source catching up has lag over the
// threshold for a long time, but its committed position keeps moving. That is the healthy
// case the frozen-position condition exists for.
func TestASourceWhosePositionMovesIsNotEscalated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}
	ctx := context.Background()

	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, stoppedReader)
	rewindSourceLag(t, s, "p1", 3*time.Hour)

	moved := stoppedReader
	moved.position = "0/16C0000"
	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, moved)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a moving source was escalated: %v", err)
	}
}

// TestEverySourceSideLagProbeCarriesItsHeartbeatCount: the alarm only escalates once the
// heartbeat count has stopped changing, and a probe that leaves the count out gives 0, which
// never escalates. So a source-side probe built without it would page nobody however long
// its source went unread. Both live probes (slot lag, binlog lag) open a connection to the
// source database and have no test of their own; this holds every one of them, and any added
// later, to passing the count.
func TestEverySourceSideLagProbeCarriesItsHeartbeatCount(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sourceSide := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "sourceLagProbe" {
				return true
			}
			keys := map[string]string{}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						v, _ := kv.Value.(*ast.Ident)
						keys[k.Name] = ""
						if v != nil {
							keys[k.Name] = v.Name
						}
					}
				}
			}
			if keys["sourceSide"] != "true" {
				return true
			}
			sourceSide++
			if _, ok := keys["heartbeats"]; !ok {
				t.Errorf("%s: a source-side lag probe without its heartbeat count can never escalate", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if sourceSide < 2 {
		t.Fatalf("found %d source-side lag probes, want the slot and binlog probes at least", sourceSide)
	}
}

// TestSourceLagClockDoesNotSurviveAStop: a tick that does not see the pipeline running drops
// its clock. Otherwise a user who stops a pipeline for two hours and starts it again gets a
// critical alert on the first reading, for lag their own stop caused.
func TestSourceLagClockDoesNotSurviveAStop(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}
	ctx := context.Background()

	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, stoppedReader)
	rewindSourceLag(t, s, "p1", 2*time.Hour)

	s.pruneSourceLag(map[string]bool{"some-other-pipeline": true})

	expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
	s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, stoppedReader)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the lag clock survived a tick that did not see the pipeline: %v", err)
	}
}

// TestSourceLagDoesNotEscalateOverAStrongerAlarm: when a more specific alarm about the same
// pipeline is already open, the sustained-lag push would only repeat it with a vaguer
// message. Each of these holds the lag at a warning; a sink-lag that is itself only a
// warning does not.
func TestSourceLagDoesNotEscalateOverAStrongerAlarm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		open    bool
		wantSev IssueSeverity
	}{
		{"a stronger alarm is open", true, IssueSeverityWarning},
		{"nothing stronger is open", false, IssueSeverityCritical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			s := &CDCSentinel{db: db}
			ctx := context.Background()

			expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
			s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, stoppedReader)
			rewindSourceLag(t, s, "p1", 3*time.Hour)

			rows := sqlmock.NewRows([]string{"x"})
			if tc.open {
				rows.AddRow(1)
			}
			mock.ExpectQuery("FROM sentinel_active_issues").WillReturnRows(rows)
			expectSourceLagUpsert(mock, "p1", tc.wantSev)
			s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "postgresql", "lag", map[string]interface{}{}, stoppedReader)

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestSourceLagSuppressionNamesEveryStrongerAlarm pins which open issues hold the lag at a
// warning: the source has stopped (stalled or capture stopped), the connector is down, the
// sink is critically behind, or the slot's WAL is the problem. A sink-lag only counts when
// critical — a warning sink-lag says nothing stronger than this alert does.
func TestSourceLagSuppressionNamesEveryStrongerAlarm(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}

	mock.ExpectQuery("FROM sentinel_active_issues").
		WithArgs(sourceStalledIssueID("p1"), captureStoppedIssueID("p1"), connectorIssueID("p1"),
			"cdc-wal-rsync_slot_p1", sinkLagIssueID("p1"), string(IssueSeverityCritical)).
		WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(1))

	if !s.sourceLagSuppressed(context.Background(), "p1", "rsync_slot_p1") {
		t.Fatal("an open stronger alarm did not suppress the escalation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestTheConsumerProxyNeverEscalates: the MySQL fallback measures the sink's Kafka backlog,
// not the source, so however long it lasts it is never reported as the source falling
// behind — and it never asks whether to suppress an escalation it cannot make.
func TestTheConsumerProxyNeverEscalates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	s := &CDCSentinel{db: db}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		expectSourceLagUpsert(mock, "p1", IssueSeverityWarning)
		s.emitSourceLagAlert(ctx, "p1", "orders-cdc", "mysql", "lag", map[string]interface{}{}, sourceLagProbe{})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sourceLag["p1"]; ok {
		t.Fatal("the consumer proxy started a source-lag clock")
	}
}

// TestATickDropsTheLagClockOfAPipelineItDidNotFindRunning drives the prune through
// checkSourceDBLag: a pipeline neither source-lag branch found running loses its clock, but
// only when both discovery queries succeeded. A failed query says nothing about which
// pipelines stopped, and dropping every clock on a database blip would restart each
// half-hour count from zero.
func TestATickDropsTheLagClockOfAPipelineItDidNotFindRunning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pgFails   bool
		wantClock bool
	}{
		{"both queries answered", false, false},
		{"a discovery query failed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			s := &CDCSentinel{db: db, sourceLag: map[string]sourceLagState{
				"stopped-1": {since: time.Now().Add(-2 * time.Hour), posMovedAt: time.Now().Add(-2 * time.Hour)},
			}}

			pg := mock.ExpectQuery(`JOIN cdc_resources cr`)
			if tc.pgFails {
				pg.WillReturnError(context.DeadlineExceeded)
			} else {
				pg.WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connection_id", "resource_name"}))
			}
			mock.ExpectQuery(`ILIKE 'mysql%'`).
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "source_connection_id"}))
			mock.ExpectQuery(`LEFT JOIN connections c`).
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_type"}))

			s.checkSourceDBLag(context.Background())
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}

			s.mu.Lock()
			defer s.mu.Unlock()
			if _, ok := s.sourceLag["stopped-1"]; ok != tc.wantClock {
				t.Fatalf("clock kept = %v, want %v", ok, tc.wantClock)
			}
		})
	}
}

// TestBinlogLagDiscoveryNamesEveryBinlogSource: the binlog branch measures a source by its
// Debezium binlog offset (file/pos), which a MariaDB source has too, yet its discovery query
// named only 'mysql%'. A MariaDB pipeline's source lag was never measured, so its
// sustained-lag alert could never fire (KI-MARIADB-SOURCE-LAG-NOT-MEASURED). The class is a
// hand-written family list that leaves out a member: the family is whatever horizonFamily
// folds onto "mysql", so a type added there and not here fails this test.
func TestBinlogLagDiscoveryNamesEveryBinlogSource(t *testing.T) {
	var binlogSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(expected, actual string) error {
			if err := sqlmock.QueryMatcherRegexp.Match(expected, actual); err != nil {
				return err
			}
			if strings.Contains(expected, "source_connection_id") {
				binlogSQL = actual
			}
			return nil
		})))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`JOIN cdc_resources cr`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connection_id", "resource_name"}))
	mock.ExpectQuery(`JOIN connections c ON c.id = p.source_connection_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "source_connection_id"}))
	mock.ExpectQuery(`LEFT JOIN connections c`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_type"}))

	s := &CDCSentinel{db: db}
	s.checkSourceDBLag(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	sql := strings.Join(strings.Fields(binlogSQL), " ")
	var prefixes []string
	for _, m := range regexp.MustCompile(`connector_type ILIKE '([a-z_]+)%'`).FindAllStringSubmatch(sql, -1) {
		prefixes = append(prefixes, m[1])
	}
	for _, connectorType := range []string{"mysql", "MySQL", "mariadb", "MariaDB", "sqlserver", "mssql", "postgresql", "oracle", "mongodb"} {
		if horizonFamily(connectorType) != "mysql" {
			continue
		}
		named := false
		for _, p := range prefixes {
			if strings.HasPrefix(strings.ToLower(connectorType), p) {
				named = true
			}
		}
		if !named {
			t.Errorf("a %s source has a binlog offset but the binlog-lag discovery query does not find it (it names %v)", connectorType, prefixes)
		}
	}
	// An unbracketed OR would bind as (running AND cdc AND mysql) OR mariadb, and measure
	// MariaDB pipelines that are stopped.
	bracketed := regexp.MustCompile(`AND \(c\.connector_type ILIKE '[a-z_]+%'( OR c\.connector_type ILIKE '[a-z_]+%')*\)`)
	if len(prefixes) > 1 && !bracketed.MatchString(sql) {
		t.Errorf("the family filter is not bracketed, so the running/CDC conditions do not cover every family member: %s", sql)
	}
}
