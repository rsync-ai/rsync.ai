package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/backend-orchestrator/internal/cdc"
)

// connectionReleaseBudget bounds one release. api-gateway waits a little longer
// than this, so it hears the answer rather than its own timeout.
const connectionReleaseBudget = 20 * time.Second

// releaseConnectionObjects drops what a connection's deleted pipelines left on its
// source and returns how many objects it dropped.
type releaseConnectionObjects func(ctx context.Context, db *sql.DB, connectionID string) (int, error)

func releasePostgresConnectionObjects(ctx context.Context, db *sql.DB, connectionID string) (int, error) {
	return cdc.NewPostgreSQLManager(db).ReleaseConnectionObjects(ctx, connectionID)
}

// ReleaseConnectionCDCResources drops the replication slots and publications that
// deleted pipelines left on one connection's source database, now rather than on
// the CDC reconciler's next pass. api-gateway's forced connection delete calls it
// before deleting: the delete erases the cdc_resources rows and the credentials
// the reconciler needs to drop them, so whatever is still undropped afterwards
// stays on the source until somebody drops it by hand.
//
// It answers 200 with success:false when the ledger cannot be read. A slot or
// publication that could not be dropped (source unreachable, slot still active)
// is not an error here: it stays recorded, and the caller reads the ledger again.
func ReleaseConnectionCDCResources(db *sql.DB) gin.HandlerFunc {
	return releaseConnectionCDCResources(db, releasePostgresConnectionObjects, connectionReleaseBudget)
}

func releaseConnectionCDCResources(db *sql.DB, release releaseConnectionObjects, budget time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		connectionID := strings.TrimSpace(c.Param("connection_id"))
		if _, err := uuid.Parse(connectionID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connection id"})
			return
		}
		// SECURITY (tenant isolation): this connects to the source with the
		// connection's decrypted credentials.
		if !assertConnectionOwner(c, db, connectionID) {
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), budget)
		defer cancel()
		dropped, err := release(ctx, db, connectionID)
		if err != nil {
			log.WithError(err).WithField("connection_id", connectionID).
				Warn("connection release: could not read the CDC resources to drop")
			c.JSON(http.StatusOK, gin.H{"success": false, "dropped": 0, "errors": []string{err.Error()}})
			return
		}
		log.WithFields(log.Fields{"connection_id": connectionID, "dropped": dropped}).
			Info("connection release: dropped what deleted pipelines left on the source")
		c.JSON(http.StatusOK, gin.H{"success": true, "dropped": dropped})
	}
}
