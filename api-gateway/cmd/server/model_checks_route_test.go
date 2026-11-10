package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

// The data-check routes (migration 138) sit under /explorer/saved/:id beside the SQL
// approval queue and the run history: checks/test and checks/pending/… are static
// segments where PUT and DELETE take a :checkId, and runs/:runId/checks extends the
// run history. Each must reach its own handler, and the SQL queue's
// pending/approve must not become a check approval. Every /explorer/saved route in
// main.go is served together, each answering with its handler's name; gin panics on a
// registration that conflicts with another.
func TestModelCheckRoutesCoexistWithTheSavedQueryRoutes(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	routes := mainRoutes(t, string(src), "/explorer/saved")
	if len(routes) < 20 {
		t.Fatalf("found %d /explorer/saved routes in main.go; the reader is not finding them", len(routes))
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	for _, rt := range routes {
		name := rt.handler
		r.Handle(rt.method, rt.path, func(c *gin.Context) { c.String(http.StatusOK, name) })
	}

	const (
		model   = "/explorer/saved/11111111-1111-1111-1111-111111111111"
		checkID = "22222222-2222-2222-2222-222222222222"
		pending = "33333333-3333-3333-3333-333333333333"
		runID   = "44444444-4444-4444-4444-444444444444"
	)
	cases := []struct{ method, path, want string }{
		{http.MethodGet, model + "/checks", "ListModelChecks"},
		{http.MethodPost, model + "/checks", "CreateModelCheck"},
		{http.MethodPost, model + "/checks/test", "TestModelChecks"},
		{http.MethodPut, model + "/checks/" + checkID, "UpdateModelCheck"},
		{http.MethodDelete, model + "/checks/" + checkID, "DeleteModelCheck"},
		{http.MethodPost, model + "/checks/pending/" + pending + "/approve", "ApproveModelCheckEdit"},
		{http.MethodPost, model + "/checks/pending/" + pending + "/reject", "RejectModelCheckEdit"},
		{http.MethodGet, model + "/runs/" + runID + "/checks", "ListModelRunChecks"},
		{http.MethodGet, model + "/runs", "ListSavedQueryRuns"},
		{http.MethodPost, model + "/pending/approve", "ApproveSavedQueryEdit"},
		{http.MethodPost, model + "/pending/reject", "RejectSavedQueryEdit"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("%s %s reached %q (%d), want %s", tc.method, tc.path, w.Body.String(), w.Code, tc.want)
		}
	}
}
