package projector

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// A new run replaces a parked one: the gateway closes the old execution, points
// pipeline_progress at the new run and tells the old workflow to stop. The old
// workflow's last events (its cancel, a final heartbeat) still reach the projector,
// and without a check they took the progress row back from the run that replaced
// it. In these tests the stored row belongs to guardOtherExec (the newer run) and
// the events come from guardExecID (the replaced one).

func TestProjectEvent_ReplacedRunsCancelLeavesTheNewerRunsRow(t *testing.T) {
	projectOverRow(t, "processing", guardOtherExec, map[string]interface{}{
		"event_type":   "PIPELINE_FAILED",
		"pipeline_id":  guardPipelineID,
		"execution_id": guardExecID,
		"status":       "cancelled",
		"message":      "Pipeline cancelled",
	}, false, isSuperseded, "")
}

func TestProjectEvent_ReplacedRunsLastHeartbeatLeavesTheNewerRunsRow(t *testing.T) {
	projectOverRow(t, "processing", guardOtherExec, map[string]interface{}{
		"event_type":   "STAGE_PROGRESS",
		"pipeline_id":  guardPipelineID,
		"execution_id": guardExecID,
		"stage":        "intent",
		"status":       "processing",
	}, false, isSuperseded, "")
}

// The control: the same cancel from a run that was not replaced takes the row, as a
// new run always could.
func TestProjectEvent_OtherRunNotSupersededStillTakesTheRow(t *testing.T) {
	projectOverRow(t, "processing", guardOtherExec, map[string]interface{}{
		"event_type":   "PIPELINE_FAILED",
		"pipeline_id":  guardPipelineID,
		"execution_id": guardExecID,
		"status":       "cancelled",
		"message":      "Pipeline cancelled",
	}, false, notSuperseded, "cancelled")
}

// When the check cannot answer, the event projects exactly as it did before the
// check existed.
func TestProjectEvent_SupersededCheckFailsOpen(t *testing.T) {
	projectOverRow(t, "processing", guardOtherExec, map[string]interface{}{
		"event_type":   "STAGE_STARTED",
		"pipeline_id":  guardPipelineID,
		"execution_id": guardExecID,
		"stage":        "intent",
		"status":       "processing",
	}, false, supersedeCheckFails, "processing")
}

// An execution id that is not a run's UUID is never looked up.
func TestProjectEvent_NonUUIDExecutionIsNotChecked(t *testing.T) {
	projectOverRow(t, "processing", guardOtherExec, map[string]interface{}{
		"event_type":   "STAGE_STARTED",
		"pipeline_id":  guardPipelineID,
		"execution_id": "legacy-run-1",
		"stage":        "intent",
		"status":       "processing",
	}, false, noSupersedeCheck, "processing")
}

// The adapter guards its writes to the same rows with its own copy of the query
// (the two modules share no code). Two copies that disagree would let a replaced
// run's progress land through one writer and not the other.
func TestSupersededQueryMatchesTheAdapters(t *testing.T) {
	const adapterFile = "../../../backend-temporal-adapter/internal/workflows/run_superseded.go"
	f, err := parser.ParseFile(token.NewFileSet(), adapterFile, nil, 0)
	if err != nil {
		t.Fatalf("could not read the adapter's copy at %s: %v", adapterFile, err)
	}
	var adapterSQL string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "runSupersededSQL" || len(vs.Values) != 1 {
			return true
		}
		if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			adapterSQL, _ = strconv.Unquote(lit.Value)
		}
		return false
	})
	if adapterSQL == "" {
		t.Fatalf("no runSupersededSQL string constant in %s", adapterFile)
	}
	collapse := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if collapse(adapterSQL) != collapse(runSupersededSQL) {
		t.Fatalf("the projector's runSupersededSQL differs from the adapter's:\nprojector: %s\nadapter:   %s",
			collapse(runSupersededSQL), collapse(adapterSQL))
	}
}

// The owning run's own events never pay for the lookup.
func TestProjectEvent_OwningRunIsNotChecked(t *testing.T) {
	projectOverRow(t, "processing", guardExecID, map[string]interface{}{
		"event_type":   "STAGE_PROGRESS",
		"pipeline_id":  guardPipelineID,
		"execution_id": guardExecID,
		"stage":        "executor",
		"status":       "processing",
	}, false, noSupersedeCheck, "processing")
}
