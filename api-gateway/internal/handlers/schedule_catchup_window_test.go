package handlers

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// Bug class: a Temporal schedule policy set only at create, and kept stale by
// update. Pipeline and model schedules were created with a 1-minute catch-up
// window, so a fire missed while the machine slept or Temporal restarted for
// longer than a minute was dropped (11-06: the Mac slept over the NSE
// pipeline's 14:30 UTC fire and Temporal recorded missed_catchup_window: 1).
// Editing a schedule copied the old policy back, so no edit could repair it.

// catchupFloor is the window the fix promises: a fire missed by up to a day
// still runs once when the machine wakes.
const catchupFloor = 24 * time.Hour

type catchupFakeTemporal struct {
	client.Client
	sc *catchupFakeSchedules
}

func (f *catchupFakeTemporal) ScheduleClient() client.ScheduleClient { return f.sc }

// catchupFakeSchedules records every create and keeps one stored schedule per
// id, which Update reads and writes the way the SDK does: DoUpdate gets the
// description, and temporal.ErrSkipScheduleUpdate writes nothing.
type catchupFakeSchedules struct {
	client.ScheduleClient
	mu      sync.Mutex
	created []client.ScheduleOptions
	stored  map[string]client.Schedule
	writes  map[string]int
	broken  map[string]error
}

func (s *catchupFakeSchedules) Create(_ context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, o)
	return &catchupFakeHandle{s: s, id: o.ID}, nil
}

func (s *catchupFakeSchedules) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return &catchupFakeHandle{s: s, id: id}
}

type catchupFakeHandle struct {
	client.ScheduleHandle
	s  *catchupFakeSchedules
	id string
}

func (h *catchupFakeHandle) Update(_ context.Context, o client.ScheduleUpdateOptions) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if err := h.s.broken[h.id]; err != nil {
		return err
	}
	cur, ok := h.s.stored[h.id]
	if !ok {
		return serviceerror.NewNotFound("schedule not found")
	}
	// Hand DoUpdate its own copy, as the SDK does (it decodes a fresh
	// description per call), so an in-place change cannot pose as a write.
	in := cur
	if cur.Policy != nil {
		p := *cur.Policy
		in.Policy = &p
	}
	if cur.State != nil {
		st := *cur.State
		in.State = &st
	}
	up, err := o.DoUpdate(client.ScheduleUpdateInput{Description: client.ScheduleDescription{Schedule: in}})
	if errors.Is(err, temporal.ErrSkipScheduleUpdate) {
		return nil
	}
	if err != nil {
		return err
	}
	h.s.stored[h.id] = *up.Schedule
	h.s.writes[h.id]++
	return nil
}

func installCatchupTemporal(t *testing.T) *catchupFakeSchedules {
	t.Helper()
	sc := &catchupFakeSchedules{
		stored: map[string]client.Schedule{},
		writes: map[string]int{},
		broken: map[string]error{},
	}
	temporalMu.Lock()
	prev := temporalClient
	temporalClient = &catchupFakeTemporal{sc: sc}
	temporalMu.Unlock()
	t.Cleanup(func() {
		temporalMu.Lock()
		temporalClient = prev
		temporalMu.Unlock()
	})
	return sc
}

// oldSchedule is a schedule as the code before this fix created it.
func oldSchedule(paused bool) client.Schedule {
	return client.Schedule{
		Spec:   &client.ScheduleSpec{CronExpressions: []string{"0 20 * * 1-5"}, TimeZoneName: "Asia/Calcutta"},
		Action: &client.ScheduleWorkflowAction{Workflow: "ScheduledPipelineRunWorkflow", TaskQueue: "pipeline-workflows"},
		Policy: &client.SchedulePolicies{
			Overlap:        enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
			CatchupWindow:  time.Minute,
			PauseOnFailure: true,
		},
		State: &client.ScheduleState{Paused: paused, Note: "kept"},
	}
}

