package sentinel

import "time"

// DefaultSinkDrainStallAfter is how long a CDC sink may sit on a backlog without committing
// before the sink-lag alarm fires. Overridable via CDC_SINK_DRAIN_STALL_AFTER (minimum 30s).
//
// The sink commits its Kafka offsets right after each destination write, but it does not
// write every event as it arrives: a cloud-storage destination holds a batch open for up to
// maxObjectFlushIntervalSeconds (240 s, kafka-sink-worker main.go) before it writes the file.
// So a healthy sink can sit on a backlog with a flat committed position for the whole flush
// window plus however long the write takes.
//
// The bound has to cover that as the alarm actually SEES it: the check runs on the
// source-lag tick (CDC_SOURCE_LAG_POLL_INTERVAL, default 2 min), so a burst that lands just
// after a tick that saw no backlog has its clock started a whole tick early. With tick T,
// flush window F and write time w, the first tick at or past the bound must come after
// T + F + w. Five minutes failed that at T = 2 min (the alarm fires at 6 min, and
// 6 min < 2 + 4 + w for any write at all). Seven minutes fires at 8 min at T = 2 min and at
// 7 min at T = 1 min, which leaves a two-minute write in both
// (TestSinkDrain_FlushWindowNeverAlarmsAHealthySink).
const DefaultSinkDrainStallAfter = 7 * time.Minute

// DefaultSinkDrainNotifyAfter is how long a sink must stay stalled before the alarm stops
// being a warning on the pipeline page and becomes a notification to the pipeline's owner
// (CDC_SINK_NOT_DRAINING). Overridable via CDC_SINK_DRAIN_NOTIFY_AFTER; never below the
// stall bound.
//
// The two are separate because they answer different questions. The stall bound is "could
// this still be one slow write?", and a warning on the page is a cheap way to be wrong
// about that. A notification interrupts somebody, so it waits until no batch, however slow,
// explains the silence: fifteen minutes is more than three flush windows with no commit.
const DefaultSinkDrainNotifyAfter = 15 * time.Minute

// sinkDrainAlert is how loudly a stalled sink is reported: a warning on the pipeline page
// ("sink_lag") until it has been stalled for notifyAfter, then a critical alert that is
// pushed to the pipeline's owner ("sink_not_draining", notify.go). Both are written under the
// same issue id, so the page shows one alert that escalates rather than two.
func sinkDrainAlert(stalledFor, notifyAfter time.Duration) (IssueSeverity, string) {
	if stalledFor >= notifyAfter {
		return IssueSeverityCritical, "sink_not_draining"
	}
	return IssueSeverityWarning, "sink_lag"
}

// sinkDrainNotifyAfter reads the notification bound, held at or above the stall bound: a
// notification for a sink the alarm does not yet call stalled would be the false alarm the
// stall bound exists to prevent.
func sinkDrainNotifyAfter() time.Duration {
	notifyAfter := walDurationFromEnv("CDC_SINK_DRAIN_NOTIFY_AFTER", DefaultSinkDrainNotifyAfter)
	if stallAfter := walDurationFromEnv("CDC_SINK_DRAIN_STALL_AFTER", DefaultSinkDrainStallAfter); notifyAfter < stallAfter {
		return stallAfter
	}
	return notifyAfter
}

// sinkDrainState is what the sink-lag alarm remembers about one pipeline between ticks.
type sinkDrainState struct {
	committed int64
	// movingAt is the last time the sink was seen doing its job: its committed position
	// moved forward, or it had no backlog at all.
	movingAt time.Time
}

// decideSinkDrainAlarm says whether a sink with a backlog has stopped draining it.
//
// Lag on its own cannot say that. The first load of a large table puts a backlog far above
// any threshold on a perfectly healthy sink, and alarming on lag alone told the user rows
// were "NOT reaching the destination" while they were. What separates a busy sink from a
// stuck one is whether its committed position moves, so the alarm needs a backlog AND no
// movement for stallAfter.
//
// A sink that is dead, wedged, or replaying the same batch without committing never moves
// its committed position, so it still alarms. A sink that crashes but commits some batches
// between crashes is moving and does not alarm here.
//
// The first reading for a pipeline only starts the clock, and so does a committed position
// that went backwards (the group was reset or recreated), because neither says how long
// the sink has been stuck. Having no backlog also restarts the clock, so a burst that lands
// just before a tick on a sink idle for hours is not reported as hours of being stuck.
//
// backlog is ANY unconsumed event (lag > 0), not the 1000-event CDC_SINK_KAFKA_LAG_ALERT
// threshold the auto-restart rung uses. With the threshold, a sink stuck on 999 changes
// never alarmed, and on a quiet table that can be every change there is. Counting from one
// is safe because the sink commits every message it reads -- written, skipped, or parked
// in the DLQ -- and commits only after the destination has it (kafka-sink-worker main.go:
// CommitInterval 0, every branch of the consume loop commits or hands to a batcher that
// does). So a healthy sink with nothing to do reads lag 0, and a batch held open for its
// flush window is covered by the stall bound above.
//
// The third return, `moved`, is narrower than the alarm and is reported separately
// because the UI needs the raw fact, not the verdict. It says only "this reading's
// committed offset differs from the previous one" -- so it is false on the FIRST
// reading for a pipeline (nothing to compare against) and false when the offset went
// backwards (a reset group), neither of which is evidence the sink is working. It is
// what lets a tile distinguish a sink chewing through a first load from a sink that
// has died, and it is deliberately independent of `backlog`: a caught-up sink with
// nothing to do is not moving, and that is not a fault.
func decideSinkDrainAlarm(prev sinkDrainState, seen bool, committed int64, backlog bool, now time.Time, stallAfter time.Duration) (sinkDrainState, bool, bool) {
	moved := seen && committed > prev.committed
	next := sinkDrainState{committed: committed, movingAt: prev.movingAt}
	if !seen || !backlog || committed != prev.committed {
		next.movingAt = now
		return next, false, moved
	}
	return next, now.Sub(prev.movingAt) >= stallAfter, moved
}

// observeSinkDrain records this tick's committed position for a pipeline and returns
// whether its sink-lag alarm should be raised, for how long the sink has not moved,
// and whether its committed offset advanced since the previous reading.
func (s *CDCSentinel) observeSinkDrain(pipelineID string, committed int64, backlog bool, now time.Time) (bool, time.Duration, bool) {
	stallAfter := walDurationFromEnv("CDC_SINK_DRAIN_STALL_AFTER", DefaultSinkDrainStallAfter)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sinkDrain == nil {
		s.sinkDrain = make(map[string]sinkDrainState)
	}
	prev, seen := s.sinkDrain[pipelineID]
	next, alarm, moved := decideSinkDrainAlarm(prev, seen, committed, backlog, now, stallAfter)
	s.sinkDrain[pipelineID] = next
	return alarm, now.Sub(next.movingAt), moved
}
