package agentgw

import (
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/handlers"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
)

// get_pipeline_health through the real handler and its workspace gate: the
// role check and the health query both bind the token's workspace, and the
// run's error text (a row value and a password complaint) never leaves.
func TestIntegration_PipelineHealthThroughRealHandler(t *testing.T) {
	const (
		user = "11111111-1111-4111-8111-111111111111"
		ws   = "22222222-2222-4222-8222-222222222222"
	)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = mockDB
	defer func() { db.DB = prev; _ = mockDB.Close() }()

	now := time.Now()
	mock.ExpectQuery(`FROM pipelines r\s+JOIN workspace_members wm`).
		WithArgs(pipeID, user, ws).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("viewer"))
	mock.ExpectQuery(`WHERE p.id = \$1 AND p.workspace_id = \$2`).
		WithArgs(pipeID, ws).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "is_cdc", "pp_status", "pp_reason", "pp_updated",
			"execution_id", "execution_status", "started_at", "completed_at", "error_message",
			"last_success_at", "schedule_type", "schedule_status", "cron", "every_seconds", "timezone",
			"dependency_unhealthy", "derived_status",
		}).AddRow(
			"orders", false, "failed", nil, now,
			runID, "failed", now.Add(-time.Hour), now,
			`pq: password authentication failed for user "etl" near `+canary,
			now.Add(-48*time.Hour), "cron", "active", "0 * * * *", nil, "UTC",
			false, "failed",
		))

	h := DefaultHandlers()
	actor := services.Actor{UserID: user, WorkspaceID: ws, Role: security.WSViewer,
		Channel: services.ChannelAgent, Scopes: []string{services.ScopeMetadataRead}}
	r, audit := testServer(t, h, actor)

	res := call(t, r, "get_pipeline_health", map[string]any{"pipeline_id": pipeID})
	d := data(t, res)
	blocking, _ := d["blocking"].(map[string]any)
	if d["name"] != "orders" || blocking["kind"] != "failed" || blocking["error_class"] != "auth" {
		t.Fatalf("health = %v", d)
	}
	if text := resultText(res); strings.Contains(text, canary) || strings.Contains(text, "password authentication") {
		t.Fatalf("error text leaked: %s", text)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
	if (*audit)[0] != (auditRow{"get_pipeline_health", "pipeline", pipeID, "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}
}

// A pipeline outside the token's workspace is "not found" to the agent.
func TestIntegration_OtherWorkspaceIsNotFound(t *testing.T) {
	const (
		user = "11111111-1111-4111-8111-111111111111"
		ws   = "22222222-2222-4222-8222-222222222222"
	)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = mockDB
	defer func() { db.DB = prev; _ = mockDB.Close() }()
	mock.ExpectQuery(`FROM pipelines r\s+JOIN workspace_members wm`).
		WithArgs(pipeID, user, ws).
		WillReturnRows(sqlmock.NewRows([]string{"role"}))

	actor := services.Actor{UserID: user, WorkspaceID: ws, Role: security.WSOwner,
		Channel: services.ChannelAgent, Scopes: []string{services.ScopeMetadataRead}}
	r, _ := testServer(t, Handlers{GetPipelineHealth: handlers.GetPipelineHealth}, actor)
	res := call(t, r, "get_pipeline_health", map[string]any{"pipeline_id": pipeID})
	if !isError(res) || !strings.HasPrefix(resultText(res), "Not found in this workspace.") {
		t.Fatalf("got %v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
