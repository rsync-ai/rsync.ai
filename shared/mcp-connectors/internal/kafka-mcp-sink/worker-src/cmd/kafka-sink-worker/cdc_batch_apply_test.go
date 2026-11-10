package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// Tests for cdc_batch_apply.go: what one flush sends to a relational database and to a
// warehouse, which changes a batch takes, and the destination's batch_* settings.

const batchApplyTopic = "cdc.shop.orders"

// batchDest is a fake destination that records every tool call, in order, and
// answers each write with the count of rows it was sent.
type batchDest struct {
	mu    sync.Mutex
	calls []batchDestCall
}

type batchDestCall struct {
	name string
	args map[string]interface{}
}

func (r *batchDest) RoundTrip(req *http.Request) (*http.Response, error) {
	var body struct {
		Params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		} `json:"params"`
	}
	raw, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(raw, &body)
	r.mu.Lock()
	r.calls = append(r.calls, batchDestCall{name: body.Params.Name, args: body.Params.Arguments})
	r.mu.Unlock()
	data, _ := body.Params.Arguments["data"].([]interface{})
	field := "rows_upserted"
	if strings.HasSuffix(body.Params.Name, "_"+relationalDeleteTool) {
		field = "rows_deleted"
	}
	result := fmt.Sprintf(`{"success":true,%q:%d}`, field, len(data))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`)),
		Request:    req,
	}, nil
}

// writes is the recorded data calls, leaving out schema work (ensure_table and the like).
func (r *batchDest) writes() []batchDestCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []batchDestCall
	for _, c := range r.calls {
		for _, tool := range []string{"_upsert_data", "_" + relationalDeleteTool, "_merge", "_import_data", "_load"} {
			if strings.HasSuffix(c.name, tool) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func (c batchDestCall) dataJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(c.args["data"])
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return string(raw)
}

// stamp is the kafka_offset.offset the call stamped, and whether it stamped one.
func (c batchDestCall) stamp() (int64, bool) {
	ko, ok := c.args["kafka_offset"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	f, ok := ko["offset"].(float64)
	return int64(f), ok
}

func batchApplyBatcher(t *testing.T, destType string, destCfg map[string]interface{}, rt http.RoundTripper) (*cdcDBBatcher, *highWaterTracker) {
	t.Helper()
	b, hw := dbBatcherFor(t, rt)
	if destCfg == nil {
		destCfg = map[string]interface{}{}
	}
	b.destType = destType
	b.destCfg = destCfg
	b.cfg.DestinationConfig = destCfg
	b.warehouse = isDataWarehouseConnector(destType)
	return b, hw
}

// orderChange is one change to shop.orders keyed on id. A delete carries its Before
// image; an upsert its After image.
func orderChange(offset int64, op string, image map[string]interface{}) (kafka.Message, *SinkMessage) {
	sm := &SinkMessage{PipelineID: ackFKPipelineID, Table: "shop.orders", CDCOp: op, SourceTS: 1_700_000_000_000,
		KeyFields: []string{"id"}}
	if image != nil {
		sm.PK = map[string]interface{}{"id": image["id"]}
	}
	if op == "d" {
		sm.Before = image
	} else {
		sm.After = image
	}
	return kafka.Message{Topic: batchApplyTopic, Partition: 0, Offset: offset}, sm
}

func flushOpenBatch(t *testing.T, b *cdcDBBatcher, reason string) {
	t.Helper()
	for key, batch := range b.batches {
		if batch.table == "shop.orders" {
			b.flushBatch(t.Context(), key, batch, reason)
			return
		}
	}
	t.Fatal("no open batch for shop.orders")
}

func row(kv ...interface{}) map[string]interface{} {
	m := map[string]interface{}{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// One relational flush is one delete_data and one upsert_data, whatever the batch
// holds: repeat upserts of a key merge column by column, a delete ends the key's row,
// an upsert after it starts a new one, and the deletes go first.
func TestBatchApply_RelationalNetsEachKeyAndDeletesInBulk(t *testing.T) {
	dest := &batchDest{}
	b, hw := batchApplyBatcher(t, "postgresql", nil, dest)
	ctx := t.Context()
	for _, c := range []struct {
		op  string
		row map[string]interface{}
	}{
		{"c", row("id", 1, "name", "a", "qty", 1)},
		{"u", row("id", 1, "qty", 2)}, // a partial image keeps name from the earlier one
		{"c", row("id", 2, "name", "b")},
		{"d", row("id", 2, "name", "b")},
		{"c", row("id", 3, "name", "c")},
		{"d", row("id", 3, "name", "c")},
		{"c", row("id", 3, "name", "again")},
		{"d", row("id", 4, "name", "gone")},
	} {
		msg, sm := orderChange(int64(len(b.batchFor(t))), c.op, c.row)
		if !b.add(ctx, msg, sm) {
			t.Fatalf("offset %d (%s) was declined; a keyed change must batch", msg.Offset, c.op)
		}
	}

	var logs string
	logs = captureLogStderr(t, func() { flushOpenBatch(t, b, "test") })

	writes := dest.writes()
	if len(writes) != 2 {
		t.Fatalf("want one delete_data and one upsert_data, got %d calls: %v", len(writes), writes)
	}
	del, up := writes[0], writes[1]
	if del.name != "postgresql_delete_data" || up.name != "postgresql_upsert_data" {
		t.Fatalf("calls %s, %s: the deletes must go first, then the upserts", del.name, up.name)
	}
	if got, want := del.dataJSON(t), `[{"id":2},{"id":3},{"id":4}]`; got != want {
		t.Fatalf("delete_data keys %s, want %s: one entry per key, the key columns only", got, want)
	}
	if kf := fmt.Sprint(del.args["key_fields"]); kf != "[id]" {
		t.Fatalf("delete_data key_fields %s, want [id]", kf)
	}
	if got, want := up.dataJSON(t), `[{"id":1,"name":"a","qty":2},{"id":3,"name":"again"}]`; got != want {
		t.Fatalf("upsert_data rows %s, want %s", got, want)
	}

	// Only the final call stamps the batch's landing offset; nothing below the batch
	// had landed, so the delete stamps nothing a restart could read past.
	if off, ok := del.stamp(); ok {
		t.Fatalf("delete_data stamped offset %d before the batch landed", off)
	}
	if off, ok := up.stamp(); !ok || off != 7 {
		t.Fatalf("upsert_data stamped (%d, %v), want 7", off, ok)
	}
	if !hw.seen(batchApplyTopic, 0, 7) {
		t.Fatal("the landed batch must count as written through its last offset")
	}
	if got := loadCounter(b.cdcDeletes, "shop.orders"); got != 3 {
		t.Fatalf("deletes counted %d, want 3 (one per delete event)", got)
	}

	var flushed map[string]any
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"cdc batch flushed"`) {
			_ = json.Unmarshal([]byte(line), &flushed)
		}
	}
	if flushed == nil {
		t.Fatalf("no \"cdc batch flushed\" line in:\n%s", logs)
	}
	for field, want := range map[string]any{"rows": 8.0, "calls": 2.0, "rows_sent": 2.0, "deletes_sent": 3.0,
		"rows_deleted": 3.0, "tool": "postgresql_delete_data,postgresql_upsert_data"} {
		if flushed[field] != want {
			t.Fatalf("flush line %s=%v, want %v (line %v)", field, flushed[field], want, flushed)
		}
	}
	for _, value := range []string{"again", "gone"} {
		if strings.Contains(logs, value) {
			t.Fatalf("a row value (%q) reached the logs:\n%s", value, logs)
		}
	}
}

