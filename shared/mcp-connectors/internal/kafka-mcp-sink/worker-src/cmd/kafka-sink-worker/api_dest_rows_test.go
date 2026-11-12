package main

// An API destination written row by row through its key map (api_dest_rows.go).
//
// Bug class (generator trust plan P5, the four #1440 known issues): the sink
// treated a REST API like a table it could upsert into by key. The API names a
// record by an id it makes up, so a re-run created every record again, a Reload
// appended, a batch the API half wrote was retried whole (duplicates) or DLQ'd
// whole (rows that landed counted as lost), and a batch that outlasted the sink's
// HTTP timeout made records whose ids the sink never learned. These tests drive
// the writer with a scripted destination and an in-memory key map; the bench
// (llm-service harness/sink_loop.py + write_bench.py) runs the same contract
// against the generated connector.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// memAPIRecords is the key map in memory.
type memAPIRecords struct {
	ids   map[string]string // source key → record id
	stamp map[string]string // source key → last execution that read it
}

func newMemAPIRecords() *memAPIRecords {
	return &memAPIRecords{ids: map[string]string{}, stamp: map[string]string{}}
}

func (m *memAPIRecords) lookup(_ context.Context, _ apiRecordScope, keys []string, exec string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if id, ok := m.ids[k]; ok {
			out[k] = id
			m.stamp[k] = exec
		}
	}
	return out, nil
}

func (m *memAPIRecords) save(_ context.Context, _ apiRecordScope, ids map[string]string, exec string) error {
	for k, id := range ids {
		m.ids[k], m.stamp[k] = id, exec
	}
	return nil
}

func (m *memAPIRecords) forget(_ context.Context, _ apiRecordScope, keys []string) error {
	for _, k := range keys {
		delete(m.ids, k)
		delete(m.stamp, k)
	}
	return nil
}

// apiCall is one call the scripted destination got.
type apiCall struct {
	op        string
	data      []map[string]interface{}
	recordIDs []string
	budget    float64
}

// scriptedAPI answers each call through answer(call number from 0, the call).
type scriptedAPI struct {
	calls  []apiCall
	answer func(n int, c apiCall) (map[string]interface{}, error)
}

func (s *scriptedAPI) call(_ context.Context, op string, args map[string]interface{}) (map[string]interface{}, error) {
	c := apiCall{op: op}
	c.data, _ = args["data"].([]map[string]interface{})
	c.recordIDs, _ = args["record_ids"].([]string)
	c.budget, _ = args["time_budget_s"].(float64)
	s.calls = append(s.calls, c)
	return s.answer(len(s.calls)-1, c)
}

// perRow answers one result per row from status(row, its record id).
func perRow(c apiCall, status func(row map[string]interface{}, recordID string) map[string]interface{}) map[string]interface{} {
	results := make([]interface{}, len(c.recordIDs))
	ok := true
	for i, id := range c.recordIDs {
		var row map[string]interface{}
		if i < len(c.data) {
			row = c.data[i]
		}
		r := status(row, id)
		if r["status"] == "failed" {
			ok = false
		}
		results[i] = r
	}
	return map[string]interface{}{"success": ok, "results": results}
}

// upsertOK creates a record "rec-<id>" for a row with no record id, else updates it.
func upsertOK(row map[string]interface{}, recordID string) map[string]interface{} {
	if recordID == "" {
		return map[string]interface{}{"status": "created", "id": fmt.Sprintf("rec-%v", row["id"])}
	}
	return map[string]interface{}{"status": "updated", "id": recordID}
}

type sleepLog struct{ waits []time.Duration }

func (s *sleepLog) sleep(ctx context.Context, d time.Duration) bool {
	s.waits = append(s.waits, d)
	return ctx.Err() == nil
}

func newTestWriter(dest *scriptedAPI, store apiRecordStore, sl *sleepLog) *apiRowWriter {
	return &apiRowWriter{call: dest.call, store: store, sleep: sl.sleep, budget: 30 * time.Second}
}

var testScope = apiRecordScope{pipelineID: "p1", sourceTable: "public.users", destTable: "tasks"}

func rowsOf(ids ...int) []map[string]interface{} {
	out := make([]map[string]interface{}, len(ids))
	for i, id := range ids {
		out[i] = map[string]interface{}{"id": int64(id), "name": fmt.Sprintf("n%d", id)}
	}
	return out
}

