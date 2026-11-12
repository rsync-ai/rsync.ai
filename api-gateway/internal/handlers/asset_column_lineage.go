package handlers

// Column lineage on the Lineage page: which source column feeds which column of a
// model, and which columns only filter, join or group its rows. Lineage design step L1,
// docs/internal/lineage-catalog-design.md § 3.
//
// llm-service reads each model's SQL with sqlglot (POST /v1/lineage/columns, no model
// call) and the answer is saved in model_column_lineage (migration 152). The asset graph
// reads the saved answers and maps each edge's table onto the table nodes it already
// draws, with the same matchTableReference its reads edges use. A model with no answer
// for its current SQL, or an old one, is read again in the background, one workspace at
// a time, as the lake links are (lake_external_tables.go); the graph says pending and
// the page asks again.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"

	"api-gateway/internal/cache"
	"api-gateway/internal/db"
	"api-gateway/internal/validators"
)

const (
	// A warehouse's columns change without the model changing, so an answer is read
	// again once a day.
	columnLineageMaxAge = 24 * time.Hour
	// An answer read without the warehouse's columns may be partial (a SELECT *, an
	// unqualified column in a join), so it is read again sooner.
	columnLineageNoSchemaRetry = 10 * time.Minute
	columnLineageTimeout       = 3 * time.Minute
	columnLineageBackoff       = 5 * time.Minute
	// llm-service takes at most this many models per call (lineage_routes.MAX_MODELS),
	// and these per model.
	columnLineageBatch      = 100
	columnLineageMaxTables  = 500
	columnLineageMaxColumns = 5000
	columnLineageMaxSQL     = 200_000
	// The most column edges one graph carries; past it the graph says truncated.
	columnLineageGraphEdgeLimit = 20_000

	columnLineageStatusOK          = "ok"
	columnLineageStatusPartial     = "partial"
	columnLineageStatusFailed      = "failed"
	columnLineageStatusUnsupported = "unsupported"
)

// columnLineageEdge is one edge as llm-service answers and model_column_lineage keeps
// it: Table is the source table's name parts as the SQL wrote them.
type columnLineageEdge struct {
	Table    []string `json:"table"`
	Column   string   `json:"column"`
	ToColumn string   `json:"to_column"`
	Type     string   `json:"type"`
	Subtype  string   `json:"subtype"`
}

// savedColumnLineage is one model's answer. Columns, Edges and Truncated are the
// result column; Status and Reason are columns of their own.
type savedColumnLineage struct {
	Status    string              `json:"-"`
	Reason    string              `json:"-"`
	Columns   []string            `json:"columns"`
	Edges     []columnLineageEdge `json:"edges"`
	Truncated bool                `json:"truncated"`
}

// assetColumnLineage is column_lineage on the asset graph response (design § 3.4).
type assetColumnLineage struct {
	// Pending says some model's columns are being read for the first time; the page
	// asks again.
	Pending   bool               `json:"pending"`
	Truncated bool               `json:"truncated"`
	Models    []assetColumnModel `json:"models"`
	Edges     []assetColumnEdge  `json:"edges"`
}

