package handlers

// Workflow changes (migration 133): a change to a workflow's branches that Ask
// rsync.ai or an agent token proposed (the plan_workflow_change tool).
// Proposing changes nothing. The server checks the new branches as an edit
// would, the query run once included, and stores them; the person who asked
// opens the change in rsync.ai, sees the workflow before and after, and clicks
// Apply. That click is a REST call with their session, checked as
// workflow.write, that saves through the same path as an edit
// (saveWorkflowEdit). A change proposed against an earlier version of the
// workflow is never applied over a newer one.
//
// A change edits only the branches: their conditions, messages and channels,
// added, removed or reordered. The name, the trigger and the query stay as the
// person last saved them; they edit those in the workflow's editor. No model
// is shown a workflow's SQL or its messages (ListWorkflowsAs returns them to
// Go code that shapes them away).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"
)

const (
	// workflowChangeTTL is how long a change can be applied after it was proposed.
	workflowChangeTTL = 24 * time.Hour
	// maxOpenWorkflowChanges caps the unapplied, unexpired changes one person
	// has in a workspace, so a looping agent cannot fill the table.
	maxOpenWorkflowChanges = 20
	// workflowChangeSummaryMax caps the model's one-line summary of a change.
	workflowChangeSummaryMax = 300
	// WorkflowsForModelMax caps the workflows list_workflows shows;
	// ListWorkflowsAs reads one more, so the tool can say the list was cut.
	WorkflowsForModelMax = 50
	// changeStatusStale is a proposed change whose workflow was saved since:
	// it can no longer be applied.
	changeStatusStale = "stale"
)

