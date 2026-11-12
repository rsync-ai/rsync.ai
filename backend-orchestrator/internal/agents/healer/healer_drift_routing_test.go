package healer

// One drift policy for batch and CDC: only a dropped source table waits for a person.
// A new column or a type change is applied on the spot, a dropped column is recorded
// and the destination keeps it, and a notice about the source is recorded. Approving a
// dropped table drops the destination table through the connector; the approval row in
// the database, not the message, is what allows it.
//
// The class these tests pin: a schema change that asks nothing of the user must never
// wait for them, and the one change that deletes data must never happen without them.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/IBM/sarama"
	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
)

const routingPipelineID = "11111111-1111-1111-1111-111111111111"

func TestRouteSchemaChange(t *testing.T) {
	cases := []struct {
		name string
		sc   SchemaChange
		want driftRoute
	}{
		{"new column", SchemaChange{ChangeType: "add_column", DDL: "ALTER TABLE s.t ADD COLUMN c text"}, routeApply},
		{"type change", SchemaChange{ChangeType: "modify_column", DDL: "ALTER TABLE s.t ALTER COLUMN c TYPE bigint"}, routeApply},
		{"dropped column, as a note", SchemaChange{ChangeType: "drop_column", DDL: DroppedColumnNote("s.t", "c")}, routeRecord},
		// Producers from before this change still send the statement. It must be
		// recorded, never queued and never run.
		{"dropped column, legacy statement", SchemaChange{ChangeType: "drop_column", DDL: "ALTER TABLE s.t DROP COLUMN c"}, routeRecord},
		{"dropped column, statement without a change type", SchemaChange{DDL: "ALTER TABLE s.t DROP COLUMN IF EXISTS c;"}, routeRecord},
		{"new source table", SchemaChange{ChangeType: "create_table", DDL: "-- drift: table s.t appeared in source"}, routeRecord},
		{"declared type change note", SchemaChange{ChangeType: "modify_column", DDL: "-- drift: s.t.c declared type changed in the source"}, routeRecord},
		{"dropped table, by change type", SchemaChange{ChangeType: "drop_table", DDL: "DROP TABLE rsync_s_1a2b3c4d.t"}, routeAwaitDecision},
		{"dropped table, by statement", SchemaChange{DDL: "  drop table s.t"}, routeAwaitDecision},
		{"dropped table, no statement", SchemaChange{ChangeType: "DROP_TABLE"}, routeAwaitDecision},
		{"truncate is never run automatically", SchemaChange{DDL: "TRUNCATE TABLE s.t"}, routeAwaitDecision},
		{"a column named drop_* is still a new column", SchemaChange{ChangeType: "add_column", DDL: "ALTER TABLE s.t ADD COLUMN drop_reason text"}, routeApply},
	}
	for _, tc := range cases {
		if got := routeSchemaChange(tc.sc); got != tc.want {
			t.Errorf("%s: routeSchemaChange(%+v) = %v, want %v", tc.name, tc.sc, got, tc.want)
		}
	}
}

