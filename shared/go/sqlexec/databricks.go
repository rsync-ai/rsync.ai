package sqlexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/mask"
	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// =============================================================================
// DATABRICKS SQL (Statement Execution API)
// =============================================================================

func getStringConfig(config map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := config[k]; ok {
			switch vv := v.(type) {
			case string:
				if strings.TrimSpace(vv) != "" {
					return strings.TrimSpace(vv)
				}
			}
		}
	}
	return ""
}

func normalizeDatabricksHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	return s
}

func parseWarehouseIDFromHTTPPath(httpPath string) string {
	// Common patterns:
	// - /sql/1.0/warehouses/<warehouse_id>
	// - /sql/1.0/warehouses/<warehouse_id>/
	s := strings.TrimSpace(httpPath)
	if s == "" {
		return ""
	}
	re := regexp.MustCompile(`/warehouses/([^/]+)`)
	m := re.FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

type databricksStatementResponse struct {
	StatementID string `json:"statement_id"`
	Status      struct {
		State string `json:"state"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"status"`
	// Column schema lives under the TOP-LEVEL `manifest.schema.columns` in the
	// Databricks SQL Statement Execution API — NOT under `result.schema` (which
	// does not exist). Reading it from `result` yielded empty columns, so every
	// row was built as an empty map — the Data Explorer returned blank results.
	Manifest struct {
		Schema struct {
			Columns []struct {
				Name string `json:"name"`
				Type string `json:"type_text,omitempty"`
			} `json:"columns"`
		} `json:"schema"`
	} `json:"manifest"`
	Result *struct {
		DataArray [][]interface{} `json:"data_array"`
	} `json:"result"`
}

func databricksExecuteStatement(ctx context.Context, newClient func(time.Duration) *http.Client, host, token, warehouseID, sqlText, catalog, schemaName string) (*databricksStatementResponse, error) {
	return databricksExecuteStatementParams(ctx, newClient, host, token, warehouseID, sqlText, catalog, schemaName, nil)
}

// DatabricksParam is one named parameter of a statement, bound as :Name in its SQL by
// the Statement Execution API, never pasted into it. A dashboard filter's values are
// sent this way.
type DatabricksParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

func databricksExecuteStatementParams(ctx context.Context, newClient func(time.Duration) *http.Client, host, token, warehouseID, sqlText, catalog, schemaName string, params []DatabricksParam) (*databricksStatementResponse, error) {
	if host == "" || token == "" || warehouseID == "" {
		return nil, fmt.Errorf("missing databricks config (host/token/warehouse_id required)")
	}
	url := fmt.Sprintf("https://%s/api/2.0/sql/statements", host)
	body := map[string]interface{}{
		"statement":       sqlText,
		"warehouse_id":    warehouseID,
		"disposition":     "INLINE",
		"format":          "JSON_ARRAY",
		"wait_timeout":    "30s",
		"on_wait_timeout": "CANCEL",
	}
	if strings.TrimSpace(catalog) != "" {
		body["catalog"] = catalog
	}
	if strings.TrimSpace(schemaName) != "" {
		body["schema"] = schemaName
	}
	if len(params) > 0 {
		body["parameters"] = params
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	// SSRF-guarded: host is user-supplied Databricks connection config and this
	// request carries a Bearer token — must refuse internal/link-local targets
	// (SAFEHTTP_ALLOW_HOSTS opt-out for a private-network workspace). The caller
	// passes that guard in as newClient (the gateway's safehttp.NewClient).
	if newClient == nil {
		return nil, fmt.Errorf("databricks: no HTTP client to reach the workspace with")
	}
	client := newClient(35 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("databricks statement failed (HTTP %d): %s", resp.StatusCode, string(b))
	}
	var out databricksStatementResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("failed to parse databricks response: %w", err)
	}
	return &out, nil
}

// executeDatabricksQueryParams runs a read with named parameters bound by the
// Statement Execution API, masked when redact is set.
func executeDatabricksQueryParams(ctx context.Context, newClient func(time.Duration) *http.Client, config map[string]interface{}, sqlQuery string, limit int, redact bool, params []DatabricksParam) (*Result, error) {
	host := normalizeDatabricksHost(getStringConfig(config, "host", "server_hostname", "workspace_host"))
	token := getStringConfig(config, "access_token", "token", "pat")
	warehouseID := getStringConfig(config, "warehouse_id", "warehouseId", "sql_warehouse_id")
	httpPath := getStringConfig(config, "http_path")
	if warehouseID == "" {
		warehouseID = parseWarehouseIDFromHTTPPath(httpPath)
	}
	catalog := getStringConfig(config, "catalog", "database")
	schemaName := getStringConfig(config, "schema", "schema_name")

	queryCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	// Clamp query LIMIT (Databricks supports LIMIT).
	wrappedSQL := stmt.ClampLimit(sqlQuery, limit+1) // +1 row so the Truncated flag below is accurate (ClampLimit caps the DB-side LIMIT)

	start := time.Now()
	resp, err := databricksExecuteStatementParams(queryCtx, newClient, host, token, warehouseID, wrappedSQL, catalog, schemaName, params)
	if err != nil {
		return nil, err
	}

	if resp.Status.State != "" && strings.ToUpper(resp.Status.State) != "SUCCEEDED" {
		msg := strings.TrimSpace(resp.Status.Error.Message)
		if msg == "" {
			msg = fmt.Sprintf("statement status=%s", resp.Status.State)
		}
		return nil, fmt.Errorf("databricks query failed: %s", msg)
	}
	if resp.Result == nil {
		return nil, fmt.Errorf("databricks query returned no result")
	}

	cols := []string{}
	types := []string{}
	for _, c := range resp.Manifest.Schema.Columns {
		if c.Name != "" {
			cols = append(cols, c.Name)
			types = append(types, c.Type)
		}
	}

	rows := []map[string]interface{}{}
	warnings := []string{}
	truncated := false

	for _, arr := range resp.Result.DataArray {
		if len(rows) >= limit {
			truncated = true
			break
		}
		row := map[string]interface{}{}
		for i, colName := range cols {
			var v interface{} = nil
			if i < len(arr) {
				v = arr[i]
			}
			// PII redaction bypassed for data-owner export (T2-7).
			if redact && mask.ShouldRedactColumnName(colName) {
				v = mask.RedactForPreview(v)
			}
			row[colName] = v
		}
		rows = append(rows, row)
	}

	return &Result{
		Columns:         cols,
		Rows:            rows,
		RowCount:        len(rows),
		ExecutionTimeMs: time.Since(start).Milliseconds(),
		Truncated:       truncated,
		Warnings:        warnings,
		ColumnTypes:     types,
	}, nil
}