// WorkflowChange is a proposed change to a workflow's branches, with the
// workflow as it is now (Current) and as the change would leave it (Proposed).
// Status is proposed, applied, expired or stale.
type WorkflowChange struct {
	ID           string    `json:"id"`
	WorkflowID   string    `json:"workflow_id"`
	WorkflowName string    `json:"workflow_name"`
	Summary      string    `json:"summary"`
	Status       string    `json:"status"`
	ProposedBy   string    `json:"proposed_by"`
	Current      *Workflow `json:"current"`
	Proposed     *Workflow `json:"proposed"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	URL          string    `json:"url"`
	// QueryUnchecked is set only on the change ProposeWorkflowChange just
	// stored, when the query was still running at the check's deadline.
	QueryUnchecked bool `json:"-"`

	baseUpdatedAt  time.Time
	definition     workflow.Definition
	nodeChannelIDs map[string][]string
}

// workflowChangeURL is the workflow's page with the change open on it.
func workflowChangeURL(workflowID, changeID string) string {
	return workflowURL(workflowID) + "?change=" + changeID
}

// ProposeWorkflowChange checks a change to a workflow's branches and stores it
// for actor's user to apply. in is the workflow's branches as they should be
// after the change, top to bottom; a branch left out is removed. It changes no
// workflow. Only Ask rsync.ai and an agent token propose changes; a person
// edits the workflow directly.
func ProposeWorkflowChange(c *gin.Context, actor services.Actor, workflowID string, in []WorkflowBranchInput,
	summary string) (*WorkflowChange, error) {
	if actor.Channel != services.ChannelAssistant && actor.Channel != services.ChannelAgent {
		return nil, refusePlan("forbidden", http.StatusForbidden, "Only Ask rsync.ai or an agent token proposes a workflow change.")
	}
	if err := services.Authorize(actor, services.ActionWorkflowWrite); err != nil {
		return nil, workflowForbidden(actor)
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	ws, userID := actor.WorkspaceID, actor.UserID

	workflowID = strings.TrimSpace(workflowID)
	w, err := loadWorkflow(ctx, database, ws, workflowID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refusePlan("workflow_not_found", http.StatusNotFound,
			"No workflow %s in this workspace. list_workflows gives each workflow's id.", workflowID)
	}
	if err != nil {
		return nil, err
	}
	if len(in) == 0 {
		return nil, refusePlan("invalid_branches", http.StatusBadRequest,
			"Send the workflow's conditions as they should be after the change, top to bottom: {\"keep\": n} "+
				"for condition n as it is, a new one to add one. A condition left out is removed.")
	}
	summary = workflow.CapRunes(strings.Join(strings.Fields(summary), " "), workflowChangeSummaryMax)
	if w.Definition.ConditionStep() == nil {
		return nil, refusePlan("invalid_branches", http.StatusUnprocessableEntity,
			"This workflow has no conditions, so a change has none to edit. Draft the workflow again instead.")
	}

	def := w.Definition.Clone()
	branches, refusal := draftBranches(in, w)
	if refusal != nil {
		return nil, refusal
	}
	def.ReplaceBranches(branches)
	if _, _, refusal, err = validateWorkflowDefinition(ctx, database, ws, userID, &def, actor.Role, true); err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	channels, refusal, err := resolveBranchChannels(ctx, database, ws, in, def, w)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	if len(workflowEditChanges(w, w.Name, def, channels)) == 0 {
		return nil, refusePlan("no_change", http.StatusBadRequest,
			"These are the workflow's conditions as they are now, so there is no change to propose.")
	}

	var open int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_changes
		WHERE workspace_id = $1 AND created_by = $2 AND status = 'proposed' AND expires_at > NOW()
	`, ws, userID).Scan(&open); err != nil {
		return nil, fmt.Errorf("count open workflow changes: %w", err)
	}
	if open >= maxOpenWorkflowChanges {
		return nil, refusePlan("too_many_changes", http.StatusTooManyRequests,
			"You have %d workflow changes waiting to be applied. Apply them or let them expire (after 24 hours) "+
				"before proposing more.", open)
	}

	// The query is the workflow's own; a change only moves the conditions
	// and slots it is checked against. A refusal goes to a model: the
	// database's error scrubbed of literals, or the result's column names.
	checked, qerr, inv, err := runWorkflowCheck(ctx, database, ws, def, actor.Role)
	if err != nil {
		return nil, err
	}
	if qerr != nil {
		return nil, refusePlan("query_failed", http.StatusUnprocessableEntity,
			"rsync.ai ran the workflow's query once to check the change and it failed, so nothing was stored: %s. "+
				"A change cannot edit the query; tell the person, who can fix it in the workflow's editor.",
			llmscrub.ScrubMax(qerr.Error(), workflowErrorMax))
	}
	if inv != nil {
		r := refusePlan("invalid_"+inv.Field, http.StatusUnprocessableEntity, "%s", inv.Message)
		r.Branch, r.Step = inv.Branch, inv.Step
		return nil, r
	}

	byNode := map[string][]string{}
	for _, s := range def.SendSteps() {
		byNode[s.ID] = []string{}
	}
	for _, ch := range channels {
		byNode[ch.NodeID] = append(byNode[ch.NodeID], ch.ID)
	}
	rawDef, err := json.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow change: %w", err)
	}
	rawChannels, err := json.Marshal(byNode)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow change channels: %w", err)
	}
	var tokenID *string
	if actor.Channel == services.ChannelAgent {
		tokenID = nullString(c.GetString(ctxTokenID))
	}
	change := &WorkflowChange{
		WorkflowID:     w.ID,
		WorkflowName:   w.Name,
		Summary:        summary,
		Status:         planStatusProposed,
		ProposedBy:     actorKindFor(actor.Channel),
		Current:        w,
		Proposed:       proposedWorkflow(w, def, channels),
		QueryUnchecked: !checked,
		baseUpdatedAt:  w.UpdatedAt,
		definition:     def,
		nodeChannelIDs: byNode,
	}
	if err := database.QueryRowContext(ctx, `
		INSERT INTO workflow_changes (
			workspace_id, workflow_id, created_by, actor_kind, token_id, base_updated_at,
			definition, node_channel_ids, summary, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW() + ($10 * INTERVAL '1 second'))
		RETURNING id, created_at, expires_at
	`, ws, w.ID, userID, change.ProposedBy, tokenID, w.UpdatedAt, string(rawDef), string(rawChannels), summary,
		int(workflowChangeTTL.Seconds())).Scan(&change.ID, &change.CreatedAt, &change.ExpiresAt); err != nil {
		return nil, fmt.Errorf("store workflow change: %w", err)
	}
	change.URL = workflowChangeURL(w.ID, change.ID)

	logAudit(c, "propose_workflow_change", "workflow_change", change.ID, map[string]interface{}{
		"workflow_id":   w.ID,
		"changed":       workflowEditChanges(w, w.Name, def, channels),
		"branches":      len(def.Branches()),
		"channel":       string(actor.Channel),
		"query_checked": checked,
	})
	return change, nil
}

