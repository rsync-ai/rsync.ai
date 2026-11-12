package sentinel

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
)

// The bug this file guards (KI-CDC-MONGO-RESUME-TOKEN-SILENT-STALL): a MongoDB source
// connector whose resume token had aged out of the oplog retried the same permanent
// error roughly ten times a minute for three days, never left RUNNING, and so reported a
// fully green /status the entire time. Nothing in rsync.ai noticed, because everything that
// looks for trouble was looking at the connector's STATE.
//
// The freshness alarm looks at the connector's committed source POSITION instead, which
// is the one thing a looping connector cannot fake. Two properties make that sound, and
// both are easy to break by accident, so both are pinned here:
//
//   - It must not alarm on a merely quiet source. That is what the heartbeat gate is
//     for, and why the first reading only ever starts the clock.
//   - It must alarm on a frozen one, and keep alarming — a watchdog that fires once and
//     then forgets is indistinguishable from one that never fired.

// freshTick is one sentinel poll as the freshness alarm sees it.
type freshTick struct {
	at          time.Duration // since the first tick
	fingerprint string
	// unobserved says no poll read this connector since the previous tick (it was paused,
	// stopped, or Connect did not answer). Otherwise the sentinel polled it every
	// freshnessTestPoll in between and saw the previous tick's position.
	unobserved bool
}

// freshnessTestPoll is the poll interval the tick helpers run the sentinel at: the
// shipped SentinelPollInterval, so the observation-gap rule sees real cadence.
const freshnessTestPoll = SentinelPollInterval

// testHeartbeatEvery is the shipped heartbeat interval (heartbeat.interval.ms=300000).
// Four of them is the 20m bound the tick tests set, so it never moves that bound.
const testHeartbeatEvery = 5 * time.Minute

// runFreshnessTicks feeds ticks through observeSourceFreshness the way
// checkSourceFreshness does, and returns each tick's alarm verdict.
func runFreshnessTicks(t *testing.T, ticks []freshTick) []bool {
	t.Helper()
	return runFreshnessTicksFor(t, "conn-1", ticks)
}

// runFreshnessTicksFor also replays the polls between two ticks, at the previous tick's
// position, unless the later tick is marked unobserved. A test lists only the readings
// it asserts on; the sentinel in production reads the connector on every poll.
func runFreshnessTicksFor(t *testing.T, connector string, ticks []freshTick) []bool {
	t.Helper()
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	s := &CDCSentinel{pollInterval: freshnessTestPoll} // no map on purpose: a bare literal must not panic
	got := make([]bool, len(ticks))
	for i, tk := range ticks {
		if i > 0 && !tk.unobserved {
			prev := ticks[i-1]
			for at := prev.at + freshnessTestPoll; at < tk.at; at += freshnessTestPoll {
				s.observeSourceFreshness(connector, prev.fingerprint, testHeartbeatEvery, freshnessStart.Add(at))
			}
		}
		got[i], _ = s.observeSourceFreshness(connector, tk.fingerprint, testHeartbeatEvery, freshnessStart.Add(tk.at))
	}
	return got
}

var freshnessStart = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func assertFreshnessAlarms(t *testing.T, got, want []bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d verdicts, want %d (got %v, want %v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tick %d: alarm=%v, want %v (all: got %v, want %v)", i, got[i], want[i], got, want)
		}
	}
}

// A heartbeating connector advances its position on a timer whether or not the source is
// writing, so a position that keeps changing is proof of life no matter how quiet the
// database is.
func TestSourceFreshness_AdvancingPositionNeverAlarms(t *testing.T) {
	var ticks []freshTick
	for m := 0; m <= 90; m += 5 {
		ticks = append(ticks, freshTick{
			at:          time.Duration(m) * time.Minute,
			fingerprint: "token-" + strings.Repeat("a", m),
		})
	}
	got := runFreshnessTicks(t, ticks)
	assertFreshnessAlarms(t, got, make([]bool, len(ticks)))
}

// The live shape of the bug: Connect says RUNNING, the position never moves. The alarm
// must fire once the window has fully passed and must keep firing — this connector was
// stuck for three days, and an alarm that cleared itself after one tick would have been
// as useless as no alarm at all.
func TestSourceFreshness_FrozenPositionAlarmsAfterTheWindowAndStays(t *testing.T) {
	got := runFreshnessTicks(t, []freshTick{
		{at: 0, fingerprint: "tok-A"},                  // first reading: starts the clock
		{at: 5 * time.Minute, fingerprint: "tok-A"},    // 5m frozen
		{at: 19 * time.Minute, fingerprint: "tok-A"},   // 19m frozen: not yet
		{at: 20 * time.Minute, fingerprint: "tok-A"},   // 20m frozen: alarm (>= is deliberate)
		{at: 3 * 24 * time.Hour, fingerprint: "tok-A"}, // three days later: still alarming
	})
	assertFreshnessAlarms(t, got, []bool{false, false, false, true, true})
}

// Recovery has to be believed the moment the position moves, and a later freeze has to
// serve a full fresh window before it alarms again.
func TestSourceFreshness_MovingAgainClearsAndRestartsTheClock(t *testing.T) {
	got := runFreshnessTicks(t, []freshTick{
		{at: 0, fingerprint: "tok-A"},
		{at: 25 * time.Minute, fingerprint: "tok-A"}, // frozen: alarm
		{at: 26 * time.Minute, fingerprint: "tok-B"}, // moved: clear
		{at: 45 * time.Minute, fingerprint: "tok-B"}, // 19m frozen: not yet
		{at: 46 * time.Minute, fingerprint: "tok-B"}, // 20m frozen: alarm
	})
	assertFreshnessAlarms(t, got, []bool{false, true, false, false, true})
}

