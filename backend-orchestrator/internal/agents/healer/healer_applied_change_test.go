package healer

// CDC-A1: the CDC sink applies a new source column to the destination the moment it
// streams in and used to tell nobody, so the schema changes page stayed empty while the
// destination schema changed underneath the user. The sink now reports it with
// applied=true; the healer must record it as history and must NOT route it like a new
// change, which can only produce two wrong answers for a change that is already live: a
// pending approval for something that already happened, or a re-apply of DDL the
// destination already has.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/IBM/sarama"
	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

// Every notification body is scrubbed before it is persisted and shown. llmscrub's
// dangling-quote rule is fail-closed — an unpaired ' masks everything after it — so the
// previous copy ("…in the pipeline's Schema Changes tab") reached users as
// "…in the pipeline'[redacted]", losing the only instruction the alert carried. The
// scrubber is right; the copy was wrong. This pins the copy, for every outcome.
//
// It also pins WHERE the copy sends people. The apostrophe fix kept the words "Schema
// Changes tab", and there is no such tab: PipelineDetailTabsClient renders exactly six
// (Overview, Execution History, Steps/DAG, Table statistics, Transforms, Data flow).
// Schema drift lives on its own route, /pipelines/{id}/schema-changes, which the alert
// already deep-links via Category=schema_drift (TestActionURLForError). So an alert that
// survived the scrubber intact still ended with an instruction that could not be
// followed — the user goes to the pipeline, counts six tabs, and finds no way in.
func TestDriftNotificationCopySurvivesScrub(t *testing.T) {
	addColumn := SchemaChange{ChangeType: "add_column", Table: "public.orders", ColumnName: "email"}
	dropColumn := SchemaChange{ChangeType: "drop_column", Table: "public.orders", ColumnName: "note"}
	dropColumnNoName := SchemaChange{ChangeType: "drop_column", Table: "public.orders"}
	dropTable := SchemaChange{ChangeType: "drop_table", Table: "public.orders", DDL: "DROP TABLE rsync_public_1a2b3c4d.orders"}

	// "table" contains "tab", so the tab check is on the word, not the substring.
	namesATab := regexp.MustCompile(`\btabs?\b`)

	cases := []struct {
		name   string
		sc     SchemaChange
		notice driftNotice
	}{
		{"applied", addColumn, noticeApplied},
		{"kept column", dropColumn, noticeKept},
		{"kept column, name unknown", dropColumnNoName, noticeKept},
		{"recorded", addColumn, noticeRecorded},
		{"failed", addColumn, noticeFailed},
		{"awaiting decision on a dropped table", dropTable, noticeAwaitingDecision},
		{"awaiting decision on another change", addColumn, noticeAwaitingDecision},
	}
	for _, tc := range cases {
		msg := schemaDriftNotificationText(tc.sc, tc.notice)
		if got := llmscrub.Scrub(msg); got != msg {
			t.Errorf("%s: notification copy is mangled by llmscrub.\n  before: %s\n   after: %s", tc.name, msg, got)
		}
		if strings.Contains(msg, "'") {
			t.Errorf("%s: notification copy contains an apostrophe; llmscrub masks the rest of the sentence: %s", tc.name, msg)
		}
		// Naming the destination is the actionable part and the part the old bug ate.
		if !strings.Contains(msg, "schema changes page") {
			t.Errorf("%s: notification copy no longer names the schema changes page: %s", tc.name, msg)
		}
		// ...and it must not name a tab. There is none; sending users to hunt for one
		// is the same dead end as saying nothing.
		if namesATab.MatchString(strings.ToLower(msg)) {
			t.Errorf("%s: notification copy points at a tab, but the pipeline detail has no Schema Changes tab: %s", tc.name, msg)
		}
	}
}

// The dropped-table decision is the one the user must get right, so its sentence has to
// say what each button does, including that Reject still takes the table out.
func TestDropTableNotificationSaysWhatEachButtonDoes(t *testing.T) {
	msg := schemaDriftNotificationText(
		SchemaChange{ChangeType: "drop_table", Table: "public.orders"}, noticeAwaitingDecision)
	for _, want := range []string{"Approve drops the destination table", "Reject keeps it", "leaves this pipeline"} {
		if !strings.Contains(msg, want) {
			t.Errorf("dropped-table notification does not say %q: %s", want, msg)
		}
	}
}

