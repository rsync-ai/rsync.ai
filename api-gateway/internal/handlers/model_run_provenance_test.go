package handlers

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"go.temporal.io/api/serviceerror"
)

// failOnSwallowedError fails the test if the code under it logged a warning or worse.
//
// The skip writers log a failed statement and carry on, by design: a history row is not
// worth failing a rebuild over. That also means a statement the mock did not expect —
// a second row for a diamond's apex, a lookup past the chain bound — surfaces only as a
// log line, and ExpectationsWereMet still passes. Both of those mutations survived
// until this was added.
func failOnSwallowedError(t *testing.T) {
	t.Helper()
	std := logrus.StandardLogger()
	previous := std.ReplaceHooks(make(logrus.LevelHooks))
	hook := logrustest.NewLocal(std)
	t.Cleanup(func() {
		std.ReplaceHooks(previous)
		for _, e := range hook.AllEntries() {
			if e.Level <= logrus.WarnLevel {
				t.Errorf("a failure was logged and swallowed: %s %v", e.Message, e.Data)
			}
		}
	})
}

// Phase 1.1: a triggered run says what woke it (G2), a model that was deliberately not
// rebuilt gets a row saying why (G3, G7, G4), and a Temporal that just failed is not
// re-dialled on every hop (G11). Each test pins the statement or the decision, because
// the failure mode of every one of these was silence rather than an error.

const (
	provUpstream = "44444444-4444-4444-4444-444444444444"
	provRunA     = "a0000000-0000-0000-0000-00000000000a"
	provModelB   = "b0000000-0000-0000-0000-00000000000b"
	provModelC   = "c0000000-0000-0000-0000-00000000000c"
	provModelD   = "d0000000-0000-0000-0000-00000000000d"
	provRunB     = "b1000000-0000-0000-0000-00000000000b"
	provRunC     = "c1000000-0000-0000-0000-00000000000c"
	provRunD     = "d1000000-0000-0000-0000-00000000000d"
	provSchedule = "5c000000-0000-0000-0000-000000000005"
	provActor    = "ac000000-0000-0000-0000-0000000000ac"
)

var provHistoryInsert = `INSERT INTO saved_query_runs[\s\S]*upstream_kind, upstream_id, upstream_run_id, origin_execution_id, trigger_depth, coalesced_count[\s\S]*RETURNING run_id::text`

func TestRecordModelRunOutcome_ATriggeredRunRecordsWhatWokeItAndReturnsItsRow(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(provHistoryInsert).
		WithArgs(savedQueryID, provSchedule, string(triggerTriggered), "succeeded", "read", "analytics.b", sqlmock.AnyArg(), "",
			"", provActor, sqlmock.AnyArg(),
			upstreamKindModel, provUpstream, provRunA, "exec-1", int64(3), int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()

	got := recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "succeeded", StatementClass: "read", TargetTable: "analytics.b"},
		modelRunAudit{Trigger: triggerTriggered, ScheduleID: provSchedule, ActorID: provActor,
			Provenance: runProvenance{UpstreamKind: upstreamKindModel, UpstreamID: provUpstream,
				UpstreamRunID: provRunA, OriginExecutionID: "exec-1", Depth: 3, Coalesced: 2}})
	if got != provRunB {
		t.Errorf("returned run id %q, want %q: without it the models this run wakes cannot point back", got, provRunB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordModelRunOutcome_AManualRunCarriesNoProvenanceEvenIfGiven(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(provHistoryInsert).
		WithArgs(savedQueryID, "", string(triggerManual), "succeeded", "read", "", sqlmock.AnyArg(), "",
			"", provActor, sqlmock.AnyArg(),
			"", "", "", "", int64(0), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()

	recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "succeeded", StatementClass: "read"},
		modelRunAudit{Trigger: triggerManual, ActorID: provActor,
			Provenance: runProvenance{UpstreamKind: upstreamKindModel, UpstreamID: provUpstream, Depth: 4}})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a manual run must not claim an upstream woke it: %v", err)
	}
}

