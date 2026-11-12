package handlers

// Pipeline plans: a pipeline Ask rsync.ai or an agent token proposed (the
// plan_pipeline tool). Proposing creates nothing and moves no data. The server
// checks the proposal with the chat's own rules (sync mode, CDC capability,
// schedule, namespace, the tables against the source) and stores it; the person
// who asked opens it in rsync.ai, picks the tables it could not pick for them and
// clicks Create. That click is a REST call with their session, checked as
// pipeline.create and pipeline.run, and creates the pipeline exactly as chat
// does (createAndRunPipelineAs). No agent token can apply a plan: agent tokens
// reach only /api/agent/v1/mcp.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"api-gateway/internal/chat"
	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	"api-gateway/internal/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/shared/naming"
	log "github.com/sirupsen/logrus"
)

const (
	// pipelinePlanTTL is how long a plan can be applied after it was proposed.
	pipelinePlanTTL = 24 * time.Hour
	// maxOpenPipelinePlans caps the unapplied, unexpired plans one person has
	// in a workspace, so a looping agent cannot fill the table.
	maxOpenPipelinePlans = 20
	// maxPlanTables caps the tables one plan may name.
	maxPlanTables = 100
)

// PipelinePlanInput is what plan_pipeline asks for. Every field but the two
// connections is optional.
type PipelinePlanInput struct {
	SourceConnectionID      string
	DestinationConnectionID string
	// Tables are the source tables to copy, bare or schema-qualified.
	Tables []string
	// SyncMode is "batch", "cdc", or empty for the source connection's default.
	SyncMode string
	// ScheduleType is "cron" or "interval"; a batch pipeline only.
	ScheduleType string
	Cron         string
	EverySeconds int
	Timezone     string
	// DestinationNamespace is the schema or database the data lands in; empty
	// lets rsync.ai choose, as on every create.
	DestinationNamespace string
}

// PlanConnection is one side of a plan.
type PlanConnection struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ConnectorType string `json:"connector_type"`
}

// PlanTableChoice is a table name the source has in more than one schema. The
// server never picks one: the person does, on the plan.
type PlanTableChoice struct {
	Name    string   `json:"name"`
	Options []string `json:"options"`
}

