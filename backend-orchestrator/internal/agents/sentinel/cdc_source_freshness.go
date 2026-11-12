package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultSourceFreshnessStallAfter is how long a RUNNING CDC source connector may hold
// the same committed stream position before the freshness alarm fires. Overridable via
// CDC_SOURCE_FRESHNESS_STALL_AFTER (minimum 30s).
//
// Twenty minutes is four missed heartbeats at the shipped five-minute interval, so a
// single slow or skipped heartbeat never alarms. A connector configured to heartbeat less
// often gets four of its own intervals instead (sourceFreshnessStallBound).
const DefaultSourceFreshnessStallAfter = 20 * time.Minute

// sourceFreshnessStallHeartbeats is how many heartbeat intervals a position must hold
// before it counts as stalled, whatever CDC_SOURCE_FRESHNESS_STALL_AFTER says.
const sourceFreshnessStallHeartbeats = 4

// sourceFreshnessStallBound is how long a connector that heartbeats every heartbeatEvery
// may go without progress before the alarm fires: the configured bound, or four of its
// heartbeats when that is longer.
func sourceFreshnessStallBound(heartbeatEvery time.Duration) time.Duration {
	bound := walDurationFromEnv("CDC_SOURCE_FRESHNESS_STALL_AFTER", DefaultSourceFreshnessStallAfter)
	if beats := sourceFreshnessStallHeartbeats * heartbeatEvery; beats > bound {
		return beats
	}
	return bound
}

// sourceFreshnessObservationGapPolls is how many sentinel polls a connector may go
// unobserved before its next reading restarts the stall clock instead of continuing it.
const sourceFreshnessObservationGapPolls = 3

// sourceFreshnessState is what the freshness alarm remembers about one connector
// between ticks.
type sourceFreshnessState struct {
	// fingerprint identifies the connector's committed source position. Its contents
	// are opaque and connector-specific (a Mongo resume token, a Postgres LSN, a MySQL
	// binlog coordinate); only whether it CHANGED is ever interpreted.
	fingerprint string
	// movingAt is the last time the connector was seen making progress: its committed
	// source position changed.
	movingAt time.Time
	// observedAt is the last tick that read this connector's position at all. A paused
	// or stopped connector stays registered in Connect, so forgetSourceFreshness keeps
	// its entry, but nobody observes it while it is not RUNNING. Zero for an entry that
	// noteCaptureDown created before any reading.
	observedAt time.Time
	// moved: this reading's position differs from the previous reading's. Only a move
	// clears the stall alarm; a first reading or one after a gap proves nothing.
	moved bool
	// advancedAfter is the earliest the latest observed move can have happened: the
	// reading before the one that saw the position change.
	advancedAfter time.Time
	// downAt is the last tick that saw this connector's capture down (Connect FAILED, or
	// the dependency probe's capture-down verdict). Carried across readings.
	downAt time.Time
	// blind: the latest freshness check could not read this connector's progress at
	// all, so captureProvenMoving has nothing to judge by.
	blind bool
}

// decideSourceFreshnessAlarm says whether a source connector that Kafka Connect calls
// RUNNING has actually stopped advancing through its source's change stream.
//
// Connect's task state cannot answer this. Debezium retries a retriable error on a loop
// without ever leaving RUNNING, so a connector whose MongoDB resume token has aged out
// of the oplog reports a fully green /status while it moves nothing — for as long as
// nobody looks at the container logs (KI-CDC-MONGO-RESUME-TOKEN-SILENT-STALL). The
// committed source offset is the signal that does answer it: a streaming connector
// advances it, a looping one never does.
//
// This is only sound when the connector emits heartbeats, because a genuinely idle
// source has nothing to advance past and would otherwise alarm for being quiet. The
// caller enforces that gate (sourceHeartbeatEnabled) and decides what the fingerprint
// holds: a MongoDB heartbeat advances the position on a timer whether or not the source
// is writing, while a PostgreSQL or MySQL one does not, so for those the fingerprint also
// carries the heartbeat topic's end offset (heartbeatAdvancesAnIdlePosition). Either way
// "idle but alive" and "dead" stop looking alike.
//
// The first reading for a connector only starts the clock, because it says nothing about
// how long the position has been frozen. So does the first reading after a gap longer than
// maxGap: a connector that was paused, stopped or otherwise not RUNNING for that long
// resumes at the position it held before, and the time it spent not running is not time
// spent stalled.
func decideSourceFreshnessAlarm(prev sourceFreshnessState, seen bool, fingerprint string, now time.Time, stallAfter, maxGap time.Duration) (sourceFreshnessState, bool) {
	next := sourceFreshnessState{
		fingerprint:   fingerprint,
		movingAt:      prev.movingAt,
		observedAt:    now,
		advancedAfter: prev.advancedAfter,
		downAt:        prev.downAt,
	}
	if seen && fingerprint != prev.fingerprint {
		next.moved = true
		next.advancedAfter = prev.observedAt
	}
	if !seen || fingerprint != prev.fingerprint || now.Sub(prev.observedAt) > maxGap {
		next.movingAt = now
		return next, false
	}
	return next, now.Sub(prev.movingAt) >= stallAfter
}

