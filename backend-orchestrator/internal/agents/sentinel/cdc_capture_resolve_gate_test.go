package sentinel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Pins when an alert about a CDC stream's capture side may close.
//
// The bug class: an alert closes on a signal that only says the connector is up, not that
// the stream moves. On prod on 2026-10-22 the KYC pipeline's capture side went down at
// 09:18 and the owner was paged. At 09:25 Kafka Connect said RUNNING again and the probe
// went healthy, so the owner was told "Change capture is running again". It was not: the
// MongoDB resume token had left the oplog, the connector retried without ever committing
// a position, and the stall alarm fired 21 minutes later.
//
// Every close in this class now waits for captureProvenMoving: two readings of the
// committed position, both taken after the capture was last seen down, that differ.
// Members: capture-stopped (surfaceCaptureDown), connector-down (the RUNNING branch of
// checkActivePipelines), and the stall alarm, which used to clear on a first reading.

const kycConnector = "cdc-11111111"

func mongoHeartbeatConfig() map[string]interface{} {
	return map[string]interface{}{
		"connector.class":       "io.debezium.connector.mongodb.MongoDbConnector",
		"heartbeat.interval.ms": "300000",
		"topic.prefix":          "rsync." + kycConnector,
	}
}

// mongoOffsets is a committed MongoDB position. Its heartbeat moves the resume token
// even on an idle collection, so a token that does not move is a stream that does not.
func mongoOffsets(token string) map[string]interface{} {
	return map[string]interface{}{"offsets": []interface{}{map[string]interface{}{
		"partition": map[string]interface{}{"server_id": "rsync." + kycConnector},
		"offset":    map[string]interface{}{"resume_token": token, "sec": float64(-1), "ord": float64(-1)},
	}}}
}

// --- the gate itself, at explicit times ---

func gateSentinel() *CDCSentinel {
	return &CDCSentinel{pollInterval: freshnessTestPoll}
}

func readAt(s *CDCSentinel, connector, token string, at time.Time) {
	fp, _ := sourceOffsetFingerprint(mongoOffsets(token))
	s.observeSourceFreshness(connector, fp, testHeartbeatEvery, at)
}

