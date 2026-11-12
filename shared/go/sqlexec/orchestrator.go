package sqlexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/mask"
)

// ExplorerQuery runs an Explorer statement through the orchestrator's
// /api/v1/agent/explorer-query endpoint, which invokes the connector's MCP tool.
// Used for delegated engines (data warehouses such as BigQuery) the gateway has no
// native driver for. When isWrite is false the orchestrator runs the connector's
// `export` (rows-returning) tool; when true it runs `execute` (no-rows write) and
// returns an affected-row count. The SQL has ALREADY passed the gateway's role-aware
// statement guard (validators.ValidateExplorerStatement in ExecuteExplorerQuery) and
// the connection was loaded workspace-scoped before this is called — the delegated
// path adds no new trust boundary beyond the existing direct-driver path.
func (o Orchestrator) ExplorerQuery(ctx context.Context, connectorType string, config map[string]interface{}, sqlQuery string, limit int, redact bool, isWrite bool) (*Result, error) {
	// Delegated warehouse queries can be slower than a direct-driver preview; give the
	// connector's MCP export (30s ceiling) headroom without hanging the UI.
	start := time.Now()
	body, err := o.PostExplorerQuery(ctx, map[string]interface{}{
		"connector_type": connectorType,
		"config":         config,
		"query":          sqlQuery,
		"limit":          limit,
		// write=true tells the orchestrator to run the connector's MCP `execute`
		// (no-rows) tool instead of `export`. Older orchestrators ignore the field and
		// keep read behavior — writes then surface a clear "not supported" error.
		"write": isWrite,
	}, 60*time.Second)
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Rows         []map[string]interface{} `json:"rows"`
		Columns      []string                 `json:"columns"`
		RowCount     int                      `json:"row_count"`
		RowsAffected *int64                   `json:"rows_affected"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("explorer_query: decode response: %w", err)
	}
	// The round trip through the orchestrator and the connector is the statement's time.
	took := time.Since(start).Milliseconds()

	// Write path: the orchestrator ran the connector's `execute` tool. Return the
	// affected-row count (no rows/columns to scan or redact).
	if isWrite {
		return &Result{
			Columns:         []string{},
			Rows:            []map[string]interface{}{},
			RowsAffected:    envelope.RowsAffected,
			ExecutionTimeMs: took,
		}, nil
	}

	// Backfill columns from the first row if the connector didn't emit them. A row is a
	// map, so its keys are sorted: one order every run (KI-DELEGATED-QUERY-COLUMN-ORDER).
	if len(envelope.Columns) == 0 && len(envelope.Rows) > 0 {
		for k := range envelope.Rows[0] {
			envelope.Columns = append(envelope.Columns, k)
		}
		sort.Strings(envelope.Columns)
	}

	// Apply the same PII-by-column-name redaction the direct-driver preview path uses
	// (executeXQueryWithRedact) so delegated warehouse previews are consistent. Export
	// callers pass redact=false — the data owner sees raw values (T2-7).
	if redact {
		for _, row := range envelope.Rows {
			for col, val := range row {
				if mask.ShouldRedactColumnName(col) {
					row[col] = mask.RedactForPreview(val)
				}
			}
		}
	}

	return &Result{
		Columns:         envelope.Columns,
		Rows:            envelope.Rows,
		RowCount:        envelope.RowCount,
		ExecutionTimeMs: took,
		Truncated:       limit > 0 && envelope.RowCount >= limit,
	}, nil
}

// PostExplorerQuery sends one request to the orchestrator's explorer-query endpoint
// and returns the response body, or the orchestrator's error text verbatim so
// ClassifyExecutionError upstream can give the user something specific. timeout is
// the HTTP client's: an interactive query's minute, or a model run's whole budget.
func (o Orchestrator) PostExplorerQuery(ctx context.Context, payload map[string]interface{}, timeout time.Duration) ([]byte, error) {
	orchestratorURL := o.URL
	if orchestratorURL == "" {
		return nil, fmt.Errorf("explorer_query: no orchestrator URL")
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("explorer_query: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		orchestratorURL+"/api/v1/agent/explorer-query",
		bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("explorer_query: build http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.Authorize != nil {
		o.Authorize(httpReq)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("explorer_query: orchestrator unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errEnv struct {
			Error   string `json:"error"`
			Details string `json:"details"`
		}
		_ = json.Unmarshal(body, &errEnv)
		msg := strings.TrimSpace(errEnv.Details)
		if msg == "" {
			msg = strings.TrimSpace(errEnv.Error)
		}
		if msg == "" {
			msg = fmt.Sprintf("orchestrator returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return body, nil
}