// batchFor is every change the open shop.orders batch holds (zero when none is open).
func (b *cdcDBBatcher) batchFor(t *testing.T) []map[string]interface{} {
	t.Helper()
	for _, batch := range b.batches {
		if batch.table == "shop.orders" {
			return batch.rows
		}
	}
	return nil
}

// A call before the final one stamps the floor that is already safe without this
// batch, so its _rsync_cdc_offsets row never moves past an unlanded change.
func TestBatchApply_AnEarlierCallStampsOnlyWhatAlreadyLanded(t *testing.T) {
	dest := &batchDest{}
	b, _ := batchApplyBatcher(t, "postgresql", nil, dest)
	ctx := t.Context()

	msg, sm := orderChange(0, "c", row("id", 1, "name", "a"))
	b.add(ctx, msg, sm)
	flushOpenBatch(t, b, "test")

	msg, sm = orderChange(1, "d", row("id", 2))
	b.add(ctx, msg, sm)
	msg, sm = orderChange(2, "c", row("id", 3, "name", "c"))
	b.add(ctx, msg, sm)
	flushOpenBatch(t, b, "test")

	writes := dest.writes()
	if len(writes) != 3 {
		t.Fatalf("want 3 writes, got %v", writes)
	}
	if off, ok := writes[1].stamp(); !ok || off != 0 {
		t.Fatalf("the second flush's delete_data stamped (%d, %v), want 0 — what had landed before it", off, ok)
	}
	if off, ok := writes[2].stamp(); !ok || off != 2 {
		t.Fatalf("the second flush's upsert_data stamped (%d, %v), want 2", off, ok)
	}
}