func TestRecordModelRunSkip_WritesASkippedTriggeredRowWithoutStampingTheBadge(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	// No UPDATE saved_queries expectation: sqlmock fails on any statement not expected,
	// so a skip that moved last_run_* would fail here.
	mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*'triggered', 'skipped', \$3[\s\S]*RETURNING run_id::text`).
		WithArgs(provModelB, provSchedule, skipUpstreamFailed, "", provActor,
			upstreamKindPipeline, provUpstream, "", "exec-7", int64(1), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))

	got := recordModelRunSkip(context.Background(), db.DB, provModelB, provSchedule, provActor, skipUpstreamFailed, "",
		provenanceFor(modelRefreshSource{Kind: upstreamKindPipeline, ID: provUpstream, ExecutionID: "exec-7"}, 0))
	if got != provRunB {
		t.Errorf("returned %q, want %q", got, provRunB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCascadeSkipReason_OnlyAStableStopTellsTheModelsBelow(t *testing.T) {
	cases := []struct {
		name string
		res  *modelRunResult
		want string
	}{
		{"no result", nil, ""},
		{"succeeded", &modelRunResult{Status: "succeeded"}, ""},
		{"failed", &modelRunResult{Status: "failed", Error: "boom"}, skipUpstreamFailed},
		{"blocked by a data check", &modelRunResult{Status: "failed", Error: modelCheckBlockedError, CheckBlocked: true}, skipUpstreamCheckFailed},
		{"refused and paused", &modelRunResult{Status: "skipped", AutoPauseReason: "identity"}, skipUpstreamSkipped},
		// Another run of the same model is in flight; it will wake the same consumers.
		{"already in progress", &modelRunResult{Status: "skipped", Error: "already in progress"}, ""},
	}
	for _, tc := range cases {
		if got := cascadeSkipReason(tc.res); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func expectDownstreamLookup(mock sqlmock.Sqlmock, upstream string, targets ...[2]string) {
	rows := sqlmock.NewRows([]string{"schedule_id", "saved_query_id", "run_as_user_id"})
	for _, tg := range targets {
		rows.AddRow(tg[0], tg[1], provActor)
	}
	mock.ExpectQuery(`WHERE u.upstream_saved_query_id = \$1::uuid[\s\S]*s.status = 'active'`).
		WithArgs(upstream).WillReturnRows(rows)
}

// The diamond A → {B, C} → D. B and C get A's reason and point at A's run; D gets
// upstream_skipped, points at B's skip row (the first path to reach it), and is written
// once even though C reaches it too.
func TestRecordSkippedDownstream_TheWholeSubtreeIsRecordedOnceAndLinkedHopByHop(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)
	skipInsert := `INSERT INTO saved_query_runs[\s\S]*'skipped'`

	expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB}, [2]string{"sc", provModelC})
	mock.ExpectQuery(skipInsert).
		WithArgs(provModelB, "sb", skipUpstreamFailed, "", provActor, upstreamKindModel, provUpstream, provRunA, "", int64(1), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectQuery(skipInsert).
		WithArgs(provModelC, "sc", skipUpstreamFailed, "", provActor, upstreamKindModel, provUpstream, provRunA, "", int64(1), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunC))
	expectDownstreamLookup(mock, provModelB, [2]string{"sd", provModelD})
	mock.ExpectQuery(skipInsert).
		WithArgs(provModelD, "sd", skipUpstreamSkipped, "", provActor, upstreamKindModel, provModelB, provRunB, "", int64(2), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunD))
	expectDownstreamLookup(mock, provModelC, [2]string{"sd", provModelD})
	expectDownstreamLookup(mock, provModelD)

	if err := recordSkippedDownstream(context.Background(), db.DB, modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, RunID: provRunA, Depth: 1},
		skipUpstreamFailed); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordSkippedDownstream_StopsAtTheChainBound(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	// Past the bound: no lookup at all, so a ring cannot make the cascade loop.
	if err := recordSkippedDownstream(context.Background(), db.DB, modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, Depth: maxTriggerChainDepth + 1},
		skipUpstreamFailed); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The other side of the bound: a hop exactly at it is still walked. Without this, a
// bound that stopped one hop early would pass the test above just as well.
func TestRecordSkippedDownstream_WalksAHopExactlyAtTheBound(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	expectDownstreamLookup(mock, provUpstream)
	if err := recordSkippedDownstream(context.Background(), db.DB, modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, Depth: maxTriggerChainDepth},
		skipUpstreamFailed); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The cascade's lookup error comes back to the caller. It used to be logged and dropped,
// which was all a goroutine could do; the /downstream endpoint answers 500 on it instead,
// so the workflow retries rather than reading "nothing below".
func TestRecordSkippedDownstream_ALookupErrorComesBack(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`WHERE u.upstream_saved_query_id = \$1::uuid`).WillReturnError(errors.New("connection reset"))
	if err := recordSkippedDownstream(context.Background(), db.DB, modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, RunID: provRunA, Depth: 1},
		skipUpstreamFailed); err == nil {
		t.Fatal("a lookup error was swallowed")
	}
	mock.ExpectQuery(`WHERE u.upstream_saved_query_id = \$1::uuid`).WillReturnError(errors.New("connection reset"))
	if err := recordChainDepthSkips(context.Background(), db.DB, modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, RunID: provRunA, Depth: maxTriggerChainDepth + 1}); err == nil {
		t.Fatal("a lookup error was swallowed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordChainDepthSkips_TheModelPastTheBoundGetsARowSayingSo(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	depth := maxTriggerChainDepth + 1
	expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB})
	mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*'skipped'`).
		WithArgs(provModelB, "sb", skipChainDepthExceeded, "the rebuild chain reached its limit of 8 hops", provActor,
			upstreamKindModel, provUpstream, provRunA, "", int64(depth), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))

	if err := recordChainDepthSkips(context.Background(), db.DB, modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, RunID: provRunA, Depth: depth}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