func TestParseDropColumnDDL(t *testing.T) {
	cases := []struct {
		ddl        string
		wantOK     bool
		table, col string
	}{
		{"ALTER TABLE public.orders DROP COLUMN note", true, "public.orders", "note"},
		{"alter table t drop column c;", true, "t", "c"},
		{"ALTER TABLE t DROP COLUMN IF EXISTS c", true, "t", "c"},
		{"ALTER TABLE t DROP COLUMN c, DROP COLUMN d", false, "", ""},
		{"ALTER TABLE t DROP CONSTRAINT k", false, "", ""},
		{"ALTER TABLE t ADD COLUMN c text", false, "", ""},
		{"DROP TABLE t", false, "", ""},
		{"", false, "", ""},
	}
	for _, tc := range cases {
		table, col, ok := parseDropColumnDDL(tc.ddl)
		if ok != tc.wantOK || table != tc.table || col != tc.col {
			t.Errorf("parseDropColumnDDL(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.ddl, table, col, ok, tc.table, tc.col, tc.wantOK)
		}
	}
}

// The note is the one recorded form of a dropped column, whichever path reported it:
// the UNIQUE (pipeline_id, ddl) row must be the same row, and nothing may run it.
func TestDroppedColumnNote(t *testing.T) {
	note := DroppedColumnNote(" public.orders ", " note ")
	if want := "-- drift: column public.orders.note was dropped in the source. The destination keeps the column; rsync.ai no longer writes values to it."; note != want {
		t.Fatalf("DroppedColumnNote = %q, want %q", note, want)
	}
	if !isAdvisoryDDL(note) || !notAutoAppliedDDL(note) {
		t.Errorf("the dropped-column note must read as advisory, or approving an old row would run it: %q", note)
	}
	if err := (&Agent{}).applyMigration(context.Background(), routingPipelineID, note); err == nil {
		t.Error("applyMigration accepted the dropped-column note")
	}

	for _, sc := range []SchemaChange{
		{ChangeType: "drop_column", Table: "public.orders", DDL: "ALTER TABLE public.orders DROP COLUMN note"},
		{ChangeType: "drop_column", Table: "public.orders", ColumnName: "note", DDL: "ALTER TABLE other.name DROP COLUMN ignored"},
		{Table: "public.orders", DDL: "ALTER TABLE public.orders DROP COLUMN IF EXISTS note;"},
	} {
		got := sc
		normalizeDroppedColumn(&got)
		if got.DDL != DroppedColumnNote("public.orders", "note") || got.ColumnName != "note" {
			t.Errorf("normalizeDroppedColumn(%+v) = DDL %q column %q, want the note for public.orders.note", sc, got.DDL, got.ColumnName)
		}
	}

	already := SchemaChange{ChangeType: "drop_column", Table: "public.orders", ColumnName: "note", DDL: note}
	normalizeDroppedColumn(&already)
	if already.DDL != note {
		t.Errorf("normalizeDroppedColumn rewrote a note that was already normalized: %q", already.DDL)
	}
}

// ── sqlmock helpers ─────────────────────────────────────────────────────────

// containsArg matches a string query argument that contains the given text.
type containsArg string

func (c containsArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, string(c))
}

// notNilArg matches any non-NULL query argument.
type notNilArg struct{}

func (notNilArg) Match(v driver.Value) bool { return v != nil }

func q(fragment string) string { return regexp.QuoteMeta(fragment) }

func newMockAgent(t *testing.T) (*Agent, sqlmock.Sqlmock, *recordingExecutor) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	exec := &recordingExecutor{}
	return &Agent{db: db, mcpClient: exec, loadDestination: postgresDestination}, mock, exec
}

func expectNamespace(mock sqlmock.Sqlmock, ns string) {
	mock.ExpectQuery(q("config->>'destination_namespace'")).
		WithArgs(routingPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"ns"}).AddRow(ns))
}

func schemaChangeMessage(t *testing.T, sc map[string]interface{}) *sarama.ConsumerMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"event_type":    "SCHEMA_CHANGE_DETECTED",
		"pipeline_id":   routingPipelineID,
		"schema_change": sc,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &sarama.ConsumerMessage{Value: payload}
}

// ── changes handled without a person ────────────────────────────────────────

// A dropped column, including one reported as the old DROP COLUMN statement, is filed as
// the note, marked decided by the system, and announced once. Nothing reaches the
// destination: it keeps the column.
func TestDroppedColumnIsRecordedAndKept(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	sent := captureNotifications(a)
	note := DroppedColumnNote("public.orders", "note")

	mock.ExpectExec(q("INSERT INTO schema_change_approvals")).
		WithArgs(routingPipelineID, "drop_column", "public.orders", note, sqlmock.AnyArg(),
			"approved", appliedBySchemaDrift, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "drop_column", "public.orders", "skipped", sqlmock.AnyArg(), "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	msg := schemaChangeMessage(t, map[string]interface{}{
		"change_type": "drop_column",
		"table":       "public.orders",
		"ddl":         "ALTER TABLE public.orders DROP COLUMN note",
	})
	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleSchemaChangeMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 0 {
		t.Errorf("a dropped column reached the destination connector: %v", exec.operations())
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].UserMessage, "Column note was dropped from public.orders") {
		t.Errorf("want one kept-column notification, got %+v", *sent)
	}
	wantNoticeCode(t, *sent, diagnose.CodeSchemaDriftHandled)
}