// A delete keyed only by the message key may not compare equal to the same key read
// from an upsert row, so it never joins a batch holding upserts: the batch is sealed
// and flushed first, and the two calls keep the changes' order.
func TestBatchApply_AMessageKeyDeleteSealsABatchOfUpserts(t *testing.T) {
	dest := &batchDest{}
	b, _ := batchApplyBatcher(t, "postgresql", nil, dest)
	ctx := t.Context()

	msg, sm := orderChange(0, "c", row("id", 1, "name", "a"))
	b.add(ctx, msg, sm)
	msg, sm = orderChange(1, "d", nil)
	sm.PK = map[string]interface{}{"id": 1}
	if adm := b.admit(msg, sm); adm.decline || !adm.pkDerived || adm.kind != cdcBatchDelete {
		t.Fatalf("admit = %+v, want a batched delete keyed from the message key", adm)
	}
	b.add(ctx, msg, sm)
	b.lanes.waitAll()
	if rows := b.batchFor(t); len(rows) != 1 {
		t.Fatalf("the open batch holds %d changes, want only the delete: the upsert batch must be sealed first", len(rows))
	}
	flushOpenBatch(t, b, "test")

	writes := dest.writes()
	if len(writes) != 2 || writes[0].name != "postgresql_upsert_data" || writes[1].name != "postgresql_delete_data" {
		t.Fatalf("want upsert_data then delete_data, got %v", writes)
	}
	if got := writes[1].dataJSON(t); got != `[{"id":1}]` {
		t.Fatalf("delete_data keys %s, want [{\"id\":1}]", got)
	}
}

