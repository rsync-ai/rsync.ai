package main

// Batched CDC apply for relational and warehouse destinations (cdcDBBatcher).
//
// A destination connection sets the batch shape, the way a DMS endpoint does:
// batch_max_rows, batch_max_mb and batch_interval_seconds, from the connector's
// Advanced settings. The worker sets the memory ceiling (bufferBudget) and how a
// batch is applied:
//
//   - Relational (PostgreSQL, MySQL, …): every change of a table goes into one batch,
//     deletes included. On flush, repeat changes to the same key collapse into one row,
//     every delete goes out in one delete_data call, then every surviving row in one
//     upsert_data call. Deleting first is correct because the netting below leaves an
//     upsert only for a key whose LAST change was not a delete.
//   - Warehouse (BigQuery, Snowflake, Databricks, Redshift): one row per key, its final
//     state, in one MERGE per table per interval (one per row shape, see
//     warehouseShapeKey).
//   - Append-only history: every change is a row; one import_data / load per column set.
//
// Anything the batch cannot represent exactly is declined by admit and takes the
// single-event path (processCDCEvent), after the table's open batch is flushed, so
// the order of changes to a table is never altered.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// ---------------------------------------------------------------------------
// Destination batch settings
// ---------------------------------------------------------------------------

const (
	batchMaxRowsKey         = "batch_max_rows"
	batchMaxMBKey           = "batch_max_mb"
	batchIntervalSecondsKey = "batch_interval_seconds"
)

// batchSetting is one whole-number batch setting and the range the worker accepts.
// The kafka-mcp-sink connector refuses the same values before it spawns a worker
// (batch_settings_error in versions/v1.0.0/connector.py); both sides run
// testdata/batch_setting_cases.json, so change them together.
type batchSetting struct {
	key    string
	unit   string // " of seconds", or "" for a count
	lo, hi int64
}

// batchSettings bounds: a row cap of a million and 1 GiB keep one batch inside what a
// single destination call can carry; the interval ceiling is the object-storage flush
// ceiling (maxObjectFlushIntervalSeconds), which stays where it is.
var batchSettings = []batchSetting{
	{key: batchMaxRowsKey, lo: 1, hi: 1_000_000},
	{key: batchMaxMBKey, unit: " of MB", lo: 1, hi: 1024},
	{key: batchIntervalSecondsKey, unit: " of seconds", lo: minObjectFlushIntervalSeconds, hi: maxObjectFlushIntervalSeconds},
}

// wholeNumberSetting reads destCfg[key] as a whole number from lo to hi. It accepts
// what the two callers send: a JSON number from a direct caller, or a string from the
// orchestrator (which flattens connection config to strings, a JSON null becoming
// "<nil>"). set=false means unset; set && !valid means refused, with shown the value
// as the refusal prints it (at most 32 characters).
func wholeNumberSetting(destCfg map[string]interface{}, key string, lo, hi int64) (n int64, set bool, shown string, valid bool) {
	raw, present := destCfg[key]
	if !present || raw == nil {
		return 0, false, "", false
	}
	shown = fmt.Sprintf("%v", raw)
	if s, isStr := raw.(string); isStr {
		shown = s
	}
	if len(shown) > 32 {
		shown = shown[:32] + "..."
	}
	switch v := raw.(type) {
	case float64:
		// Range first, so the int64 conversion below is always defined.
		if !(v >= float64(lo) && v <= float64(hi)) || v != float64(int64(v)) {
			return 0, true, shown, false
		}
		n = int64(v)
	case int:
		n = int64(v)
	case int64:
		n = v
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, true, shown, false
		}
		n = i
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "<nil>" || strings.EqualFold(s, "null") {
			return 0, false, "", false
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, true, shown, false
		}
		n = i
	default:
		return 0, true, shown, false
	}
	if n < lo || n > hi {
		return 0, true, shown, false
	}
	return n, true, shown, true
}