// The CDC sink reports a dropped column as already handled, with no statement at all
// (kafka-mcp-sink reportDroppedColumns). The healer files its own note as the DDL, the
// same row the batch detector files, and runs nothing.
func TestSinkReportedDroppedColumnIsFiledAsTheNote(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	sent := captureNotifications(a)
	note := DroppedColumnNote("public.orders", "note")

	mock.ExpectExec(q("INSERT INTO schema_change_approvals")).
		WithArgs(routingPipelineID, "drop_column", "public.orders", note, sqlmock.AnyArg(),
			"approved", appliedByCDCSink, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "drop_column", "public.orders", "skipped", sqlmock.AnyArg(), "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	msg := schemaChangeMessage(t, map[string]interface{}{
		"change_type": "drop_column",
		"table":       "public.orders",
		"column_name": "note",
		"applied":     true,
	})
	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleSchemaChangeMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 0 {
		t.Errorf("a dropped column reached the destination connector: %v", exec.operations())
	}
	wantNoticeCode(t, *sent, diagnose.CodeSchemaDriftHandled)
}

// The healer changes column definitions only on PostgreSQL and MySQL. Elsewhere a new
// column is recorded, not reported as a failure: nothing went wrong, and the user has
// nothing to do.
func TestNewColumnOnUnsupportedDestinationIsRecorded(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	a.loadDestination = func(context.Context, string) (string, map[string]string, error) {
		return "bigquery", map[string]string{}, nil
	}
	sent := captureNotifications(a)
	ddl := "ALTER TABLE public.orders ADD COLUMN email string"

	mock.ExpectExec(q("INSERT INTO schema_change_approvals")).
		WithArgs(routingPipelineID, "add_column", "public.orders", ddl, containsArg("PostgreSQL and MySQL"),
			"approved", appliedBySchemaDrift, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "add_column", "public.orders", "skipped", sqlmock.AnyArg(), "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	msg := schemaChangeMessage(t, map[string]interface{}{
		"change_type": "add_column", "table": "public.orders", "column_name": "email", "ddl": ddl,
	})
	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleSchemaChangeMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 0 {
		t.Errorf("an unsupported destination reached the connector: %v", exec.operations())
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].UserMessage, "Schema change recorded") {
		t.Errorf("want one recorded notification, got %+v", *sent)
	}
	wantNoticeCode(t, *sent, diagnose.CodeSchemaDriftHandled)
}

// A type change the connector refuses (a narrowing) is filed as failed with the
// connector's reason, and the user is told it could not be applied.
func TestRefusedTypeChangeIsReportedFailed(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	exec.resp = &mcp.ExecuteResponse{Success: false, Error: "column c: bigint to int is not a safe widening"}
	sent := captureNotifications(a)
	ddl := "ALTER TABLE public.orders ALTER COLUMN c TYPE int"

	expectNamespace(mock, "rsync_public_1a2b3c4d")
	mock.ExpectExec(q("INSERT INTO schema_change_approvals")).
		WithArgs(routingPipelineID, "modify_column", "public.orders", ddl, sqlmock.AnyArg(),
			"failed", appliedBySchemaDrift, nil, containsArg("not a safe widening")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "modify_column", "public.orders", "failed", sqlmock.AnyArg(), "", containsArg("not a safe widening")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	msg := schemaChangeMessage(t, map[string]interface{}{
		"change_type": "modify_column", "table": "public.orders", "column_name": "c", "ddl": ddl,
	})
	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleSchemaChangeMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 1 {
		t.Fatalf("connector calls = %v, want one ensure_table", exec.operations())
	}
	p := exec.requests[0].Params
	if exec.requests[0].Operation != "ensure_table" || p["table"] != "orders" || p["namespace"] != "rsync_public_1a2b3c4d" || p["strict_type_change"] != true {
		t.Errorf("ensure_table request = %s %+v, want table=orders namespace=rsync_public_1a2b3c4d strict", exec.requests[0].Operation, p)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].UserMessage, "could not be applied automatically") {
		t.Errorf("want one could-not-apply notification, got %+v", *sent)
	}
	wantNoticeCode(t, *sent, diagnose.CodeSchemaDriftApplyFailed)
}