// A warehouse flush keeps each key's final state and makes one MERGE per row shape:
// the upserts that share columns and value kinds go together, a delete is a tombstone
// row in its own MERGE, and a row with another shape gets its own.
func TestBatchApply_WarehouseMergesOncePerShape(t *testing.T) {
	dest := &batchDest{}
	b, _ := batchApplyBatcher(t, "bigquery", nil, dest)
	if !b.warehouse {
		t.Fatal("bigquery must be batched as a warehouse")
	}
	ctx := t.Context()
	for i, c := range []struct {
		op  string
		row map[string]interface{}
	}{
		{"c", row("id", 1, "name", "a")},
		{"u", row("id", 1, "name", "b")},
		{"c", row("id", 2, "name", "c")},
		{"d", row("id", 2, "name", "c")},
		{"c", row("id", 3, "name", "x")},
		{"c", row("id", 4, "name", "y", "qty", 5)},
		{"d", row("id", 5, "name", "z")},
		{"c", row("id", 5, "name", "back")}, // an upsert after a delete replaces the tombstone
	} {
		msg, sm := orderChange(int64(i), c.op, c.row)
		if !b.add(ctx, msg, sm) {
			t.Fatalf("offset %d (%s) was declined", i, c.op)
		}
	}
	flushOpenBatch(t, b, "test")

	writes := dest.writes()
	want := []struct{ op, data string }{
		{"upsert", `[{"id":1,"name":"b"},{"id":3,"name":"x"},{"id":5,"name":"back"}]`},
		{"delete", `[{"_rsync_deleted":true,"id":2,"name":"c"}]`},
		{"upsert", `[{"id":4,"name":"y","qty":5}]`},
	}
	if len(writes) != len(want) {
		t.Fatalf("want %d merges, got %d: %v", len(want), len(writes), writes)
	}
	for i, w := range want {
		c := writes[i]
		if c.name != "bigquery_merge" || c.args["operation"] != w.op || c.dataJSON(t) != w.data {
			t.Fatalf("merge %d = %s op=%v data=%s, want bigquery_merge op=%s data=%s", i, c.name, c.args["operation"], c.dataJSON(t), w.op, w.data)
		}
		if fmt.Sprint(c.args["primary_keys"]) != "[id]" {
			t.Fatalf("merge %d primary_keys %v, want [id]", i, c.args["primary_keys"])
		}
		_, stamped := c.stamp()
		if final := i == len(want)-1; stamped != final {
			t.Fatalf("merge %d stamped=%v; only the final call stamps the batch's offset here", i, stamped)
		}
	}
}

func TestWarehouseShapeKey_SeparatesKindsAndTombstones(t *testing.T) {
	a := warehouseShapeKey(row("id", 1, "name", "a"), "u|")
	if b := warehouseShapeKey(row("name", "z", "id", 9), "u|"); a != b {
		t.Fatalf("same columns and kinds must share a shape: %q vs %q", a, b)
	}
	for name, other := range map[string]string{
		"a null value":          warehouseShapeKey(row("id", 1, "name", nil), "u|"),
		"a float":               warehouseShapeKey(row("id", 1.5, "name", "a"), "u|"),
		"an extra column":       warehouseShapeKey(row("id", 1, "name", "a", "qty", 2), "u|"),
		"the tombstone prefix":  warehouseShapeKey(row("id", 1, "name", "a"), "d|"),
		"a json object":         warehouseShapeKey(row("id", 1, "name", map[string]interface{}{}), "u|"),
		"a json number decimal": warehouseShapeKey(row("id", json.Number("1.5"), "name", "a"), "u|"),
	} {
		if other == a {
			t.Fatalf("%s must change the shape (%q)", name, a)
		}
	}
	if valueKind(float64(3)) != valueKind(3) || valueKind(json.Number("3")) != "i" {
		t.Fatal("a whole number is one kind however it was decoded")
	}
}

