package healer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IBM/sarama"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/rsync-ai/backend-orchestrator/internal/cdc"
	"github.com/rsync-ai/backend-orchestrator/internal/config"
	"github.com/rsync-ai/backend-orchestrator/internal/connections"
	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
	appmetrics "github.com/rsync-ai/backend-orchestrator/internal/metrics"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

var healerTracer = otel.Tracer("healer-agent")

const (
	HealerTopic = "rsync.healer.schema-changes"

	// ResultsTopic carries one HealingResult per user-approved change the healer
	// executes (publishHealingResult); the api-gateway notifier turns an applied
	// one into the "Schema change applied" confirmation. A change the healer
	// handles on its own is announced by its own drift notification instead
	// (notifyDrift), so it is not published here: both would reach the inbox
	// under different codes and the user would read every change twice. It
	// belongs to the schema-drift path, so like HealerTopic and
	// ApprovedChangeTopic it is produced to, consumed and provisioned
	// (kafka.EnsurePlatformTopics) only when RSYNC_SCHEMA_DRIFT_ENABLED=true.
	//
	// NotifyTopic is rsync.notifications, the user-notification topic the
	// api-gateway notifier always consumes (notifyUserStructured). Not gated.
	ResultsTopic = "rsync.healer.results"
	NotifyTopic  = "rsync.notifications"
)

// connectorExecutor is the slice of *mcp.Client the healer uses: one call to a
// destination connector tool. An interface so tests can stand in for the connector.
type connectorExecutor interface {
	ExecuteWithContext(ctx context.Context, req mcp.ExecuteRequest) (*mcp.ExecuteResponse, error)
}

// Agent represents the Healer agent for self-healing pipelines
type Agent struct {
	kafkaManager *kafka.Manager
	db           *sql.DB
	ctx          context.Context
	cancel       context.CancelFunc
	// mcpClient invokes destination connector tools: ensure_table, so a column
	// change lands via the SAME path the batch write uses instead of running
	// source-shaped DDL directly against the dest (ensureColumnViaConnector), and
	// drop_table for an approved table drop (dropDestinationTable).
	mcpClient connectorExecutor
	// loadDestination resolves a pipeline's destination connector type and
	// config. Nil means the pipelines and connections tables, which is the
	// production path; tests set it so they need no connection store.
	loadDestination func(ctx context.Context, pipelineID string) (connectorType string, cfg map[string]string, err error)
	// notifications, when set, receives each user notification instead of Kafka.
	// Tests only.
	notifications func(pipelineID string, se *diagnose.StructuredError)
}

// SchemaChangeEvent represents a schema change event from kafka-mcp-sink
type SchemaChangeEvent struct {
	EventType    string                 `json:"event_type"`
	PipelineID   string                 `json:"pipeline_id"`
	Timestamp    string                 `json:"timestamp"`
	SchemaChange SchemaChange           `json:"schema_change"`
	Context      map[string]interface{} `json:"context"`
	ActionNeeded bool                   `json:"action_needed"`
}

// SchemaChange represents details of a schema change
type SchemaChange struct {
	ChangeType string `json:"change_type"`
	Table      string `json:"table"`
	Database   string `json:"database"`
	SchemaName string `json:"schema_name"`
	ColumnName string `json:"column_name"`
	ColumnType string `json:"column_type"`
	DDL        string `json:"ddl"`
	RiskLevel  string `json:"risk_level"`
	DetectedAt string `json:"detected_at"`
	Applied    bool   `json:"applied"`
	Error      string `json:"error"`
}

// HealingResult represents the result of a healing operation
type HealingResult struct {
	PipelineID   string `json:"pipeline_id"`
	ChangeType   string `json:"change_type"`
	Table        string `json:"table"`
	Status       string `json:"status"` // "applied", "skipped", "pending_approval", "failed"
	Reason       string `json:"reason"`
	DDLApplied   string `json:"ddl_applied,omitempty"`
	ErrorMessage string `json:"error,omitempty"`
}

// NewAgent creates a new Healer agent. toolsDir is the MCP connectors root used
// to build the destination-connector client for ensure_table and drop_table
// (empty is tolerated — it resolves to the default tools dir).
func NewAgent(kafkaManager *kafka.Manager, db *sql.DB, toolsDir string) *Agent {
	ctx, cancel := context.WithCancel(context.Background())

	return &Agent{
		kafkaManager: kafkaManager,
		db:           db,
		ctx:          ctx,
		cancel:       cancel,
		mcpClient:    mcp.NewClient(mcp.NewServerManager(toolsDir)),
	}
}

// ApprovedChangeTopic is published by api-gateway when a user approves a pending DDL.
const ApprovedChangeTopic = "rsync.healer.approved-changes"

// schemaSubscription is a topic→handler binding for the schema-drift path.
// Required=true means a failed subscription is fatal (return error); Required=false
// means best-effort (log + continue).
type schemaSubscription struct {
	Topic    string
	Handler  kafka.ConsumeHandlerWithContext
	Required bool
}

// schemaDriftSubscriptions is the SINGLE source of truth for which topics the
// drift-detect → approve path consumes: exactly HealerTopic + ApprovedChangeTopic
// and NEVER the executor/planner DLQs. Centralised here so a broker-free unit
// test can assert the DLQ topics never creep into this path (subscribing them
// would double-react to execution failures already handled by the executor
// worker's executeWithHealer + suggestRecoveryAction and, asynchronously, by the
// canonical diagnose→heal HealWorker).
func (a *Agent) schemaDriftSubscriptions() []schemaSubscription {
	return []schemaSubscription{
		{Topic: HealerTopic, Handler: a.handleSchemaChangeMessage, Required: true},
		{Topic: ApprovedChangeTopic, Handler: a.handleApprovedChange, Required: false},
	}
}

// StartSchemaOnly begins consuming ONLY the two schema-drift topics
// (HealerTopic + ApprovedChangeTopic) — the proactive drift-detect → approve
// path. It deliberately omits any executor/planner DLQ subscriptions: reactive
// DLQ consumers would double-react to execution failures already handled
// synchronously by the executor worker's executeWithHealer + suggestRecoveryAction
// and asynchronously by the canonical diagnose→heal HealWorker. (An earlier full
// Start() that also subscribed those DLQs and ran a second, divergent LLM error
// classifier was removed when the healer consolidated on that canonical path.)
// handleSchemaChangeMessage has no synchronous twin, so subscribing the two
// schema topics in isolation is collision-free.
//
// Gated by config.SchemaDriftEnabled HERE as well as at the call site
// (workers ExecutorWorker.Start): a consumer-group subscription auto-creates
// its topic, so a caller that skipped the check would mint the rsync.healer.*
// topics that kafka.EnsurePlatformTopics deliberately leaves out when the flag
// is off. Off → no subscription → no topic.
func (a *Agent) StartSchemaOnly() error {
	if !config.SchemaDriftEnabled() {
		log.Info("[HealerAgent] RSYNC_SCHEMA_DRIFT_ENABLED is not true; schema-drift consumers not started")
		return nil
	}
	log.Info("[HealerAgent] Starting schema-drift consumers (drift-detect → approve)...")

	for _, s := range a.schemaDriftSubscriptions() {
		if err := a.kafkaManager.ConsumeWithContext(s.Topic, s.Handler); err != nil {
			if s.Required {
				return fmt.Errorf("failed to start consumer for %s: %w", s.Topic, err)
			}
			log.Warnf("[HealerAgent] Could not start consumer for %s: %v", s.Topic, err)
			continue
		}
		log.Info("[HealerAgent] Listening on ", s.Topic)
	}

	return nil
}