// PlanSchedule is a plan's batch schedule.
type PlanSchedule struct {
	Type         string `json:"type"`
	Cron         string `json:"cron,omitempty"`
	EverySeconds int    `json:"every_seconds,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Description  string `json:"description"`
}

func (s PlanSchedule) spec() ScheduleSpec {
	return ScheduleSpec{Cron: s.Cron, EverySeconds: s.EverySeconds, Timezone: s.Timezone}
}

// PipelinePlan is a proposed pipeline. It holds names, types and ids, never a
// row value. Status is proposed, applied or expired.
type PipelinePlan struct {
	ID                   string            `json:"id"`
	Status               string            `json:"status"`
	ProposedBy           string            `json:"proposed_by"`
	Source               PlanConnection    `json:"source"`
	Destination          PlanConnection    `json:"destination"`
	SyncMode             string            `json:"sync_mode"`
	CDCMode              string            `json:"cdc_mode,omitempty"`
	Tables               []string          `json:"tables"`
	TableChoices         []PlanTableChoice `json:"table_choices,omitempty"`
	MissingTables        []string          `json:"missing_tables,omitempty"`
	TablesChecked        bool              `json:"tables_checked"`
	Schedule             *PlanSchedule     `json:"schedule,omitempty"`
	DestinationNamespace string            `json:"destination_namespace,omitempty"`
	Notes                []string          `json:"notes,omitempty"`
	URL                  string            `json:"url"`
	CreatedAt            time.Time         `json:"created_at"`
	ExpiresAt            time.Time         `json:"expires_at"`
	AppliedPipelineID    string            `json:"applied_pipeline_id,omitempty"`
}

// pipelinePlanBody is what the plan column stores: the plan without its
// envelope. The row's own columns hold the id, status, author and times, so a
// copy in the column was blank at insert and stale after an apply.
type pipelinePlanBody struct {
	Source               PlanConnection    `json:"source"`
	Destination          PlanConnection    `json:"destination"`
	SyncMode             string            `json:"sync_mode"`
	CDCMode              string            `json:"cdc_mode,omitempty"`
	Tables               []string          `json:"tables"`
	TableChoices         []PlanTableChoice `json:"table_choices,omitempty"`
	MissingTables        []string          `json:"missing_tables,omitempty"`
	TablesChecked        bool              `json:"tables_checked"`
	Schedule             *PlanSchedule     `json:"schedule,omitempty"`
	DestinationNamespace string            `json:"destination_namespace,omitempty"`
	Notes                []string          `json:"notes,omitempty"`
}

func (p *PipelinePlan) body() pipelinePlanBody {
	return pipelinePlanBody{
		Source: p.Source, Destination: p.Destination, SyncMode: p.SyncMode, CDCMode: p.CDCMode,
		Tables: p.Tables, TableChoices: p.TableChoices, MissingTables: p.MissingTables,
		TablesChecked: p.TablesChecked, Schedule: p.Schedule,
		DestinationNamespace: p.DestinationNamespace, Notes: p.Notes,
	}
}

func (p *PipelinePlan) setBody(b pipelinePlanBody) {
	p.Source, p.Destination, p.SyncMode, p.CDCMode = b.Source, b.Destination, b.SyncMode, b.CDCMode
	p.Tables, p.TableChoices, p.MissingTables = b.Tables, b.TableChoices, b.MissingTables
	p.TablesChecked, p.Schedule = b.TablesChecked, b.Schedule
	p.DestinationNamespace, p.Notes = b.DestinationNamespace, b.Notes
}

const (
	planStatusProposed = "proposed"
	planStatusApplied  = "applied"
	planStatusExpired  = "expired"
)

// PlanRefusal is a proposal or an apply the server refuses, with the HTTP
// status and a sentence for the person.
type PlanRefusal struct {
	Code    string
	Status  int
	Message string
	// Branch is the 1-based workflow branch a workflow refusal is about, so
	// the editor can open it; 0 when it is about no branch.
	Branch int
	// Step is the id of the workflow step a workflow refusal is about, ""
	// when it is about none.
	Step string
}

func (r *PlanRefusal) Error() string { return r.Message }

func refusePlan(code string, status int, format string, a ...any) *PlanRefusal {
	return &PlanRefusal{Code: code, Status: status, Message: fmt.Sprintf(format, a...)}
}

// planForbidden says why the caller may not propose a pipeline, in its own terms.
func planForbidden(actor services.Actor) *PlanRefusal {
	switch {
	case !actor.Role.Meets(security.WSMember) && actor.Channel == services.ChannelAgent:
		return refusePlan("forbidden", http.StatusForbidden,
			"This token's user is a viewer in the workspace, and only a member, admin or owner can create pipelines.")
	case !actor.Role.Meets(security.WSMember):
		return refusePlan("forbidden", http.StatusForbidden,
			"Only a workspace member, admin or owner can create pipelines. Ask an admin to give you the member role.")
	default:
		return refusePlan("forbidden", http.StatusForbidden,
			"This token lacks the %s scope. Create a token that has it.", services.ScopePipelinesPlan)
	}
}

// pipelinePlanURL is the page in rsync.ai where the person reviews and creates a plan.
func pipelinePlanURL(id string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("APP_BASE_URL")), "/")
	if base == "" {
		base = "http://localhost:3000"
	}
	return base + "/pipeline-plans/" + id
}

// ProposePipelinePlan checks a proposed pipeline and stores it as a plan for
// actor's user. It creates no pipeline. Only Ask rsync.ai and an agent token
// propose plans; a person in the UI creates the pipeline directly.
func ProposePipelinePlan(c *gin.Context, actor services.Actor, in PipelinePlanInput) (*PipelinePlan, error) {
	if actor.Channel != services.ChannelAssistant && actor.Channel != services.ChannelAgent {
		return nil, refusePlan("forbidden", http.StatusForbidden, "Only Ask rsync.ai or an agent token proposes a pipeline plan.")
	}
	if err := services.Authorize(actor, services.ActionPipelinePlan); err != nil {
		return nil, planForbidden(actor)
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	ws, userID := actor.WorkspaceID, actor.UserID

	srcID := strings.TrimSpace(in.SourceConnectionID)
	dstID := strings.TrimSpace(in.DestinationConnectionID)
	for _, id := range []string{srcID, dstID} {
		if _, err := uuid.Parse(id); err != nil {
			return nil, refusePlan("invalid_connection", http.StatusBadRequest,
				"%q is not a connection id. list_connections gives each connection's id.", id)
		}
	}
	if strings.EqualFold(srcID, dstID) {
		return nil, refusePlan("same_connection", http.StatusBadRequest,
			"The source and the destination are the same connection. Pick two different ones.")
	}

	var open int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pipeline_plans
		WHERE workspace_id = $1 AND created_by = $2 AND status = 'proposed' AND expires_at > NOW()
	`, ws, userID).Scan(&open); err != nil {
		return nil, fmt.Errorf("count open pipeline plans: %w", err)
	}
	if open >= maxOpenPipelinePlans {
		return nil, refusePlan("too_many_plans", http.StatusTooManyRequests,
			"You have %d pipeline plans waiting to be created. Create them or let them expire (after 24 hours) before proposing more.", open)
	}

	plan := &PipelinePlan{
		ProposedBy:    actorKindFor(actor.Channel),
		TablesChecked: true,
	}
	for _, side := range []struct {
		id   string
		into *PlanConnection
		what string
	}{{srcID, &plan.Source, "source"}, {dstID, &plan.Destination, "destination"}} {
		side.into.ID = side.id
		err := database.QueryRowContext(ctx,
			`SELECT name, connector_type FROM connections WHERE id = $1 AND workspace_id = $2`,
			side.id, ws).Scan(&side.into.Name, &side.into.ConnectorType)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refusePlan("connection_not_found", http.StatusNotFound,
				"No %s connection %s in this workspace. list_connections gives each connection's id.", side.what, side.id)
		}
		if err != nil {
			return nil, fmt.Errorf("load %s connection: %w", side.what, err)
		}
	}
	if msg := connectionLifecycleDraftError(database, ws, srcID, dstID); msg != "" {
		return nil, refusePlan("draft_connector", http.StatusUnprocessableEntity, "%s", msg)
	}

	// Sync mode, as chat's confirmation resolves it: what was asked, else the
	// source connection's default, else batch; CDC only from a source that can.
	switch mode := strings.ToLower(strings.TrimSpace(in.SyncMode)); mode {
	case "batch", "cdc":
		plan.SyncMode = mode
	case "", "auto":
		if connSync, connCDC := getSourceConnectionModes(database, srcID); connSync == "cdc" || connSync == "batch" {
			plan.SyncMode = connSync
			if connSync == "cdc" {
				plan.CDCMode = connCDC
			}
		} else {
			plan.SyncMode = "batch"
		}
	default:
		return nil, refusePlan("invalid_sync_mode", http.StatusBadRequest,
			"sync_mode %q is not one of batch or cdc.", in.SyncMode)
	}
	if plan.SyncMode == "cdc" && !connectorSupportsCDC(plan.Source.ConnectorType) {
		plan.SyncMode, plan.CDCMode = "batch", ""
		plan.Notes = append(plan.Notes, fmt.Sprintf(
			"%s can't stream changes (CDC), so this pipeline copies in batch.", getFriendlyName(plan.Source.ConnectorType)))
	}
	if plan.SyncMode == "cdc" && plan.CDCMode == "" {
		plan.CDCMode = "initial"
	}

	// Schedule: a batch pipeline's runs. CDC streams continuously.
	schedType := strings.ToLower(strings.TrimSpace(in.ScheduleType))
	if schedType == "" && strings.TrimSpace(in.Cron) != "" {
		schedType = "cron"
	} else if schedType == "" && in.EverySeconds > 0 {
		schedType = "interval"
	}
	if schedType != "" {
		if plan.SyncMode == "cdc" {
			plan.Notes = append(plan.Notes,
				"CDC pipelines stream changes continuously, so the schedule was left out. To copy on a schedule "+
					"instead, ask for a batch pipeline.")
		} else {
			sched := PlanSchedule{Type: schedType, Timezone: strings.TrimSpace(in.Timezone)}
			if schedType == "cron" {
				sched.Cron = strings.TrimSpace(in.Cron)
			} else {
				sched.EverySeconds = in.EverySeconds
			}
			if err := validateScheduleSpec(sched.Type, sched.spec()); err != nil {
				return nil, refusePlan("invalid_schedule", http.StatusBadRequest, "The schedule is not valid: %v.", err)
			}
			if sched.Timezone != "" {
				if _, err := time.LoadLocation(sched.Timezone); err != nil || sched.Timezone == "Local" {
					return nil, refusePlan("invalid_schedule", http.StatusBadRequest,
						"%q is not a time zone. Use an IANA name such as Europe/Berlin.", sched.Timezone)
				}
			}
			sched.Description = describeSchedule(sched.Type, sched.spec())
			plan.Schedule = &sched
		}
	}

	if ns := strings.TrimSpace(in.DestinationNamespace); ns != "" {
		if reason := naming.ValidateNamespace(ns); reason != "" {
			return nil, refusePlan("invalid_namespace", http.StatusBadRequest,
				"destination_namespace %q is not valid: %s", ns, reason)
		}
		plan.DestinationNamespace = ns
	}

	requested := uniqueTrimmed(in.Tables)
	if len(requested) > maxPlanTables {
		return nil, refusePlan("too_many_tables", http.StatusBadRequest,
			"A plan can name at most %d tables; this one names %d.", maxPlanTables, len(requested))
	}
	plan.Tables = []string{}
	if len(requested) == 0 {
		plan.Notes = append(plan.Notes, "No tables were named, so rsync.ai asks which ones to copy when the pipeline first runs.")
	} else {
		// The same check a chat create makes: the schema cache, else a live
		// read of the source. A name in more than one schema is left for the
		// person to choose; it is never picked for them.
		qualified, missing, ambiguous, ok := validateAndQualifySelectedTablesFromCache(ctx, srcID, requested)
		if !ok {
			qualified, missing, ambiguous, ok = qualifyNamedTablesFromSource(ctx, ws, srcID, userID, requested)
		}
		if ok {
			plan.setCheckedTables(qualified, missing, ambiguous)
		} else {
			plan.TablesChecked = false
			plan.Tables = append(plan.Tables, requested...)
			plan.Notes = append(plan.Notes,
				"rsync.ai couldn't read the source's table list, so the tables are checked when the pipeline is created; any it can't match are asked about when it first runs.")
		}
	}

	if plan.SyncMode == "cdc" && chat.NormalizeConnectorName(plan.Source.ConnectorType) == "postgresql" {
		plan.Notes = append(plan.Notes,
			"Creating this pipeline sets up a publication and a replication slot on the PostgreSQL source.")
	}

	raw, err := json.Marshal(plan.body())
	if err != nil {
		return nil, fmt.Errorf("marshal pipeline plan: %w", err)
	}
	var tokenID *string
	if actor.Channel == services.ChannelAgent {
		tokenID = nullString(c.GetString(ctxTokenID))
	}
	if err := database.QueryRowContext(ctx, `
		INSERT INTO pipeline_plans (
			workspace_id, created_by, actor_kind, token_id,
			source_connection_id, destination_connection_id, plan, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW() + ($8 * INTERVAL '1 second'))
		RETURNING id, created_at, expires_at
	`, ws, userID, plan.ProposedBy, tokenID, srcID, dstID, string(raw), int(pipelinePlanTTL.Seconds()),
	).Scan(&plan.ID, &plan.CreatedAt, &plan.ExpiresAt); err != nil {
		return nil, fmt.Errorf("store pipeline plan: %w", err)
	}
	plan.Status = planStatusProposed
	plan.URL = pipelinePlanURL(plan.ID)

	logAudit(c, "propose_pipeline", "pipeline_plan", plan.ID, map[string]interface{}{
		"source_connection_id":      srcID,
		"destination_connection_id": dstID,
		"sync_mode":                 plan.SyncMode,
		"tables":                    len(plan.Tables),
		"channel":                   string(actor.Channel),
	})
	return plan, nil
}