// batchSettingsError is the refusal for the first invalid batch setting, or nil.
// loadConfig refuses it at startup, so a batcher never runs on a value nobody set.
func batchSettingsError(destCfg map[string]interface{}) error {
	for _, s := range batchSettings {
		if _, set, shown, valid := wholeNumberSetting(destCfg, s.key, s.lo, s.hi); set && !valid {
			return fmt.Errorf("%s must be a whole number%s from %d to %d (got %q). Clear the setting to use the default",
				s.key, s.unit, s.lo, s.hi, shown)
		}
	}
	return nil
}

// applyDestBatchSettings lets the destination's batch_* settings win over the worker's
// batching defaults. loadConfig has already refused an invalid value, so one here only
// means the batcher was built without it; keep the default rather than guess.
func applyDestBatchSettings(p *cdcBatchingParams, destCfg map[string]interface{}) {
	if err := batchSettingsError(destCfg); err != nil {
		logf("warning", "ignoring invalid destination batch settings, keeping the defaults: %v", err)
		return
	}
	for _, st := range batchSettings {
		n, set, _, _ := wholeNumberSetting(destCfg, st.key, st.lo, st.hi)
		if !set {
			continue
		}
		switch st.key {
		case batchMaxRowsKey:
			p.maxEvents = int(n)
		case batchMaxMBKey:
			p.maxBytes = int(n) << 20
		case batchIntervalSecondsKey:
			p.flushInterval = time.Duration(n) * time.Second
		}
	}
}

// Warehouse defaults: a MERGE rewrites table files and is billed per statement, so it
// runs once a minute per table, not every 5 s. Each still yields to cdc_batching and
// to the destination's batch_* settings.
const (
	defaultWarehouseFlushInterval = 60 * time.Second
	defaultWarehouseMaxEvents     = 10_000
	defaultWarehouseMaxBytes      = 32 << 20
)

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

type cdcBatchEventKind uint8

const (
	cdcBatchUpsert cdcBatchEventKind = iota
	cdcBatchDelete
)

// kindOf is the kind of batch position i. A batch built without kinds (tests, and
// every batch before deletes were batched) holds upserts only.
func (b *cdcDBBatch) kindOf(i int) cdcBatchEventKind {
	if i >= 0 && i < len(b.kinds) {
		return b.kinds[i]
	}
	return cdcBatchUpsert
}

// The destination-config key lists each path reads, in order. relationalKeySettings is
// what the upsert batch has always read; singleEventKeySettings is the single-event
// path's (writeCDCToDestination). A delete is batched only when both name the same
// key, so batching never changes which columns a delete matches.
var (
	relationalKeySettings  = []string{"key_fields", "primary_keys", "primary_key_fields"}
	singleEventKeySettings = []string{"key_fields", "primary_keys", "primary_key_fields", "pk_fields", "primary_key"}
	warehouseKeySettings   = []string{"primary_keys", "key_fields", "primary_key_fields"}
)

// batchedDeleteDestinations take a list of keys in one relationalDeleteTool call.
// Others keep the single-event delete. Relational databases only: the sink never
// deletes a record in an API destination (TestTheSinkDeletesNoRecordInAnAPI).
var batchedDeleteDestinations = map[string]bool{"postgresql": true, "postgres": true, "mysql": true}

// relationalDeleteTool is the delete a batch sends, only ever to a
// batchedDeleteDestinations connector.
const relationalDeleteTool = "delete_data"

func configuredKeyFields(destCfg map[string]interface{}, settings []string) []string {
	for _, k := range settings {
		if v, ok := destCfg[k]; ok {
			if kf := toStringSlice(v); len(kf) > 0 {
				return kf
			}
		}
	}
	return nil
}

// pickKey projects row onto keyFields; false when a key column is absent or null.
func pickKey(row map[string]interface{}, keyFields []string) (map[string]interface{}, bool) {
	if row == nil || len(keyFields) == 0 {
		return nil, false
	}
	out := make(map[string]interface{}, len(keyFields))
	for _, f := range keyFields {
		v, ok := row[f]
		if !ok || v == nil {
			return nil, false
		}
		out[f] = v
	}
	return out, true
}

func sameFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Admission
// ---------------------------------------------------------------------------

// cdcAdmission is what add() needs to buffer one change, or why it cannot.
type cdcAdmission struct {
	kind      cdcBatchEventKind
	row       map[string]interface{}
	keyFields []string
	// pkDerived: the delete's key came from the message key, not a row image, so it
	// may not compare equal to the same key read from an upsert row.
	pkDerived bool
	// unkeyed: an upsert the netting cannot key; it is written as it is.
	unkeyed bool
	// skip: nothing to write (a filtered change); acknowledge it.
	skip bool
	// decline: the batch cannot represent it; the single-event path takes it.
	decline bool
}

// admit decides how one change enters the batch. It reads the message and never
// changes it.
func (b *cdcDBBatcher) admit(msg kafka.Message, sm *SinkMessage) cdcAdmission {
	var kind cdcBatchEventKind
	switch sm.CDCOp {
	case "c", "r", "u":
		kind = cdcBatchUpsert
	case "d":
		kind = cdcBatchDelete
	default:
		return cdcAdmission{decline: true}
	}
	// One row per change. A multi-row change is not something Debezium sends; the
	// single-event path writes it as it always has.
	if len(sm.Data) > 1 {
		return cdcAdmission{decline: true}
	}
	upsertRow := func() map[string]interface{} {
		if len(sm.Data) > 0 {
			return sm.Data[0]
		}
		return sm.After
	}

	switch {
	case cdcAppendMode(b.cfg):
		// A document destination keys each history entry on its Kafka record, per
		// event; that path stays single-event.
		if isDocumentDBConnector(b.destType) {
			return cdcAdmission{decline: true}
		}
		if kind == cdcBatchDelete {
			r := sm.Before
			if r == nil {
				r = sm.PK
			}
			if r == nil {
				return cdcAdmission{decline: true}
			}
			return cdcAdmission{kind: kind, row: withCDCIdentity(r, sm)}
		}
		r := upsertRow()
		if r == nil {
			return cdcAdmission{skip: true}
		}
		return cdcAdmission{kind: kind, row: withCDCIdentity(r, sm)}

	case b.warehouse:
		if kind == cdcBatchDelete && (sm.PK == nil || len(sm.KeyFields) == 0) {
			return cdcAdmission{decline: true}
		}
		keys := configuredKeyFields(b.destCfg, warehouseKeySettings)
		if len(keys) == 0 && len(sm.KeyFields) > 0 {
			keys = append([]string{}, sm.KeyFields...)
		}
		if len(keys) == 0 {
			return cdcAdmission{decline: true}
		}
		var row map[string]interface{}
		if kind == cdcBatchDelete {
			row = sm.Before
			if row == nil && len(sm.Data) > 0 {
				row = sm.Data[0]
			}
			if row != nil {
				row = copyMap(row)
				row["_rsync_deleted"] = true
			}
		} else {
			if len(sm.Data) == 0 && sm.After == nil {
				return cdcAdmission{skip: true}
			}
			row = sm.After
			if row == nil {
				row = sm.Data[0]
			}
		}
		// Netting is by key, so a row whose key cannot be read is not batched.
		if _, ok := pickKey(row, keys); !ok {
			return cdcAdmission{decline: true}
		}
		return cdcAdmission{kind: kind, row: row, keyFields: keys}

	case kind == cdcBatchUpsert:
		row := upsertRow()
		if row == nil {
			return cdcAdmission{skip: true}
		}
		keys := configuredKeyFields(b.destCfg, relationalKeySettings)
		if len(keys) == 0 && len(sm.KeyFields) > 0 {
			keys = append([]string{}, sm.KeyFields...)
		}
		// No third source: a key is what the source (its message key) or the
		// destination config declares, never a column that looks like one
		// (KI-CDC-SINK-GUESSED-KEY-COLLAPSES-ROWS). A keyless row bound for a document
		// destination is keyed on its Kafka record
		// (KI-MONGODB-DEST-KEYLESS-REPLAY-DUPLICATES): a deterministic _id, upserted,
		// so a redelivery replaces its own document instead of inserting a second one.
		if len(keys) == 0 && isDocumentDBConnector(b.destType) {
			row = withKeylessDocumentID(row, b.cfg.PipelineID, msg, 0)
			keys = append([]string{}, keylessDocumentKey...)
		}
		unkeyed := len(keys) == 0
		if !unkeyed {
			_, ok := rowKeyIdentity(row, keys)
			unkeyed = !ok
		}
		return cdcAdmission{kind: kind, row: row, keyFields: keys, unkeyed: unkeyed}

	default: // relational delete
		if sm.PK == nil || len(sm.KeyFields) == 0 || isDocumentDBConnector(b.destType) || !batchedDeleteDestinations[b.destType] {
			return cdcAdmission{decline: true}
		}
		configured := configuredKeyFields(b.destCfg, relationalKeySettings)
		if !sameFields(configured, configuredKeyFields(b.destCfg, singleEventKeySettings)) {
			return cdcAdmission{decline: true}
		}
		keys := configured
		if len(keys) == 0 {
			keys = append([]string{}, sm.KeyFields...)
		}
		if k, ok := pickKey(sm.Before, keys); ok {
			return cdcAdmission{kind: kind, row: k, keyFields: keys}
		}
		if len(sm.Data) > 0 {
			if k, ok := pickKey(sm.Data[0], keys); ok {
				return cdcAdmission{kind: kind, row: k, keyFields: keys}
			}
		}
		if k, ok := pickKey(sm.PK, keys); ok {
			return cdcAdmission{kind: kind, row: k, keyFields: keys, pkDerived: true}
		}
		return cdcAdmission{decline: true}
	}
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

// cdcApplyCall is one destination call of a flush.
type cdcApplyCall struct {
	tool    string // without the destination prefix
	args    map[string]interface{}
	sent    int  // rows sent
	deletes bool // a delete_data call
	// positions are the batch positions this call writes or supersedes.
	positions []int
}

// cdcApplyPlan is every call one flush makes, in order, and the ensure that runs first.
type cdcApplyPlan struct {
	calls []cdcApplyCall
	// sent maps a document destination's rows back to batch positions (destSkippedRows).
	sent []int

	ensure        bool
	ensureRows    []map[string]interface{}
	ensureKeys    []string
	ensureTypes   map[string]string
	detectDropped bool
}

func (b *cdcDBBatcher) newCallArgs(batch *cdcDBBatch, rows []map[string]interface{}, operation string, positions []int) map[string]interface{} {
	args := map[string]interface{}{
		"config":       b.destCfg,
		"table":        batch.targetTable,
		"source_table": batch.table,
		"data":         rows,
		"operation":    operation,
	}
	if n := len(positions); n > 0 {
		if last := positions[n-1]; last < len(batch.sms) && batch.sms[last] != nil {
			sm := batch.sms[last]
			args["cdc_metadata"] = map[string]interface{}{
				"op":          bronzeOpFromDebezium(sm.CDCOp),
				"debezium_op": sm.CDCOp,
				"tx_id":       sm.TxID,
				"lsn":         sm.LSN,
				"source_ts":   sm.SourceTS,
			}
		}
	}
	// batch.targetTable is bare; no-op when the pipeline has no real namespace.
	addNamespaceParam(args, batch.namespace)
	return args
}

// planFlush turns a batch into its destination calls.
func (b *cdcDBBatcher) planFlush(batch *cdcDBBatch) cdcApplyPlan {
	switch {
	case cdcAppendMode(b.cfg):
		return b.planAppend(batch)
	case b.warehouse:
		return b.planWarehouse(batch)
	case isDocumentDBConnector(b.destType):
		return b.planDocument(batch)
	default:
		return b.planRelational(batch)
	}
}

// planRelational nets the batch per key. Upserts to one key merge column by column
// into one row (a later value wins, a column a later image lacks keeps the earlier
// value, as applying them in order would); a delete ends the key's row and records
// the key once; an upsert after the delete starts a new row. So every recorded delete
// precedes every surviving upsert of its key, and the deletes go first.
func (b *cdcDBBatcher) planRelational(batch *cdcDBBatch) cdcApplyPlan {
	type slot struct {
		row       map[string]interface{}
		copied    bool
		positions []int
	}
	var (
		order      []*slot
		live       = map[string]*slot{}
		delRows    []map[string]interface{}
		delPos     []int
		delSeen    = map[string]bool{}
		upsertKeys = batch.keyFields
	)
	for i, row := range batch.rows {
		if batch.kindOf(i) == cdcBatchDelete {
			id, _ := rowKeyIdentity(row, batch.keyFields)
			if s := live[id]; s != nil {
				delete(live, id)
				// Superseded by this delete: they land when it does.
				delPos = append(delPos, s.positions...)
				s.row, s.positions = nil, nil
			}
			delPos = append(delPos, i)
			if !delSeen[id] {
				delSeen[id] = true
				delRows = append(delRows, row)
			}
			continue
		}
		id, keyed := rowKeyIdentity(row, upsertKeys)
		if !keyed || len(upsertKeys) == 0 {
			order = append(order, &slot{row: row, positions: []int{i}})
			continue
		}
		if s := live[id]; s != nil {
			if !s.copied {
				s.row, s.copied = copyMap(s.row), true
			}
			for k, v := range row {
				s.row[k] = v
			}
			s.positions = append(s.positions, i)
			continue
		}
		s := &slot{row: row, positions: []int{i}}
		live[id] = s
		order = append(order, s)
	}

	plan := cdcApplyPlan{ensure: true, ensureKeys: batch.keyFields, ensureTypes: batch.columnTypes}
	if len(delRows) > 0 {
		args := b.newCallArgs(batch, delRows, "delete", delPos)
		args["key_fields"] = batch.keyFields
		args["primary_key_fields"] = batch.keyFields
		plan.calls = append(plan.calls, cdcApplyCall{tool: relationalDeleteTool, args: args, sent: len(delRows), deletes: true, positions: delPos})
	}
	var upRows []map[string]interface{}
	var upPos []int
	for _, s := range order {
		if s.row != nil {
			upRows = append(upRows, s.row)
			upPos = append(upPos, s.positions...)
		}
	}
	if len(upRows) > 0 {
		sort.Ints(upPos)
		args := b.newCallArgs(batch, upRows, "upsert", upPos)
		tool := "upsert_data"
		if len(batch.keyFields) > 0 {
			args["key_fields"] = batch.keyFields
			args["primary_key_fields"] = batch.keyFields
		} else {
			// Keyless table → synthetic-PK path: ensureDestinationTable created
			// _rsync_row_hash (NOT NULL) + a unique index for exactly this case, and
			// upsert_data(synthetic_pk=true) computes it. A plain insert would put a NULL
			// hash in every row.
			args["synthetic_pk"] = true
		}
		// The write call carries the column types too: if ensure was skipped or lost a
		// race, the connector's own create is still typed (KI-CDC-TYPE-RACE).
		if len(batch.columnTypes) > 0 {
			args["column_types"] = batch.columnTypes
		}
		plan.calls = append(plan.calls, cdcApplyCall{tool: tool, args: args, sent: len(upRows), positions: upPos})
		plan.ensureRows = upRows
		plan.detectDropped = true
	} else {
		// A batch of deletes: ensure still creates the table, from the key columns.
		plan.ensureRows = delRows
	}
	return plan
}

// planDocument writes each key once, by its last event (#22, lastEventPerKey): a
// document destination applies a batch unordered, and every write replaces the whole
// document. Deletes never reach a document batch (admit declines them).
func (b *cdcDBBatcher) planDocument(batch *cdcDBBatch) cdcApplyPlan {
	data, sent := batch.rows, lastEventPerKey(batch.rows, batch.keyFields)
	if sent != nil {
		data = make([]map[string]interface{}, len(sent))
		for j, i := range sent {
			data[j] = batch.rows[i]
		}
	}
	all := make([]int, len(batch.rows))
	for i := range all {
		all[i] = i
	}
	args := b.newCallArgs(batch, data, "upsert", all)
	if len(batch.keyFields) > 0 {
		args["key_fields"] = batch.keyFields
		args["primary_key_fields"] = batch.keyFields
	}
	if len(batch.columnTypes) > 0 {
		args["column_types"] = batch.columnTypes
	}
	return cdcApplyPlan{
		calls: []cdcApplyCall{{tool: "upsert_data", args: args, sent: len(data), positions: all}},
		sent:  sent,
	}
}

// planWarehouse keeps one row per key, its final state, in first-seen order: an upsert
// overlays the key's upsert row, a delete replaces it with the tombstone, and an
// upsert after a delete replaces the tombstone. Rows are then grouped by shape
// (warehouseShapeKey), one MERGE per group. Keys are disjoint across groups, so the
// groups' order does not matter.
func (b *cdcDBBatcher) planWarehouse(batch *cdcDBBatch) cdcApplyPlan {
	type slot struct {
		row       map[string]interface{}
		copied    bool
		del       bool
		positions []int
	}
	var order []*slot
	byKey := map[string]*slot{}
	for i, row := range batch.rows {
		del := batch.kindOf(i) == cdcBatchDelete
		id, _ := rowKeyIdentity(row, batch.keyFields)
		s := byKey[id]
		if s == nil {
			s = &slot{row: row, del: del}
			byKey[id] = s
			order = append(order, s)
		} else if del || s.del {
			s.row, s.copied, s.del = row, false, del
		} else {
			if !s.copied {
				s.row, s.copied = copyMap(s.row), true
			}
			for k, v := range row {
				s.row[k] = v
			}
		}
		s.positions = append(s.positions, i)
	}

	type group struct {
		del       bool
		rows      []map[string]interface{}
		positions []int
	}
	var groups []*group
	byShape := map[string]*group{}
	for _, s := range order {
		prefix := "u|"
		if s.del {
			prefix = "d|"
		}
		shape := warehouseShapeKey(s.row, prefix)
		g := byShape[shape]
		if g == nil {
			g = &group{del: s.del}
			byShape[shape] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, s.row)
		g.positions = append(g.positions, s.positions...)
	}

	var plan cdcApplyPlan
	for _, g := range groups {
		sort.Ints(g.positions)
		op := "upsert"
		if g.del {
			op = "delete"
		}
		args := b.newCallArgs(batch, g.rows, op, g.positions)
		args["primary_keys"] = batch.keyFields
		args["key_fields"] = batch.keyFields
		plan.calls = append(plan.calls, cdcApplyCall{tool: "merge", args: args, sent: len(g.rows), deletes: g.del, positions: g.positions})
	}
	return plan
}

// planAppend writes every change as its own row. A relational import_data takes its
// column list from the first row, so rows are grouped by column set; a warehouse load
// infers its schema from the rows, so by shape.
func (b *cdcDBBatcher) planAppend(batch *cdcDBBatch) cdcApplyPlan {
	type group struct {
		rows      []map[string]interface{}
		positions []int
	}
	var groups []*group
	byShape := map[string]*group{}
	for i, row := range batch.rows {
		var shape string
		if b.warehouse {
			shape = warehouseShapeKey(row, "")
		} else {
			cols := make([]string, 0, len(row))
			for k := range row {
				cols = append(cols, k)
			}
			sort.Strings(cols)
			shape = strings.Join(cols, "\x00")
		}
		g := byShape[shape]
		if g == nil {
			g = &group{}
			byShape[shape] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, row)
		g.positions = append(g.positions, i)
	}

	plan := cdcApplyPlan{}
	types := mergeCDCIdentityColumnTypes(batch.columnTypes)
	for _, g := range groups {
		args := b.newCallArgs(batch, g.rows, "insert", g.positions)
		tool := "load"
		if !b.warehouse {
			tool = "import_data"
			args["column_types"] = types
		}
		plan.calls = append(plan.calls, cdcApplyCall{tool: tool, args: args, sent: len(g.rows), positions: g.positions})
		if !b.warehouse {
			plan.ensureRows = append(plan.ensureRows, g.rows...)
		}
	}
	if !b.warehouse {
		plan.ensure = true
		plan.ensureTypes = types
	}
	return plan
}

// valueKind is how a warehouse load would type a value: rows whose same column holds
// different kinds go in separate calls, so one row can never retype the column for
// the rest.
func valueKind(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "n"
	case bool:
		return "b"
	case string:
		return "s"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "i"
	case float32:
		return valueKind(float64(x))
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e21 {
			return "i"
		}
		return "f"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "f"
		}
		return "i"
	case map[string]interface{}, []interface{}:
		return "j"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// warehouseShapeKey is a row's column names and value kinds, sorted, after prefix.
// A MERGE takes one shape: BigQuery loads its staging table from the rows' inferred
// schema, and a tombstone and an upsert of the same table differ in _rsync_deleted.
func warehouseShapeKey(row map[string]interface{}, prefix string) string {
	cols := make([]string, 0, len(row))
	for k, v := range row {
		cols = append(cols, k+":"+valueKind(v))
	}
	sort.Strings(cols)
	return prefix + strings.Join(cols, ",")
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

// applyCall makes one planned call. Only the final call of a plan stamps the batch's
// landing floor; an earlier one stamps what is already safe without this batch (or
// nothing, when nothing is), so a restart after a partial flush redelivers the whole
// batch.
func (b *cdcDBBatcher) applyCall(ctx context.Context, batch *cdcDBBatch, call cdcApplyCall, final bool) (map[string]interface{}, int64, error) {
	offset := b.gate.floorIfLanded(batch.topic, batch.partition, noHold, -1)
	if final {
		offset = b.gate.floorIfLanded(batch.topic, batch.partition, batch.firstOffset, batch.lastOffset)
	}
	args := call.args
	// The final call always stamps, as the one-call flush always has; an earlier call
	// stamps only a real floor.
	if final || offset >= 0 {
		args = copyMap(call.args)
		args["kafka_offset"] = map[string]interface{}{
			"pipeline_id": b.cfg.PipelineID,
			"topic":       batch.topic,
			"partition":   batch.partition,
			"offset":      offset,
		}
	}
	result, err := callDestinationTool(ctx, b.httpClient, b.cfg, b.destType, call.tool, args)
	if err != nil {
		return nil, 0, err
	}
	// A success without a write count is the silent-data-loss signature: never
	// fabricate one (see flushBatch). A legitimate zero is trusted.
	n, ok := extractDestRowCount(result)
	if !ok {
		return nil, 0, fmt.Errorf("dest %s_%s returned success without a write-count field (possible non-landing write); table=%s rows=%d",
			b.destType, call.tool, batch.targetTable, call.sent)
	}
	return result, n, nil
}

// applyWithRetry makes one call with the batcher's bounded retry.
func (b *cdcDBBatcher) applyWithRetry(ctx context.Context, batch *cdcDBBatch, call cdcApplyCall, final bool) (map[string]interface{}, int64, error) {
	var lastErr error
	for attempt := 0; attempt <= b.params.maxRetries; attempt++ {
		result, n, err := b.applyCall(ctx, batch, call, final)
		if err == nil {
			return result, n, nil
		}
		lastErr = err
		if attempt < b.params.maxRetries {
			time.Sleep(b.params.backoff*time.Duration(1<<attempt) + time.Duration(rand.Intn(250))*time.Millisecond)
		}
	}
	return nil, 0, lastErr
}

// positionsOf is the set of batch positions the given calls write.
func positionsOf(calls []cdcApplyCall) map[int]bool {
	out := map[int]bool{}
	for _, c := range calls {
		for _, i := range c.positions {
			out[i] = true
		}
	}
	return out
}
