package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"api-gateway/internal/config"
	"api-gateway/internal/security"

	kafkaclient "github.com/rsync-ai/shared/kafkaclient"
	log "github.com/sirupsen/logrus"

	"github.com/gin-gonic/gin"
)

// SchemaChangeApproval represents a pending schema migration requiring user approval.
type SchemaChangeApproval struct {
	ID          string     `json:"id"`
	PipelineID  string     `json:"pipeline_id"`
	ChangeType  string     `json:"change_type"`
	TableName   string     `json:"table_name"`
	DDL         string     `json:"ddl"`
	Reasoning   string     `json:"reasoning"`
	Risks       string     `json:"risks"`
	UserMessage string     `json:"user_message"`
	Status      string     `json:"status"`
	// AutoApplicable is false when the DDL trips the healer's destructive-token
	// guard: approval then only RECORDS the decision — the healer will refuse to
	// run the DDL and the user must apply it manually on the destination. A
	// dropped source table is the exception: approving it drops the destination
	// table, so it is auto-applicable.
	AutoApplicable bool       `json:"auto_applicable"`
	ReviewedBy     *string    `json:"reviewed_by"`
	ReviewedAt     *time.Time `json:"reviewed_at"`
	AppliedAt      *time.Time `json:"applied_at"`
	ErrorMsg       *string    `json:"error_message"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// autoApplicableDDL reports whether the healer would actually auto-apply this
// DDL after approval. MUST stay in lockstep with notAutoAppliedDDL in
// backend-orchestrator/internal/agents/healer/healer.go, which refuses both
// DROP/TRUNCATE and advisory notes even post-approval. Surfacing the same
// predicate here lets the UI say honestly that approving such a row is
// record-only — and gates the dispatch below, so an approval the healer would
// refuse is never published to it.
func autoApplicableDDL(ddl string) bool {
	d := strings.ToLower(strings.TrimSpace(ddl))
	// A drift note ("-- drift: …") is a statement about the source, not DDL for
	// the destination. Every database accepts a bare comment, so dispatching one
	// reported a successful apply that changed nothing.
	if strings.HasPrefix(d, "--") {
		return false
	}
	if strings.Contains(d, " drop ") || strings.HasPrefix(d, "drop ") ||
		strings.Contains(d, " truncate ") || strings.HasPrefix(d, "truncate ") {
		return false
	}
	return true
}

// SchemaDriftPolicy is the per-pipeline drift-detector policy stored under
// pipelines.config->'schema_drift_policy' (JSONB — no dedicated column, same
// pattern as destination_config in pipelines.go). The orchestrator detector
// (backend-orchestrator/internal/agents/executor/schema_drift.go
// parseSchemaDriftPolicy) reads the same shape; keep the field set in
// lockstep. Absent/partial policy = every field defaults TRUE; the global
// RSYNC_SCHEMA_DRIFT_ENABLED env flag still gates the detector as a whole.
type SchemaDriftPolicy struct {
	Enabled      bool `json:"enabled"`
	NotifyOnAdd  bool `json:"notify_on_add"`
	NotifyOnDrop bool `json:"notify_on_drop"`
}

// schemaDriftPolicyFromJSON decodes the raw JSONB value with absent fields
// defaulting to true (a bare {} must not read as all-off), mirroring the
// orchestrator's parseSchemaDriftPolicy fail-open semantics.
func schemaDriftPolicyFromJSON(raw []byte) SchemaDriftPolicy {
	out := SchemaDriftPolicy{Enabled: true, NotifyOnAdd: true, NotifyOnDrop: true}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return out
	}
	var p struct {
		Enabled      *bool `json:"enabled"`
		NotifyOnAdd  *bool `json:"notify_on_add"`
		NotifyOnDrop *bool `json:"notify_on_drop"`
	}
	if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
		return out
	}
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	if p.NotifyOnAdd != nil {
		out.NotifyOnAdd = *p.NotifyOnAdd
	}
	if p.NotifyOnDrop != nil {
		out.NotifyOnDrop = *p.NotifyOnDrop
	}
	return out
}

// schemaDriftDetectorEnabled reports the installation-wide detector switch. The
// orchestrator owns the detector and reads the same env var
// (backend-orchestrator/internal/agents/executor/schema_drift.go
// schemaDriftEnabled); the gateway only mirrors it so the policy card can say
// "detection is off for this installation" instead of showing a per-pipeline
// switch that does nothing. docker-compose.yml passes the identical value to
// both services, and test_orchestrator_feature_flags_reach_compose.py pins that.
// The value comes from config.SchemaDriftEnabled, the one reader of the flag in
// this module, which also gates the rsync.healer.* publish and subscription.
func schemaDriftDetectorEnabled() bool {
	return config.SchemaDriftEnabled()
}

var schemaEvolutionDB *sql.DB
var schemaEvolutionKafka KafkaProducer

func SetSchemaEvolutionDeps(db *sql.DB, kp KafkaProducer) {
	schemaEvolutionDB = db
	schemaEvolutionKafka = kp
}

// ListPipelineSchemaChanges returns pending (and recent) schema change proposals.
func ListPipelineSchemaChanges(c *gin.Context) {
	pipelineID, ok := requireUUIDParam(c, "id", "invalid_pipeline_id", "Invalid pipeline ID format")
	if !ok {
		return
	}
	if _, ok := requirePipelineWorkspaceRole(c, pipelineID, security.WSViewer); !ok {
		return
	}

	if schemaEvolutionDB == nil {
		c.JSON(http.StatusOK, gin.H{"schema_changes": []interface{}{}})
		return
	}

	rows, err := schemaEvolutionDB.QueryContext(c.Request.Context(), `
		SELECT id, pipeline_id, change_type, table_name, ddl,
		       COALESCE(reasoning,''), COALESCE(risks,''), COALESCE(user_message,''),
		       status,
		       reviewed_by, reviewed_at, applied_at, error_message,
		       created_at, updated_at
		FROM schema_change_approvals
		WHERE pipeline_id = $1
		ORDER BY created_at DESC
		LIMIT 50
	`, pipelineID)
	if err != nil {
		log.WithError(err).Error("ListPipelineSchemaChanges: query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	defer rows.Close()

	changes := make([]SchemaChangeApproval, 0)
	for rows.Next() {
		var sc SchemaChangeApproval
		if err := rows.Scan(
			&sc.ID, &sc.PipelineID, &sc.ChangeType, &sc.TableName, &sc.DDL,
			&sc.Reasoning, &sc.Risks, &sc.UserMessage,
			&sc.Status,
			&sc.ReviewedBy, &sc.ReviewedAt, &sc.AppliedAt, &sc.ErrorMsg,
			&sc.CreatedAt, &sc.UpdatedAt,
		); err != nil {
			log.WithError(err).Warn("ListPipelineSchemaChanges: scan error")
			continue
		}
		sc.AutoApplicable = autoApplicableDDL(sc.DDL) || isDropTableChange(sc.ChangeType, sc.DDL)
		changes = append(changes, sc)
	}

	c.JSON(http.StatusOK, gin.H{"schema_changes": changes})
}

// GetPipelineSchemaDriftPolicy returns the pipeline's per-pipeline drift policy,
// resolved to defaults (all true) when unset, plus detector_enabled: whether the
// installation runs the detector at all.
func GetPipelineSchemaDriftPolicy(c *gin.Context) {
	pipelineID, ok := requireUUIDParam(c, "id", "invalid_pipeline_id", "Invalid pipeline ID format")
	if !ok {
		return
	}
	if _, ok := requirePipelineWorkspaceRole(c, pipelineID, security.WSViewer); !ok {
		return
	}

	if schemaEvolutionDB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	var raw sql.NullString
	if err := schemaEvolutionDB.QueryRowContext(c.Request.Context(),
		`SELECT COALESCE(config->'schema_drift_policy', 'null'::jsonb)::text FROM pipelines WHERE id = $1::uuid`,
		pipelineID,
	).Scan(&raw); err != nil {
		log.WithError(err).Error("GetPipelineSchemaDriftPolicy: query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"schema_drift_policy": schemaDriftPolicyFromJSON([]byte(raw.String)),
		"detector_enabled":    schemaDriftDetectorEnabled(),
	})
}

// UpdatePipelineSchemaDriftPolicy PUTs the pipeline's drift policy. Absent body
// fields default to true (same semantics as reads) and the RESOLVED policy is
// persisted, so the stored JSONB is always complete and unambiguous. Written
// into pipelines.config via jsonb_set — the persistDestinationConfig pattern.
func UpdatePipelineSchemaDriftPolicy(c *gin.Context) {
	pipelineID, ok := requireUUIDParam(c, "id", "invalid_pipeline_id", "Invalid pipeline ID format")
	if !ok {
		return
	}
	if _, ok := requirePipelineWorkspaceRole(c, pipelineID, security.WSMember); !ok {
		return
	}

	if schemaEvolutionDB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	var body struct {
		Enabled      *bool `json:"enabled"`
		NotifyOnAdd  *bool `json:"notify_on_add"`
		NotifyOnDrop *bool `json:"notify_on_drop"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "Body must be JSON: {enabled, notify_on_add, notify_on_drop} (booleans, all optional)"})
		return
	}
	policy := SchemaDriftPolicy{Enabled: true, NotifyOnAdd: true, NotifyOnDrop: true}
	if body.Enabled != nil {
		policy.Enabled = *body.Enabled
	}
	if body.NotifyOnAdd != nil {
		policy.NotifyOnAdd = *body.NotifyOnAdd
	}
	if body.NotifyOnDrop != nil {
		policy.NotifyOnDrop = *body.NotifyOnDrop
	}

	raw, err := json.Marshal(policy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode policy"})
		return
	}
	if _, err := schemaEvolutionDB.ExecContext(c.Request.Context(), `
		UPDATE pipelines
		SET config = jsonb_set(COALESCE(config, '{}'::jsonb), '{schema_drift_policy}', $2::jsonb, true),
		    updated_at = NOW()
		WHERE id = $1::uuid
	`, pipelineID, string(raw)); err != nil {
		log.WithError(err).Error("UpdatePipelineSchemaDriftPolicy: update failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"schema_drift_policy": policy, "detector_enabled": schemaDriftDetectorEnabled()})
}

// ApproveSchemaChange marks a schema change as approved and triggers DDL application
// by publishing to the healer's approved-changes topic. Approving a dropped source
// table first takes the table out of the pipeline, then asks the healer to drop the
// destination table.
func ApproveSchemaChange(c *gin.Context) {
	pipelineID, ok := requireUUIDParam(c, "id", "invalid_pipeline_id", "Invalid pipeline ID format")
	if !ok {
		return
	}
	if _, ok := requirePipelineWorkspaceRole(c, pipelineID, security.WSMember); !ok {
		return
	}
	changeID := c.Param("changeId")

	if schemaEvolutionDB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	userEmail := ""
	if u, ok := c.Get("user_email"); ok {
		userEmail, _ = u.(string)
	}

	d, err := approveSchemaChangeCore(c.Request.Context(), schemaEvolutionDB, schemaEvolutionKafka, pipelineID, changeID, userEmail)
	if err != nil {
		respondSchemaDecisionError(c, "ApproveSchemaChange", err)
		return
	}
	if !d.Actioned {
		c.JSON(http.StatusNotFound, gin.H{"error": "schema change not found or already actioned"})
		return
	}

	// auto_applicable echoes what approval actually did, so the client's message
	// comes from the server that decided rather than from a second copy of the
	// predicate in the browser.
	c.JSON(http.StatusOK, gin.H{
		"status":          "approved",
		"auto_applicable": d.Dispatched,
		"drop_table":      d.DropTable,
		"table_removed":   d.TableRemoved,
	})
}

// schemaDecision is what approving or rejecting one schema change did.
type schemaDecision struct {
	Actioned     bool // the row was pending and is now decided; false = already actioned or missing
	Dispatched   bool // approve only: the healer was asked to apply the change
	DropTable    bool // the change is a dropped source table
	TableRemoved bool // the table was taken out of the pipeline's config.selected_tables
}

// errLastSelectedTable: the dropped table is the only one the pipeline syncs, so
// removing it would leave a pipeline with nothing to sync. The decision is not
// saved; the user adds another table or deletes the pipeline.
var errLastSelectedTable = errors.New("the dropped table is the pipeline's only selected table")

// tableRemovalError is a failure to take a dropped table out of the pipeline. The
// decision was not saved (the row is back to pending); public is what the user is
// told, err is the cause, which is logged scrubbed and never sent to the client.
type tableRemovalError struct {
	status int
	public string
	err    error
}

func (e *tableRemovalError) Error() string { return e.public + ": " + e.err.Error() }
func (e *tableRemovalError) Unwrap() error { return e.err }

// respondSchemaDecisionError maps an approve/reject failure to its response. A
// failure to remove a dropped table says why and that nothing was saved; anything
// else is a database error, as before.
func respondSchemaDecisionError(c *gin.Context, op string, err error) {
	var removal *tableRemovalError
	switch {
	case errors.Is(err, errLastSelectedTable):
		respondError(c, http.StatusConflict, "last_selected_table",
			"This is the only table the pipeline syncs, so it cannot be removed from the pipeline. "+
				"Add another table to the pipeline, or delete the pipeline. The decision was not saved.", err)
	case errors.As(err, &removal):
		respondError(c, removal.status, "table_removal_failed", removal.public, removal.err)
	default:
		log.WithError(err).Error(op + ": update failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
	}
}

// isDropTableChange reports whether a schema change is a dropped source table.
// MUST stay in lockstep with isDropTableChange in
// backend-orchestrator/internal/agents/healer/healer.go, which routes the same
// rows to a decision and, once approved, drops the destination table.
func isDropTableChange(changeType, ddl string) bool {
	if strings.EqualFold(strings.TrimSpace(changeType), "drop_table") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ddl)), "drop table")
}

