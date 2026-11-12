package sentinel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/cdc"
	"github.com/rsync-ai/backend-orchestrator/internal/connections"
	"github.com/rsync-ai/backend-orchestrator/internal/utils"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
	log "github.com/sirupsen/logrus"
)

// ============================================================================
// WAL watchdog (Part B)
//
// PROBLEM: a PostgreSQL replication slot forces the source server to retain all
// WAL since the slot's restart_lsn. If a slot stops being consumed — the owning
// pipeline was STOPPED (but not deleted) or its Debezium connector stalled — the
// retained WAL grows without bound and eventually fills the source's disk,
// taking down not just CDC but the entire source database. Azure/RDS managed
// Postgres are especially unforgiving here (fixed disk, hard to grow live).
//
// The Part A reconciler reaps slots of DELETED pipelines on a fixed 5m cadence.
// A STOPPED pipeline keeps its slot — it is the position Start resumes from — so
// its WAL keeps growing while it is stopped. The watchdog is the WAL-pressure
// safety net for exactly that: it measures retained WAL directly, ALERTS in two
// tiers so the user sees the problem early, and at CRITICAL fixes the pipeline
// instead of the slot:
//
//   - stopped (or failed/cancelled) → START it (resume mode), so it consumes the slot again
//   - paused                        → RESUME its connector
//   - deleted                       → drop the slot; there is nothing left to resume
//   - running                       → alert only; the connector-health rungs own it
//
// SAFETY: the watchdog NEVER drops the slot of a pipeline that still exists. That
// slot is the pipeline's position, and the user did nothing to give it up; losing
// it would silently lose every change made while the pipeline was not running.
// Auto-starts are capped per pipeline (walAutoStartCap in walAutoStartWindow),
// because the sentinel itself stops a pipeline whose connector keeps failing; once
// the cap is spent the alert says so and the slot is still kept.
// ============================================================================

func walDurationFromEnv(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 30*time.Second {
		return d
	}
	return def
}

func walBytesFromEnv(name string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return n
	}
	return def
}

func walBoolFromEnv(name string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

// walWatchdogLoop periodically inspects retained WAL per replication slot.
func (s *CDCSentinel) walWatchdogLoop() {
	// Sanity: critical must be >= warn, else the warn tier would never resolve.
	if s.walCriticalBytes < s.walWarnBytes {
		s.walCriticalBytes = s.walWarnBytes
	}
	// Same for the position horizon, which counts DOWN: critical must be the shorter.
	if s.horizonCritical > s.horizonWarn {
		s.horizonCritical = s.horizonWarn
	}
	log.WithFields(log.Fields{
		"interval":         s.walWatchdogInterval.String(),
		"warn_mb":          s.walWarnBytes / (1024 * 1024),
		"critical_mb":      s.walCriticalBytes / (1024 * 1024),
		"auto_act":         s.walAutoAct,
		"horizon_warn":     s.horizonWarn.String(),
		"horizon_critical": s.horizonCritical.String(),
	}).Info("🛡️ CDC WAL watchdog started (retained-WAL monitor; at critical starts a stopped pipeline, never drops its slot)")

	ticker := time.NewTicker(s.walWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkWALRetention(s.ctx)
			s.checkPositionHorizons(s.ctx)
		}
	}
}

// walSlotRow is one PostgreSQL replication slot resource joined to its (possibly
// absent) owning pipeline.
type walSlotRow struct {
	slotName      string
	connectionID  string
	pipelineID    sql.NullString
	pipelineState string // '' when the pipeline was deleted (LEFT JOIN miss)
	pipelineName  string
}

// walRemedy is what the watchdog does about a slot past CRITICAL.
type walRemedy string

const (
	walRemedyAlertOnly  walRemedy = "none"
	walRemedyDropOrphan walRemedy = "drop_orphan_slot"
	walRemedyStart      walRemedy = "auto_start_pipeline"
	walRemedyResume     walRemedy = "auto_resume_pipeline"
)

const (
	// walAutoStartCap auto-starts/resumes per pipeline in walAutoStartWindow, the same
	// budget the healer's backoff retry uses.
	walAutoStartCap    = 3
	walAutoStartWindow = "24 hours"
)

