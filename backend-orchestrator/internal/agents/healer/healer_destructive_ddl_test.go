package healer

// C1: the schema-drift approval card tells the user that a destructive change is
// NOT auto-applied and that approving only records their decision. The healer used
// to be handed those approvals anyway, refuse them inside applyMigration, flip the
// approval row from `approved` to `failed`, and raise a red alert — punishing the
// user for doing exactly what the screen said. isDestructiveDDL is now the single
// definition both the guard and the approved-changes handler consult, and
// handleApprovedChange returns before applyMigration ever sees such a statement.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/IBM/sarama"
)

func TestIsDestructiveDDL(t *testing.T) {
	// Destructive means "would delete data if run", which an advisory note is not —
	// the lockstep with the gateway lives in TestNotAutoAppliedDDL below, because
	// there are now two independent reasons a statement is never auto-applied.
	tests := []struct {
		ddl  string
		want bool
	}{
		{"ALTER TABLE public.orders ADD COLUMN total numeric", false},
		{"ALTER TABLE public.orders ALTER COLUMN id TYPE bigint", false},
		{"-- drift: table public.shipments appeared in source", false},
		{"ALTER TABLE public.orders DROP COLUMN name", true},
		{"DROP TABLE public.legacy", true},
		{"TRUNCATE TABLE public.orders", true},
		{"ALTER TABLE x; TRUNCATE y", true},
		{"alter table t drop column c", true},
		{"   DROP TABLE t", true},
		// Token check, not substring: a column NAMED drop_* is fine.
		{"ALTER TABLE t ADD COLUMN drop_reason text", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isDestructiveDDL(tt.ddl); got != tt.want {
			t.Errorf("isDestructiveDDL(%q) = %v, want %v", tt.ddl, got, tt.want)
		}
	}
}

// notAutoAppliedDDL is the predicate the gateway mirrors as !autoApplicableDDL
// (handlers/schema_evolution.go, TestAutoApplicableDDL): the gateway tells the user
// what approval will do and gates dispatch on it, this decides what actually
// happens on the other side. They must not disagree about the same statement —
// that disagreement IS the defect. Kept case-for-case with TestAutoApplicableDDL,
// inverted, so a change to one predicate that isn't mirrored shows up as a failing
// case here rather than as a user approving a change nobody applies.
func TestNotAutoAppliedDDL(t *testing.T) {
	tests := []struct {
		ddl  string
		want bool
	}{
		{"ALTER TABLE public.orders ADD COLUMN total numeric", false},
		{"ALTER TABLE public.orders ALTER COLUMN id TYPE bigint", false},
		// Advisory notes: a comment executes cleanly everywhere, so letting one
		// through reported an apply that never happened.
		{"-- drift: table public.shipments appeared in source", true},
		{"-- drift: public.orders.note declared type changed in the source from varchar(50) to varchar(10) (both map to string, so the destination column is unchanged)", true},
		{"   -- drift: anything", true},
		{"ALTER TABLE public.orders DROP COLUMN name", true},
		{"DROP TABLE public.legacy", true},
		{"TRUNCATE TABLE public.orders", true},
		{"ALTER TABLE x; TRUNCATE y", true},
		{"alter table t drop column c", true},
		{"   DROP TABLE t", true},
		{"ALTER TABLE t ADD COLUMN drop_reason text", false},
	}
	for _, tt := range tests {
		if got := notAutoAppliedDDL(tt.ddl); got != tt.want {
			t.Errorf("notAutoAppliedDDL(%q) = %v, want %v", tt.ddl, got, tt.want)
		}
	}
}

// CDC-D1: the cdcstats agent reports a source table that CDC stopped seeing by
// publishing it onto HealerTopic with applied=false, so it takes this package's ordinary
// route → file → notify path. A dropped table is the one change that waits for a person,
// and approving it drops the destination table through the connector, never by running
// this statement. It no longer reports dropped columns: the destination keeps them.
//
// The dependency runs cdcstats → healer, so the assertion has to live here. If either
// predicate is ever narrowed such that this stops matching, CDC would file a dropped
// table as something to apply.
func TestCDCReportedDropTableAwaitsDecision(t *testing.T) {
	// Keep in lockstep with classifySchemaChange in
	// internal/agents/cdcstats/schema_changes.go — the only DDL form it emits.
	ddl := "DROP TABLE pipeline_test.cdc_drift"
	if !isDestructiveDDL(ddl) {
		t.Errorf("cdcstats emits %q; isDestructiveDDL must classify it destructive or applyMigration would run it", ddl)
	}
	if got := routeSchemaChange(SchemaChange{ChangeType: "drop_table", Table: "pipeline_test.cdc_drift", DDL: ddl}); got != routeAwaitDecision {
		t.Errorf("routeSchemaChange(cdcstats drop_table) = %v, want routeAwaitDecision", got)
	}
}

// handleApprovedChange must not touch applyMigration for destructive DDL other than a
// dropped table. If the destructive branch is ever removed, applyMigration refuses the
// statement and the handler raises "Approved DDL failed to apply" — so the test asserts
// that no notification is sent and no connector is called.
func TestHandleApprovedChange_DestructiveDDL_NotApplied(t *testing.T) {
	exec := &recordingExecutor{}
	a := &Agent{mcpClient: exec, loadDestination: postgresDestination}
	sent := captureNotifications(a)
	payload, err := json.Marshal(map[string]interface{}{
		"event_type":  "schema_change_approved",
		"approval_id": "approval-1",
		"pipeline_id": "11111111-1111-1111-1111-111111111111",
		"change_type": "drop_column",
		"table_name":  "orders",
		"ddl":         "ALTER TABLE orders DROP COLUMN legacy_ref",
		"approved_by": "alice@example.com",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := a.handleApprovedChange(context.Background(), &sarama.ConsumerMessage{Value: payload}); err != nil {
		t.Fatalf("handleApprovedChange returned error: %v", err)
	}
	if len(exec.requests) != 0 {
		t.Errorf("destructive DDL reached the destination connector: %v", exec.operations())
	}
	if len(*sent) != 0 {
		t.Errorf("approving destructive DDL raised %d notification(s); first: %s", len(*sent), (*sent)[0].UserMessage)
	}
}

// B1: the same guarantee for advisory notes. Approving one used to send a SQL
// comment to the destination, which executed it happily and left the row marked
// `applied` having applied nothing — the C1 falsehood from the other side. A dropped
// column is now an advisory note too (DroppedColumnNote), so an old pending row for one
// must record the approval and run nothing.
func TestHandleApprovedChange_AdvisoryDDL_NotApplied(t *testing.T) {
	for _, ddl := range []string{
		"-- drift: table public.shipments appeared in source",
		"-- drift: public.orders.note declared type changed in the source from varchar(50) to varchar(10) (both map to string, so the destination column is unchanged)",
		DroppedColumnNote("public.orders", "note"),
	} {
		exec := &recordingExecutor{}
		a := &Agent{mcpClient: exec, loadDestination: postgresDestination}
		sent := captureNotifications(a)
		payload, err := json.Marshal(map[string]interface{}{
			"event_type":  "schema_change_approved",
			"approval_id": "approval-advisory",
			"pipeline_id": "11111111-1111-1111-1111-111111111111",
			"change_type": "modify_column",
			"table_name":  "orders",
			"ddl":         ddl,
			"approved_by": "alice@example.com",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := a.handleApprovedChange(context.Background(), &sarama.ConsumerMessage{Value: payload}); err != nil {
			t.Fatalf("handleApprovedChange(%q) returned error: %v", ddl, err)
		}
		if len(exec.requests) != 0 {
			t.Errorf("advisory note %q reached the destination connector: %v", ddl, exec.operations())
		}
		if len(*sent) != 0 {
			t.Errorf("approving advisory note %q raised %d notification(s); first: %s", ddl, len(*sent), (*sent)[0].UserMessage)
		}
	}
}

// The auto-apply path can reach applyMigration without passing through
// handleApprovedChange, so the refusal has to hold there independently. It must
// refuse BEFORE touching a.db (nil here) — an advisory note is not a statement.
func TestApplyMigration_RefusesAdvisoryDDL(t *testing.T) {
	a := &Agent{}
	err := a.applyMigration(context.Background(), "11111111-1111-1111-1111-111111111111",
		"-- drift: table public.shipments appeared in source")
	if err == nil {
		t.Fatal("applyMigration accepted an advisory drift note; it would be sent to the destination and report a successful apply that never happened")
	}
}