func TestCaptureProvenMoving(t *testing.T) {
	at := func(m int) time.Time { return freshnessStart.Add(time.Duration(m) * time.Minute) }

	t.Run("never seen down: running means running", func(t *testing.T) {
		s := gateSentinel()
		if !s.captureProvenMoving(kycConnector) {
			t.Fatal("a connector with no bookkeeping at all (a fresh orchestrator) was held back")
		}
		readAt(s, kycConnector, "A", at(0))
		readAt(s, kycConnector, "A", at(1))
		if !s.captureProvenMoving(kycConnector) {
			t.Fatal("a connector never seen down was held back")
		}
	})

	t.Run("frozen after the outage", func(t *testing.T) {
		s := gateSentinel()
		readAt(s, kycConnector, "A", at(0))
		s.noteCaptureDown(kycConnector, at(1))
		readAt(s, kycConnector, "A", at(2))
		readAt(s, kycConnector, "A", at(3))
		if s.captureProvenMoving(kycConnector) {
			t.Fatal("a resume token frozen since the outage was taken as proof the stream moves")
		}
	})

	t.Run("moved after the outage", func(t *testing.T) {
		s := gateSentinel()
		s.noteCaptureDown(kycConnector, at(0))
		readAt(s, kycConnector, "A", at(1))
		readAt(s, kycConnector, "B", at(2))
		if !s.captureProvenMoving(kycConnector) {
			t.Fatal("a token that moved between two post-outage readings was not taken as proof")
		}
	})

	// The first post-outage reading is compared with one taken before the outage, so the
	// change may be progress the stream made before it went down.
	t.Run("a move that straddles the outage proves nothing", func(t *testing.T) {
		s := gateSentinel()
		readAt(s, kycConnector, "A", at(0))
		s.noteCaptureDown(kycConnector, at(1))
		readAt(s, kycConnector, "B", at(2))
		if s.captureProvenMoving(kycConnector) {
			t.Fatal("a change first seen across the outage was taken as proof")
		}
		readAt(s, kycConnector, "C", at(3))
		if !s.captureProvenMoving(kycConnector) {
			t.Fatal("a change between two post-outage readings was not taken as proof")
		}
	})

	t.Run("a second outage voids the earlier proof", func(t *testing.T) {
		s := gateSentinel()
		s.noteCaptureDown(kycConnector, at(0))
		readAt(s, kycConnector, "A", at(1))
		readAt(s, kycConnector, "B", at(2))
		s.noteCaptureDown(kycConnector, at(3))
		if s.captureProvenMoving(kycConnector) {
			t.Fatal("proof from before the latest outage still counted")
		}
		readAt(s, kycConnector, "B", at(4))
		readAt(s, kycConnector, "C", at(5))
		if !s.captureProvenMoving(kycConnector) {
			t.Fatal("a move after the second outage was not taken as proof")
		}
	})

	// A connector this check cannot read (no heartbeats, no offsets endpoint) can never
	// show a move, so it is judged as before rather than held open for good.
	t.Run("a connector that cannot be read is judged as before, until it can be", func(t *testing.T) {
		s := gateSentinel()
		s.noteCaptureDown(kycConnector, at(0))
		s.markSourceFreshnessBlind(kycConnector)
		if !s.captureProvenMoving(kycConnector) {
			t.Fatal("a connector whose progress cannot be read was held back forever")
		}
		readAt(s, kycConnector, "A", at(1))
		if s.captureProvenMoving(kycConnector) {
			t.Fatal("a reading did not end the blind fallback")
		}
	})

	t.Run("bookkeeping edges", func(t *testing.T) {
		s := gateSentinel()
		s.noteCaptureDown("", at(0))
		s.markSourceFreshnessBlind("cdc-never-seen")
		if len(s.sourceFreshness) != 0 {
			t.Fatalf("no-op calls created entries: %v", s.sourceFreshness)
		}
		other := "cdc-22222222"
		s.noteCaptureDown(kycConnector, at(0))
		readAt(s, other, "A", at(1))
		readAt(s, other, "A", at(2))
		if !s.captureProvenMoving(other) {
			t.Fatal("one connector's outage held back another")
		}
	})
}

// The stall sibling: only a reading that differs from the one before clears the stall.
func TestSourceFreshnessMovedOnlyOnAChangedReading(t *testing.T) {
	s := gateSentinel()
	at := func(m int) time.Time { return freshnessStart.Add(time.Duration(m) * time.Minute) }
	for i, step := range []struct {
		at    time.Time
		token string
		moved bool
	}{
		{at(0), "A", false},  // first reading: nothing to compare with
		{at(1), "A", false},  // frozen
		{at(2), "B", true},   // moved
		{at(3), "B", false},  // frozen again
		{at(30), "B", false}, // first reading after a gap restarts the clock, proves nothing
	} {
		readAt(s, kycConnector, step.token, step.at)
		if got := s.sourceFreshnessMoved(kycConnector); got != step.moved {
			t.Fatalf("reading %d (%s): moved=%v, want %v", i, step.token, got, step.moved)
		}
	}
}

// --- the KYC sequence, through the Sentinel's real poll ---

// captureResolveHarness is one MongoDB CDC pipeline driven through checkActivePipelines:
// a Kafka Connect serving the connector's status, config and committed resume token,
// the dependency probe's verdict, and the alerts the owner has been paged for.
type captureResolveHarness struct {
	t    *testing.T
	s    *CDCSentinel
	sent *[]sentResolved

	mu       sync.Mutex
	connect  string // "RUNNING", "FAILED", or "" for Connect not answering
	token    string
	restarts int

	probeFails int                  // consecutive unhealthy probes; 0 = healthy
	open       map[string]time.Time // alert id -> when it was raised
}