// observeSourceFreshness records this tick's progress reading for a connector that
// heartbeats every heartbeatEvery, and returns whether its freshness alarm should be
// raised, and for how long the reading has not moved.
func (s *CDCSentinel) observeSourceFreshness(connectorName, fingerprint string, heartbeatEvery time.Duration, now time.Time) (bool, time.Duration) {
	stallAfter := sourceFreshnessStallBound(heartbeatEvery)
	// Three polls, not one: a single slow or skipped tick must not reset a stall that is
	// about to fire.
	interval := s.pollInterval
	if interval <= 0 {
		interval = pollIntervalFromEnv()
	}
	maxGap := sourceFreshnessObservationGapPolls * interval

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sourceFreshness == nil {
		s.sourceFreshness = make(map[string]sourceFreshnessState)
	}
	prev, ok := s.sourceFreshness[connectorName]
	// An entry noteCaptureDown made has no reading in it yet.
	seen := ok && !prev.observedAt.IsZero()
	next, alarm := decideSourceFreshnessAlarm(prev, seen, fingerprint, now, stallAfter, maxGap)
	s.sourceFreshness[connectorName] = next
	return alarm, now.Sub(next.movingAt)
}

// sourceFreshnessMoved reports whether the connector's latest reading saw its position
// change from the one before.
func (s *CDCSentinel) sourceFreshnessMoved(connectorName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sourceFreshness[connectorName].moved
}

// markSourceFreshnessBlind records that this tick could not read the connector's
// progress. It never creates an entry: a connector with none has no clock to keep.
func (s *CDCSentinel) markSourceFreshnessBlind(connectorName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.sourceFreshness[connectorName]; ok {
		st.blind = true
		s.sourceFreshness[connectorName] = st
	}
}

// noteCaptureDown records that the connector's capture side was seen down at now.
func (s *CDCSentinel) noteCaptureDown(connectorName string, now time.Time) {
	if connectorName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sourceFreshness == nil {
		s.sourceFreshness = make(map[string]sourceFreshnessState)
	}
	st := s.sourceFreshness[connectorName]
	st.downAt = now
	s.sourceFreshness[connectorName] = st
}

// captureProvenMoving says whether a capture side that was seen down may be announced
// as working again: two readings of its committed position, both taken after it was
// last seen down, differ.
//
// Connect's RUNNING and the probe's healthy cannot say this. A MongoDB connector whose
// resume token has left the oplog is RUNNING between its retries, so on prod on
// 2026-10-22 "Change capture is running again" went out while the stream moved nothing,
// and the stall alarm fired 21 minutes later.
//
// No down episode on record (a fresh orchestrator) or a connector this check cannot
// read (no heartbeats, no offsets endpoint) is judged as before: running means running.
func (s *CDCSentinel) captureProvenMoving(connectorName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sourceFreshness[connectorName]
	if !ok || st.downAt.IsZero() || st.blind {
		return true
	}
	return st.advancedAfter.After(st.downAt)
}

// forgetSourceFreshness drops a connector's freshness bookkeeping, so a connector that
// is deleted and later recreated under the same name starts from a clean slate instead
// of inheriting a stale frozen-since timestamp.
func (s *CDCSentinel) forgetSourceFreshness(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.sourceFreshness {
		if !keep[name] {
			delete(s.sourceFreshness, name)
		}
	}
}

// sourceStalledIssueID keys the freshness alarm. It is a class of its own: neither lag
// resolver may clear it, and it must not collide with cdc-connector-down-*, which needs
// Connect to admit the connector failed. This alarm exists for the case where it never
// does.
func sourceStalledIssueID(pipelineID string) string {
	return fmt.Sprintf("cdc-source-stalled-%s", pipelineID)
}

