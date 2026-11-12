package sentinel

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// The bug class: an alert raised by a detector that reads RUNNING CDC pipelines only
// outlives the pipeline's run. Each of these alerts is cleared by the same detector once
// it sees the stream healthy again, but every one of them selects `status = 'running'`
// (activeCDCPipelinesQuery, captureDependencyQuery), and the dependency probe stops
// probing the statuses below. So once the pipeline is stopped — by its owner, or by the
// Sentinel's own restart cap — nothing ever looks at it again, and the alert stays open
// for good: a capture-stopped banner on a pipeline the owner deliberately stopped.
//
// The close is silent. The owner already got the Stop notice (D1), and the resolved
// copy would say the stream is moving again, which on a stopped pipeline is false. If
// the problem is still there after Resume, the detector raises the alert again.

// alertsClosedOnStop are the alerts that describe a RUNNING pipeline's stream and are
// re-checked only while it runs. Each builder's id is a fixed prefix + the pipeline id
// (TestAlertsClosedOnStopAreAPrefixPlusThePipelineID).
//
// Kept open on purpose (TestEveryCDCAlertIsClosedOnStopOrKeptForAReason):
//   - connectorIssueID — the Sentinel's terminal verdict; it is WHY the pipeline stopped.
//   - sinkWedgedEscalationIssueID — a terminal escalation; it closes once the sink applies again after Start (resolveSinkWedged).
//   - rowsParkedIssueID — the rows are still parked whether or not the pipeline runs.
//   - cdc-position-* — raised only on a stopped pipeline (its resume position is gone).
//   - cdc-wal-<slot> — keyed by slot; a stopped pipeline's slot still holds WAL.
var alertsClosedOnStop = []func(pipelineID string) string{
	captureStoppedIssueID,
	sourceStalledIssueID,
	sourceLagIssueID,
	sinkLagIssueID,
	sinkAbsentIssueID,
}

// stoppedPipelineStatuses are the statuses no CDC detector re-checks: the same list the
// dependency probe skips (workers/dependency_probe.go). Listed rather than "not
// running", so a transient status (a pipeline being started or stopped) keeps its alerts.
var stoppedPipelineStatuses = []string{"stopped", "paused", "failed", "cancelled", "completed", "archived"}

// closeOnStopSQL deletes, in one statement, every alertsClosedOnStop row whose pipeline
// is in a stopped status. Built once: the ids are matched as prefix || pipeline id so
// the query needs no list of pipelines.
var closeOnStopSQL, closeOnStopArgs = buildCloseOnStopQuery()

func buildCloseOnStopQuery() (string, []interface{}) {
	args := []interface{}{string(ComponentTypeCDCPipeline)}
	statuses := make([]string, len(stoppedPipelineStatuses))
	for i, st := range stoppedPipelineStatuses {
		args = append(args, st)
		statuses[i] = fmt.Sprintf("$%d", len(args))
	}
	ids := make([]string, len(alertsClosedOnStop))
	for i, issueID := range alertsClosedOnStop {
		args = append(args, issueID(""))
		ids[i] = fmt.Sprintf("$%d::text || p.id::text", len(args))
	}
	return fmt.Sprintf(`
		DELETE FROM sentinel_active_issues si
		USING pipelines p
		WHERE si.component_type = $1
		  AND si.component_id = p.id::text
		  AND p.status IN (%s)
		  AND si.id IN (%s)
		RETURNING si.id`,
		strings.Join(statuses, ", "), strings.Join(ids, ", ")), args
}

// closeAlertsOfStoppedPipelines silently closes the running-only alerts of every CDC
// pipeline that is no longer running. Fail-soft: an error leaves the rows for the next
// tick.
func (s *CDCSentinel) closeAlertsOfStoppedPipelines(ctx context.Context) {
	if s.db == nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, closeOnStopSQL, closeOnStopArgs...)
	if err != nil {
		log.WithError(err).Debug("🛡️ Sentinel could not close the alerts of stopped CDC pipelines")
		return
	}
	defer rows.Close()
	var closed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.WithError(err).Debug("🛡️ Sentinel failed to scan a closed CDC alert id")
			continue
		}
		closed = append(closed, id)
	}
	if len(closed) > 0 {
		log.WithField("issue_ids", closed).
			Info("🛡️ Closed alerts of stopped CDC pipelines (nothing re-checks a stopped pipeline; no notice sent)")
	}
}