// proposedWorkflow is w as def and channels would leave it, for the person to
// compare with w. It has no last run: it has not run.
func proposedWorkflow(w *Workflow, def workflow.Definition, channels []WorkflowChannel) *Workflow {
	p := *w
	p.Definition = def
	p.Channels = append([]WorkflowChannel{}, channels...)
	p.LastRun = nil
	p.describeBranches()
	return &p
}

// loadWorkflowChange reads a change its creator owns in the workspace; any
// other change reads as not found. An unapplied change past expires_at reads
// as expired. Current and Proposed are left for the caller.
func loadWorkflowChange(ctx context.Context, database *sql.DB, id, ws, userID string) (*WorkflowChange, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, sql.ErrNoRows
	}
	var (
		ch            WorkflowChange
		rawDef, rawCh []byte
	)
	err := database.QueryRowContext(ctx, `
		SELECT workflow_id, actor_kind, base_updated_at, definition, node_channel_ids, summary, status,
		       created_at, expires_at
		FROM workflow_changes
		WHERE id = $1 AND workspace_id = $2 AND created_by = $3
	`, id, ws, userID).Scan(&ch.WorkflowID, &ch.ProposedBy, &ch.baseUpdatedAt, &rawDef, &rawCh, &ch.Summary, &ch.Status,
		&ch.CreatedAt, &ch.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawDef, &ch.definition); err != nil {
		return nil, fmt.Errorf("read workflow change: %w", err)
	}
	if err := json.Unmarshal(rawCh, &ch.nodeChannelIDs); err != nil {
		return nil, fmt.Errorf("read workflow change channels: %w", err)
	}
	ch.ID = id
	if ch.Status == planStatusProposed && !time.Now().Before(ch.ExpiresAt) {
		ch.Status = planStatusExpired
	}
	ch.URL = workflowChangeURL(ch.WorkflowID, id)
	return &ch, nil
}

// workflowChangeNotOpen refuses a change that was already applied, has
// expired, or whose workflow was saved since it was proposed.
func workflowChangeNotOpen(ch *WorkflowChange) *PlanRefusal {
	switch ch.Status {
	case planStatusApplied:
		return refusePlan("already_applied", http.StatusConflict, "This change was already applied.")
	case planStatusExpired:
		return refusePlan("expired", http.StatusGone,
			"This change expired 24 hours after it was proposed. Ask rsync.ai again for a new one.")
	case changeStatusStale:
		return refusePlan("workflow_changed", http.StatusConflict,
			"The workflow was changed since Ask rsync.ai proposed this change, so it was not applied. Ask again "+
				"for the change on the workflow as it is now.")
	}
	return nil
}

