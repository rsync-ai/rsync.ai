package assistant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"api-gateway/internal/db"
	"api-gateway/internal/handlers"
	"api-gateway/internal/services"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// The admin routes are mounted on main.go's /admin group, which runs
// handlers.AdminRoleMiddleware: a signed-in user who is not a platform admin
// gets 403 and the key table is never read. This runs the real middleware
// against a mocked users/sessions lookup.
func TestAdminRoutesNeedAPlatformAdmin(t *testing.T) {
	for _, tc := range []struct {
		role   string
		status int
	}{
		{"member", http.StatusForbidden},
		{"admin", http.StatusOK}, // the control: the same request passes for an admin
	} {
		t.Run(tc.role, func(t *testing.T) {
			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			prev := db.DB
			db.DB = mockDB
			t.Cleanup(func() { db.DB = prev; _ = mockDB.Close() })
			mock.ExpectQuery(`FROM users u\s+JOIN sessions s`).
				WillReturnRows(sqlmock.NewRows([]string{"id", "email", "role", "status"}).
					AddRow("6f1d2b9e-1c1e-4f43-8d55-0d1f2b6e2a10", tc.role+"@example.com", tc.role, "active"))

			gin.SetMode(gin.TestMode)
			keys := &fakeKeys{rows: map[string]savedKey{}}
			r := gin.New()
			api := r.Group("/api/v1")
			admin := api.Group("/admin")
			admin.Use(handlers.AdminRoleMiddleware())
			if _, err := Mount(api, admin, Options{
				Tools: &fakeTools{}, LLM: &fakeStepper{}, Keys: keys,
				Actor:   func(*gin.Context) services.Actor { return services.Actor{} },
				QuotaOK: func(context.Context, string) bool { return true },
				Audit:   func(*gin.Context, string, string, string, map[string]interface{}) {},
			}); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/assistant/models", nil)
			req.Header.Set("Authorization", "Bearer unit-test-session-token")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("role %s: status %d, want %d: %s", tc.role, w.Code, tc.status, w.Body.String())
			}
			if wantList := tc.status == http.StatusOK; (keys.lists > 0) != wantList {
				t.Fatalf("role %s: key table read %d times", tc.role, keys.lists)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
