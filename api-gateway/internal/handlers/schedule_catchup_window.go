package handlers

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"api-gateway/internal/db"

	log "github.com/sirupsen/logrus"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

var scheduleCatchupReconcileOnce sync.Once

// ReconcileScheduleCatchupWindows widens the catch-up window of every pipeline
// and model schedule made before scheduleCatchupWindow, which Temporal keeps
// until something rewrites the policy. It retries in the background until
// Temporal answers for all of them. A schedule already at the window or wider
// is not written, so after the first pass a restart only reads.
func ReconcileScheduleCatchupWindows(ctx context.Context) {
	scheduleCatchupReconcileOnce.Do(func() {
		go func() {
			for pause := 5 * time.Second; ; pause *= 2 {
				if pause > 2*time.Minute {
					pause = 2 * time.Minute
				}
				done, err := reconcileScheduleCatchupWindows(ctx, db.GetDB())
				if done {
					return
				}
				log.WithError(err).Warn("[schedules] could not widen schedule catch-up windows; retrying")
				if !waitOrDone(ctx, pause) {
					return
				}
			}
		}()
	})
}

// reconcileScheduleCatchupWindows reports done once every listed schedule was
// widened, already wide, or is gone from Temporal.
func reconcileScheduleCatchupWindows(ctx context.Context, database *sql.DB) (bool, error) {
	if database == nil {
		return false, errors.New("database not available")
	}
	tc := getTemporalClient()
	if tc == nil {
		return false, errNoTemporalClient
	}
	// Paused schedules too: a resume must not bring the old window back. Event
	// triggered model schedules have no Temporal schedule (NULL id).
	rows, err := database.QueryContext(ctx, `
		SELECT temporal_schedule_id FROM pipeline_schedules
		WHERE status <> 'deleted' AND temporal_schedule_id <> ''
		UNION
		SELECT temporal_schedule_id FROM saved_query_schedules
		WHERE status <> 'deleted' AND temporal_schedule_id IS NOT NULL AND temporal_schedule_id <> ''`)
	if err != nil {
		return isUndefinedTable(err), err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	widened := 0
	var lastErr error
	for _, id := range ids {
		updateCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		wrote := false
		err := tc.ScheduleClient().GetHandle(updateCtx, id).Update(updateCtx, client.ScheduleUpdateOptions{
			DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
				s := in.Description.Schedule
				p := withScheduleCatchupWindow(s.Policy)
				if s.Policy != nil && p.CatchupWindow == s.Policy.CatchupWindow {
					return nil, temporal.ErrSkipScheduleUpdate
				}
				s.Policy = p
				wrote = true
				return &client.ScheduleUpdate{Schedule: &s}, nil
			},
		})
		cancel()
		var nf *serviceerror.NotFound
		switch {
		case err == nil:
			if wrote {
				widened++
			}
		case errors.As(err, &nf):
			// Deleted in Temporal but not here: nothing to widen.
		default:
			log.WithError(err).WithField("temporal_schedule_id", id).Warn("[schedules] could not widen the catch-up window")
			lastErr = err
		}
	}
	if widened > 0 {
		log.WithFields(log.Fields{"widened": widened, "schedules": len(ids), "catchup_window": scheduleCatchupWindow.String()}).
			Info("[schedules] widened old catch-up windows")
	}
	return lastErr == nil, lastErr
}
