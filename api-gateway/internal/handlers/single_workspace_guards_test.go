package handlers

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Single-workspace mode keeps an install at one workspace. The UI hides the
// controls; these guards are the real gate:
//
//   - CreateWorkspace answers 403 single_workspace_mode before touching the DB.
//   - DeleteWorkspace refuses the install workspace (409 install_workspace); an
//     older team workspace an upgraded install still holds deletes as before.
//   - AdminDeleteUser refuses the install workspace's owner (409
//     install_workspace_owner): workspaces.owner_id is ON DELETE CASCADE, so
//     deleting them would delete the install workspace with everything in it.
//   - WorkspaceContextMiddleware falls back to the caller's install-workspace
//     membership when they have no personal workspace (single-mode sign-ups
//     never get one).
//   - AdminGetSettings reports the effective registration default, invite_only
//     once users exist.
//
// Each multi-mode control runs in ORDERED mode with nothing extra queued, so a
// single-mode query leaking into cloud fails it.

const (
	swIsInstallQ      = `SELECT EXISTS\(SELECT 1 FROM instance_settings WHERE key = \$1 AND value = \$2\)`
	swInstallOwnerQ   = `SELECT EXISTS\(SELECT 1 FROM instance_settings s JOIN workspaces w ON w\.id::text = s\.value WHERE s\.key = \$1 AND w\.owner_id = \$2\)`
	swContextFallback = `SELECT wm\.workspace_id, wm\.role FROM workspace_members wm ` +
		`LEFT JOIN instance_settings s ON s\.key = \$2 AND s\.value = wm\.workspace_id::text ` +
		`WHERE wm\.user_id = \$1 ORDER BY \(s\.key IS NOT NULL\) DESC, wm\.created_at ASC LIMIT 1`
	swAllSettingsQ = `SELECT key, value FROM instance_settings ORDER BY key`
)

func swExists(v bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"exists"}).AddRow(v)
}