var unsatisfiedQuery = `WITH me AS[\s\S]*MAX\(r.started_at\)[\s\S]*s.upstream_policy = 'all'[\s\S]*SELECT COALESCE\(u.upstream_pipeline_id, u.upstream_saved_query_id\)::text,[\s\S]*ur.status = 'succeeded'[\s\S]*'PIPELINE_COMPLETED'\s+AND e.received_at > COALESCE\(me.baseline, '-infinity'::timestamptz\)[\s\S]*cdc_table_watermarks w[\s\S]*pipeline_sink_lag l[\s\S]*l.total_lag = 0\)\s+FROM me[\s\S]*IS DISTINCT FROM NULLIF\(\$2, ''\)::uuid`

// upstreamFactRows is the shape unsatisfiedUpstreams scans: one row of facts per upstream.
func upstreamFactRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "is_model", "baseline", "model_succeeded_at", "pipeline_completed_at",
		"is_cdc", "watermark_source_time", "sink_caught_up_at"})
}

func TestUnsatisfiedUpstreams_ReturnsTheStaleSiblingsExcludingTheOneFiring(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	baseline := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, provUpstream).
		WillReturnRows(upstreamFactRows().
			// A model upstream last rebuilt before the baseline: stale.
			AddRow(provModelB, true, baseline, baseline.Add(-time.Minute), nil, false, nil, nil).
			// A CDC pipeline whose sink drained after the baseline: fresh, so not listed.
			AddRow(provRunB, false, baseline, nil, nil, true, nil, baseline.Add(time.Minute)).
			// A batch pipeline that has not completed since: stale.
			AddRow(provRunA, false, baseline, nil, nil, false, nil, nil))
	// The stale model is a cron model, with no upstreams to watch: unknown, so it still waits.
	mock.ExpectQuery(ancestryQuery).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(ancestryRows().
			AddRow(provModelB, baseline.Add(-2*time.Minute), nil, false, nil, nil, false, nil))

	got, err := unsatisfiedUpstreams(context.Background(), db.DB, provSchedule, provUpstream)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != provModelB+","+provRunA {
		t.Errorf("got %v", got)
	}
	if msg := waitingOnMessage(got); msg != "waiting on 2 upstreams that have not rebuilt since this model's last build" {
		t.Errorf("message = %q", msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func internalRunRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/explorer/models/:id/run", RunSavedQueryModelInternal)
	return r
}

func expectEventSchedule(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM saved_query_schedules s[\s\S]*s.schedule_type = \$3`).
		WithArgs(savedQueryID, provSchedule, scheduleAfterUpstream).
		WillReturnRows(sqlmock.NewRows([]string{"schedule_id", "temporal_schedule_id", "run_as_user_id", "status"}).
			AddRow(provSchedule, nil, provActor, "active"))
}

// G4. A fan-in on 'all' with a stale sibling does not rebuild: it records why, and
// answers 200 so Temporal does not retry a decision.
func TestRunSavedQueryModelInternal_AFanInWaitingOnASiblingRecordsASkipInsteadOfRunning(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectEventSchedule(mock)
	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, provUpstream).
		WillReturnRows(upstreamFactRows().AddRow(provModelB, true, nil, nil, nil, false, nil, nil))
	mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*'skipped'`).
		WithArgs(savedQueryID, provSchedule, skipWaitingOnUpstreams,
			"waiting on 1 upstream that has not rebuilt since this model's last build", provActor,
			upstreamKindPipeline, provUpstream, "", "exec-1", int64(1), int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))

	w := doJSON(internalRunRouter(), http.MethodPost, "/internal/explorer/models/"+savedQueryID+"/run", map[string]any{
		"schedule_id": provSchedule, "trigger": scheduleAfterUpstream,
		"upstream_kind": upstreamKindPipeline, "upstream_id": provUpstream, "execution_id": "exec-1", "coalesced": 2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Status     string `json:"status"`
		SkipReason string `json:"skip_reason"`
		WaitingOn  int    `json:"waiting_on"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Status != "skipped" || body.SkipReason != skipWaitingOnUpstreams || body.WaitingOn != 1 {
		t.Errorf("body = %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a waiting fan-in must record a skip and run nothing: %v", err)
	}
}

// The policy check has a retry on this door, so a lookup error is a 500 rather than a
// guess. A malformed upstream id reaches the query as "" rather than failing its uuid
// cast on every retry.
func TestRunSavedQueryModelInternal_APolicyLookupErrorIsRetryableAndAMalformedIdIsNotBound(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectEventSchedule(mock)
	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, "").
		WillReturnError(errors.New("connection reset"))

	w := doJSON(internalRunRouter(), http.MethodPost, "/internal/explorer/models/"+savedQueryID+"/run", map[string]any{
		"schedule_id": provSchedule, "trigger": scheduleAfterUpstream,
		"upstream_kind": upstreamKindPipeline, "upstream_id": "not-a-uuid",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Version skew: an adapter sending a kind this gateway does not know loses the
// provenance, but the upstream that fired must still be excluded from the fan-in check,
// or it counts as stale against itself and an 'all' model never rebuilds.
func TestRunSavedQueryModelInternal_AnUnknownKindStillExcludesTheFiringUpstream(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectEventSchedule(mock)
	mock.ExpectQuery(unsatisfiedQuery).
		WithArgs(provSchedule, provUpstream).
		WillReturnError(errors.New("stop here"))

	w := doJSON(internalRunRouter(), http.MethodPost, "/internal/explorer/models/"+savedQueryID+"/run", map[string]any{
		"schedule_id": provSchedule, "trigger": scheduleAfterUpstream,
		"upstream_kind": "dataset", "upstream_id": provUpstream,
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected the stubbed lookup error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the firing upstream was not excluded: %v", err)
	}
}

// G11. One failed start opens the breaker for the window, inside which no completion
// dials; once the window passes a start goes through again, and Temporal answering —
// success or already started — closes it.
func TestFanOutBreaker_OneFailureStopsTheDialsForTheWindowOnly(t *testing.T) {
	fastFanOutRetry(t)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	b := &fanOutBreaker{window: time.Minute, now: func() time.Time { return now }}

	resolves := 0
	var next error
	resolve := func() upstreamFanOutDispatch {
		resolves++
		return func(context.Context, upstreamFanOut) error { return next }
	}
	start := func() error { return startUpstreamFanOut(context.Background(), b, resolve, upstreamFanOut{}) }

	if err := start(); err != nil {
		t.Fatalf("a fresh breaker: %v", err)
	}
	next = errors.New("context deadline exceeded")
	if err := start(); err == nil {
		t.Fatal("a start that kept failing returned nil")
	}
	now = now.Add(30 * time.Second)
	before := resolves
	if err := start(); !errors.Is(err, errFanOutBreakerOpen) {
		t.Fatalf("inside the window: %v, want errFanOutBreakerOpen", err)
	}
	if resolves != before {
		t.Errorf("resolved the dispatcher %d times inside the window: an open breaker must not even build a client", resolves-before)
	}

	now = now.Add(31 * time.Second)
	next = serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "")
	if err := start(); err != nil {
		t.Fatalf("the window passed and Temporal answered already-started: %v", err)
	}
	if !b.until.IsZero() {
		t.Error("an already-started answer left the breaker open")
	}

	next = errors.New("unavailable")
	_ = start()
	next = nil
	now = now.Add(2 * time.Minute)
	if err := start(); err != nil || !b.until.IsZero() {
		t.Errorf("a success after the window must close the breaker (err=%v, until=%v)", err, b.until)
	}
}

// When the window passes, one start probes Temporal and every other completion is refused
// until it answers: a burst must not all dial a Temporal that is still down, each holding
// the client mutex for the dial deadline.
func TestFanOutBreaker_AfterTheWindowOnlyOneProbeDials(t *testing.T) {
	fastFanOutRetry(t)
	var mu sync.Mutex
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	b := &fanOutBreaker{window: time.Minute, now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	_ = startUpstreamFanOut(context.Background(), b,
		resolveTo(func(context.Context, upstreamFanOut) error { return errors.New("unavailable") }), upstreamFanOut{})
	advance(61 * time.Second)

	dialling, release, probeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		probeDone <- startUpstreamFanOut(context.Background(), b, resolveTo(func(context.Context, upstreamFanOut) error {
			close(dialling)
			<-release
			return nil
		}), upstreamFanOut{})
	}()
	<-dialling
	for i := 0; i < 3; i++ {
		resolves := 0
		err := startUpstreamFanOut(context.Background(), b, func() upstreamFanOutDispatch { resolves++; return nil }, upstreamFanOut{})
		if !errors.Is(err, errFanOutBreakerOpen) || resolves != 0 {
			t.Fatalf("a second start while the probe was out: err=%v, resolves=%d", err, resolves)
		}
	}
	close(release)
	if err := <-probeDone; err != nil {
		t.Fatalf("the probe: %v", err)
	}
	if !b.acquire() {
		t.Fatal("a probe that got through left the breaker open")
	}
}

// A probe that never answers is given up on after a window, so a lost one cannot keep
// Temporal bypassed for good.
func TestFanOutBreaker_AProbeThatNeverAnswersIsGivenUpOn(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	b := &fanOutBreaker{window: time.Minute, now: func() time.Time { return now }, until: now.Add(-time.Second)}
	if !b.acquire() {
		t.Fatal("no probe after the window")
	}
	if b.acquire() {
		t.Fatal("a second probe while the first is out")
	}
	now = now.Add(61 * time.Second)
	if !b.acquire() {
		t.Fatal("a probe that never answered kept Temporal bypassed")
	}
}

// No client is a failed start, not a released probe. getTemporalClient returns nil when
// its dial fails, after holding the client mutex for the dial's deadline; releasing the
// probe would let the very next completion pay that again.
func TestFanOutBreaker_NoClientReopensTheWindow(t *testing.T) {
	fastFanOutRetry(t)
	b := closedBreaker()
	if err := startUpstreamFanOut(context.Background(), b, func() upstreamFanOutDispatch { return nil }, upstreamFanOut{}); err == nil {
		t.Fatal("no client returned nil")
	}
	if b.acquire() {
		t.Fatal("no client left the breaker closed")
	}
}

func TestListSavedQueryRuns_ReturnsProvenanceAndNamesOnlyAVisibleUpstream(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`FROM saved_queries r\s+JOIN workspace_members`).
		WithArgs(savedQueryID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("viewer"))
	mock.ExpectQuery(`FROM saved_queries\s+WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(savedQueryID, wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(1))
	ts := time.Date(2026, 9, 16, 8, 50, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM saved_query_runs r[\s\S]*LEFT JOIN pipelines up ON r.upstream_kind = 'pipeline'[\s\S]*up.workspace_id = sq.workspace_id[\s\S]*um.workspace_id = sq.workspace_id[\s\S]*\(um.visibility = 'workspace' OR um.created_by = \$2\)`).
		WithArgs(savedQueryID, wsScopeUser, "", nil, nil, defaultRunPageSize+1).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "saved_query_id", "schedule_id", "trigger_source", "status",
			"statement_class", "target_table", "rows_affected", "error", "auto_pause_reason", "started_at", "finished_at",
			"ran_as_user_id", "upstream_kind", "upstream_id", "upstream_name", "upstream_run_id", "origin_execution_id",
			"trigger_depth", "coalesced_count", "skip_reason"}).
			AddRow(provRunB, savedQueryID, provSchedule, "triggered", "skipped", "", "", nil, "waiting on 1 upstream that has not rebuilt since this model's last build", "",
				ts, ts, provActor, "model", provUpstream, "orders_daily", provRunA, "exec-1", 2, 3, skipWaitingOnUpstreams))

	r := savedQueryRouter(http.MethodGet, "/explorer/saved/:id/runs", "viewer", ListSavedQueryRuns)
	w := doJSON(r, http.MethodGet, "/explorer/saved/"+savedQueryID+"/runs", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Runs []SavedQueryRun `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Runs) != 1 {
		t.Fatalf("body = %s", w.Body.String())
	}
	got := body.Runs[0]
	if got.UpstreamName != "orders_daily" || got.UpstreamRunID != provRunA || got.OriginExecutionID != "exec-1" ||
		got.TriggerDepth != 2 || got.CoalescedCount != 3 || got.SkipReason != skipWaitingOnUpstreams {
		t.Errorf("run = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// elapsedMicrosArg matches the started_at argument: the attempt's elapsed time in
// microseconds, which the insert subtracts from the DB's NOW().
type elapsedMicrosArg struct{ min, max int64 }

func (a elapsedMicrosArg) Match(v driver.Value) bool {
	n, ok := v.(int64)
	return ok && n >= a.min && n <= a.max
}

// started_at is stamped on the DB clock, like finished_at and the upstream events the
// fan-in policy compares it with; a gateway clock ahead of the DB would otherwise make a
// build look newer than completions that landed after it.
func TestRecordModelRunOutcome_StartedAtIsOnTheDatabaseClock(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*NOW\(\) - \(\$11::bigint \* interval '1 microsecond'\), NOW\(\)`).
		WithArgs(savedQueryID, "", string(triggerManual), "succeeded", "read", "", sqlmock.AnyArg(), "",
			"", provActor, elapsedMicrosArg{min: 2_000_000, max: 60_000_000},
			"", "", "", "", int64(0), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()

	recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "succeeded", StatementClass: "read"},
		modelRunAudit{Trigger: triggerManual, ActorID: provActor, StartedAt: time.Now().Add(-2 * time.Second)})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A start time in the future (a clock step between start and finish) must not produce a
// started_at after finished_at.
func TestRecordModelRunOutcome_FutureStartClampsToZeroElapsed(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO saved_query_runs`).
		WithArgs(savedQueryID, "", string(triggerManual), "succeeded", "read", "", sqlmock.AnyArg(), "",
			"", provActor, int64(0),
			"", "", "", "", int64(0), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectCommit()

	recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "succeeded", StatementClass: "read"},
		modelRunAudit{Trigger: triggerManual, ActorID: provActor, StartedAt: time.Now().Add(time.Hour)})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