// handleApprovedChange processes a user-approved schema change and applies the DDL.
func (a *Agent) handleApprovedChange(ctx context.Context, msg *sarama.ConsumerMessage) error {
	_, span := healerTracer.Start(ctx, "handle_approved_change")
	defer span.End()

	var payload map[string]interface{}
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Errorf("[HealerAgent] Failed to unmarshal approved change: %v", err)
		return err
	}

	approvalID, _ := payload["approval_id"].(string)
	pipelineID, _ := payload["pipeline_id"].(string)
	ddl, _ := payload["ddl"].(string)
	changeType, _ := payload["change_type"].(string)
	tableName, _ := payload["table_name"].(string)

	span.SetAttributes(
		attribute.String("pipeline_id", pipelineID),
		attribute.String("approval_id", approvalID),
	)

	// A dropped source table is the one change that waits for a person, and approving
	// it is the one destructive action the healer takes: it drops the destination table.
	// It is run through the destination connector, never as the stored statement.
	if isDropTableChange(changeType, ddl) {
		a.dropApprovedTable(ctx, approvalID, pipelineID, changeType, tableName, ddl)
		return nil
	}

	// Any other destructive DDL is never applied, approved or not — applyMigration would
	// refuse it a few lines below and we would then mark the user's approval
	// `failed` and alarm them, for approving a change the UI told them they would
	// have to run by hand. api-gateway no longer dispatches these; this is the
	// backstop for messages already on the topic and for any other producer.
	// Leave the row `approved` (decision recorded) and say so, quietly.
	if notAutoAppliedDDL(ddl) {
		reason := "Approved, but destructive DDL is never auto-applied — run it manually on the destination"
		if isAdvisoryDDL(ddl) {
			reason = "Approved — this drift is a notice, not a statement; there is nothing to apply to the destination"
		}
		log.Infof("[HealerAgent] Approved DDL is not auto-applied by design (approval %s, pipeline %s): %s",
			approvalID, pipelineID, ddl)
		// recordHealingResult writes through a.db without a nil check, matching the
		// a.db != nil guards used elsewhere in this handler.
		if a.db != nil {
			a.recordHealingResult(ctx, &HealingResult{
				PipelineID: pipelineID,
				ChangeType: changeType,
				Table:      tableName,
				Status:     "skipped",
				Reason:     reason,
				DDLApplied: "",
			})
		}
		return nil
	}

	log.Infof("[HealerAgent] Applying user-approved DDL for pipeline %s (approval %s): %s", pipelineID, approvalID, ddl)

	if err := a.applyMigration(ctx, pipelineID, ddl); err != nil {
		log.Errorf("[HealerAgent] Failed to apply approved DDL (approval %s): %v", approvalID, err)
		if a.db != nil && approvalID != "" {
			_, _ = a.db.ExecContext(ctx, `
				UPDATE schema_change_approvals
				SET status = 'failed', error_message = $1, updated_at = NOW()
				WHERE id = $2
			`, err.Error(), approvalID)
		}
		a.notifyUser(pipelineID, fmt.Sprintf("⚠️ Approved DDL failed to apply: %s", err.Error()))
		return nil
	}

	log.Infof("[HealerAgent] ✅ Approved DDL applied successfully (approval %s)", approvalID)

	if a.db != nil && approvalID != "" {
		_, _ = a.db.ExecContext(ctx, `
			UPDATE schema_change_approvals
			SET status = 'applied', applied_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, approvalID)
	}

	result := &HealingResult{
		PipelineID: pipelineID,
		ChangeType: changeType,
		Table:      tableName,
		Status:     "applied",
		Reason:     "User approved DDL migration",
		DDLApplied: ddl,
	}
	a.recordHealingResult(ctx, result)
	a.publishHealingResult(result)

	return nil
}

// dropApprovedTable drops the destination table of a source table the user approved
// dropping. The api-gateway has already taken the table out of the pipeline. The
// approval row is checked here, not trusted from the message: nothing is dropped
// unless schema_change_approvals says a person approved this exact row, so no other
// producer on the topic can make the healer drop a table.
func (a *Agent) dropApprovedTable(ctx context.Context, approvalID, pipelineID, changeType, tableName, ddl string) {
	if a.db == nil || strings.TrimSpace(approvalID) == "" {
		log.Warnf("[HealerAgent] Table drop for pipeline %s has no approval row to check; nothing dropped", pipelineID)
		return
	}
	var approvedTable string
	err := a.db.QueryRowContext(ctx, `
		SELECT table_name FROM schema_change_approvals
		WHERE id = $1 AND pipeline_id = $2 AND status = 'approved'
	`, approvalID, pipelineID).Scan(&approvedTable)
	if err != nil {
		log.Warnf("[HealerAgent] Table drop %s for pipeline %s is not an approved row (%v); nothing dropped",
			approvalID, pipelineID, err)
		return
	}

	table := bareTable(approvedTable)
	if fields := strings.Fields(strings.TrimRight(strings.TrimSpace(ddl), ";")); table == "" && len(fields) >= 3 {
		table = bareTable(fields[len(fields)-1]) // DROP TABLE [IF EXISTS] <t>
	}

	dropped, dropErr := a.dropDestinationTable(ctx, pipelineID, table)
	if dropErr != nil {
		log.Errorf("[HealerAgent] Approved table drop failed (approval %s): %v", approvalID, dropErr)
		_, _ = a.db.ExecContext(ctx, `
			UPDATE schema_change_approvals
			SET status = 'failed', error_message = $1, updated_at = NOW()
			WHERE id = $2
		`, dropErr.Error(), approvalID)
		a.notifyUser(pipelineID, fmt.Sprintf("⚠️ Approved table drop failed: %s", dropErr.Error()))
		return
	}

	_, _ = a.db.ExecContext(ctx, `
		UPDATE schema_change_approvals
		SET status = 'applied', applied_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, approvalID)

	reason := "User approved: the destination table was dropped and the table was removed from this pipeline"
	if !dropped {
		reason = "User approved: the destination table was already gone, and the table was removed from this pipeline"
	}
	log.Infof("[HealerAgent] ✅ Approved table drop done (approval %s, table %s, dropped=%v)", approvalID, table, dropped)
	result := &HealingResult{
		PipelineID: pipelineID,
		ChangeType: changeType,
		Table:      tableName,
		Status:     "applied",
		Reason:     reason,
		DDLApplied: ddl,
	}
	a.recordHealingResult(ctx, result)
	a.publishHealingResult(result)
}

// dropDestinationTable runs the destination connector's drop_table, the same call and
// parameters a reload uses (executor, run_mode=reload): the bare table name, the
// pipeline's destination namespace, and the pipeline id for the connector's ownership
// check. Reports whether a table was actually dropped.
func (a *Agent) dropDestinationTable(ctx context.Context, pipelineID, table string) (bool, error) {
	if strings.TrimSpace(table) == "" {
		return false, fmt.Errorf("no destination table to drop")
	}
	connectorType, cfg, err := a.destination(ctx, pipelineID)
	if err != nil {
		return false, err
	}
	if a.mcpClient == nil {
		return false, fmt.Errorf("mcp client not configured for drop_table")
	}
	destConn := normalizeDestConnector(connectorType)
	resp, err := a.mcpClient.ExecuteWithContext(ctx, mcp.ExecuteRequest{
		Connector: destConn,
		Version:   "latest",
		Operation: "drop_table",
		Config:    cfg,
		Params: map[string]interface{}{
			"table":       table,
			"namespace":   a.destinationNamespace(ctx, pipelineID),
			"pipeline_id": pipelineID,
		},
	})
	if err != nil {
		return false, fmt.Errorf("drop_table on the %s destination failed: %v", destConn, err)
	}
	if resp == nil || !resp.Success {
		msg := "unknown error"
		if resp != nil && strings.TrimSpace(resp.Error) != "" {
			msg = resp.Error
		}
		return false, fmt.Errorf("drop_table on the %s destination failed: %s", destConn, msg)
	}
	dropped, _ := resp.Result["dropped"].(bool)
	return dropped, nil
}

// Stop gracefully shuts down the healer agent
func (a *Agent) Stop() {
	log.Info("[HealerAgent] Stopping...")
	a.cancel()
}

