package workflows

// storedProgress is the pipeline_progress row's progress as StateUpdateActivity
// read it, before its write. ok is false when there is no row.
type storedProgress struct {
	executionID          string
	percent, step, total int
	ok                   bool
}

// progressForUpdate is the progress StateUpdateActivity persists for update.
//
// The counts come from the update's execution_plan. An update with no plan (the
// pause and resume writes) says nothing about progress, so the same run keeps
// what it had: writing deriveProgressFromExecutionPlan's 0% / 0 of 7 there is how
// a one-step chat run came to show "7/7 steps". A row from another run, or no
// row, still starts from the 7-step default, as the projector's new-row path does.
func progressForUpdate(status string, meta map[string]interface{}, executionID string, stored storedProgress) (percent, step, total int) {
	if !hasExecutionPlanStages(meta) && stored.ok && executionID != "" && stored.executionID == executionID {
		percent, step = normalizeTerminalProgress(status, stored.percent, stored.step, stored.total)
		return percent, step, stored.total
	}
	percent, step, total = deriveProgressFromExecutionPlan(meta)
	if total == 0 {
		total = 7
	}
	percent, step = normalizeTerminalProgress(status, percent, step, total)
	return percent, step, total
}

func hasExecutionPlanStages(meta map[string]interface{}) bool {
	plan, ok := meta["execution_plan"].(map[string]interface{})
	if !ok {
		return false
	}
	_, ok = plan["stages"].([]interface{})
	return ok
}