// admit decides which changes a batch takes. Anything it declines goes to the
// single-event path, unchanged.
func TestBatchApply_AdmitDeclinesWhatABatchCannotRepresent(t *testing.T) {
	cases := []struct {
		name     string
		destType string
		destCfg  map[string]interface{}
		build    func() (kafka.Message, *SinkMessage)
		want     string // decline, skip, upsert, delete, delete-pk
	}{
		{"an unknown op", "postgresql", nil, func() (kafka.Message, *SinkMessage) {
			m, sm := orderChange(0, "c", row("id", 1))
			sm.CDCOp = "t"
			return m, sm
		}, "decline"},
		{"a multi-row change", "postgresql", nil, func() (kafka.Message, *SinkMessage) {
			m, sm := orderChange(0, "c", row("id", 1))
			sm.Data = []map[string]interface{}{row("id", 1), row("id", 2)}
			return m, sm
		}, "decline"},
		{"an upsert without a row", "postgresql", nil, func() (kafka.Message, *SinkMessage) {
			m, sm := orderChange(0, "u", nil)
			return m, sm
		}, "skip"},
		{"a keyed upsert", "postgresql", nil, func() (kafka.Message, *SinkMessage) { return orderChange(0, "c", row("id", 1)) }, "upsert"},
		{"a delete with its Before image", "postgresql", nil, func() (kafka.Message, *SinkMessage) {
			return orderChange(0, "d", row("id", 1, "name", "a"))
		}, "delete"},
		{"a mysql delete", "mysql", nil, func() (kafka.Message, *SinkMessage) { return orderChange(0, "d", row("id", 1)) }, "delete"},
		{"a delete keyed only by the message key", "postgresql", nil, func() (kafka.Message, *SinkMessage) {
			m, sm := orderChange(0, "d", nil)
			sm.PK = map[string]interface{}{"id": 1}
			return m, sm
		}, "delete-pk"},
		{"a delete without a key", "postgresql", nil, func() (kafka.Message, *SinkMessage) {
			m, sm := orderChange(0, "d", row("id", 1))
			sm.PK = nil
			return m, sm
		}, "decline"},
		{"a delete whose key the two paths read differently", "postgresql", map[string]interface{}{"pk_fields": "id"},
			func() (kafka.Message, *SinkMessage) { return orderChange(0, "d", row("id", 1)) }, "decline"},
		{"a delete to a database without a bulk delete", "sqlserver", nil,
			func() (kafka.Message, *SinkMessage) { return orderChange(0, "d", row("id", 1)) }, "decline"},
		{"a delete to a document database", "mongodb", nil,
			func() (kafka.Message, *SinkMessage) { return orderChange(0, "d", row("id", 1)) }, "decline"},
		{"a document database in append mode", "mongodb", map[string]interface{}{"cdc_write_mode": "append"},
			func() (kafka.Message, *SinkMessage) { return orderChange(0, "c", row("id", 1)) }, "decline"},
		{"an append-mode delete without an image or key", "postgresql", map[string]interface{}{"cdc_write_mode": "append"},
			func() (kafka.Message, *SinkMessage) {
				m, sm := orderChange(0, "d", nil)
				return m, sm
			}, "decline"},
		{"an append-mode change without a row", "postgresql", map[string]interface{}{"cdc_write_mode": "append"},
			func() (kafka.Message, *SinkMessage) {
				m, sm := orderChange(0, "c", nil)
				return m, sm
			}, "skip"},
		{"a warehouse delete without a key", "bigquery", nil, func() (kafka.Message, *SinkMessage) {
			m, sm := orderChange(0, "d", row("id", 1))
			sm.PK = nil
			return m, sm
		}, "decline"},
		{"a warehouse row whose key is null", "bigquery", nil, func() (kafka.Message, *SinkMessage) {
			return orderChange(0, "c", row("id", nil, "name", "a"))
		}, "decline"},
		{"a warehouse delete", "bigquery", nil, func() (kafka.Message, *SinkMessage) { return orderChange(0, "d", row("id", 1)) }, "delete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := batchApplyBatcher(t, tc.destType, tc.destCfg, &batchDest{})
			msg, sm := tc.build()
			adm := b.admit(msg, sm)
			var got string
			switch {
			case adm.decline:
				got = "decline"
			case adm.skip:
				got = "skip"
			case adm.kind == cdcBatchDelete && adm.pkDerived:
				got = "delete-pk"
			case adm.kind == cdcBatchDelete:
				got = "delete"
			default:
				got = "upsert"
			}
			if got != tc.want {
				t.Fatalf("admit = %s (%+v), want %s", got, adm, tc.want)
			}
			if tc.want == "delete" && !b.warehouse {
				if k := fmt.Sprint(adm.row); k != "map[id:1]" {
					t.Fatalf("a relational delete carries its key columns only, got %s", k)
				}
			}
		})
	}
}

