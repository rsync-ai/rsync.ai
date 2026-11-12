package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/notifier"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/shared/crypto"
)

// wfDB is the sqlmock database wsScopeMockDB installed.
func wfDB() *sql.DB { return db.DB }

func mustEncrypt(t *testing.T, s string) string {
	t.Helper()
	enc, err := crypto.EncryptString(s)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

const (
	wfConn    = "aaaaaaaa-0000-4000-8000-0000000000c1"
	wfID      = "ffffffff-0000-4000-8000-0000000000f1"
	wfRunID   = "ffffffff-0000-4000-8000-0000000000r1"
	wfChannel = "cccccccc-0000-4000-8000-0000000000d1"
	wfPipe    = "55555555-5555-5555-5555-5555555555f1"
)

var (
	wfConnSQL     = regexp.QuoteMeta(`SELECT name, connector_type FROM connections WHERE id = $1 AND workspace_id = $2`)
	wfChannelsSQL = regexp.QuoteMeta(`SELECT id, name, kind FROM delivery_channels WHERE workspace_id = $1`)
	wfDraftsSQL   = regexp.QuoteMeta(`SELECT COUNT(*) FROM workflows WHERE workspace_id = $1 AND created_by = $2 AND status = 'draft'`)
)

func wfInput() WorkflowDraftInput {
	return WorkflowDraftInput{
		Name: "Weekly revenue", ConnectionID: wfConn,
		SQL:     "SELECT sum(total) AS revenue FROM gold.orders",
		Trigger: "schedule", Cron: "0 9 * * 1",
		Branches: []WorkflowBranchInput{{Condition: "always", Message: "Revenue: {{revenue}}", Channels: []string{"finance"}}},
	}
}

// A workflow's query is one read statement; nothing else gets through, for
// any role.
func TestWorkflowSQLRefusal(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"WITH t AS (SELECT 1 AS n) SELECT n FROM t",
		"select count(*) from orders where status = 'failed'",
	} {
		if r := workflowSQLRefusal(q, security.WSOwner); r != "" {
			t.Errorf("%q refused: %s", q, r)
		}
	}
	for _, q := range []string{
		"DELETE FROM orders",
		"UPDATE orders SET total = 0",
		"DROP TABLE orders",
		"SELECT 1; DROP TABLE orders",
		"WITH d AS (DELETE FROM orders RETURNING *) SELECT * FROM d",
		"",
		"GRANT ALL ON orders TO public",
	} {
		if r := workflowSQLRefusal(q, security.WSOwner); r == "" {
			t.Errorf("%q was accepted", q)
		}
	}
}

// Only Ask rsync.ai or an agent token drafts, and only for an owner or admin;
// a broken definition is refused before the database.
func TestProposeWorkflowDraft_WhoAndShapeBeforeTheDatabase(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	rest := services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSOwner, Channel: services.ChannelREST}
	_, err := ProposeWorkflowDraft(planCtx(""), rest, wfInput())
	wantRefusal(t, err, "forbidden", http.StatusForbidden)

	_, err = ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSMember), wfInput())
	r := wantRefusal(t, err, "forbidden", http.StatusForbidden)
	if !strings.Contains(r.Message, "owner or admin") {
		t.Fatalf("message = %q", r.Message)
	}

	bad := wfInput()
	bad.Cron = "every monday"
	_, err = ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSAdmin), bad)
	wantRefusal(t, err, "invalid_cron", http.StatusBadRequest)

	bad = wfInput()
	bad.Branches[0].Condition, bad.Branches[0].ConditionOp, bad.Branches[0].Threshold = "value", ">", "lots"
	_, err = ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSAdmin), bad)
	wantRefusal(t, err, "invalid_threshold", http.StatusBadRequest)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The connection must be a SQL one in the workspace, and the SQL one read.
func TestProposeWorkflowDraft_ConnectionAndSQL(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	admin := assistantAs(security.WSAdmin)

	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("events", "mongodb"))
	_, err := ProposeWorkflowDraft(planCtx(""), admin, wfInput())
	wantRefusal(t, err, "connection_not_sql", http.StatusUnprocessableEntity)

	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	write := wfInput()
	write.SQL = "DELETE FROM gold.orders"
	_, err = ProposeWorkflowDraft(planCtx(""), admin, write)
	wantRefusal(t, err, "invalid_sql", http.StatusBadRequest)

	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).WillReturnError(sql.ErrNoRows)
	_, err = ProposeWorkflowDraft(planCtx(""), admin, wfInput())
	wantRefusal(t, err, "connection_not_found", http.StatusNotFound)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A channel name the workspace lacks is refused with the names it has, so the
