package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"api-gateway/internal/db"
	"api-gateway/internal/notifier"
)

// A scheduled run that stops to ask a question (pick tables, approve a change, fix
// a connection) waits until somebody answers. Nobody started it, so nobody is
// watching it, and before this the only sign was a run that never finished. The
// projector calls NotifyPipelineAwaitingInput for every PIPELINE_WAITING it stores
// (projector.EventProjector.OnPipelineWaiting, wired in main.go), and the owner of
// a scheduled run is told once per wait. A run somebody started by hand is not: the
// person who pressed Run is looking at the question.

// waitAlertRowSQL is what the database says about the waiting run now. The event
// is a few milliseconds old by the time it is checked; a run answered, paused or
// replaced since must not be reported as waiting.
const waitAlertRowSQL = `
	SELECT COALESCE(e.trigger_source, 'manual'),
	       COALESCE(pp.execution_id = e.id, false),
	       COALESCE(pp.status, ''),
	       COALESCE(pp.blocking_reason_type, '')
	FROM executions e
	LEFT JOIN pipeline_progress pp ON pp.pipeline_id = e.pipeline_id
	WHERE e.id = $1 AND e.pipeline_id = $2`

// waitAlertTTL is how long one wait is remembered. The notifier's own dedup window
// is the same 60 minutes; this copy covers the time between two publishes, before
// the first is persisted, which that one cannot.
const waitAlertTTL = 60 * time.Minute

// waitAlertClaims remembers which waits were alerted, keyed execution:blocking type.
type waitAlertClaims struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newWaitAlertClaims() *waitAlertClaims {
	return &waitAlertClaims{seen: map[string]time.Time{}}
}

// claim reports whether key is new, and remembers it if so. Entries past the TTL
// are dropped on the way, so the map holds at most an hour of waits.
func (c *waitAlertClaims) claim(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, at := range c.seen {
		if now.Sub(at) >= waitAlertTTL {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = now
	return true
}

var waitAlerts = newWaitAlertClaims()

// The body of an awaiting-input notice. Fixed: the question's own wording can name
// tables and columns, and a notice carries ids and codes only.
const awaitingInputMessage = "A scheduled run is waiting for an answer in rsync.ai."

// NotifyPipelineAwaitingInput tells a pipeline's owner its scheduled run stopped to
// ask a question. Best-effort and quiet: any doubt about the run (a lookup that
// failed, an id that is not a run's) tells nobody, since the run is still on the
// pipeline's page either way.
func NotifyPipelineAwaitingInput(ctx context.Context, pipelineID, executionID, blockingType string) {
	database := db.GetDB()
	if database == nil || currentRunNoticeNotifier() == nil {
		return
	}
	if _, err := uuid.Parse(pipelineID); err != nil {
		return
	}
	if _, err := uuid.Parse(executionID); err != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var trigger, progress, blocking string
	var progressIsIt bool
	if err := database.QueryRowContext(qctx, waitAlertRowSQL, executionID, pipelineID).Scan(
		&trigger, &progressIsIt, &progress, &blocking,
	); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.WithError(err).WithField("pipeline_id", pipelineID).
				Warn("awaiting-input alert: could not re-read the run (nobody told)")
		}
		return
	}
	// Scheduled only (D4): a manual run's owner is watching it, and a run a stream or
	// a workflow started is told about by what started it.
	if trigger != "scheduled" || !progressIsIt || progress != "waiting_for_user" || blocking == "paused_by_user" {
		return
	}

	key := executionID + ":" + SafeCode(strings.TrimSpace(blockingType))
	if !waitAlerts.claim(key, time.Now()) {
		return
	}
	publishRunNotice(runNotice{
		PipelineID:   pipelineID,
		Code:         notifier.CodePipelineAwaitingInput,
		Severity:     "warning",
		FailureType:  "user_action_required",
		Message:      awaitingInputMessage,
		ActionURL:    "/pipelines/" + pipelineID,
		DedupSubject: key,
	})
}
