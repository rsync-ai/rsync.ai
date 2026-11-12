package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const releaseTestConnection = "3f0c2a8e-5b1d-4c7e-9a2f-6d8e0b1c2d3e"

type releaseResponse struct {
	Success bool     `json:"success"`
	Dropped int      `json:"dropped"`
	Errors  []string `json:"errors"`
	Error   string   `json:"error"`
}

// serveRelease runs the release handler on connectionID as the given principal.
func serveRelease(t *testing.T, db *sql.DB, release releaseConnectionObjects, connectionID string, principal func(*gin.Context)) (*httptest.ResponseRecorder, releaseResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { principal(c) })
	r.POST("/cdc/connections/:connection_id/release", releaseConnectionCDCResources(db, release, time.Second))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/cdc/connections/"+connectionID+"/release", nil))
	var body releaseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	return w, body
}

func asInternal(c *gin.Context) { c.Set("auth_internal", true) }

func TestReleaseConnectionCDCResources_DropsForTheNamedConnection(t *testing.T) {
	var got []string
	release := func(_ context.Context, _ *sql.DB, id string) (int, error) {
		got = append(got, id)
		return 2, nil
	}

	w, body := serveRelease(t, nil, release, releaseTestConnection, asInternal)

	if w.Code != http.StatusOK || !body.Success || body.Dropped != 2 {
		t.Fatalf("status %d body %+v; want 200, success, dropped 2", w.Code, body)
	}
	if len(got) != 1 || got[0] != releaseTestConnection {
		t.Fatalf("release called with %q; want exactly the named connection", got)
	}
}

// A ledger that cannot be read is reported, not a silent "dropped 0".
func TestReleaseConnectionCDCResources_ReportsALedgerReadFailure(t *testing.T) {
	release := func(context.Context, *sql.DB, string) (int, error) {
		return 0, errors.New("cdc_resources unreadable")
	}

	w, body := serveRelease(t, nil, release, releaseTestConnection, asInternal)

	if w.Code != http.StatusOK || body.Success || len(body.Errors) != 1 {
		t.Fatalf("status %d body %+v; want 200 with success:false and the error", w.Code, body)
	}
}

func TestReleaseConnectionCDCResources_RefusesAnIDThatIsNotAUUID(t *testing.T) {
	called := false
	release := func(context.Context, *sql.DB, string) (int, error) { called = true; return 0, nil }

	w, _ := serveRelease(t, nil, release, "not-a-uuid", asInternal)

	if w.Code != http.StatusBadRequest || called {
		t.Fatalf("status %d, release called %v; want 400 and no release", w.Code, called)
	}
}

// The release connects to the source with the connection's credentials, so a user
// outside the connection's workspace is refused before anything is dropped.
// DropsForTheNamedConnection is the internal-caller control.
func TestReleaseConnectionCDCResources_RefusesAUserOutsideTheWorkspace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	const outsider = "99999999-9999-9999-9999-999999999999"
	mock.ExpectQuery(`FROM connections c`).
		WithArgs(releaseTestConnection, outsider).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "user_id", "role"}).
			AddRow("11111111-1111-1111-1111-111111111111", outsider, nil))
	called := false
	release := func(context.Context, *sql.DB, string) (int, error) { called = true; return 0, nil }

	w, _ := serveRelease(t, db, release, releaseTestConnection, func(c *gin.Context) { c.Set("auth_user_id", outsider) })

	if w.Code != http.StatusForbidden || called {
		t.Fatalf("status %d, release called %v; want 403 and no release", w.Code, called)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("membership was never checked: %v", err)
	}
}
