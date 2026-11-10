package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// wsScopeUser / wsScopeWS / wsScopeMockDB / wsScopeRouterAsRole and the actor
// helpers (agentWith, assistantAs, chatAdmin) come from sibling test files.

const (
	planSrc   = "aaaaaaaa-0000-4000-8000-000000000001"
	planDst   = "aaaaaaaa-0000-4000-8000-000000000002"
	planRowID = "ffffffff-0000-4000-8000-000000000001"
	planTok   = "33333333-3333-3333-3333-333333333334"
	planPipe  = "55555555-5555-5555-5555-555555555555"
)

var (
	planCountSQL  = regexp.QuoteMeta(`SELECT COUNT(*) FROM pipeline_plans`)
	planConnSQL   = regexp.QuoteMeta(`SELECT name, connector_type FROM connections WHERE id = $1 AND workspace_id = $2`)
	planDraftSQL  = regexp.QuoteMeta(`SELECT connector_type, connector_version FROM connections WHERE id = $1 AND workspace_id = $2`)
	planModesSQL  = `SELECT sync_mode, cdc_mode\s+FROM connections\s+WHERE id = \$1`
	planInsertSQL = `INSERT INTO pipeline_plans \(`
	planLoadSQL   = regexp.QuoteMeta(`SELECT plan, status, actor_kind, applied_pipeline_id, created_at, expires_at`)
	planClaimSQL  = regexp.QuoteMeta(`UPDATE pipeline_plans SET status = 'applied', applied_at = NOW()`)
	planLoadCols  = []string{"plan", "status", "actor_kind", "applied_pipeline_id", "created_at", "expires_at"}
)

// noConnectorMetadata makes connectorSupportsCDC answer from its list of
// streaming sources, not from the connectors this checkout holds.
func noConnectorMetadata(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", t.TempDir())
	t.Setenv("MCP_INTERNAL_CONNECTORS_PATH", t.TempDir())
}

// seedLifecycle answers the draft check from the cache, so no executions query runs.
func seedLifecycle(t *testing.T, stages map[string]string) {
	t.Helper()
	lifecycleCacheMu.Lock()
	prev := lifecycleCache
	lifecycleCache = map[string]lifecycleCacheEntry{}
	for k, v := range stages {
		lifecycleCache[k] = lifecycleCacheEntry{value: v, computed: time.Now()}
	}
	lifecycleCacheMu.Unlock()
	t.Cleanup(func() {
		lifecycleCacheMu.Lock()
		lifecycleCache = prev
		lifecycleCacheMu.Unlock()
	})
}

func planCtx(tokenID string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/agent/v1/mcp", nil)
	c.Set("user_id", wsScopeUser)
	c.Set(ctxWorkspaceID, wsScopeWS)
	if tokenID != "" {
		c.Set(ctxTokenID, tokenID)
	}
	return c
}