// loadWorkflowChangeView reads a change with the workflow as it is now and as
// the change would leave it. A proposed change whose workflow was saved since
// reads as stale. A channel deleted since is left out of the view; applying
// refuses it.
func loadWorkflowChangeView(ctx context.Context, database *sql.DB, id, ws, userID string) (*WorkflowChange, error) {
	ch, err := loadWorkflowChange(ctx, database, id, ws, userID)
	if err != nil {
		return nil, err
	}
	w, err := loadWorkflow(ctx, database, ws, ch.WorkflowID)
	if err != nil {
		return nil, err
	}
	ch.Current, ch.WorkflowName = w, w.Name
	if ch.Status == planStatusProposed && !w.UpdatedAt.Equal(ch.baseUpdatedAt) {
		ch.Status = changeStatusStale
	}
	var ids []string
	for _, list := range ch.nodeChannelIDs {
		ids = append(ids, list...)
	}
	byID := map[string]WorkflowChannel{}
	if len(ids) > 0 {
		rows, err := database.QueryContext(ctx, `
			SELECT id, name, kind FROM delivery_channels
			WHERE workspace_id = $1 AND id = ANY(string_to_array($2, ',')::uuid[])
		`, ws, strings.Join(ids, ","))
		if err != nil {
			return nil, fmt.Errorf("load delivery channels: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c WorkflowChannel
			if err := rows.Scan(&c.ID, &c.Name, &c.Kind); err != nil {
				return nil, err
			}
			byID[strings.ToLower(c.ID)] = c
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var channels []WorkflowChannel
	for _, s := range ch.definition.SendSteps() {
		for _, id := range ch.nodeChannelIDs[s.ID] {
			if c, ok := byID[strings.ToLower(id)]; ok {
				c.NodeID = s.ID
				channels = append(channels, c)
			}
		}
	}
	ch.Proposed = proposedWorkflow(w, ch.definition, channels)
	return ch, nil
}

// GetWorkflowChange returns a change to the person it was proposed for.
// GET /api/v1/workflow-changes/:id
func GetWorkflowChange(c *gin.Context) {
	userID, ws, role, database, ok := workflowRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ch, err := loadWorkflowChangeView(c.Request.Context(), database, c.Param("id"), ws, userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow change not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[workflow_changes] load failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the workflow change"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"change": ch, "can_manage": role.Meets(security.WSAdmin)})
}

// ApplyWorkflowChange saves a proposed change as an edit of its workflow, as
// the signed-in person it was proposed for. It makes every check an edit
// makes, the query run included, and is refused when the workflow was saved
// since the change was proposed. A change is applied once.
// POST /api/v1/workflow-changes/:id/apply
func ApplyWorkflowChange(c *gin.Context) {
	userID, ws, role, database, ok := workflowRequest(c, security.WSAdmin)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	ch, err := loadWorkflowChange(ctx, database, c.Param("id"), ws, userID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workflow change not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[workflow_changes] load failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the workflow change"})
		return
	}
	if refusal := workflowChangeNotOpen(ch); refusal != nil {
		writeWorkflowRefusal(c, refusal)
		return
	}
	w, err := loadWorkflow(ctx, database, ws, ch.WorkflowID)
	if err != nil {
		writeWorkflowLoadError(c, err)
		return
	}
	if !w.UpdatedAt.Equal(ch.baseUpdatedAt) {
		ch.Status = changeStatusStale
		writeWorkflowRefusal(c, workflowChangeNotOpen(ch))
		return
	}

	def := ch.definition
	conn, pipeline, refusal, err := validateWorkflowDefinition(ctx, database, ws, userID, &def, role, false)
	if err == nil && refusal == nil {
		var channels []WorkflowChannel
		channels, refusal, err = sendChannelsByID(ctx, database, ws, def, ch.nodeChannelIDs, nil, nil)
		if err == nil && refusal == nil {
			var checked bool
			checked, refusal, err = checkWorkflowForPerson(ctx, database, ws, def, role, "nothing was applied")
			if err == nil && refusal == nil {
				saveWorkflowEdit(c, database, ws, w, userID, workflowEdit{name: w.Name, def: def, conn: conn,
					pipeline: pipeline, channels: channels, checked: checked, changeID: ch.ID})
				return
			}
		}
	}
	if err != nil {
		log.WithError(err).Error("[workflow_changes] apply check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the change"})
		return
	}
	writeWorkflowRefusal(c, refusal)
}

// ListWorkflowsAs lists the workspace's workflows, newest first and at most
// WorkflowsForModelMax+1 of them, for list_workflows. They hold the SQL and
// the messages: the caller shapes them to names and descriptions before
// anything reaches a model.
func ListWorkflowsAs(c *gin.Context, actor services.Actor) ([]*Workflow, error) {
	if err := services.Authorize(actor, services.ActionWorkflowRead); err != nil {
		return nil, refusePlan("forbidden", http.StatusForbidden,
			"This token lacks the %s scope. Create a token that has it.", services.ScopeMetadataRead)
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	rows, err := database.QueryContext(ctx, workflowSelect+` WHERE w.workspace_id = $1 ORDER BY w.created_at DESC LIMIT $2`,
		actor.WorkspaceID, WorkflowsForModelMax+1)
	if err != nil {
		if isUndefinedTable(err) {
			return []*Workflow{}, nil
		}
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	list := []*Workflow{}
	for rows.Next() {
		w, err := scanWorkflow(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := attachWorkflowExtras(ctx, database, actor.WorkspaceID, list); err != nil {
		return nil, err
	}
	return list, nil
}
