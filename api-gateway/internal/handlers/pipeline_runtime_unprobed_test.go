package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The bug class: a dependency verdict served as current for a pipeline the probe no longer
// checks. The orchestrator's dependency probe skips a paused or stopped pipeline
// (dependency_probe.go sweep), so its pipeline_dependency_health rows freeze at the last
// sweep before Stop — almost always "healthy", since Stop writes pipelines.status in the
// same request that parks the connector. The Runtime dependencies panel then showed a
// STOPPED Debezium connector as a green "healthy" row (seen 10-22).

func depRow(status string, checked time.Time) RuntimeDep {
	dep := RuntimeDep{
		Kind: "debezium_task", Identifier: "rsync-cdc-orders", Status: status,
		LastCheckedAt: &checked, LastHealthyAt: &checked,
	}
	if status != "healthy" {
		dep.LastError = "connector=FAILED"
	}
	return dep
}

// TestMarkUnprobedDeps_AStoppedOrPausedPipelineHasNoCurrentHealthyVerdict: the row keeps
// when it was last checked and last healthy, and says why it is not current.
func TestMarkUnprobedDeps_AStoppedOrPausedPipelineHasNoCurrentHealthyVerdict(t *testing.T) {
	checked := time.Now().Add(-time.Hour)
	for _, status := range []string{"stopped", "paused", "STOPPED", " Paused "} {
		deps := []RuntimeDep{depRow("healthy", checked)}
		deps[0].Details = map[string]interface{}{"connector_state": "RUNNING"}

		markUnprobedDeps(deps, status)

		got := deps[0]
		if got.Status != "unknown" {
			t.Errorf("%q: a frozen healthy row reads %q, want unknown — nothing has checked it since the pipeline stopped", status, got.Status)
		}
		if got.LastCheckedAt == nil || !got.LastCheckedAt.Equal(checked) || got.LastHealthyAt == nil || !got.LastHealthyAt.Equal(checked) {
			t.Errorf("%q: lost when it was last checked / last healthy: %+v", status, got)
		}
		if got.Details["not_checked_while"] == "" || got.Details["last_result"] != "healthy" {
			t.Errorf("%q: details do not say why the verdict is not current: %v", status, got.Details)
		}
		if got.Details["connector_state"] != "RUNNING" {
			t.Errorf("%q: dropped the probe's own details: %v", status, got.Details)
		}
	}
	deps := []RuntimeDep{depRow("healthy", checked)} // nil Details
	markUnprobedDeps(deps, "stopped")
	if deps[0].Details["not_checked_while"] != "stopped" {
		t.Fatalf("a row with no details gained none: %v", deps[0].Details)
	}
}

// TestMarkUnprobedDeps_KeepsAProblemRow: an unhealthy or degraded reading is the last known
// problem, which the user needs before pressing Start; only the all-clear is withdrawn.
func TestMarkUnprobedDeps_KeepsAProblemRow(t *testing.T) {
	checked := time.Now().Add(-time.Hour)
	for _, status := range []string{"unhealthy", "degraded", "unknown"} {
		deps := []RuntimeDep{depRow(status, checked)}
		want := deps[0]
		markUnprobedDeps(deps, "stopped")
		if !reflect.DeepEqual(deps[0], want) {
			t.Errorf("a stopped pipeline's %s row changed: %+v, want %+v", status, deps[0], want)
		}
	}
}

// TestMarkUnprobedDeps_ControlProbedOrFinishedPipelinesAreUnchanged: a running pipeline is
// probed every 15 s, so its verdict is current. A finished run's rows are that run's record
// (the panel labels an unchecked one "Not checked" already), so they are left alone too.
func TestMarkUnprobedDeps_ControlProbedOrFinishedPipelinesAreUnchanged(t *testing.T) {
	checked := time.Now().Add(-time.Hour)
	for _, status := range []string{"running", "processing", "completed", "failed", "cancelled", "archived", ""} {
		deps := []RuntimeDep{depRow("healthy", checked)}
		want := deps[0]
		markUnprobedDeps(deps, status)
		if !reflect.DeepEqual(deps[0], want) {
			t.Errorf("status %q: the row changed: %+v, want %+v", status, deps[0], want)
		}
	}
}

// TestAggregateDepHealth: the fold loadRuntimeDeps did inline. A stopped pipeline whose rows
// were all healthy aggregates to unknown once they are marked: no dependency was checked.
func TestAggregateDepHealth(t *testing.T) {
	now := time.Now()
	row := func(s string) RuntimeDep { return depRow(s, now) }
	cases := []struct {
		deps []RuntimeDep
		want string
	}{
		{nil, "unknown"},
		{[]RuntimeDep{row("unknown")}, "unknown"},
		{[]RuntimeDep{row("healthy"), row("unknown")}, "healthy"},
		{[]RuntimeDep{row("healthy"), row("degraded")}, "degraded"},
		{[]RuntimeDep{row("degraded"), row("unhealthy"), row("healthy")}, "unhealthy"},
	}
	for _, c := range cases {
		if got := aggregateDepHealth(c.deps); got != c.want {
			t.Errorf("aggregateDepHealth(%v) = %q, want %q", c.deps, got, c.want)
		}
	}
	stopped := []RuntimeDep{row("healthy"), row("healthy")}
	markUnprobedDeps(stopped, "stopped")
	if got := aggregateDepHealth(stopped); got != "unknown" {
		t.Errorf("a stopped pipeline's marked rows aggregate to %q, want unknown", got)
	}
}

