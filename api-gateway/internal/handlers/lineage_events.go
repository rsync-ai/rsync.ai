package handlers

// Lineage history, design step L2 (docs/internal/lineage-catalog-design.md § 4): one
// lineage_events row per run, naming the tables it read and wrote, so the Lineage page
// can say what ran against a table and when, and the OpenLineage export (L3,
// openlineage*.go) has the runs to send.
//
// A background recorder reads the runs that already exist rather than hooking each
// place a run ends. A model run is a saved_query_runs row; a batch pipeline run is an
// execution that ended. Reading them means a run that ended while the gateway was down
// is still recorded when it comes back (within the look-back), and the unique (job,
// run_ref) index makes recording one twice a no-op, so two gateways, or a slow tick
// overlapping the next, record each run once. A CDC pipeline never ends, so its run is
// the trailing-edge window (cdc_trailing_edge.go), recorded by recordingCDCFire after
// each fire.
//
// Names only, as the rest of the page: never a row, a row count, the SQL or an error
// (design § 4.1).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

const (
	lineageRecorderTick       = time.Minute
	lineageRecorderStartDelay = 30 * time.Second
	// A run that ended longer ago than this when the recorder first reads it (the
	// gateway was down for days) is not recorded.
	lineageRecorderLookback = 48 * time.Hour
	// An execution's status can still change after it ends (the post-run checks turn a
	// completed run into silent_drop_detected), so one is recorded this long after.
	lineageExecutionSettle = 5 * time.Minute
	lineageRecorderBatch   = 500
	lineageRetentionEvery  = 24 * time.Hour
	lineageRetentionBatch  = 1000

	// LineageEventRetentionDaysEnv is how long a run's event is kept.
	LineageEventRetentionDaysEnv     = "LINEAGE_EVENT_RETENTION_DAYS"
	defaultLineageEventRetentionDays = 365

	lineageJobModel    = "model"
	lineageJobPipeline = "pipeline"

	lineageEventComplete = "COMPLETE"
	lineageEventFail     = "FAIL"
	lineageEventAbort    = "ABORT"

	// lineageKeySep joins a table key's parts. Not "\x00", which the graph's node ids
	// use: Postgres text cannot hold it.
	lineageKeySep = "\x1f"
)

// lineageDataset is one table a run read or wrote, as lineage_events keeps it.
type lineageDataset struct {
	ConnectionID string `json:"connection_id"`
	Name         string `json:"name"`
	// Captured is a table a pipeline wrote under no destination name: Name is the
	// source-side name it was captured under, not its name where it landed.
	Captured bool `json:"captured,omitempty"`
}

// lineageEvent is one run, as the recorder writes it.
type lineageEvent struct {
	WorkspaceID  string
	JobKind      string
	SavedQueryID string
	PipelineID   string
	RunRef       string
	EventType    string
	StartedAt    sql.NullTime
	EndedAt      time.Time
	Inputs       []lineageDataset
	Outputs      []lineageDataset
	// TableKeys are what a table node's history finds the run by (lineageTableKey).
	TableKeys []string
	SQLHash   string
}

// lineageTableKey is the key a table node's history looks a run up by: the graph's
// table key (producedTableKey) with lineageKeySep for the separator.
func lineageTableKey(connectionID, name string) string {
	return connectionID + lineageKeySep + strings.ToLower(name)
}

// lineageCapturedKey is the key of a table a pipeline wrote under no destination name,
// the graph's captured node (buildAssetGraph registerUnplaced).
func lineageCapturedKey(connectionID, name string) string {
	return connectionID + lineageKeySep + "captured" + lineageKeySep + strings.ToLower(name)
}

// newLineageEvent starts an event with empty, not nil, lists: the columns are NOT NULL
// and the JSON is read back as lists.
func newLineageEvent(workspaceID, kind, jobID, runRef, eventType string, startedAt sql.NullTime, endedAt time.Time) lineageEvent {
	ev := lineageEvent{
		WorkspaceID: workspaceID, JobKind: kind, RunRef: runRef, EventType: eventType,
		StartedAt: startedAt, EndedAt: endedAt,
		Inputs: []lineageDataset{}, Outputs: []lineageDataset{}, TableKeys: []string{},
	}
	if kind == lineageJobModel {
		ev.SavedQueryID = jobID
	} else {
		ev.PipelineID = jobID
	}
	return ev
}