// agent can pick one; it never creates a channel.
func TestProposeWorkflowDraft_UnknownChannelListsTheRealOnes(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	mock.ExpectQuery(wfChannelsSQL).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).
			AddRow(wfChannel, "#ops", "slack").AddRow("cccccccc-0000-4000-8000-0000000000d2", "Finance team", "email"))
	in := wfInput()
	in.Branches[0].Channels = []string{"#finance"}
	_, err := ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSAdmin), in)
	r := wantRefusal(t, err, "channel_not_found", http.StatusNotFound)
	if !strings.Contains(r.Message, "#ops (slack), Finance team (email)") {
		t.Fatalf("message = %q", r.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A good draft is stored as a draft, with its channels, in one transaction;
// "ops" matches the channel "#ops".
func TestProposeWorkflowDraft_StoresADraft(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	mock.ExpectQuery(wfChannelsSQL).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel, "#ops", "slack"))
	mock.ExpectQuery(wfDraftsSQL).WithArgs(wsScopeWS, wsScopeUser).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	var stored string
	mock.ExpectQuery(`INSERT INTO workflows`).
		WithArgs(wsScopeWS, "Weekly revenue", captureJSONArg(&stored), "schedule", wfConn, sqlmock.AnyArg(), wsScopeUser, "assistant").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfID))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_1", wfChannel, 0).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectWorkflowLoad(mock, "draft")
	calls := stubWorkflowQuery(t, workflow.Result{Columns: []string{"revenue"},
		Rows: []map[string]any{{"revenue": 1250.5}}, RowCount: 1}, nil)

	in := wfInput()
	in.Branches[0].Channels = []string{"ops"}
	w, err := ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSAdmin), in)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID != wfID || w.Status != "draft" || len(w.Channels) != 1 || w.Channels[0].Name != "#ops" {
		t.Fatalf("workflow = %+v", w)
	}
	if *calls != 1 || w.DraftUnchecked {
		t.Fatalf("query ran %d times, unchecked = %v; want one checked run", *calls, w.DraftUnchecked)
	}
	var def workflow.Definition
	if err := json.Unmarshal([]byte(stored), &def); err != nil {
		t.Fatal(err)
	}
	if def.Trigger.Timezone != "UTC" || def.Version != 3 || def.Query().SQL != in.SQL || len(def.Branches()) != 1 ||
		def.Branches()[0].Condition.Type != "always" || def.Branches()[0].Message.Body != "Revenue: {{revenue}}" {
		t.Fatalf("stored definition = %+v", def)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Drafting runs the query once and refuses a draft that cannot work, before
// anything is stored: SQL the database rejects, a slot no column fills, a
// value condition on a column that is missing or not a number. Drafts on prod
// were stored with SQL that did not compile (an aggregate over a LIMIT-cut
// CTE) and failed only at Test. The refusal goes to a model: the database
// error is scrubbed, and no refusal carries a row value.
func TestProposeWorkflowDraft_RunsTheQueryOnce(t *testing.T) {
	failed := errors.New(`the query failed (syntax): pq: column "o.totl" does not exist; ` +
		`invalid input syntax for type integer: "jane@example.com" near 'Jane Doe'`)
	for _, tc := range []struct {
		name      string
		edit      func(*WorkflowDraftInput)
		res       workflow.Result
		err       error
		code      string
		want      []string
		leakCheck []string
	}{
		{name: "the database rejects the SQL", err: failed, code: "query_failed",
			want:      []string{`column "o.totl" does not exist`, "nothing was stored", "describe_schema"},
			leakCheck: []string{"jane", "Jane Doe"}},
		{name: "a slot no column fills",
			res:  workflow.Result{Columns: []string{"total", "orders"}, Rows: []map[string]any{{"total": 9, "orders": 3}}, RowCount: 1},
			code: "invalid_message", want: []string{"{{revenue}}", "total, orders"}},
		{name: "a subject slot no column fills",
			edit: func(in *WorkflowDraftInput) { in.Branches[0].Subject = "Revenue {{week}}" },
			res:  workflow.Result{Columns: []string{"revenue"}, Rows: []map[string]any{{"revenue": 9}}, RowCount: 1},
			code: "invalid_subject", want: []string{"{{week}}", "revenue"}},
		{name: "a value condition on a column that is not a number",
			edit: func(in *WorkflowDraftInput) {
				b := &in.Branches[0]
				b.Condition, b.ConditionColumn, b.ConditionOp, b.Threshold = "value", "status", ">", "5"
				in.SQL, b.Message = "SELECT status FROM gold.orders", "Status: {{status}}"
			},
			res:  workflow.Result{Columns: []string{"status"}, Rows: []map[string]any{{"status": "overdue-ACME"}}, RowCount: 1},
			code: "invalid_condition_column", want: []string{"status in the first row is not a number"},
			leakCheck: []string{"overdue", "ACME"}},
		{name: "a value condition on a missing column, with no rows",
			edit: func(in *WorkflowDraftInput) {
				b := &in.Branches[0]
				b.Condition, b.ConditionColumn, b.ConditionOp, b.Threshold = "value", "failed", ">", "5"
			},
			res:  workflow.Result{Columns: []string{"revenue"}},
			code: "invalid_condition_column", want: []string{`no column "failed"`, "revenue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
				WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
			mock.ExpectQuery(wfChannelsSQL).WithArgs(wsScopeWS).
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel, "finance", "slack"))
			calls := stubWorkflowQuery(t, tc.res, tc.err)

			in := wfInput()
			if tc.edit != nil {
				tc.edit(&in)
			}
			_, err := ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSAdmin), in)
			r := wantRefusal(t, err, tc.code, http.StatusUnprocessableEntity)
			for _, w := range tc.want {
				if !strings.Contains(r.Message, w) {
					t.Errorf("message %q lacks %q", r.Message, w)
				}
			}
			for _, leak := range tc.leakCheck {
				if strings.Contains(r.Message, leak) {
					t.Errorf("message %q carries the value %q", r.Message, leak)
				}
			}
			if *calls != 1 {
				t.Fatalf("query ran %d times, want 1", *calls)
			}
			// Nothing stored: no draft count, no INSERT.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A query still running at the check's deadline is not refused: a slow
// warehouse is not a broken draft. The draft is stored and marked unchecked,
// so the tool tells the person to click Test.
func TestProposeWorkflowDraft_SlowQueryIsStoredUnchecked(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	prevTimeout := workflowDraftCheckTimeout
	workflowDraftCheckTimeout = 20 * time.Millisecond
	prevQuery := workflowQuery
	workflowQuery = func(ctx context.Context, _ *sql.DB, _, _, _ string, _ security.WorkspaceRole) (workflow.Result, int64, error) {
		<-ctx.Done()
		return workflow.Result{}, 20, errors.New("the query failed (timeout): canceling statement due to user request")
	}
	t.Cleanup(func() { workflowDraftCheckTimeout, workflowQuery = prevTimeout, prevQuery })

	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	mock.ExpectQuery(wfChannelsSQL).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel, "finance", "slack"))
	mock.ExpectQuery(wfDraftsSQL).WithArgs(wsScopeWS, wsScopeUser).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO workflows`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfID))
	mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_1", wfChannel, 0).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectWorkflowLoad(mock, "draft")

	w, err := ProposeWorkflowDraft(planCtx(""), assistantAs(security.WSAdmin), wfInput())
	if err != nil {
		t.Fatal(err)
	}
	if !w.DraftUnchecked {
		t.Fatal("a draft whose query outlived the check must be marked unchecked")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func wfDefinitionJSON(cond workflow.Condition, quiet int) string {
	return wfBranchesJSON(quiet, workflow.Branch{Condition: cond,
		Message: workflow.Message{Subject: "Failed payments", Body: "{{failed}} failed <!channel>"}})
}

// wfBranchesJSON is a stored definition with these branches.
func wfBranchesJSON(quiet int, branches ...workflow.Branch) string {
	def := workflow.FromBranches(workflow.Trigger{Type: "schedule", Cron: "0 9 * * 1", Timezone: "UTC"},
		workflow.Query{ConnectionID: wfConn, SQL: "SELECT count(*) AS failed FROM payments"}, branches, quiet)
	b, _ := json.Marshal(def)
	return string(b)
}

// expectWorkflowLoad answers loadWorkflow: the row, its channels, its last run.
func expectWorkflowLoad(mock sqlmock.Sqlmock, status string) {
	expectWorkflowLoadOf(mock, status, wfDefinitionJSON(workflow.Condition{Type: "always"}, 0), wfConn, wfUpdatedAt)
}

// wfUpdatedAt is the stored workflow's updated_at, which an edit must name.
var wfUpdatedAt = time.Date(2026, 10, 2, 9, 30, 0, 123456000, time.UTC)

// expectWorkflowLoadOf answers loadWorkflow with this definition; a nil
// connection is one that was deleted.
func expectWorkflowLoadOf(mock sqlmock.Sqlmock, status, definition string, conn any, updatedAt time.Time) {
	cname, ctype := "warehouse", "postgresql"
	if conn == nil {
		cname, ctype = "", ""
	}
	mock.ExpectQuery(`FROM workflows w`).WithArgs(wfID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "paused_reason", "definition", "next_run_at",
			"proposed_by", "created_at", "updated_at", "connection_id", "cname", "ctype", "pipeline_id", "pname"}).
			AddRow(wfID, "Weekly revenue", status, "", definition,
				nil, "assistant", updatedAt, updatedAt, conn, cname, ctype, nil, ""))
	mock.ExpectQuery(`FROM workflow_channels wc JOIN delivery_channels dc`).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "id", "name", "kind", "node_id"}).AddRow(wfID, wfChannel, "#ops", "slack", "send_1"))
	mock.ExpectQuery(`SELECT DISTINCT ON \(r.workflow_id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "id", "trigger", "status", "condition_met", "row_count",
			"deliveries", "notes", "error", "started_at", "finished_at", "branch", "stop_requested"}))
}

