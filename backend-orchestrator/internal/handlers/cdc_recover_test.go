package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// The recovery endpoint drives four Kafka Connect calls after its checks: park the
// connector, optionally clear its offsets, write the new config, start it again.
// It used to discard the answer to every one except the offset reset and the
// config write, and to answer 200 "CDC recovery triggered" regardless — so a
// refused resume left a connector parked while the operator read "recovered". And
// it parked with /pause, which Kafka Connect's offsets API (KIP-875) refuses: an
// offset reset needs the connector STOPPED.
//
// These tests answer each Connect call from a fake and assert both what the
// operator is told and what was asked of Connect, in order.

const (
	recoverTestPipelineID = "7c1e0a55-1111-4222-8333-944455556666"
	recoverTestConnector  = "cdc-7c1e0a55"
)

// fakeRecoverConnect is a Kafka Connect holding one FAILED MongoDB connector (so
// the PostgreSQL re-provisioning branch is not taken). It answers each
// "METHOD path" with the status in answers — a 404 for anything else — and
// records the calls in order.
func fakeRecoverConnect(t *testing.T, answers map[string]int) *[]string {
	t.Helper()
	var mu sync.Mutex
	calls := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		*calls = append(*calls, key)
		mu.Unlock()
		base := "/connectors/" + recoverTestConnector
		switch key {
		case "GET " + base + "/status":
			_, _ = w.Write([]byte(`{"name":"` + recoverTestConnector + `","connector":{"state":"RUNNING"},"tasks":[{"id":0,"state":"FAILED"}]}`))
			return
		case "GET " + base + "/config":
			_, _ = w.Write([]byte(`{"connector.class":"io.debezium.connector.mongodb.MongoDbConnector","collection.include.list":"shop.orders","snapshot.mode":"initial"}`))
			return
		}
		code, ok := answers[key]
		if !ok {
			code = http.StatusNotFound
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KAFKA_CONNECT_URL", srv.URL)
	return calls
}

func runRecover(t *testing.T, answers map[string]int, body string) (int, map[string]interface{}, []string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("CDC_RECOVERY_ENABLED", "true")
	calls := fakeRecoverConnect(t, answers)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery("FROM cdc_resources").WithArgs(recoverTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"resource_name"}).AddRow(recoverTestConnector))
	mock.ExpectQuery("SELECT source_connection_id").WithArgs(recoverTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"source_connection_id"}).AddRow("0f0e0d0c-1111-4222-8333-944455556666"))

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("auth_internal", true) })
	r.POST("/cdc/pipelines/:pipeline_id/recover", RecoverCDCPipeline(db))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/cdc/pipelines/"+recoverTestPipelineID+"/recover",
		bytes.NewReader([]byte(body))))
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db: %v (body %v)", err, out)
	}
	return w.Code, out, *calls
}

const (
	recoverWithReset    = `{"operator":"test","reason":"position lost","snapshot_mode":"initial","reset_offsets":true}`
	recoverWithoutReset = `{"operator":"test","reason":"task failed","snapshot_mode":"recovery"}`
)

// connectWrites drops the two reads every run makes, leaving the calls that change
// the connector.
func connectWrites(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func pathOf(verb, suffix string) string {
	return verb + " /connectors/" + recoverTestConnector + suffix
}

func TestRecoverStopsTheConnectorBeforeClearingItsOffsets(t *testing.T) {
	code, body, calls := runRecover(t, map[string]int{
		pathOf("PUT", "/stop"):       http.StatusNoContent,
		pathOf("DELETE", "/offsets"): http.StatusOK,
		pathOf("PUT", "/config"):     http.StatusOK,
		pathOf("PUT", "/resume"):     http.StatusAccepted,
	}, recoverWithReset)

	want := []string{pathOf("PUT", "/stop"), pathOf("DELETE", "/offsets"), pathOf("PUT", "/config"), pathOf("PUT", "/resume")}
	if got := connectWrites(calls); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("connect calls = %v\nwant %v\n\nKafka Connect resets offsets only for a STOPPED "+
			"connector (KIP-875); a paused one answers 400, so recovery with reset_offsets "+
			"could never clear a lost position", got, want)
	}
	if code != http.StatusOK || body["success"] != true {
		t.Fatalf("code=%d body=%v; want 200 success", code, body)
	}
}

