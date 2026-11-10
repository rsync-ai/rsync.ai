package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"api-gateway/internal/db"
)

// Register in single-workspace mode: no personal workspace. One transaction
// takes the install-workspace lock, re-reads the user count and the registration
// gate under it (two first sign-ups cannot both become admin, and a gate that
// closed while bcrypt ran still holds), inserts the user, finds or creates the
// install workspace and joins it with the role the table in
// installWorkspaceRole gives.
//
// These tests run in ORDERED mode: the lock must come straight after BEGIN, and
// a personal-workspace INSERT the single path should never make has no
// expectation, so it errors and turns the 201 into a 500.
//
// iwLockQ / iwStoredQ / iwCountQ / iwSettingQ / iwWorkspace come from
// install_workspace_test.go; wsInviteToken / wsInviteEmail / wsScopeWS from
// sibling test files.

const swInstanceInvite = "inst-invite-token"

func swRegisterReq(invite, workspaceInvite string) *http.Request {
	body, _ := json.Marshal(map[string]string{"email": wsInviteEmail, "password": "password123"})
	q := url.Values{}
	if invite != "" {
		q.Set("invite", invite)
	}
	if workspaceInvite != "" {
		q.Set("workspace_invite", workspaceInvite)
	}
	target := "/api/v1/auth/register"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// swSingleMode turns single-workspace mode on, pins logAudit's global DB to nil
// (a no-op) and returns an ordered mock.
func swSingleMode(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	prev := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = prev })
	dbConn, mock, cleanup := wsSignupMockDB(t)
	t.Cleanup(cleanup)
	return dbConn, mock
}

// swExpectGate queues the user count and the registration_mode read. saved nil
// means the key is absent, so the single-mode default applies.
func swExpectGate(mock sqlmock.Sqlmock, users int64, saved interface{}) {
	mock.ExpectQuery(iwCountQ).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(users))
	q := mock.ExpectQuery(iwSettingQ).WithArgs("registration_mode")
	if saved == nil {
		q.WillReturnError(sql.ErrNoRows)
	} else {
		q.WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(saved))
	}
}

func swExpectEmailFree(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM users WHERE email = \$1\)`).
		WithArgs(wsInviteEmail).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
}

// swExpectLockedGate queues BEGIN, the lock, then the gate again under it.
func swExpectLockedGate(mock sqlmock.Sqlmock, users int64, saved interface{}) {
	mock.ExpectBegin()
	mock.ExpectExec(iwLockQ).WithArgs("install_workspace").WillReturnResult(sqlmock.NewResult(0, 0))
	swExpectGate(mock, users, saved)
}

func swExpectFreePlan(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT duration_days FROM plans WHERE name = 'free'`).
		WillReturnRows(sqlmock.NewRows([]string{"duration_days"}).AddRow(30))
}

func swExpectUserInsert(mock sqlmock.Sqlmock, role string) {
	mock.ExpectExec(`INSERT INTO users`).
		WithArgs(sqlmock.AnyArg(), wsInviteEmail, sqlmock.AnyArg(), role,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func swExpectStoredInstall(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery(iwStoredQ).WithArgs(installWorkspaceSetting).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wsID))
}