// handleSchemaChangeMessage processes incoming schema change events (Original Logic)
func (a *Agent) handleSchemaChangeMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	_, span := healerTracer.Start(ctx, "handle_schema_change")
	defer span.End()

	log.Infof("[HealerAgent] Received schema change event from offset %d", msg.Offset)

	// Parse event using smart deserialization (handles both Avro and JSON)
	var event SchemaChangeEvent
	if err := kafka.SmartDeserialize(msg.Value, &event); err != nil {
		log.Errorf("[HealerAgent] Failed to unmarshal event: %v", err)
		return err
	}

	span.SetAttributes(
		attribute.String("pipeline_id", event.PipelineID),
		attribute.String("event_type", event.EventType),
		attribute.String("change_type", event.SchemaChange.ChangeType),
		attribute.String("table", event.SchemaChange.Table),
	)

	log.Infof("[HealerAgent] Processing %s for pipeline %s: %s on %s",
		event.EventType, event.PipelineID,
		event.SchemaChange.ChangeType, event.SchemaChange.Table)

	// A change the producer already applied is history, not a decision. The CDC sink's
	// additive reconciler adds a new source column to the destination the moment it
	// appears (kafka-mcp-sink reportAppliedSchemaDrift) — by the time this message
	// lands, the column is live. Queueing it would invite a human to approve something
	// that has already happened, and applying it would re-run DDL against a destination
	// that has it. Record it and say so.
	if event.SchemaChange.Applied {
		return a.recordAlreadyAppliedChange(ctx, &event)
	}

	// One policy for batch and CDC: only a dropped table waits for a person.
	switch routeSchemaChange(event.SchemaChange) {
	case routeAwaitDecision:
		a.queueForDecision(ctx, &event)
	case routeRecord:
		a.recordDriftNotice(ctx, &event)
	default:
		a.autoApplyChange(ctx, &event)
	}
	return nil
}

// errUnsupportedDestination marks a destination the healer does not change column
// definitions on. It is not a failure of the change: the healer has nothing to run
// there, so the change is recorded rather than reported as broken.
var errUnsupportedDestination = errors.New("unsupported destination for schema changes")

// driftRoute is what the healer does with one detected schema change. The policy is
// the same for batch and CDC: only a dropped table waits for a person. Everything
// else is handled on the spot, and the user is told what happened after the fact.
type driftRoute int

const (
	// routeApply runs the change against the destination (a new column, a type change).
	routeApply driftRoute = iota
	// routeRecord files the change and runs nothing: a notice about the source (a new
	// table, a declared-type change) or a dropped column, which the destination keeps.
	routeRecord
	// routeAwaitDecision queues the change for a person. Only a dropped table lands
	// here, plus any other destructive statement a producer might send, which is
	// never run without a decision.
	routeAwaitDecision
)

// routeSchemaChange is the single statement of the drift policy.
func routeSchemaChange(sc SchemaChange) driftRoute {
	ddl := strings.TrimSpace(sc.DDL)
	switch {
	case isDropTableChange(sc.ChangeType, ddl):
		return routeAwaitDecision
	case isDroppedColumn(sc), isAdvisoryDDL(ddl):
		return routeRecord
	case isDestructiveDDL(ddl):
		return routeAwaitDecision
	default:
		return routeApply
	}
}

// isDropTableChange reports whether a change is a dropped source table, by its
// change type or by its statement.
func isDropTableChange(changeType, ddl string) bool {
	if strings.EqualFold(strings.TrimSpace(changeType), "drop_table") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ddl)), "drop table")
}

// parseDropColumnDDL extracts (table, column) from "ALTER TABLE <t> DROP COLUMN
// [IF EXISTS] <c>", the statement older producers sent for a dropped column.
func parseDropColumnDDL(ddl string) (table, column string, ok bool) {
	fields := strings.Fields(strings.TrimRight(strings.TrimSpace(ddl), ";"))
	if len(fields) < 6 ||
		!strings.EqualFold(fields[0], "ALTER") || !strings.EqualFold(fields[1], "TABLE") ||
		!strings.EqualFold(fields[3], "DROP") || !strings.EqualFold(fields[4], "COLUMN") {
		return "", "", false
	}
	rest := fields[5:]
	if len(rest) >= 3 && strings.EqualFold(rest[0], "IF") && strings.EqualFold(rest[1], "EXISTS") {
		rest = rest[2:]
	}
	if len(rest) != 1 {
		return "", "", false
	}
	return fields[2], rest[0], true
}

// isDroppedColumn reports whether a change is a column dropped from the source.
func isDroppedColumn(sc SchemaChange) bool {
	if strings.EqualFold(strings.TrimSpace(sc.ChangeType), "drop_column") {
		return true
	}
	_, _, ok := parseDropColumnDDL(sc.DDL)
	return ok
}

// DroppedColumnNote is the recorded "DDL" for a column dropped from the source. It is
// a note, not a statement: the destination keeps the column, with its history, and
// rsync.ai stops writing to it, so there is nothing to run. The batch detector and this
// package both build it here, so the UNIQUE (pipeline_id, ddl) row for one dropped
// column is the same row whichever path reported it.
func DroppedColumnNote(table, column string) string {
	return fmt.Sprintf(
		"-- drift: column %s.%s was dropped in the source. The destination keeps the column; rsync.ai no longer writes values to it.",
		strings.TrimSpace(table), strings.TrimSpace(column))
}

// normalizeDroppedColumn rewrites a dropped column reported as a DROP COLUMN statement
// into the note, so no path can ever run it and the row dedups with the batch one.
func normalizeDroppedColumn(sc *SchemaChange) {
	if isAdvisoryDDL(sc.DDL) {
		return
	}
	table, column, _ := parseDropColumnDDL(sc.DDL)
	if c := strings.TrimSpace(sc.ColumnName); c != "" {
		column = c
	}
	if column == "" {
		return
	}
	if t := strings.TrimSpace(sc.Table); t != "" {
		table = t
	}
	sc.ColumnName = column
	sc.DDL = DroppedColumnNote(table, column)
}

// driftNotice picks the sentence the user is told for one change.
type driftNotice int

const (
	noticeApplied driftNotice = iota
	noticeKept
	noticeRecorded
	noticeFailed
	noticeAwaitingDecision
)

// driftOutcome is how one change the healer handled on its own ends up: the
// schema_change_approvals status, who acted, why, and what the user is told.
type driftOutcome struct {
	status string // schema_change_approvals.status: applied | approved | failed
	actor  string
	reason string
	errMsg string
	notice driftNotice
}

// appliedBySchemaDrift is the reviewed_by attribution for a change the healer handled
// without a person, mirroring appliedByCDCSink.
const appliedBySchemaDrift = "system:schema-drift"

// fileDriftOutcome records a change the healer handled on its own: the row on the
// schema changes page, the healing history, and one notification. It deliberately
// does not publish to ResultsTopic; the notification here is the only announcement.
func (a *Agent) fileDriftOutcome(ctx context.Context, event *SchemaChangeEvent, o driftOutcome) {
	sc := event.SchemaChange
	if a.db != nil {
		var appliedAt, errMsg interface{}
		if o.status == "applied" {
			appliedAt = time.Now().UTC()
		}
		if o.errMsg != "" {
			errMsg = o.errMsg
		}
		// ON CONFLICT keeps the UNIQUE (pipeline_id, ddl) row authoritative: a change
		// reported twice (a retry, or batch and CDC both seeing it) is one row, and a
		// row queued under the old policy takes the outcome it has now.
		if _, err := a.db.ExecContext(ctx, `
			INSERT INTO schema_change_approvals (
				pipeline_id, change_type, table_name, ddl, reasoning, status,
				reviewed_by, reviewed_at, applied_at, error_message, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8, $9, NOW())
			ON CONFLICT (pipeline_id, ddl) DO UPDATE SET
				status = EXCLUDED.status,
				reasoning = EXCLUDED.reasoning,
				reviewed_by = EXCLUDED.reviewed_by,
				reviewed_at = NOW(),
				applied_at = EXCLUDED.applied_at,
				error_message = EXCLUDED.error_message,
				updated_at = NOW()
		`,
			event.PipelineID, sc.ChangeType, sc.Table, sc.DDL, o.reason, o.status,
			o.actor, appliedAt, errMsg,
		); err != nil {
			log.Errorf("[HealerAgent] Failed to record schema change outcome: %v", err)
		}
	}

	result := &HealingResult{
		PipelineID:   event.PipelineID,
		ChangeType:   sc.ChangeType,
		Table:        sc.Table,
		Status:       "skipped",
		Reason:       o.reason,
		ErrorMessage: o.errMsg,
	}
	switch o.status {
	case "applied":
		result.Status = "applied"
		result.DDLApplied = sc.DDL
	case "failed":
		result.Status = "failed"
	}
	a.recordHealingResult(ctx, result)
	a.notifyDrift(event, o.notice)

	log.Infof("[HealerAgent] Schema change %s on %s (pipeline %s): %s — %s",
		sc.ChangeType, sc.Table, event.PipelineID, o.status, o.reason)
}

