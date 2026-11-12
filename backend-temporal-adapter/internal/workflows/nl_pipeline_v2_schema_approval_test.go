package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// A schema drift the run would not repair on its own parked the run on the
// "approval_given" signal for 24 hours, behind an "Approve" button. Nothing sends
// that signal: the gateway's only path into a pipeline run is
// signalPipelineWorkflowWithFallback, and no caller passes it "approval_given". So
// every such run waited out the day and then failed with a timeout that said only
// that nobody approved. It now fails at once, saying what changed, why rsync.ai
// stopped, and the DDL it would have run, so the user can apply it and run again.
func TestNLPipelineV2_SchemaChangeNobodyCanApproveFailsAtOnceWithTheReason(t *testing.T) {
	sc := schemaApprovalParkScenario(t)
	sc.legacyBefore = "" // a new run, recorded under the current code

	r := runHITLPark(t, sc, nil)

	// Denominator: the run reached the gate (the planner ran on the executor's drift).
	for _, want := range sc.wantCalls {
		if !containsString(r.calls, want) {
			t.Fatalf("%s never ran (calls %v), so the run never reached the schema gate", want, r.calls)
		}
	}
	if len(r.parks) != 0 {
		t.Fatalf("the run parked %v on an approval nothing can give", r.parks)
	}
	if r.elapsed >= time.Minute {
		t.Fatalf("the run ended %s after start; it must not wait for an approval", r.elapsed)
	}
	if r.err == nil {
		t.Fatal("the run must fail: the destination still does not match the source")
	}
	msg := r.err.Error()
	for _, want := range []string{
		"schema drift detected on orders",                    // what changed (the executor's report)
		"a column type change needs review",                  // why rsync.ai did not apply it
		"ALTER TABLE orders ALTER COLUMN total TYPE numeric", // what to run
		"run the pipeline again",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "waiting for") {
		t.Errorf("failure %q still reads as a timed-out wait", msg)
	}
	if len(r.statusWrites) != 1 || r.statusWrites[0] != "failed" {
		t.Fatalf("expected exactly one terminal status write of \"failed\", got %v", r.statusWrites)
	}
	if !containsString(r.domainEvents, "STAGE_FAILED") {
		t.Fatalf("expected STAGE_FAILED, got %v", r.domainEvents)
	}
}

// Every reason the planner gives for not applying a change has to read as one in
// that failure, including the two it used to word as "Additive columns detected":
// no column missing at all, and more columns than it adds on its own.
func TestSchemaRepairNeedsAPersonMessage(t *testing.T) {
	many := make([]string, maxAutoSchemaDDL+1)
	for i := range many {
		many[i] = "ALTER TABLE x ADD COLUMN c" + itoa(i) + " int"
	}
	for _, tc := range []struct {
		name string
		plan SchemaRepairPlan
		want []string
	}{
		{
			name: "a planner refusal with DDL",
			plan: SchemaRepairPlan{Reason: "Schema auto-repair is only supported for Postgres→Postgres in beta",
				DDL:     []string{`ALTER TABLE "public"."orders" ADD COLUMN IF NOT EXISTS "note" text`},
				Changes: []SchemaRepairChange{{Table: "public.orders", Column: "note", Type: "text"}}},
			want: []string{"only supported for Postgres→Postgres", "public.orders.note (text)", `ADD COLUMN IF NOT EXISTS "note" text`, "run the pipeline again"},
		},
		{
			name: "no DDL to offer",
			plan: SchemaRepairPlan{Reason: noMissingColumnReason},
			want: []string{noMissingColumnReason, "in line with the source", "run the pipeline again"},
		},
		{
			name: "no reason at all",
			plan: SchemaRepairPlan{},
			want: []string{"will not change it on its own.", "run the pipeline again"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := schemaRepairNeedsAPersonMessage("schema drift detected on orders", tc.plan)
			for _, w := range append(tc.want, "schema drift detected on orders") {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not say %q", got, w)
				}
			}
		})
	}
	if r := tooManyColumnsReason(len(many)); !strings.Contains(r, itoa(len(many))) || !strings.Contains(r, itoa(maxAutoSchemaDDL)) {
		t.Errorf("the too-many reason %q must name both counts", r)
	}
}