// walStoppedStates are the pipeline states in which nothing consumes the slot and Start
// is the way back: the user's Stop, and the two ends a run reaches on its own.
var walStoppedStates = map[string]bool{"stopped": true, "failed": true, "cancelled": true}

// walDisposition buckets a slot by its pipeline: deleted, paused, stopped, or the
// pipeline's own state for everything else (running, waiting_for_user, …).
func walDisposition(sl walSlotRow) string {
	switch {
	case !sl.pipelineID.Valid || sl.pipelineState == "":
		return "deleted"
	case sl.pipelineState == "paused":
		return "paused"
	case walStoppedStates[sl.pipelineState]:
		return "stopped"
	default:
		return sl.pipelineState
	}
}

// walCriticalRemedy maps a disposition to the CRITICAL-tier action. Only a deleted
// pipeline's slot is ever dropped.
func walCriticalRemedy(disposition string, autoAct bool) walRemedy {
	if !autoAct {
		return walRemedyAlertOnly
	}
	switch disposition {
	case "deleted":
		return walRemedyDropOrphan
	case "stopped":
		return walRemedyStart
	case "paused":
		return walRemedyResume
	}
	return walRemedyAlertOnly
}

// checkWALRetention inspects retained WAL for every tracked PostgreSQL
// replication slot and applies the two-tier alert/action policy.
func (s *CDCSentinel) checkWALRetention(ctx context.Context) {
	defer recoverTick("wal_watchdog")
	rows, err := s.db.QueryContext(ctx, `
		SELECT cr.resource_name, cr.connection_id, cr.pipeline_id,
		       COALESCE(p.status, ''), COALESCE(p.name, '')
		FROM cdc_resources cr
		LEFT JOIN pipelines p ON p.id = cr.pipeline_id
		WHERE cr.resource_type = 'replication_slot'
		  AND cr.database_type = 'postgresql'
		  AND cr.status IN ('active', 'inactive', 'failed', 'orphaned')
	`)
	if err != nil {
		log.WithError(err).Debug("🛡️ WAL watchdog: failed to query slots; skipping")
		return
	}
	defer rows.Close()

	var slots []walSlotRow
	for rows.Next() {
		var r walSlotRow
		if err := rows.Scan(&r.slotName, &r.connectionID, &r.pipelineID, &r.pipelineState, &r.pipelineName); err != nil {
			continue
		}
		slots = append(slots, r)
	}
	if len(slots) == 0 {
		return
	}

	// Cache one source connection per connection_id for this sweep.
	openSource := s.walOpenSource
	if openSource == nil {
		mgr := connections.NewManager(s.db)
		openSource = func(ctx context.Context, connectionID string) (*sql.DB, error) {
			return s.openSourcePG(ctx, mgr, connectionID)
		}
	}
	conns := make(map[string]*sql.DB)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	var orphanSlots []string                // CRITICAL slots of deleted pipelines, dropped after the loop
	remedied := make(map[string]walOutcome) // pipeline → this sweep's start/resume; a pipeline can own several slots
	for _, sl := range slots {
		srcDB := conns[sl.connectionID]
		if srcDB == nil {
			db, derr := openSource(ctx, sl.connectionID)
			if derr != nil {
				log.WithError(derr).WithField("connection_id", sl.connectionID).
					Debug("🛡️ WAL watchdog: cannot open source; skipping its slots")
				conns[sl.connectionID] = nil // remember the failure (nil) so we don't retry every slot
				continue
			}
			conns[sl.connectionID] = db
			srcDB = db
		}

		var retained int64
		var active bool
		qerr := srcDB.QueryRowContext(ctx,
			`SELECT COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn), 0), active
			 FROM pg_replication_slots WHERE slot_name = $1`, sl.slotName).Scan(&retained, &active)
		if qerr != nil {
			// No row → the physical slot is already gone (dropped). Nothing to watch.
			s.resolveWALAlert(ctx, sl.slotName)
			continue
		}

		disposition := walDisposition(sl)
		remedy := walCriticalRemedy(disposition, s.walAutoAct)
		retainedMB := float64(retained) / (1024 * 1024)

		switch {
		case retained >= s.walCriticalBytes:
			var outcome walOutcome
			switch remedy {
			case walRemedyStart, walRemedyResume:
				// Act first so the alert can say what happened.
				o, done := remedied[sl.pipelineID.String]
				if !done {
					o = s.walRemediate(ctx, sl.pipelineID.String, remedy, "wal_pressure", map[string]interface{}{
						"slot_name":      sl.slotName,
						"retained_bytes": retained,
						"message": walDid(remedy) + " automatically: its replication slot passed the critical WAL threshold. " +
							"The slot was kept.",
					})
					remedied[sl.pipelineID.String] = o
				}
				outcome = o
			case walRemedyDropOrphan:
				orphanSlots = append(orphanSlots, sl.slotName)
			}
			s.emitWALAlert(ctx, sl, walAlert{
				retained: retained, active: active, sev: IssueSeverityCritical,
				disposition: disposition, remedy: remedy, outcome: outcome,
			})
		case retained >= s.walWarnBytes:
			s.emitWALAlert(ctx, sl, walAlert{
				retained: retained, active: active, sev: IssueSeverityWarning,
				disposition: disposition, remedy: remedy,
			})
		default:
			// Below warn → healthy, clear any stale alert for this slot.
			s.resolveWALAlert(ctx, sl.slotName)
			log.WithFields(log.Fields{
				"slot":        sl.slotName,
				"retained_mb": fmt.Sprintf("%.1f", retainedMB),
			}).Debug("🛡️ WAL watchdog: slot healthy")
		}
	}

	// One drop pass for the CRITICAL slots of deleted pipelines, without waiting for the
	// reconciler's next 5m pass. It goes through the reaper, which re-selects deleted
	// pipelines only, so a pipeline created or restored since the listing keeps its slot.
	if len(orphanSlots) > 0 {
		dropped, derr := cdc.NewPostgreSQLManager(s.db).ReapNamedOrphanedSlots(ctx, orphanSlots)
		if derr != nil {
			log.WithError(derr).Warn("🛡️ WAL watchdog: drop of critical orphaned slots failed")
		} else {
			if dropped > 0 {
				log.Warnf("🛡️ WAL watchdog: dropped %d critical replication slot(s) of deleted pipelines", dropped)
			}
			// Clear the active-issue rows for the slots just dropped. Once a slot
			// resource flips to 'deleted' it is no longer selected by this loop, so a
			// later sweep would never resolve its alert — do it here.
			for _, slotName := range orphanSlots {
				s.resolveWALAlert(ctx, slotName)
			}
		}
	}
}

