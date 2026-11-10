package main

// API destinations: each source row is written to the record it became
// (generator trust plan P5).
//
// A REST API names a record by an id it makes up when the record is created; the
// source names a row by its key. A destination whose get_capabilities says
// record_ids (a generated REST connector) is therefore written through a key map —
// source key → the id the API gave the record — kept in the platform table
// api_destination_records (migration 144):
//
//   - each row is sent with its record id from the map (update that record) or none
//     (create one), with a time budget under the sink's HTTP timeout;
//   - the connector answers one result per row: created / updated (with the id),
//     failed (with retryable) or pending (not reached within the budget);
//   - pending rows are sent again at once; a retryable failure is sent again after a
//     backoff, up to apiMaxRounds rounds that wrote nothing new; a row that still
//     failed goes to the DLQ ALONE, and the batch's ack counts the rows that landed;
//   - nothing is ever deleted: writing to an app is reverse ETL, where a run updates
//     the records it wrote before. The gateway refuses a Reload into an API
//     (apiDestinationReloadRefusal); one that still arrives updates in place too.
//
// The bench's reference model is llm-service/src/agents/tool_generator/harness/
// sink_loop.py (Sink._send_rows / _write_rows): keep the two in step.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	// apiMaxRounds caps the rounds of a send that write nothing new — the
	// whole-batch retry cap (maxBatchAttempts in main) applied row by row.
	apiMaxRounds = 5
)

// apiWriteTimeBudget is the time_budget_s a write call carries: half the sink's
// HTTP timeout, so the connector stops sending rows and answers while the sink is
// still waiting — leaving room for one slow row. A connector that ran past the
// timeout would have made records whose ids the sink never learns.
func apiWriteTimeBudget() time.Duration { return destHTTPTimeout() / 2 }

// apiRecordWrites reports whether the batch path writes this destination row by
// row through the key map: its get_capabilities says record_ids, and the user did
// not ask for append mode (there every row is a new record, as asked).
func apiRecordWrites(ctx context.Context, httpClient *http.Client, cfg *WorkerConfig, ddl *DDLSupport, destType string) bool {
	if isObjectStorageConnector(destType) || isDataWarehouseConnector(destType) || isDocumentDBConnector(destType) {
		return false
	}
	return !cdcAppendMode(cfg) && ddl.recordIDs(ctx, httpClient, cfg, destType)
}

// apiSourceKey names a source row in the key map: "k:" + sha256 of its key values
// as JSON, or "r:" + sha256 of the whole row when the source has no key — so the
// same row finds the same record, and the platform table holds no source value.
// Every batch decodes numbers the same way (normalizeJSONNumbers), so the hash is
// stable from run to run.
func apiSourceKey(row map[string]interface{}, keyFields []string) string {
	kind := "r:"
	var body interface{} = row
	if len(keyFields) > 0 {
		vals := make([]interface{}, len(keyFields))
		for i, k := range keyFields {
			vals[i] = row[k]
		}
		kind, body = "k:", vals
	}
	b, err := json.Marshal(body) // map keys marshal sorted
	if err != nil {
		b = []byte(fmt.Sprint(body))
	}
	sum := sha256.Sum256(b)
	return kind + hex.EncodeToString(sum[:])
}

// apiRecordScope is one key map: a pipeline's rows of one source table written to
// one destination table. A Reload of one source table never deletes the records
// another one wrote.
type apiRecordScope struct {
	pipelineID, sourceTable, destTable string
}

func apiScopeFor(sm *SinkMessage, destTable string) apiRecordScope {
	src := strings.TrimSpace(sm.SourceTable)
	if src == "" {
		src = sm.Table
	}
	return apiRecordScope{pipelineID: sm.PipelineID, sourceTable: src, destTable: destTable}
}

// apiRecordStore is the key map's storage: pgAPIRecords in production.
type apiRecordStore interface {
	// lookup returns the record id of each mapped key and stamps it with the run
	// (last_execution_id: the last run that read the row).
	lookup(ctx context.Context, s apiRecordScope, keys []string, executionID string) (map[string]string, error)
	// save maps each key to its record id, stamped with the run.
	save(ctx context.Context, s apiRecordScope, ids map[string]string, executionID string) error
	// forget removes keys from the map.
	forget(ctx context.Context, s apiRecordScope, keys []string) error
}