// setCheckedTables records what the source said about the named tables: the
// ones it matched, the ones it lacks, and each name it has in more than one
// schema as a choice for the person, never picked.
func (p *PipelinePlan) setCheckedTables(qualified, missing []string, ambiguous map[string][]string) {
	p.Tables = append(p.Tables, qualified...)
	p.MissingTables = missing
	names := make([]string, 0, len(ambiguous))
	for n := range ambiguous {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p.TableChoices = append(p.TableChoices, PlanTableChoice{Name: n, Options: ambiguous[n]})
	}
	if len(missing) > 0 {
		p.Notes = append(p.Notes, "Not found in the source, so left out: "+backtickList(missing)+".")
	}
}

// uniqueTrimmed drops blanks and repeats, keeping the first spelling in order.
func uniqueTrimmed(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	return out
}

// loadPipelinePlan reads a plan its creator owns in the workspace; any other
// plan reads as not found. The row's columns are the truth for id, status and
// times; an unapplied plan past expires_at reads as expired.
func loadPipelinePlan(c *gin.Context, database *sql.DB, id, workspaceID, userID string) (*PipelinePlan, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, sql.ErrNoRows
	}
	var (
		raw       []byte
		status    string
		kind      string
		appliedID sql.NullString
		plan      PipelinePlan
	)
	err := database.QueryRowContext(c.Request.Context(), `
		SELECT plan, status, actor_kind, applied_pipeline_id, created_at, expires_at
		FROM pipeline_plans
		WHERE id = $1 AND workspace_id = $2 AND created_by = $3
	`, id, workspaceID, userID).Scan(&raw, &status, &kind, &appliedID, &plan.CreatedAt, &plan.ExpiresAt)
	if err != nil {
		return nil, err
	}
	// A row stored before the body split holds the envelope too; its keys are
	// not body fields, so they are dropped here and the columns stand.
	var body pipelinePlanBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("read pipeline plan: %w", err)
	}
	plan.setBody(body)
	plan.ID, plan.ProposedBy = id, kind
	plan.AppliedPipelineID = appliedID.String
	plan.Status = status
	if status == planStatusProposed && !time.Now().Before(plan.ExpiresAt) {
		plan.Status = planStatusExpired
	}
	if plan.Tables == nil {
		plan.Tables = []string{}
	}
	plan.URL = pipelinePlanURL(id)
	return &plan, nil
}