type assetColumnModel struct {
	NodeID    string   `json:"node_id"`
	Status    string   `json:"status"`
	Columns   []string `json:"columns"`
	Reason    string   `json:"reason,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// assetColumnEdge is one column edge on the graph. From is the table node the graph
// draws a reads edge from, or "" when the table is not one the graph knows (or the name
// could be two of them); FromTable keeps the name either way.
type assetColumnEdge struct {
	From       string `json:"from"`
	FromTable  string `json:"from_table"`
	FromColumn string `json:"from_column"`
	To         string `json:"to"`
	ToColumn   string `json:"to_column"`
	Type       string `json:"type"`
	Subtype    string `json:"subtype"`
}

// columnLineageJob is one model to read.
type columnLineageJob struct {
	ModelID       string
	SQL           string
	ConnectorType string
	ConnectionID  string
	Hash          string
}

// columnLineageLoad is what the graph gets: the answers for the models' current SQL,
// and whether a read that will add missing ones is under way.
type columnLineageLoad struct {
	ByModel map[string]savedColumnLineage
	Pending bool
}

// columnLineageSQLHash keys an answer to one version of a model: the connection type
// decides how the SQL is read, so it is part of the key.
func columnLineageSQLHash(connectorType, sqlText string) string {
	sum := sha256.Sum256([]byte(connectorType + "\x00" + sqlText))
	return hex.EncodeToString(sum[:])
}

// columnLineageStale says an answer for the current SQL should be read again. A failed
// answer waits the day like any other: its SQL did not change, and a changed SQL is a
// new hash and read at once.
func columnLineageStale(hadSchema bool, computedAt, now time.Time) bool {
	age := now.Sub(computedAt)
	if !hadSchema && age > columnLineageNoSchemaRetry {
		return true
	}
	return age > columnLineageMaxAge
}

// loadColumnLineage reads the saved answers for the models' current SQL and starts a
// background read for the models that have none or an old one. A model with no SQL is
// left out.
func loadColumnLineage(ctx context.Context, database *sql.DB, workspaceID string, models []modelAsset) (columnLineageLoad, error) {
	out := columnLineageLoad{ByModel: map[string]savedColumnLineage{}}
	want := map[string]string{}
	ids := []string{}
	for _, m := range models {
		if strings.TrimSpace(m.SQLText) == "" {
			continue
		}
		want[m.ID] = columnLineageSQLHash(m.ConnectorType, m.SQLText)
		ids = append(ids, m.ID)
	}
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := database.QueryContext(ctx, `
		SELECT saved_query_id::text, sql_hash, status, reason, result, had_schema, computed_at
		FROM model_column_lineage
		WHERE workspace_id = $1::uuid
		  AND saved_query_id::text = ANY($2)`, workspaceID, pgdriver.StringArray(ids))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	now := time.Now()
	stale := map[string]bool{}
	for rows.Next() {
		var id, hash, status, reason string
		var result []byte
		var hadSchema bool
		var computedAt time.Time
		if err := rows.Scan(&id, &hash, &status, &reason, &result, &hadSchema, &computedAt); err != nil {
			return out, err
		}
		if want[id] != hash {
			continue
		}
		var saved savedColumnLineage
		if err := json.Unmarshal(result, &saved); err != nil {
			// Read again, as if there were no answer.
			log.WithError(err).Warnf("[Lineage] model %s: unreadable column lineage row", id)
			continue
		}
		saved.Status, saved.Reason = status, reason
		out.ByModel[id] = saved
		stale[id] = columnLineageStale(hadSchema, computedAt, now)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	var jobs []columnLineageJob
	for _, m := range models {
		hash, ok := want[m.ID]
		if !ok {
			continue
		}
		if _, have := out.ByModel[m.ID]; have && !stale[m.ID] {
			continue
		}
		jobs = append(jobs, columnLineageJob{
			ModelID: m.ID, SQL: m.SQLText, ConnectorType: m.ConnectorType,
			ConnectionID: m.ConnectionID, Hash: hash,
		})
	}
	if len(jobs) == 0 {
		return out, nil
	}
	if refreshColumnLineage(workspaceID, jobs) {
		// Pending only for a model with nothing to show yet: an old answer is shown
		// while the new one is read.
		for _, j := range jobs {
			if _, have := out.ByModel[j.ModelID]; !have {
				out.Pending = true
				break
			}
		}
	}
	return out, nil
}

var (
	columnLineageRefreshing sync.Map // workspace id → struct{}
	columnLineageFailed     sync.Map // workspace id → time.Time of the last failed read
)

// refreshColumnLineage reads the jobs' columns in the background, one read per
// workspace at a time. It answers whether a read is under way: false while the
// workspace is backing off after a failed read, so the page stops asking. A var so
// tests can stand in for it.
var refreshColumnLineage = func(workspaceID string, jobs []columnLineageJob) bool {
	if at, ok := columnLineageFailed.Load(workspaceID); ok && time.Since(at.(time.Time)) < columnLineageBackoff {
		return false
	}
	if _, busy := columnLineageRefreshing.LoadOrStore(workspaceID, struct{}{}); busy {
		return true
	}
	go func() {
		defer columnLineageRefreshing.Delete(workspaceID)
		// The workspace was membership-checked by the request that asked for the graph;
		// carried so llm-service's log lines name it (stampLLMWorkspace).
		ctx, cancel := context.WithTimeout(withLLMWorkspace(context.Background(), workspaceID), columnLineageTimeout)
		defer cancel()
		if err := computeColumnLineage(ctx, db.GetDB(), workspaceID, jobs); err != nil {
			columnLineageFailed.Store(workspaceID, time.Now())
			log.Warnf("[Lineage] workspace %s: could not read the models' column lineage: %v", workspaceID, err)
			return
		}
		columnLineageFailed.Delete(workspaceID)
	}()
	return true
}

// columnLineageRequestTable and columnLineageRequestModel are llm-service's request
// shapes (lineage_routes.py LineageTable, LineageModel).
type columnLineageRequestTable struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

type columnLineageRequestModel struct {
	ID            string                      `json:"id"`
	SQL           string                      `json:"sql"`
	ConnectorType string                      `json:"connector_type"`
	Tables        []columnLineageRequestTable `json:"tables"`
}

type columnLineageAnswer struct {
	ID        string              `json:"id"`
	Status    string              `json:"status"`
	Columns   []string            `json:"columns"`
	Edges     []columnLineageEdge `json:"edges"`
	Reason    string              `json:"reason"`
	Truncated bool                `json:"truncated"`
}

// columnLineageSent is what was sent for one model, kept to save beside its answer.
type columnLineageSent struct {
	job        columnLineageJob
	schemaHash string
	hadSchema  bool
}

// computeColumnLineage reads the jobs' columns and saves each answer. Each model is
// sent with the columns of the tables its SQL names, from its connection's schema
// index; a connection whose index cannot be read sends its models without, and their
// answers are read again sooner (columnLineageNoSchemaRetry).
func computeColumnLineage(ctx context.Context, database *sql.DB, workspaceID string, jobs []columnLineageJob) error {
	if database == nil {
		return errors.New("database not available")
	}
	indexes := map[string]*cache.ExplorerSchemaIndex{}
	indexRead := map[string]bool{}
	var requests []columnLineageRequestModel
	sent := map[string]columnLineageSent{}
	for _, j := range jobs {
		if len(j.SQL) > columnLineageMaxSQL {
			if err := saveColumnLineage(ctx, database, workspaceID, columnLineageSent{job: j, hadSchema: true}, columnLineageAnswer{
				Status: columnLineageStatusFailed, Reason: "the model's SQL is too long to read for column lineage",
			}); err != nil {
				return err
			}
			continue
		}
		document := isDocumentConnector(j.ConnectorType)
		if _, done := indexRead[j.ConnectionID]; !done && !document && j.ConnectionID != "" {
			idx, err := columnLineageSchemaIndex(ctx, database, workspaceID, j.ConnectionID)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.WithError(err).Warnf("[Lineage] connection %s: could not read its columns for column lineage", j.ConnectionID)
			}
			indexes[j.ConnectionID], indexRead[j.ConnectionID] = idx, err == nil && idx != nil
		}
		var tables []columnLineageRequestTable
		if idx := indexes[j.ConnectionID]; idx != nil && !document {
			tables = columnLineageTablesFor(validators.ExtractTableReferences(j.SQL), idx.Tables)
		}
		requests = append(requests, columnLineageRequestModel{
			ID: j.ModelID, SQL: j.SQL, ConnectorType: columnLineageDialect(j.ConnectorType), Tables: tables,
		})
		sent[j.ModelID] = columnLineageSent{
			job:        j,
			schemaHash: columnLineageSchemaHash(tables),
			// A document model has no SQL to read and needs no columns to say so.
			hadSchema: document || indexRead[j.ConnectionID],
		}
	}

	for start := 0; start < len(requests); start += columnLineageBatch {
		end := min(start+columnLineageBatch, len(requests))
		answers, err := postColumnLineage(ctx, requests[start:end])
		if err != nil {
			return err
		}
		for _, a := range answers {
			s, ok := sent[a.ID]
			if !ok {
				continue
			}
			if err := saveColumnLineage(ctx, database, workspaceID, s, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// columnLineageDialect is the SQL dialect llm-service reads a model in. The connection
// type alone is not enough: azure-postgres and supabase-postgres are PostgreSQL to the
// Explorer and unknown to sqlglot. A type the Explorer runs no SQL for is sent as it
// is, and llm-service answers unsupported.
func columnLineageDialect(connectorType string) string {
	capability := ResolveExplorerCapability(connectorType)
	if capability.QueryLanguage == langSQL && capability.Dialect != "" {
		return capability.Dialect
	}
	return connectorType
}

// columnLineageSchemaIndex is a connection's schema index, from the Explorer's cache or
// read and cached, exactly as GET /explorer/connections/:id/schema-index does. A var so
// tests can stand in for it.
var columnLineageSchemaIndex = func(ctx context.Context, database *sql.DB, workspaceID, connectionID string) (*cache.ExplorerSchemaIndex, error) {
	var connectorType, configEncrypted string
	if err := database.QueryRowContext(ctx, `
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2`, connectionID, workspaceID).Scan(&connectorType, &configEncrypted); err != nil {
		return nil, err
	}
	cacheID := schemaIndexCacheID(connectionID, false)
	if explorerCache != nil {
		if cached, err := explorerCache.GetSchemaIndex(ctx, cacheID); err == nil && cached != nil {
			return cached, nil
		}
	}
	idx, err := buildSchemaIndex(ctx, connectionID, workspaceID, connectorType, configEncrypted, false)
	if err != nil {
		return nil, err
	}
	if explorerCache != nil {
		cacheCopy := *idx
		cacheCopy.ConnectionID = cacheID
		if err := explorerCache.SetSchemaIndex(ctx, &cacheCopy); err != nil {
			log.WithError(err).Warn("[Lineage] could not cache a schema index")
		}
	}
	return idx, nil
}

// columnLineageTablesFor picks the index tables a model's SQL names: the same name, and
// the same schema when the reference names one. Names compare without case, as the
// read matcher's do. Only these tables' columns are sent, and at most the limits
// llm-service takes.
func columnLineageTablesFor(refs []validators.TableRef, tables []cache.ExplorerTableIndex) []columnLineageRequestTable {
	type key struct{ schema, name string }
	picked := map[key]bool{}
	var out []columnLineageRequestTable
	for _, ref := range refs {
		name := strings.ToLower(ref.Name())
		schema := ""
		if len(ref.Parts) >= 2 {
			schema = strings.ToLower(ref.Parts[len(ref.Parts)-2])
		}
		for _, t := range tables {
			if strings.ToLower(t.Name) != name || (schema != "" && strings.ToLower(t.Schema) != schema) {
				continue
			}
			k := key{strings.ToLower(t.Schema), strings.ToLower(t.Name)}
			if picked[k] || len(t.Columns) == 0 {
				continue
			}
			if len(out) == columnLineageMaxTables {
				return out
			}
			picked[k] = true
			cols := make([]string, 0, min(len(t.Columns), columnLineageMaxColumns))
			for _, c := range t.Columns {
				if len(cols) == columnLineageMaxColumns {
					break
				}
				cols = append(cols, c.Name)
			}
			out = append(out, columnLineageRequestTable{Schema: t.Schema, Name: t.Name, Columns: cols})
		}
	}
	return out
}

// columnLineageSchemaHash fingerprints the column lists sent for a model, so a later
// step can tell an answer read against other columns.
func columnLineageSchemaHash(tables []columnLineageRequestTable) string {
	if len(tables) == 0 {
		return ""
	}
	b, _ := json.Marshal(tables)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// postColumnLineage sends one batch to llm-service. The answer must name the models
// sent; an answer about another id is dropped by the caller.
func postColumnLineage(ctx context.Context, models []columnLineageRequestModel) ([]columnLineageAnswer, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("LLM_SERVICE_URL")), "/")
	if base == "" {
		base = "http://llm-service:5000"
	}
	body, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/lineage/columns", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET")); s != "" {
		req.Header.Set("X-Internal-Secret", s)
	}
	stampLLMWorkspace(req)
	resp, err := (&http.Client{Timeout: columnLineageTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm-service answered %d for column lineage", resp.StatusCode)
	}
	var out struct {
		Models []columnLineageAnswer `json:"models"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llm-service column lineage answer: %w", err)
	}
	return out.Models, nil
}