func TestAPIRowWriter_ARerunUpdatesTheRecordsTheFirstRunMade(t *testing.T) {
	store := newMemAPIRecords()
	dest := &scriptedAPI{answer: func(_ int, c apiCall) (map[string]interface{}, error) { return perRow(c, upsertOK), nil }}
	w := newTestWriter(dest, store, &sleepLog{})
	ctx := context.Background()

	first, err := w.writeRows(ctx, testScope, "exec-1", rowsOf(1, 2, 3), []string{"id"}, map[string]interface{}{"table": "tasks"})
	if err != nil || first.written != 3 || len(first.failedRows) != 0 {
		t.Fatalf("first run: written=%d failed=%d err=%v, want 3/0/nil", first.written, len(first.failedRows), err)
	}
	if got := dest.calls[0].recordIDs; strings.Join(got, ",") != ",," {
		t.Fatalf("first run sent record ids %q, want none (create)", got)
	}
	if len(store.ids) != 3 {
		t.Fatalf("key map holds %d records after the first run, want 3", len(store.ids))
	}

	second, err := w.writeRows(ctx, testScope, "exec-2", rowsOf(1, 2, 3), []string{"id"}, map[string]interface{}{"table": "tasks"})
	if err != nil || second.written != 3 {
		t.Fatalf("re-run: written=%d err=%v, want 3/nil", second.written, err)
	}
	if got := strings.Join(dest.calls[1].recordIDs, ","); got != "rec-1,rec-2,rec-3" {
		t.Fatalf("re-run sent record ids %q, want rec-1,rec-2,rec-3 — a re-run must update, not create", got)
	}
	if dest.calls[1].budget != 30 {
		t.Fatalf("time_budget_s = %v, want 30", dest.calls[1].budget)
	}
	for k, e := range store.stamp {
		if e != "exec-2" {
			t.Fatalf("key %s stamped %q after the re-run, want exec-2", k, e)
		}
	}
}

func TestAPIRowWriter_RowsTheBudgetDidNotReachAreSentAgainAtOnce(t *testing.T) {
	dest := &scriptedAPI{answer: func(n int, c apiCall) (map[string]interface{}, error) {
		return perRow(c, func(row map[string]interface{}, id string) map[string]interface{} {
			if n == 0 && row["id"] != int64(1) {
				return map[string]interface{}{"status": "pending"}
			}
			return upsertOK(row, id)
		}), nil
	}}
	sl := &sleepLog{}
	res, err := newTestWriter(dest, newMemAPIRecords(), sl).writeRows(context.Background(), testScope, "e", rowsOf(1, 2, 3), []string{"id"}, nil)
	if err != nil || res.written != 3 || len(res.failedRows) != 0 {
		t.Fatalf("written=%d failed=%d err=%v, want 3/0/nil", res.written, len(res.failedRows), err)
	}
	if len(dest.calls) != 2 || len(dest.calls[1].data) != 2 {
		t.Fatalf("calls=%d (second carried %d rows), want 2 calls, the second with the 2 pending rows", len(dest.calls), len(dest.calls[1].data))
	}
	if len(sl.waits) != 0 {
		t.Fatalf("waited %v before sending pending rows again, want no wait", sl.waits)
	}
}