// The first reading after an orchestrator restart says nothing about how long the
// position has been frozen — the connector may have been streaming happily a second ago.
// Treating it as the start of a stall would alarm on every restart.
func TestSourceFreshness_FirstReadingOnlyStartsTheClock(t *testing.T) {
	// A single tick a week after the epoch: the elapsed time is enormous, and it still
	// must not alarm, because nothing was observed before it.
	got := runFreshnessTicks(t, []freshTick{{at: 7 * 24 * time.Hour, fingerprint: "tok-A"}})
	assertFreshnessAlarms(t, got, []bool{false})
}

// A paused or stopped connector stays registered in Connect, so forgetSourceFreshness
// keeps its bookkeeping, but it is not RUNNING and nobody observes it. On resume its
// committed position is the one it held before the pause, and until the first heartbeat
// commits a new one that position matches the remembered fingerprint, with a movingAt from
// before the pause. Without a gap check, a resume more than 20 minutes after a pause
// alarmed on its first tick. A gap in observation says nothing about whether the
// connector moved, so it must restart the clock, exactly like a first reading.
func TestSourceFreshnessRestartsTheClockAfterAStop(t *testing.T) {
	got := runFreshnessTicks(t, []freshTick{
		{at: 0, fingerprint: "tok-A"},
		{at: 1 * time.Minute, fingerprint: "tok-A"},                    // still streaming, pipeline then paused
		{at: 45 * time.Minute, fingerprint: "tok-A", unobserved: true}, // resumed 44m later, same position: not a stall
		{at: 64 * time.Minute, fingerprint: "tok-A"},                   // 19m frozen since the resume: not yet
		{at: 65 * time.Minute, fingerprint: "tok-A"},                   // 20m frozen since the resume: alarm
	})
	assertFreshnessAlarms(t, got, []bool{false, false, false, false, true})
}

// The gap that restarts the clock is a few missed polls, not one. A single slow or
// skipped tick (a Connect call that timed out) must not reset a stall that was about to
// fire, or a connector whose Connect is slow would never alarm.
func TestSourceFreshnessOneSkippedPollKeepsTheClock(t *testing.T) {
	got := runFreshnessTicks(t, []freshTick{
		{at: 0, fingerprint: "tok-A"},
		{at: 17 * time.Minute, fingerprint: "tok-A"},
		{at: 20 * time.Minute, fingerprint: "tok-A", unobserved: true}, // two polls missed, a 3-poll gap: still 20m frozen
	})
	assertFreshnessAlarms(t, got, []bool{false, false, true})
}

// decideSourceFreshnessAlarm's `seen` flag, exercised directly.
//
// Through observeSourceFreshness the flag looks redundant — a first reading has a zero
// prev.fingerprint, so the "position changed" branch resets the clock anyway. It stops
// being redundant the moment a caller passes an empty fingerprint, where without `seen`
// the zero movingAt would be read as "frozen since the zero time" and alarm instantly.
// sourceOffsetFingerprint never returns an empty string today; this pins the function's
// behaviour so that it stays correct if one ever does.
func TestDecideSourceFreshnessAlarm_UnseenConnectorNeverAlarms(t *testing.T) {
	var zero sourceFreshnessState
	next, alarm := decideSourceFreshnessAlarm(zero, false, "", freshnessStart, 20*time.Minute, 3*time.Minute)
	if alarm {
		t.Fatal("a connector observed for the first time alarmed")
	}
	if !next.movingAt.Equal(freshnessStart) {
		t.Fatalf("movingAt = %s, want the current tick %s — the clock must start now, not at the zero time",
			next.movingAt, freshnessStart)
	}
}

// Two connectors are tracked independently: one stuck pipeline must not implicate a
// healthy one, and a healthy one must not mask a stuck one.
func TestSourceFreshness_ConnectorsAreIndependent(t *testing.T) {
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	s := &CDCSentinel{pollInterval: time.Hour} // hourly polls: the sparse readings below are consecutive

	type step struct {
		conn      string
		at        time.Duration
		fp        string
		wantAlarm bool
	}
	for i, st := range []step{
		{"stuck", 0, "frozen", false},
		{"healthy", 0, "moving-0", false},
		{"stuck", 25 * time.Minute, "frozen", true},
		{"healthy", 25 * time.Minute, "moving-1", false},
		{"stuck", 30 * time.Minute, "frozen", true},
		{"healthy", 30 * time.Minute, "moving-2", false},
	} {
		alarm, _ := s.observeSourceFreshness(st.conn, st.fp, testHeartbeatEvery, freshnessStart.Add(st.at))
		if alarm != st.wantAlarm {
			t.Fatalf("step %d (%s @ %s): alarm=%v, want %v", i, st.conn, st.at, alarm, st.wantAlarm)
		}
	}
}

