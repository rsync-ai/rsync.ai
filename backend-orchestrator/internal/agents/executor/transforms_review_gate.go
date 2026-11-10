package executor

// The transform step for a chat pipeline whose tables were named in the message.
//
// The chat's transform suggestions step (SuggestionsReviewDialog) opens from the
// table-selection pause: pick tables, then review transforms, then data moves.
// Naming the tables in the message ("sync users from pg to bq") pre-selects them
// (KI-NLCHAT-TABLENAME-IGNORED), so the run never paused and the step was never
// shown: data moved with no transforms, and the pipeline's Transforms tab was
// empty. The gateway now marks such a pipeline with
// config.transforms_review_pending, and the first transfer pauses ONCE on the
// same table-selection wait, carrying the named tables so the chat goes straight
// to the transform step. Resuming re-dispatches the transfer, which finds the
// mark gone and moves the data.
//
// The pause sits after planNLTransforms, so a "mask email" in the message is
// already stored as rules when the step opens (the dialog lists them as saved).
// Before it, planNLTransforms would find the rules the step saved and skip, and
// the requested mask would be lost.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// consumeTransformsReview clears the pipeline's transforms_review_pending mark and
// reports whether it was set. Test-and-clear is one statement, so the pause is
// raised at most once: a rerun, a scheduled run or a second dispatch after the
// resume never parks on it again. It is a review step, not a safety control, so
// any failure to read or clear it moves the data as before (masks the message
// asked for are planNLTransforms', which fails closed on its own).
func (a *Agent) consumeTransformsReview(ctx context.Context, pipelineID string) bool {
	if a.db == nil {
		return false
	}
	pipelineID = strings.TrimSpace(pipelineID)
	if !looksLikeUUID(pipelineID) {
		return false
	}
	var id string
	err := a.db.QueryRowContext(ctx, `
		UPDATE pipelines
		SET config = config - 'transforms_review_pending',
		    updated_at = NOW()
		WHERE id = $1
		  AND config->'transforms_review_pending' = 'true'::jsonb
		RETURNING id
	`, pipelineID).Scan(&id)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).
			Warn("transforms review: could not read the pending mark; moving data without the transform step")
		return false
	}
	return true
}

// transformsReviewPause is the one-time table-selection pause that shows the
// chat's transform step for tables the message already named. The named tables
// are both the offered list (so a client that does not know review_transforms_only
// still shows a picker holding exactly them) and preselected_tables (so the chat
// skips the picker and opens the transform step).
func transformsReviewPause(task ExecutorTask, tables []string) ExecutorResponse {
	sourceType := ""
	var cfg map[string]string
	if task.Source != nil {
		sourceType = task.Source.Type
		cfg = task.Source.Config
	}
	// The names are already qualified (qualifySelectedTablesForSource), and the
	// picker sends `schema.name` or `name` back, so the whole name goes in Name.
	offered := make([]TableMetadata, 0, len(tables))
	for _, t := range tables {
		offered = append(offered, TableMetadata{Name: t})
	}
	result := tableSelectionResult(sourceType, cfg, offered, fmt.Sprintf(
		"Review transforms for %d named %s before any data moves.", len(tables), pluralTable(len(tables))))
	result["preselected_tables"] = append([]string(nil), tables...)
	result["review_transforms_only"] = true
	return ExecutorResponse{
		TaskID:     task.TaskID,
		PipelineID: task.PipelineID,
		Status:     "waiting_for_table_selection",
		Error:      "Review transforms before data moves.",
		Result:     result,
	}
}

func pluralTable(n int) string {
	if n == 1 {
		return "table"
	}
	return "tables"
}
