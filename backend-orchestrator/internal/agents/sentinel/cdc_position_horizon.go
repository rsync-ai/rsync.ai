package sentinel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/cdc"
	"github.com/rsync-ai/backend-orchestrator/internal/utils"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
	log "github.com/sirupsen/logrus"
)

// ============================================================================
// Position horizon — the WAL watchdog's rule for sources without a slot
//
// A PostgreSQL slot makes the source KEEP its WAL, so a stopped pipeline threatens the
// source's disk (cdc_wal_watchdog.go). MySQL/MariaDB and SQL Server do the opposite: they
// purge their change log on a retention clock whether or not a connector has read it, so a
// stopped pipeline threatens its own POSITION. Once the binlog file (or the change rows)
// behind the stored offset is purged, Start cannot resume and every change since is lost
// to a Reload.
//
// Same rule as the slot: alert at WARN, and at CRITICAL start the stopped pipeline (resume
// a paused one) so it reads past its position before the source purges it — within the
// same per-pipeline budget. Nothing here drops or rewinds anything, and nothing writes to
// the source. A position already purged is reported, not acted on: starting cannot bring
// it back.
// ============================================================================

// horizonRow is a stopped or paused CDC pipeline on a source that purges its change log.
type horizonRow struct {
	pipelineID    string
	pipelineName  string
	pipelineState string
	connectionID  string
	family        string // "mysql" | "sqlserver"
}

// horizonFamily folds a connector_type to the family whose purge clock applies.
func horizonFamily(connectorType string) string {
	t := strings.ToLower(strings.TrimSpace(connectorType))
	switch {
	case strings.HasPrefix(t, "mysql"), strings.HasPrefix(t, "mariadb"):
		return "mysql"
	case strings.HasPrefix(t, "sqlserver"), strings.HasPrefix(t, "mssql"):
		return "sqlserver"
	}
	return ""
}

// horizonOffsetKey is the offset key that carries a family's position.
var horizonOffsetKey = map[string]string{"mysql": "file", "sqlserver": "commit_lsn"}

// checkPositionHorizons measures every stopped or paused MySQL/MariaDB and SQL Server CDC
// pipeline's stored position against the source's purge clock.
func (s *CDCSentinel) checkPositionHorizons(ctx context.Context) {
	defer recoverTick("position_horizon")

	// A pipeline that runs again, or no longer exists, has nothing left to warn about.
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM sentinel_active_issues si
		WHERE si.id LIKE 'cdc-position-%'
		  AND NOT EXISTS (
		      SELECT 1 FROM pipelines p
		      WHERE si.id = 'cdc-position-' || p.id::text
		        AND p.status IN ('stopped', 'failed', 'cancelled', 'paused'))
	`); err != nil {
		log.WithError(err).Debug("🛡️ position horizon: could not clear alerts of running pipelines")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, COALESCE(p.name, ''), p.status, p.source_connection_id, c.connector_type
		FROM pipelines p
		JOIN connections c ON c.id = p.source_connection_id
		WHERE p.status IN ('stopped', 'failed', 'cancelled', 'paused')
		  AND (p.sync_mode = 'cdc' OR p.cdc_mode IS NOT NULL)
		  AND (   c.connector_type ILIKE 'mysql%'
		       OR c.connector_type ILIKE 'mariadb%'
		       OR c.connector_type ILIKE 'sqlserver%'
		       OR c.connector_type ILIKE 'mssql%')
	`)
	if err != nil {
		log.WithError(err).Debug("🛡️ position horizon: failed to query stopped CDC pipelines; skipping")
		return
	}
	var targets []horizonRow
	for rows.Next() {
		var r horizonRow
		var connectorType string
		if err := rows.Scan(&r.pipelineID, &r.pipelineName, &r.pipelineState, &r.connectionID, &connectorType); err != nil {
			continue
		}
		if r.family = horizonFamily(connectorType); r.family != "" {
			targets = append(targets, r)
		}
	}
	rows.Close()

	for _, r := range targets {
		s.checkPositionHorizon(ctx, r)
	}
}

