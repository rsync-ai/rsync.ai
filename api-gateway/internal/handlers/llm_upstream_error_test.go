package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// A failed llm-service call reaches the browser as fixed text and a trace id,
// never as the llm-service body. The body below carries a host, an email and a
// password the way a FastAPI 422 or a provider error quotes its input; each
// case checks the answer names none of them and still says what failed.

const upstreamLeak = `{"detail":"could not connect to db.internal.acme.io:5432 as alice@example.com with password hunter2-canary"}`

var upstreamCanaries = []string{"db.internal.acme.io", "alice@example.com", "hunter2-canary"}

func assertNoUpstreamBody(t *testing.T, w *httptest.ResponseRecorder, wantError string) {
	t.Helper()
	for _, canary := range upstreamCanaries {
		if strings.Contains(w.Body.String(), canary) {
			t.Errorf("response echoes the llm-service body (%q): %s", canary, w.Body.String())
		}
	}
	got := decode(t, w)
	if got["error"] != wantError {
		t.Errorf("error = %v, want %q", got["error"], wantError)
	}
	if _, ok := got["trace_id"]; !ok {
		t.Errorf("no trace_id to find the logged body by: %v", got)
	}
}

func TestControl_TheUpstreamBodyCarriesEveryCanary(t *testing.T) {
	for _, canary := range upstreamCanaries {
		if !strings.Contains(upstreamLeak, canary) {
			t.Fatalf("control: %q missing from the upstream body", canary)
		}
	}
}

func TestGenerateSQL_DoesNotEchoTheUpstreamBody(t *testing.T) {
	t.Setenv("TEXT2SQL_ENDPOINT", upstream(t, http.StatusInternalServerError, upstreamLeak).URL)
	w := callGenerateSQL(t)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream 500 (%s)", w.Code, w.Body.String())
	}
	assertNoUpstreamBody(t, w, "SQL generation failed")
}

func callSuggestions(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/suggestions/generate",
		strings.NewReader(`{"schema":{"tables":[]}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	GenerateSuggestions(c)
	return w
}

func TestGenerateSuggestions_DoesNotEchoTheUpstreamBody(t *testing.T) {
	t.Setenv("LLM_SERVICE_URL", upstream(t, http.StatusUnprocessableEntity, upstreamLeak).URL)
	w := callSuggestions(t)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the upstream 422 (%s)", w.Code, w.Body.String())
	}
	assertNoUpstreamBody(t, w, "Suggestions failed")
}

func TestGenerateSuggestions_PassesASuccessThrough(t *testing.T) {
	// Control: only a failure is replaced; a 200 is the llm-service answer.
	t.Setenv("LLM_SERVICE_URL", upstream(t, http.StatusOK, `{"suggestions":[{"title":"mask email"}]}`).URL)
	w := callSuggestions(t)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "mask email") {
		t.Fatalf("status = %d body = %s, want the llm-service answer", w.Code, w.Body.String())
	}
}

func TestDiagnosePipeline_DoesNotEchoTheUpstreamBody(t *testing.T) {
	fixedTime := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	const pipelineID = "33333333-3333-3333-3333-333333333333"
	mock.ExpectQuery(`SELECT wm.role`).
		WithArgs(pipelineID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("owner"))
	mock.ExpectQuery(`FROM pipelines p`).
		WithArgs(pipelineID).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "status", "sync_mode", "source_connection_id", "destination_connection_id",
			"src_type", "dst_type", "created_at", "updated_at",
		}).AddRow("orders", "failed", "batch", "s1", "d1", "postgresql", "bigquery", fixedTime, fixedTime))
	// Every later evidence query is best-effort and ignores its error.
	t.Setenv("LLM_SERVICE_URL", upstream(t, http.StatusInternalServerError, upstreamLeak).URL)

	r := wsScopeRouter(http.MethodPost, "/api/v1/pipelines/:id/diagnose", DiagnosePipeline)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/"+pipelineID+"/diagnose", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	assertNoUpstreamBody(t, w, "diagnose_unavailable")
	if _, ok := decode(t, w)["evidence"]; !ok {
		t.Errorf("the evidence must still reach on-call when the model call fails: %s", w.Body.String())
	}
}