// GetPipelinePlan returns a plan to the person it was proposed for.
// GET /api/v1/pipeline-plans/:id
func GetPipelinePlan(c *gin.Context) {
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	plan, err := loadPipelinePlan(c, database, c.Param("id"), ws, userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pipeline plan not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[pipeline_plans] failed to load plan")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pipeline plan"})
		return
	}
	c.JSON(http.StatusOK, plan)
}

// applyPipelinePlanRequest is the person's table pick. Empty means the plan's
// own tables.
type applyPipelinePlanRequest struct {
	Tables []string `json:"tables"`
}

// planTablesToApply is the table list a plan is created with: the person's
// pick, which may only name tables the plan holds or offers as a choice; or,
// with no pick, the plan's own tables, refused while a choice is open.
func planTablesToApply(plan *PipelinePlan, picked []string) ([]string, *PlanRefusal) {
	picked = uniqueTrimmed(picked)
	if len(picked) == 0 {
		if len(plan.TableChoices) > 0 {
			names := make([]string, 0, len(plan.TableChoices))
			for _, ch := range plan.TableChoices {
				names = append(names, ch.Name)
			}
			return nil, refusePlan("choose_tables", http.StatusConflict,
				"Pick which table you mean for %s: each is in more than one schema.", backtickList(names))
		}
		return plan.Tables, nil
	}
	allowed := make(map[string]bool, len(plan.Tables))
	for _, t := range plan.Tables {
		allowed[t] = true
	}
	for _, ch := range plan.TableChoices {
		for _, o := range ch.Options {
			allowed[o] = true
		}
	}
	for _, t := range picked {
		if !allowed[t] {
			return nil, refusePlan("table_not_in_plan", http.StatusBadRequest,
				"`%s` is not one of this plan's tables. Ask for a new plan to add it.", t)
		}
	}
	return picked, nil
}