// withoutSelectedTable returns the selection without table. Names match exactly,
// or by bare name when one side carries no schema ("orders" vs "public.orders") —
// the executor's withoutTablesAwaitingDrop rule
// (backend-orchestrator/internal/agents/executor/schema_drift.go), so the table
// the executor held back is the one removed here. removed=false: not selected.
func withoutSelectedTable(selected []string, table string) (kept []string, removed bool) {
	key := strings.TrimSpace(table)
	kept = make([]string, 0, len(selected))
	for _, entry := range selected {
		e := strings.TrimSpace(entry)
		if key != "" && (e == key ||
			(!strings.Contains(e, ".") && e == bareName(key)) ||
			(!strings.Contains(key, ".") && bareName(e) == key)) {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	return kept, removed
}

// bareName is a table name without its schema: the part after the last dot.
func bareName(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.LastIndex(n, "."); i >= 0 {
		return strings.TrimSpace(n[i+1:])
	}
	return n
}

// tableRemovalLocks serialises table removals per pipeline in this process.
// Deciding several dropped tables in quick succession is the normal case (a
// source that drops three tables files three rows), and each decision rewrites
// the whole selection; unserialised, the second would restore the table the
// first removed. The compare-and-swap in removeTableFromPipeline covers the
// cross-process case by failing instead.
var tableRemovalLocks sync.Map

func lockPipelineTables(pipelineID string) func() {
	v, _ := tableRemovalLocks.LoadOrStore(pipelineID, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// removeTableFromPipeline takes a dropped source table out of the pipeline's
// config.selected_tables — the selection batch runs, CDC and the pre-flight all
// read — so the pipeline stops syncing a table that no longer exists. Both
// decisions on a dropped table do this; only approval also drops the destination
// table. A table that is not selected is already out: nil, nothing written.
//
// CDC pipelines follow UpdatePipelineCDCTables: the new list goes to the
// orchestrator (which rewrites the connector's include list) before it is saved,
// because saving a list the connector never received would desync the two. A
// pipeline with no connector yet only saves it. Refreshing the streaming-only list
// and restarting the sink are best-effort, as they are there.
func removeTableFromPipeline(ctx context.Context, database *sql.DB, pipelineID, table string) error {
	unlock := lockPipelineTables(pipelineID)
	defer unlock()

	var raw string
	if err := database.QueryRowContext(ctx,
		`SELECT COALESCE(config->'selected_tables', '[]'::jsonb)::text FROM pipelines WHERE id = $1::uuid`,
		pipelineID,
	).Scan(&raw); err != nil {
		return &tableRemovalError{http.StatusInternalServerError,
			"Could not read the pipeline's table selection, so the decision was not saved. Try again.", err}
	}
	var selected []string
	if err := json.Unmarshal([]byte(raw), &selected); err != nil {
		return &tableRemovalError{http.StatusInternalServerError,
			"The pipeline's table selection is not a list of table names, so the decision was not saved.", err}
	}
	kept, removed := withoutSelectedTable(selected, table)
	if !removed {
		return nil
	}
	if len(kept) == 0 {
		return errLastSelectedTable
	}

	var isCDC bool
	if err := database.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pipelines p WHERE p.id = $1::uuid AND `+pipelineRowIsCDCSQL+`)`,
		pipelineID,
	).Scan(&isCDC); err != nil {
		return &tableRemovalError{http.StatusInternalServerError,
			"Could not read the pipeline's sync mode, so the decision was not saved. Try again.", err}
	}
	if isCDC {
		_, connErr := findDebeziumConnectorName(pipelineID)
		switch {
		case connErr == nil:
			status, _, perr := pushCDCTableList(ctx, pipelineID, kept)
			if perr != nil {
				return &tableRemovalError{http.StatusBadGateway,
					"The orchestrator is unreachable, so the table could not be removed from the CDC stream. The decision was not saved; try again.", perr}
			}
			if status < 200 || status >= 300 {
				return &tableRemovalError{http.StatusBadGateway,
					fmt.Sprintf("The orchestrator refused the new CDC table list (status %d), so the table was not removed. The decision was not saved.", status),
					fmt.Errorf("orchestrator status %d", status)}
			}
		case errors.Is(connErr, errCDCConnectorNotProvisioned):
			// No connector yet: the saved selection is all there is to change.
		default:
			return &tableRemovalError{http.StatusBadGateway,
				"Kafka Connect is unreachable, so the table could not be removed from the CDC stream. The decision was not saved; try again.", connErr}
		}
	}

	keptJSON, err := json.Marshal(kept)
	if err != nil {
		return &tableRemovalError{http.StatusInternalServerError,
			"Could not encode the pipeline's new table selection, so the decision was not saved.", err}
	}
	// Compare-and-swap on the selection read above: a table edit, or a decision
	// made through another gateway process, in between is refused rather than
	// overwritten. The user retries against the list as it is now.
	res, err := database.ExecContext(ctx, `
		UPDATE pipelines
		SET config = jsonb_set(COALESCE(config, '{}'::jsonb), '{selected_tables}', $1::jsonb, true),
		    updated_at = NOW()
		WHERE id = $2::uuid
		  AND COALESCE(config->'selected_tables', '[]'::jsonb) = $3::jsonb
	`, string(keptJSON), pipelineID, raw)
	if err != nil {
		return &tableRemovalError{http.StatusInternalServerError,
			"Could not save the pipeline's new table selection, so the decision was not saved. Try again.", err}
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &tableRemovalError{http.StatusConflict,
			"The pipeline's table selection changed while this decision was being saved, so the decision was not saved. Try again.",
			errors.New("selected_tables changed between read and write")}
	}

	if isCDC {
		if err := updateCDCStreamingOnlyTables(database, pipelineID, nil, []string{table}); err != nil {
			log.WithError(err).WithField("pipeline_id", pipelineID).Warn("removeTableFromPipeline: cdc_streaming_only_tables not updated (ignored)")
		}
		if r := restartCDCSink(ctx, pipelineID); r["success"] != true {
			log.WithField("pipeline_id", pipelineID).Warn("removeTableFromPipeline: sink restart failed (ignored); it drops the table's topic on its next restart")
		}
	}
	log.WithFields(log.Fields{"pipeline_id": pipelineID, "table": table, "cdc": isCDC}).
		Info("dropped source table removed from the pipeline")
	return nil
}

// reopenSchemaChange puts a row this request just decided back to pending, when
// the side effect the decision depends on failed: a decision that did not happen
// must not read as made. Guarded on the status this request set, so it never
// reopens a row something else has moved on since.
func reopenSchemaChange(ctx context.Context, database *sql.DB, pipelineID, changeID, decidedStatus string) {
	if _, err := database.ExecContext(ctx, `
		UPDATE schema_change_approvals
		SET status = 'pending', reviewed_by = NULL, reviewed_at = NULL, updated_at = NOW()
		WHERE id = $1 AND pipeline_id = $2 AND status = $3
	`, changeID, pipelineID, decidedStatus); err != nil {
		log.WithError(err).WithFields(log.Fields{"pipeline_id": pipelineID, "change_id": changeID}).
			Error("reopenSchemaChange: could not put the decision back to pending")
	}
}

// decidedSchemaChange reads the row a decision just claimed. What the decision
// does depends on the change type, so a row that cannot be read is reopened and
// reported as an error rather than decided blind.
func decidedSchemaChange(ctx context.Context, database *sql.DB, pipelineID, changeID, decidedStatus string) (SchemaChangeApproval, error) {
	var sc SchemaChangeApproval
	if err := database.QueryRowContext(ctx, `
		SELECT id, pipeline_id, change_type, table_name, ddl FROM schema_change_approvals WHERE id = $1
	`, changeID).Scan(&sc.ID, &sc.PipelineID, &sc.ChangeType, &sc.TableName, &sc.DDL); err != nil {
		reopenSchemaChange(ctx, database, pipelineID, changeID, decidedStatus)
		return sc, fmt.Errorf("decided schema change unreadable: %w", err)
	}
	return sc, nil
}

// approveSchemaChangeCore performs the pending-guarded approval UPDATE and, for
// DDL the healer can actually run, republishes to the healer for application. It
// is independent of gin/HTTP so BOTH the UI handler and the Slack receiver share
// ONE approval path.
//
// A dropped source table is taken out of the pipeline first, then dispatched: the
// healer drops the destination table. If the table cannot be removed the row is
// reopened and the error returned — the decision is not saved.
//
// Other DDL that trips the destructive-token guard is NOT dispatched. The
// healer's applyMigration refuses such DDL post-approval, so dispatching it
// produced a guaranteed failure: the row we had just set to `approved` was flipped
// to `failed` with "refusing to auto-apply potentially destructive ddl" and the
// user got a red alert — for doing exactly what the approval card told them to do
// ("approving records your decision; run the DDL manually"). Not publishing is
// what makes that promise true: the decision stays recorded as `approved` and
// nothing pretends to apply it.
//
// Actioned=false means the row was not `pending` (already actioned or missing) —
// callers surface that as a soft 404, never an error. Dispatched reports whether
// the healer was asked to act; false means the user must run the DDL manually.
// The kafka publish itself stays a best-effort side effect, exactly as before.
func approveSchemaChangeCore(ctx context.Context, database *sql.DB, kafka KafkaProducer, pipelineID, changeID, reviewer string) (schemaDecision, error) {
	var d schemaDecision
	res, err := database.ExecContext(ctx, `
		UPDATE schema_change_approvals
		SET status = 'approved', reviewed_by = $1, reviewed_at = NOW(), updated_at = NOW()
		WHERE id = $2 AND pipeline_id = $3 AND status = 'pending'
	`, reviewer, changeID, pipelineID)
	if err != nil {
		return d, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return d, nil
	}
	// The row has left `pending`, so the bell may now be pointing at an empty
	// queue. Deferred so every remaining exit path clears it identically; a row
	// reopened below is pending again, so the badge is left alone.
	defer func() { _, _ = clearDriftNotificationsIfQueueEmpty(ctx, database, pipelineID) }()

	sc, err := decidedSchemaChange(ctx, database, pipelineID, changeID, "approved")
	if err != nil {
		return d, err
	}
	d.Actioned = true

	if isDropTableChange(sc.ChangeType, sc.DDL) {
		d.DropTable = true
		if err := removeTableFromPipeline(ctx, database, pipelineID, sc.TableName); err != nil {
			reopenSchemaChange(ctx, database, pipelineID, changeID, "approved")
			return schemaDecision{}, err
		}
		d.TableRemoved = true
		d.Dispatched = dispatchApprovedChange(kafka, sc, reviewer)
		return d, nil
	}

	if !autoApplicableDDL(sc.DDL) {
		log.WithFields(log.Fields{
			"pipeline_id": sc.PipelineID,
			"change_id":   sc.ID,
			"change_type": sc.ChangeType,
		}).Info("approveSchemaChangeCore: DDL is not auto-applied (destructive, or an advisory drift note) — decision recorded, not dispatched to healer")
		return d, nil
	}
	d.Dispatched = dispatchApprovedChange(kafka, sc, reviewer)
	return d, nil
}

// dispatchApprovedChange asks the healer to apply an approved change. false means
// it was not asked, and the user must act on the destination by hand.
func dispatchApprovedChange(kafka KafkaProducer, sc SchemaChangeApproval, reviewer string) bool {
	// rsync.healer.approved-changes exists only while schema drift is on: the
	// orchestrator creates it, and starts the healer consumer that applies what
	// arrives on it, only when RSYNC_SCHEMA_DRIFT_ENABLED=true. With the flag off
	// there is nobody to apply the DDL, and a publish would auto-create the topic
	// (the UnifiedProducer allows it) on an installation that does not provision
	// it. Record the decision and report it as not dispatched, so the user is told
	// to run the DDL by hand rather than that it was applied.
	if !config.SchemaDriftEnabled() {
		log.WithFields(log.Fields{
			"pipeline_id": sc.PipelineID,
			"change_id":   sc.ID,
		}).Info("approveSchemaChangeCore: schema drift is off for this installation (RSYNC_SCHEMA_DRIFT_ENABLED) — decision recorded, not dispatched to healer")
		return false
	}

	// No producer, or a failed publish, is the same outcome as the record-only
	// branches above: the healer never sees the change, so report it as not
	// dispatched and the user is told to run the DDL. This used to discard the
	// publish error and fall through to `dispatched`, telling the user a DDL
	// was being applied that nobody had received.
	if kafka == nil {
		log.WithFields(log.Fields{
			"pipeline_id": sc.PipelineID,
			"change_id":   sc.ID,
		}).Warn("approveSchemaChangeCore: no Kafka producer — decision recorded, not dispatched to healer")
		return false
	}
	// Qualified here because api-gateway's UnifiedProducer has no chokepoint of
	// its own -- unlike the orchestrator's kafka.Manager, it hands the topic to
	// the client verbatim, so the call site is the only place the namespace can
	// be applied. The healer on the other end consumes through that Manager and
	// therefore reads the QUALIFIED name (healer.go:168 -> manager.go:920). Left
	// bare, a deployment with a custom KAFKA_TOPIC_PREFIX publishes every
	// approval to a topic the healer never reads: the UI records the approval,
	// the user is told it was applied, and the DDL is never executed.
	if err := kafka.SendPipelineRequest(kafkaclient.Topic("rsync.healer.approved-changes"), sc.ID, map[string]interface{}{
		"event_type":  "schema_change_approved",
		"approval_id": sc.ID,
		"pipeline_id": sc.PipelineID,
		"change_type": sc.ChangeType,
		"table_name":  sc.TableName,
		"ddl":         sc.DDL,
		"approved_by": reviewer,
		"approved_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"pipeline_id": sc.PipelineID,
			"change_id":   sc.ID,
		}).Error("approveSchemaChangeCore: publish to healer failed — decision recorded, not dispatched")
		return false
	}
	return true
}

// clearDriftNotificationsIfQueueEmpty marks the pipeline's unread schema-drift
// notifications read once nothing is left to review.
//
// The bell badge answers "is there something waiting for me?". A drift
// notification is only marked read when the user reaches the approval page
// *through the bell row*, so approving from the page directly (or from Slack)
// left the badge counting work that was already done — and clicking it landed on
// "You're all caught up", the empty state. Two surfaces answering the same
// question and disagreeing.
//
// Scope is deliberate on both axes:
//
//   - Only when zero `pending` rows remain. Approving 1 of 3 leaves the
//     notification honest, because there is still something to review.
//   - Only SCHEMA_DRIFT_DETECTED rows for this pipeline. A failure notification
//     on the same pipeline is a different question and must survive.
//
// It clears the row for EVERY recipient, not just the reviewer: the notification
// is about a shared resource, and once the queue is empty it is stale for
// everyone. Leaving a teammate's copy unread would just send them to the same
// empty page.
//
// Best-effort by design — the approval is already committed, so a failure here
// is logged and swallowed rather than turned into an error the user sees. It
// still RETURNS what it did (rows cleared, error) purely so a test can see it:
// the callers discard both, but "swallowed the error" and "never ran the
// statement" are indistinguishable from the outside otherwise, and that is
// exactly the difference the pending>0 guard turns on.
func clearDriftNotificationsIfQueueEmpty(ctx context.Context, database *sql.DB, pipelineID string) (int64, error) {
	if database == nil {
		return 0, nil
	}
	var pending int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM schema_change_approvals
		WHERE pipeline_id = $1 AND status = 'pending'
	`, pipelineID).Scan(&pending); err != nil {
		log.WithError(err).Warn("clearDriftNotificationsIfQueueEmpty: pending count failed; leaving the badge alone")
		return 0, err
	}
	if pending > 0 {
		return 0, nil
	}
	res, err := database.ExecContext(ctx, `
		UPDATE pipeline_notifications
		SET read_at = NOW()
		WHERE pipeline_id = $1::uuid
		  AND read_at IS NULL
		  AND metadata->>'error_code' = 'SCHEMA_DRIFT_DETECTED'
	`, pipelineID)
	if err != nil {
		log.WithError(err).Warn("clearDriftNotificationsIfQueueEmpty: mark-read failed; leaving the badge alone")
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		log.WithFields(log.Fields{"pipeline_id": pipelineID, "cleared": n}).
			Info("schema-change queue emptied; cleared stale drift notifications")
	}
	return n, nil
}

// RejectSchemaChange marks a schema change as rejected. Rejecting a dropped
// source table still takes the table out of the pipeline; the destination table
// is kept.
func RejectSchemaChange(c *gin.Context) {
	pipelineID, ok := requireUUIDParam(c, "id", "invalid_pipeline_id", "Invalid pipeline ID format")
	if !ok {
		return
	}
	if _, ok := requirePipelineWorkspaceRole(c, pipelineID, security.WSMember); !ok {
		return
	}
	changeID := c.Param("changeId")

	if schemaEvolutionDB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	userEmail := ""
	if u, ok := c.Get("user_email"); ok {
		userEmail, _ = u.(string)
	}

	d, err := rejectSchemaChangeCore(c.Request.Context(), schemaEvolutionDB, pipelineID, changeID, userEmail)
	if err != nil {
		respondSchemaDecisionError(c, "RejectSchemaChange", err)
		return
	}
	if !d.Actioned {
		c.JSON(http.StatusNotFound, gin.H{"error": "schema change not found or already actioned"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "rejected", "drop_table": d.DropTable, "table_removed": d.TableRemoved})
}

// rejectSchemaChangeCore performs the pending-guarded rejection UPDATE,
// independent of gin/HTTP so the UI handler and the Slack receiver share it.
// Actioned=false means the row was not `pending`. Rejection never publishes to
// the healer — that is approve-only. A rejected dropped table still leaves the
// pipeline (the source no longer has it); if it cannot be removed the row is
// reopened and the error returned, exactly as on approve.
func rejectSchemaChangeCore(ctx context.Context, database *sql.DB, pipelineID, changeID, reviewer string) (schemaDecision, error) {
	var d schemaDecision
	res, err := database.ExecContext(ctx, `
		UPDATE schema_change_approvals
		SET status = 'rejected', reviewed_by = $1, reviewed_at = NOW(), updated_at = NOW()
		WHERE id = $2 AND pipeline_id = $3 AND status = 'pending'
	`, reviewer, changeID, pipelineID)
	if err != nil {
		return d, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return d, nil
	}
	sc, err := decidedSchemaChange(ctx, database, pipelineID, changeID, "rejected")
	if err != nil {
		return d, err
	}
	d.Actioned = true
	if isDropTableChange(sc.ChangeType, sc.DDL) {
		d.DropTable = true
		if err := removeTableFromPipeline(ctx, database, pipelineID, sc.TableName); err != nil {
			reopenSchemaChange(ctx, database, pipelineID, changeID, "rejected")
			return schemaDecision{}, err
		}
		d.TableRemoved = true
	}
	// A rejection empties the queue just as an approval does — the user has
	// decided, so the badge must stop asking.
	_, _ = clearDriftNotificationsIfQueueEmpty(ctx, database, pipelineID)
	return d, nil
}
