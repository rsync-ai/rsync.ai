package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"go.temporal.io/api/serviceerror"
)

// ============================================================================
// The next hop moves to the workflow (workflows-design.md § 4.2)
// ============================================================================
// A model run used to wake the models below it from inside the run request, on a
// goroutine: a gateway restart between the history row and the fan-out lost the hop
// with nothing recorded. The run's own workflow now asks for the hop (next_hop
// "caller"), gets the run row back, and lists the models below through /downstream.

// The run endpoint leaves the hop to the workflow only when the workflow asked for it
// AND there is a run row to hand back. Without the row the workflow could not list the
// models below, so the gateway keeps the hop rather than drop it.
func TestLeavesHopToCaller_OnlyWhenAskedAndThereIsARowToHandBack(t *testing.T) {
	for _, tc := range []struct {
		nextHop, runID string
		want           bool
	}{
		{modelNextHopCaller, provRunA, true},
		{modelNextHopCaller, "", false},
		{"", provRunA, false},
		{"gateway", provRunA, false},
	} {
		if got := leavesHopToCaller(internalModelRunRequest{NextHop: tc.nextHop}, tc.runID); got != tc.want {
			t.Errorf("leavesHopToCaller(next_hop %q, run %q) = %v, want %v", tc.nextHop, tc.runID, got, tc.want)
		}
	}
}

