package sentinel

import (
	"context"
	"database/sql"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultSourceLagNotifyAfter is how long source-side lag must stay over its threshold, with
// the connector's committed position not moving and no heartbeat written, before the warning
// on the pipeline page becomes a notification to the pipeline's owner (CDC_SOURCE_LAG_HIGH).
// Overridable via CDC_SOURCE_LAG_NOTIFY_AFTER (minimum 30s).
//
// Lag over the threshold on its own is a warning, because it is also what a healthy
// connector looks like while it works through a burst. Half an hour with a position that
// never moves and no heartbeat is neither a burst nor a quiet source: the source is
// producing changes and nothing is reading them, and the slot or binlog keeps growing on
// the user's database while it lasts.
const DefaultSourceLagNotifyAfter = 30 * time.Minute

// sourceLagNotifyAfter reads the notification bound.
func sourceLagNotifyAfter() time.Duration {
	return walDurationFromEnv("CDC_SOURCE_LAG_NOTIFY_AFTER", DefaultSourceLagNotifyAfter)
}

// sourceLagProbe is what a lag reading knows beyond the lag itself, and so whether it can
// ever say the source has stopped being read.
type sourceLagProbe struct {
	// sourceSide is true for a reading of the source's own log (slot lag, binlog lag) and
	// false for the MySQL consumer-lag proxy, which measures the sink's Kafka backlog.
	sourceSide bool
	// position is the connector's committed source position: the slot's
	// confirmed_flush_lsn, or the binlog file:pos. "" when the probe cannot read one.
	position string
	// heartbeats is how many heartbeats the connector has written to its heartbeat topic
	// (the topic's end offset), 0 when it emits none or the count cannot be read. A
	// PostgreSQL or MySQL heartbeat does not move the committed position of a connector
	// whose tables are quiet (heartbeatAdvancesAnIdlePosition), so on a server whose log
	// grows from other databases the position freezes while the reader is healthy. Only
	// heartbeats that stop arriving say nothing is reading.
	heartbeats int64
	// slot is the PostgreSQL replication slot, whose WAL alarm (cdc-wal-<slot>) is the more
	// specific report of the same problem. "" for MySQL.
	slot string
}

// sourceLagAlert is how loudly a source-lag reading is reported: a warning on the pipeline
// page ("source_lag") until the lag has been over its threshold, the committed position
// frozen and the heartbeat count unchanged, each for notifyAfter, then a critical alert that
// is pushed to the pipeline's owner ("source_lag_sustained", notify.go). A reading that is
// not source-side never escalates, and neither does one with no heartbeat count, whose
// silence clock restarts on every reading (decideSourceLag): neither can tell a stopped
// reader from a quiet one.
func sourceLagAlert(overFor, frozenFor, silentFor, notifyAfter time.Duration, sourceSide bool) (IssueSeverity, string) {
	if sourceSide && overFor >= notifyAfter && frozenFor >= notifyAfter && silentFor >= notifyAfter {
		return IssueSeverityCritical, "source_lag_sustained"
	}
	return IssueSeverityWarning, "source_lag"
}

// sourceLagCriticalSince is when a critical source_lag_sustained reading's stall turned
// critical: the moment the last of its three clocks reached notifyAfter. It is the alert's
// episode for the api-gateway, rather than its row's detected_at, because the row outlives
// a stall: once heartbeats return it stays open as a warning for as long as a quiet source
// sits over the lag threshold, and a stall after that was dropped as a repeat of the first
// page. Every reading of one stall gives the same instant, since the clocks start at fixed
// readings; the next stall gives a later one, since a heartbeat restarts the silence clock.
// The clocks are in memory, so a stall that outlasts an orchestrator restart is a new
// episode: it is pushed again unless it was pushed in the hour before.
func sourceLagCriticalSince(now time.Time, overFor, frozenFor, silentFor, notifyAfter time.Duration) time.Time {
	return now.Add(notifyAfter - min(overFor, frozenFor, silentFor))
}

// heartbeatsWritten is how many heartbeats a source connector has written: the end offset
// of its heartbeat topic, read the way the freshness check reads it. 0 when the connector
// emits no heartbeats or the count cannot be read, which never escalates.
func (s *CDCSentinel) heartbeatsWritten(config map[string]interface{}) int64 {
	if !sourceHeartbeatEnabled(config) {
		return 0
	}
	topic, ok := sourceHeartbeatTopic(config)
	if !ok {
		return 0
	}
	end, ok := s.heartbeatTopicEndOffset(topic)
	if !ok {
		return 0
	}
	return end
}

// sourceLagState is what the source-lag alarm remembers about one pipeline between ticks.
type sourceLagState struct {
	// since is the first reading of the current run of over-threshold readings.
	since time.Time
	// lastPos and posMovedAt are the committed position last seen and when it last changed.
	lastPos    string
	posMovedAt time.Time
	// lastBeats and beatsMovedAt are the heartbeat count last seen and when it last changed.
	lastBeats    int64
	beatsMovedAt time.Time
}

// decideSourceLag folds one over-threshold reading into a pipeline's state. The first
// reading starts every clock, because it says nothing about how long the lag has lasted. A
// position that changes, in either direction, restarts the frozen clock; so does a reading
// with no position, since it cannot show the position is frozen. A heartbeat count that
// changes restarts the silence clock, and so does a reading with no count.
func decideSourceLag(prev sourceLagState, seen bool, position string, beats int64, now time.Time) sourceLagState {
	if !seen {
		return sourceLagState{since: now, lastPos: position, posMovedAt: now, lastBeats: beats, beatsMovedAt: now}
	}
	if position == "" || position != prev.lastPos {
		prev.lastPos = position
		prev.posMovedAt = now
	}
	if beats == 0 || beats != prev.lastBeats {
		prev.lastBeats = beats
		prev.beatsMovedAt = now
	}
	return prev
}

// observeSourceLag records an over-threshold reading and returns how long the lag has been
// over its threshold, how long the committed position has not moved, and how long no
// heartbeat has arrived.
func (s *CDCSentinel) observeSourceLag(pipelineID string, probe sourceLagProbe, now time.Time) (overFor, frozenFor, silentFor time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sourceLag == nil {
		s.sourceLag = make(map[string]sourceLagState)
	}
	prev, seen := s.sourceLag[pipelineID]
	next := decideSourceLag(prev, seen, probe.position, probe.heartbeats, now)
	s.sourceLag[pipelineID] = next
	return now.Sub(next.since), now.Sub(next.posMovedAt), now.Sub(next.beatsMovedAt)
}

// forgetSourceLag drops a pipeline's clock once its lag is back under the threshold.
func (s *CDCSentinel) forgetSourceLag(pipelineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sourceLag, pipelineID)
}

