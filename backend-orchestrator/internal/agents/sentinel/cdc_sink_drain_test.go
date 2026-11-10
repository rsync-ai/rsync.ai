package sentinel

import (
	"fmt"
	"testing"
	"time"
)

// tick is one sentinel poll as the sink-lag alarm sees it.
type tick struct {
	at        time.Duration // since the first tick
	committed int64
	lag       bool
}

// runDrainTicks feeds ticks through observeSinkDrain the way checkSinkConsumerLag does and
// returns the alarm verdict of each one.
func runDrainTicks(t *testing.T, ticks []tick) []bool {
	t.Helper()
	t.Setenv("CDC_SINK_DRAIN_STALL_AFTER", "5m")
	s := &CDCSentinel{} // no map on purpose: a bare literal must not panic
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	got := make([]bool, len(ticks))
	for i, tk := range ticks {
		got[i], _, _ = s.observeSinkDrain("p1", tk.committed, tk.lag, start.Add(tk.at))
	}
	return got
}

func assertAlarms(t *testing.T, got, want []bool) {
	t.Helper()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tick %d: alarm=%v, want %v (all: got %v, want %v)", i, got[i], want[i], got, want)
		}
	}
}

// The false alarm this replaces: a first load keeps the backlog far above the threshold for
// many minutes while the sink commits every tick.
func TestSinkDrain_BacklogThatIsDrainingNeverAlarms(t *testing.T) {
	var ticks []tick
	for m := 0; m <= 30; m++ {
		ticks = append(ticks, tick{at: time.Duration(m) * time.Minute, committed: int64(m) * 5000, lag: true})
	}
	got := runDrainTicks(t, ticks)
	assertAlarms(t, got, make([]bool, len(ticks)))
}

// A dead or wedged sink never commits, so a backlog that does not move must alarm once the
// stall window has passed, and keep alarming.
func TestSinkDrain_BacklogThatStopsMovingAlarmsAfterTheWindow(t *testing.T) {
	got := runDrainTicks(t, []tick{
		{at: 0, committed: 100, lag: true},                // first reading: starts the clock
		{at: 1 * time.Minute, committed: 100, lag: true},  // 1m flat
		{at: 4 * time.Minute, committed: 100, lag: true},  // 4m flat
		{at: 5 * time.Minute, committed: 100, lag: true},  // 5m flat: alarm
		{at: 10 * time.Minute, committed: 100, lag: true}, // still stuck: still alarms
	})
	assertAlarms(t, got, []bool{false, false, false, true, true})
}

// Once the sink commits again the alarm clears, and a later stall needs a full window again.
func TestSinkDrain_MovingAgainClearsTheAlarmAndRestartsTheClock(t *testing.T) {
	got := runDrainTicks(t, []tick{
		{at: 0, committed: 100, lag: true},
		{at: 6 * time.Minute, committed: 100, lag: true},  // stuck: alarm
		{at: 7 * time.Minute, committed: 900, lag: true},  // moved: clear
		{at: 11 * time.Minute, committed: 900, lag: true}, // 4m flat: not yet
		{at: 12 * time.Minute, committed: 900, lag: true}, // 5m flat: alarm
	})
	assertAlarms(t, got, []bool{false, true, false, false, true})
}

// A sink idle for hours on a quiet stream has a flat committed position. A burst that lands
// just before a tick must not be reported as hours of being stuck.
func TestSinkDrain_IdleSinkHitByABurstIsNotStuckYet(t *testing.T) {
	got := runDrainTicks(t, []tick{
		{at: 0, committed: 100, lag: false},
		{at: 3 * time.Hour, committed: 100, lag: false},
		{at: 3*time.Hour + time.Minute, committed: 100, lag: true}, // burst, not committed yet
		{at: 3*time.Hour + 6*time.Minute, committed: 100, lag: true},
	})
	assertAlarms(t, got, []bool{false, false, false, true})
}

// The first reading after an orchestrator restart says nothing about how long the sink has
// been stuck, and neither does a committed position that went backwards (group reset).
func TestSinkDrain_FirstReadingAndResetOnlyStartTheClock(t *testing.T) {
	got := runDrainTicks(t, []tick{
		{at: 0, committed: 5000, lag: true},
		{at: 6 * time.Minute, committed: 10, lag: true}, // reset backwards: restart clock
		{at: 7 * time.Minute, committed: 10, lag: true},
		{at: 11 * time.Minute, committed: 10, lag: true},
	})
	assertAlarms(t, got, []bool{false, false, false, true})
}

// Pipelines keep separate clocks: one stuck sink must not raise or clear another's alarm.
func TestSinkDrain_PipelinesAreTrackedSeparately(t *testing.T) {
	t.Setenv("CDC_SINK_DRAIN_STALL_AFTER", "5m")
	s := &CDCSentinel{}
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	s.observeSinkDrain("stuck", 1, true, start)
	s.observeSinkDrain("busy", 1, true, start)
	if stuck, _, _ := s.observeSinkDrain("stuck", 1, true, start.Add(6*time.Minute)); !stuck {
		t.Fatal("stuck pipeline did not alarm after 6m without committing")
	}
	if busy, _, _ := s.observeSinkDrain("busy", 2000, true, start.Add(6*time.Minute)); busy {
		t.Fatal("a pipeline that committed alarmed because another pipeline was stuck")
	}
}

// sinkFlushWindow is how long the sink holds a cloud-storage batch open before it writes the
// file (kafka-sink-worker main.go maxObjectFlushIntervalSeconds), and sinkWriteTime the
// slowest write the stall bound promises to tolerate (DefaultSinkDrainStallAfter's comment).
const (
	sinkFlushWindow = 240 * time.Second
	sinkWriteTime   = 120 * time.Second
)