func (s *CDCSentinel) checkPositionHorizon(ctx context.Context, r horizonRow) {
	logger := log.WithFields(log.Fields{"pipeline_id": r.pipelineID, "source": r.family})

	// Stop parks the connector with its offsets; a pipeline that never started has none,
	// and a Connect older than 3.6 has no offsets endpoint. Either way, nothing to measure.
	offsets, ok := s.fetchSourceOffsets(ctx, "cdc-"+utils.SafeID8(r.pipelineID))
	if !ok {
		logger.Debug("🛡️ position horizon: no readable stored position; skipping")
		return
	}
	offset := offsetWithKey(offsets, horizonOffsetKey[r.family])
	if offset == nil {
		logger.Debug("🛡️ position horizon: stored offset carries no position; skipping")
		return
	}

	measure := s.horizonMeasure
	if measure == nil {
		measure = s.measurePositionHorizon
	}
	h, err := measure(ctx, r.family, r.connectionID, offset)
	if err != nil {
		// The error can carry source text; it stays in the log, never in an alert.
		logger.WithError(err).Debug("🛡️ position horizon: cannot measure the source's purge clock; skipping")
		return
	}

	disposition := walDisposition(walSlotRow{
		pipelineID: sql.NullString{String: r.pipelineID, Valid: true}, pipelineState: r.pipelineState,
	})
	remedy := walCriticalRemedy(disposition, s.walAutoAct)
	a := horizonAlert{row: r, horizon: h, disposition: disposition, remedy: remedy}

	switch {
	case h.Safe:
		s.resolveHorizonAlert(ctx, r.pipelineID)
	case h.Purged:
		// Starting cannot bring the position back, so this is reported, never acted on.
		a.sev, a.remedy = IssueSeverityCritical, walRemedyAlertOnly
		s.emitHorizonAlert(ctx, a)
	case h.Remaining <= s.horizonCritical:
		a.sev = IssueSeverityCritical
		if remedy == walRemedyStart || remedy == walRemedyResume {
			a.outcome = s.walRemediate(ctx, r.pipelineID, remedy, "position_retention", map[string]interface{}{
				"source":            r.family,
				"remaining_seconds": int64(h.Remaining / time.Second),
				"retention_seconds": int64(h.Retention / time.Second),
				"message": walDid(remedy) + " automatically: the source was about to purge the change log " +
					"at its stored position. The position was kept.",
			})
		}
		s.emitHorizonAlert(ctx, a)
	case h.Remaining <= s.horizonWarn:
		a.sev = IssueSeverityWarning
		s.emitHorizonAlert(ctx, a)
	default:
		s.resolveHorizonAlert(ctx, r.pipelineID)
	}
}

// offsetWithKey returns the first stored offset that carries key. rsync.ai runs one connector
// per pipeline against one database, so a MySQL or SQL Server connector has one partition.
func offsetWithKey(offsets map[string]interface{}, key string) map[string]interface{} {
	list, _ := offsets["offsets"].([]interface{})
	for _, item := range list {
		entry, _ := item.(map[string]interface{})
		off, _ := entry["offset"].(map[string]interface{})
		if v, ok := off[key].(string); ok && strings.TrimSpace(v) != "" {
			return off
		}
	}
	return nil
}

// measurePositionHorizon reads the source's purge clock for a stored Debezium offset.
func (s *CDCSentinel) measurePositionHorizon(ctx context.Context, family, connectionID string, offset map[string]interface{}) (cdc.PositionHorizon, error) {
	switch family {
	case "mysql":
		pos, eventTime := mysqlOffsetPosition(offset)
		return cdc.NewMySQLManager(s.db).PositionHorizon(ctx, connectionID, pos, eventTime)
	case "sqlserver":
		lsn, _ := offset["commit_lsn"].(string)
		return cdc.NewSQLServerManager(s.db).PositionHorizon(ctx, connectionID, lsn)
	}
	return cdc.PositionHorizon{}, fmt.Errorf("no purge clock for source family %q", family)
}

