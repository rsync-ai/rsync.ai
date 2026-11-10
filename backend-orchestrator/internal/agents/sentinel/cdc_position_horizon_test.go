package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/backend-orchestrator/internal/cdc"
	"github.com/rsync-ai/backend-orchestrator/internal/utils"
)

// MySQL/MariaDB and SQL Server purge their change log on a clock, so a stopped CDC
// pipeline loses its POSITION rather than filling the source's disk. The WAL watchdog's
// rule holds for them too: warn, then start the stopped pipeline (resume a paused one)
// before the source purges the log behind its stored offset, and never drop or rewind
// anything. A position already purged is reported, not acted on.

// horizonHarness is one position-horizon sweep against a mocked metadata DB, a fake
// Kafka Connect / api-gateway server that serves the stored offsets and records every
// other call, and a stubbed purge-clock measurement.
type horizonHarness struct {
	s        *CDCSentinel
	meta     sqlmock.Sqlmock
	mu       sync.Mutex
	queries  []string
	calls    []string // "METHOD path body"
	offsets  string   // the GET offsets body; "" answers 404
	measured []string // "family connection key=value" per measurement
}

func newHorizonHarness(t *testing.T, autoAct bool, httpStatus int, h cdc.PositionHorizon, measureErr error) *horizonHarness {
	t.Helper()
	hh := &horizonHarness{}
	record := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		hh.mu.Lock()
		hh.queries = append(hh.queries, actual)
		hh.mu.Unlock()
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	metaDB, meta, err := sqlmock.New(sqlmock.QueryMatcherOption(record))
	if err != nil {
		t.Fatal(err)
	}
	offsetsPath := "/connectors/cdc-" + utils.SafeID8(walTestPipeline) + "/offsets"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == offsetsPath {
			if hh.offsets == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, hh.offsets)
			return
		}
		body, _ := io.ReadAll(r.Body)
		hh.mu.Lock()
		hh.calls = append(hh.calls, r.Method+" "+r.URL.Path+" "+string(body))
		hh.mu.Unlock()
		w.WriteHeader(httpStatus)
	}))
	t.Cleanup(func() { srv.Close(); metaDB.Close() })

	hh.meta = meta
	hh.s = &CDCSentinel{
		db:              metaDB,
		httpClient:      srv.Client(),
		connectURL:      srv.URL,
		apiGatewayURL:   srv.URL,
		walAutoAct:      autoAct,
		horizonWarn:     24 * time.Hour,
		horizonCritical: 6 * time.Hour,
		horizonMeasure: func(_ context.Context, family, connectionID string, offset map[string]interface{}) (cdc.PositionHorizon, error) {
			// Read the value before locking: the sweep recovers a panic, and a panic here while
			// holding the lock would hang the test instead of failing it.
			key := horizonOffsetKey[family]
			val, _ := offset[key].(string)
			hh.mu.Lock()
			hh.measured = append(hh.measured, family+" "+connectionID+" "+key+"="+val)
			hh.mu.Unlock()
			return h, measureErr
		},
	}
	return hh
}

const (
	mysqlOffsets     = `{"offsets":[{"partition":{"server":"rsync"},"offset":{"file":"mysql-bin.000002","pos":1234,"ts_sec":1759000000}}]}`
	sqlserverOffsets = `{"offsets":[{"partition":{"server":"rsync","database":"shop"},"offset":{"commit_lsn":"0000002b:00000768:0005","change_lsn":"0000002b:00000768:0003"}}]}`
)

