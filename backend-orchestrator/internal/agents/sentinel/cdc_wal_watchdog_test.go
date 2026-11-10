package sentinel

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A CDC pipeline the user stopped keeps its replication slot: the slot is where Start
// resumes from, and the user did nothing to give it up. WAL pressure must not take it
// away either. Past CRITICAL the watchdog STARTS the stopped pipeline so it consumes the
// slot again, RESUMES a paused one, and drops only the slot of a pipeline that no longer
// exists. Before this, a stopped pipeline's slot was dropped at CRITICAL and every change
// made while it was stopped was lost to a Reload (KI-AUTONOMY-INVERTED-DEFAULTS).

const (
	walTestSlot     = "debezium_slot_orders"
	walTestConn     = "11111111-2222-3333-4444-555555555555"
	walTestPipeline = "abcdef12-3456-7890-abcd-ef1234567890"
	walGB           = int64(1024 * 1024 * 1024)
)

// walHarness is one watchdog sweep against a mocked metadata DB, a mocked source
// PostgreSQL, and fake api-gateway / Kafka Connect servers that record every call.
type walHarness struct {
	s       *CDCSentinel
	meta    sqlmock.Sqlmock
	src     sqlmock.Sqlmock
	mu      sync.Mutex
	queries []string // every SQL statement the metadata DB saw
	calls   []string // every HTTP call, as "METHOD path secret=<header>"
}

func newWALHarness(t *testing.T, autoAct bool, httpStatus int) *walHarness {
	t.Helper()
	h := &walHarness{}
	record := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		h.mu.Lock()
		h.queries = append(h.queries, actual)
		h.mu.Unlock()
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	metaDB, meta, err := sqlmock.New(sqlmock.QueryMatcherOption(record))
	if err != nil {
		t.Fatal(err)
	}
	srcDB, src, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.calls = append(h.calls, r.Method+" "+r.URL.Path+" secret="+r.Header.Get("X-Internal-Secret"))
		h.mu.Unlock()
		w.WriteHeader(httpStatus)
	}))
	t.Cleanup(func() { srv.Close(); metaDB.Close() })
	t.Setenv("INTERNAL_SERVICE_SECRET", "s2s-test-secret")

	h.meta, h.src = meta, src
	h.s = &CDCSentinel{
		db:               metaDB,
		httpClient:       srv.Client(),
		connectURL:       srv.URL,
		apiGatewayURL:    srv.URL,
		walWarnBytes:     512 * 1024 * 1024,
		walCriticalBytes: 2 * walGB,
		walAutoAct:       autoAct,
		walOpenSource: func(context.Context, string) (*sql.DB, error) {
			return srcDB, nil
		},
	}
	return h
}

// slot queues the watchdog's slot listing (one slot) and the source's measurement of it.
// state "" with pipelineID "" is a deleted pipeline (LEFT JOIN miss, pipeline_id NULL).
func (h *walHarness) slot(pipelineID, state string, retained int64) {
	var pid interface{}
	if pipelineID != "" {
		pid = pipelineID
	}
	h.meta.ExpectQuery(`FROM cdc_resources cr`).WillReturnRows(
		sqlmock.NewRows([]string{"resource_name", "connection_id", "pipeline_id", "status", "name"}).
			AddRow(walTestSlot, walTestConn, pid, state, "orders"))
	h.src.ExpectQuery(`pg_replication_slots`).WithArgs(walTestSlot).WillReturnRows(
		sqlmock.NewRows([]string{"retained", "active"}).AddRow(retained, false))
}

// alert expects the sentinel_active_issues upsert and captures its description + metadata.
func (h *walHarness) alert(desc, meta *string) {
	h.meta.ExpectQuery(`INSERT INTO sentinel_active_issues`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), capture{desc}, capture{meta}).
		WillReturnRows(detectedAtRow())
}

func (h *walHarness) budget(used int) {
	h.meta.ExpectQuery(`FROM pipeline_run_events`).WithArgs(walTestPipeline, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(used))
}