func TestAPIRowWriter_OnlyTheRowsTheDestinationRefusedFail(t *testing.T) {
	dest := &scriptedAPI{answer: func(_ int, c apiCall) (map[string]interface{}, error) {
		return perRow(c, func(row map[string]interface{}, id string) map[string]interface{} {
			if row["id"] == int64(2) {
				return map[string]interface{}{"status": "failed", "error": "HTTP 400: name is required", "retryable": false}
			}
			return upsertOK(row, id)
		}), nil
	}}
	store := newMemAPIRecords()
	res, err := newTestWriter(dest, store, &sleepLog{}).writeRows(context.Background(), testScope, "e", rowsOf(1, 2, 3), []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.written != 2 || len(res.failedRows) != 1 || res.failedRows[0]["id"] != int64(2) {
		t.Fatalf("written=%d failedRows=%v, want 2 written and row 2 failed", res.written, res.failedRows)
	}
	if res.errText != "1 of 3 rows failed: HTTP 400: name is required" {
		t.Fatalf("errText = %q", res.errText)
	}
	if len(dest.calls) != 1 {
		t.Fatalf("a refusal that is not retryable was sent %d times, want 1", len(dest.calls))
	}
	if len(store.ids) != 2 {
		t.Fatalf("key map holds %d records, want the 2 that landed", len(store.ids))
	}
}

func TestAPIRowWriter_ARetryableFailureBacksOffAndEndsAtTheRoundCap(t *testing.T) {
	dest := &scriptedAPI{answer: func(_ int, c apiCall) (map[string]interface{}, error) {
		return perRow(c, func(row map[string]interface{}, id string) map[string]interface{} {
			if row["id"] == int64(2) {
				return map[string]interface{}{"status": "failed", "error": "HTTP 429", "retryable": true}
			}
			return upsertOK(row, id)
		}), nil
	}}
	sl := &sleepLog{}
	res, err := newTestWriter(dest, newMemAPIRecords(), sl).writeRows(context.Background(), testScope, "e", rowsOf(1, 2, 3), []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dest.calls) != apiMaxRounds {
		t.Fatalf("calls = %d, want %d", len(dest.calls), apiMaxRounds)
	}
	for i, c := range dest.calls[1:] {
		if len(c.data) != 1 {
			t.Fatalf("retry %d resent %d rows, want only the failed one", i+1, len(c.data))
		}
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second}
	if fmt.Sprint(sl.waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", sl.waits, want)
	}
	if res.written != 2 || len(res.failedRows) != 1 || !strings.Contains(res.errText, "HTTP 429") {
		t.Fatalf("written=%d failed=%d errText=%q", res.written, len(res.failedRows), res.errText)
	}
}

func TestAPIRowWriter_AnAnswerWithoutOneResultARowRetriesEveryRow(t *testing.T) {
	dest := &scriptedAPI{answer: func(n int, c apiCall) (map[string]interface{}, error) {
		switch n {
		case 0:
			return map[string]interface{}{"success": false}, errors.New("dest error: HTTP 502")
		case 1:
			return map[string]interface{}{"success": true, "results": []interface{}{}}, nil
		}
		return perRow(c, upsertOK), nil
	}}
	sl := &sleepLog{}
	res, err := newTestWriter(dest, newMemAPIRecords(), sl).writeRows(context.Background(), testScope, "e", rowsOf(1, 2), []string{"id"}, nil)
	if err != nil || res.written != 2 {
		t.Fatalf("written=%d err=%v, want 2/nil", res.written, err)
	}
	if len(dest.calls) != 3 || len(dest.calls[2].data) != 2 || len(sl.waits) != 2 {
		t.Fatalf("calls=%d waits=%v, want 3 calls carrying both rows and 2 waits", len(dest.calls), sl.waits)
	}
}

func TestAPIRowWriter_AKeyTwiceInOneMessageIsOneRecordItsLastRow(t *testing.T) {
	dest := &scriptedAPI{answer: func(_ int, c apiCall) (map[string]interface{}, error) {
		return perRow(c, func(map[string]interface{}, string) map[string]interface{} {
			return map[string]interface{}{"status": "failed", "error": "HTTP 400", "retryable": false}
		}), nil
	}}
	rows := []map[string]interface{}{{"id": int64(1), "name": "old"}, {"id": int64(1), "name": "new"}}
	res, err := newTestWriter(dest, newMemAPIRecords(), &sleepLog{}).writeRows(context.Background(), testScope, "e", rows, []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dest.calls[0].data) != 1 || dest.calls[0].data[0]["name"] != "new" {
		t.Fatalf("sent %v, want the key's last row only", dest.calls[0].data)
	}
	if res.written != 0 || len(res.failedRows) != 2 || res.errText != "2 of 2 rows failed: HTTP 400" {
		t.Fatalf("written=%d failed=%d errText=%q — both message rows of the failed key go to the DLQ", res.written, len(res.failedRows), res.errText)
	}
}

func TestAPIRowWriter_ARecordWithNoIDIsNotMapped(t *testing.T) {
	dest := &scriptedAPI{answer: func(_ int, c apiCall) (map[string]interface{}, error) {
		return perRow(c, func(map[string]interface{}, string) map[string]interface{} {
			return map[string]interface{}{"status": "created"}
		}), nil
	}}
	store := newMemAPIRecords()
	res, err := newTestWriter(dest, store, &sleepLog{}).writeRows(context.Background(), testScope, "e", rowsOf(1), []string{"id"}, nil)
	if err != nil || res.written != 1 {
		t.Fatalf("written=%d err=%v", res.written, err)
	}
	if len(store.ids) != 0 || len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "returned no id") {
		t.Fatalf("key map=%v warnings=%v, want nothing mapped and one warning", store.ids, res.warnings)
	}
}

