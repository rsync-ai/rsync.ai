package workers

import (
	"context"
	"database/sql"
	"net/http"

	log "github.com/sirupsen/logrus"
)

// The workspace a /plan call is made for.
//
// The planner service logs every model call in an llm_egress line and names its
// workspace from the request's X-Workspace-ID (llm-service src/egress/scope.py).
// The intent and planner workers set it from the task; without it each of the
// planner's lines read workspace=-.

const planWorkspaceHeader = "X-Workspace-ID"

// taskWorkspaceID is the workspace a task runs for: the one the task names,
// else its pipeline's. "" when neither is known.
func taskWorkspaceID(ctx context.Context, db *sql.DB, task Task) string {
	for _, m := range []map[string]interface{}{task.Payload, task.Context} {
		if ws := getStringFromMap(m, "workspace_id"); ws != "" {
			return ws
		}
	}
	ws, err := pipelineWorkspaceID(ctx, db, task.PipelineID)
	if err != nil {
		log.WithError(err).WithField("pipeline_id", task.PipelineID).
			Warn("Could not read workspace_id for pipeline; its planner egress lines will say workspace=-")
	}
	return ws
}

// pipelineWorkspaceID reads the tenant a pipeline belongs to
// (pipelines.workspace_id, NOT NULL since migration 069). "" with no error
// when the handle or the id is missing.
func pipelineWorkspaceID(ctx context.Context, db *sql.DB, pipelineID string) (string, error) {
	if db == nil || pipelineID == "" {
		return "", nil
	}
	var workspaceID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT workspace_id FROM pipelines WHERE id = $1`, pipelineID).Scan(&workspaceID); err != nil {
		return "", err
	}
	return workspaceID.String, nil
}

// stampPlanWorkspace sets X-Workspace-ID on a /plan request when the workspace
// is known.
func stampPlanWorkspace(req *http.Request, workspaceID string) {
	if workspaceID != "" {
		req.Header.Set(planWorkspaceHeader, workspaceID)
	}
}
