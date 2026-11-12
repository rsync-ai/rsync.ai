package notifier

import (
	"context"
	"database/sql"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// "No longer detected" notices (decision D2).
//
// A resolved notice answers one pushed alert, so it is delivered only when that
// alert was pushed for this episode and stayed open:
//
//   - a row of the alert (same scope, action_url and subject, so the same dedup
//     key as the raise) exists since the last resolved notice for it, and, when
//     the producer said when the episode opened, no older than the dedup window
//     before that;
//   - the alert has been open for resolvedMinOpen, counted from the later of that
//     row and the episode's opened_at.
//
// A second resolved notice for the same alert finds no row since the first, so
// it is dropped too. In the other direction, an alert that has a resolved notice
// dedups only back to its last one, so a relapse after a delivered resolved
// notice is always delivered.
//
// The notice goes through the rest of handleMessage unchanged: the same
// recipients and the same category (categories.go) as its alert, so the same
// channels and the same mutes.

const (
	resolvedMinOpen = 5 * time.Minute
	// resolvedClockSlack absorbs a producer clock that runs ahead of the database's.
	resolvedClockSlack = time.Minute
)

// resolvedPairs maps each resolved code to the dedup identity of the alert it
// answers: the alert's code, or its event type when it has none.
var resolvedPairs = map[string]string{
	CodeSourceStreamStalledResolved: "CDC_SOURCE_STREAM_STALLED",
	CodeCaptureStoppedResolved:      "CDC_CAPTURE_STOPPED",
	CodeConnectorDownResolved:       "CDC_CONNECTOR_DOWN",
	CodeSinkNotDrainingResolved:     "CDC_SINK_NOT_DRAINING",
	CodeWALPressureResolved:         "CDC_WAL_PRESSURE",
	CodeInfrastructureDownResolved:  "INFRASTRUCTURE_DOWN",
}

// resolvedCodeFor is resolvedPairs inverted: an alert's identity to its resolved code.
var resolvedCodeFor = func() map[string]string {
	out := make(map[string]string, len(resolvedPairs))
	for resolved, raise := range resolvedPairs {
		out[raise] = resolved
	}
	return out
}()

// resolvedNoticeDue reports whether a resolved notice is to be delivered. raiseKey
// is the dedup key its alert was stored under, resolvedKey the notice's own, and
// openedAt the producer's RFC 3339 time for when the episode opened ("" if unsent).
func (n *Notifier) resolvedNoticeDue(ctx context.Context, raiseKey, resolvedKey, openedAt string) (bool, error) {
	opened, hasOpened := parseOpenedAt(openedAt)
	// An episode's alert may have been deduped against a row up to one window
	// before the episode opened. Anything older is another episode's.
	var since sql.NullTime
	if hasOpened {
		since = sql.NullTime{Time: opened.Add(-time.Duration(dedupWindowMinutes)*time.Minute - resolvedClockSlack), Valid: true}
	}

	var firstRaise sql.NullTime
	var now time.Time
	err := n.db.QueryRowContext(ctx, `
		SELECT MIN(created_at), NOW() FROM pipeline_notifications
		WHERE dedup_key = $1
		  AND created_at > COALESCE((SELECT MAX(created_at) FROM pipeline_notifications WHERE dedup_key = $2), '-infinity'::timestamptz)
		  AND created_at >= COALESCE($3::timestamptz, '-infinity'::timestamptz)
	`, raiseKey, resolvedKey, since).Scan(&firstRaise, &now)
	if err != nil {
		return false, err
	}
	fields := log.Fields{"dedup_key": resolvedKey, "raise_key": raiseKey}
	if !firstRaise.Valid {
		// Never pushed, or already answered: nobody was told there was a problem.
		log.WithFields(fields).Debug("notifier: resolved notice has no open alert, skipping")
		return false, nil
	}
	start := firstRaise.Time
	if hasOpened && opened.After(start) {
		start = opened
	}
	if open := now.Sub(start); open < resolvedMinOpen {
		log.WithFields(fields).WithField("open_for", open.String()).
			Debug("notifier: resolved notice for an alert open under the minimum, skipping")
		return false, nil
	}
	return true, nil
}

func parseOpenedAt(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
