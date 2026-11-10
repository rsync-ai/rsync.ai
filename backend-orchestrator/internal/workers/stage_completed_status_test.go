package workers

// Census of the pipeline-level Status every STAGE_COMPLETED event carries.
//
// ProgressEvent.Status is NOT the stage's status — it is the PIPELINE's status.
// The api-gateway projector writes it through verbatim
// (api-gateway/internal/projector/event_projector.go:320,
// `status = ... THEN EXCLUDED.status`) into pipeline_progress.status, which is
// what the UI, the list view's derived_status and the /flow terminal check all
// read. A *stage* event that claims a *run-terminal* status therefore reports
// the whole run finished while it is still in flight.
//
// That is not hypothetical. On prod 2026-08-03, execution fa0122a9 emitted
// (pipeline_run_events.occurred_at):
//
//	STAGE_COMPLETED | executor | completed  | 7/8 | 14:53:29   <- this bug
//	STAGE_COMPLETED | executor | processing | -/- | 14:53:38   <- adapter, contradicts it
//	PIPELINE_COMPLETED         | completed  |     | 14:53:42   <- the real terminal event
//
// The run-terminal claim was published 17.8 s before the execution actually
// ended (executions.end_time 14:53:46.781) and stood as the newest
// status-bearing event for 9 s, until the temporal-adapter contradicted it.
//
// What makes it survivable on the happy path is precisely that adapter event.
// On a run that dies after step 7 there is no corrective event, so 'completed'
// is the last word and the failure is never surfaced — a false success, which
// is exactly what post-deploy regression check 2 exists to catch.
//
// Ten of the eleven STAGE_COMPLETED sites in this package say "processing"; the
// eleventh is the streaming handoff described below. This file pins that
// convention so the defect cannot come back, and so a new emit site has to make
// a deliberate choice rather than copy the wrong neighbour. The count is not
// hardcoded anywhere — the test logs what it actually checked, so a drifting
// comment cannot make the assertion drift with it.
//
// This census resolves identifiers, not just literals. The earlier version
// scanned literal `Status:` fields only and noted the gap as a known limit —
// then the identical defect was found in the temporal-adapter written exactly
// that way (`Status: execStatus`), where a literals-only scan reports "0
// problems" while walking past it. A stated limit is still a hole. Any Status
// expression this test cannot resolve is now a FAILURE rather than a silent
// skip, and the count of sites *asserted on* is checked against the count of
// sites *found* — a scan that matches ten sites and reaches a verdict on three
// looks exhaustive and is not.
//
// Sibling census, same invariant, other service:
// backend-temporal-adapter/internal/workflows/stage_completed_status_test.go
//
// STAGE_FAILED is held to the same rule (TestStageFailedNeverClaimsRunTerminalStatus).
// A worker's failed task is not a failed run: the temporal-adapter decides what
// happens next — park the run in the connection-fix wait, replan, or retry the
// activity — and writes the run-terminal status itself when it gives up
// (UpdatePipelineStatusActivity, deferred in nl_pipeline_v2_workflow.go). On
// prod 2026-10-05, execution 3ba9c192 (the #1405 deploy check) emitted
//
//	STAGE_FAILED     | executor | failed           | 09:28:16.916  <- this bug
//	PIPELINE_WAITING |          | waiting_for_user |   ~40-180 ms later
//
// so pipeline_progress.status read "failed" for a run that was waiting for a
// setting and later completed — post-deploy regression check 2a. The stage
// itself still reads failed: the stage state comes from the event type
// (progress_events.go stateForEventType: STAGE_FAILED -> "failed"), not from
// Status. infra_preflight.go's emitProgress already sent "processing" for its
// STAGE_FAILED; the executor and intent workers sent "failed".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// nonTerminalEventStatus is the allowlist: statuses that do NOT mark a run
// finished, and so are safe for a mid-run stage event to publish.
//
// Anything else is run-terminal. Adding a status here is a deliberate assertion
// that the UI and derived_status will not treat it as "this run is over".
var nonTerminalEventStatus = map[string]bool{
	"processing": true, // the convention: 9 of 10 sites in this package

	// The streaming handoff (executor.go:464-467). The executor started a CDC/
	// streaming pipeline and the workflow proceeds to monitor_streaming; the
	// run is emphatically not over. UpdatePipelineStatusActivity later
	// reconciles the row to 'running' for the same reason
	// (backend-temporal-adapter/internal/workflows/pipeline_status_activity.go:72).
	"running": true,

	"pending":          true,
	"waiting_for_user": true,
}

func TestStageCompletedNeverClaimsRunTerminalStatus(t *testing.T) {
	assertStageEventStatusNonTerminal(t, "STAGE_COMPLETED",
		"Use \"processing\" like the other STAGE_COMPLETED sites, and let PIPELINE_COMPLETED\n"+
			"    carry the terminal status.")
}

