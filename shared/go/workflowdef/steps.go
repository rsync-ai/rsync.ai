package workflowdef

import (
	"fmt"
	"strings"
)

// Step-run statuses (workflow_step_runs.status). A step is running while its
// own work runs and waiting while it waits on another run (a pipeline, a
// model) or a timer; the others are final.
const (
	StatusRunning   = "running"
	StatusWaiting   = "waiting"
	StatusSucceeded = "succeeded"
	// StatusWarned is a refresh_model step whose model ran and whose checks
	// warned: the step passed.
	StatusWarned  = "warned"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	StatusStopped = "stopped"
)

// StepOutcome is how a finished step ended, as its children's rules read it.
type StepOutcome struct {
	Status string
	// Case is the condition a condition step took, 0-based, -1 for none.
	Case int
}

func passed(status string) bool { return status == StatusSucceeded || status == StatusWarned }

// Gate decides a step from its parents' outcomes. ready is false while a
// parent has not finished. Once ready, run says whether the step runs; a step
// that does not is skipped, and note says why. A root is ready at once.
//
// Rules: all_success runs when every parent passed (succeeded or warned);
// all_done runs once every parent finished, however; any_failed runs when a
// parent failed. A skipped parent has not passed and has not failed, so a
// skip carries down all_success steps and stops at all_done. A condition's
// send runs only when the condition took its case.
func Gate(s Step, done map[string]StepOutcome) (ready, run bool, note string) {
	if len(s.After) == 0 {
		return true, true, ""
	}
	for _, p := range s.After {
		if _, ok := done[p]; !ok {
			return false, false, ""
		}
	}
	var notPassed, failed []string
	for _, p := range s.After {
		o := done[p]
		if !passed(o.Status) {
			notPassed = append(notPassed, p)
		}
		if o.Status == StatusFailed {
			failed = append(failed, p)
		}
	}
	rule := s.Rule
	if rule == "" {
		rule = RuleAllSuccess
	}
	switch rule {
	case RuleAllDone:
		return true, true, ""
	case RuleAnyFailed:
		if len(failed) > 0 {
			return true, true, ""
		}
		return true, false, fmt.Sprintf("It runs only when a step before it fails, and none of %s did.",
			strings.Join(s.After, ", "))
	}
	if len(notPassed) > 0 {
		return true, false, fmt.Sprintf("It runs only when every step before it passes, and %s did not.",
			strings.Join(notPassed, ", "))
	}
	if s.Type == StepSend && s.Case != nil {
		took := done[s.After[0]].Case
		if took != *s.Case {
			if took < 0 {
				return true, false, "No condition was true."
			}
			return true, false, fmt.Sprintf("Condition %d was true, not this one.", took+1)
		}
	}
	return true, true, ""
}

// RunStatus is a run's status from its steps': stopped when someone stopped
// it, else failed when a step failed (a send not marked required may fail
// without failing the run), else succeeded. statuses holds each step's final
// status; a step missing from it never ran.
func RunStatus(d Definition, statuses map[string]string, stopRequested bool) string {
	if stopRequested {
		return StatusStopped
	}
	for _, s := range d.Steps {
		if statuses[s.ID] != StatusFailed {
			continue
		}
		if s.Type == StepSend && !s.Required {
			continue
		}
		return StatusFailed
	}
	return StatusSucceeded
}
