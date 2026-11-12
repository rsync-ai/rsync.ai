package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"api-gateway/internal/validators"
)

// MongoDB models: a saved query on a document connection.
//
// The saved-query row is the same one a SQL model uses. What differs is what sql_text
// holds — not SQL but the pipeline preview's own request, as JSON:
//
//	{"database": "shop", "collection": "orders", "pipeline": [{"$group": …}]}
//
// and how a run reaches the engine: the connector's `materialize` tool, through the
// orchestrator's /agent/explorer-materialize, which appends the one write a model
// makes — a $out to the model's target. The pipeline itself can name no write stage:
// ValidateDocumentAggregate refuses $out and $merge at any depth, here on the way in
// AND on every run, and the connector refuses them again.
//
// Only table mode exists. Statement mode runs SQL for its effect; a pipeline has no
// effect but its result, which is what table mode writes.

// documentModelQuery is the stored shape. Pipeline stays raw so key order (a $sort, a
// compound $group _id) and 64-bit integers survive the round trip.
type documentModelQuery struct {
	Database   string          `json:"database,omitempty"`
	Collection string          `json:"collection"`
	Pipeline   json.RawMessage `json:"pipeline"`
}

// isDocumentConnector reports whether a connection's queries are pipelines rather than
// SQL. Resolved through the Explorer capability table, the same answer the frontend's
// explorer_mode carries, so the two cannot disagree about what a saved query holds.
func isDocumentConnector(connectorType string) bool {
	return ResolveExplorerCapability(connectorType).QueryLanguage == langDocument
}

// modelSupported reports whether a saved query on this connector can be a model: a SQL
// engine modelDialect knows, or a document store whose models are a pipeline.
func modelSupported(connectorType string) bool {
	return modelDialect(connectorType) != "" || isDocumentConnector(connectorType)
}

// parseDocumentModelQuery decodes and validates a stored pipeline. Errors name a path
// or a stage, never a value.
func parseDocumentModelQuery(text string) (*validators.DocumentAggregateSpec, *validators.DocumentFindError) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var q documentModelQuery
	if err := dec.Decode(&q); err != nil {
		return nil, &validators.DocumentFindError{Code: "invalid_pipeline",
			Message: "a saved pipeline must be a JSON object with collection and pipeline, and optionally database"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &validators.DocumentFindError{Code: "invalid_pipeline",
			Message: "a saved pipeline must be a single JSON object"}
	}
	spec, ferr := validators.ValidateDocumentAggregate(validators.DocumentAggregateRequest{
		Database: q.Database, Collection: q.Collection, Pipeline: q.Pipeline,
	})
	if ferr != nil {
		return nil, ferr
	}
	// A model writes every document the pipeline produces; the preview's page size
	// does not apply.
	spec.Limit = 0
	return spec, nil
}

// modelStatementClass is the class a run is checked and recorded as. A pipeline is a
// read: it can name no write stage, and the one write a model makes is the
// connector's own $out to the model's target — the same reasoning that makes a
// table-mode SELECT a read although its run issues CREATE TABLE.
func modelStatementClass(m *savedQueryModel) validators.StatementClass {
	if isDocumentConnector(m.ConnectorType) {
		return validators.ClassRead
	}
	return validators.ClassifyStatementSQL(m.SQLText)
}

// authorizeDocumentModel is authorizeModelRun's document half, reached after the role
// floor. It re-validates the CURRENT pipeline on every run, as the SQL gate does.
func authorizeDocumentModel(m *savedQueryModel) string {
	if isStatementModel(m.Materialization) {
		return "a MongoDB model writes its pipeline's result to a collection; statement mode is for SQL engines — " +
			"write it to a collection instead"
	}
	if _, ferr := parseDocumentModelQuery(m.SQLText); ferr != nil {
		return "the model's pipeline is not one rsync.ai can run: " + documentFindErrorText(ferr)
	}
	return ""
}

// documentFindErrorText renders a validation error with its path.
func documentFindErrorText(ferr *validators.DocumentFindError) string {
	if ferr.Path == "" {
		return ferr.Message
	}
	return ferr.Path + ": " + ferr.Message
}

