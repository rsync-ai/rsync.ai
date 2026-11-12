package connections

import (
	"context"
	"database/sql"
	"strings"

	log "github.com/sirupsen/logrus"
)

// A connection runs on the connector version stored on it. A row still at
// "latest" (every connection created before pinning, and any created while
// latest.json could not be read) is pinned the first time a run resolves it,
// to the version "latest" means at that moment. A connector version published
// later, on purpose or by mistake, then cannot move a pipeline that already
// runs; moving it is an explicit edit of the connection's version.
//
// The api-gateway pins the same way (services.VersionResolver) with the same
// guarded UPDATE, so whichever service resolves first wins and neither can
// move the other's pin.

// LatestResolver answers what "latest" means for a connector today, from its
// latest.json. *mcp.ServerManager satisfies it.
type LatestResolver interface {
	ResolveConcreteVersion(connectorName, version string) (string, error)
}

// IsFloatingVersion reports whether a stored connector_version follows
// latest.json instead of naming a version. The column is NOT NULL DEFAULT
// 'latest', so floating is "latest" or empty.
func IsFloatingVersion(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "" || v == "latest"
}

// PinVersion stores concrete on a connection whose version still floats and
// returns the version the connection is pinned to. The WHERE clause makes the
// first pin win: when another resolve pinned the row first, nothing is
// written and that pin is returned, so a pin is never moved.
func PinVersion(ctx context.Context, db *sql.DB, connectionID, concrete string) (string, error) {
	var pinned string
	err := db.QueryRowContext(ctx,
		`UPDATE connections SET connector_version = $2 WHERE id = $1 AND lower(btrim(connector_version)) IN ('', 'latest') RETURNING connector_version`,
		connectionID, concrete,
	).Scan(&pinned)
	if err == sql.ErrNoRows {
		err = db.QueryRowContext(ctx,
			`SELECT connector_version FROM connections WHERE id = $1`,
			connectionID,
		).Scan(&pinned)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(pinned), nil
}

// ResolvePinned returns the concrete version a connection runs on. An
// explicit stored version is only normalised ("1.0.0" -> "v1.0.0"). A
// floating one is resolved through r and pinned on the row. A failure never
// fails the caller: an unresolvable "latest" stays as stored (today's
// behaviour), and a failed pin write still runs the version just resolved.
func ResolvePinned(ctx context.Context, db *sql.DB, r LatestResolver, connectionID, connectorType, stored string) string {
	stored = strings.TrimSpace(stored)
	if r == nil || strings.TrimSpace(connectorType) == "" {
		return stored
	}
	if !IsFloatingVersion(stored) {
		if v, err := r.ResolveConcreteVersion(connectorType, stored); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return stored
	}

	concrete, err := r.ResolveConcreteVersion(connectorType, "latest")
	concrete = strings.TrimSpace(concrete)
	if err != nil || concrete == "" || IsFloatingVersion(concrete) {
		log.Warnf("⚠️  Connection %s: cannot resolve %s@latest to a version to pin (%v); running unpinned", connectionID, connectorType, err)
		return stored
	}
	if db == nil || strings.TrimSpace(connectionID) == "" {
		return concrete
	}

	pinned, err := PinVersion(ctx, db, connectionID, concrete)
	if err != nil || IsFloatingVersion(pinned) {
		log.Warnf("⚠️  Connection %s: failed to pin %s to %s (%v); this run uses %s", connectionID, connectorType, concrete, err, concrete)
		return concrete
	}
	if pinned == concrete {
		log.Infof("📌 Pinned connection %s to %s %s (was latest)", connectionID, connectorType, pinned)
	} else {
		log.Infof("📌 Connection %s already pinned to %s %s (latest is %s)", connectionID, connectorType, pinned, concrete)
	}
	return pinned
}