func wfRun(cond workflow.Condition, quiet int) *workflowRunRow {
	var def workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(cond, quiet)), &def)
	return &workflowRunRow{RunID: wfRunID, Trigger: "schedule", WorkflowID: wfID, WorkspaceID: wsScopeWS,
		Name: "Failed payments", Status: "active", Definition: def, ConnectionID: wfConn, RunAsUserID: wsScopeUser}
}

func expectRole(mock sqlmock.Sqlmock, role string) {
	q := mock.ExpectQuery(`SELECT role FROM workspace_members`).WithArgs(wsScopeWS, wsScopeUser)
	if role == "" {
		q.WillReturnError(sql.ErrNoRows)
		return
	}
	q.WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
}

func stubWorkflowQuery(t *testing.T, res workflow.Result, err error) *int {
	t.Helper()
	calls := 0
	prev := workflowQuery
	workflowQuery = func(context.Context, *sql.DB, string, string, string, security.WorkspaceRole) (workflow.Result, int64, error) {
		calls++
		return res, 1, err
	}
	t.Cleanup(func() { workflowQuery = prev })
	return &calls
}

func stubSlack(t *testing.T, fail error) *[]string {
	t.Helper()
	var sent []string
	prev := postSlack
	postSlack = func(_ context.Context, url, text string) error {
		sent = append(sent, url+"|"+text)
		return fail
	}
	t.Cleanup(func() { postSlack = prev })
	return &sent
}