func TestAPIRowWriter_StopsWhenTheWorkerShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dest := &scriptedAPI{answer: func(_ int, c apiCall) (map[string]interface{}, error) {
		cancel()
		return perRow(c, func(map[string]interface{}, string) map[string]interface{} {
			return map[string]interface{}{"status": "failed", "error": "HTTP 503", "retryable": true}
		}), nil
	}}
	_, err := newTestWriter(dest, newMemAPIRecords(), &sleepLog{}).writeRows(ctx, testScope, "e", rowsOf(1), []string{"id"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (offset left uncommitted)", err)
	}
}

func TestAPISourceKey_SameRowSameKeyEveryRun(t *testing.T) {
	decode := func(s string) map[string]interface{} {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return normalizeJSONNumbers(m).(map[string]interface{})
	}
	a := decode(`{"id": 9007199254740993, "name": "x", "amount": 1.5}`)
	b := decode(`{"amount": 1.5, "name": "x", "id": 9007199254740993}`)
	if apiSourceKey(a, []string{"id"}) != apiSourceKey(b, []string{"id"}) {
		t.Fatal("the same key decoded twice gave two source keys")
	}
	if apiSourceKey(a, nil) != apiSourceKey(b, nil) {
		t.Fatal("the same keyless row decoded twice gave two source keys")
	}
	c := decode(`{"id": 9007199254740992, "name": "x", "amount": 1.5}`)
	if apiSourceKey(a, []string{"id"}) == apiSourceKey(c, []string{"id"}) {
		t.Fatal("two ids one apart share a source key (number precision lost)")
	}
	if !strings.HasPrefix(apiSourceKey(a, []string{"id"}), "k:") || !strings.HasPrefix(apiSourceKey(a, nil), "r:") {
		t.Fatal("source key kinds: want k: with key fields, r: without")
	}
	if strings.Contains(apiSourceKey(a, []string{"id"}), "9007199254740993") {
		t.Fatal("source key carries a row value; it must be a hash")
	}
}

