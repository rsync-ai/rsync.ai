package handlers

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Single-workspace mode (RSYNC_SINGLE_WORKSPACE=true, the self-host compose and
// Helm only): one install workspace per install, recorded in instance_settings
// under installWorkspaceSetting. The helpers below find or create it; every
// caller holds the install-workspace advisory lock first, so two first sign-ups
// can never make two of them.
//
// Cloud sets nothing, so effectiveRegistrationMode must issue exactly the query
// Register always issued and nothing more: the multi-mode tests below run in
// ordered mode, where an extra COUNT fails them.

const (
	iwWorkspace = "99999999-9999-9999-9999-999999999999"
	iwOwner     = "77777777-7777-7777-7777-777777777777"
)

const (
	iwLockQ    = `SELECT pg_advisory_xact_lock\(hashtext\(\$1\)\)`
	iwStoredQ  = `SELECT w\.id FROM instance_settings s JOIN workspaces w ON w\.id::text = s\.value WHERE s\.key = \$1`
	iwAdoptQ   = `(?s)SELECT w\.id FROM workspaces w JOIN users u ON u\.id = w\.owner_id\s+WHERE NOT w\.is_personal AND u\.role = 'admin'\s+ORDER BY u\.created_at, w\.created_at LIMIT 1`
	iwOwnerQ   = `SELECT id FROM users ORDER BY \(role = 'admin'\) DESC, created_at ASC LIMIT 1`
	iwCreateQ  = `INSERT INTO workspaces \(name, slug, owner_id, plan, is_personal\)\s+VALUES \(\$1, \$2, \$3, 'free', false\)\s+RETURNING id`
	iwOwnerRow = `INSERT INTO workspace_members \(workspace_id, user_id, role\)\s+VALUES \(\$1, \$2, 'owner'\)\s+ON CONFLICT \(workspace_id, user_id\) DO NOTHING`
	// updated_by is NULL on purpose: instance_settings.updated_by references
	// users(id), and a row naming the first user would block deleting them.
	iwUpsertQ = `INSERT INTO instance_settings \(key, value, updated_by, updated_at\)\s+VALUES \(\$1, \$2, NULL, NOW\(\)\)\s+` +
		`ON CONFLICT \(key\) DO UPDATE SET value = EXCLUDED\.value, updated_by = NULL, updated_at = NOW\(\)`
	iwCountQ   = `SELECT COUNT\(\*\) FROM users`
	iwSettingQ = `SELECT value FROM instance_settings WHERE key = \$1`
)

// iwTx opens a transaction on a fresh ordered sqlmock, the shape every caller of
// ensureInstallWorkspaceLocked hands it.
func iwTx(t *testing.T) (sqlmock.Sqlmock, *sql.Tx) {
	t.Helper()
	dbConn, mock, cleanup := wsSignupMockDB(t)
	t.Cleanup(cleanup)
	mock.ExpectBegin()
	tx, err := dbConn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return mock, tx
}

func TestLockInstallWorkspace_TakesTheTransactionLock(t *testing.T) {
	mock, tx := iwTx(t)
	mock.ExpectExec(iwLockQ).WithArgs("install_workspace").WillReturnResult(sqlmock.NewResult(0, 0))

	if err := lockInstallWorkspace(tx); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func TestEnsureInstallWorkspace_StoredWorkspaceIsReused(t *testing.T) {
	mock, tx := iwTx(t)
	mock.ExpectQuery(iwStoredQ).WithArgs(installWorkspaceSetting).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(iwWorkspace))
	// Nothing else: no adopt, no create, no rewrite of the stored key.

	got, err := ensureInstallWorkspaceLocked(tx)
	if err != nil || got != iwWorkspace {
		t.Fatalf("want (%q, nil), got (%q, %v)", iwWorkspace, got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// The v0.1.8 upgrade path: no key yet, but the first admin already runs a team
// workspace. It becomes the install workspace instead of a new, empty one.
func TestEnsureInstallWorkspace_AdoptsTheFirstAdminsTeamWorkspace(t *testing.T) {
	mock, tx := iwTx(t)
	mock.ExpectQuery(iwStoredQ).WithArgs(installWorkspaceSetting).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(iwAdoptQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(iwWorkspace))
	mock.ExpectExec(iwUpsertQ).WithArgs(installWorkspaceSetting, iwWorkspace).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := ensureInstallWorkspaceLocked(tx)
	if err != nil || got != iwWorkspace {
		t.Fatalf("want (%q, nil), got (%q, %v)", iwWorkspace, got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A fresh install: a team workspace (is_personal false, because a personal one
// refuses invites) owned by the earliest admin, then the key.
func TestEnsureInstallWorkspace_CreatesATeamWorkspaceForTheFirstAdmin(t *testing.T) {
	mock, tx := iwTx(t)
	mock.ExpectQuery(iwStoredQ).WithArgs(installWorkspaceSetting).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(iwAdoptQ).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(iwOwnerQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(iwOwner))
	mock.ExpectQuery(iwCreateQ).WithArgs("Workspace", sqlmock.AnyArg(), iwOwner).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(iwWorkspace))
	mock.ExpectExec(iwOwnerRow).WithArgs(iwWorkspace, iwOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(iwUpsertQ).WithArgs(installWorkspaceSetting, iwWorkspace).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := ensureInstallWorkspaceLocked(tx)
	if err != nil || got != iwWorkspace {
		t.Fatalf("want (%q, nil), got (%q, %v)", iwWorkspace, got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A failed read is not "no install workspace": treating it as one would create
// a second workspace on a database blip.
func TestEnsureInstallWorkspace_ReadErrorCreatesNothing(t *testing.T) {
	mock, tx := iwTx(t)
	mock.ExpectQuery(iwStoredQ).WithArgs(installWorkspaceSetting).WillReturnError(errTestDBDown)

	if got, err := ensureInstallWorkspaceLocked(tx); err == nil || got != "" {
		t.Fatalf("want ('', err), got (%q, %v)", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func TestInstallWorkspaceRole(t *testing.T) {
	for _, tc := range []struct {
		firstUser  bool
		inviteRole string
		want       string
	}{
		{true, "", "owner"},
		{true, "viewer", "owner"}, // the first user owns the install whatever they hold
		{false, "admin", "admin"},
		{false, "power_user", "member"},
		{false, "user", "member"},
		{false, "viewer", "viewer"},
		{false, "", "viewer"},     // open sign-up, no invite
		{false, "junk", "viewer"}, // an unknown role gets the least access
	} {
		if got := installWorkspaceRole(tc.firstUser, tc.inviteRole); got != tc.want {
			t.Fatalf("installWorkspaceRole(%v, %q) = %q, want %q", tc.firstUser, tc.inviteRole, got, tc.want)
		}
	}
}

func TestDefaultRegistrationMode(t *testing.T) {
	for _, tc := range []struct {
		single string
		users  int64
		want   string
	}{
		{"", 0, "open"},
		{"", 5, "open"}, // cloud: unchanged
		{"true", 0, "open"},
		{"true", 1, "invite_only"}, // after the first user, strangers need an invite
	} {
		t.Setenv("RSYNC_SINGLE_WORKSPACE", tc.single)
		if got := defaultRegistrationMode(tc.users); got != tc.want {
			t.Fatalf("single=%q users=%d: got %q, want %q", tc.single, tc.users, got, tc.want)
		}
	}
}

func TestEffectiveRegistrationMode_MultiModeReadsOnlyTheSetting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved interface{} // nil = key absent
		want  string
	}{
		{"absent", nil, "open"},
		{"saved invite_only", "invite_only", "invite_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RSYNC_SINGLE_WORKSPACE", "")
			dbConn, mock, cleanup := wsSignupMockDB(t)
			defer cleanup()
			q := mock.ExpectQuery(iwSettingQ).WithArgs("registration_mode")
			if tc.saved == nil {
				q.WillReturnError(sql.ErrNoRows)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(tc.saved))
			}

			got, err := effectiveRegistrationMode(dbConn)
			if err != nil || got != tc.want {
				t.Fatalf("want (%q, nil), got (%q, %v)", tc.want, got, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

func TestEffectiveRegistrationMode_SingleModeDefaultsToInviteOnlyOnceUsersExist(t *testing.T) {
	for _, tc := range []struct {
		name  string
		users int64
		saved interface{}
		want  string
	}{
		{"fresh install", 0, nil, "open"},
		{"users, nothing saved", 3, nil, "invite_only"},
		{"admin chose open", 3, "open", "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
			dbConn, mock, cleanup := wsSignupMockDB(t)
			defer cleanup()
			mock.ExpectQuery(iwCountQ).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tc.users))
			q := mock.ExpectQuery(iwSettingQ).WithArgs("registration_mode")
			if tc.saved == nil {
				q.WillReturnError(sql.ErrNoRows)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(tc.saved))
			}

			got, err := effectiveRegistrationMode(dbConn)
			if err != nil || got != tc.want {
				t.Fatalf("want (%q, nil), got (%q, %v)", tc.want, got, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// Not knowing how many users exist means not knowing the default, so the
// caller refuses (503) rather than guess "open".
func TestEffectiveRegistrationMode_SingleModeCountErrorIsAnError(t *testing.T) {
	t.Setenv("RSYNC_SINGLE_WORKSPACE", "true")
	dbConn, mock, cleanup := wsSignupMockDB(t)
	defer cleanup()
	mock.ExpectQuery(iwCountQ).WillReturnError(errTestDBDown)

	if got, err := effectiveRegistrationMode(dbConn); err == nil {
		t.Fatalf("want an error, got (%q, nil)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