// walOutcome is what a start/resume attempt came to. note is metadata only (counts, an
// HTTP status code): it goes into alert text, which reaches notifications and LLM prompts.
type walOutcome struct {
	result string // "done" | "budget_spent" | "budget_unknown" | "refused" | "unreachable"
	note   string
}

// walDid is the past tense of a start/resume remedy, for timeline and alert text.
func walDid(remedy walRemedy) string {
	if remedy == walRemedyResume {
		return "Resumed"
	}
	return "Started"
}

// walRemediate starts a stopped pipeline or resumes a paused one, within the per-pipeline
// budget, and records each success on the run timeline, which is also what the budget
// counts. reason names the pressure ("wal_pressure", "position_retention"); detail is the
// timeline event's payload, metadata only. It never touches the slot or the stored position.
func (s *CDCSentinel) walRemediate(ctx context.Context, pipelineID string, remedy walRemedy, reason string, detail map[string]interface{}) walOutcome {
	logger := log.WithFields(log.Fields{"pipeline_id": pipelineID, "action": remedy, "reason": reason})

	var used int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pipeline_run_events
		WHERE pipeline_id = $1
		  AND event_type IN ('wal_watchdog_auto_start', 'wal_watchdog_auto_resume')
		  AND created_at >= NOW() - $2::interval
	`, pipelineID, walAutoStartWindow).Scan(&used); err != nil {
		// Unknown budget → do not act; a start loop is worse than one more sweep's wait.
		logger.WithError(err).Warn("🛡️ WAL watchdog: cannot read the auto-start budget; not acting this sweep")
		return walOutcome{result: "budget_unknown"}
	}
	if used >= walAutoStartCap {
		logger.WithField("used", used).Warn("🛡️ WAL watchdog: auto-start budget spent; position kept, pipeline left as is")
		return walOutcome{result: "budget_spent", note: fmt.Sprintf("%d times in the last %s", used, walAutoStartWindow)}
	}

	eventType := "wal_watchdog_auto_start"
	var status int
	var err error
	if remedy == walRemedyResume {
		eventType = "wal_watchdog_auto_resume"
		status, err = s.walResumePipeline(ctx, pipelineID)
	} else {
		status, err = s.walStartPipeline(ctx, pipelineID, reason)
	}
	if err != nil {
		logger.WithError(err).Warn("🛡️ WAL watchdog: could not reach the service to start the pipeline; position kept")
		return walOutcome{result: "unreachable"}
	}
	if status < 200 || status >= 300 {
		logger.WithField("http_status", status).Warn("🛡️ WAL watchdog: start of the pipeline was refused; position kept")
		return walOutcome{result: "refused", note: fmt.Sprintf("HTTP %d", status)}
	}

	if remedy == walRemedyResume {
		// The connector is running again; say so the way the CDC resume route does. Guarded
		// on 'paused' so a Stop or Delete made meanwhile is not overwritten.
		if _, uerr := s.db.ExecContext(ctx,
			`UPDATE pipelines SET status = 'running', updated_at = NOW() WHERE id = $1 AND status = 'paused'`,
			pipelineID); uerr != nil {
			logger.WithError(uerr).Warn("🛡️ WAL watchdog: connector resumed but pipeline status not updated")
		}
	}

	event := map[string]interface{}{"reason": reason}
	for k, v := range detail {
		event[k] = v
	}
	payload, _ := json.Marshal(event)
	if _, eerr := s.db.ExecContext(ctx, `
		INSERT INTO pipeline_run_events (pipeline_id, event_id, event_type, severity, payload, redacted)
		VALUES ($1, $2, $3, 'warn', $4, false)
		ON CONFLICT DO NOTHING
	`, pipelineID, fmt.Sprintf("%s-%d", eventType, time.Now().UnixNano()), eventType, payload); eerr != nil {
		logger.WithError(eerr).Warn("🛡️ WAL watchdog: pipeline started but its timeline event was not written")
	}
	logger.Warn("🛡️ WAL watchdog: pipeline started so it reads from its kept position again")
	return walOutcome{result: "done"}
}

// walStartPipeline asks the api-gateway to run a stopped pipeline through the internal
// route the healer's backoff retry uses (authenticated by X-Internal-Secret).
func (s *CDCSentinel) walStartPipeline(ctx context.Context, pipelineID, reason string) (int, error) {
	body, _ := json.Marshal(map[string]string{"triggered_by": "cdc_wal_watchdog", "reason": reason})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/internal/pipelines/%s/run", s.apiGatewayURL, pipelineID), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Source", "cdc_wal_watchdog")
	if secret := os.Getenv("INTERNAL_SERVICE_SECRET"); secret != "" {
		req.Header.Set("X-Internal-Secret", secret)
	}
	return s.walDo(req)
}

// walResumePipeline resumes a paused pipeline's source connector, as the CDC resume route does.
func (s *CDCSentinel) walResumePipeline(ctx context.Context, pipelineID string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/connectors/cdc-%s/resume", s.connectURL, utils.SafeID8(pipelineID)), nil)
	if err != nil {
		return 0, err
	}
	return s.walDo(req)
}

func (s *CDCSentinel) walDo(req *http.Request) (int, error) {
	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// openSourcePG opens a short-lived connection to a source PostgreSQL using the
// stored connection config, with host-aware SSL (managed sources require TLS).
func (s *CDCSentinel) openSourcePG(ctx context.Context, mgr *connections.Manager, connectionID string) (*sql.DB, error) {
	cfg, err := mgr.Get(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	host := cfg["host"]
	port := 5432
	if p := strings.TrimSpace(cfg["port"]); p != "" {
		if n, err2 := fmt.Sscanf(p, "%d", &port); n == 0 || err2 != nil {
			port = 5432
		}
	}
	database := cfg["database"]
	if database == "" {
		database = cfg["db_name"]
	}
	sslmode := cdc.ResolvePostgresSSLMode(
		map[string]interface{}{"sslmode": cfg["sslmode"], "ssl_mode": cfg["ssl_mode"]}, host)

	connStr := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=5",
		host, port, database, cfg["user"], cfg["password"], sslmode)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// walAlert is one slot's alert: its measurement, tier, and what CRITICAL does (remedy)
// or, at CRITICAL, what it came to (outcome).
type walAlert struct {
	retained    int64
	active      bool
	sev         IssueSeverity
	disposition string
	remedy      walRemedy
	outcome     walOutcome
}

// walAlertAdvice is the sentence after the measurement: what the watchdog did or will do.
// Every path that leaves an existing pipeline's slot in place says so.
func (s *CDCSentinel) walAlertAdvice(a walAlert) string {
	criticalMB := float64(s.walCriticalBytes) / (1024 * 1024)
	const kept = " The slot is kept, so no change is lost — start the pipeline, or delete it to release the slot."
	if a.sev != IssueSeverityCritical {
		switch a.remedy {
		case walRemedyStart:
			return fmt.Sprintf(" At %.0f MB the watchdog will start the pipeline automatically so it consumes the slot; the slot is kept.", criticalMB)
		case walRemedyResume:
			return fmt.Sprintf(" At %.0f MB the watchdog will resume the pipeline automatically so it consumes the slot; the slot is kept.", criticalMB)
		case walRemedyDropOrphan:
			return fmt.Sprintf(" The pipeline was deleted; at %.0f MB its orphaned slot is dropped.", criticalMB)
		}
	} else {
		switch a.remedy {
		case walRemedyDropOrphan:
			return " The pipeline was deleted, so its orphaned slot is dropped."
		case walRemedyStart, walRemedyResume:
			verb, did := "start", "started"
			if a.remedy == walRemedyResume {
				verb, did = "resume", "resumed"
			}
			switch a.outcome.result {
			case "done":
				return " The pipeline was " + did + " automatically so it consumes the slot again; the slot is kept."
			case "budget_spent":
				return " The pipeline was already " + did + " automatically " + a.outcome.note +
					" and is not running again, so it was left as is." + kept
			case "refused":
				return " The watchdog could not " + verb + " the pipeline (" + a.outcome.note + ")." + kept
			default:
				return " The watchdog could not " + verb + " the pipeline this time and will retry." + kept
			}
		}
	}
	if a.disposition == "deleted" {
		return " The pipeline was deleted; automatic action is off, so the slot is kept until the reconciler drops it."
	}
	if a.disposition == "stopped" || a.disposition == "paused" {
		return " Automatic action is off (CDC_WAL_WATCHDOG_AUTOACT)." + kept
	}
	return " The pipeline is " + a.disposition + " but not consuming this slot — check its CDC connector. The slot is kept."
}

// emitWALAlert persists a WAL-pressure issue (keyed by slot so deleted pipelines
// are still covered) and emits a SENTINEL_ALERT domain event + a user
// notification on rsync.notifications.
func (s *CDCSentinel) emitWALAlert(ctx context.Context, sl walSlotRow, a walAlert) {
	retained, active, sev, disposition := a.retained, a.active, a.sev, a.disposition
	retainedMB := float64(retained) / (1024 * 1024)
	pipelineID := ""
	if sl.pipelineID.Valid {
		pipelineID = sl.pipelineID.String
	}

	// action is what this alert's sweep did; critical_action what CRITICAL does.
	action := string(walRemedyAlertOnly)
	if sev == IssueSeverityCritical {
		action = string(a.remedy)
	}

	description := fmt.Sprintf(
		"Replication slot %s is retaining %.1f MB of WAL on the source (pipeline %s, consumer active=%t). "+
			"Unbounded WAL retention can exhaust source disk.",
		sl.slotName, retainedMB, disposition, active) + s.walAlertAdvice(a)

	metadata := map[string]interface{}{
		"slot_name":       sl.slotName,
		"connection_id":   sl.connectionID,
		"retained_bytes":  retained,
		"retained_mb":     retainedMB,
		"slot_active":     active,
		"disposition":     disposition,
		"action":          action,
		"critical_action": string(a.remedy),
		"warn_bytes":      s.walWarnBytes,
		"critical_bytes":  s.walCriticalBytes,
	}
	if a.outcome.result != "" {
		metadata["action_result"] = a.outcome.result
	}
	metaJSON, _ := json.Marshal(metadata)

	// Persist to sentinel_active_issues (keyed by slot name).
	issueID := fmt.Sprintf("cdc-wal-%s", sl.slotName)
	componentID := pipelineID
	if componentID == "" {
		componentID = sl.slotName
	}
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
	`, issueID, string(IssueTypeHighLag), string(sev),
		componentID, string(ComponentTypeCDCPipeline), description, metaJSON).Scan(&detectedAt); err != nil {
		log.WithError(err).WithField("slot", sl.slotName).Warn("🛡️ WAL watchdog: failed to persist issue")
	}

	status := "warning"
	envelope := diagnose.SeverityWarning
	if sev == IssueSeverityCritical {
		status = "critical"
		envelope = diagnose.SeverityError
	}

	// User notification (Slack/email fan-out happens off rsync.notifications). Its
	// severity is the one a warning that turns critical is pushed again for. The code
	// gives it its own headline and the structured error its level in the bell: without
	// them the api-gateway titled it after its type and filed it as info.
	se := diagnose.NewStructuredError("CDC_WAL_PRESSURE", diagnose.FailureTypeSystemError, diagnose.AudienceUser, description)
	se.Severity = envelope
	notification := map[string]interface{}{
		"type":        "cdc_wal_pressure",
		"pipeline_id": pipelineID,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  notificationActionURL(pipelineID),
		"message":     description,
		"severity":    status,
		"_code":       "CDC_WAL_PRESSURE",
		"error":       se,
	}
	stampEpisodeOpened(notification, detectedAt)
	emitNotification(s.kafkaManager, sl.slotName, IssueTypeHighLag, notification)

	if s.kafkaManager == nil {
		return
	}

	// Domain event for the pipeline overview UI (only when we have a pipeline).
	if pipelineID != "" {
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
				"alert_type": "wal_retention",
				"details":    metadata,
			},
		}
		if b, err := json.Marshal(event); err == nil {
			_ = s.kafkaManager.ProduceWithHeaders("pipeline.domain.events", []byte(pipelineID), b,
				map[string]string{"trace_id": pipelineID})
		}
	}

	log.WithFields(log.Fields{
		"slot":        sl.slotName,
		"pipeline_id": pipelineID,
		"retained_mb": fmt.Sprintf("%.1f", retainedMB),
		"severity":    status,
		"action":      action,
	}).Warn("🛡️ WAL watchdog: retained-WAL alert emitted")
}

// resolveWALAlert clears a WAL-pressure issue once the slot is healthy or gone, and tells
// the pipeline's owner when that closes an alert that was open. A slot with no pipeline
// (component_id is the slot name, see emitWALAlert) has no owner to tell.
func (s *CDCSentinel) resolveWALAlert(ctx context.Context, slotName string) {
	issueID := fmt.Sprintf("cdc-wal-%s", slotName)
	var componentID string
	var openedAt time.Time
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM sentinel_active_issues WHERE id = $1 RETURNING component_id, detected_at`, issueID).
		Scan(&componentID, &openedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return // nothing was open
	}
	if err != nil {
		log.WithError(err).WithField("slot", slotName).Debug("🛡️ WAL watchdog: resolve delete failed (may not exist)")
		return
	}
	if componentID == "" || componentID == slotName {
		return
	}
	publishResolved(s.kafkaManager, slotName, resolvedWAL, componentID, notificationActionURL(componentID), "", "", openedAt)
}

func notificationActionURL(pipelineID string) string {
	if pipelineID == "" {
		return "/pipelines"
	}
	return "/pipelines/" + pipelineID
}