// expectPlanChecks expects a proposal's reads before the sync mode: the open
// plan count, both connections in the workspace, and the draft check on each.
func expectPlanChecks(t *testing.T, mock sqlmock.Sqlmock, srcType, dstType string) {
	t.Helper()
	seedLifecycle(t, map[string]string{srcType + "@v1.0.0": "ga", dstType + "@v1.0.0": "ga"})
	mock.ExpectQuery(planCountSQL).WithArgs(wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(planConnSQL).WithArgs(planSrc, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("orders db", srcType))
	mock.ExpectQuery(planConnSQL).WithArgs(planDst, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}).AddRow("warehouse", dstType))
	mock.ExpectQuery(planDraftSQL).WithArgs(planSrc, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type", "connector_version"}).AddRow(srcType, "v1.0.0"))
	mock.ExpectQuery(planDraftSQL).WithArgs(planDst, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type", "connector_version"}).AddRow(dstType, "v1.0.0"))
}

func wantRefusal(t *testing.T, err error, code string, status int) *PlanRefusal {
	t.Helper()
	var r *PlanRefusal
	if !errors.As(err, &r) {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
	if r.Code != code || r.Status != status {
		t.Fatalf("refusal = %s %d (%q), want %s %d", r.Code, r.Status, r.Message, code, status)
	}
	return r
}

// Only Ask rsync.ai and an agent token propose a plan, and only for a member user
// with the scope; each refusal says why in the caller's terms. None reaches
// the database.
func TestProposePipelinePlan_WhoMayPropose(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	in := PipelinePlanInput{SourceConnectionID: planSrc, DestinationConnectionID: planDst}

	rest := services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSOwner, Channel: services.ChannelREST}
	for _, actor := range []services.Actor{rest, chatAdmin()} {
		_, err := ProposePipelinePlan(planCtx(""), actor, in)
		wantRefusal(t, err, "forbidden", http.StatusForbidden)
	}
	for _, tc := range []struct {
		actor services.Actor
		says  string
	}{
		{agentWith(security.WSViewer, services.ScopeMetadataRead, services.ScopePipelinesPlan), "token's user is a viewer"},
		{agentWith(security.WSOwner, services.ScopeMetadataRead, services.ScopeModelsWrite), services.ScopePipelinesPlan},
		{assistantAs(security.WSViewer), "member role"},
	} {
		_, err := ProposePipelinePlan(planCtx(""), tc.actor, in)
		if r := wantRefusal(t, err, "forbidden", http.StatusForbidden); !strings.Contains(r.Message, tc.says) {
			t.Errorf("%s/%s: message %q does not say %q", tc.actor.Channel, tc.actor.Role, r.Message, tc.says)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A proposal names two different connections of the caller's workspace, from
// connectors that have run, and a person has a bounded number open.
func TestProposePipelinePlan_RefusesBadConnections(t *testing.T) {
	agent := agentWith(security.WSMember, services.ScopeMetadataRead, services.ScopePipelinesPlan)

	t.Run("not an id, or the same one twice", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		_, err := ProposePipelinePlan(planCtx(""), agent, PipelinePlanInput{SourceConnectionID: "orders db", DestinationConnectionID: planDst})
		wantRefusal(t, err, "invalid_connection", http.StatusBadRequest)
		_, err = ProposePipelinePlan(planCtx(""), agent, PipelinePlanInput{SourceConnectionID: planSrc, DestinationConnectionID: strings.ToUpper(planSrc)})
		wantRefusal(t, err, "same_connection", http.StatusBadRequest)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("too many open plans", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(planCountSQL).WithArgs(wsScopeWS, wsScopeUser).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(maxOpenPipelinePlans))
		_, err := ProposePipelinePlan(planCtx(""), agent, PipelinePlanInput{SourceConnectionID: planSrc, DestinationConnectionID: planDst})
		wantRefusal(t, err, "too_many_plans", http.StatusTooManyRequests)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("another workspace's connection", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(planCountSQL).WithArgs(wsScopeWS, wsScopeUser).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(planConnSQL).WithArgs(planSrc, wsScopeWS).
			WillReturnRows(sqlmock.NewRows([]string{"name", "connector_type"}))
		_, err := ProposePipelinePlan(planCtx(""), agent, PipelinePlanInput{SourceConnectionID: planSrc, DestinationConnectionID: planDst})
		wantRefusal(t, err, "connection_not_found", http.StatusNotFound)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a draft connector", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		expectPlanChecks(t, mock, "postgresql", "acme-crm")
		seedLifecycle(t, map[string]string{"postgresql@v1.0.0": "ga", "acme-crm@v1.0.0": "draft"})
		_, err := ProposePipelinePlan(planCtx(""), agent, PipelinePlanInput{SourceConnectionID: planSrc, DestinationConnectionID: planDst})
		wantRefusal(t, err, "draft_connector", http.StatusUnprocessableEntity)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// A CDC request from a source that can't stream becomes a batch plan with a
// note, keeps its schedule, and is stored with the token that proposed it. The
// mock lists every statement in order and refuses any other, so the plan row
// and its audit row are all a proposal writes: no pipeline.
func TestProposePipelinePlan_StoresACheckedPlanAndCreatesNothing(t *testing.T) {
	noConnectorMetadata(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectPlanChecks(t, mock, "salesforce", "bigquery")
	stored := &capturedArg{} // session_token_at_rest_test.go
	created := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(planInsertSQL).
		WithArgs(wsScopeWS, wsScopeUser, "agent", planTok, planSrc, planDst, stored, 86400).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "expires_at"}).
			AddRow(planRowID, created, created.Add(pipelinePlanTTL)))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	agent := agentWith(security.WSMember, services.ScopeMetadataRead, services.ScopePipelinesPlan)
	plan, err := ProposePipelinePlan(planCtx(planTok), agent, PipelinePlanInput{
		SourceConnectionID: planSrc, DestinationConnectionID: planDst, SyncMode: "cdc",
		Cron: "0 * * * *", Timezone: "Europe/Berlin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if plan.ID != planRowID || plan.Status != planStatusProposed || plan.ProposedBy != "agent" ||
		!strings.HasSuffix(plan.URL, "/pipeline-plans/"+planRowID) || !plan.ExpiresAt.Equal(created.Add(24*time.Hour)) {
		t.Errorf("plan = %+v", plan)
	}
	if plan.SyncMode != "batch" || plan.CDCMode != "" || plan.Schedule == nil || plan.Schedule.Type != "cron" ||
		plan.Schedule.Timezone != "Europe/Berlin" || plan.Schedule.Description == "" {
		t.Errorf("mode %q/%q, schedule %+v", plan.SyncMode, plan.CDCMode, plan.Schedule)
	}
	notes := strings.Join(plan.Notes, " | ")
	if !strings.Contains(notes, "can't stream changes (CDC)") || !strings.Contains(notes, "No tables were named") {
		t.Errorf("notes = %q", notes)
	}

	var row PipelinePlan
	if err := json.Unmarshal([]byte(stored.string(t)), &row); err != nil {
		t.Fatalf("stored plan: %v", err)
	}
	if row.SyncMode != "batch" || row.Source.Name != "orders db" || row.Destination.ConnectorType != "bigquery" ||
		row.Schedule == nil || row.Schedule.Cron != "0 * * * *" || row.Tables == nil {
		t.Errorf("stored plan = %+v", row)
	}
	// The column holds the plan, not its envelope: the row's own columns hold the
	// id, status, author and times, and a copy here was blank or stale.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored.string(t)), &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range pipelinePlanEnvelope {
		if v, ok := keys[k]; ok {
			t.Errorf("the plan column stores the envelope's %q (%s)", k, v)
		}
	}
}

// pipelinePlanEnvelope is every PipelinePlan field the row's columns hold, or
// that is derived from them.
var pipelinePlanEnvelope = []string{"id", "status", "proposed_by", "url", "created_at", "expires_at", "applied_pipeline_id"}

// Each PipelinePlan field is either the plan, stored in the column, or the
// envelope, read from the columns: a field added to one and not the other would
// be dropped on the way to the database or stored twice.
func TestPipelinePlan_EachFieldIsTheBodyOrTheEnvelope(t *testing.T) {
	jsonNames := func(v any) map[string]bool {
		out := map[string]bool{}
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			out[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
		}
		return out
	}
	body := jsonNames(pipelinePlanBody{})
	envelope := map[string]bool{}
	for _, k := range pipelinePlanEnvelope {
		envelope[k] = true
	}
	plan := jsonNames(PipelinePlan{})
	for name := range plan {
		if body[name] == envelope[name] {
			t.Errorf("PipelinePlan field %q: in the stored body %v, in the envelope %v; want exactly one", name, body[name], envelope[name])
		}
	}
	for name := range body {
		if !plan[name] {
			t.Errorf("pipelinePlanBody field %q is not a PipelinePlan field", name)
		}
	}
	// Round trip: the body carries every plan field, and only those.
	full := PipelinePlan{
		ID: planRowID, Status: planStatusApplied, ProposedBy: "agent", URL: "u", AppliedPipelineID: "p",
		Source: openPlan.Source, Destination: openPlan.Destination, SyncMode: "cdc", CDCMode: "logical",
		Tables: []string{"public.orders"}, TableChoices: []PlanTableChoice{{Name: "orders", Options: []string{"public.orders"}}},
		MissingTables: []string{"x"}, TablesChecked: true, Schedule: &PlanSchedule{Type: "cron", Cron: "0 * * * *"},
		DestinationNamespace: "ns", Notes: []string{"n"},
		CreatedAt: time.Now(), ExpiresAt: time.Now(),
	}
	var back PipelinePlan
	back.setBody(full.body())
	want := full
	want.ID, want.Status, want.ProposedBy, want.URL, want.AppliedPipelineID = "", "", "", "", ""
	want.CreatedAt, want.ExpiresAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("body round trip lost a field:\n got %+v\nwant %+v", back, want)
	}
}

// A row stored before the body split holds the envelope too; the columns still win.
func TestGetPipelinePlan_AnOldRowsStoredEnvelopeIsIgnored(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	old := openPlan
	old.ID, old.Status, old.ProposedBy, old.URL = "stale-id", planStatusApplied, "agent", "stale-url"
	old.AppliedPipelineID, old.CreatedAt, old.ExpiresAt = "stale-pipeline", time.Unix(0, 0), time.Unix(0, 0)
	expires := time.Now().Add(time.Hour)
	mock.ExpectQuery(planLoadSQL).WithArgs(planRowID, wsScopeWS, wsScopeUser).
		WillReturnRows(planRows(t, old, planStatusProposed, nil, expires))
	w := httptest.NewRecorder()
	wsScopeRouterAsRole(http.MethodGet, "/pipeline-plans/:id", GetPipelinePlan, "viewer").
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pipeline-plans/"+planRowID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var plan PipelinePlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.ID != planRowID || plan.Status != planStatusProposed || plan.ProposedBy != "assistant" ||
		!strings.HasSuffix(plan.URL, "/pipeline-plans/"+planRowID) || plan.Source.Name != "orders db" ||
		len(plan.Tables) != 1 || plan.AppliedPipelineID != "" || !plan.ExpiresAt.Equal(expires) {
		t.Errorf("plan = %+v", plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// With no mode asked, the source connection's default is used; a CDC plan
// drops the schedule with a note, and Ask rsync.ai's plan records no token even
// when the request carries one.
func TestProposePipelinePlan_CDCFromTheConnectionDefault(t *testing.T) {
	noConnectorMetadata(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectPlanChecks(t, mock, "postgresql", "bigquery")
	mock.ExpectQuery(planModesSQL).WithArgs(planSrc).
		WillReturnRows(sqlmock.NewRows([]string{"sync_mode", "cdc_mode"}).AddRow("CDC", nil))
	now := time.Now()
	mock.ExpectQuery(planInsertSQL).
		WithArgs(wsScopeWS, wsScopeUser, "assistant", nil, planSrc, planDst, sqlmock.AnyArg(), 86400).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "expires_at"}).AddRow(planRowID, now, now.Add(pipelinePlanTTL)))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	plan, err := ProposePipelinePlan(planCtx(planTok), assistantAs(security.WSMember), PipelinePlanInput{
		SourceConnectionID: planSrc, DestinationConnectionID: planDst, ScheduleType: "interval", EverySeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if plan.SyncMode != "cdc" || plan.CDCMode != "initial" || plan.Schedule != nil || plan.ProposedBy != "assistant" {
		t.Errorf("plan = %+v", plan)
	}
	notes := strings.Join(plan.Notes, " | ")
	if !strings.Contains(notes, "schedule was left out") || !strings.Contains(notes, "publication and a replication slot") {
		t.Errorf("notes = %q", notes)
	}
}

// A schedule, time zone or mode the pipeline could not run with is refused
// before anything is stored.
func TestProposePipelinePlan_RefusesWhatCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   PipelinePlanInput
		code string
	}{
		{"interval under a minute", PipelinePlanInput{ScheduleType: "interval", EverySeconds: 30}, "invalid_schedule"},
		{"not a cron expression", PipelinePlanInput{Cron: "every hour"}, "invalid_schedule"},
		// A model asked for a one-off copy has passed schedule_type cron alone;
		// the plan must not store a schedule it cannot run.
		{"cron type with no cron", PipelinePlanInput{ScheduleType: "cron"}, "invalid_schedule"},
		{"not a time zone", PipelinePlanInput{Cron: "0 * * * *", Timezone: "Mars/Olympus"}, "invalid_schedule"},
		{"the server's own zone", PipelinePlanInput{Cron: "0 * * * *", Timezone: "Local"}, "invalid_schedule"},
		{"not a mode", PipelinePlanInput{SyncMode: "hourly"}, "invalid_sync_mode"},
		{"too many tables", PipelinePlanInput{Tables: manyTables(maxPlanTables + 1)}, "too_many_tables"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noConnectorMetadata(t)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectPlanChecks(t, mock, "mysql", "bigquery")
			in := tc.in
			in.SourceConnectionID, in.DestinationConnectionID = planSrc, planDst
			if in.SyncMode == "" {
				in.SyncMode = "batch"
			}
			_, err := ProposePipelinePlan(planCtx(""), assistantAs(security.WSMember), in)
			wantRefusal(t, err, tc.code, http.StatusBadRequest)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func manyTables(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strings.Repeat("x", i+1)
	}
	return out
}

// A name the source has in two schemas becomes a choice on the plan and is
// never picked for the person; creating it waits until they pick, and a pick
// may only name a table the plan holds or offers.
func TestPlanTables_ANameInTwoSchemasIsNeverPicked(t *testing.T) {
	known := []schemaTableRef{
		{Schema: "public", Name: "orders"}, {Schema: "public", Name: "customers"}, {Schema: "sales", Name: "customers"},
	}
	plan := &PipelinePlan{Tables: []string{}}
	plan.setCheckedTables(qualifyNamedTables(known, []string{"orders", "customers", "refunds"}))
	if !reflect.DeepEqual(plan.Tables, []string{"public.orders"}) ||
		!reflect.DeepEqual(plan.TableChoices, []PlanTableChoice{{Name: "customers", Options: []string{"public.customers", "sales.customers"}}}) ||
		!reflect.DeepEqual(plan.MissingTables, []string{"refunds"}) || len(plan.Notes) != 1 {
		t.Fatalf("plan tables = %v, choices = %v, missing = %v, notes = %v", plan.Tables, plan.TableChoices, plan.MissingTables, plan.Notes)
	}

	if _, r := planTablesToApply(plan, nil); r == nil || r.Code != "choose_tables" || r.Status != http.StatusConflict {
		t.Fatalf("no pick while a choice is open: %+v", r)
	}
	got, r := planTablesToApply(plan, []string{" sales.customers", "public.orders", "public.orders"})
	if r != nil || !reflect.DeepEqual(got, []string{"sales.customers", "public.orders"}) {
		t.Fatalf("a pick from the plan: %v, %+v", got, r)
	}
	if _, r := planTablesToApply(plan, []string{"public.orders", "hr.salaries"}); r == nil || r.Code != "table_not_in_plan" {
		t.Fatalf("a table the plan does not hold: %+v", r)
	}

	settled := &PipelinePlan{Tables: []string{"public.orders"}}
	if got, r := planTablesToApply(settled, nil); r != nil || !reflect.DeepEqual(got, []string{"public.orders"}) {
		t.Fatalf("no pick, no choice: %v, %+v", got, r)
	}
}

// planRows is the stored plan row loadPipelinePlan reads.
func planRows(t *testing.T, plan PipelinePlan, status string, applied any, expires time.Time) *sqlmock.Rows {
	t.Helper()
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return sqlmock.NewRows(planLoadCols).AddRow(raw, status, "assistant", applied, expires.Add(-pipelinePlanTTL), expires)
}

var openPlan = PipelinePlan{
	Source:      PlanConnection{ID: planSrc, Name: "orders db", ConnectorType: "postgresql"},
	Destination: PlanConnection{ID: planDst, Name: "warehouse", ConnectorType: "bigquery"},
	SyncMode:    "batch",
	Tables:      []string{"public.orders"},
}

func applyPlan(t *testing.T, role, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/pipeline-plans/"+id+"/apply", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/pipeline-plans/"+id+"/apply", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	wsScopeRouterAsRole(http.MethodPost, "/pipeline-plans/:id/apply", ApplyPipelinePlan, role).ServeHTTP(w, req)
	return w
}

func responseCode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return body
}

// A plan is applied by the signed-in person it was proposed for, once, while
// it is open; every other apply is refused before anything is claimed or created.
func TestApplyPipelinePlan_Refusals(t *testing.T) {
	future := time.Now().Add(time.Hour)
	loadArgs := []driver.Value{planRowID, wsScopeWS, wsScopeUser}

	t.Run("a viewer", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		if w := applyPlan(t, "viewer", planRowID, ""); w.Code != http.StatusForbidden {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("not a plan id", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		if w := applyPlan(t, "member", "orders", ""); w.Code != http.StatusNotFound {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("another person's plan", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(planLoadSQL).WithArgs(loadArgs...).WillReturnRows(sqlmock.NewRows(planLoadCols))
		if w := applyPlan(t, "member", planRowID, ""); w.Code != http.StatusNotFound {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	for _, tc := range []struct {
		name    string
		plan    PipelinePlan
		status  string
		applied any
		expires time.Time
		body    string
		code    int
		says    string
	}{
		{"expired", openPlan, planStatusProposed, nil, time.Now().Add(-time.Minute), "", http.StatusGone, "expired"},
		{"already applied", openPlan, planStatusApplied, planPipe, future, "", http.StatusConflict, "already_applied"},
		{"a table choice still open", PipelinePlan{Source: openPlan.Source, Destination: openPlan.Destination,
			SyncMode: "batch", Tables: []string{"public.orders"},
			TableChoices: []PlanTableChoice{{Name: "customers", Options: []string{"public.customers", "sales.customers"}}}},
			planStatusProposed, nil, future, "", http.StatusConflict, "choose_tables"},
		{"a table the plan does not hold", openPlan, planStatusProposed, nil, future,
			`{"tables":["hr.salaries"]}`, http.StatusBadRequest, "table_not_in_plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(planLoadSQL).WithArgs(loadArgs...).
				WillReturnRows(planRows(t, tc.plan, tc.status, tc.applied, tc.expires))
			w := applyPlan(t, "member", planRowID, tc.body)
			if w.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			body := responseCode(t, w)
			if body["code"] != tc.says {
				t.Fatalf("code = %v, want %s", body["code"], tc.says)
			}
			if tc.says == "already_applied" && body["applied_pipeline_id"] != planPipe {
				t.Fatalf("applied_pipeline_id = %v", body["applied_pipeline_id"])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}

	// Two clicks: the second loses the claim and is told what happened.
	for _, tc := range []struct {
		name   string
		reload *sqlmock.Rows
		code   int
		says   string
	}{
		{"claim lost to an apply", planRows(t, openPlan, planStatusApplied, planPipe, future), http.StatusConflict, "already_applied"},
		{"claim lost, cause unseen", planRows(t, openPlan, planStatusProposed, nil, future), http.StatusConflict, "plan_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(planLoadSQL).WithArgs(loadArgs...).
				WillReturnRows(planRows(t, openPlan, planStatusProposed, nil, future))
			mock.ExpectQuery(planClaimSQL).WithArgs(loadArgs...).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectQuery(planLoadSQL).WithArgs(loadArgs...).WillReturnRows(tc.reload)
			w := applyPlan(t, "member", planRowID, "")
			if w.Code != tc.code || responseCode(t, w)["code"] != tc.says {
				t.Fatalf("status %d, want %d %s: %s", w.Code, tc.code, tc.says, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A plan reads back only for the person it was proposed for, and an unapplied
// one past its time reads as expired.
func TestGetPipelinePlan(t *testing.T) {
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		wsScopeRouterAsRole(http.MethodGet, "/pipeline-plans/:id", GetPipelinePlan, "viewer").
			ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pipeline-plans/"+planRowID, nil))
		return w
	}

	t.Run("their own, expired", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(planLoadSQL).WithArgs(planRowID, wsScopeWS, wsScopeUser).
			WillReturnRows(planRows(t, openPlan, planStatusProposed, nil, time.Now().Add(-time.Minute)))
		w := get()
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var plan PipelinePlan
		if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.ID != planRowID || plan.Status != planStatusExpired || plan.ProposedBy != "assistant" ||
			plan.Source.Name != "orders db" || !strings.HasSuffix(plan.URL, "/pipeline-plans/"+planRowID) {
			t.Errorf("plan = %+v", plan)
		}
	})

	t.Run("someone else's", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(planLoadSQL).WithArgs(planRowID, wsScopeWS, wsScopeUser).WillReturnRows(sqlmock.NewRows(planLoadCols))
		if w := get(); w.Code != http.StatusNotFound {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// Any member may give their token pipelines:plan, since a plan creates nothing;
// the admin-only scopes stay admin-only.
func TestAgentTokenScopes_AnyMemberMayGrantPlan(t *testing.T) {
	got, status, msg := agentTokenScopes([]string{services.ScopePipelinesPlan}, false)
	if status != 0 || !reflect.DeepEqual(got, []string{services.ScopeMetadataRead, services.ScopePipelinesPlan}) {
		t.Fatalf("member granting pipelines:plan: %v, %d %q", got, status, msg)
	}
	if _, status, _ := agentTokenScopes([]string{services.ScopeModelsWrite}, false); status != http.StatusForbidden {
		t.Fatalf("control: a member granting models:write got %d, want 403", status)
	}
}
