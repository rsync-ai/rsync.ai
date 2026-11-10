package executor

// KI-NODE-KINDS-DECLARED-BUT-UNROUTED, the executor's half. The temporal
// adapter sends every DAG node it does not resolve itself as a
// `type: node_execution` task. The executor has a handler for exactly one of
// them — a destination node's data_transfer. Any other node task carrying both
// connection IDs used to reach the plan-less fallback in executeTask and run a
// full source-to-destination transfer: a `notification` or `wait` node copied
// the table again.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nodeTask(kind, operation string) ExecutorTask {
	return ExecutorTask{
		TaskID:     "task-1",
		PipelineID: "pipe-1",
		Operation:  operation,
		Params: map[string]interface{}{
			"source_connection_id":      "src-conn",
			"destination_connection_id": "dst-conn",
		},
		Payload: map[string]interface{}{
			"type":                      "node_execution",
			"node_id":                   kind + "_1",
			"node_kind":                 kind,
			"operation":                 operation,
			"source_connection_id":      "src-conn",
			"destination_connection_id": "dst-conn",
		},
	}
}

func TestRefuseUnhandledNodeTask(t *testing.T) {
	cases := []struct {
		name    string
		task    ExecutorTask
		refused bool
	}{
		{"a destination's data_transfer runs", nodeTask("destination", "data_transfer"), false},
		{"a notification node", nodeTask("notification", "execute"), true},
		{"a wait node from a legacy history", nodeTask("wait", "execute"), true},
		{"a destination without data_transfer", nodeTask("destination", "execute"), true},
		{"data_transfer forced onto another kind", nodeTask("merge", "data_transfer"), true},
		{"a node task with no kind", nodeTask("", "execute"), true},
		{"not a node task", ExecutorTask{Operation: "execute", Payload: map[string]interface{}{"plan": map[string]interface{}{}}}, false},
		{"no payload", ExecutorTask{Operation: "data_transfer"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, refused := refuseUnhandledNodeTask(c.task)
			if refused != c.refused {
				t.Fatalf("refused = %v, want %v (response %+v)", refused, c.refused, resp)
			}
			if !refused {
				return
			}
			if resp.Status != "failed" || resp.TaskID != c.task.TaskID || resp.PipelineID != c.task.PipelineID {
				t.Errorf("response = %+v, want a failed response for the task", resp)
			}
			nodeID, _ := c.task.Payload["node_id"].(string)
			if !strings.Contains(resp.Error, nodeID) || !strings.Contains(resp.Error, "destination") {
				t.Errorf("error %q must name the node (%s) and the one node the executor runs", resp.Error, nodeID)
			}
		})
	}
}

func TestNodeTaskGuardMatchesSharedGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "shared", "workflow_node_kinds_golden.json"))
	if err != nil {
		t.Fatalf("reading shared/workflow_node_kinds_golden.json: %v", err)
	}
	var g struct {
		ExecutedByExecutor struct {
			NodeKind  string `json:"node_kind"`
			Operation string `json:"operation"`
		} `json:"executed_by_executor"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("parsing the golden: %v", err)
	}
	if g.ExecutedByExecutor.NodeKind != executorNodeKind || g.ExecutedByExecutor.Operation != executorNodeOperation {
		t.Errorf("the executor runs %s/%s, the golden says %s/%s", executorNodeKind, executorNodeOperation,
			g.ExecutedByExecutor.NodeKind, g.ExecutedByExecutor.Operation)
	}
}

func TestUnhandledNodeTaskNeverReachesThePlanLessTransfer(t *testing.T) {
	// A zero Agent: the plan-less transfer dereferences its connection manager,
	// so reaching it panics. The refusal must come first.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a notification node reached the plan-less source-to-destination transfer "+
				"(it panicked on the zero Agent: %v)", r)
		}
	}()
	resp := (&Agent{}).executeTask(context.Background(), nodeTask("notification", "execute"))
	if resp.Status != "failed" || !strings.Contains(resp.Error, "notification") {
		t.Errorf("response = %+v, want failed naming the notification node", resp)
	}
}