// sinkBatch is one batch as Kafka sees it: lag from the moment its first change lands until
// the sink commits it. A batch that is never committed has committedAt < 0.
type sinkBatch struct{ openedAt, committedAt time.Duration }

// pollSink runs the sentinel's poll over a simulated sink -- ticks every `every`, the first
// at `phase` -- and returns when the alarm first fired (ok=false if it never did).
func pollSink(t *testing.T, batches []sinkBatch, every, phase, until time.Duration) (time.Duration, bool) {
	t.Helper()
	s := &CDCSentinel{}
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for at := phase; at <= until; at += every {
		var committed int64
		backlog := false
		for _, b := range batches {
			done := b.committedAt >= 0 && b.committedAt <= at
			if done {
				committed++
			}
			if b.openedAt <= at && !done {
				backlog = true
			}
		}
		if alarm, _, _ := s.observeSinkDrain("p1", committed, backlog, start.Add(at)); alarm {
			return at, true
		}
	}
	return 0, false
}

// sweepHealthySinks is every healthy shape the stall bound must sit through, at both poll
// intervals and every burst offset against the tick in 5 s steps: one burst on an idle sink
// whose batch is held for the full flush window and then takes the slowest write, and a
// steady trickle that keeps a backlog open forever while committing once per window. It
// returns the first shape that alarmed.
func sweepHealthySinks(t *testing.T) (string, bool) {
	t.Helper()
	burstAt := 10 * time.Minute
	for _, every := range []time.Duration{time.Minute, 2 * time.Minute} {
		for phase := time.Duration(0); phase < every; phase += 5 * time.Second {
			burst := []sinkBatch{{openedAt: burstAt, committedAt: burstAt + sinkFlushWindow + sinkWriteTime}}
			if at, fired := pollSink(t, burst, every, phase, burstAt+time.Hour); fired {
				return fmt.Sprintf("one burst, tick %s, phase %s: alarm at %s", every, phase, at), true
			}

			var trickle []sinkBatch
			for opened := time.Duration(0); opened < 2*time.Hour; opened += sinkFlushWindow + sinkWriteTime {
				trickle = append(trickle, sinkBatch{openedAt: opened, committedAt: opened + sinkFlushWindow + sinkWriteTime})
			}
			if at, fired := pollSink(t, trickle, every, phase, 2*time.Hour); fired {
				return fmt.Sprintf("steady trickle, tick %s, phase %s: alarm at %s", every, phase, at), true
			}
		}
	}
	return "", false
}

// A healthy sink holding a cloud-storage batch for its whole flush window must never alarm,
// whatever the burst's offset against the poll. Five minutes failed this (the control below),
// which is why the default is seven.
func TestSinkDrain_FlushWindowNeverAlarmsAHealthySink(t *testing.T) {
	t.Setenv("CDC_SINK_DRAIN_STALL_AFTER", "") // the default, whatever the environment says
	if shape, fired := sweepHealthySinks(t); fired {
		t.Fatalf("healthy sink alarmed at the %s default: %s", DefaultSinkDrainStallAfter, shape)
	}

	// Control: the sweep must be able to fail, and it does at the old five-minute bound.
	t.Setenv("CDC_SINK_DRAIN_STALL_AFTER", "5m")
	if _, fired := sweepHealthySinks(t); !fired {
		t.Fatal("control: the sweep found no false alarm at 5m, so it cannot tell a safe bound from an unsafe one")
	}
}

// A dead sink must still alarm, and within one poll of the stall bound from the burst: the
// tick that last saw no backlog can come up to one interval before the burst lands.
func TestSinkDrain_DeadSinkAlarmsWithinTheBoundPlusOneTick(t *testing.T) {
	t.Setenv("CDC_SINK_DRAIN_STALL_AFTER", "")
	burstAt := 10 * time.Minute
	dead := []sinkBatch{{openedAt: burstAt, committedAt: -1}}
	for _, every := range []time.Duration{time.Minute, 2 * time.Minute} {
		for phase := time.Duration(0); phase < every; phase += 5 * time.Second {
			at, fired := pollSink(t, dead, every, phase, burstAt+time.Hour)
			if !fired {
				t.Fatalf("tick %s, phase %s: a sink that never commits never alarmed", every, phase)
			}
			if late := at - burstAt; late > DefaultSinkDrainStallAfter+every {
				t.Fatalf("tick %s, phase %s: alarmed %s after the burst, want within %s", every, phase, late, DefaultSinkDrainStallAfter+every)
			}
		}
	}
}

// The alarm is one issue that escalates: a warning from the stall bound, a pushed critical
// from the notify bound.
func TestSinkDrainAlert_EscalatesAtTheNotifyBound(t *testing.T) {
	notify := 15 * time.Minute
	for _, c := range []struct {
		stalled  time.Duration
		severity IssueSeverity
		alert    string
	}{
		{7 * time.Minute, IssueSeverityWarning, "sink_lag"},
		{notify - time.Second, IssueSeverityWarning, "sink_lag"},
		{notify, IssueSeverityCritical, "sink_not_draining"},
		{2 * time.Hour, IssueSeverityCritical, "sink_not_draining"},
	} {
		if sev, alert := sinkDrainAlert(c.stalled, notify); sev != c.severity || alert != c.alert {
			t.Errorf("stalled %s: got %s/%s, want %s/%s", c.stalled, sev, alert, c.severity, c.alert)
		}
	}
}