func swMetOrFail(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// ---- CreateWorkspace ----

func TestCreateWorkspace_SingleModeRefusedBeforeAnyQuery(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(`{"name":"Second"}`))
	req.Header.Set("Content-Type", "application/json")
	wsScopeRouter(http.MethodPost, "/api/v1/workspaces", CreateWorkspace).ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden || !strings.Contains(resp.Body.String(), `"single_workspace_mode"`) {
		t.Fatalf("want 403 single_workspace_mode, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}

// ---- DeleteWorkspace ----

func TestDeleteWorkspace_SingleModeInstallWorkspaceUndeletable(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	wsParamGate(mock, "owner")
	wsIsPersonal(mock, false)
	mock.ExpectQuery(swIsInstallQ).WithArgs(installWorkspaceSetting, wsScopeWS).WillReturnRows(swExists(true))
	// No count, no DELETE.

	resp := httptest.NewRecorder()
	deleteWorkspaceRouter().ServeHTTP(resp, wsDeleteReq())

	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), `"install_workspace"`) {
		t.Fatalf("want 409 install_workspace, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}

func TestDeleteWorkspace_SingleModeOtherTeamWorkspaceStillDeletes(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	wsParamGate(mock, "owner")
	wsIsPersonal(mock, false)
	mock.ExpectQuery(swIsInstallQ).WithArgs(installWorkspaceSetting, wsScopeWS).WillReturnRows(swExists(false))
	wsResourceCount(mock, 0)
	mock.ExpectExec(`DELETE FROM workspaces WHERE id = \$1`).WithArgs(wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))

	resp := httptest.NewRecorder()
	deleteWorkspaceRouter().ServeHTTP(resp, wsDeleteReq())

	if resp.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}

func TestDeleteWorkspace_SingleModeInstallLookupErrorDeletesNothing(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	wsParamGate(mock, "owner")
	wsIsPersonal(mock, false)
	mock.ExpectQuery(swIsInstallQ).WithArgs(installWorkspaceSetting, wsScopeWS).WillReturnError(errTestDBDown)

	resp := httptest.NewRecorder()
	deleteWorkspaceRouter().ServeHTTP(resp, wsDeleteReq())

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}

// ---- AdminDeleteUser ----

// swExpectUserAndWorkspacesLocked is dgExpectUserLockedAndCounted without the
// count: the install-owner check runs between the locks and the count.
func swExpectUserAndWorkspacesLocked(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT email FROM users WHERE id = \$1`).WithArgs(dgTargetID).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("target@example.com"))
	mock.ExpectExec(`FROM workspaces WHERE owner_id = \$1`).WithArgs(dgTargetID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestAdminDeleteUser_SingleModeInstallOwnerRefused(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	swExpectUserAndWorkspacesLocked(mock)
	mock.ExpectQuery(swInstallOwnerQ).WithArgs(installWorkspaceSetting, dgTargetID).WillReturnRows(swExists(true))
	mock.ExpectRollback()

	w := dgServeAdminDelete(dgTargetID)

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"install_workspace_owner"`) {
		t.Fatalf("want 409 install_workspace_owner, got %d: %s", w.Code, w.Body.String())
	}
	swMetOrFail(t, mock)
}

func TestAdminDeleteUser_SingleModeOtherUserStillDeletes(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	swExpectUserAndWorkspacesLocked(mock)
	mock.ExpectQuery(swInstallOwnerQ).WithArgs(installWorkspaceSetting, dgTargetID).WillReturnRows(swExists(false))
	mock.ExpectQuery(`(?s)FROM pipelines.*FROM connections.*FROM saved_queries.*FROM pipeline_schedules.*FROM saved_query_schedules`).
		WithArgs(dgTargetID).
		WillReturnRows(sqlmock.NewRows([]string{"pipelines", "connections", "saved_queries", "schedules"}).AddRow(0, 0, 0, 0))
	mock.ExpectExec(`DELETE FROM sessions WHERE user_id = \$1`).WithArgs(dgTargetID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM users WHERE id = \$1`).WithArgs(dgTargetID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	w := dgServeAdminDelete(dgTargetID)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	swMetOrFail(t, mock)
}

// ---- WorkspaceContextMiddleware ----

func TestWorkspaceContext_SingleModeFallsBackToTheInstallMembership(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	_, mock, cleanup := setupMockDB(t)
	defer cleanup()
	mock.ExpectQuery(personalQ).WithArgs(wsUser).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(swContextFallback).WithArgs(wsUser, installWorkspaceSetting).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "role"}).AddRow(wsTeam, "viewer"))

	w := wsReq(wsMiddlewareEngine(), "/api/v1/probe", wsUser, "", "")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), wsTeam) || !strings.Contains(w.Body.String(), `"viewer"`) {
		t.Fatalf("want 200 in %s as viewer, got %d: %s", wsTeam, w.Code, w.Body.String())
	}
	swMetOrFail(t, mock)
}

func TestWorkspaceContext_SingleModeNoMembershipLeavesContextUnset(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	_, mock, cleanup := setupMockDB(t)
	defer cleanup()
	mock.ExpectQuery(personalQ).WithArgs(wsUser).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(swContextFallback).WithArgs(wsUser, installWorkspaceSetting).WillReturnError(sql.ErrNoRows)

	w := wsReq(wsMiddlewareEngine(), "/api/v1/probe", wsUser, "", "")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"workspace_id":""`) {
		t.Fatalf("want 200 with no workspace, got %d: %s", w.Code, w.Body.String())
	}
	swMetOrFail(t, mock)
}

func TestWorkspaceContext_SingleModeFallbackErrorIs503(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	_, mock, cleanup := setupMockDB(t)
	defer cleanup()
	mock.ExpectQuery(personalQ).WithArgs(wsUser).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(swContextFallback).WithArgs(wsUser, installWorkspaceSetting).WillReturnError(errTestDBDown)

	w := wsReq(wsMiddlewareEngine(), "/api/v1/probe", wsUser, "", "")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
	swMetOrFail(t, mock)
}

// Cloud: no personal workspace leaves the context unset, with no second query.
func TestWorkspaceContext_MultiModeNoPersonalAsksNothingMore(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "")
	_, mock, cleanup := setupMockDB(t)
	defer cleanup()
	mock.ExpectQuery(personalQ).WithArgs(wsUser).WillReturnError(sql.ErrNoRows)

	w := wsReq(wsMiddlewareEngine(), "/api/v1/probe", wsUser, "", "")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"workspace_id":""`) {
		t.Fatalf("want 200 with no workspace, got %d: %s", w.Code, w.Body.String())
	}
	swMetOrFail(t, mock)
}

// ---- AdminGetSettings ----

func TestAdminGetSettings_SingleModeAbsentKeyReportsTheEffectiveDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		users int64
		want  string
	}{
		{"fresh install", 0, `"registration_mode":"open"`},
		{"users exist", 2, `"registration_mode":"invite_only"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
			mock := withMockGlobalDB(t)
			mock.ExpectQuery(swAllSettingsQ).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("branding_name", "Acme"))
			mock.ExpectQuery(iwCountQ).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.users))

			resp := getAdminSettings()

			if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), tc.want) {
				t.Fatalf("want 200 with %s, got %d: %s", tc.want, resp.Code, resp.Body.String())
			}
			swMetOrFail(t, mock)
		})
	}
}

func TestAdminGetSettings_SingleModeSavedKeyWins(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock := withMockGlobalDB(t)
	mock.ExpectQuery(swAllSettingsQ).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("registration_mode", "open"))
	// No COUNT: the admin's choice needs no default.

	resp := getAdminSettings()

	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"registration_mode":"open"`) {
		t.Fatalf("want 200 with the saved open, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}

// Not knowing the user count means not knowing the default; reporting "open"
// would tell the admin strangers can sign up when they cannot.
func TestAdminGetSettings_SingleModeCountErrorIsNotServedAsOpen(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	mock := withMockGlobalDB(t)
	mock.ExpectQuery(swAllSettingsQ).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("branding_name", "Acme"))
	mock.ExpectQuery(iwCountQ).WillReturnError(errTestDBDown)

	resp := getAdminSettings()

	if resp.Code != http.StatusInternalServerError || strings.Contains(resp.Body.String(), "registration_mode") {
		t.Fatalf("want 500 with no settings, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}

func TestAdminGetSettings_MultiModeAbsentKeyAsksNothingMore(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "")
	mock := withMockGlobalDB(t)
	mock.ExpectQuery(swAllSettingsQ).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow("branding_name", "Acme"))

	resp := getAdminSettings()

	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"registration_mode":"open"`) {
		t.Fatalf("want 200 with open, got %d: %s", resp.Code, resp.Body.String())
	}
	swMetOrFail(t, mock)
}