// autoApplyChange runs a change against the destination without asking: a new column,
// or a type change that is a safe widening. A narrowing type change is refused by the
// connector (strict), and that refusal is what the user reads.
func (a *Agent) autoApplyChange(ctx context.Context, event *SchemaChangeEvent) {
	err := a.applyMigration(ctx, event.PipelineID, event.SchemaChange.DDL)
	switch {
	case err == nil:
		a.fileDriftOutcome(ctx, event, driftOutcome{
			status: "applied",
			actor:  appliedBySchemaDrift,
			reason: "Applied automatically: only a dropped table waits for approval",
			notice: noticeApplied,
		})
	case errors.Is(err, errUnsupportedDestination):
		a.fileDriftOutcome(ctx, event, driftOutcome{
			status: "approved",
			actor:  appliedBySchemaDrift,
			reason: "Recorded automatically. The healer changes column definitions only on PostgreSQL and MySQL destinations, so nothing was run against this one",
			notice: noticeRecorded,
		})
	default:
		log.Errorf("[HealerAgent] Automatic schema change failed for pipeline %s: %v", event.PipelineID, err)
		a.fileDriftOutcome(ctx, event, driftOutcome{
			status: "failed",
			actor:  appliedBySchemaDrift,
			reason: "Could not be applied automatically",
			errMsg: err.Error(),
			notice: noticeFailed,
		})
	}
}

// recordDriftNotice files a change that asks nothing of the destination: a dropped
// column, which the destination keeps, or a notice about the source.
func (a *Agent) recordDriftNotice(ctx context.Context, event *SchemaChangeEvent) {
	if isDroppedColumn(event.SchemaChange) {
		normalizeDroppedColumn(&event.SchemaChange)
		a.fileDriftOutcome(ctx, event, driftOutcome{
			status: "approved",
			actor:  appliedBySchemaDrift,
			reason: "Recorded automatically. The destination keeps the column and its history; rsync.ai no longer writes values to it",
			notice: noticeKept,
		})
		return
	}
	a.fileDriftOutcome(ctx, event, driftOutcome{
		status: "approved",
		actor:  appliedBySchemaDrift,
		reason: "Recorded automatically. This is a notice about the source, so nothing was run against the destination",
		notice: noticeRecorded,
	})
}

// queueForDecision files a change that waits for a person. The user is told once:
// a change re-detected while it is still pending is not announced again.
func (a *Agent) queueForDecision(ctx context.Context, event *SchemaChangeEvent) {
	reason := "A destructive change waits for a decision; it is never run automatically"
	var risks []string
	if isDropTableChange(event.SchemaChange.ChangeType, event.SchemaChange.DDL) {
		reason = "A dropped source table waits for a decision. Approve drops the destination table, Reject keeps it, and either way the table leaves this pipeline"
		risks = []string{"Approve permanently deletes the destination table and its rows"}
	}
	if !a.storePendingApproval(ctx, event, reason, risks) {
		log.Infof("[HealerAgent] %s on %s (pipeline %s) is already waiting for a decision; not announced again",
			event.SchemaChange.ChangeType, event.SchemaChange.Table, event.PipelineID)
		return
	}
	a.recordHealingResult(ctx, &HealingResult{
		PipelineID: event.PipelineID,
		ChangeType: event.SchemaChange.ChangeType,
		Table:      event.SchemaChange.Table,
		Status:     "pending_approval",
		Reason:     reason,
	})
	a.notifyDrift(event, noticeAwaitingDecision)
}

// storePendingApproval queues a change for a decision and reports whether it is newly
// pending. A row already pending stays as it is, which is what keeps a batch pipeline
// that re-detects a dropped table on every run from re-announcing it. A row decided
// before this detection goes back to pending, because the change has happened again.
// A row decided after it is left alone: the message is older than the decision.
// Approved is never re-pended, because an approved row is a drop already in flight.
// Withdrawn (WithdrawPendingTableDrops) is, so a table that vanishes again is asked
// about again.
func (a *Agent) storePendingApproval(ctx context.Context, event *SchemaChangeEvent, reasoning string, risks []string) bool {
	if a.db == nil {
		return true
	}
	var detectedAt interface{}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(event.SchemaChange.DetectedAt)); err == nil {
		detectedAt = t.UTC()
	}
	var risksText interface{}
	if len(risks) > 0 {
		risksText = strings.Join(risks, "; ")
	}
	var id string
	err := a.db.QueryRowContext(ctx, `
		INSERT INTO schema_change_approvals (
			pipeline_id, change_type, table_name, ddl, reasoning, risks, status, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'pending', NOW())
		ON CONFLICT (pipeline_id, ddl) DO UPDATE SET
			reasoning = EXCLUDED.reasoning,
			risks = EXCLUDED.risks,
			status = 'pending',
			reviewed_by = NULL,
			reviewed_at = NULL,
			applied_at = NULL,
			error_message = NULL,
			updated_at = NOW()
		WHERE schema_change_approvals.status IN ('rejected', 'applied', 'failed', 'withdrawn')
			AND schema_change_approvals.reviewed_at IS NOT NULL
			AND schema_change_approvals.reviewed_at < $7
		RETURNING id
	`,
		event.PipelineID,
		event.SchemaChange.ChangeType,
		event.SchemaChange.Table,
		event.SchemaChange.DDL,
		reasoning,
		risksText,
		detectedAt,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		log.Errorf("[HealerAgent] Failed to store pending approval: %v", err)
		return false
	}
	return true
}

// DroppedTableIsBackReason is shown on a pending drop decision withdrawn because
// the table is in the source again.
const DroppedTableIsBackReason = "The table is back in the source, so nothing was dropped and it stays in this pipeline."

// WithdrawPendingTableDrops withdraws a pipeline's pending dropped-table decisions
// for source tables that turn out to be there: back in the source's list, or left
// out of it only because the connection is missing a setting the table needs.
// Approve on such a row would drop a destination table whose source table exists,
// so it must stop being offered. Only a pending drop_table row moves; a decided
// one is left as it is. reason replaces the row's reasoning, which the schema
// changes page shows. It returns the tables withdrawn. Best-effort: an error is
// logged and nothing is withdrawn.
func WithdrawPendingTableDrops(ctx context.Context, db *sql.DB, pipelineID string, tables []string, reason string) []string {
	if db == nil {
		return nil
	}
	withdrawn, err := withdrawPendingTableDrops(ctx, db, pipelineID, tables, reason)
	if err != nil {
		log.Warnf("[HealerAgent] pipeline %s: could not withdraw pending table drops: %v", pipelineID, err)
		return nil
	}
	return withdrawn
}

// WithdrawPendingTableDropsTx is WithdrawPendingTableDrops inside the caller's
// transaction, for a caller that changes another fact about the same table and
// must not commit one without the other. It returns the error for the rollback.
func WithdrawPendingTableDropsTx(ctx context.Context, tx *sql.Tx, pipelineID string, tables []string, reason string) ([]string, error) {
	return withdrawPendingTableDrops(ctx, tx, pipelineID, tables, reason)
}