// apiDestCaller makes one destination tool call: callDestinationToolAnswer in
// production, which hands back the answer of a call that said success=false.
type apiDestCaller func(ctx context.Context, op string, args map[string]interface{}) (map[string]interface{}, error)

type apiRowWriter struct {
	call   apiDestCaller
	store  apiRecordStore
	sleep  func(context.Context, time.Duration) bool
	budget time.Duration
}

func newAPIRowWriter(httpClient *http.Client, cfg *WorkerConfig, destType string, store apiRecordStore) *apiRowWriter {
	return &apiRowWriter{
		call: func(ctx context.Context, op string, args map[string]interface{}) (map[string]interface{}, error) {
			return callDestinationToolAnswer(ctx, httpClient, cfg, destType, op, args)
		},
		store:  store,
		sleep:  ctxAwareSleep,
		budget: apiWriteTimeBudget(),
	}
}

func (w *apiRowWriter) budgetText() string {
	return fmt.Sprintf("not sent within the %g s time budget", w.budget.Seconds())
}

// send sends keys until each has a final answer, and returns the keys that failed
// with their reason. landed gets, after each call, the keys the destination wrote
// and the record id it gave each. A pending row is sent again at once; a round with
// a retryable failure, or that wrote nothing, counts a round and backs off
// round*2 s. The error is ctx's, or landed's.
func (w *apiRowWriter) send(ctx context.Context, op string, keys []string,
	paramsFor func([]string) map[string]interface{},
	landed func(context.Context, map[string]string) error) (map[string]string, error) {
	failed := map[string]string{}
	todo := append([]string(nil), keys...)
	rounds := 0
	for len(todo) > 0 {
		if err := ctx.Err(); err != nil {
			return failed, err
		}
		reasons := make(map[string]string, len(todo))
		var retry []string
		progress := false
		res, err := w.call(ctx, op, paramsFor(todo))
		results, isList := res["results"].([]interface{})
		if !isList || len(results) != len(todo) {
			reason := ""
			switch {
			case err != nil:
				reason = err.Error()
			case isList:
				reason = fmt.Sprintf("dest response has %d per-row results for %d rows", len(results), len(todo))
			default:
				reason = fmt.Sprintf("dest response has no per-row results for %d rows", len(todo))
			}
			retry = todo
			for _, k := range todo {
				reasons[k] = reason
			}
		} else {
			got := map[string]string{}
			for i, k := range todo {
				r, _ := results[i].(map[string]interface{})
				switch status := toString(r["status"]); status {
				case "created", "updated", "deleted":
					progress = true
					got[k] = toString(r["id"])
				case "pending":
					retry = append(retry, k)
					reasons[k] = w.budgetText()
				default:
					reason := strings.TrimSpace(toString(r["error"]))
					if reason == "" {
						reason = "the destination refused the row and gave no reason"
					}
					if rt, _ := r["retryable"].(bool); rt {
						retry = append(retry, k)
						reasons[k] = reason
					} else {
						failed[k] = reason
					}
				}
			}
			if len(got) > 0 {
				if err := landed(ctx, got); err != nil {
					return failed, err
				}
			}
		}
		todo = retry
		if len(todo) == 0 {
			break
		}
		// Rows the budget did not reach are sent again at once; anything else
		// waits, and rounds that write nothing end at the cap.
		if progress && allReasonsAre(todo, reasons, w.budgetText()) {
			continue
		}
		rounds++
		if rounds >= apiMaxRounds {
			for _, k := range todo {
				failed[k] = reasons[k]
			}
			break
		}
		if !w.sleep(ctx, time.Duration(rounds)*2*time.Second) {
			return failed, ctx.Err()
		}
	}
	return failed, nil
}

func allReasonsAre(keys []string, reasons map[string]string, want string) bool {
	for _, k := range keys {
		if reasons[k] != want {
			return false
		}
	}
	return true
}

// apiBatchResult is one Kafka message written row by row.
type apiBatchResult struct {
	written    int64                    // rows of the message that landed
	failedRows []map[string]interface{} // the rows that go to the DLQ
	errText    string                   // why they failed; "" when none did
	warnings   []string
}

