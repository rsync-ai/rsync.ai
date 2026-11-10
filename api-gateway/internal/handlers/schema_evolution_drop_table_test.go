package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// A dropped source table is the one schema change that waits for a decision, and
// both decisions take the table out of the pipeline: approve also drops the
// destination table (via the healer), reject keeps it. These pin that, and that a
// decision whose table removal fails is not saved — the row goes back to pending.

const dropTestTable = "public.orders"
const dropTestDDL = "DROP TABLE public.orders"

// expectSelectedTables stamps the read of config.selected_tables.
func expectSelectedTables(mock sqlmock.Sqlmock, pipelineID, raw string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(config->'selected_tables', '[]'::jsonb)::text FROM pipelines WHERE id = $1::uuid`)).
		WithArgs(pipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"selected"}).AddRow(raw))
}

// expectIsCDC stamps the pipeline's sync-mode check.
func expectIsCDC(mock sqlmock.Sqlmock, pipelineID string, cdc bool) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS (SELECT 1 FROM pipelines p WHERE p.id = $1::uuid AND ` + pipelineRowIsCDCSQL + `)`)).
		WithArgs(pipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(cdc))
}

// expectSaveSelection stamps the compare-and-swap write of the kept selection:
// the new list, and the list it was computed from as the guard. The guard clause
// is matched too — without it a concurrent table edit is silently overwritten.
func expectSaveSelection(mock sqlmock.Sqlmock, pipelineID, keptJSON, readRaw string, rows int64) {
	mock.ExpectExec(`UPDATE pipelines(?s).*`+regexp.QuoteMeta(`AND COALESCE(config->'selected_tables', '[]'::jsonb) = $3::jsonb`)).
		WithArgs(keptJSON, pipelineID, readRaw).
		WillReturnResult(sqlmock.NewResult(0, rows))
}

// expectReopen stamps putting a decided row back to pending.
func expectReopen(mock sqlmock.Sqlmock, changeID, pipelineID, decidedStatus string) {
	mock.ExpectExec(regexp.QuoteMeta(`SET status = 'pending', reviewed_by = NULL`)).
		WithArgs(changeID, pipelineID, decidedStatus).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func decideDropTable(t *testing.T, handler gin.HandlerFunc, verb string) *httptest.ResponseRecorder {
	t.Helper()
	r := newApproveRouter(handler, "alice@example.com", "user-A")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/pipelines/"+schemaEvoTestPipelineUUID+"/schema-changes/change-1/"+verb, nil))
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, w.Body.String())
	}
	return body
}

// Approving a dropped table on a batch pipeline takes the table out of
// selected_tables, then asks the healer to drop the destination table.
func TestApproveDropTable_Batch_RemovesTableThenDispatches(t *testing.T) {
	t.Setenv("RSYNC_SCHEMA_DRIFT_ENABLED", "true")
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, kafka *fakeKafka) {
		raw := `["public.orders", "public.customers"]`
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, raw)
		expectIsCDC(mock, schemaEvoTestPipelineUUID, false)
		expectSaveSelection(mock, schemaEvoTestPipelineUUID, `["public.customers"]`, raw, 1)
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 0)

		w := decideDropTable(t, ApproveSchemaChange, "approve")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		for _, k := range []string{"drop_table", "table_removed", "auto_applicable"} {
			if body[k] != true {
				t.Errorf("%s = %v, want true: %s", k, body[k], w.Body.String())
			}
		}
		if len(kafka.calls) != 1 || kafka.calls[0] != "rsync.healer.approved-changes" {
			t.Errorf("approving a dropped table must ask the healer to drop the destination table; got %v", kafka.calls)
		}
	})
}

