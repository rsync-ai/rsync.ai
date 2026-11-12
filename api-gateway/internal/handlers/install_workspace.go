package handlers

import (
	"database/sql"

	"api-gateway/internal/config"
)

// Single-workspace mode (config.SingleWorkspace, RSYNC_SINGLE_WORKSPACE=true in
// the self-host compose and Helm only): an install has one team workspace that
// every new user joins, instead of a personal workspace each. Its id lives in
// instance_settings under installWorkspaceSetting. That key is not on the admin
// PATCH allowlist (admin_settings.go): only ensureInstallWorkspaceLocked writes it.
// See docs/architecture/multi-tenancy.md "Single-workspace mode".
const installWorkspaceSetting = "install_workspace_id"

// installWorkspaceLockKey names the advisory lock that serialises everything
// which decides the install workspace or who is the first user.
const installWorkspaceLockKey = "install_workspace"

// execer is the write half of *sql.DB and *sql.Tx, so the sign-up helpers run
// inside the caller's transaction.
type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// lockInstallWorkspace takes the install-workspace lock for the rest of tx. Two
// first sign-ups racing both read zero users before it; under it the second
// reads one, so there is never a second admin or a second install workspace.
func lockInstallWorkspace(tx *sql.Tx) error {
	_, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext($1))", installWorkspaceLockKey)
	return err
}

// ensureInstallWorkspaceLocked returns the install workspace, creating it if
// there is none. The caller holds lockInstallWorkspace on tx. In order:
//
//  1. the workspace the stored key names, if it still exists;
//  2. the earliest admin's oldest team workspace (an install upgraded from
//     multi mode already has one, and its data belongs in the install workspace);
//  3. a new team workspace owned by the earliest admin. Team, not personal: a
//     personal workspace refuses invites (workspaces.go CreateWorkspaceInvite).
//
// Only sql.ErrNoRows moves on to the next step. Any other read error returns,
// because "could not read it" is not "there is none", and guessing creates a
// second workspace on a database blip.
func ensureInstallWorkspaceLocked(tx *sql.Tx) (string, error) {
	var wsID string
	err := tx.QueryRow(`SELECT w.id FROM instance_settings s JOIN workspaces w ON w.id::text = s.value WHERE s.key = $1`,
		installWorkspaceSetting).Scan(&wsID)
	if err == nil {
		return wsID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	err = tx.QueryRow(`SELECT w.id FROM workspaces w JOIN users u ON u.id = w.owner_id
		WHERE NOT w.is_personal AND u.role = 'admin'
		ORDER BY u.created_at, w.created_at LIMIT 1`).Scan(&wsID)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == sql.ErrNoRows {
		var ownerID string
		if err := tx.QueryRow(`SELECT id FROM users ORDER BY (role = 'admin') DESC, created_at ASC LIMIT 1`).Scan(&ownerID); err != nil {
			return "", err
		}
		if err := tx.QueryRow(`
			INSERT INTO workspaces (name, slug, owner_id, plan, is_personal)
			VALUES ($1, $2, $3, 'free', false)
			RETURNING id
		`, "Workspace", "workspace-"+randomSlugSuffix(), ownerID).Scan(&wsID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`
			INSERT INTO workspace_members (workspace_id, user_id, role)
			VALUES ($1, $2, 'owner')
			ON CONFLICT (workspace_id, user_id) DO NOTHING
		`, wsID, ownerID); err != nil {
			return "", err
		}
	}

	// updated_by stays NULL: it references users(id), and naming the first user
	// here would block deleting them.
	if _, err := tx.Exec(`
		INSERT INTO instance_settings (key, value, updated_by, updated_at)
		VALUES ($1, $2, NULL, NOW())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = NULL, updated_at = NOW()
	`, installWorkspaceSetting, wsID); err != nil {
		return "", err
	}
	return wsID, nil
}

// isInstallWorkspace reports whether wsID is the install workspace. False in
// multi mode without a query.
func isInstallWorkspace(q rowQueryer, wsID string) (bool, error) {
	if !config.SingleWorkspace() {
		return false, nil
	}
	var is bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM instance_settings WHERE key = $1 AND value = $2)`,
		installWorkspaceSetting, wsID).Scan(&is)
	return is, err
}

// joinWorkspace adds userID to wsID with role. An existing membership is kept
// as it is.
func joinWorkspace(e execer, wsID, userID, role string) error {
	_, err := e.Exec(`
		INSERT INTO workspace_members (workspace_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, user_id) DO NOTHING
	`, wsID, userID, role)
	return err
}

// installWorkspaceRole is the workspace role a new user gets in the install
// workspace. The first user owns it. An instance invite's users.role maps to
// the nearest workspace role. Anyone else, including an open sign-up with no
// invite, gets viewer: the least access, which an admin can raise.
func installWorkspaceRole(firstUser bool, inviteRole string) string {
	if firstUser {
		return "owner"
	}
	switch inviteRole {
	case "admin":
		return "admin"
	case "power_user", "user":
		return "member"
	default:
		return "viewer"
	}
}

// defaultRegistrationMode is registration_mode when none is saved. Cloud stays
// "open". In single-workspace mode, once the first user exists, the install
// holds company data, so a stranger needs an invite until an admin opens it.
func defaultRegistrationMode(users int64) string {
	if config.SingleWorkspace() && users > 0 {
		return "invite_only"
	}
	return "open"
}

// registrationGate reads what decides whether a sign-up may proceed: the saved
// registration_mode, else defaultRegistrationMode. In single-workspace mode
// that default needs the user count, so it counts first and returns it too. In
// multi mode it issues only the setting read Register always issued, and users
// is -1 (not read).
func registrationGate(q rowQueryer) (mode string, users int64, err error) {
	users = -1
	if config.SingleWorkspace() {
		if err := q.QueryRow("SELECT COUNT(*) FROM users").Scan(&users); err != nil {
			return "", -1, err
		}
	}
	mode, err = getInstanceSetting(q, "registration_mode", defaultRegistrationMode(users))
	if err != nil {
		return "", -1, err
	}
	return mode, users, nil
}

// effectiveRegistrationMode is registration_mode as Register enforces it.
func effectiveRegistrationMode(q rowQueryer) (string, error) {
	mode, _, err := registrationGate(q)
	return mode, err
}

// holdsSignupInvite reports whether a sign-up carries an invite that passes
// invite_only. An instance invite always does. A workspace invite does only in
// single-workspace mode, where joining a workspace is joining the install; on
// cloud it adds a team to an account that must get in some other way.
func holdsSignupInvite(inviteToken, workspaceInviteToken string) bool {
	return inviteToken != "" || (config.SingleWorkspace() && workspaceInviteToken != "")
}
