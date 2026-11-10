package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// KI-CHAT-SKIPS-WORKSPACE-ROLE: the chat path created and ran pipelines, and
// started connector generation, with no workspace-role check and no audit row,
// while REST refused a viewer and audited every create. These tests pin the chat
// path to the same role table (services.Authorize) and the same audit action.

// chatAuthzContext is a gin context as AuthRequired + WorkspaceContextMiddleware
// leave it, at the given workspace role.
func chatAuthzContext(t *testing.T, role string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/chat/message", nil)
	c.Set("user_id", wsScopeUser)
	c.Set(ctxWorkspaceID, wsScopeWS)
	c.Set(ctxWorkspaceRole, role)
	return c
}

// A viewer is refused before any database work: no pipeline row, no run.
func TestChatCreatePipeline_ViewerIsRefused(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	_, _, _, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSViewer)),
		"sync pg to bq", wsScopeUser, "trace", "", "", "batch", "", "", nlTransformSpec{}, true, nil, "")
	if !errors.Is(err, services.ErrForbidden) {
		t.Fatalf("viewer create: err = %v, want services.ErrForbidden", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

// jsonContains matches a jsonb details argument that contains every fragment.
type jsonContains []string

func (j jsonContains) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, frag := range j {
		if !strings.Contains(s, frag) {
			return false
		}
	}
	return true
}

// A member's chat create writes the same audit action REST writes, marked as
// coming from chat.
func TestChatCreatePipeline_MemberIsAudited(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectExec(`INSERT INTO pipelines`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(wsScopeUser, "create_pipeline", "pipeline", sqlmock.AnyArg(),
			jsonContains{`"channel":"chat"`}, sqlmock.AnyArg(), wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipelines SET status = 'scheduled'`).WillReturnResult(sqlmock.NewResult(0, 1))

	id, _, _, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"sync pg to bq", wsScopeUser, "trace", "", "", "batch", "", "", nlTransformSpec{}, true, nil, "")
	if err != nil || id == "" {
		t.Fatalf("member create: id=%q err=%v, want a pipeline", id, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// Every file that inserts a pipeline row asks services.Authorize first. A third
// create path added later without the role check fails here, not in production.
func TestEveryPipelineInsertAsksAuthorize(t *testing.T) {
	insert := regexp.MustCompile(`(?i)INSERT\s+INTO\s+pipelines\s*\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !insert.Match(src) {
			continue
		}
		found++
		if !bytes.Contains(src, []byte("services.Authorize(")) {
			t.Errorf("%s inserts into pipelines but never calls services.Authorize", f)
		}
	}
	if found < 2 {
		t.Fatalf("found %d files inserting pipelines, want at least 2 (chat and REST): the scan is not looking where the inserts are", found)
	}
}

// Connector generation from chat needs a workspace admin, as the REST route does
// (WorkspaceGeneratorMiddleware). A member gets a refusal, and nothing is started.
func TestChatGenerateCommand_MemberIsRefused(t *testing.T) {
	pinNoCatalogDB(t)
	actor := services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSMember, Channel: services.ChannelChat}
	resp, handled := (&ChatHandler{}).maybeHandleGenerateCommand(context.Background(), actor, "generate acme-crm-authz-test", "trace")
	if !handled {
		t.Fatal("generate command not handled")
	}
	if resp.Type == "connector_generation_started" {
		t.Fatalf("member started a connector generation: %+v", resp)
	}
	if !strings.Contains(resp.Message, "admin") {
		t.Fatalf("refusal does not say who can generate: %q", resp.Message)
	}
}

// KI-PII-APPROVAL-SELF-APPROVE: the member who requested a PII approval cannot
// decide it. Someone else in the workspace must.
func TestDecideApproval_RequesterCannotDecide(t *testing.T) {
	const approvalID = "66666666-6666-6666-6666-666666666666"
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT requested_by::text FROM pii_approval_requests WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(approvalID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"requested_by"}).AddRow(wsScopeUser))

	r := piiRoleRouter(http.MethodPost, "/api/v1/pii/approvals/:id/decide", "member", newPIIHandlerWithMock().DecideApproval)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/pii/approvals/"+approvalID+"/decide",
		strings.NewReader(`{"decision":"approved"}`)))

	if w.Code != http.StatusForbidden {
		t.Fatalf("self-decide: got %d %s, want 403", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// Another member's request is still decided, and the write itself refuses the
// requester too, so the rule does not rest on the read alone.
func TestDecideApproval_OtherMemberDecides(t *testing.T) {
	const approvalID = "66666666-6666-6666-6666-666666666666"
	const requester = "33333333-3333-3333-3333-333333333333"
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT requested_by::text FROM pii_approval_requests WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(approvalID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"requested_by"}).AddRow(requester))
	mock.ExpectExec(`UPDATE pii_approval_requests SET status = \$1, decided_by = \$2, decided_at = \$3, conditions = \$4 WHERE id = \$5 AND workspace_id = \$6 AND requested_by <> \$2`).
		WithArgs("approved", wsScopeUser, sqlmock.AnyArg(), sqlmock.AnyArg(), approvalID, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO pii_approval_audit`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := piiRoleRouter(http.MethodPost, "/api/v1/pii/approvals/:id/decide", "member", newPIIHandlerWithMock().DecideApproval)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/pii/approvals/"+approvalID+"/decide",
		strings.NewReader(`{"decision":"approved"}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("other member decides: got %d %s, want 200", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// KI-NLCHAT-TABLE-LIST-IGNORED: when the named tables cannot be pre-selected
// (here the schema cache is cold), the chat reply says so and names them, instead
// of starting a pipeline with no tables and no word about why.
func TestChatCreatePipeline_NamedTablesNoteReachesReply(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	const srcConn = "44444444-4444-4444-4444-444444444444"
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	stubPreselectDiscover(t, nil, errors.New("orchestrator unreachable"))

	mock.ExpectQuery(`SELECT connector_type, connector_version FROM connections`).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT 1 FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(srcConn, wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	expectSourceInstalled(t, mock, srcConn, "postgresql")
	mock.ExpectExec(`INSERT INTO pipelines`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipelines SET status = 'scheduled'`).WillReturnResult(sqlmock.NewResult(0, 1))

	_, _, note, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"move orders and customers from pg to bq", wsScopeUser, "trace", srcConn, "", "batch", "", "",
		nlTransformSpec{}, true, []string{"orders", "customers"}, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, w := range []string{"`orders`", "`customers`", "table-selection step"} {
		if !strings.Contains(note, w) {
			t.Fatalf("reply note = %q, missing %q", note, w)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// stubPreselectDiscover replaces the live table-list read for one test.
func stubPreselectDiscover(t *testing.T, tables []schemaTableRef, err error) *[]string {
	t.Helper()
	var calls []string
	prev := preselectDiscover
	preselectDiscover = func(_ context.Context, workspaceID, connectionID, _ string) ([]schemaTableRef, error) {
		calls = append(calls, workspaceID+"/"+connectionID)
		return tables, err
	}
	t.Cleanup(func() { preselectDiscover = prev })
	return &calls
}

// "Copy the matches collection from my MongoDB Source" on prod (2026-09-30) said
// "I couldn't check `matches` against the source schema yet": the check read only
// the 5-minute schema cache, which is cold for any source nobody has browsed.
// A cold cache now reads the source's table list, scoped to the caller's
// workspace, and pre-selects on a clean match.
func TestChatCreatePipeline_NamedTablesColdCacheReadsTheSource(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	const srcConn = "44444444-4444-4444-4444-444444444444"
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	calls := stubPreselectDiscover(t, []schemaTableRef{
		{Schema: "datingapp", Name: "matches"},
		{Schema: "datingapp", Name: "subscriptions"},
	}, nil)

	mock.ExpectQuery(`SELECT connector_type, connector_version FROM connections`).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT 1 FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(srcConn, wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	expectSourceInstalled(t, mock, srcConn, "postgresql")
	mock.ExpectExec(`INSERT INTO pipelines`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	// Schedule-only (the `true` below): no transforms_review_pending mark, so
	// the unattended first run never parks on the chat's transform step.
	mock.ExpectExec(`'\{selected_tables\}'`).
		WithArgs(sqlmock.AnyArg(), `["datingapp.matches"]`, `{}`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipelines SET status = 'scheduled'`).WillReturnResult(sqlmock.NewResult(0, 1))

	_, _, note, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"copy the matches collection from my mongodb source to my gcs dest", wsScopeUser, "trace", srcConn, "", "batch", "", "",
		nlTransformSpec{}, true, []string{"matches"}, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if want := []string{wsScopeWS + "/" + srcConn}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("table-list reads = %v, want %v (one read, in the caller's workspace)", *calls, want)
	}
	for _, w := range []string{"Pre-selected only", "`datingapp.matches`"} {
		if !strings.Contains(note, w) {
			t.Fatalf("reply note = %q, missing %q", note, w)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}