// finish drops repeats, keeping the first of each, and sorts the keys.
func (ev *lineageEvent) finish() {
	ev.Inputs = uniqueLineageDatasets(ev.Inputs)
	ev.Outputs = uniqueLineageDatasets(ev.Outputs)
	seen := map[string]bool{}
	keys := []string{}
	for _, k := range ev.TableKeys {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	ev.TableKeys = keys
}

func uniqueLineageDatasets(in []lineageDataset) []lineageDataset {
	seen := map[lineageDataset]bool{}
	out := []lineageDataset{}
	for _, d := range in {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// modelRunRow is one saved_query_runs row with its model, as the recorder reads it.
type modelRunRow struct {
	RunID       string
	ModelID     string
	WorkspaceID string
	Status      string
	StartedAt   time.Time
	FinishedAt  time.Time
	// RunTarget is the table the run recorded writing, "" when it recorded none.
	RunTarget       string
	Name            string
	SQLText         string
	Materialization string
	TargetTable     string
	ConnectionID    string
	ConnectorType   string
	// SQLKnown says the model was not edited after the run started, so its SQL now is
	// the SQL that ran. A run does not touch the model's updated_at (migration 119).
	SQLKnown bool
}

// lineageEventNamesTables says whether a run of this kind that ended this way read and
// wrote tables. A model run that was skipped ran nothing, so it did neither: the run is
// kept (it is a fact about the model), the tables are not. A pipeline run that was
// cancelled may have moved some before it stopped. Every place that writes or reads an
// event's datasets asks this, so the two cannot disagree.
func lineageEventNamesTables(jobKind, eventType string) bool {
	return !(jobKind == lineageJobModel && eventType == lineageEventAbort)
}

// modelRunEventType maps saved_query_runs.status (succeeded, failed, skipped).
func modelRunEventType(status string) string {
	switch status {
	case "succeeded":
		return lineageEventComplete
	case "skipped":
		return lineageEventAbort
	default:
		return lineageEventFail
	}
}

// modelRunEvent is a model run's event. Its tables come from the model's SQL by the
// graph's own rules (modelReadReferences, modelProducedTables), and only when that SQL
// is the SQL that ran; otherwise it names only the table the run recorded writing. A
// skipped run names none (lineageEventNamesTables) but keeps the SQL version: an event
// that names the version keeps its column lineage (saveColumnLineage).
func modelRunEvent(r modelRunRow) lineageEvent {
	ev := newLineageEvent(r.WorkspaceID, lineageJobModel, r.ModelID, r.RunID, modelRunEventType(r.Status),
		sql.NullTime{Time: r.StartedAt, Valid: true}, r.FinishedAt)
	sqlKnown := r.SQLKnown && strings.TrimSpace(r.SQLText) != ""
	if sqlKnown {
		ev.SQLHash = columnLineageSQLHash(r.ConnectorType, r.SQLText)
	}
	if !lineageEventNamesTables(ev.JobKind, ev.EventType) {
		return ev
	}
	conn := r.ConnectionID
	var produced []producedTable
	if sqlKnown {
		if conn != "" {
			for _, ref := range modelReadReferences(r.ConnectorType, r.SQLText) {
				ev.Inputs = append(ev.Inputs, lineageDataset{ConnectionID: conn, Name: strings.ToLower(ref.Qualified())})
				// The graph matches a read by its last two parts (matchTableReference).
				ev.TableKeys = append(ev.TableKeys, lineageTableKey(conn, ref.SchemaQualified()))
			}
		}
		target := r.TargetTable
		if r.RunTarget != "" {
			target = r.RunTarget
		}
		produced = modelProducedTables(r.ConnectorType, r.ModelID, r.Name, r.Materialization, target, r.SQLText)
	} else if t := splitModelTarget(r.RunTarget); t.TableName != "" {
		produced = []producedTable{t}
	}
	if conn != "" {
		for _, p := range produced {
			name := p.DestQualified
			if name == "" {
				name = p.TableName
			}
			ev.Outputs = append(ev.Outputs, lineageDataset{ConnectionID: conn, Name: strings.ToLower(name)})
			ev.TableKeys = append(ev.TableKeys, lineageTableKey(conn, name))
		}
	}
	ev.finish()
	return ev
}

// pipelineRunRow is one ended batch execution, or one CDC window, with its pipeline's
// connections.
type pipelineRunRow struct {
	RunRef             string
	PipelineID         string
	WorkspaceID        string
	EventType          string
	StartedAt          sql.NullTime
	EndedAt            time.Time
	SourceConnectionID string
	DestConnectionID   string
}

// pipelineRunTable is one table a pipeline run moved: Qualified is its source-side name
// (pipeline_run_table_stats.qualified_name, cdc_table_watermarks.table_name), and
// DestQualified its name where it landed, "" when the run recorded none.
type pipelineRunTable struct {
	Qualified     string
	TableName     string
	DestQualified string
}

// executionEventType maps a terminal executions.status.
func executionEventType(status string) string {
	switch strings.ToLower(status) {
	case "completed", "success":
		return lineageEventComplete
	case "cancelled":
		return lineageEventAbort
	default:
		return lineageEventFail
	}
}

// pipelineRunEvent is a pipeline run's event. A table it landed under no destination
// name is the graph's captured table, keyed as the graph draws it. Its source tables
// have no node on the graph, so no key.
func pipelineRunEvent(r pipelineRunRow, tables []pipelineRunTable) lineageEvent {
	ev := newLineageEvent(r.WorkspaceID, lineageJobPipeline, r.PipelineID, r.RunRef, r.EventType, r.StartedAt, r.EndedAt)
	for _, t := range tables {
		src := t.Qualified
		if src == "" {
			src = t.TableName
		}
		if src != "" && r.SourceConnectionID != "" {
			ev.Inputs = append(ev.Inputs, lineageDataset{ConnectionID: r.SourceConnectionID, Name: strings.ToLower(src)})
		}
		if r.DestConnectionID == "" {
			continue
		}
		switch {
		case t.DestQualified != "":
			ev.Outputs = append(ev.Outputs, lineageDataset{ConnectionID: r.DestConnectionID, Name: strings.ToLower(t.DestQualified)})
			ev.TableKeys = append(ev.TableKeys, lineageTableKey(r.DestConnectionID, t.DestQualified))
		case src != "":
			ev.Outputs = append(ev.Outputs, lineageDataset{ConnectionID: r.DestConnectionID, Name: strings.ToLower(src), Captured: true})
			ev.TableKeys = append(ev.TableKeys, lineageCapturedKey(r.DestConnectionID, src))
		}
	}
	ev.finish()
	return ev
}

const insertLineageEventSQL = `
	INSERT INTO lineage_events
	    (workspace_id, job_kind, saved_query_id, pipeline_id, run_ref, event_type,
	     started_at, ended_at, inputs, outputs, table_keys, sql_hash)
	VALUES ($1::uuid, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6,
	        $7, $8, $9::jsonb, $10::jsonb, $11::text[], $12)
	ON CONFLICT DO NOTHING
	RETURNING id::text`

// insertLineageEvent records ev and returns its id, or "" when the run was already
// recorded.
func insertLineageEvent(ctx context.Context, database *sql.DB, ev lineageEvent) (string, error) {
	inputs, err := json.Marshal(ev.Inputs)
	if err != nil {
		return "", err
	}
	outputs, err := json.Marshal(ev.Outputs)
	if err != nil {
		return "", err
	}
	var id string
	err = database.QueryRowContext(ctx, insertLineageEventSQL,
		ev.WorkspaceID, ev.JobKind, ev.SavedQueryID, ev.PipelineID, ev.RunRef, ev.EventType,
		ev.StartedAt, ev.EndedAt, string(inputs), string(outputs), pgdriver.StringArray(ev.TableKeys), ev.SQLHash,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// lineagePushHook hands newly recorded events to the OpenLineage push, nil when the
// export is not registered (RegisterOpenLineage). It must not block.
var lineagePushHook func(eventIDs []string)

func pushLineageEvents(ids []string) {
	if len(ids) > 0 && lineagePushHook != nil {
		lineagePushHook(ids)
	}
}

// recordModelRunsSQL reads the model runs that ended in the look-back ($1 seconds) and
// have no event, oldest first, at most $2.
const recordModelRunsSQL = `
	SELECT r.run_id::text, sq.id::text, sq.workspace_id::text, r.status,
	       r.started_at, r.finished_at, COALESCE(r.target_table, ''),
	       COALESCE(sq.name, ''), COALESCE(sq.sql_text, ''), COALESCE(sq.materialization, ''),
	       COALESCE(sq.target_table, ''), COALESCE(sq.connection_id::text, ''),
	       COALESCE(cn.connector_type, ''),
	       sq.updated_at <= r.started_at
	FROM saved_query_runs r
	JOIN saved_queries sq ON sq.id = r.saved_query_id
	LEFT JOIN connections cn ON cn.id = sq.connection_id
	WHERE r.finished_at >= now() - make_interval(secs => $1)
	  AND NOT EXISTS (
	      SELECT 1 FROM lineage_events e
	      WHERE e.saved_query_id = r.saved_query_id AND e.run_ref = r.run_id::text)
	ORDER BY r.finished_at, r.run_id
	LIMIT $2`

// recordModelRuns records the model runs not yet recorded and returns the new events'
// ids. The models those runs used get their column lineage read if it has not been
// (loadColumnLineage), so the version a run used has its columns.
func recordModelRuns(ctx context.Context, database *sql.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, recordModelRunsSQL, int64(lineageRecorderLookback/time.Second), lineageRecorderBatch)
	if err != nil {
		return nil, err
	}
	var runs []modelRunRow
	for rows.Next() {
		var r modelRunRow
		if err := rows.Scan(&r.RunID, &r.ModelID, &r.WorkspaceID, &r.Status, &r.StartedAt, &r.FinishedAt, &r.RunTarget,
			&r.Name, &r.SQLText, &r.Materialization, &r.TargetTable, &r.ConnectionID, &r.ConnectorType, &r.SQLKnown); err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var ids []string
	models := map[string]map[string]modelAsset{} // workspace → model id → model
	for _, r := range runs {
		id, err := insertLineageEvent(ctx, database, modelRunEvent(r))
		if err != nil {
			log.WithError(err).WithField("saved_query_id", r.ModelID).Warn("[Lineage] could not record a model run")
			continue
		}
		if id != "" {
			ids = append(ids, id)
		}
		if r.SQLKnown {
			if models[r.WorkspaceID] == nil {
				models[r.WorkspaceID] = map[string]modelAsset{}
			}
			models[r.WorkspaceID][r.ModelID] = modelAsset{
				ID: r.ModelID, Name: r.Name, SQLText: r.SQLText, ConnectionID: r.ConnectionID,
				ConnectorType: r.ConnectorType, Materialization: r.Materialization, TargetTable: r.TargetTable,
			}
		}
	}
	for workspaceID, byID := range models {
		list := make([]modelAsset, 0, len(byID))
		for _, m := range byID {
			list = append(list, m)
		}
		if _, err := loadColumnLineage(ctx, database, workspaceID, list); err != nil {
			log.WithError(err).WithField("workspace_id", workspaceID).Warn("[Lineage] could not check the recorded models' column lineage")
		}
	}
	return ids, nil
}

// recordExecutionsSQL reads the batch executions that ended in the look-back ($1
// seconds), at least the settle time ago ($2 seconds), and have no event, oldest first,
// at most $3. A CDC pipeline's execution is its backfill handing over to streaming,
// not a run; its runs are its windows.
var recordExecutionsSQL = `
	SELECT e.id::text, p.id::text, p.workspace_id::text, e.status,
	       e.start_time, COALESCE(e.end_time, e.start_time),
	       COALESCE(p.source_connection_id::text, ''), COALESCE(p.destination_connection_id::text, '')
	FROM executions e
	JOIN pipelines p ON p.id = e.pipeline_id
	WHERE e.start_time >= now() - make_interval(secs => $1)
	  AND e.status IN ('completed', 'success', 'cancelled', 'failed', 'error',
	                   'silent_drop_detected', 'silent_partial_drop_detected',
	                   'credential_check_failed')
	  AND COALESCE(e.end_time, e.start_time) <= now() - make_interval(secs => $2)
	  AND p.workspace_id IS NOT NULL
	  AND NOT ` + pipelineRowIsCDCSQL + `
	  AND NOT EXISTS (
	      SELECT 1 FROM lineage_events le
	      WHERE le.pipeline_id = e.pipeline_id AND le.run_ref = e.id::text)
	ORDER BY e.start_time, e.id
	LIMIT $3`

// recordPipelineRuns records the ended batch executions not yet recorded, with the
// tables each one's stats rows name, and returns the new events' ids.
func recordPipelineRuns(ctx context.Context, database *sql.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, recordExecutionsSQL,
		int64(lineageRecorderLookback/time.Second), int64(lineageExecutionSettle/time.Second), lineageRecorderBatch)
	if err != nil {
		return nil, err
	}
	var runs []pipelineRunRow
	var execIDs []string
	for rows.Next() {
		var r pipelineRunRow
		var status string
		if err := rows.Scan(&r.RunRef, &r.PipelineID, &r.WorkspaceID, &status, &r.StartedAt, &r.EndedAt,
			&r.SourceConnectionID, &r.DestConnectionID); err != nil {
			rows.Close()
			return nil, err
		}
		r.EventType = executionEventType(status)
		runs = append(runs, r)
		execIDs = append(execIDs, r.RunRef)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(runs) == 0 {
		return nil, nil
	}

	tables := map[string][]pipelineRunTable{}
	trows, err := database.QueryContext(ctx, `
		SELECT execution_id::text, COALESCE(qualified_name, ''), COALESCE(table_name, ''),
		       COALESCE(destination_qualified_name, '')
		FROM pipeline_run_table_stats
		WHERE execution_id = ANY($1::uuid[])
		ORDER BY 1, 2, 3`, pgdriver.StringArray(execIDs))
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var execID string
		var t pipelineRunTable
		if err := trows.Scan(&execID, &t.Qualified, &t.TableName, &t.DestQualified); err != nil {
			trows.Close()
			return nil, err
		}
		tables[execID] = append(tables[execID], t)
	}
	if err := trows.Err(); err != nil {
		trows.Close()
		return nil, err
	}
	trows.Close()

	var ids []string
	for _, r := range runs {
		id, err := insertLineageEvent(ctx, database, pipelineRunEvent(r, tables[r.RunRef]))
		if err != nil {
			log.WithError(err).WithField("pipeline_id", r.PipelineID).Warn("[Lineage] could not record a pipeline run")
			continue
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// parseCDCTrailingEdgeOccurrence reads back the window a trailing-edge occurrence names
// (cdcTrailingEdgeOccurrence): the boundary it fired at and the window's length.
func parseCDCTrailingEdgeOccurrence(occurrence string) (time.Time, time.Duration, bool) {
	rest, ok := strings.CutPrefix(occurrence, "cdc-trail-")
	if !ok {
		return time.Time{}, 0, false
	}
	mins, idx, ok := strings.Cut(rest, "m-")
	if !ok {
		return time.Time{}, 0, false
	}
	m, err1 := strconv.ParseInt(mins, 10, 64)
	n, err2 := strconv.ParseInt(idx, 10, 64)
	if err1 != nil || err2 != nil || m <= 0 {
		return time.Time{}, 0, false
	}
	window := time.Duration(m) * time.Minute
	boundary := time.Unix(n*m*60, 0).UTC()
	if cdcTrailingEdgeOccurrence(window, boundary) != occurrence {
		return time.Time{}, 0, false
	}
	return boundary, window, true
}

// recordingCDCFire records a CDC window's event after fire, whatever fire answers: the
// rows landed either way, and a fire the next sweep repeats records the same occurrence,
// which the unique index drops. Recording runs before the sweep writes the fire's state,
// so the state row still holds the previous fire: its window_end starts this window and
// its landed_through is where this window's landings begin.
func recordingCDCFire(fire cdcTrailingEdgeFire) cdcTrailingEdgeFire {
	return func(ctx context.Context, database *sql.DB, pipelineID, occurrence string) error {
		err := fire(ctx, database, pipelineID, occurrence)
		id, rerr := recordCDCWindow(ctx, database, pipelineID, occurrence)
		switch {
		case rerr != nil && !isUndefinedTable(rerr):
			log.WithError(rerr).WithFields(log.Fields{"pipeline_id": pipelineID, "occurrence": occurrence}).
				Warn("[Lineage] could not record a CDC window")
		case id != "":
			pushLineageEvents([]string{id})
		}
		return err
	}
}

// recordCDCWindow records one CDC window: the tables that landed since the previous
// fire covered, ended at the boundary. It returns the new event's id, "" when the window
// was already recorded or names no pipeline.
func recordCDCWindow(ctx context.Context, database *sql.DB, pipelineID, occurrence string) (string, error) {
	boundary, window, ok := parseCDCTrailingEdgeOccurrence(occurrence)
	if !ok {
		return "", fmt.Errorf("not a trailing-edge occurrence: %q", occurrence)
	}
	r := pipelineRunRow{RunRef: occurrence, PipelineID: pipelineID, EventType: lineageEventComplete, EndedAt: boundary}
	var windowEnd, landedThrough sql.NullTime
	err := database.QueryRowContext(ctx, `
		SELECT p.workspace_id::text,
		       COALESCE(p.source_connection_id::text, ''), COALESCE(p.destination_connection_id::text, ''),
		       s.window_end, s.landed_through
		FROM pipelines p
		LEFT JOIN cdc_trailing_edge_state s ON s.pipeline_id = p.id
		WHERE p.id = $1::uuid AND p.workspace_id IS NOT NULL`, pipelineID,
	).Scan(&r.WorkspaceID, &r.SourceConnectionID, &r.DestConnectionID, &windowEnd, &landedThrough)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	r.StartedAt = sql.NullTime{Time: boundary.Add(-window), Valid: true}
	if windowEnd.Valid && windowEnd.Time.Before(boundary) {
		r.StartedAt = windowEnd
	}

	rows, err := database.QueryContext(ctx, `
		SELECT table_name, COALESCE(destination_qualified_name, '')
		FROM cdc_table_watermarks
		WHERE pipeline_id = $1::uuid
		  AND landed_at > COALESCE($2::timestamptz, '-infinity'::timestamptz)
		ORDER BY table_name`, pipelineID, landedThrough)
	if err != nil {
		return "", err
	}
	var tables []pipelineRunTable
	for rows.Next() {
		var t pipelineRunTable
		if err := rows.Scan(&t.Qualified, &t.DestQualified); err != nil {
			rows.Close()
			return "", err
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	return insertLineageEvent(ctx, database, pipelineRunEvent(r, tables))
}

// lineageEventRetentionDays reads LINEAGE_EVENT_RETENTION_DAYS; anything not a positive
// whole number keeps the default.
func lineageEventRetentionDays() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(LineageEventRetentionDaysEnv))); err == nil && v > 0 {
		return v
	}
	return defaultLineageEventRetentionDays
}

// sweepLineageRetention deletes the events older than days, in batches, then the column
// lineage of model versions no event names any more and the model no longer runs.
func sweepLineageRetention(ctx context.Context, database *sql.DB, days int) (events, versions int64, err error) {
	for {
		res, err := database.ExecContext(ctx, `
			DELETE FROM lineage_events
			WHERE id IN (
			    SELECT id FROM lineage_events
			    WHERE ended_at < now() - make_interval(days => $1)
			    LIMIT $2)`, days, lineageRetentionBatch)
		if err != nil {
			return events, versions, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return events, versions, err
		}
		events += n
		if n < lineageRetentionBatch {
			break
		}
	}
	versions, err = sweepUnnamedColumnLineage(ctx, database)
	return events, versions, err
}

// sweepUnnamedColumnLineage deletes each model's column lineage rows for SQL that is
// not its current SQL and that no event names: the versions saveColumnLineage kept for
// an event the retention sweep has since deleted. The current version's hash is worked
// out here, as the graph does (columnLineageSQLHash), rather than in SQL.
func sweepUnnamedColumnLineage(ctx context.Context, database *sql.DB) (int64, error) {
	var deleted int64
	afterModel, afterHash := "00000000-0000-0000-0000-000000000000", ""
	for {
		rows, err := database.QueryContext(ctx, `
			SELECT m.saved_query_id::text, m.sql_hash, COALESCE(sq.sql_text, ''), COALESCE(cn.connector_type, '')
			FROM model_column_lineage m
			JOIN saved_queries sq ON sq.id = m.saved_query_id
			LEFT JOIN connections cn ON cn.id = sq.connection_id
			WHERE (m.saved_query_id, m.sql_hash) > ($1::uuid, $2)
			  AND NOT EXISTS (
			      SELECT 1 FROM lineage_events e
			      WHERE e.saved_query_id = m.saved_query_id AND e.sql_hash = m.sql_hash)
			ORDER BY m.saved_query_id, m.sql_hash
			LIMIT $3`, afterModel, afterHash, lineageRetentionBatch)
		if err != nil {
			return deleted, err
		}
		old := map[string][]string{} // model id → hashes that are not its current SQL
		n := 0
		for rows.Next() {
			var id, hash, sqlText, connectorType string
			if err := rows.Scan(&id, &hash, &sqlText, &connectorType); err != nil {
				rows.Close()
				return deleted, err
			}
			n++
			afterModel, afterHash = id, hash
			if hash != columnLineageSQLHash(connectorType, sqlText) {
				old[id] = append(old[id], hash)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return deleted, err
		}
		rows.Close()
		for id, hashes := range old {
			res, err := database.ExecContext(ctx, `
				DELETE FROM model_column_lineage m
				WHERE m.saved_query_id = $1::uuid AND m.sql_hash = ANY($2::text[])
				  AND NOT EXISTS (
				      SELECT 1 FROM lineage_events e
				      WHERE e.saved_query_id = m.saved_query_id AND e.sql_hash = m.sql_hash)`,
				id, pgdriver.StringArray(hashes))
			if err != nil {
				return deleted, err
			}
			if k, err := res.RowsAffected(); err == nil {
				deleted += k
			}
		}
		if n < lineageRetentionBatch {
			return deleted, nil
		}
	}
}

// recordLineageTick records the runs that ended since the last tick and hands the new
// events to the push.
func recordLineageTick(ctx context.Context, database *sql.DB) {
	var ids []string
	for _, step := range []struct {
		what   string
		record func(context.Context, *sql.DB) ([]string, error)
	}{
		{"model runs", recordModelRuns},
		{"pipeline runs", recordPipelineRuns},
	} {
		got, err := step.record(ctx, database)
		if err != nil {
			if !isUndefinedTable(err) && ctx.Err() == nil {
				log.WithError(err).Warnf("[Lineage] could not record %s", step.what)
			}
			continue
		}
		ids = append(ids, got...)
	}
	pushLineageEvents(ids)
}

// StartLineageRecorder records each run's lineage event every minute and deletes the
// events past LINEAGE_EVENT_RETENTION_DAYS once a day, until ctx ends.
func StartLineageRecorder(ctx context.Context, database *sql.DB) {
	if database == nil {
		return
	}
	days := lineageEventRetentionDays()
	log.Infof("[Lineage] recording run history every %s, kept %d days", lineageRecorderTick, days)
	go func() {
		timer := time.NewTimer(lineageRecorderStartDelay)
		defer timer.Stop()
		var swept time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			recordLineageTick(ctx, database)
			if time.Since(swept) >= lineageRetentionEvery {
				events, versions, err := sweepLineageRetention(ctx, database, days)
				switch {
				case err != nil && !isUndefinedTable(err) && ctx.Err() == nil:
					log.WithError(err).Warn("[Lineage] retention sweep failed; retried tomorrow")
				case events > 0 || versions > 0:
					log.Infof("[Lineage] retention: deleted %d events and %d unused column lineage versions", events, versions)
				}
				swept = time.Now()
			}
			timer.Reset(lineageRecorderTick)
		}
	}()
}