// checkSourceFreshness is the RUNNING-connector counterpart of handleFailedConnector:
// it asks whether a connector Kafka Connect calls healthy is actually moving.
//
// Progress is read one of two ways. A MongoDB heartbeat moves the committed position
// itself, so the position alone is the reading. A PostgreSQL or MySQL heartbeat does not
// (heartbeatAdvancesAnIdlePosition), but every heartbeat is still a record on the
// connector's heartbeat topic, so for those the reading is the position plus that topic's
// end offset: an idle source moves the second, a dead connector moves neither.
//
// Every rung is a skip rather than an alarm when it cannot answer — no offsets endpoint
// (Connect < 3.6), no heartbeats configured, no committed position yet, no heartbeat
// written yet, a snapshot still running, Kafka unreadable. A watchdog that alarms on its
// own blindness is worse than no watchdog.
func (s *CDCSentinel) checkSourceFreshness(ctx context.Context, pipelineID, connectorName string, connState interface{}) {
	if s.db == nil || pipelineID == "" {
		return
	}
	// Every skip below leaves captureProvenMoving nothing to judge by, so it falls back
	// to taking RUNNING at its word for this connector.
	read := false
	defer func() {
		if !read {
			s.markSourceFreshnessBlind(connectorName)
		}
	}()

	cfg, ok := s.fetchSourceConfig(ctx, connectorName)
	if !ok || !sourceHeartbeatEnabled(cfg) {
		// Without heartbeats a frozen position means "the source had no writes", which is
		// perfectly healthy. Only a connector that heartbeats is expected to show progress
		// on a timer, and only then does frozen mean stalled.
		return
	}

	offsets, ok := s.fetchSourceOffsets(ctx, connectorName)
	if !ok {
		return
	}
	fingerprint, ok := sourceOffsetFingerprint(offsets)
	if !ok {
		return
	}

	detectedBy := "cdc_sentinel_source_freshness"
	heartbeatTopic := ""
	if !heartbeatAdvancesAnIdlePosition(cfg) {
		// A blocking snapshot writes no heartbeats, and the topic may still hold an
		// earlier run's, so a long re-snapshot would look exactly like a stall.
		if sourceOffsetsMidSnapshot(offsets) {
			return
		}
		topic, ok := sourceHeartbeatTopic(cfg)
		if !ok {
			return
		}
		end, ok := s.heartbeatTopicEndOffset(topic)
		if !ok {
			return
		}
		fingerprint += "|hb:" + strconv.FormatInt(end, 10)
		detectedBy, heartbeatTopic = "heartbeat_topic", topic
	}
	heartbeatEvery, _ := sourceHeartbeatInterval(cfg)

	read = true
	stalled, stalledFor := s.observeSourceFreshness(connectorName, fingerprint, heartbeatEvery, time.Now())
	issueID := sourceStalledIssueID(pipelineID)
	if !stalled {
		// A position that moved is the only positive proof this connector is alive, so it
		// is the only thing that clears the alarm. A first reading, or the first after a
		// gap, only restarts the clock: clearing on it told the owner a stalled stream was
		// "moving again" after every orchestrator restart.
		if s.sourceFreshnessMoved(connectorName) {
			s.resolveLagIssue(ctx, issueID, pipelineID)
		}
		return
	}

	description := sourceStalledDescription(connectorName, stalledFor, heartbeatEvery, heartbeatTopic != "")
	if errText := diagnosableErrorText(traceFromConnectorState(connState), maxIssueErrorText); errText != "" {
		description += ": " + errText
	}

	metadata := map[string]interface{}{
		"connector":           connectorName,
		"stalled_for_seconds": int64(stalledFor.Seconds()),
		"connector_state":     "RUNNING",
		"detected_by":         detectedBy,
	}
	if heartbeatTopic != "" {
		metadata["heartbeat_topic"] = heartbeatTopic
	}
	s.emitCDCIssue(ctx, issueID,
		IssueTypeSourceStreamStalled, IssueSeverityCritical,
		"source_stream_stalled", pipelineID, "", "", description, metadata)

	log.WithFields(log.Fields{
		"pipeline_id": pipelineID,
		"connector":   connectorName,
		"stalled_for": stalledFor.Round(time.Second).String(),
		"detected_by": detectedBy,
	}).Error("🛡️ Sentinel raised a source-stream-stalled issue (connector RUNNING but not advancing)")
}