// The reported duration is what lands in the issue description and metadata, so it has to
// be the age of the freeze, not the age of the connector.
func TestSourceFreshness_ReportsHowLongThePositionHasBeenFrozen(t *testing.T) {
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	s := &CDCSentinel{pollInterval: time.Hour} // hourly polls: the sparse readings below are consecutive

	s.observeSourceFreshness("c", "tok-A", testHeartbeatEvery, freshnessStart)
	s.observeSourceFreshness("c", "tok-B", testHeartbeatEvery, freshnessStart.Add(time.Hour)) // moved at T+1h
	_, frozenFor := s.observeSourceFreshness("c", "tok-B", testHeartbeatEvery, freshnessStart.Add(90*time.Minute))

	if frozenFor != 30*time.Minute {
		t.Fatalf("frozen for %s, want 30m — the clock must run from the last MOVE, not from the first sighting", frozenFor)
	}
}

// A connector deleted and recreated under the same name (which is what "delete the
// pipeline and create it again" does) must not inherit the dead one's frozen-since
// timestamp and alarm immediately.
func TestSourceFreshness_ForgettingAConnectorResetsItsClock(t *testing.T) {
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	s := &CDCSentinel{}

	s.observeSourceFreshness("gone", "tok-A", testHeartbeatEvery, freshnessStart)
	s.observeSourceFreshness("kept", "tok-A", testHeartbeatEvery, freshnessStart)

	s.forgetSourceFreshness(map[string]bool{"kept": true})

	if _, ok := s.sourceFreshness["gone"]; ok {
		t.Fatal("forgetSourceFreshness kept a connector that is no longer running")
	}
	if _, ok := s.sourceFreshness["kept"]; !ok {
		t.Fatal("forgetSourceFreshness dropped a connector that is still running")
	}

	// The recreated connector reports the same position it had before (a fresh snapshot
	// can legitimately produce one), 25 minutes later. Because its history was dropped,
	// this is a first reading and must only start the clock.
	alarm, _ := s.observeSourceFreshness("gone", "tok-A", testHeartbeatEvery, freshnessStart.Add(25*time.Minute))
	if alarm {
		t.Fatal("a recreated connector alarmed on its first reading — it inherited the deleted one's clock")
	}
}

// forgetSourceFreshness runs on a Sentinel that has never observed anything (the startup
// window, and every unit test that does not touch freshness).
func TestSourceFreshness_ForgetOnAnEmptySentinelDoesNotPanic(t *testing.T) {
	s := &CDCSentinel{}
	s.forgetSourceFreshness(map[string]bool{"anything": true})
}