// writeRows writes one Kafka message's rows through the key map. base holds the
// call's fixed params (config, table, namespace, key fields, column types). The
// error is ctx's or the key map's: the caller leaves the offset uncommitted.
func (w *apiRowWriter) writeRows(ctx context.Context, s apiRecordScope, executionID string,
	rows []map[string]interface{}, keyFields []string, base map[string]interface{}) (apiBatchResult, error) {
	keys := make([]string, len(rows))
	last := map[string]map[string]interface{}{}
	var order []string
	for i, r := range rows {
		k := apiSourceKey(r, keyFields)
		keys[i] = k
		if _, seen := last[k]; !seen {
			order = append(order, k)
		}
		last[k] = r // a key twice in one message: its last row is the record
	}
	ids, err := w.store.lookup(ctx, s, order, executionID)
	if err != nil {
		return apiBatchResult{}, fmt.Errorf("api key map lookup: %w", err)
	}
	var out apiBatchResult
	paramsFor := func(todo []string) map[string]interface{} {
		p := copyParams(base)
		data := make([]map[string]interface{}, len(todo))
		recordIDs := make([]string, len(todo))
		for i, k := range todo {
			data[i] = last[k]
			recordIDs[i] = ids[k]
		}
		p["data"] = data
		p["record_ids"] = recordIDs
		p["time_budget_s"] = w.budget.Seconds()
		return p
	}
	landed := func(ctx context.Context, got map[string]string) error {
		toSave := map[string]string{}
		var noID []string
		for k, id := range got {
			if strings.TrimSpace(id) == "" {
				noID = append(noID, k) // the next run cannot find it: it will be created again
				delete(ids, k)
				continue
			}
			toSave[k] = id
			ids[k] = id
		}
		if len(noID) > 0 {
			out.warnings = append(out.warnings, fmt.Sprintf("%s: the destination wrote %d records and returned no id", s.destTable, len(noID)))
			if err := w.store.forget(ctx, s, noID); err != nil {
				return fmt.Errorf("api key map forget: %w", err)
			}
		}
		if len(toSave) == 0 {
			return nil
		}
		if err := w.store.save(ctx, s, toSave, executionID); err != nil {
			return fmt.Errorf("api key map save: %w", err)
		}
		return nil
	}
	failed, err := w.send(ctx, "upsert_data", order, paramsFor, landed)
	if err != nil {
		return out, err
	}
	for i, r := range rows {
		if _, bad := failed[keys[i]]; bad {
			out.failedRows = append(out.failedRows, r)
		}
	}
	out.written = int64(len(rows) - len(out.failedRows))
	if len(failed) > 0 {
		out.errText = fmt.Sprintf("%d of %d rows failed: %s", len(out.failedRows), len(rows), strings.Join(firstDistinct(failed, 3), "; "))
	}
	return out, nil
}

// firstDistinct returns up to n of the distinct values of m, sorted.
func firstDistinct(m map[string]string, n int) []string {
	seen := map[string]struct{}{}
	var vals []string
	for _, v := range m {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			vals = append(vals, v)
		}
	}
	sort.Strings(vals)
	if len(vals) > n {
		vals = vals[:n]
	}
	return vals
}

func copyParams(base map[string]interface{}) map[string]interface{} {
	p := make(map[string]interface{}, len(base)+3)
	for k, v := range base {
		p[k] = v
	}
	return p
}

// apiWriteParams is the fixed part of an API destination's write call: what
// writeToDestination sends, minus the rows. No ensure_table: an API has no DDL.
func apiWriteParams(cfg *WorkerConfig, sm *SinkMessage, targetTable string) map[string]interface{} {
	p := map[string]interface{}{
		"config": cfg.DestinationConfig,
		"table":  targetTable,
	}
	addNamespaceParam(p, sm.DBOrSchema)
	if len(sm.KeyFields) > 0 {
		p["key_fields"] = append([]string{}, sm.KeyFields...)
		p["primary_key_fields"] = append([]string{}, sm.KeyFields...)
	}
	if len(sm.ColumnTypes) > 0 {
		ct := make(map[string]string, len(sm.ColumnTypes))
		for k, v := range sm.ColumnTypes {
			ct[k] = v
		}
		p["column_types"] = ct
	}
	return p
}

// apiTargetTable is the destination table writeToDestination would write.
func apiTargetTable(cfg *WorkerConfig, destType string, sm *SinkMessage) string {
	return resolveDestTableForWrite(destType, cfg.DestinationConfig, sm.Table, sm.DBOrSchema)
}