func (h *walHarness) recordsAutoStart(eventType string) {
	h.meta.ExpectExec(`INSERT INTO pipeline_run_events`).
		WithArgs(walTestPipeline, sqlmock.AnyArg(), eventType, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func (h *walHarness) sweep(t *testing.T) {
	t.Helper()
	h.s.checkWALRetention(context.Background())
	if err := h.meta.ExpectationsWereMet(); err != nil {
		t.Errorf("metadata DB: %v\nqueries seen:\n  %s", err, strings.Join(h.queries, "\n  "))
	}
	if err := h.src.ExpectationsWereMet(); err != nil {
		t.Errorf("source DB: %v", err)
	}
}

// neverDropsALiveSlot fails if the sweep asked for any slot of an existing pipeline to be
// reaped. The reaper's SQL is the only way the watchdog drops a slot, so a query that
// selects on pipeline status is the drop of a live pipeline's resume position.
func (h *walHarness) neverDropsALiveSlot(t *testing.T) {
	t.Helper()
	for _, q := range h.queries {
		if strings.Contains(q, "p.status = 'stopped'") || strings.Contains(q, "DROP") {
			t.Errorf("watchdog reached for the slot of a pipeline that still exists:\n%s", q)
		}
	}
}

func (h *walHarness) httpCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

type capture struct{ into *string }

func (c capture) Match(v driver.Value) bool {
	if c.into != nil {
		switch x := v.(type) {
		case string:
			*c.into = x
		case []byte:
			*c.into = string(x)
		}
	}
	return true
}

func walAction(t *testing.T, meta string) string {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("alert metadata is not JSON: %v (%q)", err, meta)
	}
	a, _ := m["action"].(string)
	return a
}

func TestWALWatchdog_StoppedPipelinePastCriticalIsStartedNotDropped(t *testing.T) {
	for _, state := range []string{"stopped", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			h := newWALHarness(t, true, http.StatusOK)
			var desc, meta string
			h.slot(walTestPipeline, state, 3*walGB)
			h.budget(0)
			h.recordsAutoStart("wal_watchdog_auto_start")
			h.alert(&desc, &meta)
			h.sweep(t)
			h.neverDropsALiveSlot(t)

			want := "POST /api/v1/internal/pipelines/" + walTestPipeline + "/run secret=s2s-test-secret"
			if got := h.httpCalls(); len(got) != 1 || got[0] != want {
				t.Fatalf("HTTP calls = %q, want exactly [%q]", got, want)
			}
			if a := walAction(t, meta); a != string(walRemedyStart) {
				t.Errorf("alert action = %q, want %q", a, walRemedyStart)
			}
			if !strings.Contains(desc, "started") || strings.Contains(strings.ToLower(desc), "dropping") {
				t.Errorf("alert does not say the pipeline was started: %q", desc)
			}
		})
	}
}

func TestWALWatchdog_PausedPipelinePastCriticalIsResumed(t *testing.T) {
	h := newWALHarness(t, true, http.StatusAccepted)
	var desc, meta string
	h.slot(walTestPipeline, "paused", 3*walGB)
	h.budget(0)
	h.meta.ExpectExec(`UPDATE pipelines SET status = 'running'.*status = 'paused'`).
		WithArgs(walTestPipeline).WillReturnResult(sqlmock.NewResult(0, 1))
	h.recordsAutoStart("wal_watchdog_auto_resume")
	h.alert(&desc, &meta)
	h.sweep(t)
	h.neverDropsALiveSlot(t)

	want := "PUT /connectors/cdc-abcdef12/resume secret="
	if got := h.httpCalls(); len(got) != 1 || got[0] != want {
		t.Fatalf("HTTP calls = %q, want exactly [%q]", got, want)
	}
	if a := walAction(t, meta); a != string(walRemedyResume) {
		t.Errorf("alert action = %q, want %q", a, walRemedyResume)
	}
}