// Rejecting a dropped table still takes it out of the pipeline — the source no
// longer has it — but never asks the healer for anything: the destination table
// is kept.
func TestRejectDropTable_Batch_RemovesTableKeepsDestination(t *testing.T) {
	t.Setenv("RSYNC_SCHEMA_DRIFT_ENABLED", "true")
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, kafka *fakeKafka) {
		// A bare selected name matches the qualified table the detector filed.
		raw := `["orders", "customers"]`
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, raw)
		expectIsCDC(mock, schemaEvoTestPipelineUUID, false)
		expectSaveSelection(mock, schemaEvoTestPipelineUUID, `["customers"]`, raw, 1)
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 0)

		w := decideDropTable(t, RejectSchemaChange, "reject")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		if body["status"] != "rejected" || body["drop_table"] != true || body["table_removed"] != true {
			t.Errorf("want rejected + drop_table + table_removed, got %s", w.Body.String())
		}
		if len(kafka.calls) != 0 {
			t.Errorf("reject must not publish to the healer; got %v", kafka.calls)
		}
	})
}

// A table that is already out of the selection needs no write, and the decision
// still succeeds: running it twice, or after a manual table edit, is harmless.
func TestApproveDropTable_AlreadyUnselected_Succeeds(t *testing.T) {
	t.Setenv("RSYNC_SCHEMA_DRIFT_ENABLED", "true")
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, kafka *fakeKafka) {
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, `["public.customers"]`)
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 0)

		w := decideDropTable(t, ApproveSchemaChange, "approve")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if body := decodeBody(t, w); body["table_removed"] != true {
			t.Errorf("an unselected table is out of the pipeline: table_removed = %v", body["table_removed"])
		}
		if len(kafka.calls) != 1 {
			t.Errorf("the destination table is still dropped; got %v", kafka.calls)
		}
	})
}

// Removing the only selected table would leave a pipeline that syncs nothing.
// The decision is refused with 409 and the row goes back to pending, so the badge
// keeps asking and the healer is never told to drop anything.
func TestApproveDropTable_LastSelectedTable_409AndReopens(t *testing.T) {
	t.Setenv("RSYNC_SCHEMA_DRIFT_ENABLED", "true")
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, kafka *fakeKafka) {
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, `["orders"]`)
		expectReopen(mock, "change-1", schemaEvoTestPipelineUUID, "approved")
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 1)

		w := decideDropTable(t, ApproveSchemaChange, "approve")
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
		if body := decodeBody(t, w); body["error"] != "last_selected_table" {
			t.Errorf("error = %v, want last_selected_table", body["error"])
		}
		if len(kafka.calls) != 0 {
			t.Errorf("a refused decision must not reach the healer; got %v", kafka.calls)
		}
	})
}

// The selection is written with a compare-and-swap on the list it was computed
// from. A table edit in between makes the write match nothing: the decision is
// refused and reopened rather than overwriting the edit.
func TestRejectDropTable_SelectionChangedMeanwhile_Reopens(t *testing.T) {
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, _ *fakeKafka) {
		raw := `["public.orders", "public.customers"]`
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, raw)
		expectIsCDC(mock, schemaEvoTestPipelineUUID, false)
		expectSaveSelection(mock, schemaEvoTestPipelineUUID, `["public.customers"]`, raw, 0)
		expectReopen(mock, "change-1", schemaEvoTestPipelineUUID, "rejected")

		w := decideDropTable(t, RejectSchemaChange, "reject")
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
		if body := decodeBody(t, w); body["error"] != "table_removal_failed" {
			t.Errorf("error = %v, want table_removal_failed", body["error"])
		}
	})
}

// cdcStubs stands in for Kafka Connect and the orchestrator. connectors is what
// GET /connectors lists; pushStatus is the orchestrator's answer to the table
// list, whose last body is recorded.
type cdcStubs struct {
	mu          sync.Mutex
	pushed      [][]string
	sinkRestart int
}