// saveColumnLineage keeps one model's answer for its SQL and deletes the answers for
// its older SQL that no recorded run names: a run's lineage event keeps the columns of
// the version it ran (lineage_events.go), until the retention sweep deletes the event.
// The insert reads the model's row, so an answer for a model deleted, or moved out of
// the workspace, while it was read is not saved.
func saveColumnLineage(ctx context.Context, database *sql.DB, workspaceID string, s columnLineageSent, a columnLineageAnswer) error {
	status := a.Status
	switch status {
	case columnLineageStatusOK, columnLineageStatusPartial, columnLineageStatusFailed, columnLineageStatusUnsupported:
	default:
		status = columnLineageStatusFailed
	}
	edges := a.Edges
	if edges == nil {
		edges = []columnLineageEdge{}
	}
	columns := a.Columns
	if columns == nil {
		columns = []string{}
	}
	result, err := json.Marshal(savedColumnLineage{Columns: columns, Edges: edges, Truncated: a.Truncated})
	if err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_column_lineage
		    (saved_query_id, sql_hash, workspace_id, status, reason, result, schema_hash, had_schema, computed_at)
		SELECT sq.id, $3, sq.workspace_id, $4, $5, $6::jsonb, $7, $8, NOW()
		FROM saved_queries sq
		WHERE sq.id = $1::uuid AND sq.workspace_id = $2::uuid
		ON CONFLICT (saved_query_id, sql_hash) DO UPDATE SET
		    status = EXCLUDED.status, reason = EXCLUDED.reason, result = EXCLUDED.result,
		    schema_hash = EXCLUDED.schema_hash, had_schema = EXCLUDED.had_schema,
		    computed_at = EXCLUDED.computed_at`,
		s.job.ModelID, workspaceID, s.job.Hash, status, a.Reason, string(result), s.schemaHash, s.hadSchema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM model_column_lineage
		WHERE saved_query_id = $1::uuid AND workspace_id = $2::uuid AND sql_hash <> $3
		  AND NOT EXISTS (
		      SELECT 1 FROM lineage_events e
		      WHERE e.saved_query_id = model_column_lineage.saved_query_id
		        AND e.sql_hash = model_column_lineage.sql_hash)`,
		s.job.ModelID, workspaceID, s.job.Hash); err != nil {
		return err
	}
	return tx.Commit()
}

