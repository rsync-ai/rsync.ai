package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// PlanUsageAs is what Ask's get_usage reads: the plan, its limits, and the
// counts the create and query gates compare against them. Counts only: the
// member count is a COUNT, never a member's e-mail.

func planUsageContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c
}

var usageViewer = services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSViewer,
	Channel: services.ChannelAssistant, Scopes: services.AssistantScopes}

func expectUsageCounts(mock sqlmock.Sqlmock, pipelines, queries, members int) {
	mock.ExpectQuery(`COUNT\(\*\) FROM pipelines WHERE workspace_id = \$1`).
		WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(pipelines))
	mock.ExpectQuery(`monthly_query_count`).
		WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(queries))
	mock.ExpectQuery(`COUNT\(\*\) FROM workspace_members`).
		WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(members))
}

func TestPlanUsage_ReadsThePlanItsLimitsAndItsCounts(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM plans`).WillReturnRows(planCatalogueRows())
	mock.ExpectQuery(`FROM workspaces w`).WithArgs(wsScopeWS).WillReturnRows(workspacePlanRow("starter"))
	expectUsageCounts(mock, 3, 17, 4)

	u, err := PlanUsageAs(planUsageContext(), usageViewer)
	if err != nil {
		t.Fatalf("PlanUsageAs: %v", err)
	}
	if u.Plan != "starter" || u.PipelineLimit == nil || *u.PipelineLimit != 5 || u.PipelinesUsed != 3 ||
		u.QueryLimit == nil || *u.QueryLimit != 3000 || u.QueriesUsed != 17 || u.Members != 4 {
		t.Fatalf("usage = %+v, want starter 3/5 pipelines, 17/3000 queries, 4 members", u)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// With billing off there is no limit to report: both read as unlimited (nil),
// never as 0, which would read as "you have none left".
func TestPlanUsage_NoBillingIsUnlimited(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectUsageCounts(mock, 2, 0, 1)

	u, err := PlanUsageAs(planUsageContext(), usageViewer)
	if err != nil {
		t.Fatalf("PlanUsageAs: %v", err)
	}
	if u.PipelineLimit != nil || u.QueryLimit != nil {
		t.Fatalf("billing off: limits = %v, %v, want nil (unlimited)", u.PipelineLimit, u.QueryLimit)
	}
	if u.PipelinesUsed != 2 || u.Members != 1 {
		t.Fatalf("usage = %+v, want 2 pipelines and 1 member", u)
	}
}

// A meter that cannot be read fails the call: reporting 0 would be a wrong
// answer the user acts on.
func TestPlanUsage_AMeterErrorFailsRatherThanReadsZero(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`COUNT\(\*\) FROM pipelines WHERE workspace_id = \$1`).
		WithArgs(wsScopeWS).WillReturnError(os.ErrClosed)
	// The other meters read fine, so only the pipeline count's error can fail it.
	mock.ExpectQuery(`monthly_query_count`).
		WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`COUNT\(\*\) FROM workspace_members`).
		WithArgs(wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	if u, err := PlanUsageAs(planUsageContext(), usageViewer); err == nil {
		t.Fatalf("a failed pipeline count must fail the call, got %+v", u)
	}
}

func TestPlanUsage_ATokenWithoutMetadataReadIsRefused(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	agent := services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSOwner,
		Channel: services.ChannelAgent, Scopes: []string{services.ScopeWorkflowsWrite}}

	_, err := PlanUsageAs(planUsageContext(), agent)
	var refusal *PlanRefusal
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 refusal", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a refused call must read nothing: %v", err)
	}
}