func TestRecoverWithoutResetPausesRestartsAndResumes(t *testing.T) {
	code, body, calls := runRecover(t, map[string]int{
		pathOf("PUT", "/pause"):    http.StatusAccepted,
		pathOf("PUT", "/config"):   http.StatusOK,
		pathOf("POST", "/restart"): http.StatusAccepted,
		pathOf("PUT", "/resume"):   http.StatusAccepted,
	}, recoverWithoutReset)

	want := []string{pathOf("PUT", "/pause"), pathOf("PUT", "/config"), pathOf("POST", "/restart"), pathOf("PUT", "/resume")}
	if got := connectWrites(calls); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("connect calls = %v\nwant %v", got, want)
	}
	if code != http.StatusOK || body["success"] != true {
		t.Fatalf("code=%d body=%v; want 200 success", code, body)
	}
}

// A refused resume leaves the connector parked. That is not a recovery.
func TestRecoverReportsARefusedResume(t *testing.T) {
	code, body, _ := runRecover(t, map[string]int{
		pathOf("PUT", "/pause"):    http.StatusAccepted,
		pathOf("PUT", "/config"):   http.StatusOK,
		pathOf("POST", "/restart"): http.StatusAccepted,
		pathOf("PUT", "/resume"):   http.StatusInternalServerError,
	}, recoverWithoutReset)

	if code < 300 || body["success"] != false {
		t.Fatalf("code=%d body=%v\n\nKafka Connect refused the resume, so the connector is still "+
			"paused, and the operator was told the recovery succeeded", code, body)
	}
}

func TestRecoverReportsARefusedRestart(t *testing.T) {
	code, body, _ := runRecover(t, map[string]int{
		pathOf("PUT", "/pause"):    http.StatusAccepted,
		pathOf("PUT", "/config"):   http.StatusOK,
		pathOf("POST", "/restart"): http.StatusConflict,
		pathOf("PUT", "/resume"):   http.StatusAccepted,
	}, recoverWithoutReset)

	if code < 300 || body["success"] != false {
		t.Fatalf("code=%d body=%v; the failed task was never restarted, so it is still FAILED", code, body)
	}
}

// If the connector cannot be parked, nothing after it may run: writing a new
// snapshot.mode into a connector that is still running changes it under the
// operator's feet.
func TestRecoverStopsAtARefusedPark(t *testing.T) {
	code, body, calls := runRecover(t, map[string]int{
		pathOf("PUT", "/pause"): http.StatusInternalServerError,
	}, recoverWithoutReset)

	if code < 300 || body["success"] != false {
		t.Fatalf("code=%d body=%v; want a failure when the pause is refused", code, body)
	}
	if got := connectWrites(calls); len(got) != 1 {
		t.Fatalf("connect calls after a refused pause = %v; want none", got)
	}
}

// The offsets are intact when their reset fails, so starting the connector again
// puts it back exactly as the operator found it. Leaving it stopped strands a
// pipeline that nothing will restart.
func TestRecoverStartsTheConnectorAgainWhenTheOffsetResetFails(t *testing.T) {
	code, body, calls := runRecover(t, map[string]int{
		pathOf("PUT", "/stop"):       http.StatusNoContent,
		pathOf("DELETE", "/offsets"): http.StatusBadRequest,
		pathOf("PUT", "/resume"):     http.StatusAccepted,
	}, recoverWithReset)

	if code < 300 || body["success"] != false {
		t.Fatalf("code=%d body=%v; want a failure", code, body)
	}
	want := []string{pathOf("PUT", "/stop"), pathOf("DELETE", "/offsets"), pathOf("PUT", "/resume")}
	if got := connectWrites(calls); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("connect calls = %v\nwant %v — the connector was left stopped", got, want)
	}
	if body["connector_state"] != "resumed" {
		t.Errorf("connector_state = %v; want resumed", body["connector_state"])
	}
}

// Once the offsets are gone the old config must not run: started with no stored
// position, it would take its own snapshot.mode — a snapshot nobody chose. The
// connector stays stopped and the answer says so.
func TestRecoverLeavesTheConnectorStoppedWhenTheConfigWriteFailsAfterTheReset(t *testing.T) {
	code, body, calls := runRecover(t, map[string]int{
		pathOf("PUT", "/stop"):       http.StatusNoContent,
		pathOf("DELETE", "/offsets"): http.StatusOK,
		pathOf("PUT", "/config"):     http.StatusInternalServerError,
		pathOf("PUT", "/resume"):     http.StatusAccepted,
	}, recoverWithReset)

	if code < 300 || body["success"] != false {
		t.Fatalf("code=%d body=%v; want a failure", code, body)
	}
	for _, c := range calls {
		if c == pathOf("PUT", "/resume") {
			t.Fatalf("connect calls = %v; resumed a connector whose offsets were cleared, under its old config", calls)
		}
	}
	if body["connector_state"] != "stopped" {
		t.Errorf("connector_state = %v; want stopped", body["connector_state"])
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "offsets") {
		t.Errorf("message %q does not tell the operator the offsets are already cleared", msg)
	}
}