// mysqlOffsetPosition reads a Debezium MySQL offset: file, pos, and ts_sec, the time of the
// last event the connector read (JSON numbers decode as float64).
func mysqlOffsetPosition(offset map[string]interface{}) (cdc.BinlogPosition, time.Time) {
	var pos cdc.BinlogPosition
	pos.File, _ = offset["file"].(string)
	if p, ok := offset["pos"].(float64); ok && p > 0 {
		pos.Pos = uint64(p)
	}
	pos.GTID, _ = offset["gtids"].(string)
	var eventTime time.Time
	if ts, ok := offset["ts_sec"].(float64); ok && ts > 0 {
		eventTime = time.Unix(int64(ts), 0)
	}
	return pos, eventTime
}

// horizonAlert is one pipeline's position-horizon alert.
type horizonAlert struct {
	row         horizonRow
	horizon     cdc.PositionHorizon
	sev         IssueSeverity
	disposition string
	remedy      walRemedy
	outcome     walOutcome
}

var horizonLogName = map[string]string{"mysql": "binlog", "sqlserver": "change tables"}

// horizonDescription is metadata only — durations, the source family, an HTTP status —
// because it reaches notifications and LLM prompts.
func (s *CDCSentinel) horizonDescription(a horizonAlert) string {
	logName := horizonLogName[a.row.family]
	if a.horizon.Purged {
		return fmt.Sprintf("The %s behind this %s pipeline's stored position is no longer on the source, "+
			"so Start cannot resume from it. Reload the pipeline to re-read its tables.", logName, a.disposition)
	}

	var when string
	if a.horizon.Remaining > 0 {
		when = "in about " + spanOf(a.horizon.Remaining)
	} else {
		when = "at any time now"
	}
	retention := "retention " + spanOf(a.horizon.Retention)
	if a.horizon.RetentionAssumed {
		retention += ", the default; the source did not report its own"
	}
	desc := fmt.Sprintf("This %s pipeline's stored position may be purged from the source's %s %s (%s). "+
		"Once it is, Start cannot resume and the changes since are lost to a Reload.",
		a.disposition, logName, when, retention)

	const kept = " The position is kept — start the pipeline before then."
	if a.sev != IssueSeverityCritical {
		switch a.remedy {
		case walRemedyStart:
			return desc + fmt.Sprintf(" %s before the purge, the watchdog starts the pipeline automatically.", spanOf(s.horizonCritical))
		case walRemedyResume:
			return desc + fmt.Sprintf(" %s before the purge, the watchdog resumes the pipeline automatically.", spanOf(s.horizonCritical))
		}
		return desc + " Automatic action is off (CDC_WAL_WATCHDOG_AUTOACT)." + kept
	}
	switch a.remedy {
	case walRemedyStart, walRemedyResume:
		verb, did := "start", "started"
		if a.remedy == walRemedyResume {
			verb, did = "resume", "resumed"
		}
		switch a.outcome.result {
		case "done":
			return desc + " The pipeline was " + did + " automatically so it reads past its position first; the position is kept."
		case "budget_spent":
			return desc + " The pipeline was already " + did + " automatically " + a.outcome.note +
				" and is not running again, so it was left as is." + kept
		case "refused":
			return desc + " The watchdog could not " + verb + " the pipeline (" + a.outcome.note + ")." + kept
		default:
			return desc + " The watchdog could not " + verb + " the pipeline this time and will retry." + kept
		}
	}
	return desc + " Automatic action is off (CDC_WAL_WATCHDOG_AUTOACT)." + kept
}

// spanOf is humanDuration (notify.go) for a time.Duration.
func spanOf(d time.Duration) string { return humanDuration(int64(d / time.Second)) }