func withdrawPendingTableDrops(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, pipelineID string, tables []string, reason string) ([]string, error) {
	if strings.TrimSpace(pipelineID) == "" || len(tables) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(tables)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
		UPDATE schema_change_approvals
		SET status = 'withdrawn', reviewed_by = $3, reviewed_at = NOW(), reasoning = $4, updated_at = NOW()
		WHERE pipeline_id = $1
			AND table_name IN (SELECT jsonb_array_elements_text($2::jsonb))
			AND change_type = 'drop_table'
			AND status = 'pending'
		RETURNING table_name
	`, pipelineID, string(list), appliedBySchemaDrift, reason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var withdrawn []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err == nil {
			withdrawn = append(withdrawn, table)
		}
	}
	return withdrawn, rows.Err()
}

// notifyDrift sends the one notification for a change. Category=schema_drift
// deep-links it to the schema changes page (actionURLForError).
func (a *Agent) notifyDrift(event *SchemaChangeEvent, notice driftNotice) {
	se := diagnose.FromDiagnosis(
		diagnose.Diagnosis{Category: diagnose.CategorySchemaDrift},
		diagnose.Signal{PipelineID: event.PipelineID},
	)
	se.Code, se.Severity = driftNoticeCode(notice)
	se.UserMessage = llmscrub.Scrub(schemaDriftNotificationText(event.SchemaChange, notice))
	// Every drift on a pipeline shares this code AND this action_url, so without a
	// subject the notifier's 60-minute window collapsed them all into the first one:
	// drop a column, then drop a table five minutes later, and only the column was
	// ever announced.
	se.DedupSubject = schemaDriftDedupSubject(event.SchemaChange)
	a.notifyUserStructured(event.PipelineID, se)
}

// driftNoticeCode is the notification code and severity for one notice. Only a change
// awaiting a decision keeps SCHEMA_DRIFT_DETECTED, the one code the notifier gives Slack
// Approve/Reject buttons; a button on a change already handled would act on whatever
// else happens to be pending on the pipeline.
func driftNoticeCode(notice driftNotice) (string, diagnose.Severity) {
	switch notice {
	case noticeAwaitingDecision:
		return diagnose.CodeSchemaDriftDetected, diagnose.SeverityWarning
	case noticeFailed:
		return diagnose.CodeSchemaDriftApplyFailed, diagnose.SeverityWarning
	default:
		return diagnose.CodeSchemaDriftHandled, diagnose.SeverityInfo
	}
}

// isDestructiveDDL reports whether this DDL drops or truncates, in which case the
// healer never runs it as a statement — not automatically, and not after a user
// approves it. An approved dropped table is dropped through the destination
// connector's drop_table (dropApprovedTable), never by running its DDL. It
// is the single definition behind both the applyMigration guard and the
// approved-changes handler, so the two cannot drift into disagreeing about the
// same statement. The api-gateway mirrors it in autoApplicableDDL
// (handlers/schema_evolution.go) to tell the user, before they click Approve,
// that approval here is record-only; keep the two in lockstep.
func isDestructiveDDL(ddl string) bool {
	d := strings.ToLower(strings.TrimSpace(ddl))
	return strings.Contains(d, " drop ") || strings.HasPrefix(d, "drop ") ||
		strings.Contains(d, " truncate ") || strings.HasPrefix(d, "truncate ")
}

// isAdvisoryDDL reports whether this "DDL" is a note rather than a statement.
// The detector emits two of them: a new source table appearing
// ("-- drift: table X appeared in source") and a declared-type change the
// destination does not need ("-- drift: X.y declared type changed …", B1).
//
// Both used to read as auto-applicable, so approving one sent a SQL comment to
// the destination, which executed it happily, and the row was marked `applied`
// having applied nothing. That is the same false-success shape as C1, arrived at
// from the other side: there, the user was told nothing would be applied and
// something was attempted; here, they were told it was applied and nothing was.
func isAdvisoryDDL(ddl string) bool {
	return strings.HasPrefix(strings.TrimSpace(ddl), "--")
}

// notAutoAppliedDDL is the single predicate for "approving this records the
// decision and nothing runs". api-gateway mirrors it as !autoApplicableDDL to
// tell the user that BEFORE they click Approve; keep the two in lockstep.
func notAutoAppliedDDL(ddl string) bool {
	return isDestructiveDDL(ddl) || isAdvisoryDDL(ddl)
}

func (a *Agent) applyMigration(ctx context.Context, pipelineID, ddl string) error {
	pipelineID = strings.TrimSpace(pipelineID)
	ddl = strings.TrimSpace(ddl)
	if pipelineID == "" {
		return fmt.Errorf("missing pipeline_id")
	}
	if ddl == "" {
		return fmt.Errorf("missing ddl")
	}

	// Safety guard: never run a destructive statement. An approved table drop goes
	// through the connector's drop_table (dropApprovedTable), not through here.
	if isDestructiveDDL(ddl) {
		return fmt.Errorf("refusing to auto-apply potentially destructive ddl")
	}
	// An advisory note is not a statement. Sending it to the destination would
	// "succeed" (a comment is valid SQL everywhere) and report an apply that never
	// happened; the auto-apply path can reach here without passing through
	// handleApprovedChange, so the guard belongs on both.
	if isAdvisoryDDL(ddl) {
		return fmt.Errorf("refusing to apply advisory drift note as ddl")
	}

	destConnectorType, cfg, err := a.destination(ctx, pipelineID)
	if err != nil {
		return err
	}

	log.Infof("[HealerAgent] Applying DDL to %s destination for pipeline %s: %s",
		destConnectorType, pipelineID, ddl)

	// MVP: Apply directly for DB destinations we can safely connect to.
	// (Cloud warehouses / APIs should be routed via MCP in a future phase.)
	destConn := normalizeDestConnector(destConnectorType)

	// Additive ADD COLUMN → route through the destination connector's idempotent
	// ensure_table (the SAME path the batch write uses). The stored DDL is
	// source-shaped — a source schema qualifier (e.g. "pipeline_test.categories",
	// wrong for a dest that has "public.categories") and a source type token
	// (e.g. "string", which is not valid Postgres DDL). Running it verbatim
	// against the dest fails; ensure_table instead resolves the per-pipeline dest
	// namespace and maps the normalized type to the dest dialect. So add_column is
	// NEVER executed as raw DDL.
	if table, col, colType, ok := parseAddColumnDDL(ddl); ok {
		return a.ensureColumnViaConnector(ctx, pipelineID, destConn, cfg, table, col, colType, false)
	}

	// Type change (modify_column — approve-only, high-risk) → route through the SAME
	// connector ensure_table path as add_column, for the same reason: the stored DDL
	// ("ALTER TABLE inventory.products ALTER COLUMN price TYPE number") carries a source
	// schema qualifier and a canonical type token, neither valid at the dest, so raw
	// apply fails (KI-DRIFT-TYPECHANGE-APPLY-FAILS). ensure_table resolves the dest
	// namespace, maps the canonical type to the dest dialect, and applies a SAFE, non-
	// lossy widening on the existing column (MySQL via _WIDEN; Postgres via the mirrored
	// widen path). strict=true → a non-widening/narrowing change reports a clear error
	// (the user approved a change that cannot be applied safely) instead of silently
	// succeeding. Destructive DROP/TRUNCATE is already refused above.
	if table, col, colType, ok := parseModifyColumnDDL(ddl); ok {
		return a.ensureColumnViaConnector(ctx, pipelineID, destConn, cfg, table, col, colType, true)
	}

	// Any other DDL shape keeps the direct-apply path.
	switch destConn {
	case "postgresql":
		return applyDDLPostgres(ctx, cfg, ddl)
	case "mysql":
		return applyDDLMySQL(ctx, cfg, ddl)
	default:
		return fmt.Errorf("%w: %s", errUnsupportedDestination, destConnectorType)
	}
}

// destination resolves a pipeline's destination connector type (lower-cased) and its
// connection config.
func (a *Agent) destination(ctx context.Context, pipelineID string) (string, map[string]string, error) {
	if a.loadDestination != nil {
		return a.loadDestination(ctx, pipelineID)
	}
	if a.db == nil {
		return "", nil, fmt.Errorf("no database to resolve the destination connection from")
	}
	var destConnectionID, destConnectorType string
	err := a.db.QueryRowContext(ctx, `
		SELECT p.destination_connection_id, c.connector_type
		FROM pipelines p
		JOIN connections c ON c.id = p.destination_connection_id
		WHERE p.id = $1
	`, pipelineID).Scan(&destConnectionID, &destConnectorType)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get destination connection: %w", err)
	}
	destConnectionID = strings.TrimSpace(destConnectionID)
	destConnectorType = strings.ToLower(strings.TrimSpace(destConnectorType))
	if destConnectionID == "" || destConnectorType == "" {
		return "", nil, fmt.Errorf("destination connection missing for pipeline")
	}
	cfg, err := connections.NewManager(a.db).Get(ctx, destConnectionID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to load destination connection config: %w", err)
	}
	return destConnectorType, cfg, nil
}

// normalizeDestConnector folds destination connector aliases to the canonical MCP
// connector name — the value the mcp.Client uses for both the connector dir and the
// "<type>_<operation>" tool name. Mirrors the executor's dest routing (postgres→
// postgresql, mariadb→mysql). The healer must NOT import the executor package
// (executor imports healer), so this is kept in lockstep by hand.
func normalizeDestConnector(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "postgres", "postgresql":
		return "postgresql"
	case "mysql", "mariadb":
		return "mysql"
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// parseAddColumnDDL extracts (table, column, columnType) from the detector's
// deterministic add_column DDL "ALTER TABLE <table> ADD COLUMN <col> <type...>"
// (executor/schema_drift.go). <table> is source-qualified and <type> is the
// source's normalized token — both are corrected at apply time by the destination
// connector's ensure_table. Returns ok=false for any other DDL shape (e.g.
// ALTER COLUMN … TYPE, DROP COLUMN) so the caller falls back to direct apply.
func parseAddColumnDDL(ddl string) (table, column, columnType string, ok bool) {
	fields := strings.Fields(ddl)
	// Minimum: ALTER TABLE <t> ADD COLUMN <col> <type>  → 7 tokens.
	if len(fields) < 7 {
		return "", "", "", false
	}
	if !strings.EqualFold(fields[0], "ALTER") || !strings.EqualFold(fields[1], "TABLE") ||
		!strings.EqualFold(fields[3], "ADD") || !strings.EqualFold(fields[4], "COLUMN") {
		return "", "", "", false
	}
	table = strings.TrimSpace(fields[2])
	column = strings.TrimSpace(fields[5])
	columnType = strings.TrimSpace(strings.Join(fields[6:], " "))
	if table == "" || column == "" || columnType == "" {
		return "", "", "", false
	}
	return table, column, columnType, true
}

// parseModifyColumnDDL extracts (table, column, columnType) from the detector's
// deterministic modify_column DDL "ALTER TABLE <table> ALTER COLUMN <col> TYPE <type...>"
// (executor/schema_drift.go). Exactly like add_column, <table> is source-qualified and
// <type> is the source's canonical token ("number"/"string") — neither is valid at the
// destination, so running the DDL verbatim fails (KI-DRIFT-TYPECHANGE-APPLY-FAILS). Both
// are corrected at apply time by the destination connector's ensure_table (which resolves
// the dest namespace, maps canonical→dialect, and applies a SAFE widening on the existing
// column). Returns ok=false for any other DDL shape so the caller falls back to direct apply.
func parseModifyColumnDDL(ddl string) (table, column, columnType string, ok bool) {
	fields := strings.Fields(ddl)
	// Minimum: ALTER TABLE <t> ALTER COLUMN <col> TYPE <type>  → 8 tokens.
	if len(fields) < 8 {
		return "", "", "", false
	}
	if !strings.EqualFold(fields[0], "ALTER") || !strings.EqualFold(fields[1], "TABLE") ||
		!strings.EqualFold(fields[3], "ALTER") || !strings.EqualFold(fields[4], "COLUMN") ||
		!strings.EqualFold(fields[6], "TYPE") {
		return "", "", "", false
	}
	table = strings.TrimSpace(fields[2])
	column = strings.TrimSpace(fields[5])
	columnType = strings.TrimSpace(strings.Join(fields[7:], " "))
	if table == "" || column == "" || columnType == "" {
		return "", "", "", false
	}
	return table, column, columnType, true
}

// bareTable strips a schema/namespace qualifier ("pipeline_test.categories" →
// "categories"). The destination connector resolves the dest schema from the
// forwarded namespace, so it must receive the bare table name.
func bareTable(qualified string) string {
	q := strings.TrimSpace(qualified)
	if i := strings.LastIndex(q, "."); i >= 0 && i < len(q)-1 {
		return q[i+1:]
	}
	return q
}

// DestinationQualifiedTable renders the object name a drop_table statement carries,
// given the table's SOURCE key and the pipeline's destination namespace.
//
// The statement itself is never executed. It is what the user reads on the approval
// card: approving runs the destination connector's drop_table with the bare table name
// and the pipeline's namespace (dropApprovedTable), not this string. So it must name
// the table that will actually be dropped. It was being built from the SOURCE key
// (`<source_schema>.<table>`), which on this product is almost never the destination's
// name for the same table: the destination lives in the pipeline's own namespace
// (`rsync_<src-schema>_<id8>`). Showing `DROP TABLE public.orders` for a destination
// table is not a harmless typo — `public.orders` very plausibly EXISTS on the
// destination and is a different, real table.
//
// Empty or "default" namespace = legacy pipeline with nothing persisted; the connector
// writes to its own default schema, so the bare table name is what resolves in the user's
// destination session. The source qualifier is never a correct answer here.
//
// Only drop_table goes through this. add_column/modify_column keep the
// source key because the healer rewrites them at apply time (bareTable +
// destinationNamespace in ensureColumnViaConnector), and changing their text would
// re-key the schema_change_approvals UNIQUE(pipeline_id, ddl) dedup for no gain.
func DestinationQualifiedTable(destNamespace, sourceQualified string) string {
	t := bareTable(sourceQualified)
	if ns := strings.TrimSpace(destNamespace); ns != "" && !strings.EqualFold(ns, "default") {
		return ns + "." + t
	}
	return t
}

// destinationNamespace returns the per-pipeline dest namespace persisted at
// pipelines.config->>'destination_namespace' (the HITL namespace). Empty for
// legacy pipelines — the connector then falls back to its own default schema.
// Mirrors executor.resolveDestinationNamespace's DB read (not imported: executor
// imports healer).
func (a *Agent) destinationNamespace(ctx context.Context, pipelineID string) string {
	if a.db == nil {
		return ""
	}
	var ns sql.NullString
	err := a.db.QueryRowContext(ctx, `
		SELECT NULLIF(TRIM(COALESCE(config->>'destination_namespace', '')), '')
		FROM pipelines WHERE id = $1
	`, pipelineID).Scan(&ns)
	if err != nil || !ns.Valid {
		return ""
	}
	if n := strings.TrimSpace(ns.String); !strings.EqualFold(n, "default") {
		return n
	}
	return ""
}

// ensureColumnViaConnector applies a drifted column change (add_column OR modify_column
// type change) to the destination by invoking the destination MCP connector's idempotent
// ensure_table tool — the same path the batch write uses. The connector resolves the dest
// schema from the forwarded namespace and maps the normalized column type to the dest
// dialect (e.g. "string" → TEXT), so a source-shaped column definition lands correctly:
//   - add_column: ensure_table's ADD COLUMN IF NOT EXISTS creates the column.
//   - modify_column: ensure_table detects the existing column and applies a SAFE, non-lossy
//     widening (MySQL via _WIDEN, Postgres via the mirrored widen path).
// types_are_ddl is left false so the type is treated as a canonical token, not final DDL.
// synthetic_pk is set false so a keyless migration call does NOT inject the Fivetran-style
// _rsync_row_hash/_rsync_synced_at columns + unique index onto an existing table (this is a
// pure column ensure, not a table (re)creation). strict makes a requested type change that
// is NOT a safe widening report a clear error instead of silently succeeding — used for
// approved type changes, where the user must learn an unsafe cast could not be applied.
func (a *Agent) ensureColumnViaConnector(ctx context.Context, pipelineID, destConn string, cfg map[string]string, srcTable, column, columnType string, strict bool) error {
	if destConn != "postgresql" && destConn != "mysql" {
		return fmt.Errorf("%w: %s", errUnsupportedDestination, destConn)
	}
	if a.mcpClient == nil {
		return fmt.Errorf("mcp client not configured for connector-based apply")
	}

	table := bareTable(srcTable)
	if table == "" {
		return fmt.Errorf("could not resolve destination table from %q", srcTable)
	}

	params := map[string]interface{}{
		"table":        table,
		"columns":      []string{column},
		"column_types": map[string]string{column: columnType},
		// A column-migration call never (re)creates keys; block the keyless
		// synthetic-PK fallback so we don't pollute an existing table.
		"synthetic_pk": false,
	}
	if strict {
		params["strict_type_change"] = true
	}
	if pipelineID != "" {
		params["pipeline_id"] = pipelineID
	}
	// Forward the per-pipeline dest namespace (both keys, mirroring the sink's
	// addNamespaceParam) only when it is real; empty → connector default schema.
	if ns := a.destinationNamespace(ctx, pipelineID); ns != "" {
		params["namespace"] = ns
		params["db_or_schema"] = ns
	}

	log.Infof("[HealerAgent] ensure_table column change on %s dest (pipeline %s): table=%s column=%s type=%q strict=%v",
		destConn, pipelineID, table, column, columnType, strict)

	resp, err := a.mcpClient.ExecuteWithContext(ctx, mcp.ExecuteRequest{
		Connector: destConn,
		Version:   "latest",
		Operation: "ensure_table",
		Config:    cfg,
		Params:    params,
	})
	if err != nil {
		return fmt.Errorf("ensure_table call failed: %w", err)
	}
	if resp == nil || !resp.Success {
		msg := "unknown error"
		if resp != nil && strings.TrimSpace(resp.Error) != "" {
			msg = resp.Error
		}
		return fmt.Errorf("ensure_table did not succeed: %s", msg)
	}
	return nil
}

func applyDDLPostgres(ctx context.Context, cfg map[string]string, ddl string) error {
	host := strings.TrimSpace(cfg["host"])
	port := strings.TrimSpace(cfg["port"])
	user := strings.TrimSpace(cfg["user"])
	pass := cfg["password"]
	dbname := strings.TrimSpace(cfg["database"])
	if dbname == "" {
		dbname = strings.TrimSpace(cfg["dbname"])
	}
	// Host-aware SSL: stored PG connections carry no sslmode, so a bare
	// "disable" default made auto-migration DDL against a managed PG
	// (Azure/RDS) fail with "no pg_hba.conf entry … no encryption".
	sslmode := cdc.ResolvePostgresSSLMode(
		map[string]interface{}{"sslmode": cfg["sslmode"], "ssl_mode": cfg["ssl_mode"]}, host)
	if host == "" || user == "" || dbname == "" {
		return fmt.Errorf("postgres config missing host/user/database")
	}
	if port == "" {
		port = "5432"
	}

	// Use a short, bounded timeout for DDL apply.
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// NOTE: requires pq driver registered in the running binary.
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s", host, port, user, pass, dbname, sslmode)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(reqCtx); err != nil {
		return fmt.Errorf("postgres ping failed: %w", err)
	}
	if _, err := db.ExecContext(reqCtx, ddl); err != nil {
		return fmt.Errorf("postgres ddl exec failed: %w", err)
	}
	return nil
}

func applyDDLMySQL(ctx context.Context, cfg map[string]string, ddl string) error {
	host := strings.TrimSpace(cfg["host"])
	port := strings.TrimSpace(cfg["port"])
	user := strings.TrimSpace(cfg["user"])
	pass := cfg["password"]
	dbname := strings.TrimSpace(cfg["database"])
	if dbname == "" {
		dbname = strings.TrimSpace(cfg["schema"])
	}
	if host == "" || user == "" || dbname == "" {
		return fmt.Errorf("mysql config missing host/user/database")
	}
	if port == "" {
		port = "3306"
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// NOTE: requires mysql driver registered in the running binary.
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&multiStatements=false", user, pass, host, port, dbname)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(reqCtx); err != nil {
		return fmt.Errorf("mysql ping failed: %w", err)
	}
	if _, err := db.ExecContext(reqCtx, ddl); err != nil {
		return fmt.Errorf("mysql ddl exec failed: %w", err)
	}
	return nil
}

// recordAlreadyAppliedChange files a schema change the CDC sink has already handled, so
// it appears on the pipeline's schema changes page as history rather than vanishing. It
// never queues an approval and never applies anything.
//
// Batch and CDC now follow one policy (routeSchemaChange): only a dropped table waits
// for a person. The sink adds a new column itself, and keeps a dropped column in the
// destination; this records what it did and tells the user once. It used to publish to
// ResultsTopic as well, which reached the inbox as a second notification for the same
// change.
func (a *Agent) recordAlreadyAppliedChange(ctx context.Context, event *SchemaChangeEvent) error {
	if isDroppedColumn(event.SchemaChange) {
		normalizeDroppedColumn(&event.SchemaChange)
		a.fileDriftOutcome(ctx, event, driftOutcome{
			status: "approved",
			actor:  appliedByCDCSink,
			reason: "Recorded automatically. The destination keeps the column and its history; rsync.ai no longer writes values to it",
			notice: noticeKept,
		})
		return nil
	}
	a.fileDriftOutcome(ctx, event, driftOutcome{
		status: "applied",
		actor:  appliedByCDCSink,
		reason: "Applied automatically by the CDC sink's additive schema reconciler",
		notice: noticeApplied,
	})
	return nil
}

// schemaDriftNotificationText renders the user-facing drift notification, one sentence
// per outcome (driftNotice). Every variant is deliberately APOSTROPHE-FREE, and that is
// load-bearing, not style.
//
// Every notification body passes through llmscrub.Scrub, whose dangling-quote rule is
// fail-closed: a log pipeline can truncate a quoted literal open, so an unpaired ' masks
// everything after it. That is right for error text carrying row values and wrong for
// author-written copy — the previous sentence ended "…in the pipeline's Schema Changes
// tab" and reached users as "…in the pipeline'[redacted]", cutting off the one
// instruction the notification existed to give.
//
// Every variant also names the SCHEMA CHANGES PAGE, not a tab. The apostrophe fix above
// preserved the words "Schema Changes tab", and no such tab exists: the pipeline detail
// renders exactly six (Overview, Execution History, Steps/DAG, Table statistics,
// Transforms, Data flow). Drift lives on its own route, /pipelines/{id}/schema-changes,
// which this alert already deep-links (Category=schema_drift ⇒ actionURLForError). So the
// sentence survived the scrubber and still could not be followed — the user reaches the
// pipeline, counts six tabs, and stops. Naming the alert's own Review change link is the
// reliable direction: it is the one route in that does not depend on the amber drift
// badge, which renders only while something is pending.
//
// Extracted so TestDriftNotificationCopySurvivesScrub can assert the rendered sentence
// passes through Scrub unchanged. Weakening the scrubber instead would trade a user
// data-leak guard for a copy nit, and would have to be mirrored in the Python scrubber.
func schemaDriftNotificationText(sc SchemaChange, notice driftNotice) string {
	const where = "It is recorded on the schema changes page for this pipeline, which the Review change link opens."
	changeType, table := sc.ChangeType, sc.Table
	switch notice {
	case noticeApplied:
		return fmt.Sprintf(
			"Schema change applied automatically: %s on %s. Only a dropped table waits for approval, so nothing was needed from you. %s",
			changeType, table, where)
	case noticeKept:
		if c := strings.TrimSpace(sc.ColumnName); c != "" {
			return fmt.Sprintf(
				"Column %s was dropped from %s in the source. The destination keeps the column, and rsync.ai no longer writes values to it. %s",
				c, table, where)
		}
		return fmt.Sprintf(
			"A column was dropped from %s in the source. The destination keeps the column, and rsync.ai no longer writes values to it. %s",
			table, where)
	case noticeRecorded:
		return fmt.Sprintf(
			"Schema change recorded: %s on %s. Nothing was run against the destination for it. %s",
			changeType, table, where)
	case noticeFailed:
		return fmt.Sprintf(
			"Schema change could not be applied automatically: %s on %s. The reason is on the schema changes page for this pipeline, which the Review change link opens.",
			changeType, table)
	}
	if isDropTableChange(changeType, sc.DDL) {
		return fmt.Sprintf(
			"Source table %s was dropped. Approve drops the destination table, Reject keeps it, and either way the table leaves this pipeline. Decide on the schema changes page for this pipeline, which the Review change link opens.",
			table)
	}
	return fmt.Sprintf(
		"Schema change detected: %s on %s. Review it on the schema changes page for this pipeline, which the Review change link opens.",
		changeType, table)
}

// schemaDriftDedupSubject builds the per-event identity the notifier folds into its
// dedup key (diagnose.StructuredError.DedupSubject).
//
// It deliberately does NOT include the DDL. Two facts about the same drift can reach
// here with differently-rendered DDL — the batch detector's synthesized statement and
// the CDC path's normalized one — and keying on it would file both. change_type +
// table + column is the smallest tuple that separates two genuinely different drifts
// while still collapsing retries of one.
func schemaDriftDedupSubject(sc SchemaChange) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(sc.ChangeType)),
		strings.ToLower(strings.TrimSpace(sc.Table)),
		strings.ToLower(strings.TrimSpace(sc.ColumnName)),
	}
	subject := strings.Join(parts, ":")
	if subject == "::" {
		// Nothing identifying at all — fall back to the old whole-family behavior
		// rather than inventing a key that splits every retry into its own row.
		return ""
	}
	return subject
}

// appliedByCDCSink is the reviewed_by attribution for a change no human reviewed.
// Naming the actor keeps the schema changes page honest: the row says who applied it,
// and it was not the user.
const appliedByCDCSink = "system:cdc-sink"

// notifyUser sends a notification with just a string message.
//
// This is the legacy path kept for backward compatibility with the 12+
// existing callers. NEW code should use notifyUserStructured, which
// produces a full StructuredError envelope with a code + remediation.
// Migrate callers incrementally; each migration improves the user's
// notification UX from "Pipeline failed: <raw error>" to
// "MYSQL_BINLOG_FORMAT_NOT_ROW: here's the SQL to fix it".
func (a *Agent) notifyUser(pipelineID, message string) {
	// Scrub row values / PII before the message is persisted to
	// pipeline_notifications and shown to the user. Raw DB/sink errors carry
	// literal row data ("Failing row contains (...)", "Key (col)=(val)",
	// "Duplicate entry 'jane@acme.com'"), which must never reach a user-facing
	// or persisted notification.
	message = llmscrub.Scrub(message)
	// Wrap the legacy string into a minimal StructuredError so the
	// notifier consumer always sees a consistent envelope shape on the
	// wire. failure_type=system_error + audience=user is the safe
	// default for legacy callers that don't classify the failure.
	se := diagnose.NewStructuredError(
		"LEGACY_UNCLASSIFIED",
		diagnose.FailureTypeSystemError,
		diagnose.AudienceUser,
		message,
	)
	se.PipelineID = pipelineID
	se.InternalMessage = message
	a.notifyUserStructured(pipelineID, se)
}

// actionURLForError returns the RELATIVE frontend deep-link path for a
// notification. Schema-drift errors link straight to the pipeline's schema
// changes page (where the approve/reject cards render); every other error
// links to the pipeline overview. The path is intentionally relative — the
// notifier consumer prefixes the absolute host (APP_BASE_URL) at delivery
// time, so persisted rows stay host-agnostic.
func actionURLForError(pipelineID string, se *diagnose.StructuredError) string {
	if se != nil && se.Category == diagnose.CategorySchemaDrift {
		return fmt.Sprintf("/pipelines/%s/schema-changes", pipelineID)
	}
	return fmt.Sprintf("/pipelines/%s", pipelineID)
}

// notifyUserStructured emits a notification containing a full
// StructuredError envelope. This is the preferred path — callers that
// have a Diagnosis should build the StructuredError via
// diagnose.FromDiagnosis() and pass it here.
//
// Wire format (Kafka topic rsync.notifications):
//
//	{
//	  "type": "structured_error_notification",
//	  "pipeline_id": "...",
//	  "timestamp": "RFC3339",
//	  "action_url": "/pipelines/{id}",
//	  "error": <StructuredError JSON>,
//	  // legacy fields preserved for old consumers:
//	  "message": "<user_message>",
//	}
func (a *Agent) notifyUserStructured(pipelineID string, se *diagnose.StructuredError) {
	if se == nil {
		return
	}
	if se.PipelineID == "" {
		se.PipelineID = pipelineID
	}
	if a.notifications != nil {
		a.notifications(pipelineID, se)
		return
	}
	actionURL := actionURLForError(pipelineID, se)

	notification := map[string]interface{}{
		"type":        "structured_error_notification",
		"pipeline_id": pipelineID,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  actionURL,
		"error":       se,
		// Backward-compatibility: legacy consumers still read "message".
		"message": se.UserMessage,
	}

	notificationBytes, err := json.Marshal(notification)
	if err != nil {
		log.Errorf("[HealerAgent] Failed to marshal notification: %v", err)
		return
	}

	if a.kafkaManager != nil {
		if err := a.kafkaManager.Produce(NotifyTopic, nil, notificationBytes); err != nil {
			log.Errorf("[HealerAgent] Failed to publish notification: %v", err)
		}
	}

	log.WithFields(log.Fields{
		"pipeline_id":  pipelineID,
		"code":         se.Code,
		"failure_type": se.FailureType,
		"audience":     se.Audience,
	}).Infof("[HealerAgent] Sent structured notification: %s", se.UserMessage)
}

func (a *Agent) recordHealingResult(ctx context.Context, result *HealingResult) {
	if a.db != nil {
		if _, err := a.db.ExecContext(ctx, `
			INSERT INTO healing_history (
				pipeline_id, change_type, table_name,
				status, reason, ddl_applied, error_message, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		`,
			result.PipelineID,
			result.ChangeType,
			result.Table,
			result.Status,
			result.Reason,
			result.DDLApplied,
			result.ErrorMessage,
		); err != nil {
			log.Errorf("[HealerAgent] Failed to record healing result: %v", err)
		}
	}

	// F-Obs-2: record the schema-change healing action by change type and
	// resolved outcome so a "Healer actions by outcome" dashboard panel and
	// the escalation alert have data. Both labels are bounded enums.
	appmetrics.HealerActionsTotal.
		WithLabelValues(result.ChangeType, healingOutcome(result.Status)).
		Inc()
}

// healingOutcome maps a HealingResult.Status to the bounded outcome label
// used by rsync_healer_actions_total (success|failure|escalated|skipped).
// pending_approval means automated recovery deferred to a human → escalated.
func healingOutcome(status string) string {
	switch status {
	case "applied":
		return "success"
	case "failed":
		return "failure"
	case "pending_approval":
		return "escalated"
	default:
		return "skipped"
	}
}

// publishHealingResult emits the result to ResultsTopic, but only when
// RSYNC_SCHEMA_DRIFT_ENABLED=true: the topic is provisioned only then
// (kafka.EnsurePlatformTopics), and the producer's auto-create would otherwise
// bring it back at broker defaults on a default install.
func (a *Agent) publishHealingResult(result *HealingResult) {
	if !config.SchemaDriftEnabled() {
		return
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		log.Errorf("[HealerAgent] Failed to marshal result: %v", err)
		return
	}

	if a.kafkaManager != nil {
		if err := a.kafkaManager.Produce(ResultsTopic, nil, resultBytes); err != nil {
			log.Errorf("[HealerAgent] Failed to publish result: %v", err)
		}
	}
}