// dlqMessageForRows is msg holding only rows, inline: what goes to the DLQ when an
// API destination wrote the rest of the batch. A claim-check message becomes an
// inline one, so the DLQ copy carries the rows themselves.
func dlqMessageForRows(msg kafka.Message, rows []map[string]interface{}) kafka.Message {
	payload := map[string]interface{}{}
	dec := json.NewDecoder(strings.NewReader(string(msg.Value)))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil || payload == nil {
		payload = map[string]interface{}{}
	}
	payload["data"] = rows
	payload["row_count"] = len(rows)
	payload["storage_type"] = "inline"
	delete(payload, "claim_check_url")
	out := msg
	if b, err := json.Marshal(payload); err == nil {
		out.Value = b
	}
	return out
}

// pgAPIRecords is the key map in api_destination_records (migration 144). Every
// statement retries until it lands or ctx ends, like the ack ledger: a key map the
// sink cannot read or write would make it create a second record for a row.
type pgAPIRecords struct {
	db      *sql.DB
	metrics *Metrics
}

func (p pgAPIRecords) retry(ctx context.Context, label string, f func(context.Context) error) error {
	return retryUntilSuccessOrCancel(ctx, label, p.metrics, f)
}

func jsonText(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (p pgAPIRecords) lookup(ctx context.Context, s apiRecordScope, keys []string, executionID string) (map[string]string, error) {
	ids := map[string]string{}
	if len(keys) == 0 {
		return ids, nil
	}
	err := p.retry(ctx, "api key map lookup", func(c context.Context) error {
		rows, err := p.db.QueryContext(c, `
			UPDATE api_destination_records
			SET last_execution_id = $5, updated_at = NOW()
			WHERE pipeline_id = $1 AND source_table = $2 AND destination_table = $3
			  AND source_key IN (SELECT jsonb_array_elements_text($4::jsonb))
			RETURNING source_key, record_id`,
			s.pipelineID, s.sourceTable, s.destTable, jsonText(keys), executionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var k, id string
			if err := rows.Scan(&k, &id); err != nil {
				return err
			}
			got[k] = id
		}
		if err := rows.Err(); err != nil {
			return err
		}
		ids = got
		return nil
	})
	return ids, err
}

func (p pgAPIRecords) save(ctx context.Context, s apiRecordScope, ids map[string]string, executionID string) error {
	if len(ids) == 0 {
		return nil
	}
	pairs := make([][2]string, 0, len(ids))
	for k, id := range ids {
		pairs = append(pairs, [2]string{k, id})
	}
	return p.retry(ctx, "api key map save", func(c context.Context) error {
		_, err := p.db.ExecContext(c, `
			INSERT INTO api_destination_records
				(pipeline_id, source_table, destination_table, source_key, record_id, last_execution_id, updated_at)
			SELECT $1::uuid, $2, $3, e->>0, e->>1, $5, NOW()
			FROM jsonb_array_elements($4::jsonb) AS e
			ON CONFLICT (pipeline_id, source_table, destination_table, source_key)
			DO UPDATE SET record_id = EXCLUDED.record_id,
			              last_execution_id = EXCLUDED.last_execution_id,
			              updated_at = NOW()`,
			s.pipelineID, s.sourceTable, s.destTable, jsonText(pairs), executionID)
		return err
	})
}

func (p pgAPIRecords) forget(ctx context.Context, s apiRecordScope, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	return p.retry(ctx, "api key map forget", func(c context.Context) error {
		_, err := p.db.ExecContext(c, `
			DELETE FROM api_destination_records
			WHERE pipeline_id = $1 AND source_table = $2 AND destination_table = $3
			  AND source_key IN (SELECT jsonb_array_elements_text($4::jsonb))`,
			s.pipelineID, s.sourceTable, s.destTable, jsonText(keys))
		return err
	})
}

// noAPIRecords is the key map of a sink with no platform database: it maps
// nothing, so every row creates a record.
type noAPIRecords struct{}

func (noAPIRecords) lookup(context.Context, apiRecordScope, []string, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (noAPIRecords) save(context.Context, apiRecordScope, map[string]string, string) error {
	return nil
}
func (noAPIRecords) forget(context.Context, apiRecordScope, []string) error { return nil }