// A run-as user who is no longer an owner or admin stops the run before the
// query and pauses the workflow; so does a run-as user who left.
func TestEvaluateWorkflowRun_DemotedRunAsPauses(t *testing.T) {
	for _, role := range []string{"member", ""} {
		mock, cleanup := wsScopeMockDB(t)
		calls := stubWorkflowQuery(t, workflow.Result{}, nil)
		expectRole(mock, role)
		database := wfDB()
		o, plan := evaluateWorkflowRun(context.Background(), database, wfRun(workflow.Condition{Type: "always"}, 0))
		if o.status != "failed" || o.pause == "" || plan != nil || *calls != 0 {
			t.Fatalf("role %q: outcome = %+v, plan = %+v, query calls = %d", role, o, plan, *calls)
		}
		cleanup()
	}
}

// A condition that does not hold plans no send; one that holds plans one to
// each channel, and the message it sends escapes Slack markup in values.
func TestEvaluateWorkflowRun_ConditionDecidesTheSend(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
	res := workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": int64(51)}}, RowCount: 1}

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	stubWorkflowQuery(t, res, nil)
	sent := stubSlack(t, nil)
	database := wfDB()

	expectRole(mock, "admin")
	o, plan := evaluateWorkflowRun(context.Background(), database, wfRun(workflow.Condition{Type: "value", Op: ">", Threshold: 100}, 0))
	if o.status != "succeeded" || o.conditionMet == nil || *o.conditionMet || plan != nil {
		t.Fatalf("not met: outcome = %+v, plan = %+v", o, plan)
	}

	expectRole(mock, "admin")
	expectSendChannel(t, mock)
	o, plan = evaluateWorkflowRun(context.Background(), database, wfRun(workflow.Condition{Type: "value", Op: ">", Threshold: 50}, 0))
	if o.status != "succeeded" || !*o.conditionMet || plan == nil || len(plan.channels) != 1 {
		t.Fatalf("met: outcome = %+v, plan = %+v", o, plan)
	}
	if d := deliverToChannel(context.Background(), database, plan.channels[0], plan.msg); d.Status != deliveryStatusSent {
		t.Fatalf("delivery = %+v", d)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "51 failed &lt;!channel&gt;") ||
		!strings.HasPrefix((*sent)[0], "https://hooks.slack.com/services/T0/B0/x|*Failed payments*") {
		t.Fatalf("sent = %v", *sent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectSendChannel(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	enc := mustEncrypt(t, "https://hooks.slack.com/services/T0/B0/x")
	mock.ExpectQuery(`SELECT dc.id FROM workflow_channels wc`).WithArgs(wsScopeWS, wfID, "send_1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfChannel))
	mock.ExpectQuery(`FROM delivery_channels WHERE id = \$1 AND workspace_id = \$2`).WithArgs(wfChannel, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind", "slack_webhook_encrypted", "email_recipients"}).
			AddRow(wfChannel, "#ops", "slack", enc, ""))
}

// Inside the quiet period a held condition is recorded but plans no send.
// (A failed delivery failing the run is TestFinishWorkflowRun_CountsFailedChannels.)
func TestEvaluateWorkflowRun_QuietPeriod(t *testing.T) {
	res := workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": "7"}}, RowCount: 1}
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	stubWorkflowQuery(t, res, nil)
	database := wfDB()

	expectRole(mock, "owner")
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(wfID, wfRunID, 60, "send_1").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	o, plan := evaluateWorkflowRun(context.Background(), database, wfRun(workflow.Condition{Type: "rows_exist"}, 60))
	if o.status != "succeeded" || plan != nil || len(o.notes) == 0 || !strings.Contains(o.notes[len(o.notes)-1], "60 minutes") {
		t.Fatalf("quiet: outcome = %+v, plan = %+v", o, plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An email channel on an rsync.ai with neither Resend nor SMTP fails with a
// sentence that says what to set, and sends nothing.
func TestDeliverToChannel_EmailNotSetUp(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("RESEND_FROM_ADDR", "")
	t.Setenv("SMTP_HOST", "")
	notifier.InvalidateChannelCache()
	t.Cleanup(notifier.InvalidateChannelCache)
	out := deliverToChannel(context.Background(), nil,
		&sendChannel{ID: wfChannel, Name: "Finance team", Kind: deliveryKindEmail, Recipients: []string{"a@x.example"}},
		renderedMessage{Subject: "s", Text: "t", HTML: "h"})
	if out.Status != deliveryStatusFailed || !strings.Contains(out.Error, "RESEND_API_KEY") || out.Sent != 0 {
		t.Fatalf("outcome = %+v", out)
	}
}

// A completed pipeline fires its workflows once per execution: the insert is
// guarded by the partial unique index and scoped to the pipeline's workspace,
// each run copies its workflow's definition, and its Temporal id is named for
// the workflow and the execution so a repeated fire cannot start it twice. The
// workflows' abandoned runs are failed first.
func TestFireWorkflowsAfterPipeline(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	database := wfDB()
	mock.ExpectExec(`(?s)UPDATE workflow_runs r SET status = 'failed'.*w\.trigger_pipeline_id = \$1`).
		WithArgs(wfPipe, workflowAbandonedError, 900, workflowStepEndedError).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)'after_pipeline', \$2::text, f\.definition.*'workflow-run:' \|\| f\.id::text.*`+
		`ON CONFLICT \(workflow_id, trigger_ref\) WHERE trigger = 'after_pipeline' DO NOTHING.*UNION ALL`).
		WithArgs(wfPipe, "exec-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workflow_id", "temporal_workflow_id"}).
			AddRow(wfRunID, wfID, "workflow-run:"+wfID+":exec-1"))
	runs, err := fireWorkflowsAfterPipeline(context.Background(), database, wfPipe, "exec-1")
	if err != nil || len(runs) != 1 || runs[0] != (pendingWorkflowRun{RunID: wfRunID, WorkflowID: wfID,
		TemporalID: "workflow-run:" + wfID + ":exec-1"}) {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// wfErrorOf reads the error sentence of a refused REST call.
func wfErrorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	return body.Error
}

func activateWorkflow(role string) *httptest.ResponseRecorder {
	r := wsScopeRouterAsRole(http.MethodPost, "/workflows/:id/activate", ActivateWorkflow, role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflows/"+wfID+"/activate", nil))
	return w
}

// Turning a workflow on makes again every check a draft passed, not only the
// connection and the SQL text: the definition is normalized, the query runs
// once, and each slot must name a column it returns. A draft stored unchecked
// (its query outlived the check) or whose table changed since must not go on
// and fail at its first unattended run. Nothing is updated when it is refused.
func TestActivateWorkflow_ChecksAgainWhatADraftPassed(t *testing.T) {
	always := workflow.Condition{Type: "always"}
	for _, tc := range []struct {
		name string
		def  string
		conn any
		res  workflow.Result
		err  error
		want []string
		deny []string
	}{
		{name: "a slot the query no longer returns", def: wfDefinitionJSON(always, 0), conn: wfConn,
			res:  workflow.Result{Columns: []string{"total"}, Rows: []map[string]any{{"total": 3}}, RowCount: 1},
			want: []string{"{{failed}}", "total"}},
		{name: "SQL the database rejects", def: wfDefinitionJSON(always, 0), conn: wfConn,
			err:  errors.New(`the query failed (syntax): pq: relation "payments" does not exist`),
			want: []string{`relation "payments" does not exist`}},
		{name: "a definition the draft check would refuse",
			def: strings.Replace(wfDefinitionJSON(always, 0), "{{failed}} failed", "{{value}} failed", 1), conn: wfConn,
			want: []string{"{{value}} is only the number a value condition compares"}},
		{name: "a deleted connection", def: wfDefinitionJSON(always, 0), conn: nil,
			want: []string{"connection was deleted", "Edit the workflow"}, deny: []string{"Ask Ask"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			stubWorkflowQuery(t, tc.res, tc.err)
			expectWorkflowLoadOf(mock, "draft", tc.def, tc.conn, wfUpdatedAt)

			w := activateWorkflow("admin")
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("code = %d, body = %s; want 422", w.Code, w.Body.String())
			}
			msg := wfErrorOf(t, w)
			for _, s := range tc.want {
				if !strings.Contains(msg, s) {
					t.Errorf("error %q lacks %q", msg, s)
				}
			}
			for _, s := range tc.deny {
				if strings.Contains(msg, s) {
					t.Errorf("error %q carries %q", msg, s)
				}
			}
			// No UPDATE and no audit row: the workflow stays as it was.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A workflow whose query still returns what its message names turns on, as
// the caller, with its next run computed from the schedule and its Temporal
// schedule set before the change commits.
func TestActivateWorkflow_TurnsOnAfterTheCheck(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	schedules := stubWorkflowSchedules(t, nil)
	calls := stubWorkflowQuery(t, workflow.Result{Columns: []string{"failed"},
		Rows: []map[string]any{{"failed": int64(2)}}, RowCount: 1}, nil)
	expectWorkflowLoad(mock, "draft")
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE workflows SET status = 'active'`).
		WithArgs(wfID, wsScopeWS, sqlmock.AnyArg(), wsScopeUser).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowLoad(mock, "active")

	w := activateWorkflow("admin")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("query ran %d times, want 1", *calls)
	}
	schedules.want(t, upserted("0 9 * * 1"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

var (
	wfChannelsByIDSQL = regexp.QuoteMeta(`SELECT id, name, kind FROM delivery_channels
		WHERE workspace_id = $1 AND id = ANY(string_to_array($2, ',')::uuid[])`)
	wfUpdateSQL = regexp.QuoteMeta(`UPDATE workflows SET name = $3`)
)

// wfEdit is the stored workflow (wfDefinitionJSON, always) as an editor saves
// it, with its SQL, message and schedule changed.
func wfEdit() workflowUpdateRequest {
	var def workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)), &def)
	def.SetQuery(workflow.Query{ConnectionID: wfConn,
		SQL: "SELECT count(*) AS failed, sum(amount) AS lost FROM payments WHERE status = 'failed'"})
	def.CaseSend(0).Message.Body = "{{failed}} failed payments, {{lost}} lost"
	def.Trigger.Cron = "0 8 * * *"
	return workflowUpdateRequest{Name: "Failed payments", Definition: def, ChannelIDs: []string{wfChannel},
		UpdatedAt: wfUpdatedAt}
}

func updateWorkflow(t *testing.T, role string, req any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := wsScopeRouterAsRole(http.MethodPut, "/workflows/:id", UpdateWorkflow, role)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/workflows/"+wfID, bytes.NewReader(body)))
	return w
}

func expectEditLookups(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(wfConnSQL).WithArgs(wfConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
	mock.ExpectQuery(wfChannelsByIDSQL).WithArgs(wsScopeWS, wfChannel).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}).AddRow(wfChannel, "#ops", "slack"))
}

// wfFailedLost is what the edited query returns.
var wfFailedLost = workflow.Result{Columns: []string{"failed", "lost"},
	Rows: []map[string]any{{"failed": int64(2), "lost": 99.5}}, RowCount: 1}

// Editing a workflow is for owners and admins, as drafting one is; a member
// is refused before anything is read.
func TestUpdateWorkflow_MemberIsRefused(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	if w := updateWorkflow(t, "member", wfEdit()); w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s; want 403", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An edit of a copy someone has changed since is not saved over their
// change: one made before the editor's request is read, and one made between
// the read and the write (the UPDATE names the updated_at it read).
func TestUpdateWorkflow_StaleCopyIsNotSaved(t *testing.T) {
	t.Run("changed before the request", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		expectWorkflowLoadOf(mock, "active", wfDefinitionJSON(workflow.Condition{Type: "always"}, 0), wfConn,
			wfUpdatedAt.Add(time.Minute))
		w := updateWorkflow(t, "admin", wfEdit())
		if w.Code != http.StatusConflict || !strings.Contains(wfErrorOf(t, w), "Reload it") {
			t.Fatalf("code = %d, body = %s; want 409 asking for a reload", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("changed while it was checked", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		stubWorkflowQuery(t, wfFailedLost, nil)
		expectWorkflowLoad(mock, "active")
		expectEditLookups(mock)
		mock.ExpectBegin()
		mock.ExpectExec(wfUpdateSQL).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectRollback()
		w := updateWorkflow(t, "admin", wfEdit())
		if w.Code != http.StatusConflict {
			t.Fatalf("code = %d, body = %s; want 409", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no version named", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		edit := wfEdit()
		edit.UpdatedAt = time.Time{}
		if w := updateWorkflow(t, "admin", edit); w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, body = %s; want 400", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// An edit gets every check a draft gets, the query run included, and is
// refused in a person's words with a code naming the step at fault, so the
// editor can open it. Nothing is written when it is refused.
func TestUpdateWorkflow_RefusesWhatADraftWould(t *testing.T) {
	for _, tc := range []struct {
		name     string
		edit     func(*workflowUpdateRequest)
		lookups  func(sqlmock.Sqlmock)
		res      workflow.Result
		qerr     error
		code     int
		wantCode string
		want     string
	}{
		{name: "SQL that writes", edit: func(e *workflowUpdateRequest) {
			e.Definition.SetQuery(workflow.Query{ConnectionID: wfConn, SQL: "DELETE FROM payments"})
		},
			lookups: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(wfConnSQL).WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
			},
			code: http.StatusBadRequest, wantCode: "invalid_sql"},
		{name: "a slot the query does not return", edit: func(*workflowUpdateRequest) {}, lookups: expectEditLookups,
			res:  workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": int64(2)}}, RowCount: 1},
			code: http.StatusUnprocessableEntity, wantCode: "invalid_message", want: "nothing was saved"},
		{name: "SQL the database rejects", edit: func(*workflowUpdateRequest) {}, lookups: expectEditLookups,
			qerr: errors.New(`pq: column "amount" does not exist`),
			code: http.StatusUnprocessableEntity, wantCode: "query_failed", want: `column "amount" does not exist`},
		{name: "a channel the workspace lacks", edit: func(*workflowUpdateRequest) {},
			lookups: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(wfConnSQL).WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
				m.ExpectQuery(wfChannelsByIDSQL).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "kind"}))
			},
			code: http.StatusNotFound, wantCode: "channel_not_found", want: "Pick another"},
		{name: "no channel", edit: func(e *workflowUpdateRequest) { e.ChannelIDs = nil },
			lookups: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(wfConnSQL).WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", "postgresql"))
			},
			code: http.StatusBadRequest, wantCode: "no_channels"},
		{name: "an empty name", edit: func(e *workflowUpdateRequest) { e.Name = "  " }, lookups: func(sqlmock.Sqlmock) {},
			code: http.StatusBadRequest, wantCode: "invalid_name"},
		// Only Ask rsync.ai adds a pipeline, model, wait, chart or dashboard
		// step; an edit adds or removes a condition with its message
		// (workflow_create_test.go).
		{name: "a step added", edit: func(e *workflowUpdateRequest) {
			e.Definition.Steps = append(e.Definition.Steps, workflow.Step{ID: "pause", Type: workflow.StepWait,
				After: []string{"send_1"}, Minutes: 5})
		}, lookups: func(sqlmock.Sqlmock) {},
			code: http.StatusBadRequest, wantCode: "invalid_steps", want: "ask rsync.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			stubWorkflowQuery(t, tc.res, tc.qerr)
			expectWorkflowLoad(mock, "active")
			tc.lookups(mock)
			edit := wfEdit()
			tc.edit(&edit)

			w := updateWorkflow(t, "admin", edit)
			var body struct{ Error, Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tc.code || body.Code != tc.wantCode {
				t.Fatalf("got %d %q (%s); want %d %q", w.Code, body.Code, body.Error, tc.code, tc.wantCode)
			}
			if !strings.Contains(body.Error, tc.want) || strings.Contains(body.Error, "list_") {
				t.Errorf("error %q: want %q, in a person's words", body.Error, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// nullArg matches a NULL parameter; notNullArg any other.
type nullArg struct{ null bool }

func (a nullArg) Match(v driver.Value) bool { return (v == nil) == a.null }

// A saved edit replaces the definition and the channels in one transaction.
// A workflow that is on stays on: its next run follows the new schedule and
// it runs as the editor, who approved the query, and its Temporal schedule
// moves inside the transaction. A draft gets no next run and no schedule, and
// keeps its run-as user until it is turned on.
func TestUpdateWorkflow_Saves(t *testing.T) {
	for _, status := range []string{"active", "draft"} {
		t.Run(status, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			schedules := stubWorkflowSchedules(t, nil)
			calls := stubWorkflowQuery(t, wfFailedLost, nil)
			expectWorkflowLoad(mock, status)
			expectEditLookups(mock)
			mock.ExpectBegin()
			var stored string
			runAs := any(nullArg{null: true})
			if status == "active" {
				runAs = wsScopeUser
			}
			mock.ExpectExec(wfUpdateSQL).
				WithArgs(wfID, wsScopeWS, "Failed payments", captureJSONArg(&stored), "schedule", wfConn,
					nullArg{null: true}, nullArg{null: status != "active"}, runAs, wfUpdatedAt).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`DELETE FROM workflow_channels WHERE workflow_id = \$1`).WithArgs(wfID).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO workflow_channels`).WithArgs(wfID, "send_1", wfChannel, 0).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
			expectWorkflowLoad(mock, status)

			w := updateWorkflow(t, "admin", wfEdit())
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
			}
			var body struct {
				Workflow     Workflow `json:"workflow"`
				QueryChecked bool     `json:"query_checked"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Workflow.ID != wfID || body.Workflow.Status != status || !body.QueryChecked || *calls != 1 {
				t.Fatalf("body = %s, query ran %d times", w.Body.String(), *calls)
			}
			var def workflow.Definition
			if err := json.Unmarshal([]byte(stored), &def); err != nil {
				t.Fatal(err)
			}
			if def.Trigger.Cron != "0 8 * * *" || def.Branches()[0].Message.Body != wfEdit().Definition.Branches()[0].Message.Body || def.Version != 3 {
				t.Fatalf("stored definition = %+v", def)
			}
			if status == "active" {
				schedules.want(t, upserted("0 8 * * *"))
			} else {
				schedules.want(t)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The audit row names the parts an edit changed and nothing they hold: never
// the SQL or the message.
func TestWorkflowEditChanges(t *testing.T) {
	var before workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)), &before)
	w := &Workflow{Name: "Weekly revenue", Definition: before, Channels: []WorkflowChannel{{ID: wfChannel}}}
	edit := wfEdit()
	got := workflowEditChanges(w, edit.Name, edit.Definition, []WorkflowChannel{{ID: wfChannel}})
	if strings.Join(got, ",") != "name,trigger,query,branches" {
		t.Fatalf("changed = %v", got)
	}
	if got := workflowEditChanges(w, w.Name, before, nil); strings.Join(got, ",") != "channels" {
		t.Fatalf("changed = %v, want channels", got)
	}
}

// Only an owner or admin adds a channel; a webhook that is not Slack's is
// refused before it is stored; email addresses are checked and de-duplicated.
func TestCreateDeliveryChannel(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789abcdef")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	post := func(role string, body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := wsScopeRouterAsRole(http.MethodPost, "/delivery-channels", CreateDeliveryChannel, role)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/delivery-channels", bytes.NewReader(b)))
		return w
	}
	if w := post("member", map[string]any{"name": "#ops", "kind": "slack", "webhook_url": "https://hooks.slack.com/services/a"}); w.Code != http.StatusForbidden {
		t.Fatalf("member: %d", w.Code)
	}
	if w := post("admin", map[string]any{"name": "#ops", "kind": "slack", "webhook_url": "https://evil.example/hook"}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad webhook: %d", w.Code)
	}
	if w := post("admin", map[string]any{"name": "Finance", "kind": "email", "recipients": []string{"not an address"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad address: %d", w.Code)
	}
	var arr string
	mock.ExpectQuery(`INSERT INTO delivery_channels`).
		WithArgs(wsScopeWS, "Finance team", "email", nil, captureJSONArg(&arr), wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wfChannel))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	w := post("owner", map[string]any{"name": "  Finance   team ", "kind": "email",
		"recipients": []string{"CFO@Example.com", "cfo@example.com", "ops@example.com"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if arr != `{"cfo@example.com","ops@example.com"}` {
		t.Fatalf("recipients stored as %s", arr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A channel a workflow sends to cannot be deleted; the refusal names it.
func TestDeleteDeliveryChannel_InUse(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM \(SELECT DISTINCT workflow_id, channel_id FROM workflow_channels\) wc\s+JOIN workflows w`).WithArgs(wfChannel, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"names"}).AddRow("Weekly revenue"))
	r := wsScopeRouterAsRole(http.MethodDelete, "/delivery-channels/:id", DeleteDeliveryChannel, "admin")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/delivery-channels/"+wfChannel, nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Weekly revenue") {
		t.Fatalf("delete in use: %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A Slack message carries the subject, the body and the workflow's link, and
// stays under Slack's limit however long the body is.
func TestRenderWorkflowMessage(t *testing.T) {
	var def workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)), &def)
	m := def.Branches()[0].Message
	m.Body = strings.Repeat("x", 5000) + " {{nope}}"
	m.Subject = ""
	res := workflow.Result{Columns: []string{"failed"}, Rows: []map[string]any{{"failed": 1}}, RowCount: 1}
	msg, notes := renderWorkflowMessage("Payments", m, res, workflow.Outcome{Met: true}, "https://app.example/workflows/1", time.Now())
	if len([]rune(msg.Slack)) > workflow.SlackMessageMax || !strings.HasSuffix(msg.Slack, "https://app.example/workflows/1") {
		t.Fatalf("slack message is %d chars or lost its link", len([]rune(msg.Slack)))
	}
	if msg.Subject != "[rsync.ai] Payments" || !strings.Contains(msg.HTML, "https://app.example/workflows/1") {
		t.Fatalf("subject %q / html link missing", msg.Subject)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "{{nope}}") {
		t.Fatalf("notes = %v", notes)
	}
}

// A Slack message whose body already has {{link}} shows the link once: the
// renderer's own trailing link would print it a second time.
func TestRenderWorkflowMessage_LinkOnceInSlack(t *testing.T) {
	var def workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)), &def)
	m := def.Branches()[0].Message
	const link = "https://app.example/workflows/1"
	for _, c := range []struct{ subject, body string }{
		{"", "Open the workflow: {{link}}"},
		{"See {{link}}", "Done."},
		{"", "Done."},
	} {
		m.Subject, m.Body = c.subject, c.body
		msg, _ := renderWorkflowMessage("W", m, workflow.Result{}, workflow.Outcome{Met: true}, link, time.Now())
		if n := strings.Count(msg.Slack, link); n != 1 {
			t.Errorf("subject %q, body %q: the link is in the Slack message %d times:\n%s", c.subject, c.body, n, msg.Slack)
		}
	}
}

// A message that uses {{row_count}} gets a note saying what it is: the rows the
// query returned, which a reader of "Total users: 1" could not tell.
func TestRenderWorkflowMessage_SaysWhatRowCountIs(t *testing.T) {
	var def workflow.Definition
	_ = json.Unmarshal([]byte(wfDefinitionJSON(workflow.Condition{Type: "always"}, 0)), &def)
	m := def.Branches()[0].Message
	m.Body = "{{row_count}} rows"
	res := workflow.Result{Columns: []string{"n"}, Rows: []map[string]any{{"n": 5005}}, RowCount: 1}
	_, notes := renderWorkflowMessage("W", m, res, workflow.Outcome{Met: true}, "https://x", time.Now())
	if len(notes) != 1 || !strings.Contains(notes[0], "{{row_count}} is 1 here") {
		t.Fatalf("notes = %v", notes)
	}
	m.Body = "{{n}} users"
	if _, notes := renderWorkflowMessage("W", m, res, workflow.Outcome{Met: true}, "https://x", time.Now()); len(notes) != 0 {
		t.Fatalf("a message without {{row_count}} got notes %v", notes)
	}
}