// A deleted pipeline's slot has nothing left to resume, so it is the one slot the
// watchdog drops — through the reaper, which selects deleted pipelines only.
func TestWALWatchdog_DeletedPipelineSlotIsTheOnlyOneDropped(t *testing.T) {
	h := newWALHarness(t, true, http.StatusOK)
	var desc, meta string
	h.slot("", "", 3*walGB)
	h.alert(&desc, &meta)
	h.meta.ExpectQuery(`cr.pipeline_id IS NULL OR p.id IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // already reaped by the reconciler
	// A slot with no pipeline files its alert under the slot name, so there is no owner
	// to tell it cleared.
	h.meta.ExpectQuery(`DELETE FROM sentinel_active_issues WHERE id = \$1 RETURNING component_id, detected_at`).
		WithArgs("cdc-wal-" + walTestSlot).
		WillReturnRows(sqlmock.NewRows([]string{"component_id", "detected_at"}).AddRow(walTestSlot, time.Now()))
	h.sweep(t)
	h.neverDropsALiveSlot(t)

	if got := h.httpCalls(); len(got) != 0 {
		t.Fatalf("a deleted pipeline cannot be started, yet the watchdog called %q", got)
	}
	if a := walAction(t, meta); a != string(walRemedyDropOrphan) {
		t.Errorf("alert action = %q, want %q", a, walRemedyDropOrphan)
	}
}

func TestWALWatchdog_RunningPipelineIsOnlyAlerted(t *testing.T) {
	h := newWALHarness(t, true, http.StatusOK)
	var desc, meta string
	h.slot(walTestPipeline, "running", 3*walGB)
	h.alert(&desc, &meta)
	h.sweep(t)
	h.neverDropsALiveSlot(t)
	if got := h.httpCalls(); len(got) != 0 {
		t.Fatalf("running pipeline: HTTP calls = %q, want none", got)
	}
	if a := walAction(t, meta); a != string(walRemedyAlertOnly) {
		t.Errorf("alert action = %q, want %q", a, walRemedyAlertOnly)
	}
}

// The sentinel itself marks a pipeline 'stopped' when its connector fails for good, so an
// auto-start can land on a pipeline that will fail again. The budget stops a start loop;
// past it the slot is still kept and the alert tells the user what to do.
func TestWALWatchdog_AutoStartBudgetSpentKeepsSlotAndSaysSo(t *testing.T) {
	h := newWALHarness(t, true, http.StatusOK)
	var desc, meta string
	h.slot(walTestPipeline, "stopped", 3*walGB)
	h.budget(walAutoStartCap)
	h.alert(&desc, &meta)
	h.sweep(t)
	h.neverDropsALiveSlot(t)
	if got := h.httpCalls(); len(got) != 0 {
		t.Fatalf("budget spent: HTTP calls = %q, want none", got)
	}
	if !strings.Contains(desc, "kept") {
		t.Errorf("alert must say the slot is kept: %q", desc)
	}
}

// A refused start (plan gate 402, gateway down) records nothing and drops nothing.
func TestWALWatchdog_RefusedStartRecordsNothingDropsNothing(t *testing.T) {
	h := newWALHarness(t, true, http.StatusPaymentRequired)
	var desc, meta string
	h.slot(walTestPipeline, "stopped", 3*walGB)
	h.budget(0)
	h.alert(&desc, &meta)
	h.sweep(t)
	h.neverDropsALiveSlot(t)
	if !strings.Contains(desc, "402") || !strings.Contains(desc, "kept") {
		t.Errorf("alert must carry the refusal and say the slot is kept: %q", desc)
	}
}

// A budget that cannot be read is not a budget with room in it.
func TestWALWatchdog_UnreadableBudgetDoesNotStart(t *testing.T) {
	h := newWALHarness(t, true, http.StatusOK)
	var desc, meta string
	h.slot(walTestPipeline, "stopped", 3*walGB)
	h.meta.ExpectQuery(`FROM pipeline_run_events`).WillReturnError(sql.ErrConnDone)
	h.alert(&desc, &meta)
	h.sweep(t)
	h.neverDropsALiveSlot(t)
	if got := h.httpCalls(); len(got) != 0 {
		t.Fatalf("unknown budget: HTTP calls = %q, want none", got)
	}
	if !strings.Contains(desc, "kept") {
		t.Errorf("alert must say the slot is kept: %q", desc)
	}
}

// One pipeline can own several slots (one per source database); one sweep starts it once.
func TestWALWatchdog_PipelineWithTwoSlotsIsStartedOnce(t *testing.T) {
	h := newWALHarness(t, true, http.StatusOK)
	var desc1, meta1, desc2, meta2 string
	h.meta.ExpectQuery(`FROM cdc_resources cr`).WillReturnRows(
		sqlmock.NewRows([]string{"resource_name", "connection_id", "pipeline_id", "status", "name"}).
			AddRow(walTestSlot, walTestConn, walTestPipeline, "stopped", "orders").
			AddRow(walTestSlot+"_2", walTestConn, walTestPipeline, "stopped", "orders"))
	h.src.ExpectQuery(`pg_replication_slots`).WithArgs(walTestSlot).WillReturnRows(
		sqlmock.NewRows([]string{"retained", "active"}).AddRow(3*walGB, false))
	h.budget(0)
	h.recordsAutoStart("wal_watchdog_auto_start")
	h.alert(&desc1, &meta1)
	h.src.ExpectQuery(`pg_replication_slots`).WithArgs(walTestSlot + "_2").WillReturnRows(
		sqlmock.NewRows([]string{"retained", "active"}).AddRow(3*walGB, false))
	h.alert(&desc2, &meta2)
	h.sweep(t)
	h.neverDropsALiveSlot(t)
	if got := h.httpCalls(); len(got) != 1 {
		t.Fatalf("HTTP calls = %q, want exactly one start", got)
	}
	if !strings.Contains(desc2, "started") {
		t.Errorf("second slot's alert must report the same start: %q", desc2)
	}
}

func TestWALWatchdog_AutoActOffOnlyAlerts(t *testing.T) {
	h := newWALHarness(t, false, http.StatusOK)
	var desc, meta string
	h.slot(walTestPipeline, "stopped", 3*walGB)
	h.alert(&desc, &meta)
	h.sweep(t)
	h.neverDropsALiveSlot(t)
	if got := h.httpCalls(); len(got) != 0 {
		t.Fatalf("auto-act off: HTTP calls = %q, want none", got)
	}
}

// The warning tier only alerts, and tells the user what CRITICAL will do.
func TestWALWatchdog_WarningOnStoppedPipelineSaysItWillBeStarted(t *testing.T) {
	h := newWALHarness(t, true, http.StatusOK)
	var desc, meta string
	h.slot(walTestPipeline, "stopped", 1*walGB)
	h.alert(&desc, &meta)
	h.sweep(t)
	if got := h.httpCalls(); len(got) != 0 {
		t.Fatalf("warning tier: HTTP calls = %q, want none", got)
	}
	if !strings.Contains(desc, "start the pipeline automatically") {
		t.Errorf("warning must say CRITICAL starts the pipeline: %q", desc)
	}
}

// The class: whatever state a pipeline is in — including states added after this test
// was written — its slot is never the watchdog's to drop while the pipeline exists.
func TestWALWatchdog_NoExistingPipelineStateLeadsToADrop(t *testing.T) {
	states := []string{"running", "active", "paused", "stopped", "failed", "cancelled",
		"completed", "pending", "waiting_for_user", "draft", "some_future_state"}
	for _, st := range states {
		row := walSlotRow{pipelineID: sql.NullString{String: walTestPipeline, Valid: true}, pipelineState: st}
		if r := walCriticalRemedy(walDisposition(row), true); r == walRemedyDropOrphan {
			t.Errorf("state %q: watchdog would drop the slot of an existing pipeline", st)
		}
	}
	gone := walSlotRow{}
	if r := walCriticalRemedy(walDisposition(gone), true); r != walRemedyDropOrphan {
		t.Errorf("deleted pipeline: remedy %q, want %q", r, walRemedyDropOrphan)
	}
}

// TestWALAlert_CarriesItsCodeAndItsSeverity: the WAL alert's headline and severity come
// from its catalog code and structured error, as every other pushed sentinel alert's do.
// It was built by hand with neither, so the api-gateway humanized the event type into the
// headline ("Cdc Wal Pressure") and fell back to the type's default severity, info: a slot
// about to fill the source's disk read as a notice
// (KI-CDC-WAL-PRESSURE-NOTICE-RENDERS-AS-INFO). The top-level severity stays as it was,
// since the gateway's dedup compares it to push again a warning that turned critical.
func TestWALAlert_CarriesItsCodeAndItsSeverity(t *testing.T) {
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

			c.emitWALAlert(context.Background(), walSlotRow{slotName: walTestSlot, connectionID: walTestConn,
				pipelineID: sql.NullString{String: walTestPipeline, Valid: true}, pipelineState: "running", pipelineName: "orders"},
				walAlert{retained: 5 * walGB, sev: tc.sev, disposition: "running", remedy: walRemedyAlertOnly})

			if len(*sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(*sent))
			}
			n := (*sent)[0].notification
			raw, ok := n["error"]
			if !ok {
				t.Fatal("no structured error: the gateway has no code to find the headline under and no severity but info")
			}
			b, err := json.Marshal(raw)
			if err != nil {
				t.Fatalf("marshal structured error: %v", err)
			}
			var se struct {
				Code     string `json:"code"`
				Severity string `json:"severity"`
				Audience string `json:"audience"`
			}
			if err := json.Unmarshal(b, &se); err != nil {
				t.Fatalf("unmarshal structured error: %v", err)
			}
			if se.Code != "CDC_WAL_PRESSURE" || se.Audience != "user" {
				t.Errorf("structured error = %+v, want code CDC_WAL_PRESSURE for the user", se)
			}
			if se.Severity != tc.wantEnvelope {
				t.Errorf("structured error severity = %q, want %q", se.Severity, tc.wantEnvelope)
			}
			if n["severity"] != tc.wantRaw {
				t.Errorf("top-level severity = %v, want %q", n["severity"], tc.wantRaw)
			}
			if n["type"] != "cdc_wal_pressure" {
				t.Errorf("type = %v, want cdc_wal_pressure (its category is keyed on it too)", n["type"])
			}
		})
	}
}