// pipeline queues the stale-alert sweep and the listing of one stopped/paused pipeline.
func (hh *horizonHarness) pipeline(state, connectorType, offsets string) {
	hh.offsets = offsets
	// An alert outlives its pipeline's stop only while the pipeline is still stopped or
	// paused: sqlmock cannot run the SQL, so its shape is pinned here.
	hh.meta.ExpectExec(`DELETE FROM sentinel_active_issues si\s+WHERE si.id LIKE 'cdc-position-%'\s+` +
		`AND NOT EXISTS \(\s+SELECT 1 FROM pipelines p\s+WHERE si.id = 'cdc-position-' \|\| p.id::text\s+` +
		`AND p.status IN \('stopped', 'failed', 'cancelled', 'paused'\)\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	hh.meta.ExpectQuery(`FROM pipelines p\s+JOIN connections c`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "status", "source_connection_id", "connector_type"}).
			AddRow(walTestPipeline, "orders", state, walTestConn, connectorType))
}

func (hh *horizonHarness) alert(desc, meta *string) {
	hh.meta.ExpectQuery(`INSERT INTO sentinel_active_issues`).
		WithArgs("cdc-position-"+walTestPipeline, sqlmock.AnyArg(), sqlmock.AnyArg(), walTestPipeline,
			sqlmock.AnyArg(), capture{desc}, capture{meta}).
		WillReturnRows(detectedAtRow())
}

func (hh *horizonHarness) resolves() {
	hh.meta.ExpectExec(`DELETE FROM sentinel_active_issues WHERE id = \$1`).
		WithArgs("cdc-position-" + walTestPipeline).WillReturnResult(sqlmock.NewResult(0, 1))
}

func (hh *horizonHarness) budget(used int) {
	hh.meta.ExpectQuery(`FROM pipeline_run_events`).WithArgs(walTestPipeline, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(used))
}

func (hh *horizonHarness) recordsAutoStart(eventType string, payload *string) {
	hh.meta.ExpectExec(`INSERT INTO pipeline_run_events`).
		WithArgs(walTestPipeline, sqlmock.AnyArg(), eventType, capture{payload}).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func (hh *horizonHarness) sweep(t *testing.T) {
	t.Helper()
	hh.s.checkPositionHorizons(context.Background())
	if err := hh.meta.ExpectationsWereMet(); err != nil {
		t.Errorf("metadata DB: %v\nqueries seen:\n  %s", err, strings.Join(hh.queries, "\n  "))
	}
	// Nothing on this path may drop, rewind or reset a stored position.
	for _, q := range hh.queries {
		if strings.Contains(q, "DROP") || strings.Contains(q, "cdc_resources") {
			t.Errorf("position horizon reached for a stored position:\n%s", q)
		}
	}
	for _, c := range hh.httpCalls() {
		if strings.HasPrefix(c, "DELETE ") || strings.HasPrefix(c, "PATCH ") || strings.Contains(c, "/offsets") {
			t.Errorf("position horizon changed the connector's stored offsets: %q", c)
		}
	}
}

func (hh *horizonHarness) httpCalls() []string {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	return append([]string(nil), hh.calls...)
}

func metaField(t *testing.T, meta, key string) interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("alert metadata is not JSON: %v (%q)", err, meta)
	}
	return m[key]
}

func TestPositionHorizon_StoppedMySQLPipelineNearPurgeIsStarted(t *testing.T) {
	for _, state := range []string{"stopped", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			hh := newHorizonHarness(t, true, http.StatusOK,
				cdc.PositionHorizon{Retention: 7 * 24 * time.Hour, Remaining: 2 * time.Hour}, nil)
			var desc, meta, payload string
			hh.pipeline(state, "mysql", mysqlOffsets)
			hh.budget(0)
			hh.recordsAutoStart("wal_watchdog_auto_start", &payload)
			hh.alert(&desc, &meta)
			hh.sweep(t)

			if want := []string{"mysql " + walTestConn + " file=mysql-bin.000002"}; strings.Join(hh.measured, "|") != want[0] {
				t.Errorf("measured %q, want %q", hh.measured, want)
			}
			calls := hh.httpCalls()
			if len(calls) != 1 || !strings.HasPrefix(calls[0], "POST /api/v1/internal/pipelines/"+walTestPipeline+"/run ") ||
				!strings.Contains(calls[0], `"reason":"position_retention"`) {
				t.Fatalf("HTTP calls = %q, want one start with reason position_retention", calls)
			}
			if a := walAction(t, meta); a != string(walRemedyStart) {
				t.Errorf("alert action = %q, want %q", a, walRemedyStart)
			}
			if r := metaField(t, meta, "action_result"); r != "done" {
				t.Errorf("action_result = %v, want done", r)
			}
			if !strings.Contains(desc, "was started automatically") || !strings.Contains(desc, "binlog") {
				t.Errorf("alert does not say the pipeline was started: %q", desc)
			}
			if !strings.Contains(payload, `"reason":"position_retention"`) || !strings.Contains(payload, `"source":"mysql"`) {
				t.Errorf("timeline event payload = %q", payload)
			}
		})
	}
}

func TestPositionHorizon_PausedSQLServerPipelineNearPurgeIsResumed(t *testing.T) {
	hh := newHorizonHarness(t, true, http.StatusAccepted,
		cdc.PositionHorizon{Retention: 72 * time.Hour, Remaining: time.Hour}, nil)
	var desc, meta, payload string
	hh.pipeline("paused", "sqlserver", sqlserverOffsets)
	hh.budget(0)
	hh.meta.ExpectExec(`UPDATE pipelines SET status = 'running'`).WithArgs(walTestPipeline).
		WillReturnResult(sqlmock.NewResult(0, 1))
	hh.recordsAutoStart("wal_watchdog_auto_resume", &payload)
	hh.alert(&desc, &meta)
	hh.sweep(t)

	if len(hh.measured) != 1 || hh.measured[0] != "sqlserver "+walTestConn+" commit_lsn=0000002b:00000768:0005" {
		t.Errorf("measured %q", hh.measured)
	}
	want := "PUT /connectors/cdc-" + utils.SafeID8(walTestPipeline) + "/resume "
	if calls := hh.httpCalls(); len(calls) != 1 || calls[0] != want {
		t.Fatalf("HTTP calls = %q, want exactly [%q]", calls, want)
	}
	if a := walAction(t, meta); a != string(walRemedyResume) {
		t.Errorf("alert action = %q, want %q", a, walRemedyResume)
	}
	if !strings.Contains(desc, "was resumed automatically") || !strings.Contains(desc, "change tables") {
		t.Errorf("alert does not say the pipeline was resumed: %q", desc)
	}
}

func TestPositionHorizon_ConnectorTypeAliasesAreMeasured(t *testing.T) {
	for connectorType, family := range map[string]string{
		"MySQL": "mysql", "mariadb": "mysql", "mssql": "sqlserver", "sqlserver-cdc": "sqlserver",
	} {
		t.Run(connectorType, func(t *testing.T) {
			hh := newHorizonHarness(t, true, http.StatusOK, cdc.PositionHorizon{Safe: true}, nil)
			offsets := mysqlOffsets
			if family == "sqlserver" {
				offsets = sqlserverOffsets
			}
			hh.pipeline("stopped", connectorType, offsets)
			hh.resolves()
			hh.sweep(t)
			if len(hh.measured) != 1 || !strings.HasPrefix(hh.measured[0], family+" ") {
				t.Errorf("measured %q, want one %s measurement", hh.measured, family)
			}
		})
	}
	for _, other := range []string{"postgresql", "mongodb", "oracle", ""} {
		if f := horizonFamily(other); f != "" {
			t.Errorf("horizonFamily(%q) = %q, want none", other, f)
		}
	}
}

func TestPositionHorizon_WarningSaysItWillBeStartedAndDoesNothing(t *testing.T) {
	hh := newHorizonHarness(t, true, http.StatusOK,
		cdc.PositionHorizon{Retention: 7 * 24 * time.Hour, Remaining: 12 * time.Hour}, nil)
	var desc, meta string
	hh.pipeline("stopped", "mysql", mysqlOffsets)
	hh.alert(&desc, &meta)
	hh.sweep(t)

	if calls := hh.httpCalls(); len(calls) != 0 {
		t.Errorf("warning tier acted: %q", calls)
	}
	if a := walAction(t, meta); a != string(walRemedyAlertOnly) {
		t.Errorf("alert action = %q, want %q", a, walRemedyAlertOnly)
	}
	if c := metaField(t, meta, "critical_action"); c != string(walRemedyStart) {
		t.Errorf("critical_action = %v, want %s", c, walRemedyStart)
	}
	if !strings.Contains(desc, "6 hours before the purge, the watchdog starts the pipeline automatically") {
		t.Errorf("warning does not say what happens at critical: %q", desc)
	}
}

func TestPositionHorizon_FarOrSafePositionResolves(t *testing.T) {
	for name, h := range map[string]cdc.PositionHorizon{
		"far":  {Retention: 7 * 24 * time.Hour, Remaining: 48 * time.Hour},
		"safe": {Safe: true},
	} {
		t.Run(name, func(t *testing.T) {
			hh := newHorizonHarness(t, true, http.StatusOK, h, nil)
			hh.pipeline("stopped", "mysql", mysqlOffsets)
			hh.resolves()
			hh.sweep(t)
			if calls := hh.httpCalls(); len(calls) != 0 {
				t.Errorf("acted on a position in no danger: %q", calls)
			}
		})
	}
}

func TestPositionHorizon_PurgedPositionIsReportedNotActedOn(t *testing.T) {
	hh := newHorizonHarness(t, true, http.StatusOK, cdc.PositionHorizon{Purged: true}, nil)
	var desc, meta string
	hh.pipeline("stopped", "mysql", mysqlOffsets)
	hh.alert(&desc, &meta)
	hh.sweep(t)

	if calls := hh.httpCalls(); len(calls) != 0 {
		t.Errorf("started a pipeline whose position is already gone: %q", calls)
	}
	if a := walAction(t, meta); a != string(walRemedyAlertOnly) {
		t.Errorf("alert action = %q, want %q", a, walRemedyAlertOnly)
	}
	if p := metaField(t, meta, "position_purged"); p != true {
		t.Errorf("position_purged = %v", p)
	}
	if !strings.Contains(desc, "no longer on the source") || !strings.Contains(desc, "Reload") {
		t.Errorf("purged alert does not say so: %q", desc)
	}
}

func TestPositionHorizon_BudgetSpentLeavesPipelineAsIs(t *testing.T) {
	hh := newHorizonHarness(t, true, http.StatusOK,
		cdc.PositionHorizon{Retention: 72 * time.Hour, Remaining: time.Hour}, nil)
	var desc, meta string
	hh.pipeline("stopped", "mysql", mysqlOffsets)
	hh.budget(walAutoStartCap)
	hh.alert(&desc, &meta)
	hh.sweep(t)

	if calls := hh.httpCalls(); len(calls) != 0 {
		t.Errorf("started past the budget: %q", calls)
	}
	if r := metaField(t, meta, "action_result"); r != "budget_spent" {
		t.Errorf("action_result = %v, want budget_spent", r)
	}
	if !strings.Contains(desc, "left as is") || !strings.Contains(desc, "position is kept") {
		t.Errorf("alert does not say the pipeline was left: %q", desc)
	}
}

func TestPositionHorizon_AutoActOffOnlyAlerts(t *testing.T) {
	hh := newHorizonHarness(t, false, http.StatusOK,
		cdc.PositionHorizon{Retention: 72 * time.Hour, Remaining: time.Hour}, nil)
	var desc, meta string
	hh.pipeline("stopped", "mysql", mysqlOffsets)
	hh.alert(&desc, &meta)
	hh.sweep(t)

	if calls := hh.httpCalls(); len(calls) != 0 {
		t.Errorf("acted with CDC_WAL_WATCHDOG_AUTOACT off: %q", calls)
	}
	if !strings.Contains(desc, "Automatic action is off") {
		t.Errorf("alert does not say automatic action is off: %q", desc)
	}
}

func TestPositionHorizon_NothingToMeasureIsSkippedSilently(t *testing.T) {
	cases := map[string]struct {
		offsets    string
		measureErr error
		measures   bool
	}{
		"no stored offsets (connector gone or Connect < 3.6)": {offsets: ""},
		"offset without a binlog file": {
			offsets: `{"offsets":[{"partition":{},"offset":{"snapshot":"true"}}]}`,
		},
		"source unreachable": {offsets: mysqlOffsets, measureErr: errors.New("dial tcp: i/o timeout"), measures: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			hh := newHorizonHarness(t, true, http.StatusOK,
				cdc.PositionHorizon{Remaining: time.Minute, Retention: time.Hour}, c.measureErr)
			hh.pipeline("stopped", "mysql", c.offsets)
			hh.sweep(t) // no alert, no resolve, no action expected
			if calls := hh.httpCalls(); len(calls) != 0 {
				t.Errorf("acted without a measurement: %q", calls)
			}
			if got := len(hh.measured) == 1; got != c.measures {
				t.Errorf("measured = %q, want measured %v", hh.measured, c.measures)
			}
		})
	}
}

func TestMySQLOffsetPosition(t *testing.T) {
	var offset map[string]interface{}
	if err := json.Unmarshal([]byte(`{"file":"mysql-bin.000002","pos":1234,"ts_sec":1759000000,"gtids":"a:1-5"}`), &offset); err != nil {
		t.Fatal(err)
	}
	pos, at := mysqlOffsetPosition(offset)
	if pos.File != "mysql-bin.000002" || pos.Pos != 1234 || pos.GTID != "a:1-5" {
		t.Errorf("position = %+v", pos)
	}
	if !at.Equal(time.Unix(1759000000, 0)) {
		t.Errorf("event time = %v", at)
	}
	if _, at := mysqlOffsetPosition(map[string]interface{}{"file": "mysql-bin.000002"}); !at.IsZero() {
		t.Errorf("an offset without ts_sec has event time %v, want zero", at)
	}
}

// The stored-position alert names its catalog code and carries its severity in the
// structured error, as the WAL alert it mirrors does. It was built by hand with neither,
// so the api-gateway titled it after its humanized type ("Cdc Position Retention") and
// filed it as info: a binlog about to be purged out from under a stopped pipeline read as
// a notice (KI-CDC-POSITION-RETENTION-NOTICE-RENDERS-AS-INFO). The top-level severity
// stays as it was, since the gateway's dedup compares it to push again a warning that
// turned critical.
func TestHorizonAlert_CarriesItsCodeAndItsSeverity(t *testing.T) {
	for _, tc := range []struct {
		sev          IssueSeverity
		wantEnvelope string
		wantRaw      string
	}{
		{IssueSeverityWarning, "warning", "warning"},
		{IssueSeverityCritical, "error", "critical"},
	} {
		t.Run(string(tc.sev), func(t *testing.T) {
			c, _, mock := episodeSentinels(t)
			sent := captureAlerts(t)
			mock.ExpectQuery(episodeUpsert).WillReturnRows(detectedAtRow())

			c.emitHorizonAlert(context.Background(), horizonAlert{
				row:         horizonRow{pipelineID: walTestPipeline, pipelineName: "orders", pipelineState: "stopped", connectionID: walTestConn, family: "mysql"},
				horizon:     cdc.PositionHorizon{Retention: 7 * 24 * time.Hour, Remaining: 2 * time.Hour},
				sev:         tc.sev,
				disposition: "stopped",
				remedy:      walRemedyAlertOnly,
			})

			if len(*sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(*sent))
			}
			n := (*sent)[0].notification
			if n["_code"] != "CDC_POSITION_RETENTION" {
				t.Errorf("_code = %v, want CDC_POSITION_RETENTION", n["_code"])
			}
			se := structuredErrorOf(t, n)
			if se.Code != "CDC_POSITION_RETENTION" || se.Audience != "user" {
				t.Errorf("structured error = %+v, want code CDC_POSITION_RETENTION for the user", se)
			}
			if se.Severity != tc.wantEnvelope {
				t.Errorf("structured error severity = %q, want %q", se.Severity, tc.wantEnvelope)
			}
			if n["severity"] != tc.wantRaw {
				t.Errorf("top-level severity = %v, want %q", n["severity"], tc.wantRaw)
			}
			if n["type"] != "cdc_position_retention" {
				t.Errorf("type = %v, want cdc_position_retention (its category is keyed on it too)", n["type"])
			}
		})
	}
}