// The per-row fallback writes each change on its own: a delete as a delete, counted
// as one; a position the batch already landed is acknowledged without a second write.
func TestBatchApply_PerRowFallbackDeletesAndSkipsWhatLanded(t *testing.T) {
	dest := &batchDest{}
	b, hw := batchApplyBatcher(t, "postgresql", nil, dest)
	ctx := t.Context()
	msg, sm := orderChange(0, "c", row("id", 1, "name", "a"))
	b.add(ctx, msg, sm)
	msg, sm = orderChange(1, "d", row("id", 2, "name", "b"))
	b.add(ctx, msg, sm)

	for key, batch := range b.batches {
		b.flushBatchPerRow(ctx, key, batch, "test", errors.New("the batch call failed"), map[int]bool{0: true})
	}

	writes := dest.writes()
	if len(writes) != 1 || writes[0].name != "postgresql_delete_data" {
		t.Fatalf("want only the delete written row by row (position 0 had landed), got %v", writes)
	}
	if got := writes[0].dataJSON(t); got != `[{"id":2}]` {
		t.Fatalf("per-row delete keys %s, want [{\"id\":2}]", got)
	}
	if got := loadCounter(b.cdcDeletes, "shop.orders"); got != 1 {
		t.Fatalf("per-row deletes counted %d, want 1", got)
	}
	if got := loadCounter(b.cdcInserts, "shop.orders"); got != 1 {
		t.Fatalf("the landed insert counted %d, want 1", got)
	}
	if !hw.seen(batchApplyTopic, 0, 1) {
		t.Fatal("both positions handled: the partition must be written through offset 1")
	}
}

// A worker running a database batcher widens its stall window to that batcher's
// interval too: a warehouse holds changes for 60 s by default, and a stall watchdog
// shorter than that would kill a healthy worker between two flushes.
func TestConsumerStallWindow_CoversTheDatabaseBatcherInterval(t *testing.T) {
	db := &cdcDBBatcher{params: cdcBatchingParams{flushInterval: 200 * time.Second}}
	obj := &cdcObjectBatcher{params: cdcBatchingParams{flushInterval: 30 * time.Second}}
	want := stallWindowCoveringFlush(stallWatchdogTimeout(), 200*time.Second)
	if got := consumerStallWindow(nil, db); got != want || got <= 200*time.Second {
		t.Fatalf("stall window %v with a 200 s database batcher, want %v (longer than the interval)", got, want)
	}
	if got := consumerStallWindow(obj, db); got != want {
		t.Fatalf("stall window %v, want the longer batcher's %v", got, want)
	}
}

// batchSettingFixture is testdata/batch_setting_cases.json, the case list the
// kafka-mcp-sink connector's test runs too (tests/test_kafka_sink_batch_settings.py).
type batchSettingFixture struct {
	Settings []struct {
		Key  string `json:"key"`
		Unit string `json:"unit"`
		Lo   int64  `json:"lo"`
		Hi   int64  `json:"hi"`
	} `json:"settings"`
	Cases []struct {
		Name   string      `json:"name"`
		Key    string      `json:"key"`
		Value  interface{} `json:"value"`
		Expect interface{} `json:"expect"`
	} `json:"cases"`
}

