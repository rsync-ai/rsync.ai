package executor

import "fmt"

// The temporal adapter's DAG scheduler sends every workflow_graph node it does
// not resolve itself as a `type: node_execution` task. The executor has a
// handler for exactly one of them: a destination node's data_transfer. Any
// other node task used to fall through executeTask to the plan-less fallback,
// which runs a full source-to-destination transfer whenever both connection
// IDs are present, so a notification or wait node copied the table again
// (KI-NODE-KINDS-DECLARED-BUT-UNROUTED).
//
// shared/workflow_node_kinds_golden.json `executed_by_executor` holds the same
// pair; node_execution_guard_test.go and the adapter's
// node_kind_runtime_test.go both read it.
const (
	nodeExecutionTaskType = "node_execution"
	executorNodeKind      = "destination"
	executorNodeOperation = "data_transfer"
)

// refuseUnhandledNodeTask returns a failed response for a node_execution task
// the executor has no handler for. Tasks that are not node tasks pass through.
func refuseUnhandledNodeTask(task ExecutorTask) (ExecutorResponse, bool) {
	if task.Payload == nil {
		return ExecutorResponse{}, false
	}
	if t, _ := task.Payload["type"].(string); t != nodeExecutionTaskType {
		return ExecutorResponse{}, false
	}
	kind, _ := task.Payload["node_kind"].(string)
	if kind == executorNodeKind && task.Operation == executorNodeOperation {
		return ExecutorResponse{}, false
	}
	nodeID, _ := task.Payload["node_id"].(string)
	if kind == "" {
		kind = "unnamed-kind"
	}
	return ExecutorResponse{
		TaskID:     task.TaskID,
		PipelineID: task.PipelineID,
		Status:     "failed",
		Error: fmt.Sprintf("node %s is a %s node with operation %q, which the executor has no handler for; "+
			"only a %s node (operation %s) runs here",
			nodeID, kind, task.Operation, executorNodeKind, executorNodeOperation),
	}, true
}