// documentModelConfig picks the config a run sends and the source database it names.
//
// A connection that names a database is pinned to it, and the databases this
// workspace's pipelines write to on the connection are reached by widening its scope
// (explorerWidenedConfig) — exactly what an Explorer read of them gets. That is how a
// model reads the bronze collections a CDC pipeline delivered. The widened config names
// no database of its own, so the source database is then always sent explicitly.
// Anything outside that reach is left pinned, for the connector to refuse.
func documentModelConfig(ctx context.Context, m *savedQueryModel, config map[string]interface{}, sourceDB, targetDB string) (map[string]interface{}, string) {
	pinned := pinnedDocumentDatabase(m.ConnectorType, config)
	if pinned == "" {
		return config, sourceDB
	}
	if (sourceDB == "" || sourceDB == pinned) && (targetDB == "" || targetDB == pinned) {
		return config, sourceDB
	}
	widened, _ := explorerWidenedConfig(ctx, m.WorkspaceID, m.ConnectionID, m.ConnectorType, config)
	if widened == nil {
		return config, sourceDB
	}
	if sourceDB == "" {
		sourceDB = pinned
	}
	return widened, sourceDB
}

// documentModelMaxResponseBytes bounds the materialize reply, which is a few fields.
const documentModelMaxResponseBytes = 1 << 20

// runDocumentModel runs one MongoDB model through the connector's materialize tool.
// targetDatabase/targetCollection are validateModelTarget's schema/table: a target is
// `collection` (the pipeline's own database) or `database.collection`.
func runDocumentModel(ctx context.Context, m *savedQueryModel, config map[string]interface{}, targetDatabase, targetCollection string) (*int64, error) {
	spec, ferr := parseDocumentModelQuery(m.SQLText)
	if ferr != nil {
		// authorizeModelRun validated this same text under the run lock.
		return nil, errors.New(documentFindErrorText(ferr))
	}
	cfg, sourceDB := documentModelConfig(ctx, m, config, spec.Database, targetDatabase)
	target := targetCollection
	if targetDatabase != "" {
		target = targetDatabase + "." + targetCollection
	}
	payload := map[string]interface{}{
		"connector_type": m.ConnectorType,
		"connection_id":  m.ConnectionID,
		"config":         cfg,
		"collection":     spec.Collection,
		"pipeline":       spec.Pipeline,
		"target":         target,
		// target_owned: until a run has proved this model created the target, the
		// connector builds aside and renames without dropTarget, so MongoDB itself
		// refuses to take over a collection rsync.ai did not create.
		"replace":         m.TargetOwned,
		"timeout_seconds": modelStatementTimeoutSeconds(ctx),
	}
	if sourceDB != "" {
		payload["database"] = sourceDB
	}

	body, err := postDocumentMaterialize(ctx, payload, orchestratorModelClientTimeout(ctx))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		RowsAffected *int64 `json:"rows_affected"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("explorer_materialize: decode response: %w", err)
	}
	return envelope.RowsAffected, nil
}

// postDocumentMaterialize sends one model run to the orchestrator and returns the
// response body, or the connector's own message as the error. Sent once: the caller
// owns retries, and a timed-out run is not repeated behind its back.
func postDocumentMaterialize(ctx context.Context, payload map[string]interface{}, timeout time.Duration) ([]byte, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("explorer_materialize: build request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(orchestratorBaseURL(), "/")+"/api/v1/agent/explorer-materialize", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("explorer_materialize: build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setInternalServiceSecret(httpReq)

	resp, err := (&http.Client{Timeout: timeout}).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("explorer_materialize: orchestrator unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, documentModelMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("explorer_materialize: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var env struct {
			Error   string `json:"error"`
			Details string `json:"details"`
		}
		_ = json.Unmarshal(body, &env)
		msg := strings.TrimSpace(env.Details)
		if msg == "" {
			msg = strings.TrimSpace(env.Error)
		}
		if msg == "" {
			msg = fmt.Sprintf("orchestrator returned %d", resp.StatusCode)
		}
		return nil, errors.New(msg)
	}
	return body, nil
}
