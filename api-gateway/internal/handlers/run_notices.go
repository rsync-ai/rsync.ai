package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Run notices: what the gateway itself tells an owner about a run nobody was
// watching end, or one that was ended by hand. A Stop or Cancel of a pipeline, a
// scheduled run waiting for an answer, and a scheduled model or workflow that
// failed (pipeline_stop_notice.go, pipeline_wait_alert.go, model_run_alert.go,
// workflow_run_alert.go).
//
// Each one goes through the notifier's persist → dedup → bell, Slack and email
// path (notifier.Emitter.Publish), in the background: a slow Slack must never
// hold up, or fail, the Stop or the run that raised it. The words come from the
// notifier's catalog, keyed by the code. The payload carries ids and codes only,
// never an error's text, a row, a count or who pressed the button.

// runNoticePublisher delivers one notice (notifier.Emitter.Publish).
type runNoticePublisher interface {
	Publish(ctx context.Context, raw []byte) error
}

var (
	runNoticeNotifierMu sync.RWMutex
	runNoticeNotifier   runNoticePublisher
)

// SetRunNoticeNotifier wires the publisher for run notices. Unset, every stop,
// wait and failure is still recorded where it always was; nobody is told.
func SetRunNoticeNotifier(p runNoticePublisher) {
	runNoticeNotifierMu.Lock()
	defer runNoticeNotifierMu.Unlock()
	runNoticeNotifier = p
}

func currentRunNoticeNotifier() runNoticePublisher {
	runNoticeNotifierMu.RLock()
	defer runNoticeNotifierMu.RUnlock()
	return runNoticeNotifier
}

// runNoticeGo starts a publish. A seam so tests can run it in line and read what
// was sent without waiting on a goroutine.
var runNoticeGo = func(f func()) { go f() }

// runNotice is one notice, by id and code. Exactly one of PipelineID,
// SavedQueryID and WorkflowID is set: it is what the notifier routes on.
type runNotice struct {
	PipelineID   string
	SavedQueryID string
	WorkflowID   string
	Code         string
	Severity     string // "warning" or "error"
	FailureType  string
	Message      string // a fixed sentence; the catalog's Impact line follows it
	ActionURL    string
	// DedupSubject narrows the notifier's 60-minute dedup window to this event.
	DedupSubject string
}

// payload is the rsync.notifications shape (notifier.notificationPayload), with
// the envelope that makes the notifier render the code's catalog copy.
func (n runNotice) payload(now time.Time) []byte {
	body := map[string]interface{}{
		"type":       "structured_error_notification",
		"message":    n.Message,
		"timestamp":  now.UTC().Format(time.RFC3339),
		"action_url": n.ActionURL,
		"error": map[string]interface{}{
			"failure_type":  n.FailureType,
			"code":          n.Code,
			"severity":      n.Severity,
			"audience":      "user",
			"user_message":  n.Message,
			"dedup_subject": n.DedupSubject,
		},
	}
	switch {
	case n.SavedQueryID != "":
		body["saved_query_id"] = n.SavedQueryID
	case n.WorkflowID != "":
		body["workflow_id"] = n.WorkflowID
	default:
		body["pipeline_id"] = n.PipelineID
	}
	raw, _ := json.Marshal(body)
	return raw
}

// publishRunNotice sends n in the background on its own context, so a request
// that has already answered cannot cancel it. A failure is logged and dropped:
// the thing the notice is about has already happened and been recorded.
func publishRunNotice(n runNotice) {
	p := currentRunNoticeNotifier()
	if p == nil || strings.TrimSpace(n.Code) == "" {
		return
	}
	raw := n.payload(time.Now())
	runNoticeGo(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.Publish(ctx, raw); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"code":           n.Code,
				"pipeline_id":    n.PipelineID,
				"saved_query_id": n.SavedQueryID,
				"workflow_id":    n.WorkflowID,
			}).Warn("run notice: publish failed (ignored)")
		}
	})
}