// runtimeForStoppedCDC serves /runtime for a CDC pipeline in pipelineStatus whose one
// dependency row was last probed, healthy, an hour ago.
func runtimeForStoppedCDC(t *testing.T, pipelineStatus string) PipelineRuntime {
	t.Helper()
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.MatchExpectationsInOrder(false)
	checked := time.Now().Add(-time.Hour)

	mock.ExpectQuery(`SELECT wm\.role\s+FROM pipelines r`).
		WithArgs(wsScopePipeline, wsScopeUser, wsScopeWS).
		WillReturnRows(gateRoleRows("viewer"))
	mock.ExpectQuery(`SELECT status, sync_mode, created_at, updated_at`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"status", "sync_mode", "created_at", "updated_at"}).
			AddRow(pipelineStatus, "cdc", time.Now(), time.Now()))
	mock.ExpectQuery(`FROM pipeline_progress`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{
			"execution_id", "current_stage", "message",
			"progress_percent", "progress_current_step", "progress_total_steps",
			"blocking_reason_type", "blocking_reason_description", "updated_at",
		}).AddRow("55555555-5555-5555-5555-555555555555", "streaming", "Streaming pipeline active",
			100, 8, 8, nil, nil, time.Now()))
	mock.ExpectQuery(`MAX\(last_applied_ts\)[\s\S]*FROM pipeline_run_table_stats`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"max", "pending"}).AddRow(time.Now().Add(-time.Minute), int64(0)))
	mock.ExpectQuery(`DISTINCT ON \(d\.kind, d\.identifier\)`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{
			"kind", "identifier", "status", "last_checked_at", "last_healthy_at",
			"consecutive_failures", "last_error", "details",
		}).AddRow("debezium_task", "rsync-cdc-orders", "healthy", checked, checked, 0, "", []byte(`{"connector_state":"RUNNING"}`)))

	r := wsScopeRouterAsRole(http.MethodGet, "/pipelines/:id/runtime", GetPipelineRuntime, "viewer")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pipelines/"+wsScopePipeline+"/runtime", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /runtime: %d: %s", w.Code, w.Body.String())
	}
	var got PipelineRuntime
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if len(got.Deps) != 1 {
		t.Fatalf("got %d deps, want 1: %s", len(got.Deps), w.Body.String())
	}
	return got
}

// TestGetPipelineRuntime_AStoppedPipelineDoesNotServeAFrozenHealthyDependency: the 10-22
// report. The dependency row reads unknown, with its last check kept; the pipeline's own
// health stays degraded, as a stopped pipeline's always has.
func TestGetPipelineRuntime_AStoppedPipelineDoesNotServeAFrozenHealthyDependency(t *testing.T) {
	for _, status := range []string{"stopped", "paused"} {
		got := runtimeForStoppedCDC(t, status)
		dep := got.Deps[0]
		if dep.Status != "unknown" {
			t.Errorf("%s: the Debezium row reads %q, want unknown: the probe has not checked it since the pipeline %s", status, dep.Status, status)
		}
		if dep.LastCheckedAt == nil || dep.LastHealthyAt == nil {
			t.Errorf("%s: dropped the last check: %+v", status, dep)
		}
		if dep.Details["not_checked_while"] != status {
			t.Errorf("%s: details.not_checked_while = %v, want %q", status, dep.Details["not_checked_while"], status)
		}
		if got.Phase != status || got.Health != "degraded" {
			t.Errorf("%s: phase/health = %q/%q, want %s/degraded", status, got.Phase, got.Health, status)
		}
	}
}

// TestGetPipelineRuntime_ControlARunningPipelineKeepsItsVerdict: the same row on a running
// pipeline is the probe's current answer and is served as it is.
func TestGetPipelineRuntime_ControlARunningPipelineKeepsItsVerdict(t *testing.T) {
	got := runtimeForStoppedCDC(t, "running")
	if dep := got.Deps[0]; dep.Status != "healthy" || dep.Details["not_checked_while"] != nil {
		t.Fatalf("a running pipeline's dependency was relabelled: %+v", dep)
	}
	if got.Health != "healthy" {
		t.Fatalf("health = %q, want healthy", got.Health)
	}
}

// TestGatherDiagnosisEvidence_AStoppedPipelineDoesNotClaimHealthyDependencies: Diagnose reads
// the same rows through loadRuntimeDeps; the model must not be told the stopped connector
// passed a check, nor that the dependencies are healthy in aggregate.
func TestGatherDiagnosisEvidence_AStoppedPipelineDoesNotClaimHealthyDependencies(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	checked := time.Now().Add(-time.Hour)
	mock.ExpectQuery(`FROM pipelines p\s+LEFT JOIN connections sc`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "status", "sync_mode", "source_connection_id", "destination_connection_id",
			"src_type", "dst_type", "created_at", "updated_at",
		}).AddRow("orders-cdc", "stopped", "cdc", nil, nil, "postgresql", "bigquery", time.Now(), time.Now()))
	mock.ExpectQuery(`DISTINCT ON \(d\.kind, d\.identifier\)`).
		WithArgs(wsScopePipeline).
		WillReturnRows(sqlmock.NewRows([]string{
			"kind", "identifier", "status", "last_checked_at", "last_healthy_at",
			"consecutive_failures", "last_error", "details",
		}).AddRow("debezium_task", "rsync-cdc-orders", "healthy", checked, checked, 0, "", []byte("{}")))

	ev, ok := gatherDiagnosisEvidence(db, wsScopePipeline)
	if !ok {
		t.Fatal("no evidence gathered")
	}
	deps, _ := ev["dependencies"].([]RuntimeDep)
	if len(deps) != 1 || deps[0].Status != "unknown" {
		t.Fatalf("evidence dependencies = %+v, want one row reading unknown", ev["dependencies"])
	}
	if got := ev["dependency_health_aggregate"]; got != "unknown" {
		t.Fatalf("dependency_health_aggregate = %v, want unknown: nothing checked a stopped pipeline's dependencies", got)
	}
}
