package handlers

import (
	"time"

	"api-gateway/internal/notifier"
)

// publishUserStopNotice tells a pipeline's owner it was stopped: by Stop, which
// ends the pipeline, or by Cancel on one of its runs, which stops it too. Every
// one is told, the owner's own included. A pipeline that stopped and one still
// running look the same from outside, and on a shared workspace the owner is
// often not the one who pressed it.
//
// A streaming CDC pipeline gets its own code, because nothing ended but the
// stream: no run is over, and changes stop arriving until it is started again.
//
// subject is the run's execution id when the stop is about one batch run. The
// temporal adapter's terminal notice for that run (PIPELINE_RUN_STOPPED, keyed by
// the same execution id and link) then lands on this notice's dedup key, so a
// run that ends in both places tells the owner once. A CDC stop passes "": its
// stream row's id is the pipeline's own, the same on every stop, and a second
// Stop inside the dedup window is a new stop, not a repeat.
func publishUserStopNotice(pipelineID, subject string, isCDC bool, message string) {
	code := notifier.CodePipelineRunStopped
	if isCDC {
		code = notifier.CodeCDCPipelineStopped
		subject = ""
	}
	if subject == "" {
		subject = "user-stop|" + time.Now().UTC().Format(time.RFC3339)
	}
	publishRunNotice(runNotice{
		PipelineID:   pipelineID,
		Code:         code,
		Severity:     "warning",
		FailureType:  "config_error",
		Message:      message,
		ActionURL:    "/pipelines/" + pipelineID,
		DedupSubject: subject,
	})
}

// The bodies of a stop notice. Neither names who stopped it: the audit log does.
const (
	stopNoticeMessage   = "The pipeline was stopped from rsync.ai."
	cancelNoticeMessage = "A run was cancelled from rsync.ai, which stopped the pipeline."
)