func TestSourceOffsetFingerprint(t *testing.T) {
	decode := func(t *testing.T, raw string) map[string]interface{} {
		t.Helper()
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("fixture is not valid JSON: %v", err)
		}
		return m
	}

	// A connector that has committed nothing yet is starting up, not stalled. Every one
	// of these must be a skip, because alarming on them would fire on every pipeline
	// launch.
	for name, raw := range map[string]string{
		"missing offsets key": `{}`,
		"null offsets":        `{"offsets":null}`,
		"empty offsets":       `{"offsets":[]}`,
		"offsets not a list":  `{"offsets":{"resume_token":"x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := sourceOffsetFingerprint(decode(t, raw)); ok {
				t.Fatalf("%s produced a usable fingerprint; want a skip", name)
			}
		})
	}

	t.Run("nil payload", func(t *testing.T) {
		if _, ok := sourceOffsetFingerprint(nil); ok {
			t.Fatal("a nil payload produced a usable fingerprint; want a skip")
		}
	})

	// The real KIP-875 shape from the stalled pipeline on the demo VM.
	live := `{"offsets":[{"partition":{"server_id":"cdc-67b8ac8b"},` +
		`"offset":{"resume_token":"826D0A1F000000012B0429","sec":-1,"ord":-1}}]}`

	t.Run("a committed position fingerprints", func(t *testing.T) {
		fp, ok := sourceOffsetFingerprint(decode(t, live))
		if !ok {
			t.Fatal("a committed position did not produce a fingerprint")
		}
		if !strings.Contains(fp, "826D0A1F000000012B0429") {
			t.Fatalf("fingerprint %q does not carry the committed position", fp)
		}
	})

	// The alarm compares fingerprints across ticks, so the encoding must not depend on
	// map iteration order — otherwise a frozen connector would look like it is moving on
	// roughly every tick and the alarm would never fire. encoding/json sorts map keys;
	// this pins that we still rely on it.
	t.Run("stable across decodes", func(t *testing.T) {
		reordered := `{"offsets":[{"offset":{"ord":-1,"sec":-1,"resume_token":"826D0A1F000000012B0429"},` +
			`"partition":{"server_id":"cdc-67b8ac8b"}}]}`
		a, okA := sourceOffsetFingerprint(decode(t, live))
		b, okB := sourceOffsetFingerprint(decode(t, reordered))
		if !okA || !okB {
			t.Fatal("expected both payloads to fingerprint")
		}
		if a != b {
			t.Fatalf("the same position fingerprinted differently:\n  %s\n  %s", a, b)
		}
	})

	// And it must still change when the position actually changes.
	t.Run("changes when the position moves", func(t *testing.T) {
		moved := strings.Replace(live, "826D0A1F000000012B0429", "826D0A20000000012B0429", 1)
		a, _ := sourceOffsetFingerprint(decode(t, live))
		b, _ := sourceOffsetFingerprint(decode(t, moved))
		if a == b {
			t.Fatal("two different committed positions produced the same fingerprint")
		}
	})
}

// The heartbeat gate is what separates "idle" from "dead". If it ever returns true for a
// connector without heartbeats, every quiet source in every deployment starts alarming.
func TestSourceHeartbeatEnabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]interface{}
		want   bool
	}{
		{"nil config", nil, false},
		{"key absent", map[string]interface{}{"connector.class": "io.debezium.connector.mongodb.MongoDbConnector"}, false},
		{"empty string", map[string]interface{}{"heartbeat.interval.ms": ""}, false},
		{"zero disables heartbeats", map[string]interface{}{"heartbeat.interval.ms": "0"}, false},
		{"negative", map[string]interface{}{"heartbeat.interval.ms": "-1"}, false},
		{"not a number", map[string]interface{}{"heartbeat.interval.ms": "5m"}, false},
		{"nil value", map[string]interface{}{"heartbeat.interval.ms": nil}, false},
		// Connect returns config values as strings; a hand-built config or a test may
		// carry a number. Both mean the same thing.
		{"the shipped default, as a string", map[string]interface{}{"heartbeat.interval.ms": "300000"}, true},
		{"padded", map[string]interface{}{"heartbeat.interval.ms": " 300000 "}, true},
		{"as an int", map[string]interface{}{"heartbeat.interval.ms": 300000}, true},
		// JSON decodes numbers as float64. fmt.Sprint renders this one as "300000"
		// (no exponent at this magnitude), so it still reads as enabled.
		{"as a JSON number", map[string]interface{}{"heartbeat.interval.ms": float64(300000)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceHeartbeatEnabled(tc.config); got != tc.want {
				t.Fatalf("sourceHeartbeatEnabled(%v) = %v, want %v", tc.config, got, tc.want)
			}
		})
	}
}

// #12 gave PostgreSQL sources a heartbeat. It acknowledges filtered WAL, but on a
// database with no writes it repeats the same LSN, so its position alone is not a
// liveness beacon. Only MongoDB's heartbeat moves an idle position; PostgreSQL and MySQL
// are judged by their heartbeat topic as well (TestSourceFreshness_PostgresHeartbeatTopicFrozenAlarms).
func TestHeartbeatAdvancesAnIdlePosition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]interface{}
		want   bool
	}{
		{"mongodb", map[string]interface{}{"connector.class": "io.debezium.connector.mongodb.MongoDbConnector", "heartbeat.interval.ms": "300000"}, true},
		{"postgresql", map[string]interface{}{"connector.class": "io.debezium.connector.postgresql.PostgresConnector", "heartbeat.interval.ms": "300000"}, false},
		{"mysql", map[string]interface{}{"connector.class": "io.debezium.connector.mysql.MySqlConnector"}, false},
		{"class absent", map[string]interface{}{"heartbeat.interval.ms": "300000"}, false},
		{"class not a string", map[string]interface{}{"connector.class": 7}, false},
		{"nil config", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := heartbeatAdvancesAnIdlePosition(tc.config); got != tc.want {
				t.Fatalf("heartbeatAdvancesAnIdlePosition(%v) = %v, want %v", tc.config, got, tc.want)
			}
		})
	}
}

// The issue id must be its own class. Colliding with cdc-connector-down-* would let the
// connector-down resolver clear a stall that Connect never admitted to, which is exactly
// the blindness this alarm exists to fix.
func TestSourceStalledIssueIDIsItsOwnClass(t *testing.T) {
	id := sourceStalledIssueID("67b8ac8b")
	if !strings.Contains(id, "67b8ac8b") {
		t.Fatalf("issue id %q does not identify the pipeline", id)
	}
	if id == connectorIssueID("67b8ac8b") {
		t.Fatal("the stall issue id collides with the connector-down issue id")
	}
	if a, b := sourceStalledIssueID("aaa"), sourceStalledIssueID("bbb"); a == b {
		t.Fatal("two pipelines share one stall issue id")
	}
}

// diagnosableErrorText is the §3A half of the fix: the Healer builds its diagnose.Signal
// from the issue DESCRIPTION and never reads metadata (heal/issue_sweep.go), so an error
// that lives only in metadata is an error rsync.ai cannot classify. These tests pin the two
// things that makes it useful — the cause survives, and nothing secret does.
func TestDiagnosableErrorText(t *testing.T) {
	t.Run("keeps the first line and every cause, drops the frames", func(t *testing.T) {
		trace := strings.Join([]string{
			"org.apache.kafka.connect.errors.ConnectException: An exception occurred in the change event producer",
			"\tat io.debezium.pipeline.ErrorHandler.setProducerThrowable(ErrorHandler.java:52)",
			"\tat io.debezium.connector.mongodb.MongoDbStreamingChangeEventSource.lambda$0(MongoDbStreamingChangeEventSource.java:171)",
			"Caused by: com.mongodb.MongoCommandException: Command failed with error 286 (ChangeStreamHistoryLost): 'Resume of change stream was not possible, as the resume point may no longer be in the oplog.'",
			"\tat com.mongodb.internal.connection.ProtocolHelper.getCommandFailureException(ProtocolHelper.java:205)",
			"Caused by: java.lang.IllegalStateException: change stream was closed",
			"\tat java.base/java.lang.Thread.run(Thread.java:840)",
		}, "\n")

		got := diagnosableErrorText(trace, maxIssueErrorText)

		for _, want := range []string{
			"ConnectException",
			"ChangeStreamHistoryLost",
			"change stream was closed",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("the diagnosable text lost %q:\n%s", want, got)
			}
		}

		// Known and accepted: llmscrub redacts single-quoted literals, because in a
		// Postgres or MySQL error that is where a row value lives. MongoDB puts its
		// human-readable message there too ('Resume of change stream was not possible…'),
		// so that sentence does not survive. It is not worth widening the scrubber for:
		// the unquoted error NAME is what diagnose.go matches on, and it does survive.
		// This assertion exists so that a future change to llmscrub that starts eating
		// the error name is caught here rather than in production.
		if strings.Contains(got, "resume point may no longer be in the oplog") {
			t.Log("note: llmscrub no longer redacts quoted literals; the assertion above can be tightened")
		}
		if strings.Contains(got, "ErrorHandler.java") || strings.Contains(got, "Thread.java") {
			t.Fatalf("stack frames survived and will crowd out the cause:\n%s", got)
		}
		if n := strings.Count(got, " | "); n != 2 {
			t.Fatalf("expected 3 joined lines (1 header + 2 causes), got %d separators:\n%s", n, got)
		}
	})

	// This is the whole point: what lands in the description must match the rule that
	// already knows what to do about it. Without this the description says only that a
	// connector failed, which matches nothing.
	t.Run("the result is what the re-snapshot rule matches on", func(t *testing.T) {
		got := strings.ToLower(diagnosableErrorText(
			"Caused by: com.mongodb.MongoCommandException: Command failed with error 286 (ChangeStreamHistoryLost)",
			maxIssueErrorText))
		if !strings.Contains(got, "changestreamhistorylost") {
			t.Fatalf("diagnose.go's re-snapshot rule will not match this text:\n%s", got)
		}
	})

	t.Run("credentials never survive", func(t *testing.T) {
		got := diagnosableErrorText(
			"ConnectException: cannot reach mongodb://rsync_cdc:hunter2@cluster0.abcd.mongodb.net:27017/?replicaSet=rs0",
			maxIssueErrorText)
		if strings.Contains(got, "hunter2") {
			t.Fatalf("a password reached an issue description that the chat diagnoser reads back:\n%s", got)
		}
		if !strings.Contains(got, "[redacted]@") {
			t.Fatalf("expected the connection URI's userinfo to be masked:\n%s", got)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		// Prose, not a long alphanumeric run: llmscrub masks the latter as suspected
		// base64 and the input would never reach the cap, making this test vacuous.
		got := diagnosableErrorText(
			"ConnectException: "+strings.Repeat("the connector could not reach the source. ", 60), 100)
		if n := len([]rune(got)); n > 101 { // 100 runes + the ellipsis ScrubMax appends
			t.Fatalf("length %d exceeds the cap", n)
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("a truncated description should say it was truncated: %q", got)
		}
	})

	t.Run("empty and whitespace-only traces add nothing", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\n\n\t\n"} {
			if got := diagnosableErrorText(in, maxIssueErrorText); got != "" {
				t.Fatalf("diagnosableErrorText(%q) = %q, want \"\" — a trailing \": \" in the description helps nobody", in, got)
			}
		}
	})
}

// getDebeziumStatus stores the harvested trace in a map; a connector state that carries
// no trace (the live shape of this bug — Connect exposed none) must degrade to "no extra
// text", never to a panic or a stringified map.
func TestTraceFromConnectorState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state interface{}
		want  string
	}{
		{"nil", nil, ""},
		{"not a map", "RUNNING", ""},
		{"map without a trace", map[string]interface{}{"state": "RUNNING"}, ""},
		{"trace is not a string", map[string]interface{}{"trace": 42}, ""},
		{"trace present", map[string]interface{}{"state": "RUNNING", "trace": "boom"}, "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceFromConnectorState(tc.state); got != tc.want {
				t.Fatalf("traceFromConnectorState(%v) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

// The alarm's description must not decide the diagnosis. A frozen position has several
// possible causes (unreachable source, a permanent error retried forever, a lost resume
// token); a phrase in the boilerplate that matches one of diagnose.go's rules would make
// the Healer confidently pick that one every single time, including for a network
// partition. So the bare description must fall through to "nothing matched", and only the
// connector's own error text, appended after it, may steer the classification.
func TestSourceStalledDescriptionDoesNotPreJudgeTheCause(t *testing.T) {
	d := diagnose.New()
	diagnoseText := func(description string) diagnose.Diagnosis {
		// Built as heal/issue_sweep.go builds it for a sentinel issue.
		return d.Diagnose(diagnose.Signal{
			PipelineID:     hbTestPipeline,
			ErrorMessage:   description,
			ExecutorStatus: string(IssueTypeSourceStreamStalled),
			Stage:          "sentinel",
			LastEvents:     []string{"sentinel issue source_stream_stalled (severity critical)"},
		})
	}
	for _, tc := range []struct {
		rung             string
		byHeartbeatTopic bool
	}{
		{"mongodb position", false},
		{"postgres/mysql heartbeat topic", true},
	} {
		t.Run(tc.rung, func(t *testing.T) {
			description := sourceStalledDescription(hbTestConnector, 21*time.Minute, testHeartbeatEvery, tc.byHeartbeatTopic)
			// Non-vacuity: this really is the stall description.
			if !strings.Contains(description, "has not advanced its committed source position") {
				t.Fatalf("not the stall description:\n%s", description)
			}
			if got := diagnoseText(description); got.Category != diagnose.CategoryUnknown || got.SuggestedAction != diagnose.ActionEscalate {
				t.Fatalf("the bare description already matches a diagnose.go rule (%s/%s: %s), so every stall "+
					"would get that action whatever its cause:\n%s", got.Category, got.SuggestedAction, got.Rationale, description)
			}
			// And the appended connector error decides it, including for a rule that sits
			// below the provisioning rule (network), which a provisioning phrase in the
			// boilerplate would shadow.
			for cause, want := range map[string]diagnose.Action{
				"Caused by: com.mongodb.MongoCommandException: Command failed with error 286 (ChangeStreamHistoryLost)":    diagnose.ActionReSnapshot,
				"Caused by: org.postgresql.util.PSQLException: Connection to db.internal:5432 refused: connection refused": diagnose.ActionBackoffRetry,
			} {
				if got := diagnoseText(description + ": " + cause); got.SuggestedAction != want {
					t.Errorf("connector error %q → %s, want %s: the description is shadowing it", cause, got.SuggestedAction, want)
				}
			}
		})
	}
	if got := sourceStalledDescription(hbTestConnector, 21*time.Minute, testHeartbeatEvery, true); !strings.Contains(got, "every 5m0s") {
		t.Fatalf("the heartbeat-topic description does not say how often the connector heartbeats:\n%s", got)
	}
}

// The executor names the topic <topic.heartbeat.prefix>.<topic.prefix> (heartbeatTopicFor),
// which is also Debezium's own rule; the legacy heartbeat.topics.prefix key names nothing.
func TestSourceHeartbeatTopic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]interface{}
		want   string
		wantOK bool
	}{
		{"as the executor builds it", postgresHeartbeatConfig(), hbTestTopic, true},
		{"legacy key only: Debezium's default prefix", map[string]interface{}{
			"topic.prefix": "rsync.cdc-aaaaaaaa", "heartbeat.topics.prefix": "rsync.heartbeat",
		}, "__debezium-heartbeat.rsync.cdc-aaaaaaaa", true},
		{"custom delimiter", map[string]interface{}{
			"topic.prefix": "p", "topic.heartbeat.prefix": "hb", "topic.delimiter": "_",
		}, "hb_p", true},
		{"no topic.prefix", map[string]interface{}{"topic.heartbeat.prefix": "rsync.heartbeat"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sourceHeartbeatTopic(tc.config)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("sourceHeartbeatTopic = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The offset keys are Debezium 3.1's: PostgreSQL writes snapshot=<type>,
// last_snapshot_record and snapshot_completed while snapshotting; the binlog connectors
// write snapshot=<type>; connectors before 2.x wrote snapshot=true.
func TestSourceOffsetsMidSnapshot(t *testing.T) {
	offsets := func(offset map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"offsets": []interface{}{map[string]interface{}{
			"partition": map[string]interface{}{"server": "p"}, "offset": offset,
		}}}
	}
	for _, tc := range []struct {
		name    string
		payload map[string]interface{}
		want    bool
	}{
		{"postgres mid-snapshot", offsets(map[string]interface{}{"lsn": 1.0, "snapshot": "INITIAL", "last_snapshot_record": false}), true},
		{"postgres last snapshot record", offsets(map[string]interface{}{"lsn": 1.0, "snapshot": "INITIAL", "last_snapshot_record": true}), false},
		{"postgres streaming", streamingPostgresOffsets(), false},
		{"mysql mid-snapshot", offsets(map[string]interface{}{"file": "binlog.000003", "pos": 157.0, "snapshot": "INITIAL"}), true},
		{"pre-2.x connector mid-snapshot", offsets(map[string]interface{}{"lsn": 1.0, "snapshot": true}), true},
		{"snapshot false", offsets(map[string]interface{}{"lsn": 1.0, "snapshot": false}), false},
		{"no offsets", map[string]interface{}{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceOffsetsMidSnapshot(tc.payload); got != tc.want {
				t.Fatalf("sourceOffsetsMidSnapshot = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSourceFreshnessStallBound(t *testing.T) {
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	for _, tc := range []struct {
		every, want time.Duration
	}{
		{0, 20 * time.Minute},
		{5 * time.Minute, 20 * time.Minute},
		{10 * time.Minute, 40 * time.Minute},
	} {
		if got := sourceFreshnessStallBound(tc.every); got != tc.want {
			t.Errorf("sourceFreshnessStallBound(%s) = %s, want %s", tc.every, got, tc.want)
		}
	}
}

// --- PostgreSQL and MySQL: the heartbeat topic is the beacon ---
//
// Their heartbeat does not move an idle connector's committed position (see
// TestHeartbeatAdvancesAnIdlePosition), so the position alone cannot tell a quiet source
// from a dead connector. Every heartbeat is still a record on the connector's heartbeat
// topic, though, and a connector retrying a permanent error writes none. So for these
// sources progress is the position OR the heartbeat topic's end offset moving.

const (
	hbTestPipeline  = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	hbTestConnector = "cdc-aaaaaaaa"
	// What the executor names it: heartbeatTopicFor(debeziumTopicPrefixFor(params)),
	// <topic.heartbeat.prefix>.<topic.prefix>, prefix FIRST.
	hbTestTopic = "rsync.heartbeat.rsync.cdc-aaaaaaaa"
)

// postgresHeartbeatConfig is the shape _build_config gives a PostgreSQL source.
func postgresHeartbeatConfig() map[string]interface{} {
	return map[string]interface{}{
		"connector.class":         "io.debezium.connector.postgresql.PostgresConnector",
		"heartbeat.interval.ms":   "300000",
		"topic.prefix":            "rsync.cdc-aaaaaaaa",
		"topic.heartbeat.prefix":  "rsync.heartbeat",
		"heartbeat.topics.prefix": "rsync.heartbeat",
	}
}

// streamingPostgresOffsets is a committed PostgreSQL position after the snapshot.
func streamingPostgresOffsets() map[string]interface{} {
	return map[string]interface{}{"offsets": []interface{}{map[string]interface{}{
		"partition": map[string]interface{}{"server": "rsync.cdc-aaaaaaaa"},
		"offset":    map[string]interface{}{"lsn": float64(31458328), "txId": float64(812), "ts_usec": float64(1760600000000000)},
	}}}
}

// heartbeatHarness is a Kafka Connect serving one connector's config and committed
// offsets, plus a heartbeat topic whose end offset the test sets.
type heartbeatHarness struct {
	s       *CDCSentinel
	mock    sqlmock.Sqlmock
	config  map[string]interface{}
	offsets map[string]interface{}
	end     int64 // the heartbeat topic's end offset
	exists  bool  // whether the heartbeat topic exists
	asked   []string
}

func newHeartbeatHarness(t *testing.T) *heartbeatHarness {
	t.Helper()
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := &heartbeatHarness{mock: mock, config: postgresHeartbeatConfig(), offsets: streamingPostgresOffsets(), end: 42, exists: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connectors/" + hbTestConnector + "/config":
			_ = json.NewEncoder(w).Encode(h.config)
		case "/connectors/" + hbTestConnector + "/offsets":
			_ = json.NewEncoder(w).Encode(h.offsets)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	h.s = &CDCSentinel{db: db, connectURL: srv.URL, httpClient: srv.Client(), pollInterval: freshnessTestPoll}
	h.s.heartbeatEndOffset = func(topic string) (int64, bool, error) {
		h.asked = append(h.asked, topic)
		return h.end, h.exists, nil
	}
	return h
}

// tick is one sentinel poll of the connector, which Connect reports RUNNING.
func (h *heartbeatHarness) tick() {
	h.s.checkSourceFreshness(context.Background(), hbTestPipeline, hbTestConnector, nil)
}

// age pretends the connector's current reading has held for d, read on every poll.
func (h *heartbeatHarness) age(d time.Duration) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	st, ok := h.s.sourceFreshness[hbTestConnector]
	if !ok {
		return
	}
	st.movingAt = st.movingAt.Add(-d)
	st.observedAt = st.observedAt.Add(-freshnessTestPoll)
	h.s.sourceFreshness[hbTestConnector] = st
}

// observed reports whether any tick read the connector far enough to start its clock.
func (h *heartbeatHarness) observed() bool {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	_, ok := h.s.sourceFreshness[hbTestConnector]
	return ok
}

func (h *heartbeatHarness) expectResolved() {
	h.mock.ExpectQuery(regexp.QuoteMeta("DELETE FROM sentinel_active_issues WHERE id = $1 RETURNING severity, detected_at")).
		WithArgs(sourceStalledIssueID(hbTestPipeline)).
		WillReturnRows(sqlmock.NewRows([]string{"severity", "detected_at"})) // nothing was open
}

func (h *heartbeatHarness) expectStalled(detectedBy string) {
	h.mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO sentinel_active_issues")).
		WithArgs(
			sourceStalledIssueID(hbTestPipeline),
			string(IssueTypeSourceStreamStalled),
			string(IssueSeverityCritical),
			hbTestPipeline,
			string(ComponentTypeCDCPipeline),
			sqlmock.AnyArg(),
			detectedByArg(detectedBy),
		).
		WillReturnRows(detectedAtRow())
}

// detectedByArg matches the issue metadata JSON on its detected_by.
type detectedByArg string

func (d detectedByArg) Match(v driver.Value) bool {
	var b []byte
	switch x := v.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	default:
		return false
	}
	var meta map[string]interface{}
	if json.Unmarshal(b, &meta) != nil {
		return false
	}
	return meta["detected_by"] == string(d)
}

// A PostgreSQL connector Connect calls RUNNING whose position AND heartbeat topic are
// both frozen past the bound is stalled. Before the heartbeat-topic rung, no
// PostgreSQL or MySQL pipeline could raise this alarm at all.
func TestSourceFreshness_PostgresHeartbeatTopicFrozenAlarms(t *testing.T) {
	h := newHeartbeatHarness(t)
	// The first reading only starts the clock, and clears nothing: it has not seen a move.
	h.expectStalled("heartbeat_topic")

	h.tick()
	h.age(21 * time.Minute)
	h.tick()

	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a PostgreSQL connector frozen for 21m with no new heartbeat raised no stall: %v", err)
	}
	if len(h.asked) == 0 {
		t.Fatal("the heartbeat topic was never read")
	}
	for _, topic := range h.asked {
		if topic != hbTestTopic {
			t.Fatalf("read heartbeat topic %q, want %q (<topic.heartbeat.prefix>.<topic.prefix>)", topic, hbTestTopic)
		}
	}
}

// The controls. Each one would alarm if the rung judged the position alone, or judged a
// connector that is not streaming yet.
func TestSourceFreshness_IdleOrSnapshottingPostgresNeverAlarms(t *testing.T) {
	t.Run("idle source, heartbeats landing", func(t *testing.T) {
		h := newHeartbeatHarness(t)
		for i := 0; i < 3; i++ { // every reading after the first saw a heartbeat land
			h.expectResolved()
		}
		for i := 0; i < 4; i++ {
			h.tick()
			h.age(25 * time.Minute) // the position never moves...
			h.end++                 // ...but a heartbeat lands before every tick
		}
		if err := h.mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("an idle PostgreSQL source whose heartbeats keep landing was not read as alive: %v", err)
		}
	})

	// The executor pre-creates the topic, so a connector still in its first snapshot has a
	// topic with nothing on it.
	t.Run("heartbeat topic still empty", func(t *testing.T) {
		h := newHeartbeatHarness(t)
		h.end = 0
		h.tick()
		h.tick()
		if h.observed() {
			t.Fatal("a connector that has never written a heartbeat started a stall clock")
		}
	})

	t.Run("heartbeat topic not created", func(t *testing.T) {
		h := newHeartbeatHarness(t)
		h.exists = false
		h.tick()
		h.tick()
		if h.observed() {
			t.Fatal("a connector with no heartbeat topic started a stall clock")
		}
	})

	t.Run("heartbeat topic unreadable", func(t *testing.T) {
		h := newHeartbeatHarness(t)
		h.s.heartbeatEndOffset = func(string) (int64, bool, error) { return 0, true, errors.New("kafka: broker not available") }
		h.tick()
		h.tick()
		if h.observed() {
			t.Fatal("a Kafka read error started a stall clock")
		}
	})

	// A re-snapshot writes no heartbeats, but the topic still holds an earlier run's.
	t.Run("blocking snapshot running", func(t *testing.T) {
		h := newHeartbeatHarness(t)
		h.offsets = map[string]interface{}{"offsets": []interface{}{map[string]interface{}{
			"partition": map[string]interface{}{"server": "rsync.cdc-aaaaaaaa"},
			"offset": map[string]interface{}{
				"lsn": float64(31458328), "snapshot": "INITIAL",
				"last_snapshot_record": false, "snapshot_completed": false,
			},
		}}}
		h.tick()
		h.tick()
		if h.observed() {
			t.Fatal("a connector mid-snapshot started a stall clock")
		}
	})

	t.Run("no Kafka client", func(t *testing.T) {
		h := newHeartbeatHarness(t)
		h.s.heartbeatEndOffset = nil // and kafkaManager is nil, as before wiring
		h.tick()
		if h.observed() {
			t.Fatal("a sentinel that cannot read Kafka started a stall clock")
		}
	})
}

// A MongoDB heartbeat moves the position itself, so its rung never reads the topic.
func TestSourceFreshness_MongoNeverReadsTheHeartbeatTopic(t *testing.T) {
	h := newHeartbeatHarness(t)
	h.config = map[string]interface{}{
		"connector.class":       "io.debezium.connector.mongodb.MongoDbConnector",
		"heartbeat.interval.ms": "300000",
		"topic.prefix":          "rsync.cdc-aaaaaaaa",
	}
	h.expectStalled("cdc_sentinel_source_freshness")

	h.tick()
	h.age(21 * time.Minute)
	h.tick()

	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the MongoDB rung changed: %v", err)
	}
	if len(h.asked) != 0 {
		t.Fatalf("the MongoDB rung read the heartbeat topic %v", h.asked)
	}
}

// The bound is four heartbeats, never fewer: a connector configured to heartbeat every
// ten minutes is only stalled after forty, or one late heartbeat would page somebody.
func TestSourceFreshnessBoundCoversFourHeartbeats(t *testing.T) {
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "20m")
	s := &CDCSentinel{pollInterval: time.Hour} // hourly polls: the sparse readings below are consecutive

	every := 10 * time.Minute
	s.observeSourceFreshness("slow", "tok-A", every, freshnessStart)
	if alarm, _ := s.observeSourceFreshness("slow", "tok-A", every, freshnessStart.Add(25*time.Minute)); alarm {
		t.Fatal("alarmed after 25m on a 10m heartbeat: that is two and a half heartbeats, not a stall")
	}
	if alarm, _ := s.observeSourceFreshness("slow", "tok-A", every, freshnessStart.Add(40*time.Minute)); !alarm {
		t.Fatal("no alarm after four missed 10m heartbeats")
	}

	// The configured bound still wins when it is the longer one.
	t.Setenv("CDC_SOURCE_FRESHNESS_STALL_AFTER", "1h")
	s.observeSourceFreshness("patient", "tok-A", testHeartbeatEvery, freshnessStart)
	if alarm, _ := s.observeSourceFreshness("patient", "tok-A", testHeartbeatEvery, freshnessStart.Add(45*time.Minute)); alarm {
		t.Fatal("alarmed at 45m although CDC_SOURCE_FRESHNESS_STALL_AFTER is 1h")
	}
}