// The class: a HITL park whose answer no code path delivers. Every signal the
// pipeline workflow parks on must be one the gateway sends into a pipeline run.
// The one exemption is a park kept only so runs recorded before its version gate
// replay; it must still sit behind that gate.
func TestEveryHITLParkWaitsOnASignalTheGatewaySends(t *testing.T) {
	wf, err := os.ReadFile("nl_pipeline_v2_workflow.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(wf)

	// Signal constant -> wire name, and channel variable -> constant.
	constVal := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(Signal\w+)\s*=\s*"(\w+)"`).FindAllStringSubmatch(src, -1) {
		constVal[m[1]] = m[2]
	}
	chanConst := map[string]string{}
	for _, m := range regexp.MustCompile(`(\w+Ch) := workflow\.GetSignalChannel\(ctx, (Signal\w+)\)`).FindAllStringSubmatch(src, -1) {
		chanConst[m[1]] = m[2]
	}
	parked := map[string]bool{}
	waits := regexp.MustCompile(`awaitHITLSignal\(ctx, childCtx, (\w+Ch),`).FindAllStringSubmatch(src, -1)
	for _, m := range waits {
		name, ok := constVal[chanConst[m[1]]]
		if !ok {
			t.Fatalf("park on %s: cannot resolve its signal name", m[1])
		}
		parked[name] = true
	}
	// Denominator: the scan found every park the timeout guard counts.
	if parks := strings.Count(src, "state.SetWaitReason("); len(waits) == 0 || len(waits) != parks {
		t.Fatalf("scanned %d parks, but the workflow has %d", len(waits), parks)
	}

	sent := gatewayPipelineSignals(t)
	// Arming control: the scan sees the signals the gateway is known to send.
	for _, known := range []string{SignalTablesSelected, SignalCancel, SignalConnectionsConfigured} {
		if !sent[known] {
			t.Fatalf("gateway scan missed %q (found %v); this guard cannot fail", known, sent)
		}
	}

	_ = schemaApprovalFailsFastVersion // the gate named below must exist
	legacyOnly := map[string]string{SignalApprovalGiven: "schemaApprovalFailsFastVersion"}
	var unanswerable []string
	for name := range parked {
		if sent[name] {
			continue
		}
		if gate, ok := legacyOnly[name]; ok {
			if !strings.Contains(src, "workflow.GetVersion(ctx, "+gate+", workflow.DefaultVersion, 1) != workflow.DefaultVersion") {
				t.Errorf("the %q park is kept for replay only, but %s does not gate it", name, gate)
			}
			continue
		}
		unanswerable = append(unanswerable, name)
	}
	sort.Strings(unanswerable)
	if len(unanswerable) > 0 {
		t.Fatalf("the workflow parks on %v, which the gateway never sends: such a run waits out its deadline", unanswerable)
	}
}

// gatewayPipelineSignals is every signal name the gateway passes to
// signalPipelineWorkflowWithFallback, its one way to signal a pipeline run.
func gatewayPipelineSignals(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "api-gateway", "internal", "handlers")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no gateway handlers under %s (%v)", dir, err)
	}
	call := regexp.MustCompile(`signalPipelineWorkflowWithFallback\([^\n]*?"(\w+)", payload\)`)
	sent := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range call.FindAllStringSubmatch(string(body), -1) {
			sent[m[1]] = true
		}
	}
	return sent
}

func schemaApprovalParkScenario(t *testing.T) hitlParkScenario {
	t.Helper()
	for _, sc := range hitlParkScenarios {
		if sc.park == "executor/approval" {
			return sc
		}
	}
	t.Fatal("no schema approval scenario in hitlParkScenarios")
	return hitlParkScenario{}
}