// The wire contract with the adapter's modelRunActivityResult: the run's fields stay
// where they were, and run_id and next_hop sit beside them. Without the echo the
// adapter cannot tell this gateway from one that predates the field and fired the hop
// itself, and it would either hop twice or not at all.
func TestModelRunHopResult_TheRunRowAndTheEchoSitBesideTheResult(t *testing.T) {
	raw, err := json.Marshal(modelRunHopResult{
		modelRunResult: &modelRunResult{Status: "succeeded", StatementClass: "read", TargetTable: "public.t"},
		RunID:          provRunA,
		NextHop:        modelNextHopCaller,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"status": "succeeded", "statement_class": "read", "target_table": "public.t",
		"run_id": provRunA, "next_hop": modelNextHopCaller,
	}
	if len(got) != len(want) {
		t.Errorf("body %s has %d keys, want %d", raw, len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("body[%q] = %v, want %v (body %s)", k, got[k], v, raw)
		}
	}
}

// The end of an internal run either hands the hop back or takes it, never both and
// never neither: both would wake the models below twice, neither would lose the hop.
func TestAnswerInternalModelRun_HandsTheHopBackOrTakesItNeverBoth(t *testing.T) {
	type fired struct {
		id, runID, exec string
		depth           int
	}
	for _, tc := range []struct {
		name     string
		req      internalModelRunRequest
		runID    string
		wantFire bool
	}{
		{"the workflow asked and there is a row", internalModelRunRequest{NextHop: modelNextHopCaller, ExecutionID: "exec-1", Depth: 2}, provRunA, false},
		{"an adapter that predates next_hop", internalModelRunRequest{ExecutionID: "exec-1", Depth: 2}, provRunA, true},
		{"no row to hand back", internalModelRunRequest{NextHop: modelNextHopCaller, ExecutionID: "exec-1", Depth: 2}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []fired
			prev := fireAfterInternalRun
			fireAfterInternalRun = func(_ *modelRunResult, id, runID, exec string, depth int) {
				calls = append(calls, fired{id, runID, exec, depth})
			}
			t.Cleanup(func() { fireAfterInternalRun = prev })

			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			answerInternalModelRun(c, tc.req, &modelRunResult{Status: "succeeded", StatementClass: "read"}, provUpstream, tc.runID)

			if w.Code != http.StatusOK {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", w.Body.String())
			}
			handedBack := body["next_hop"] == modelNextHopCaller && body["run_id"] == tc.runID
			if tc.wantFire {
				if len(calls) != 1 || calls[0] != (fired{provUpstream, tc.runID, "exec-1", 2}) {
					t.Errorf("fired %v, want one hop from the gateway with the run's chain", calls)
				}
				if _, echoed := body["next_hop"]; echoed {
					t.Errorf("fired here AND echoed next_hop, so the workflow hops too: %s", w.Body.String())
				}
			} else {
				if len(calls) != 0 {
					t.Errorf("handed the hop back AND fired it here: %v", calls)
				}
				if !handedBack {
					t.Errorf("body %s does not hand the run back to the workflow", w.Body.String())
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// POST /internal/explorer/models/:id/downstream
// ----------------------------------------------------------------------------

func downstreamRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/explorer/models/:id/downstream", ListSavedQueryModelDownstreamInternal)
	return r
}

func downstreamPath(modelID string) string {
	return "/internal/explorer/models/" + modelID + "/downstream"
}

var runRowQuery = `SELECT status, COALESCE\(auto_pause_reason, ''\), COALESCE\(error, ''\) = \$3\s+FROM saved_query_runs\s+WHERE run_id = \$1::uuid AND saved_query_id = \$2::uuid`

var skipsAlreadyRecordedQuery = `SELECT EXISTS \(\s*SELECT 1 FROM saved_query_runs\s+WHERE upstream_run_id = \$1::uuid AND status = 'skipped'\s*\)`

func expectRunRow(mock sqlmock.Sqlmock, status, autoPause string) {
	expectRunRowBlocked(mock, status, autoPause, false)
}

func expectRunRowBlocked(mock sqlmock.Sqlmock, status, autoPause string, blocked bool) {
	mock.ExpectQuery(runRowQuery).WithArgs(provRunA, provUpstream, modelCheckBlockedError).
		WillReturnRows(sqlmock.NewRows([]string{"status", "auto_pause_reason", "check_blocked"}).AddRow(status, autoPause, blocked))
}

func decodeFanOut(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	return got
}

// A succeeded run answers with the fan-out to start, in the adapter's
// UpstreamFanOutInput shape: one hop deeper, the run as the upstream's occurrence, the
// chain's execution carried down, and the schedule of each model below.
func TestDownstream_ASucceededRunAnswersWithTheFanOutToStart(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	expectRunRow(mock, "succeeded", "")
	expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB}, [2]string{"sc", provModelC})

	w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream),
		map[string]any{"run_id": provRunA, "execution_id": "exec-1", "depth": 2})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	got := decodeFanOut(t, w.Body.Bytes())
	for k, v := range map[string]any{
		"upstream_kind": upstreamKindModel, "upstream_id": provUpstream, "upstream_run_id": provRunA,
		"execution_id": "exec-1", "depth": float64(3),
	} {
		if got[k] != v {
			t.Errorf("fan-out[%q] = %v, want %v (body %s)", k, got[k], v, w.Body.String())
		}
	}
	targets, _ := got["targets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want both models below", got["targets"])
	}
	first, _ := targets[0].(map[string]any)
	if first["schedule_id"] != "sb" || first["saved_query_id"] != provModelB {
		t.Errorf("first target = %v, want schedule sb for model B", first)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The gateway's own fan-out and the workflow's are one wire shape, built by one
// function: two hand-written maps would drift, and the adapter decodes both.
func TestUpstreamFanOutArgs_IsTheShapeBothPathsSend(t *testing.T) {
	got := upstreamFanOutArgs(upstreamFanOut{
		Source:  modelRefreshSource{Kind: upstreamKindModel, ID: "m0", RunID: "r0", ExecutionID: "e0", Depth: 2},
		Targets: []modelRefreshTarget{{ScheduleID: "s1", ModelID: "m1", RunAsUserID: "never-sent"}},
	})
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "never-sent") {
		t.Errorf("run_as_user_id reached the workflow argument: %s", raw)
	}
	want := `{"depth":2,"execution_id":"e0","targets":[{"saved_query_id":"m1","schedule_id":"s1"}],"upstream_id":"m0","upstream_kind":"model","upstream_run_id":"r0"}`
	if string(raw) != want {
		t.Errorf("args = %s, want %s", raw, want)
	}
}

func TestDownstream_NothingBelowAnswersNoTargets(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	expectRunRow(mock, "succeeded", "")
	expectDownstreamLookup(mock, provUpstream)

	w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream), map[string]any{"run_id": provRunA})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if targets, _ := decodeFanOut(t, w.Body.Bytes())["targets"].([]any); len(targets) != 0 {
		t.Errorf("targets = %v, want none", targets)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A run that failed wakes nothing and records why below it — the same cascade the
// gateway used to write from the run request (G3), now written where the workflow can
// retry it.
func TestDownstream_AFailedRunRecordsTheSkipsBelowItAndStartsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, status, autoPause string
		blocked                 bool
		reason                  string
	}{
		{"failed", "failed", "", false, skipUpstreamFailed},
		{"skipped", "skipped", "the run-as user is no longer a member", false, skipUpstreamSkipped},
		// A data check held the upstream's new table back (saved_query_checks.go).
		{"blocked by a check", "failed", "", true, skipUpstreamCheckFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			failOnSwallowedError(t)

			expectRunRowBlocked(mock, tc.status, tc.autoPause, tc.blocked)
			mock.ExpectQuery(skipsAlreadyRecordedQuery).WithArgs(provRunA).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB})
			mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*'skipped'`).
				WithArgs(provModelB, "sb", tc.reason, "", provActor, upstreamKindModel, provUpstream, provRunA, "exec-1", int64(3), int64(0)).
				WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
			expectDownstreamLookup(mock, provModelB)

			w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream),
				map[string]any{"run_id": provRunA, "execution_id": "exec-1", "depth": 1})
			if w.Code != http.StatusOK {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if targets, _ := decodeFanOut(t, w.Body.Bytes())["targets"].([]any); len(targets) != 0 {
				t.Errorf("a %s run answered targets %v, want none", tc.name, targets)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// An activity retry after a response that never arrived must not write the cascade a
// second time: the rows are keyed by nothing but their content, so a second pass would
// put every model below the failure in the history twice.
func TestDownstream_ARetryDoesNotRecordTheSkipsTwice(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	expectRunRow(mock, "failed", "")
	mock.ExpectQuery(skipsAlreadyRecordedQuery).WithArgs(provRunA).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream), map[string]any{"run_id": provRunA})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A run that neither succeeded nor stopped on a stable refusal (another run of this
// model was already in flight) tells the models below nothing: that other run wakes them.
func TestDownstream_ATransientSkipTellsTheModelsBelowNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	expectRunRow(mock, "skipped", "")
	w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream), map[string]any{"run_id": provRunA})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The chain bound applies on this path too: the hop past it records the models it
// stopped short of (G7) and starts nothing — once, for the same reason as the cascade.
func TestDownstream_TheHopPastTheBoundRecordsTheModelsItStoppedShortOf(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	expectRunRow(mock, "succeeded", "")
	mock.ExpectQuery(skipsAlreadyRecordedQuery).WithArgs(provRunA).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB})
	mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*'skipped'`).
		WithArgs(provModelB, "sb", skipChainDepthExceeded, "the rebuild chain reached its limit of 8 hops", provActor,
			upstreamKindModel, provUpstream, provRunA, "", int64(maxTriggerChainDepth+1), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))

	w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream),
		map[string]any{"run_id": provRunA, "depth": maxTriggerChainDepth})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if targets, _ := decodeFanOut(t, w.Body.Bytes())["targets"].([]any); len(targets) != 0 {
		t.Errorf("past the bound answered targets %v, want none", targets)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A database error is a 500, so the activity retries it: an empty answer would read as
// "nothing below" and the hop would be lost with no error anywhere.
func TestDownstream_ALookupErrorIsRetryableNotAnEmptyAnswer(t *testing.T) {
	for name, expect := range map[string]func(sqlmock.Sqlmock){
		"run row": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(runRowQuery).WillReturnError(errors.New("connection reset"))
		},
		"models below": func(mock sqlmock.Sqlmock) {
			expectRunRow(mock, "succeeded", "")
			mock.ExpectQuery(`WHERE u.upstream_saved_query_id = \$1::uuid`).WillReturnError(errors.New("connection reset"))
		},
		"cascade": func(mock sqlmock.Sqlmock) {
			expectRunRow(mock, "failed", "")
			mock.ExpectQuery(skipsAlreadyRecordedQuery).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			mock.ExpectQuery(`WHERE u.upstream_saved_query_id = \$1::uuid`).WillReturnError(errors.New("connection reset"))
		},
		"already recorded": func(mock sqlmock.Sqlmock) {
			expectRunRow(mock, "failed", "")
			mock.ExpectQuery(skipsAlreadyRecordedQuery).WillReturnError(errors.New("connection reset"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expect(mock)
			w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream), map[string]any{"run_id": provRunA})
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("got %d, want 500: %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A run row that is not this model's answers nothing below it: the ids are matched
// together, so one model's run cannot wake another model's consumers.
func TestDownstream_ARunOfAnotherModelWakesNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(runRowQuery).WithArgs(provRunA, provUpstream, modelCheckBlockedError).
		WillReturnRows(sqlmock.NewRows([]string{"status", "auto_pause_reason", "check_blocked"}))
	w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(provUpstream), map[string]any{"run_id": provRunA})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if targets, _ := decodeFanOut(t, w.Body.Bytes())["targets"].([]any); len(targets) != 0 {
		t.Errorf("targets = %v, want none", targets)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Ids the database cannot cast are refused with a 400, which the adapter treats as
// non-retryable: no retry changes them.
func TestDownstream_MalformedIdsAreRefused(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	for name, tc := range map[string]struct {
		model string
		body  map[string]any
	}{
		"model id":       {"nope", map[string]any{"run_id": provRunA}},
		"run id":         {provUpstream, map[string]any{"run_id": "nope"}},
		"missing run id": {provUpstream, map[string]any{}},
		"negative depth": {provUpstream, map[string]any{"run_id": provRunA, "depth": -1}},
	} {
		w := doJSON(downstreamRouter(), http.MethodPost, downstreamPath(tc.model), tc.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ----------------------------------------------------------------------------
// Starting the fan-out: retry, then record what was lost
// ----------------------------------------------------------------------------

// fastFanOutRetry shrinks the retry budget so a test of the failure path takes
// milliseconds. The loop is the same; only the clock is shorter.
func fastFanOutRetry(t *testing.T) {
	t.Helper()
	prevBudget, prevPause := fanOutRetryBudget, fanOutRetryFirstPause
	fanOutRetryBudget, fanOutRetryFirstPause = 60*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { fanOutRetryBudget, fanOutRetryFirstPause = prevBudget, prevPause })
}

func closedBreaker() *fanOutBreaker {
	return &fanOutBreaker{window: time.Minute, now: time.Now}
}

func resolveTo(d upstreamFanOutDispatch) func() upstreamFanOutDispatch {
	return func() upstreamFanOutDispatch { return d }
}

// A Temporal that is down for a moment — a restart, a rollout — must not cost the
// models below a completion their rebuild: the start is retried inside the budget.
func TestStartUpstreamFanOut_ATemporalBlipIsRetriedNotLost(t *testing.T) {
	fastFanOutRetry(t)
	calls := 0
	dispatch := func(context.Context, upstreamFanOut) error {
		calls++
		if calls < 3 {
			return errors.New("unavailable")
		}
		return nil
	}
	b := closedBreaker()
	if err := startUpstreamFanOut(context.Background(), b, resolveTo(dispatch), fanOutOf(pipelineSource("p1", "e1"), modelRefreshTarget{ModelID: "m1"})); err != nil {
		t.Fatalf("a start that succeeded on the third try returned %v", err)
	}
	if calls != 3 {
		t.Errorf("dispatched %d times, want 3", calls)
	}
	if !b.acquire() {
		t.Error("a start that got through left the breaker open")
	}
}

// Past the budget the start is given up on and reported, never turned into a silent
// success: the caller records it as trigger_lost.
func TestStartUpstreamFanOut_AStartThatNeverGetsThroughIsAnError(t *testing.T) {
	fastFanOutRetry(t)
	calls := 0
	dispatch := func(context.Context, upstreamFanOut) error { calls++; return errors.New("unavailable") }
	b := closedBreaker()
	err := startUpstreamFanOut(context.Background(), b, resolveTo(dispatch), fanOutOf(pipelineSource("p1", "e1"), modelRefreshTarget{ModelID: "m1"}))
	if err == nil {
		t.Fatal("a start that never got through returned nil")
	}
	if calls < 2 {
		t.Errorf("dispatched %d times, want it retried inside the budget", calls)
	}
	if b.acquire() {
		t.Error("a failed start left the breaker closed; the next completion would pay the full budget again")
	}
}

// No Temporal client is no longer a supported mode that rebuilds in-process: there is
// no in-process path. It is a start that did not happen, and it is reported as one.
func TestStartUpstreamFanOut_NoTemporalIsAFailureNotASilentMode(t *testing.T) {
	fastFanOutRetry(t)
	resolves := 0
	resolve := func() upstreamFanOutDispatch { resolves++; return nil }
	err := startUpstreamFanOut(context.Background(), closedBreaker(), resolve, fanOutOf(pipelineSource("p1", "e1"), modelRefreshTarget{ModelID: "m1"}))
	if !errors.Is(err, errNoTemporalClient) {
		t.Fatalf("err = %v, want errNoTemporalClient", err)
	}
	if resolves < 2 {
		t.Errorf("resolved %d times: a client that dials on the next try must get that try", resolves)
	}
}

func TestStartUpstreamFanOut_OneCallCarriesTheWholeBatch(t *testing.T) {
	var seen []upstreamFanOut
	dispatch := func(_ context.Context, f upstreamFanOut) error { seen = append(seen, f); return nil }
	targets := []modelRefreshTarget{{ModelID: "m1"}, {ModelID: "m2"}}
	if err := startUpstreamFanOut(context.Background(), closedBreaker(), resolveTo(dispatch), fanOutOf(pipelineSource("p1", "e1"), targets...)); err != nil {
		t.Fatal(err)
	}
	// One call for the whole batch, not one per model: the gateway can die the instant
	// it returns and both models are still signalled.
	if len(seen) != 1 || len(seen[0].Targets) != 2 {
		t.Fatalf("dispatch saw %v, want one call carrying both targets", seen)
	}
}

// Temporal refusing the start because this completion already has a fan-out is "already
// done", not "did not happen": no retry, no error, and so no trigger_lost row about a
// model that is being rebuilt. The SDK wraps service errors, so the type is matched
// through the wrapping.
func TestStartUpstreamFanOut_AlreadyStartedIsDone(t *testing.T) {
	for name, already := range map[string]error{
		"plain":   &serviceerror.WorkflowExecutionAlreadyStarted{},
		"wrapped": fmt.Errorf("start workflow: %w", &serviceerror.WorkflowExecutionAlreadyStarted{}),
	} {
		calls := 0
		dispatch := func(context.Context, upstreamFanOut) error { calls++; return already }
		b := closedBreaker()
		if err := startUpstreamFanOut(context.Background(), b, resolveTo(dispatch), fanOutOf(pipelineSource("p1", "e1"), modelRefreshTarget{ModelID: "m1"})); err != nil {
			t.Errorf("%s: already started returned %v", name, err)
		}
		if calls != 1 {
			t.Errorf("%s: dispatched %d times, want 1", name, calls)
		}
		if !b.acquire() {
			t.Errorf("%s: Temporal answering left the breaker open", name)
		}
	}
}

func TestStartUpstreamFanOut_TheWholeSourceReachesTemporalNotJustTheID(t *testing.T) {
	// Depth is what stops a rebuild chain, and it only bounds anything if it survives
	// every hop. This is the hop where it is easiest to lose: the dispatcher is the last
	// place the source is a Go value before it becomes a workflow argument.
	var got upstreamFanOut
	dispatch := func(_ context.Context, f upstreamFanOut) error { got = f; return nil }
	want := modelRefreshSource{Kind: upstreamKindModel, ID: "m0", ExecutionID: "e9", Depth: 3}
	if err := startUpstreamFanOut(context.Background(), closedBreaker(), resolveTo(dispatch), fanOutOf(want, modelRefreshTarget{ModelID: "m1"})); err != nil {
		t.Fatal(err)
	}
	if got.Source != want {
		t.Fatalf("dispatch saw %+v, want %+v", got.Source, want)
	}
}

// G11, kept: inside the window after a failure no completion dials at all — a failed
// dial holds the client mutex for its whole deadline — and each is reported at once.
func TestStartUpstreamFanOut_AnOpenBreakerDoesNotDial(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	b := &fanOutBreaker{window: time.Minute, now: func() time.Time { return now }, until: now.Add(30 * time.Second)}
	resolves := 0
	resolve := func() upstreamFanOutDispatch { resolves++; return nil }
	err := startUpstreamFanOut(context.Background(), b, resolve, fanOutOf(pipelineSource("p1", "e1"), modelRefreshTarget{ModelID: "m1"}))
	if !errors.Is(err, errFanOutBreakerOpen) {
		t.Fatalf("err = %v, want errFanOutBreakerOpen", err)
	}
	if resolves != 0 {
		t.Errorf("resolved the dispatcher %d times inside the window, want 0", resolves)
	}
}

// A completion whose fan-out could not be started is recorded below it as trigger_lost:
// the models it would have woken get a row saying their trigger never arrived, instead
// of a history that reads "nothing happened".
func TestDeliverFanOut_ALostTriggerIsRecordedBelowIt(t *testing.T) {
	for name, b := range map[string]*fanOutBreaker{
		"temporal down": closedBreaker(),
		"breaker open":  {window: time.Minute, now: time.Now, until: time.Now().Add(time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			fastFanOutRetry(t)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()

			src := modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, RunID: provRunA, Depth: 1}
			expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB})
			mock.ExpectQuery(`INSERT INTO saved_query_runs[\s\S]*'skipped'`).
				WithArgs(provModelB, "sb", skipTriggerLost, "", provActor, upstreamKindModel, provUpstream, provRunA, "", int64(1), int64(0)).
				WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
			expectDownstreamLookup(mock, provModelB)

			deliverFanOut(db.DB, b, func() upstreamFanOutDispatch { return nil },
				fanOutOf(src, modelRefreshTarget{ScheduleID: "sb", ModelID: provModelB, RunAsUserID: provActor}))
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeliverFanOut_ADeliveredTriggerWritesNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	deliverFanOut(db.DB, closedBreaker(), resolveTo(func(context.Context, upstreamFanOut) error { return nil }),
		fanOutOf(pipelineSource(provUpstream, "e1"), modelRefreshTarget{ModelID: provModelB}))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The door every gateway-side completion goes through: with no in-process path behind
// it, a completion that never reaches the dispatch rebuilds nothing and records nothing.
func TestFireDownstreamModels_HandsTheCompletionToTemporal(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	got := make(chan upstreamFanOut, 1)
	prevDispatch, prevBreaker := newUpstreamFanOutDispatch, fanOutTemporalBreaker
	newUpstreamFanOutDispatch = resolveTo(func(_ context.Context, f upstreamFanOut) error {
		got <- f
		return nil
	})
	fanOutTemporalBreaker = closedBreaker()
	t.Cleanup(func() { newUpstreamFanOutDispatch, fanOutTemporalBreaker = prevDispatch, prevBreaker })

	src := modelRefreshSource{Kind: upstreamKindModel, ID: provUpstream, RunID: provRunA, ExecutionID: "e1", Depth: 1}
	expectDownstreamLookup(mock, provUpstream, [2]string{"sb", provModelB})

	fireDownstreamModels(context.Background(), src)
	select {
	case f := <-got:
		if f.Source != src || f.Occurrence != fanOutOccurrence(src) {
			t.Errorf("dispatched source %+v occurrence %q, want %+v %q", f.Source, f.Occurrence, src, fanOutOccurrence(src))
		}
		if len(f.Targets) != 1 || f.Targets[0].ScheduleID != "sb" || f.Targets[0].ModelID != provModelB {
			t.Errorf("dispatched targets %+v, want schedule sb for model B", f.Targets)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the completion never reached the Temporal dispatch")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ----------------------------------------------------------------------------
// Every skip reason the code writes is one the database accepts
// ----------------------------------------------------------------------------

// recordModelRunSkip logs a refused INSERT and carries on, by design. So a skip reason
// added in Go but not to saved_query_runs_skip_reason_check is never an error: the row
// is simply not written, and the history of the model it was about reads "nothing
// happened" — exactly what the row was added to prevent. This test is that class: it
// reads the skip constants from the source and the CHECK from the newest migration that
// defines it, and fails when the two sets differ in either direction.
func TestSkipReasons_EveryOneTheCodeWritesIsOneTheDatabaseAccepts(t *testing.T) {
	code := skipReasonConstants(t)
	if len(code) < 5 {
		t.Fatalf("read %d skip reasons from the source, want at least 5: %v", len(code), code)
	}
	file, check := newestSkipReasonCheck(t)
	if strings.Join(code, ",") != strings.Join(check, ",") {
		t.Errorf("skip reasons in Go %v, but %s accepts %v", code, file, check)
	}
}

// skipReasonConstants returns the values of the const block that declares
// skipUpstreamFailed, sorted.
func skipReasonConstants(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "saved_query_models.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var vals []string
		found := false
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if name.Name == "skipUpstreamFailed" {
					found = true
				}
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						vals = append(vals, v)
					}
				}
			}
		}
		if found {
			out = vals
		}
	}
	sort.Strings(out)
	return out
}

var skipReasonCheckRe = regexp.MustCompile(`(?s)ADD CONSTRAINT saved_query_runs_skip_reason_check\s+CHECK \(skip_reason IS NULL OR skip_reason IN\s*\(([^)]*)\)\)`)

// newestSkipReasonCheck returns the migration that last defines the CHECK, and the
// reasons it accepts, sorted.
func newestSkipReasonCheck(t *testing.T) (string, []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Slice(files, func(i, j int) bool { return migrationNumber(files[i]) < migrationNumber(files[j]) })
	var newest string
	var reasons []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m := skipReasonCheckRe.FindStringSubmatch(string(raw))
		if m == nil {
			continue
		}
		newest, reasons = filepath.Base(f), nil
		for _, part := range strings.Split(m[1], ",") {
			reasons = append(reasons, strings.Trim(strings.TrimSpace(part), "'"))
		}
	}
	if newest == "" {
		t.Fatal("no migration defines saved_query_runs_skip_reason_check")
	}
	sort.Strings(reasons)
	return newest, reasons
}

func migrationNumber(path string) int {
	n, _ := strconv.Atoi(strings.SplitN(filepath.Base(path), "_", 2)[0])
	return n
}