// buildColumnLineage puts the saved answers on the graph. Each edge's table is mapped
// onto the table nodes the graph draws for the model's connection with
// matchTableReference, the rule the reads edges use, minus the model's own table; a
// name that matches no table, or two different ones, keeps From "" and its name.
func buildColumnLineage(in assetGraphInput, byConnection map[string][]producedTable) *assetColumnLineage {
	if len(in.ColumnLineage) == 0 && !in.ColumnLineagePending {
		return nil
	}
	out := &assetColumnLineage{
		Pending: in.ColumnLineagePending,
		Models:  []assetColumnModel{},
		Edges:   []assetColumnEdge{},
	}
	for _, m := range in.Models {
		saved, ok := in.ColumnLineage[m.ID]
		if !ok {
			continue
		}
		modelNodeID := assetKindModel + ":" + m.ID
		columns := saved.Columns
		if columns == nil {
			columns = []string{}
		}
		out.Models = append(out.Models, assetColumnModel{
			NodeID: modelNodeID, Status: saved.Status, Columns: columns,
			Reason: saved.Reason, Truncated: saved.Truncated,
		})
		resolved := map[string]string{}
		for _, e := range saved.Edges {
			if len(out.Edges) == columnLineageGraphEdgeLimit {
				out.Truncated = true
				break
			}
			name := strings.Join(e.Table, ".")
			from, seen := resolved[strings.ToLower(name)]
			if !seen {
				from = columnLineageTableNode(m, e.Table, byConnection)
				resolved[strings.ToLower(name)] = from
			}
			out.Edges = append(out.Edges, assetColumnEdge{
				From: from, FromTable: name, FromColumn: e.Column,
				To: modelNodeID, ToColumn: e.ToColumn, Type: e.Type, Subtype: e.Subtype,
			})
		}
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].NodeID < out.Models[j].NodeID })
	return out
}

// columnLineageTableNode is the one table node a column edge's table names, or "".
func columnLineageTableNode(m modelAsset, table []string, byConnection map[string][]producedTable) string {
	if len(table) == 0 {
		return ""
	}
	matches, _ := matchTableReference(validators.TableRef{Parts: table}, byConnection[m.ConnectionID])
	found := ""
	for _, match := range matches {
		if match.ProducerID == m.ID {
			continue
		}
		nodeID := assetKindTable + ":" + producedTableKey(m.ConnectionID, match)
		if found != "" && found != nodeID {
			return ""
		}
		found = nodeID
	}
	return found
}