func newCaptureResolveHarness(t *testing.T) *captureResolveHarness {
	t.Helper()
	h := &captureResolveHarness{t: t, sent: captureResolved(t), connect: "RUNNING", token: "826D0A1F000000012B0429", open: map[string]time.Time{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case r.URL.Path == "/connectors" && h.connect == "":
			http.Error(w, "connect worker unavailable", http.StatusServiceUnavailable)
		case r.URL.Path == "/connectors":
			task := `{"id":0,"state":"RUNNING","worker_id":"connect:8083"}`
			if h.connect == "FAILED" {
				task = `{"id":0,"state":"FAILED","worker_id":"connect:8083","trace":"org.apache.kafka.connect.errors.ConnectException: Error while attempting to read from change stream"}`
			}
			_, _ = w.Write([]byte(`{"` + kycConnector + `":{"status":{"connector":{"state":"RUNNING"},"tasks":[` + task + `]}}}`))
		case r.URL.Path == "/connectors/"+kycConnector+"/config":
			_ = json.NewEncoder(w).Encode(mongoHeartbeatConfig())
		case r.URL.Path == "/connectors/"+kycConnector+"/offsets":
			_ = json.NewEncoder(w).Encode(mongoOffsets(h.token))
		case r.URL.Path == "/connectors/"+kycConnector+"/restart" && r.Method == http.MethodPost:
			h.restarts++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	h.s = &CDCSentinel{
		connectURL:   srv.URL,
		httpClient:   srv.Client(),
		pollInterval: freshnessTestPoll,
		restartState: map[string]*connRestartState{},
	}
	return h
}

func (h *captureResolveHarness) set(connect, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connect, h.token = connect, token
}

var resolvedCodeIssue = map[string]string{
	"CDC_CAPTURE_STOPPED_RESOLVED":       captureStoppedIssueID(captureTestPID),
	"CDC_CONNECTOR_DOWN_RESOLVED":        connectorIssueID(captureTestPID),
	"CDC_SOURCE_STREAM_STALLED_RESOLVED": sourceStalledIssueID(captureTestPID),
}

// tick runs one poll and checks which alerts it announced as resolved.
//
// Every alert still open answers a close with the row the owner was paged for, so a
// close that comes too early shows up as a notice. The order of the poll's lookups is
// pinned by the capture-down and connector-down tests, so here they may come in any order.
func (h *captureResolveHarness) tick(want ...string) {
	h.t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		h.t.Fatalf("sqlmock: %v", err)
	}
	h.t.Cleanup(func() { db.Close() })
	mock.MatchExpectationsInOrder(false)
	h.s.db = db

	h.mu.Lock()
	connect := h.connect
	h.mu.Unlock()

	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(captureTestPID))
	deps := captureRows()
	if h.probeFails > 0 {
		deps.AddRow(captureTestPID, kycConnector, "unhealthy",
			"kafka connect unreachable: dial tcp 10.128.0.7:8083: connect: connection refused", h.probeFails)
	} else {
		deps.AddRow(captureTestPID, kycConnector, "healthy", "", 0)
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM pipeline_dependencies d")).WillReturnRows(deps)
	raises := h.probeFails >= CaptureDownFloor
	if raises {
		expectCaptureIssueInsert(mock, captureTestPID)
	}
	if connect != "" {
		mock.ExpectQuery(regexp.QuoteMeta("FROM cdc_resources")).
			WillReturnRows(sqlmock.NewRows([]string{"pipeline_id"}).AddRow(captureTestPID))
	}
	if connect == "FAILED" {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT cdc_mode FROM pipelines")).
			WillReturnRows(sqlmock.NewRows([]string{"cdc_mode"}).AddRow("never"))
	}
	for id, opened := range h.open {
		expectLagDelete(mock, id, lagRow(string(IssueSeverityCritical), opened))
	}

	before := len(*h.sent)
	h.s.checkActivePipelines(context.Background())

	var got []string
	for _, n := range (*h.sent)[before:] {
		got = append(got, n.kind.resolvedCode)
		if n.pipelineID != captureTestPID {
			h.t.Errorf("notice for pipeline %q, want %q", n.pipelineID, captureTestPID)
		}
		delete(h.open, resolvedCodeIssue[n.kind.resolvedCode])
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		h.t.Fatalf("poll announced %v as resolved, want %v", got, want)
	}
	// With every open alert closed as wanted, everything registered must have been asked.
	// An alert left open leaves its close unasked on purpose.
	if len(h.open) == 0 {
		if err := mock.ExpectationsWereMet(); err != nil {
			h.t.Fatalf("poll skipped a lookup: %v", err)
		}
	}
	if raises {
		h.open[captureStoppedIssueID(captureTestPID)] = time.Now()
	}
}

// The prod sequence of 2026-10-22: capture goes down, Connect comes back RUNNING, the
// probe reads healthy, and the resume token does not move because it has left the oplog.
// The owner must not be told capture is running again until the token moves.
func TestKYCCaptureStoppedIsNotAnnouncedResolvedWhileTheResumeTokenIsFrozen(t *testing.T) {
	h := newCaptureResolveHarness(t)

	h.tick() // streaming, healthy

	h.set("", "826D0A1F000000012B0429")
	h.probeFails = CaptureDownFloor
	h.tick() // 09:18: the probe has seen the capture side dead for a minute; the owner is paged
	if _, ok := h.open[captureStoppedIssueID(captureTestPID)]; !ok {
		t.Fatal("the outage raised no capture-stopped alert")
	}

	h.set("RUNNING", "826D0A1F000000012B0429")
	h.probeFails = 0
	h.tick() // 09:25: RUNNING and healthy, but the token is where it was before the outage
	h.tick()
	h.tick()

	h.set("RUNNING", "826D0A2A000000032B0429")
	// The move is read after this poll's capture verdict, so the notice waits a poll.
	h.tick()
	h.tick("CDC_CAPTURE_STOPPED_RESOLVED")
}

// The connector-down sibling: Connect says FAILED, the Sentinel restarts the connector,
// and the restarted connector is RUNNING while its token stays frozen.
func TestConnectorDownIsNotAnnouncedResolvedUntilTheStreamMoves(t *testing.T) {
	h := newCaptureResolveHarness(t)

	h.tick() // streaming, healthy

	h.set("FAILED", "826D0A1F000000012B0429")
	h.tick()
	if h.restarts != 1 {
		t.Fatalf("FAILED connector restarted %d times, want 1", h.restarts)
	}
	// The connector-down verdict the restart cap would raise next; raising it is pinned by
	// the restart-cap tests.
	h.open[connectorIssueID(captureTestPID)] = time.Now().Add(-20 * time.Minute)

	h.set("RUNNING", "826D0A1F000000012B0429")
	h.tick() // RUNNING after the restart, token frozen
	h.tick()

	h.set("RUNNING", "826D0A2A000000032B0429")
	h.tick("CDC_CONNECTOR_DOWN_RESOLVED")
}

// The stall sibling, after an orchestrator restart: the in-memory clock is gone, so the
// first reading only starts it. It used to clear the open stall too, telling the owner
// a stream that had not moved was "moving again".
func TestAnOpenStallIsNotAnnouncedResolvedByTheFirstReadingAfterARestart(t *testing.T) {
	sent := captureResolved(t)
	h := newHeartbeatHarness(t)
	h.config = mongoHeartbeatConfig()
	h.offsets = mongoOffsets("826D0A1F000000012B0429")
	expectLagDelete(h.mock, sourceStalledIssueID(hbTestPipeline),
		lagRow(string(IssueSeverityCritical), time.Now().Add(-25*time.Minute)))

	h.tick() // a fresh orchestrator's first reading of a frozen token
	if len(*sent) != 0 {
		t.Fatalf("the first reading after a restart announced %s", (*sent)[0].kind.resolvedCode)
	}

	h.offsets = mongoOffsets("826D0A2A000000032B0429")
	h.tick()
	if len(*sent) != 1 || (*sent)[0].kind.resolvedCode != "CDC_SOURCE_STREAM_STALLED_RESOLVED" {
		t.Fatalf("a token that moved announced %v, want one CDC_SOURCE_STREAM_STALLED_RESOLVED", *sent)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