// ApplyPipelinePlan creates the pipeline a plan describes, as the signed-in
// person who asked for it. A plan is applied once.
// POST /api/v1/pipeline-plans/:id/apply
func ApplyPipelinePlan(c *gin.Context) {
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	var body applyPipelinePlanRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	actor := restActor(c, userID, ws)
	if err := services.Authorize(actor, services.ActionPipelineCreate); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only a workspace member, admin or owner can create pipelines."})
		return
	}

	planID := c.Param("id")
	plan, err := loadPipelinePlan(c, database, planID, ws, userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pipeline plan not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[pipeline_plans] failed to load plan")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load pipeline plan"})
		return
	}
	scheduleOnly := plan.Schedule != nil
	if !scheduleOnly {
		if err := services.Authorize(actor, services.ActionPipelineRun); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only a workspace member, admin or owner can run pipelines."})
			return
		}
	}
	if refusal := planNotOpen(plan); refusal != nil {
		writePlanRefusal(c, refusal, plan)
		return
	}
	tables, refusal := planTablesToApply(plan, body.Tables)
	if refusal != nil {
		writePlanRefusal(c, refusal, plan)
		return
	}

	// Claim the plan before creating anything, so two clicks cannot create two
	// pipelines. A create that fails before a pipeline exists puts it back.
	var claimed string
	err = database.QueryRowContext(c.Request.Context(), `
		UPDATE pipeline_plans SET status = 'applied', applied_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND created_by = $3
		  AND status = 'proposed' AND expires_at > NOW()
		RETURNING id
	`, planID, ws, userID).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		// Applied or expired since it was read.
		if now, lerr := loadPipelinePlan(c, database, planID, ws, userID); lerr == nil {
			if refusal := planNotOpen(now); refusal != nil {
				writePlanRefusal(c, refusal, now)
				return
			}
		}
		c.JSON(http.StatusConflict, gin.H{"error": "This plan changed while it was being created. Reload it.", "code": "plan_changed"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[pipeline_plans] failed to claim plan")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create the pipeline"})
		return
	}

	srcType := chat.NormalizeConnectorName(plan.Source.ConnectorType)
	dstType := chat.NormalizeConnectorName(plan.Destination.ConnectorType)
	request := fmt.Sprintf("Sync %s to %s", srcType, dstType)
	if plan.SyncMode == "cdc" {
		request = fmt.Sprintf("Stream CDC from %s to %s", srcType, dstType)
	}
	proposer := "Ask rsync.ai"
	if plan.ProposedBy == actorKindAgent {
		proposer = "an AI agent"
	}
	origin := pipelineOrigin{
		actor:       actor,
		name:        fmt.Sprintf("%s to %s", plan.Source.Name, plan.Destination.Name),
		description: fmt.Sprintf("Created from a plan proposed by %s", proposer),
		refuse:      func(err error, _ string) error { return err },
		audit:       map[string]interface{}{"plan_id": planID, "proposed_by": plan.ProposedBy},
	}
	traceID := telemetry.TraceIDFromContext(c.Request.Context())
	pipelineID, _, note, createErr := createAndRunPipelineAs(c, origin, request, userID, traceID,
		plan.Source.ID, plan.Destination.ID, plan.SyncMode, plan.CDCMode, "", nlTransformSpec{},
		scheduleOnly, tables, plan.DestinationNamespace)
	if createErr != nil && pipelineID == "" {
		if _, rerr := database.ExecContext(c.Request.Context(), `
			UPDATE pipeline_plans SET status = 'proposed', applied_at = NULL
			WHERE id = $1 AND status = 'applied' AND applied_pipeline_id IS NULL
		`, planID); rerr != nil {
			log.WithError(rerr).WithField("plan_id", planID).Error("[pipeline_plans] failed to put back a plan whose create failed")
		}
		if errors.Is(createErr, services.ErrForbidden) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Your role in this workspace can't create or run pipelines."})
			return
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": createErr.Error(), "code": "create_failed"})
		return
	}

	resp := gin.H{"pipeline_id": pipelineID, "status": "running"}
	var warnings []string
	if createErr != nil {
		// The pipeline exists but its first run did not start.
		log.WithError(createErr).WithField("pipeline_id", pipelineID).Warn("[pipeline_plans] pipeline created, first run not started")
		warnings = append(warnings, fmt.Sprintf("The pipeline was created, but its first run did not start: %v.", createErr))
	}
	if scheduleOnly {
		resp["status"] = "scheduled"
		resp["schedule"] = plan.Schedule.Description
		if err := attachScheduleForChat(c.Request.Context(), database, pipelineID, userID, plan.Schedule.Type, plan.Schedule.spec()); err != nil {
			log.WithError(err).WithField("pipeline_id", pipelineID).Warn("[pipeline_plans] failed to attach schedule")
			warnings = append(warnings, fmt.Sprintf("The pipeline was created, but its schedule could not be set up: %v. Add one from the pipeline's settings.", err))
		}
	}
	if _, err := database.ExecContext(c.Request.Context(),
		`UPDATE pipeline_plans SET applied_pipeline_id = $2 WHERE id = $1`, planID, pipelineID); err != nil {
		log.WithError(err).WithField("plan_id", planID).Warn("[pipeline_plans] failed to record the plan's pipeline")
	}
	if note != "" {
		resp["note"] = note
	}
	if len(warnings) > 0 {
		resp["warning"] = strings.Join(warnings, " ")
	}
	c.JSON(http.StatusCreated, resp)
}

// planNotOpen refuses a plan that was already applied or has expired.
func planNotOpen(plan *PipelinePlan) *PlanRefusal {
	switch plan.Status {
	case planStatusApplied:
		return refusePlan("already_applied", http.StatusConflict, "This plan was already used to create a pipeline.")
	case planStatusExpired:
		return refusePlan("expired", http.StatusGone,
			"This plan expired 24 hours after it was proposed. Ask rsync.ai or your agent can propose a new one.")
	}
	return nil
}

func writePlanRefusal(c *gin.Context, r *PlanRefusal, plan *PipelinePlan) {
	body := gin.H{"error": r.Message, "code": r.Code}
	if r.Code == "already_applied" && plan != nil && plan.AppliedPipelineID != "" {
		body["applied_pipeline_id"] = plan.AppliedPipelineID
	}
	c.JSON(r.Status, body)
}
