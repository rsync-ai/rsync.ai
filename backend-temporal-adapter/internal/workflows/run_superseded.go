package workflows

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// A new run replaces a parked one (and closes a dead one) before that run's
// workflow has finished: the gateway closes the old execution and signals its
// workflow to stop (api-gateway pipeline_run_overlap.go). The old workflow still
// makes its last writes as it exits — pipelines.status "stopped", a progress
// state — and both of those rows are per pipeline, so without this check they
// would land on top of the run that replaced it.
//
// A run is superseded when its own execution was closed by someone else (it is
// no longer running or pending) AND a newer run of the same pipeline started
// after it. Both are needed: a run whose newer sibling failed to start keeps its
// open execution, and must still record its result.
//
// Kept identical to the copy in api-gateway/internal/projector (event_projector.go),
// which guards the progress row the same way; superseded_run_guard_test.go there
// fails if the two drift.
const runSupersededSQL = `
	SELECT EXISTS (
		SELECT 1
		FROM executions mine
		JOIN executions newer ON newer.pipeline_id = mine.pipeline_id
		WHERE mine.id = $1::uuid
		  AND mine.status NOT IN ('running', 'pending')
		  AND newer.id <> newer.pipeline_id
		  AND COALESCE(newer.trigger_source, '') <> 'cdc'
		  AND newer.start_time > mine.start_time)`

// runSuperseded reports whether a newer run owns the pipeline (see above). It
// fails open: no execution id, an id that is not a UUID, or a query error all
// read as "not superseded", so the write goes ahead exactly as it did before.
func runSuperseded(ctx context.Context, db *sql.DB, executionID string) bool {
	if db == nil || executionID == "" {
		return false
	}
	if _, err := uuid.Parse(executionID); err != nil {
		return false
	}
	var superseded bool
	if err := db.QueryRowContext(ctx, runSupersededSQL, executionID).Scan(&superseded); err != nil {
		log.WithError(err).WithField("execution_id", executionID).Warn("run superseded check failed; writing as usual")
		return false
	}
	return superseded
}