func TestStageFailedNeverClaimsRunTerminalStatus(t *testing.T) {
	assertStageEventStatusNonTerminal(t, "STAGE_FAILED",
		"A failed task is not a failed run: the workflow may still repair, replan or retry,\n"+
			"    and writes \"failed\" itself when it gives up. Use \"processing\"; the stage still\n"+
			"    reads failed (stateForEventType derives it from the event type).")
}

// assertStageEventStatusNonTerminal checks every `EventType: <eventType>`
// composite literal in this package publishes a non-terminal Status.
func assertStageEventStatusNonTerminal(t *testing.T, eventType, remedy string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse workers package: %v", err)
	}

	found := 0    // eventType composite literals anywhere in the package
	asserted := 0 // ...of those, the ones this test reached a verdict on

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			// Pass 1: count every site, including any outside a function body.
			ast.Inspect(file, func(n ast.Node) bool {
				if cl, ok := n.(*ast.CompositeLit); ok && isStageEventLit(cl, eventType) {
					found++
				}
				return true
			})

			// Pass 2: assert, scoped to a function so identifiers resolve.
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				assigns := stringAssignmentsIn(fn)

				ast.Inspect(fn.Body, func(n ast.Node) bool {
					cl, ok := n.(*ast.CompositeLit)
					if !ok || !isStageEventLit(cl, eventType) {
						return true
					}
					asserted++
					pos := fset.Position(cl.Pos())

					expr := fieldValueOf(cl, "Status")
					if expr == nil {
						t.Errorf("%s: %s has no Status field — the zero "+
							"value \"\" is published as the pipeline status.", pos, eventType)
						return true
					}

					statuses, ok := resolveStringValues(expr, assigns)
					if !ok {
						t.Errorf("%s: %s Status is an expression this census "+
							"cannot resolve (%T). Use a string literal, or a local variable "+
							"assigned only string literals — an unreadable site is an "+
							"unguarded one, which is how this same bug survived in the "+
							"temporal-adapter.", pos, eventType, expr)
						return true
					}
					for _, status := range statuses {
						if nonTerminalEventStatus[status] {
							continue
						}
						t.Errorf(
							"%s: %s publishes Status:%q, which is run-terminal.\n"+
								"    ProgressEvent.Status is the PIPELINE status and the projector writes it\n"+
								"    verbatim into pipeline_progress.status (event_projector.go:320), so this\n"+
								"    marks the whole run finished mid-flight. %s",
							pos, eventType, status, remedy)
					}
					return true
				})
			}
		}
	}

	// Guard against a silently-passing scan: if the AST walk stops matching
	// (renamed field, changed literal), this test would go green while
	// asserting nothing.
	if found == 0 {
		t.Fatalf("census matched no %s emit sites — the scan is broken, not the code", eventType)
	}
	// And against a partially-passing one, which is worse: it looks exhaustive.
	if asserted != found {
		t.Fatalf("found %d %s sites but reached a verdict on only %d — "+
			"%d site(s) sit outside any function body and went unchecked",
			found, eventType, asserted, found-asserted)
	}
	t.Logf("checked %d %s site(s)", asserted, eventType)
}

// isStageEventLit reports whether cl has the string literal EventType: eventType.
func isStageEventLit(cl *ast.CompositeLit, eventType string) bool {
	lit, ok := fieldValueOf(cl, "EventType").(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == eventType
}

// fieldValueOf returns the value expression of the named keyed field, or nil.
func fieldValueOf(cl *ast.CompositeLit, name string) ast.Expr {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
			return kv.Value
		}
	}
	return nil
}

// unresolvableStatus marks an identifier assigned something this census cannot
// read. It cannot collide with a real status: those are lowercase identifiers.
const unresolvableStatus = "\x00unresolvable"

// stringAssignmentsIn collects every string-literal value assigned to each local
// identifier anywhere in fn. Deliberately flow-insensitive: a variable that is
// run-terminal on ANY branch is a defect, whichever branch reaches the emit site.
func stringAssignmentsIn(fn *ast.FuncDecl) map[string][]string {
	out := map[string][]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			lit, ok := assign.Rhs[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				out[ident.Name] = append(out[ident.Name], unresolvableStatus)
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil {
				out[ident.Name] = append(out[ident.Name], v)
			}
		}
		return true
	})
	return out
}

// resolveStringValues reduces a Status expression to the set of strings it can
// hold. ok=false means "cannot tell", which the caller treats as a failure.
func resolveStringValues(expr ast.Expr, assigns map[string][]string) ([]string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return nil, false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return nil, false
		}
		return []string{v}, true
	case *ast.Ident:
		values, found := assigns[e.Name]
		if !found || len(values) == 0 {
			return nil, false
		}
		for _, v := range values {
			if v == unresolvableStatus {
				return nil, false
			}
		}
		return values, true
	default:
		return nil, false
	}
}