// emitHorizonAlert persists the issue (keyed by pipeline) and emits the same domain event
// and user notification as a WAL alert.
func (s *CDCSentinel) emitHorizonAlert(ctx context.Context, a horizonAlert) {
	description := s.horizonDescription(a)
	action := string(walRemedyAlertOnly)
	if a.sev == IssueSeverityCritical {
		action = string(a.remedy)
	}
	metadata := map[string]interface{}{
		"source":            a.row.family,
		"connection_id":     a.row.connectionID,
		"disposition":       a.disposition,
		"position_purged":   a.horizon.Purged,
		"remaining_seconds": int64(a.horizon.Remaining / time.Second),
		"retention_seconds": int64(a.horizon.Retention / time.Second),
		"retention_assumed": a.horizon.RetentionAssumed,
		"action":            action,
		"critical_action":   string(a.remedy),
		"warn_seconds":      int64(s.horizonWarn / time.Second),
		"critical_seconds":  int64(s.horizonCritical / time.Second),
	}
	if a.outcome.result != "" {
		metadata["action_result"] = a.outcome.result
	}
	metaJSON, _ := json.Marshal(metadata)

	pipelineID := a.row.pipelineID
	// detected_at is when this episode opened: NOW() on insert, kept on conflict.
	var detectedAt time.Time
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO sentinel_active_issues (
			id, type, severity, component_id, component_type,
			description, detected_at, occurrence_count, last_occurrence, metadata
		) VALUES ($1, $2, $3, $4, $5, $6, NOW(), 1, NOW(), $7)
		ON CONFLICT (id) DO UPDATE SET
			occurrence_count = sentinel_active_issues.occurrence_count + 1,
			last_occurrence  = NOW(),
			severity         = EXCLUDED.severity,
			description      = EXCLUDED.description,
			metadata         = EXCLUDED.metadata
		RETURNING detected_at
	`, "cdc-position-"+pipelineID, string(IssueTypeHighLag), string(a.sev),
		pipelineID, string(ComponentTypeCDCPipeline), description, metaJSON).Scan(&detectedAt); err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Warn("🛡️ position horizon: failed to persist issue")
	}

	status := "warning"
	envelope := diagnose.SeverityWarning
	if a.sev == IssueSeverityCritical {
		status = "critical"
		envelope = diagnose.SeverityError
	}
	log.WithFields(log.Fields{
		"pipeline_id": pipelineID,
		"source":      a.row.family,
		"severity":    status,
		"action":      action,
	}).Warn("🛡️ position horizon: stored-position alert emitted")

	// The code gives it its own headline and the structured error its level in the bell,
	// as for the WAL alert: without them the api-gateway titled it after its type and
	// filed it as info.
	se := diagnose.NewStructuredError("CDC_POSITION_RETENTION", diagnose.FailureTypeSystemError, diagnose.AudienceUser, description)
	se.Severity = envelope
	notification := map[string]interface{}{
		"type":        "cdc_position_retention",
		"pipeline_id": pipelineID,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  notificationActionURL(pipelineID),
		"message":     description,
		"severity":    status,
		"_code":       "CDC_POSITION_RETENTION",
		"error":       se,
	}
	stampEpisodeOpened(notification, detectedAt)
	emitNotification(s.kafkaManager, pipelineID, IssueTypeHighLag, notification)

	if s.kafkaManager == nil {
		return
	}
	event := map[string]interface{}{
		"schema_version": 2,
		"event_type":     "SENTINEL_ALERT",
		"pipeline_id":    pipelineID,
		"execution_id":   "",
		"trace_id":       pipelineID,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
		"stage":          "cdc_sentinel",
		"stage_group":    "monitoring",
		"status":         status,
		"message":        description,
		"metadata": map[string]interface{}{
			"source":     "cdc_wal_watchdog",
			"alert_type": "position_retention",
			"details":    metadata,
		},
	}
	if b, err := json.Marshal(event); err == nil {
		_ = s.kafkaManager.ProduceWithHeaders("pipeline.domain.events", []byte(pipelineID), b,
			map[string]string{"trace_id": pipelineID})
	}
}

func (s *CDCSentinel) resolveHorizonAlert(ctx context.Context, pipelineID string) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sentinel_active_issues WHERE id = $1`, "cdc-position-"+pipelineID); err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Debug("🛡️ position horizon: resolve delete failed (may not exist)")
	}
}