// sourceStalledDescription is the stall alarm's issue text, which the Healer diagnoses.
//
// Deliberately free of every phrase a diagnose.go rule matches: the re-snapshot rule's
// ("resume token", "snapshot too old", …) and the provisioning-escalation rule's, which
// include the bare words "change stream" and "debezium". A frozen position has several
// possible causes and naming one of them here would make the diagnoser confidently pick
// it every time; a provisioning phrase would also shadow every rule below it, such as the
// network rule, whatever the connector's error said. The connector's own error text, when
// Connect exposes any, is appended by the caller and is what should drive the
// classification.
func sourceStalledDescription(connectorName string, stalledFor, heartbeatEvery time.Duration, byHeartbeatTopic bool) string {
	if byHeartbeatTopic {
		return fmt.Sprintf(
			"CDC source connector %s has not advanced its committed source position or written a heartbeat for %s, although Kafka Connect still reports it RUNNING. It writes a heartbeat every %s whether or not the source has changes, so even an idle source would show progress — no change events are reaching the destination. Likely causes: the source is unreachable, the stored stream position is no longer available on the source, or the connector is retrying a permanent error without failing. Check the connector task log for the underlying error",
			connectorName, stalledFor.Round(time.Second), heartbeatEvery)
	}
	return fmt.Sprintf(
		"CDC source connector %s has not advanced its committed source position for %s, although Kafka Connect still reports it RUNNING. Heartbeats are enabled on this connector, so an idle source would still be advancing — no change events are reaching the destination. Likely causes: the stored stream position is no longer available on the source (for MongoDB, the oplog has rolled past it), the source is unreachable, or the connector is retrying a permanent error without failing. Check the connector task log for the underlying error",
		connectorName, stalledFor.Round(time.Second))
}

// sourceOffsetFingerprint reduces a Kafka Connect GET /connectors/<name>/offsets payload
// to a single comparable string.
//
// Shape (KIP-875, Connect 3.6+):
//
//	{"offsets":[{"partition":{"server_id":"..."},"offset":{"resume_token":"...","sec":N,"ord":N}}]}
//
// The offset object is re-encoded rather than read field by field on purpose: the keys
// differ per connector class (resume_token/sec/ord for MongoDB, lsn for Postgres,
// file/pos for MySQL), and the alarm only ever asks whether the value changed. Go
// marshals map keys in sorted order, so the encoding is stable across ticks.
//
// Returns ok=false when the payload carries no offsets at all — a connector that has not
// committed a position yet is starting up, not stalled.
func sourceOffsetFingerprint(payload map[string]interface{}) (string, bool) {
	if payload == nil {
		return "", false
	}
	offsets, ok := payload["offsets"].([]interface{})
	if !ok || len(offsets) == 0 {
		return "", false
	}
	encoded, err := json.Marshal(offsets)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// sourceHeartbeatEnabled reports whether a connector is configured to emit heartbeats,
// which is what makes a frozen offset mean "stalled" rather than "idle".
//
// It reads exactly one key. A Debezium source config also carries
// mongodb.connection.string, database.password and friends, so the payload is parsed and
// the single value extracted — never logged, never returned, never handed to an LLM.
func sourceHeartbeatEnabled(config map[string]interface{}) bool {
	_, ok := sourceHeartbeatInterval(config)
	return ok
}

// sourceHeartbeatInterval reads heartbeat.interval.ms, reporting ok=false when heartbeats
// are off or the value is not a positive number of milliseconds.
func sourceHeartbeatInterval(config map[string]interface{}) (time.Duration, bool) {
	if config == nil {
		return 0, false
	}
	raw, ok := config["heartbeat.interval.ms"]
	if !ok {
		return 0, false
	}
	ms, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(raw)))
	if err != nil || ms <= 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// sourceHeartbeatTopic names the topic a Debezium connector writes its heartbeats to:
// <topic.heartbeat.prefix><topic.delimiter><topic.prefix>, prefix first, which is how
// Debezium 2+ builds it. The legacy heartbeat.topics.prefix key does not name the topic,
// so only the current one is read, with Debezium's own defaults. ok=false when the config
// has no topic.prefix, which no connector the executor builds lacks.
func sourceHeartbeatTopic(config map[string]interface{}) (string, bool) {
	value := func(key, fallback string) string {
		if v, ok := config[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}
	topicPrefix := value("topic.prefix", "")
	if topicPrefix == "" {
		return "", false
	}
	return value("topic.heartbeat.prefix", "__debezium-heartbeat") + value("topic.delimiter", ".") + topicPrefix, true
}

// sourceOffsetsMidSnapshot reports whether a committed position was written by a snapshot
// that has not finished: Debezium marks those offsets with "snapshot" (a snapshot type
// such as "INITIAL", or true on older connectors) until the last snapshot record, which
// carries last_snapshot_record=true. Once streaming, the key is gone.
func sourceOffsetsMidSnapshot(payload map[string]interface{}) bool {
	offsets, _ := payload["offsets"].([]interface{})
	for _, entry := range offsets {
		e, _ := entry.(map[string]interface{})
		offset, _ := e["offset"].(map[string]interface{})
		snapshotting := false
		switch v := offset["snapshot"].(type) {
		case bool:
			snapshotting = v
		case string:
			snapshotting = v != "" && !strings.EqualFold(v, "false")
		}
		if snapshotting && offset["last_snapshot_record"] != true {
			return true
		}
	}
	return false
}

// heartbeatTopicEndOffset reads how many heartbeats a connector has written, reporting
// ok=false while the answer says nothing about liveness: no Kafka client, a read error, a
// topic that does not exist yet, or one with no heartbeat on it (a connector still in its
// first snapshot).
func (s *CDCSentinel) heartbeatTopicEndOffset(topic string) (int64, bool) {
	read := s.heartbeatEndOffset
	if read == nil {
		if s.kafkaManager == nil {
			return 0, false
		}
		read = s.kafkaManager.TopicEndOffset
	}
	end, exists, err := read(topic)
	if err != nil {
		log.WithError(err).WithField("topic", topic).Debug("Sentinel could not read a CDC heartbeat topic; treating its heartbeat count as unknown this tick")
		return 0, false
	}
	return end, exists && end > 0
}

// heartbeatAdvancesAnIdlePosition reports whether this connector's heartbeat moves its
// committed position even when the source has no writes at all. Only then does a frozen
// position prove a stall.
//
// MongoDB's does: every heartbeat commits the change stream's latest resume token, which
// the server advances on its own. PostgreSQL's does not, and neither does MySQL's. The
// PostgreSQL heartbeat (#12) commits the last LSN the connector received, so it
// acknowledges WAL that was decoded and filtered out, but a database with no writes sends
// no new LSN and the heartbeat repeats the same one; a MySQL binlog position likewise
// moves only with a record. Only heartbeat.action.query would move either, and that writes
// to the source, which rsync.ai does not do. Judging these connectors by their position
// alone would therefore raise the stall alarm on every quiet pipeline, so for them
// checkSourceFreshness also reads the heartbeat topic.
//
// connector.class is not a secret; like the heartbeat key it is read and never logged.
func heartbeatAdvancesAnIdlePosition(config map[string]interface{}) bool {
	class, _ := config["connector.class"].(string)
	return strings.Contains(strings.ToLower(class), "mongodb")
}

// fetchSourceOffsets reads a connector's committed source position via the Connect REST
// API (KIP-875, Connect 3.6+).
//
// Older workers answer 404 or 405 here. That is not an error worth alarming on — it just
// means this rung is unavailable on that cluster — so the caller skips the connector and
// the rest of the Sentinel is unaffected.
func (s *CDCSentinel) fetchSourceOffsets(ctx context.Context, connectorName string) (map[string]interface{}, bool) {
	return s.getConnectJSON(ctx, "/connectors/"+connectorName+"/offsets")
}

// fetchSourceConfig reads a connector's config so the heartbeat gate can be evaluated.
// The payload contains credentials; see sourceHeartbeatEnabled for how it is handled.
func (s *CDCSentinel) fetchSourceConfig(ctx context.Context, connectorName string) (map[string]interface{}, bool) {
	return s.getConnectJSON(ctx, "/connectors/"+connectorName+"/config")
}

// getConnectJSON GETs a Connect endpoint and decodes a JSON object, reporting ok=false
// for any transport error, non-2xx status, or undecodable body. Every caller here treats
// a failure as "this rung has nothing to say about this connector this tick".
func (s *CDCSentinel) getConnectJSON(ctx context.Context, path string) (map[string]interface{}, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.connectURL+path, nil)
	if err != nil {
		return nil, false
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}