func TestDLQMessageForRows_HoldsOnlyTheFailedRowsInline(t *testing.T) {
	msg := kafka.Message{
		Topic: "rsync.p1.batch", Partition: 2, Offset: 41,
		Headers: []kafka.Header{{Key: "run_mode", Value: []byte("reload")}},
		Value:   []byte(`{"table":"tasks","storage_type":"minio","claim_check_url":"http://minio/x","row_count":3,"batch_offset":9007199254740993,"key_fields":["id"]}`),
	}
	out := dlqMessageForRows(msg, []map[string]interface{}{{"id": int64(2)}})
	var p map[string]interface{}
	dec := json.NewDecoder(strings.NewReader(string(out.Value)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p["storage_type"] != "inline" || p["claim_check_url"] != nil || p["row_count"].(json.Number).String() != "1" {
		t.Fatalf("payload = %v, want inline, no claim check, row_count 1", p)
	}
	if rows, _ := p["data"].([]interface{}); len(rows) != 1 {
		t.Fatalf("data = %v, want the one failed row", p["data"])
	}
	if p["batch_offset"].(json.Number).String() != "9007199254740993" {
		t.Fatalf("batch_offset = %v, want it exact", p["batch_offset"])
	}
	if out.Topic != msg.Topic || out.Partition != 2 || out.Offset != 41 || len(out.Headers) != 1 {
		t.Fatal("the DLQ copy lost the message's topic, partition, offset or headers")
	}
}

// capsAnswer answers every destination call with body.
type capsAnswer struct{ body string }

func (c capsAnswer) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

func TestProbeDestCapabilities_ReadsRecordIDs(t *testing.T) {
	cfg := &WorkerConfig{DestinationConnector: "t-clickup", DestinationVersion: "1.0.0"}
	for body, want := range map[string]bool{
		`{"success":true,"record_ids":true}`:                  true,
		`{"success":true,"capabilities":{"record_ids":true}}`: true,
		`{"success":true,"supports_ddl":false}`:               false,
	} {
		caps, ok := probeDestCapabilities(context.Background(), &http.Client{Transport: capsAnswer{body}}, cfg, "t-clickup")
		if !ok || caps.recordIDs != want || caps.ddl {
			t.Errorf("%s: caps=%+v ok=%v, want recordIDs=%v ddl=false", body, caps, ok, want)
		}
	}
	ddl := &DDLSupport{RecordIDs: true, resolved: true}
	if !apiRecordWrites(context.Background(), nil, cfg, ddl, "t-clickup") {
		t.Fatal("a destination that says record_ids must be written through the key map")
	}
	if apiRecordWrites(context.Background(), nil, &WorkerConfig{DestinationConfig: map[string]interface{}{"cdc_write_mode": "append"}}, ddl, "t-clickup") {
		t.Fatal("append mode writes every row as a new record, as asked")
	}
	if apiRecordWrites(context.Background(), nil, cfg, ddl, "aws-s3") {
		t.Fatal("object storage is never an API destination")
	}
}

func TestCallDestinationToolAnswer_KeepsTheAnswerOfAHalfWrittenBatch(t *testing.T) {
	cfg := &WorkerConfig{DestinationConnector: "t-clickup", DestinationVersion: "1.0.0"}
	body := `{"success":false,"error":"1 of 2 rows failed","results":[{"status":"created","id":"a"},{"status":"failed","error":"HTTP 400"}]}`
	client := &http.Client{Transport: capsAnswer{body}}
	res, err := callDestinationToolAnswer(context.Background(), client, cfg, "t-clickup", "upsert_data", map[string]interface{}{})
	if err == nil {
		t.Fatal("a batch the destination half wrote returned no error")
	}
	if r, _ := res["results"].([]interface{}); len(r) != 2 {
		t.Fatalf("results = %v, want both rows' answers", res["results"])
	}
	if res, _ := callDestinationTool(context.Background(), client, cfg, "t-clickup", "upsert_data", map[string]interface{}{}); res != nil {
		t.Fatal("callDestinationTool must keep returning no answer with an error")
	}
}

func TestPartialBatchAck_CountsTheRowsThatLandedAndSaysWhy(t *testing.T) {
	conn := &captureConn{}
	db := newCaptureDB(t, conn)
	defer db.Close()
	sm := &SinkMessage{PipelineID: "p1", ExecutionID: "e1", Table: "tasks", StorageType: "inline", BatchOffset: 3}
	if err := persistPartialBatchAckToPostgres(context.Background(), db, sm, 2, 3, "1 of 3 rows failed: HTTP 400", "t", 0, 9); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.query, "pipeline_batch_acks") || !strings.Contains(conn.query, "DO NOTHING") {
		t.Fatalf("query = %s", conn.query)
	}
	if len(conn.args) != 12 || conn.args[4].Value != int64(2) || conn.args[5].Value != int64(3) || conn.args[10].Value != "1 of 3 rows failed: HTTP 400" {
		t.Fatalf("args = %v, want rows_written 2, rows_read 3 and the reason", conn.args)
	}
}

// Writing to an app is reverse ETL: the sink updates the records it wrote and
// deletes none — no Reload sweep, no delete of a record the source no longer has.
// Deleting is a reverse-ETL mode the user turns on (BACKLOG "Reverse ETL into an
// API destination"); adding it means changing this test on purpose.
func TestTheSinkDeletesNoRecordInAnAPI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		// The one exception is the batched relational delete, declared once and sent
		// only to batchedDeleteDestinations (checked below).
		src := strings.Replace(string(b), `relationalDeleteTool = "delete_data"`, "", 1)
		if strings.Contains(src, `"delete_data"`) {
			t.Errorf("%s calls delete_data: the sink must not delete records in an API destination", f)
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned %d source files, want the whole package", scanned)
	}

	for dest := range batchedDeleteDestinations {
		if dest != "postgresql" && dest != "postgres" && dest != "mysql" {
			t.Errorf("batchedDeleteDestinations admits %q: batched deletes go to relational databases only", dest)
		}
	}
	b := &cdcDBBatcher{cfg: &WorkerConfig{}, destType: "clickup", destCfg: map[string]interface{}{}}
	adm := b.admit(kafka.Message{}, &SinkMessage{CDCOp: "d", PK: map[string]interface{}{"id": 1}, KeyFields: []string{"id"},
		Before: map[string]interface{}{"id": 1}})
	if !adm.decline {
		t.Fatalf("a delete bound for an API destination was batched (%+v); it must take the single-event path", adm)
	}
}