func startCDCStubs(t *testing.T, connectors []string, pushStatus int) *cdcStubs {
	t.Helper()
	s := &cdcStubs{}
	connect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/connectors" {
			_ = json.NewEncoder(w).Encode(connectors)
			return
		}
		http.NotFound(w, r)
	}))
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/cdc/tables":
			var body struct {
				Tables []string `json:"tables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.pushed = append(s.pushed, body.Tables)
			w.WriteHeader(pushStatus)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sink/restart"):
			s.sinkRestart++
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(connect.Close)
	t.Cleanup(orch.Close)
	t.Setenv("KAFKA_CONNECT_URL", connect.URL)
	t.Setenv("ORCHESTRATOR_URL", orch.URL)
	return s
}

// A CDC pipeline's connector gets the new list before it is saved (the
// UpdatePipelineCDCTables order), then the sink restarts off the table's topic.
func TestRejectDropTable_CDC_PushesListThenSaves(t *testing.T) {
	stubs := startCDCStubs(t, []string{"cdc-t1-" + schemaEvoTestPipelineUUID}, http.StatusOK)
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, _ *fakeKafka) {
		raw := `["public.orders", "public.customers"]`
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, raw)
		expectIsCDC(mock, schemaEvoTestPipelineUUID, true)
		expectSaveSelection(mock, schemaEvoTestPipelineUUID, `["public.customers"]`, raw, 1)
		mock.ExpectExec(regexp.QuoteMeta(`'{`+cdcStreamingOnlyTablesKey+`}'`)).
			WithArgs(`[]`, `["public.orders"]`, schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 0)

		w := decideDropTable(t, RejectSchemaChange, "reject")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
	stubs.mu.Lock()
	defer stubs.mu.Unlock()
	if want := [][]string{{"public.customers"}}; !reflect.DeepEqual(stubs.pushed, want) {
		t.Errorf("orchestrator got %v, want %v", stubs.pushed, want)
	}
	if stubs.sinkRestart != 1 {
		t.Errorf("sink restarts = %d, want 1", stubs.sinkRestart)
	}
}

// When the orchestrator refuses the list, nothing is saved: the selection is not
// written (sqlmock would answer an unexpected write with an error, turning the
// response into the save-failure message), and the row is reopened.
func TestApproveDropTable_CDC_OrchestratorRefuses_NothingSaved(t *testing.T) {
	t.Setenv("RSYNC_SCHEMA_DRIFT_ENABLED", "true")
	startCDCStubs(t, []string{"cdc-t1-" + schemaEvoTestPipelineUUID}, http.StatusBadRequest)
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, kafka *fakeKafka) {
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, `["public.orders", "public.customers"]`)
		expectIsCDC(mock, schemaEvoTestPipelineUUID, true)
		expectReopen(mock, "change-1", schemaEvoTestPipelineUUID, "approved")
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 1)

		w := decideDropTable(t, ApproveSchemaChange, "approve")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		if body["error"] != "table_removal_failed" || !strings.Contains(body["message"].(string), "refused") {
			t.Errorf("want the orchestrator refusal surfaced, got %s", w.Body.String())
		}
		if len(kafka.calls) != 0 {
			t.Errorf("a refused decision must not reach the healer; got %v", kafka.calls)
		}
	})
}

// A CDC pipeline with no connector yet has only its saved selection to change.
func TestRejectDropTable_CDC_NoConnectorYet_SavesOnly(t *testing.T) {
	stubs := startCDCStubs(t, []string{"some-other-connector"}, http.StatusOK)
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, _ *fakeKafka) {
		raw := `["public.orders", "public.customers"]`
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE schema_change_approvals`)).
			WithArgs("alice@example.com", "change-1", schemaEvoTestPipelineUUID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDecidedRow(mock, "change-1", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL)
		expectSelectedTables(mock, schemaEvoTestPipelineUUID, raw)
		expectIsCDC(mock, schemaEvoTestPipelineUUID, true)
		expectSaveSelection(mock, schemaEvoTestPipelineUUID, `["public.customers"]`, raw, 1)
		mock.ExpectExec(regexp.QuoteMeta(`'{` + cdcStreamingOnlyTablesKey + `}'`)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectDriftBadgeClear(mock, schemaEvoTestPipelineUUID, 0)

		w := decideDropTable(t, RejectSchemaChange, "reject")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
	stubs.mu.Lock()
	defer stubs.mu.Unlock()
	if len(stubs.pushed) != 0 {
		t.Errorf("no connector: nothing to push, got %v", stubs.pushed)
	}
}

// The approval list must not tell the user that approving a dropped table only
// records the decision: approving it drops the destination table.
func TestListSchemaChanges_DropTableIsAutoApplicable(t *testing.T) {
	withSchemaEvolutionDeps(t, func(mock sqlmock.Sqlmock, _ *fakeKafka) {
		expectOwnerCheck(mock, schemaEvoTestPipelineUUID, "user-A")
		now := time.Now()
		cols := []string{"id", "pipeline_id", "change_type", "table_name", "ddl", "reasoning", "risks", "user_message",
			"status", "reviewed_by", "reviewed_at", "applied_at", "error_message", "created_at", "updated_at"}
		mock.ExpectQuery(regexp.QuoteMeta(`FROM schema_change_approvals`)).
			WithArgs(schemaEvoTestPipelineUUID).
			WillReturnRows(sqlmock.NewRows(cols).
				AddRow("c-drop", schemaEvoTestPipelineUUID, "drop_table", dropTestTable, dropTestDDL, "", "", "", "pending", nil, nil, nil, nil, now, now).
				AddRow("c-col", schemaEvoTestPipelineUUID, "drop_column", "orders", "ALTER TABLE orders DROP COLUMN x", "", "", "", "pending", nil, nil, nil, nil, now, now))

		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.GET("/api/v1/pipelines/:id/schema-changes", func(c *gin.Context) {
			c.Set("user_id", "user-A")
			c.Set(ctxWorkspaceID, schemaEvoTestWS)
			c.Set(ctxWorkspaceRole, "owner")
			ListPipelineSchemaChanges(c)
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/pipelines/"+schemaEvoTestPipelineUUID+"/schema-changes", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Changes []SchemaChangeApproval `json:"schema_changes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Changes) != 2 {
			t.Fatalf("bad body (%v): %s", err, w.Body.String())
		}
		if !body.Changes[0].AutoApplicable {
			t.Error("a dropped table must be auto_applicable: approving it drops the destination table")
		}
		if body.Changes[1].AutoApplicable {
			t.Error("any other DROP stays record-only")
		}
	})
}

func TestWithoutSelectedTable(t *testing.T) {
	cases := []struct {
		name     string
		selected []string
		table    string
		kept     []string
		removed  bool
	}{
		{"exact", []string{"public.orders", "public.customers"}, "public.orders", []string{"public.customers"}, true},
		{"bare selection, qualified table", []string{"orders", "customers"}, "public.orders", []string{"customers"}, true},
		{"qualified selection, bare table", []string{"shop.orders", "shop.customers"}, "orders", []string{"shop.customers"}, true},
		{"same bare name, other schema", []string{"sales.orders", "public.customers"}, "public.orders", []string{"sales.orders", "public.customers"}, false},
		{"case-sensitive", []string{"Orders", "customers"}, "orders", []string{"Orders", "customers"}, false},
		{"not selected", []string{"public.customers"}, "public.orders", []string{"public.customers"}, false},
		{"blank table matches nothing", []string{"", "public.customers"}, " ", []string{"", "public.customers"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, removed := withoutSelectedTable(tc.selected, tc.table)
			if removed != tc.removed || !reflect.DeepEqual(kept, tc.kept) {
				t.Errorf("withoutSelectedTable(%v, %q) = %v, %v; want %v, %v", tc.selected, tc.table, kept, removed, tc.kept, tc.removed)
			}
		})
	}
}

// isDropTableChange mirrors the healer's predicate of the same name.
func TestIsDropTableChange(t *testing.T) {
	cases := []struct {
		changeType, ddl string
		want            bool
	}{
		{"drop_table", "", true},
		{" DROP_TABLE ", "anything", true},
		{"", "DROP TABLE public.orders", true},
		{"", "  drop table x", true},
		{"drop_column", "ALTER TABLE t DROP COLUMN c", false},
		{"add_column", "ALTER TABLE t ADD COLUMN c int", false},
	}
	for _, tc := range cases {
		if got := isDropTableChange(tc.changeType, tc.ddl); got != tc.want {
			t.Errorf("isDropTableChange(%q, %q) = %v, want %v", tc.changeType, tc.ddl, got, tc.want)
		}
	}
}
