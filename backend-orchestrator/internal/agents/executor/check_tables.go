package executor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
	"github.com/rsync-ai/backend-orchestrator/internal/utils"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"
)

// Table check statuses (CheckTables). The api-gateway stores them on the
// connection (connections.last_table_check) and the frontend shows them, so the
// strings are part of that contract.
const (
	TableCheckOK           = "ok"
	TableCheckFailed       = "failed"
	TableCheckNeedsSetting = "needs_setting"
	TableCheckNotChecked   = "not_checked"
)

// tableCheckBudget bounds CheckTables: tables are read one after another, each for
// at most perTable, all of them within total; the tables left when total runs out
// are "not checked". discovery bounds the call that lists the tables. A var so a
// test can shorten it.
var tableCheckBudget = struct {
	perTable, total, discovery time.Duration
}{perTable: 15 * time.Second, total: 60 * time.Second, discovery: 30 * time.Second}

// tableCheckErrorMaxRunes caps a table's error text. The text is stored on the
// connection and shown to everyone in the workspace, so it is scrubbed like any
// connector error that leaves the orchestrator.
const tableCheckErrorMaxRunes = 300

// TableCheck is one table's result. It carries no row value and no row count.
type TableCheck struct {
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	Error           string   `json:"error,omitempty"`
	MissingSettings []string `json:"missing_settings,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
}

// TableCheckResult is CheckTables' answer: every table, and how many ended in each
// status.
type TableCheckResult struct {
	Tables       []TableCheck `json:"tables"`
	OK           int          `json:"ok"`
	Failed       int          `json:"failed"`
	NeedsSetting int          `json:"needs_setting"`
	NotChecked   int          `json:"not_checked"`
}

func (r *TableCheckResult) add(tc TableCheck) {
	r.Tables = append(r.Tables, tc)
	switch tc.Status {
	case TableCheckOK:
		r.OK++
	case TableCheckFailed:
		r.Failed++
	case TableCheckNeedsSetting:
		r.NeedsSetting++
	case TableCheckNotChecked:
		r.NotChecked++
	}
}

// CheckTables reads one page of every table a connection lists, keeping no row, so a
// table whose endpoint the connector cannot read is found when the connection is
// saved instead of on its first run. A connection test checks one endpoint (the
// auth check); every other table can still answer 404 or a shape the connector does
// not parse. A table that needs a connection setting (tables_needing_settings) is
// reported as such, not as a failure. The error is for the listing itself (a
// connector that cannot list its tables); one table's failure is in its result.
func (a *Agent) CheckTables(ctx context.Context, connectorType string, config map[string]interface{}) (*TableCheckResult, error) {
	stringConfig := make(map[string]string, len(config))
	for k, v := range config {
		stringConfig[k] = utils.ConfigString(v)
	}
	log.Infof("🧪 Checking tables for %s (config_keys=%d)", connectorType, len(stringConfig))
	call := func(ctx context.Context, req mcp.ExecuteRequest) (*mcp.ExecuteResponse, error) {
		return a.executeWithOAuthRetry(ctx, req)
	}
	return checkTables(ctx, call, connectorType, stringConfig, a.mcpClient.ConnectorDisplayName)
}

// checkTables is CheckTables with the connector call and the display-name lookup
// passed in, for tests.
func checkTables(
	ctx context.Context,
	call func(context.Context, mcp.ExecuteRequest) (*mcp.ExecuteResponse, error),
	connectorType string,
	config map[string]string,
	displayName func(connectorType, version string) string,
) (*TableCheckResult, error) {
	// The same connector instance the pipeline will run against (as SampleRows).
	version := strings.TrimSpace(config["connector_version"])
	if version == "" {
		version = strings.TrimSpace(config["version"])
	}
	request := func(op string, params map[string]interface{}) mcp.ExecuteRequest {
		params["config"] = config
		return mcp.ExecuteRequest{
			Connector: connectorType,
			Version:   version,
			Operation: op,
			Config:    config,
			Params:    params,
			// Never read on the orchestrator's own interpreter: it lacks the
			// connector's dependencies, so every table would fail with an import error.
			NoStdioWhileDeploying: true,
			DeployWaitTimeout:     tableCheckBudget.perTable,
		}
	}
	failure := func(resp *mcp.ExecuteResponse, err error, fallback string) string {
		msg := fallback
		if err != nil {
			msg = err.Error()
		} else if resp != nil && strings.TrimSpace(resp.Error) != "" {
			msg = resp.Error
		}
		if friendly, ok := mcp.FriendlyTestConnectionError(displayName(connectorType, version), msg); ok {
			msg = friendly
		}
		return msg
	}

	// The table names only: no probe of each table (include_row_counts drives it).
	dctx, cancel := context.WithTimeout(ctx, tableCheckBudget.discovery)
	listing, err := call(dctx, request("discover_schema", map[string]interface{}{
		"include_row_counts":    false,
		"include_columns":       false,
		"include_relationships": false,
		"max_tables":            sourceDiscoveryMaxTables,
	}))
	cancel()
	if err != nil || listing == nil || !listing.Success || listing.Result == nil {
		return nil, errors.New(tableCheckError(failure(listing, err, "the connector did not list its tables")))
	}
	if err := discoveryFailure(connectorType, listing.Result); err != nil {
		return nil, err
	}

	var names []string
	raw, _ := listing.Result["tables"].([]interface{})
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		// A resource the API cannot list (create-only, or get-by-id only) is not
		// read by a sync, so it is not read here either.
		if listable, present := m["supports_list"].(bool); present && !listable {
			continue
		}
		names = append(names, name)
	}

	result := &TableCheckResult{Tables: []TableCheck{}}
	totalCtx, cancelTotal := context.WithTimeout(ctx, tableCheckBudget.total)
	defer cancelTotal()
	for _, name := range names {
		if totalCtx.Err() != nil {
			result.add(TableCheck{Name: name, Status: TableCheckNotChecked})
			continue
		}
		tctx, cancelTable := context.WithTimeout(totalCtx, tableCheckBudget.perTable)
		resp, err := call(tctx, request("export", map[string]interface{}{
			"table":     name,
			"limit":     1,
			"max_pages": 1,
		}))
		timedOut := tctx.Err() != nil
		cancelTable()
		switch {
		case totalCtx.Err() != nil:
			// The total ran out while this table was read: it was not checked.
			result.add(TableCheck{Name: name, Status: TableCheckNotChecked})
		case timedOut:
			result.add(TableCheck{Name: name, Status: TableCheckFailed,
				Error: fmt.Sprintf("no answer within %s", tableCheckBudget.perTable)})
		case err != nil || resp == nil || !resp.Success:
			result.add(TableCheck{Name: name, Status: TableCheckFailed,
				Error: tableCheckError(failure(resp, err, "the connector could not read this table"))})
		default:
			// The rows are dropped here: only the paging warnings are kept.
			result.add(TableCheck{Name: name, Status: TableCheckOK, Warnings: exportWarnings(resp.Result)})
		}
	}

	needing := tablesNeedingSettings(listing.Result)
	waiting := make([]string, 0, len(needing))
	for name := range needing {
		waiting = append(waiting, name)
	}
	sort.Strings(waiting)
	for _, name := range waiting {
		result.add(TableCheck{Name: name, Status: TableCheckNeedsSetting, MissingSettings: needing[name]})
	}
	return result, nil
}

// tableCheckError scrubs a connector's error before it is stored on the connection.
// A failed image build is kept whole: its log is the connector's own code and
// requirements, never customer data, and its last lines name the cause
// ("No module named 'acme'"), which the scrub would mask.
func tableCheckError(msg string) string {
	if mcp.IsConnectorBuildFailedMessage(msg) {
		return msg
	}
	return llmscrub.ScrubMax(msg, tableCheckErrorMaxRunes)
}

// exportWarnings reads an export result's warnings (base_connector.py
// PaginationHandler: what may have left rows unread), scrubbed.
func exportWarnings(result map[string]interface{}) []string {
	raw, _ := result["warnings"].([]interface{})
	var out []string
	for _, w := range raw {
		if s, ok := w.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, llmscrub.ScrubMax(s, tableCheckErrorMaxRunes))
		}
	}
	return out
}