// swExpectMember is the caller's own membership row, role bound as $3 (the
// install workspace's owner row from ensureInstallWorkspaceLocked binds a literal).
func swExpectMember(mock sqlmock.Sqlmock, wsID, role string) {
	mock.ExpectExec(`INSERT INTO workspace_members \(workspace_id, user_id, role\) VALUES \(\$1, \$2, \$3\) ON CONFLICT \(workspace_id, user_id\) DO NOTHING`).
		WithArgs(wsID, sqlmock.AnyArg(), role).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func swExpectInstanceInvite(mock sqlmock.Sqlmock, role string) {
	mock.ExpectQuery(`SELECT id, role, expires_at, used_at\s+FROM invitations\s+WHERE token = \$1`).
		WithArgs(swInstanceInvite).
		WillReturnRows(sqlmock.NewRows([]string{"id", "role", "expires_at", "used_at"}).
			AddRow("inv-1", role, time.Now().Add(time.Hour), nil))
}

// swExpectInviteMarkedUsed is the claim of the instance invite: AND used_at IS
// NULL makes a second sign-up racing on the same token touch zero rows.
func swExpectInviteMarkedUsed(mock sqlmock.Sqlmock, rows int64) {
	mock.ExpectExec(`UPDATE invitations SET used_at = NOW\(\), used_by = \$1 WHERE id = \$2 AND used_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), "inv-1").
		WillReturnResult(sqlmock.NewResult(0, rows))
}

func swExpectWorkspaceInviteValid(mock sqlmock.Sqlmock) {
	expectInviteValidation(mock).WillReturnRows(sqlmock.NewRows([]string{"email", "status", "expires_at"}).
		AddRow(wsInviteEmail, "pending", time.Now().Add(time.Hour)))
}

func swExpectWorkspaceInviteClaim(mock sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`UPDATE workspace_invites SET status = 'accepted'.*WHERE token = \$2 AND status = 'pending' AND expires_at > NOW\(\) AND lower\(email\) = lower\(\$3\)\s+RETURNING workspace_id, role`).
		WithArgs(sqlmock.AnyArg(), wsInviteToken, wsInviteEmail)
}

// swExpectSignedIn queues COMMIT, the session row and the last-login stamp.
func swExpectSignedIn(mock sqlmock.Sqlmock) {
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO sessions`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE users SET last_login_at = NOW\(\) WHERE id = \$1`).WillReturnResult(sqlmock.NewResult(0, 1))
}

type swResponse struct {
	Role              string `json:"role"`
	JoinedWorkspaceID string `json:"joined_workspace_id"`
	Error             string `json:"error"`
}

func swServe(t *testing.T, dbConn *sql.DB, mock sqlmock.Sqlmock, req *http.Request, wantCode int) swResponse {
	t.Helper()
	resp := httptest.NewRecorder()
	registerRouter(dbConn).ServeHTTP(resp, req)
	if resp.Code != wantCode {
		t.Fatalf("expected %d, got %d: %s", wantCode, resp.Code, resp.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
	var out swResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", resp.Body.String(), err)
	}
	return out
}

func TestRegisterSingle_FirstUserOwnsANewInstallWorkspace(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 0, nil)
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 0, nil)
	swExpectUserInsert(mock, "admin")
	// ensureInstallWorkspaceLocked: nothing stored, nothing to adopt, so create.
	mock.ExpectQuery(iwStoredQ).WithArgs(installWorkspaceSetting).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(iwAdoptQ).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(iwOwnerQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(iwOwner))
	mock.ExpectQuery(iwCreateQ).WithArgs("Workspace", sqlmock.AnyArg(), iwOwner).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(iwWorkspace))
	mock.ExpectExec(iwOwnerRow).WithArgs(iwWorkspace, iwOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(iwUpsertQ).WithArgs(installWorkspaceSetting, iwWorkspace).WillReturnResult(sqlmock.NewResult(0, 1))
	swExpectMember(mock, iwWorkspace, "owner")
	swExpectSignedIn(mock)

	out := swServe(t, dbConn, mock, swRegisterReq("", ""), http.StatusCreated)
	if out.Role != "admin" || out.JoinedWorkspaceID != iwWorkspace {
		t.Fatalf("first user: want admin in %s, got %+v", iwWorkspace, out)
	}
}

func TestRegisterSingle_InstanceInviteMapsToAWorkspaceRole(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 1, nil) // invite_only by default, but an invite is held
	swExpectInstanceInvite(mock, "power_user")
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 1, nil)
	swExpectFreePlan(mock)
	swExpectUserInsert(mock, "power_user")
	swExpectStoredInstall(mock, iwWorkspace)
	swExpectMember(mock, iwWorkspace, "member")
	swExpectInviteMarkedUsed(mock, 1)
	swExpectSignedIn(mock)

	out := swServe(t, dbConn, mock, swRegisterReq(swInstanceInvite, ""), http.StatusCreated)
	if out.Role != "power_user" || out.JoinedWorkspaceID != iwWorkspace {
		t.Fatalf("want power_user in %s, got %+v", iwWorkspace, out)
	}
}

func TestRegisterSingle_OpenSignUpJoinsAsViewer(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 2, "open") // the admin chose open
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 2, "open")
	swExpectFreePlan(mock)
	swExpectUserInsert(mock, "user")
	swExpectStoredInstall(mock, iwWorkspace)
	swExpectMember(mock, iwWorkspace, "viewer")
	swExpectSignedIn(mock)

	out := swServe(t, dbConn, mock, swRegisterReq("", ""), http.StatusCreated)
	if out.JoinedWorkspaceID != iwWorkspace {
		t.Fatalf("want joined %s, got %+v", iwWorkspace, out)
	}
}

func TestRegisterSingle_UninvitedSignUpRefusedOnceUsersExist(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 1, nil) // nothing saved: invite_only

	out := swServe(t, dbConn, mock, swRegisterReq("", ""), http.StatusForbidden)
	if out.Error != "invite_required" {
		t.Fatalf("want invite_required, got %+v", out)
	}
}

// Two first sign-ups race: both read 0 users before the lock, the second reads
// 1 under it and is refused, so no second admin and no second workspace.
func TestRegisterSingle_GateIsReadAgainUnderTheLock(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 0, nil)
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 1, nil)
	mock.ExpectRollback()

	out := swServe(t, dbConn, mock, swRegisterReq("", ""), http.StatusForbidden)
	if out.Error != "invite_required" {
		t.Fatalf("want invite_required, got %+v", out)
	}
}

func TestRegisterSingle_CountErrorRefusesRegistration(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	mock.ExpectQuery(iwCountQ).WillReturnError(errTestDBDown)

	out := swServe(t, dbConn, mock, swRegisterReq("", ""), http.StatusServiceUnavailable)
	if out.Error != "registration_unavailable" {
		t.Fatalf("want registration_unavailable, got %+v", out)
	}
}

func TestRegisterSingle_WorkspaceInvitePassesInviteOnly(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 1, nil)
	swExpectWorkspaceInviteValid(mock)
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 1, nil)
	swExpectFreePlan(mock)
	swExpectUserInsert(mock, "user")
	swExpectStoredInstall(mock, wsScopeWS)
	swExpectWorkspaceInviteClaim(mock).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "role"}).AddRow(wsScopeWS, "admin"))
	swExpectMember(mock, wsScopeWS, "admin") // the invite's role, not viewer
	swExpectSignedIn(mock)

	out := swServe(t, dbConn, mock, swRegisterReq("", wsInviteToken), http.StatusCreated)
	if out.JoinedWorkspaceID != wsScopeWS {
		t.Fatalf("want joined %s, got %+v", wsScopeWS, out)
	}
}

// The invite was the only way past invite_only; if another sign-up claimed it
// first, this one is refused and the user row rolls back.
func TestRegisterSingle_WorkspaceInviteLostInARaceIsRefused(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 1, nil)
	swExpectWorkspaceInviteValid(mock)
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 1, nil)
	swExpectFreePlan(mock)
	swExpectUserInsert(mock, "user")
	swExpectStoredInstall(mock, wsScopeWS)
	swExpectWorkspaceInviteClaim(mock).WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	out := swServe(t, dbConn, mock, swRegisterReq("", wsInviteToken), http.StatusForbidden)
	if out.Error != wsInviteProblemUnusable {
		t.Fatalf("want %s, got %+v", wsInviteProblemUnusable, out)
	}
}

// An upgraded install can hold other team workspaces. An invite to one of them
// joins that workspace only: the inviter did not offer the install workspace.
func TestRegisterSingle_InviteToAnotherWorkspaceJoinsOnlyThatOne(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 2, nil)
	swExpectWorkspaceInviteValid(mock)
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 2, nil)
	swExpectFreePlan(mock)
	swExpectUserInsert(mock, "user")
	swExpectStoredInstall(mock, iwWorkspace)
	swExpectWorkspaceInviteClaim(mock).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "role"}).AddRow(wsScopeWS, "member"))
	swExpectMember(mock, wsScopeWS, "member")
	// No install-workspace membership: an extra INSERT has no expectation here.
	swExpectSignedIn(mock)

	out := swServe(t, dbConn, mock, swRegisterReq("", wsInviteToken), http.StatusCreated)
	if out.JoinedWorkspaceID != wsScopeWS {
		t.Fatalf("want joined %s, got %+v", wsScopeWS, out)
	}
}

func TestRegisterSingle_InstanceInviteUsedInARaceIsRefused(t *testing.T) {
	dbConn, mock := swSingleMode(t)
	swExpectGate(mock, 1, nil)
	swExpectInstanceInvite(mock, "user")
	swExpectEmailFree(mock)
	swExpectLockedGate(mock, 1, nil)
	swExpectFreePlan(mock)
	swExpectUserInsert(mock, "user")
	swExpectStoredInstall(mock, iwWorkspace)
	swExpectMember(mock, iwWorkspace, "member")
	swExpectInviteMarkedUsed(mock, 0) // another sign-up used it first
	mock.ExpectRollback()

	out := swServe(t, dbConn, mock, swRegisterReq(swInstanceInvite, ""), http.StatusBadRequest)
	if out.Error != "invite_used" {
		t.Fatalf("want invite_used, got %+v", out)
	}
}

// Cloud: a workspace invite is not a way past invite_only; only an instance
// invite is. The single-mode exception must not leak into multi mode.
func TestRegisterMulti_WorkspaceInviteDoesNotPassInviteOnly(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "")
	dbConn, mock, cleanup := wsSignupMockDB(t)
	defer cleanup()
	mock.ExpectQuery(iwSettingQ).WithArgs("registration_mode").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("invite_only"))

	resp := httptest.NewRecorder()
	registerRouter(dbConn).ServeHTTP(resp, swRegisterReq("", wsInviteToken))
	if resp.Code != http.StatusForbidden || !strings.Contains(resp.Body.String(), "invite_required") {
		t.Fatalf("want 403 invite_required, got %d: %s", resp.Code, resp.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// Cloud: the response names the workspace a team invite joined, so the UI can
// open it. The SQL is the multi path's, unchanged.
func TestRegisterMulti_ResponseNamesTheJoinedWorkspace(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "")
	const personalWS = "88888888-8888-8888-8888-888888888888"
	dbConn, mock, cleanup := wsSignupMockDB(t)
	defer cleanup()
	mock.MatchExpectationsInOrder(false)
	prevGlobal := db.DB
	db.DB = nil
	defer func() { db.DB = prevGlobal }()

	expectRegistrationMode(mock)
	swExpectWorkspaceInviteValid(mock)
	swExpectEmailFree(mock)
	mock.ExpectQuery(iwCountQ).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	swExpectFreePlan(mock)
	mock.ExpectExec(`INSERT INTO users`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO workspaces .* RETURNING id`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(personalWS))
	mock.ExpectExec(`INSERT INTO workspace_members \(workspace_id, user_id, role\)\s+VALUES \(\$1, \$2, 'owner'\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	swExpectWorkspaceInviteClaim(mock).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "role"}).AddRow(wsScopeWS, "member"))
	swExpectMember(mock, wsScopeWS, "member")
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO sessions`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE users SET last_login_at`).WillReturnResult(sqlmock.NewResult(0, 1))

	out := swServe(t, dbConn, mock, swRegisterReq("", wsInviteToken), http.StatusCreated)
	if out.JoinedWorkspaceID != wsScopeWS {
		t.Fatalf("want joined %s, got %+v", wsScopeWS, out)
	}
}
