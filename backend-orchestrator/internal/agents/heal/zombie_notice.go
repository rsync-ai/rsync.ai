package heal

import (
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
)

// zombieNoticeTopic is where the api-gateway notifier reads pipeline notices from.
const zombieNoticeTopic = "rsync.notifications"

// NotificationProducer is the slice of kafka.Manager the zombie sweep needs. Produce
// prefixes the topic with KAFKA_TOPIC_PREFIX, as every other producer of rsync.notifications
// does.
type NotificationProducer interface {
	Produce(topic string, key, value []byte) error
}

// NotifyRunFailuresVia sends a PIPELINE_RUN_FAILED notice for every run either zombie sweep
// reaps from now on.
func (w *HealWorker) NotifyRunFailuresVia(p NotificationProducer) {
	w.Notifications = p
	if w.ZombieSweeper != nil {
		w.ZombieSweeper.Notifications = p
	}
}

// zombieRunFailedNotification is the notice for one reaped run: the same PIPELINE_RUN_FAILED
// envelope the temporal adapter sends when a run fails (pipeline_failure_notification.go),
// because to the person who owns the pipeline it is the same event. The adapter cannot send
// it for these runs: a run is only ever reaped because its workflow never got as far as that
// alert.
//
// dedup_subject is the execution id, as in the adapter's, so a second reaped run of the same
// pipeline inside the notifier's dedup window is a second notice rather than none.
func zombieRunFailedNotification(pipelineID, executionID string, age time.Duration, now time.Time) ([]byte, error) {
	se := &diagnose.StructuredError{
		FailureType: diagnose.FailureTypeSystemError,
		Code:        "PIPELINE_RUN_FAILED",
		Severity:    diagnose.SeverityError,
		Audience:    diagnose.AudienceDeveloper,
		UserMessage: fmt.Sprintf("This run had still not finished %s after it started, so rsync.ai "+
			"marked it as failed. Check the pipeline, and run it again if the data it was moving "+
			"is still needed.", zombieAgeText(age)),
		InternalMessage: fmt.Sprintf("healer zombie sweep: execution stuck running >%s with no end_time", age),
		PipelineID:      pipelineID,
		ExecutionID:     executionID,
		OccurredAt:      now.UTC().Format(time.RFC3339),
		DedupSubject:    executionID,
	}
	return json.Marshal(map[string]interface{}{
		"type":        "structured_error_notification",
		"pipeline_id": pipelineID,
		"timestamp":   now.UTC().Format(time.RFC3339),
		"action_url":  "/pipelines/" + pipelineID,
		"error":       se,
		"message":     se.UserMessage,
	})
}

// zombieAgeText renders the sweep's age bound for the notice: hours from two hours up,
// minutes below.
func zombieAgeText(age time.Duration) string {
	if age >= 2*time.Hour {
		return fmt.Sprintf("%d hours", int(age.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(age.Minutes()))
}

// sendZombieRunFailed sends the notice for one reaped run. Best effort: the reap has already
// committed, so a failure here is logged and the sweep goes on to the next row.
func sendZombieRunFailed(p NotificationProducer, pipelineID, executionID string, age time.Duration, now time.Time) {
	if p == nil || pipelineID == "" {
		return
	}
	body, err := zombieRunFailedNotification(pipelineID, executionID, age, now)
	if err == nil {
		err = p.Produce(zombieNoticeTopic, []byte(pipelineID), body)
	}
	if err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).WithField("execution_id", executionID).
			Warn("healer: could not send the run-failed notice for a reaped execution")
	}
}