func appliedChangeMessage(t *testing.T, applied bool) *sarama.ConsumerMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"event_type":  "SCHEMA_CHANGE_DETECTED",
		"pipeline_id": "11111111-1111-1111-1111-111111111111",
		"schema_change": map[string]interface{}{
			"change_type": "add_column",
			"table":       "public.orders",
			"column_name": "email",
			"column_type": "TEXT",
			"ddl":         "ALTER TABLE public.orders ADD COLUMN email TEXT",
			"applied":     applied,
		},
		"context":       map[string]interface{}{"source": "kafka_mcp_sink", "mode": "cdc"},
		"action_needed": false,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &sarama.ConsumerMessage{Value: payload}
}

// recordingExecutor stands in for the MCP client and records every call.
type recordingExecutor struct {
	requests []mcp.ExecuteRequest
	resp     *mcp.ExecuteResponse
	err      error
}

func (r *recordingExecutor) ExecuteWithContext(_ context.Context, req mcp.ExecuteRequest) (*mcp.ExecuteResponse, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return nil, r.err
	}
	if r.resp != nil {
		return r.resp, nil
	}
	return &mcp.ExecuteResponse{Success: true, Result: map[string]interface{}{}}, nil
}

func (r *recordingExecutor) operations() []string {
	ops := make([]string, 0, len(r.requests))
	for _, req := range r.requests {
		ops = append(ops, req.Operation)
	}
	return ops
}

// captureNotifications points the agent's notifications at a slice.
func captureNotifications(a *Agent) *[]*diagnose.StructuredError {
	var got []*diagnose.StructuredError
	a.notifications = func(_ string, se *diagnose.StructuredError) { got = append(got, se) }
	return &got
}

func postgresDestination(context.Context, string) (string, map[string]string, error) {
	return "postgresql", map[string]string{"host": "dest"}, nil
}

// An already-applied change is recorded and announced once. Nothing is run against the
// destination: the column is live already.
func TestHandleSchemaChangeMessage_AlreadyApplied_RecordsWithoutApplying(t *testing.T) {
	exec := &recordingExecutor{}
	a := &Agent{mcpClient: exec, loadDestination: postgresDestination}
	sent := captureNotifications(a)

	if err := a.handleSchemaChangeMessage(context.Background(), appliedChangeMessage(t, true)); err != nil {
		t.Fatalf("handleSchemaChangeMessage returned error: %v", err)
	}
	if len(exec.requests) != 0 {
		t.Errorf("an already-applied change reached the destination connector: %v", exec.operations())
	}
	if len(*sent) != 1 {
		t.Fatalf("got %d notifications, want exactly 1", len(*sent))
	}
	if msg := (*sent)[0].UserMessage; !strings.Contains(msg, "applied automatically") {
		t.Errorf("notification does not say the change was applied: %s", msg)
	}
}

// The short-circuit is keyed on applied, not on the CDC source: an ordinary
// (not-yet-applied) new column must still be applied, through the destination
// connector's ensure_table.
func TestHandleSchemaChangeMessage_NotApplied_AppliesNewColumn(t *testing.T) {
	exec := &recordingExecutor{}
	a := &Agent{mcpClient: exec, loadDestination: postgresDestination}
	sent := captureNotifications(a)

	if err := a.handleSchemaChangeMessage(context.Background(), appliedChangeMessage(t, false)); err != nil {
		t.Fatalf("handleSchemaChangeMessage returned error: %v", err)
	}
	if ops := exec.operations(); len(ops) != 1 || ops[0] != "ensure_table" {
		t.Fatalf("connector calls = %v, want exactly [ensure_table]", ops)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].UserMessage, "applied automatically") {
		t.Errorf("want one applied-automatically notification, got %d: %+v", len(*sent), *sent)
	}
}

// recordAlreadyAppliedChange runs with no database and no broker: the sink has already
// committed the column to the destination, so a control-plane outage must degrade to a
// missing history row, never to a panic on the CDC apply path's downstream consumer.
func TestRecordAlreadyAppliedChange_NoDBNoBroker(t *testing.T) {
	a := &Agent{}
	event := &SchemaChangeEvent{
		PipelineID: "11111111-1111-1111-1111-111111111111",
		SchemaChange: SchemaChange{
			ChangeType: "add_column",
			Table:      "public.orders",
			DDL:        "ALTER TABLE public.orders ADD COLUMN email TEXT",
			Applied:    true,
		},
	}
	if err := a.recordAlreadyAppliedChange(context.Background(), event); err != nil {
		t.Fatalf("recordAlreadyAppliedChange returned error: %v", err)
	}
}