// An applied change is filed with applied_at set and announced once.
func TestNewColumnIsAppliedAndFiled(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	sent := captureNotifications(a)
	ddl := "ALTER TABLE public.orders ADD COLUMN email string"

	expectNamespace(mock, "rsync_public_1a2b3c4d")
	mock.ExpectExec(q("INSERT INTO schema_change_approvals")).
		WithArgs(routingPipelineID, "add_column", "public.orders", ddl, sqlmock.AnyArg(),
			"applied", appliedBySchemaDrift, notNilArg{}, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "add_column", "public.orders", "applied", sqlmock.AnyArg(), ddl, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	msg := schemaChangeMessage(t, map[string]interface{}{
		"change_type": "add_column", "table": "public.orders", "column_name": "email", "ddl": ddl,
	})
	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("handleSchemaChangeMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if ops := exec.operations(); len(ops) != 1 || ops[0] != "ensure_table" {
		t.Errorf("connector calls = %v, want [ensure_table]", ops)
	}
	if len(*sent) != 1 {
		t.Errorf("got %d notifications, want 1", len(*sent))
	}
	wantNoticeCode(t, *sent, diagnose.CodeSchemaDriftHandled)
}

// wantNoticeCode pins the notification code. The notifier gives Slack Approve/Reject
// buttons to SCHEMA_DRIFT_DETECTED alone, and a click resolves to whatever change is
// pending on the pipeline, so a handled change carrying that code would offer to
// approve or reject something else entirely.
func wantNoticeCode(t *testing.T, sent []*diagnose.StructuredError, want string) {
	t.Helper()
	if len(sent) != 1 {
		return // the count is asserted by the caller
	}
	if got := sent[0].Code; got != want {
		t.Errorf("notification code = %q, want %q", got, want)
	}
}

// ── the one change that waits: a dropped table ──────────────────────────────

// A batch pipeline re-detects a vanished table on every run. The first detection queues
// it and tells the user; a re-detection while it is still pending (the upsert returns no
// row) records nothing and says nothing.
func TestDroppedTableWaitsForDecisionAndIsAnnouncedOnce(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	sent := captureNotifications(a)
	ddl := "DROP TABLE rsync_public_1a2b3c4d.orders"
	msg := schemaChangeMessage(t, map[string]interface{}{
		"change_type": "drop_table", "table": "public.orders", "ddl": ddl,
		"detected_at": "2026-09-28T10:00:00Z",
	})

	pending := mock.ExpectQuery(q("INSERT INTO schema_change_approvals") + "(.|\\n)*" +
		q("WHERE schema_change_approvals.status IN ('rejected', 'applied', 'failed', 'withdrawn')") + "(.|\\n)*" +
		q("RETURNING id"))
	pending.WithArgs(routingPipelineID, "drop_table", "public.orders", ddl,
		containsArg("Approve drops the destination table"),
		containsArg("permanently deletes the destination table"), notNilArg{}).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-1"))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "drop_table", "public.orders", "pending_approval", sqlmock.AnyArg(), "", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("first detection: %v", err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].UserMessage, "Source table public.orders was dropped") {
		t.Fatalf("first detection: want one dropped-table notification, got %+v", *sent)
	}
	wantNoticeCode(t, *sent, diagnose.CodeSchemaDriftDetected)

	// Still pending: ON CONFLICT … WHERE matches nothing, so RETURNING yields no row.
	mock.ExpectQuery(q("INSERT INTO schema_change_approvals")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	if err := a.handleSchemaChangeMessage(context.Background(), msg); err != nil {
		t.Fatalf("re-detection: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Errorf("a re-detected pending table drop was announced again: %d notifications", len(*sent))
	}
	if len(exec.requests) != 0 {
		t.Errorf("a dropped table reached the destination connector before anyone decided: %v", exec.operations())
	}
}

func approvedDropMessage(t *testing.T) *sarama.ConsumerMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"event_type":  "schema_change_approved",
		"approval_id": "appr-1",
		"pipeline_id": routingPipelineID,
		"change_type": "drop_table",
		"table_name":  "public.orders",
		"ddl":         "DROP TABLE rsync_public_1a2b3c4d.orders",
		"approved_by": "alice@example.com",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &sarama.ConsumerMessage{Value: payload}
}

func expectApprovedRow(mock sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(q("SELECT table_name FROM schema_change_approvals")+"(.|\\n)*"+q("status = 'approved'")).
		WithArgs("appr-1", routingPipelineID)
}

// Approving a dropped table drops the destination table through the connector's
// drop_table, with the bare table, the pipeline's namespace and the pipeline id — the
// same call a reload makes — and marks the row applied.
func TestApprovedTableDropRunsConnectorDropTable(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	exec.resp = &mcp.ExecuteResponse{Success: true, Result: map[string]interface{}{"dropped": true}}
	sent := captureNotifications(a)

	expectApprovedRow(mock).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("public.orders"))
	expectNamespace(mock, "rsync_public_1a2b3c4d")
	mock.ExpectExec(q("SET status = 'applied', applied_at = NOW()")).
		WithArgs("appr-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT INTO healing_history")).
		WithArgs(routingPipelineID, "drop_table", "public.orders", "applied",
			containsArg("destination table was dropped"), "DROP TABLE rsync_public_1a2b3c4d.orders", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := a.handleApprovedChange(context.Background(), approvedDropMessage(t)); err != nil {
		t.Fatalf("handleApprovedChange: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 1 {
		t.Fatalf("connector calls = %v, want one drop_table", exec.operations())
	}
	req := exec.requests[0]
	if req.Connector != "postgresql" || req.Operation != "drop_table" ||
		req.Params["table"] != "orders" || req.Params["namespace"] != "rsync_public_1a2b3c4d" ||
		req.Params["pipeline_id"] != routingPipelineID {
		t.Errorf("drop request = %s %s %+v, want postgresql drop_table table=orders namespace=rsync_public_1a2b3c4d pipeline_id", req.Connector, req.Operation, req.Params)
	}
	if len(*sent) != 0 {
		t.Errorf("a successful approved drop raised a notification: %+v", *sent)
	}
}

// A drop the connector cannot run marks the row failed and tells the user why.
func TestApprovedTableDropFailureIsReported(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	exec.err = errors.New("connector has no drop_table")
	sent := captureNotifications(a)

	expectApprovedRow(mock).WillReturnRows(sqlmock.NewRows([]string{"table_name"}).AddRow("public.orders"))
	expectNamespace(mock, "")
	mock.ExpectExec(q("SET status = 'failed', error_message = $1")).
		WithArgs(containsArg("connector has no drop_table"), "appr-1").WillReturnResult(sqlmock.NewResult(0, 1))

	if err := a.handleApprovedChange(context.Background(), approvedDropMessage(t)); err != nil {
		t.Fatalf("handleApprovedChange: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0].UserMessage, "Approved table drop failed") {
		t.Errorf("want one drop-failed notification, got %+v", *sent)
	}
}

// The message alone never drops a table. Without an approved row in the database —
// pending, rejected, another pipeline's, or no database at all — nothing is dropped.
func TestTableDropWithoutApprovedRowDropsNothing(t *testing.T) {
	a, mock, exec := newMockAgent(t)
	expectApprovedRow(mock).WillReturnRows(sqlmock.NewRows([]string{"table_name"}))

	if err := a.handleApprovedChange(context.Background(), approvedDropMessage(t)); err != nil {
		t.Fatalf("handleApprovedChange: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 0 {
		t.Errorf("a table was dropped without an approved row: %v", exec.operations())
	}

	noDB := &recordingExecutor{}
	b := &Agent{mcpClient: noDB, loadDestination: postgresDestination}
	if err := b.handleApprovedChange(context.Background(), approvedDropMessage(t)); err != nil {
		t.Fatalf("handleApprovedChange without a db: %v", err)
	}
	if len(noDB.requests) != 0 {
		t.Errorf("a table was dropped with no database to check the approval in: %v", noDB.operations())
	}
}