func TestCatchupWindow_NewSchedulesRunAMissedFireOnWake(t *testing.T) {
	sc := installCatchupTemporal(t)
	ctx := context.Background()
	spec := ScheduleSpec{Cron: "0 20 * * 1-5", Timezone: "Asia/Calcutta"}

	if err := createTemporalSchedule(ctx, "pipe-sched", "pipe-1", "cron", spec); err != nil {
		t.Fatalf("pipeline create: %v", err)
	}
	if err := createTemporalModelSchedule(ctx, "model-sched", "model-1", "cron", spec); err != nil {
		t.Fatalf("model create: %v", err)
	}
	if len(sc.created) != 2 {
		t.Fatalf("want 2 creates, got %d", len(sc.created))
	}
	for _, o := range sc.created {
		if o.CatchupWindow < catchupFloor {
			t.Errorf("%s: catch-up window %v, want at least %v: a fire missed by more than that is dropped", o.ID, o.CatchupWindow, catchupFloor)
		}
		// The wide window is safe only because SKIP turns a backlog of missed
		// fires into one run.
		if o.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP {
			t.Errorf("%s: overlap %v, want SKIP", o.ID, o.Overlap)
		}
	}
}

func TestCatchupWindow_EditWidensAnOldScheduleAndKeepsTheRest(t *testing.T) {
	sc := installCatchupTemporal(t)
	sc.stored["pipe-sched"] = oldSchedule(true)

	err := updateTemporalSchedule(context.Background(), "pipe-sched", "pipe-1", "cron", ScheduleSpec{Cron: "30 20 * * 1-5", Timezone: "Asia/Calcutta"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := sc.stored["pipe-sched"]
	if got.Policy == nil || got.Policy.CatchupWindow < catchupFloor {
		t.Fatalf("catch-up window after an edit = %+v, want at least %v", got.Policy, catchupFloor)
	}
	if got.Policy.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP || !got.Policy.PauseOnFailure {
		t.Errorf("edit changed the rest of the policy: %+v", got.Policy)
	}
	if got.State == nil || !got.State.Paused || got.State.Note != "kept" {
		t.Errorf("edit changed the schedule's state: %+v", got.State)
	}
	if got.Spec == nil || len(got.Spec.CronExpressions) != 1 || got.Spec.CronExpressions[0] != "30 20 * * 1-5" {
		t.Errorf("edit did not apply the new cron: %+v", got.Spec)
	}
}

func TestCatchupWindow_StartupReconcileWidensOnlyOldSchedules(t *testing.T) {
	sc := installCatchupTemporal(t)
	sc.stored["old-active"] = oldSchedule(false)
	sc.stored["old-paused"] = oldSchedule(true)
	wide := oldSchedule(false)
	wide.Policy.CatchupWindow = 365 * 24 * time.Hour
	sc.stored["already-wide"] = wide
	dflt := oldSchedule(false)
	dflt.Policy.CatchupWindow = 0 // Temporal's default, a year
	sc.stored["temporal-default"] = dflt

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT temporal_schedule_id FROM pipeline_schedules`).
		WillReturnRows(sqlmock.NewRows([]string{"temporal_schedule_id"}).
			AddRow("old-active").AddRow("old-paused").AddRow("already-wide").
			AddRow("temporal-default").AddRow("gone-from-temporal"))

	done, err := reconcileScheduleCatchupWindows(context.Background(), database)
	if !done || err != nil {
		t.Fatalf("reconcile = (%v, %v), want done: a schedule gone from Temporal is not an error", done, err)
	}
	for _, id := range []string{"old-active", "old-paused"} {
		got := sc.stored[id]
		if got.Policy.CatchupWindow != catchupFloor {
			t.Errorf("%s: catch-up window %v, want %v", id, got.Policy.CatchupWindow, catchupFloor)
		}
		if got.Policy.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP || !got.Policy.PauseOnFailure {
			t.Errorf("%s: reconcile changed the rest of the policy: %+v", id, got.Policy)
		}
		if sc.writes[id] != 1 {
			t.Errorf("%s: %d writes, want 1", id, sc.writes[id])
		}
	}
	if !sc.stored["old-paused"].State.Paused {
		t.Error("reconcile unpaused a paused schedule")
	}
	// Only ever widen: a wider window, or Temporal's default, is not written.
	for _, id := range []string{"already-wide", "temporal-default"} {
		if sc.writes[id] != 0 {
			t.Errorf("%s: written %d times, want 0", id, sc.writes[id])
		}
	}
	if w := sc.stored["already-wide"].Policy.CatchupWindow; w != 365*24*time.Hour {
		t.Errorf("already-wide narrowed to %v", w)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	// A second start finds nothing to widen.
	mock.ExpectQuery(`SELECT temporal_schedule_id`).
		WillReturnRows(sqlmock.NewRows([]string{"temporal_schedule_id"}).AddRow("old-active"))
	if done, err := reconcileScheduleCatchupWindows(context.Background(), database); !done || err != nil {
		t.Fatalf("second reconcile = (%v, %v)", done, err)
	}
	if sc.writes["old-active"] != 1 {
		t.Errorf("second reconcile rewrote old-active: %d writes", sc.writes["old-active"])
	}
}

func TestCatchupWindow_StartupReconcileRetriesWhenTemporalFails(t *testing.T) {
	sc := installCatchupTemporal(t)
	sc.stored["old-active"] = oldSchedule(false)
	sc.broken["flaky"] = serviceerror.NewUnavailable("temporal restarting")

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT temporal_schedule_id`).
		WillReturnRows(sqlmock.NewRows([]string{"temporal_schedule_id"}).AddRow("flaky").AddRow("old-active"))

	done, err := reconcileScheduleCatchupWindows(context.Background(), database)
	if done || err == nil {
		t.Fatalf("reconcile = (%v, %v), want not done so it retries", done, err)
	}
	// One failure does not stop the others.
	if sc.stored["old-active"].Policy.CatchupWindow != catchupFloor {
		t.Errorf("old-active not widened past a failing neighbour")
	}
}

// TestCatchupWindow_EveryScheduleUsesTheSharedWindow guards the class: a new
// schedule, or a later edit, that sets its own catch-up window drifts from
// the one the reconcile and update widen to. Every CatchupWindow written in
// this package's code must be scheduleCatchupWindow; leaving it out (Temporal's
// default, a year, as workflow schedules do) is also fine.
func TestCatchupWindow_EveryScheduleUsesTheSharedWindow(t *testing.T) {
	if scheduleCatchupWindow < catchupFloor {
		t.Fatalf("scheduleCatchupWindow = %v, want at least %v", scheduleCatchupWindow, catchupFloor)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ok := func(e ast.Expr) bool {
			id, isIdent := e.(*ast.Ident)
			return isIdent && id.Name == "scheduleCatchupWindow"
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if k, isIdent := n.Key.(*ast.Ident); isIdent && k.Name == "CatchupWindow" {
					checked++
					if !ok(n.Value) {
						t.Errorf("%s: CatchupWindow set to something other than scheduleCatchupWindow", fset.Position(n.Pos()))
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if sel, isSel := lhs.(*ast.SelectorExpr); isSel && sel.Sel.Name == "CatchupWindow" && i < len(n.Rhs) {
						checked++
						if !ok(n.Rhs[i]) {
							t.Errorf("%s: CatchupWindow assigned something other than scheduleCatchupWindow", fset.Position(n.Pos()))
						}
					}
				}
			}
			return true
		})
	}
	// pipeline create, model create, withScheduleCatchupWindow (nil + widen).
	if checked < 4 {
		t.Fatalf("checked %d CatchupWindow writes, want at least 4: the scan is not seeing the code", checked)
	}
}