// pruneSourceLag drops the clock of every pipeline a tick did not find running. A stopped
// pipeline's source keeps producing changes nobody reads, by the user's choice; carrying the
// clock across the stop would page them on the first reading after they start it again.
func (s *CDCSentinel) pruneSourceLag(running map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pipelineID := range s.sourceLag {
		if !running[pipelineID] {
			delete(s.sourceLag, pipelineID)
		}
	}
}

// sourceLagSuppressed reports whether a more specific alarm about the same pipeline is
// already open, in which case the sustained-lag push would only repeat it less precisely:
// the source has stalled or stopped being captured, the connector is down, the sink is
// critically behind, or the slot's WAL is the problem. A warning sink-lag does not count.
//
// A failed lookup does not suppress. An extra notification about a real stall can be
// dismissed; a missed one cannot be taken back.
func (s *CDCSentinel) sourceLagSuppressed(ctx context.Context, pipelineID, slot string) bool {
	walIssueID := ""
	if slot != "" {
		walIssueID = "cdc-wal-" + slot
	}
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM sentinel_active_issues
		WHERE id IN ($1, $2, $3, $4)
		   OR (id = $5 AND severity = $6)
		LIMIT 1
	`, sourceStalledIssueID(pipelineID), captureStoppedIssueID(pipelineID), connectorIssueID(pipelineID),
		walIssueID, sinkLagIssueID(pipelineID), string(IssueSeverityCritical)).Scan(&one)
	if err == nil {
		return true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		log.WithError(err).WithField("pipeline_id", pipelineID).
			Debug("🛡️ source lag: could not check for a stronger open alarm; escalating")
	}
	return false
}