func TestBatchSettings_SharedCasesWithConnector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "batch_setting_cases.json"))
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var fx batchSettingFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode shared cases: %v", err)
	}
	if len(fx.Settings) != len(batchSettings) {
		t.Fatalf("fixture names %d settings, the worker %d; update the fixture, cdc_batch_apply.go and connector.py together", len(fx.Settings), len(batchSettings))
	}
	for i, s := range fx.Settings {
		w := batchSettings[i]
		if s.Key != w.key || s.Unit != w.unit || s.Lo != w.lo || s.Hi != w.hi {
			t.Fatalf("fixture setting %+v, worker %+v; update them together", s, w)
		}
	}
	kinds := map[string]int{}
	for _, tc := range fx.Cases {
		switch want := tc.Expect.(type) {
		case float64:
			kinds["accepted"]++
		case string:
			kinds[want]++
		}
	}
	if kinds["accepted"] < 6 || kinds["unset"] < 3 || kinds["refused"] < 15 || len(fx.Cases) != kinds["accepted"]+kinds["unset"]+kinds["refused"] {
		t.Fatalf("shared cases look truncated or malformed: %v of %d", kinds, len(fx.Cases))
	}

	for _, tc := range fx.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var st batchSetting
			for _, s := range batchSettings {
				if s.key == tc.Key {
					st = s
				}
			}
			if st.key == "" {
				t.Fatalf("fixture case names unknown setting %q", tc.Key)
			}
			destCfg := map[string]interface{}{tc.Key: tc.Value}
			n, set, _, valid := wholeNumberSetting(destCfg, st.key, st.lo, st.hi)
			refusal := batchSettingsError(destCfg)
			p := cdcBatchingParams{maxEvents: 7, maxBytes: 7, flushInterval: 7}
			applyDestBatchSettings(&p, destCfg)
			unchanged := p == cdcBatchingParams{maxEvents: 7, maxBytes: 7, flushInterval: 7}
			switch tc.Expect {
			case "unset":
				if set || refusal != nil || !unchanged {
					t.Fatalf("%#v: set=%v refusal=%v params=%+v, want unset", tc.Value, set, refusal, p)
				}
			case "refused":
				if !set || valid || refusal == nil || !unchanged {
					t.Fatalf("%#v: set=%v valid=%v refusal=%v params=%+v, want refused", tc.Value, set, valid, refusal, p)
				}
				if !strings.Contains(refusal.Error(), tc.Key+" must be a whole number"+st.unit) {
					t.Fatalf("refusal %q does not name the setting", refusal)
				}
			default:
				wantN := int64(tc.Expect.(float64))
				if !set || !valid || n != wantN || refusal != nil || unchanged {
					t.Fatalf("%#v: (%d, set=%v, valid=%v, %v) params=%+v, want %d", tc.Value, n, set, valid, refusal, p, wantN)
				}
			}

			// The same value through the real CONFIG path: refused ones stop the worker
			// before it starts, everything else starts.
			cfgJSON, err := json.Marshal(map[string]interface{}{
				"topic": batchApplyTopic, "consumer_group": "g", "destination_connector": "postgresql",
				"destination_config": destCfg,
				"metrics_port":       1,
			})
			if err != nil {
				t.Fatalf("marshal CONFIG: %v", err)
			}
			t.Setenv("CONFIG", string(cfgJSON))
			_, loadErr := loadConfig()
			if refused := tc.Expect == "refused"; refused != (loadErr != nil) {
				t.Fatalf("%#v: loadConfig error = %v, want refused=%v", tc.Value, loadErr, refused)
			}
		})
	}
}

func TestApplyDestBatchSettings_OverridesEachDefault(t *testing.T) {
	defaults := cdcBatchingParams{maxEvents: defaultWarehouseMaxEvents, maxBytes: defaultWarehouseMaxBytes, flushInterval: defaultWarehouseFlushInterval}
	p := defaults
	applyDestBatchSettings(&p, map[string]interface{}{batchMaxRowsKey: "500", batchMaxMBKey: float64(8), batchIntervalSecondsKey: "90"})
	if want := (cdcBatchingParams{maxEvents: 500, maxBytes: 8 << 20, flushInterval: 90 * time.Second}); p != want {
		t.Fatalf("params %+v, want %+v", p, want)
	}
	// One invalid setting keeps every default: loadConfig refuses it, so a batcher that
	// sees one was built without that check and must not guess.
	p = defaults
	applyDestBatchSettings(&p, map[string]interface{}{batchMaxRowsKey: "500", batchIntervalSecondsKey: "241"})
	if p != defaults {
		t.Fatalf("params %+v after an invalid setting, want the defaults %+v", p, defaults)
	}
}
