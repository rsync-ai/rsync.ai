package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/safehttp"
	"api-gateway/internal/validators"

	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/shared/sqlexec"
)

// ============================================================================
// Models on the engines the gateway has no database/sql driver for
// ============================================================================
// Postgres, Redshift, MySQL and SQL Server run a model through executeModelPlan: one
// connection, a transaction where the engine has transactional DDL. The warehouses
// below have neither a driver here nor transactional DDL anywhere, so each statement
// of the plan travels on its own — through the orchestrator and the connector's MCP
// `execute` tool (BigQuery, Snowflake, ClickHouse, Oracle), or through the Databricks
// Statement Execution API the Explorer already uses.
//
// With no transaction to fall back on, the safety the driver path gets from rollback
// has to come from the shape of the plan, and the shape differs per engine:
//
//   - BigQuery, Snowflake, Databricks: CREATE OR REPLACE TABLE … AS is one atomic
//     statement on each — the replacement is built before the old table is let go, a
//     failed build leaves the old one in place, and there is no window between two
//     renames to die in.
//   - ClickHouse: CREATE OR REPLACE is the same on an Atomic database, but a plain
//     CREATE TABLE … AS SELECT is not atomic, so a first run builds aside and renames in.
//   - Oracle: no CREATE OR REPLACE TABLE at all, and every DDL statement commits on
//     its own, so a rebuild is the build-then-swap of the driver path, one statement
//     at a time, with a recovery step for a run that died between its renames.
//
// The collision guarantee is the same on every engine: a run that has not proven
// ownership only ever CREATEs (or RENAMEs) onto the target name, and each engine
// refuses that when the name is taken.

// engineModelPlan is the statements one run sends, in order, and the statement that
// cleans up after a failed one. sweep is empty when a failure leaves nothing behind.
type engineModelPlan struct {
	stmts []string
	sweep string

	// auditAfter is how many statements build the new table before anything publishes
	// it, and auditTable the table they built: where a blocking data check looks
	// (saved_query_checks.go). 0 when the plan has no such point — a statement model,
	// or a rebuild that replaces the table in one statement.
	auditAfter int
	auditTable string
	// blockCleanup undoes the build when the audit refuses it, when that is more than
	// the sweep: a first run whose one CREATE is the publish.
	blockCleanup string
}

// modelEngineRunner sends one statement to the warehouse. Nothing it sends shares a
// session with anything else it sends, so a plan must not depend on one.
type modelEngineRunner interface {
	// exec runs a statement that returns no rows, and the engine's affected-row count
	// when it gives one.
	exec(ctx context.Context, stmt string) (*int64, error)
	// queryRows runs a catalog read.
	queryRows(ctx context.Context, query string) ([]map[string]interface{}, error)
}

// modelDialectRunsOnDriver reports whether the gateway holds a database/sql driver for
// a dialect, which decides between executeModelPlan and runEngineModel.
func modelDialectRunsOnDriver(dialect string) bool {
	switch dialect {
	case "postgres", "mysql", "sqlserver":
		return true
	default:
		return false
	}
}

// newModelEngineRunner picks how a warehouse's statements travel. A package var so a
// test can stand in for the warehouse.
var newModelEngineRunner = func(dialect, connectorType string, config map[string]interface{}) modelEngineRunner {
	if dialect == "databricks" {
		return newDatabricksModelRunner(config)
	}
	return orchestratorModelRunner{connectorType: connectorType, config: config}
}

// runEngineModel runs one model on a warehouse with no driver in the gateway.
func runEngineModel(ctx context.Context, runner modelEngineRunner, dialect, schemaName, tableName string, m *savedQueryModel, checks *modelCheckRun) (*int64, error) {
	plan, err := buildEngineModelPlan(ctx, runner, dialect, schemaName, tableName, m)
	if err != nil {
		return nil, err
	}
	return executeEnginePlan(ctx, runner, plan, checks, quoteModelIdent(dialect, schemaName, tableName))
}

// buildEngineModelPlan returns what one run of m sends. Only an owned Oracle rebuild
// reads the warehouse first; every other plan is decided from the model alone.
func buildEngineModelPlan(ctx context.Context, runner modelEngineRunner, dialect, schemaName, tableName string, m *savedQueryModel) (engineModelPlan, error) {
	if isStatementModel(m.Materialization) {
		// authorizeModelRun has already cleared this exact text for the run-as user's
		// role. A statement model builds no scratch table, so there is nothing to sweep.
		return engineModelPlan{stmts: []string{m.SQLText}}, nil
	}

	inner := strings.TrimRight(strings.TrimSpace(m.SQLText), ";")
	target := quoteModelIdent(dialect, schemaName, tableName)

	switch dialect {
	case "bigquery", "snowflake", "databricks":
		if !m.TargetOwned {
			return engineModelPlan{stmts: []string{fmt.Sprintf("CREATE TABLE %s AS %s", target, inner)}}, nil
		}
		// Also the right statement when the table was dropped outside rsync.ai: OR
		// REPLACE creates it, so an owned model never wedges on a missing target.
		return engineModelPlan{stmts: []string{fmt.Sprintf("CREATE OR REPLACE TABLE %s AS %s", target, inner)}}, nil

	case "clickhouse":
		// A table needs an engine. MergeTree with no sort key is the general-purpose
		// default and keeps whatever order the query produced on insert.
		const engine = "ENGINE = MergeTree ORDER BY tuple()"
		if m.TargetOwned {
			return engineModelPlan{stmts: []string{fmt.Sprintf("CREATE OR REPLACE TABLE %s %s AS %s", target, engine, inner)}}, nil
		}
		staging := quoteModelIdent(dialect, schemaName, tableName+modelStagingSuffix)
		dropStaging := "DROP TABLE IF EXISTS " + staging
		return engineModelPlan{
			stmts: []string{
				// A scratch table from a run that died before its rename.
				dropStaging,
				fmt.Sprintf("CREATE TABLE %s %s AS %s", staging, engine, inner),
				// RENAME refuses an existing destination, which is the collision check.
				fmt.Sprintf("RENAME TABLE %s TO %s", staging, target),
			},
			sweep:      dropStaging,
			auditAfter: 2,
			auditTable: staging,
		}, nil

	case "oracle":
		return oracleModelPlan(ctx, runner, schemaName, tableName, inner, m.TargetOwned)

	default:
		return engineModelPlan{}, fmt.Errorf("models are not supported for the %s dialect", dialect)
	}
}

// oracleDropIfExists drops a table and tolerates only "it is not there" (ORA-00942).
// Oracle has no DROP TABLE IF EXISTS before 23ai. PURGE skips the recycle bin, whose
// copies would otherwise hold the scratch tables' space until someone empties it.
func oracleDropIfExists(qualified string) string {
	return fmt.Sprintf("BEGIN EXECUTE IMMEDIATE 'DROP TABLE %s PURGE'; EXCEPTION WHEN OTHERS THEN IF SQLCODE != -942 THEN RAISE; END IF; END;", qualified)
}

// oracleModelPlan builds an Oracle rebuild. Every DDL statement on Oracle commits on
// its own, so the plan is shaped so that stopping after any one of its statements
// leaves the live data reachable — and the one place where it would not be, between
// the two renames, is repaired by the next run before it does anything else.
func oracleModelPlan(ctx context.Context, runner modelEngineRunner, schemaName, tableName, inner string, owned bool) (engineModelPlan, error) {
	const dialect = "oracle"
	target := quoteModelIdent(dialect, schemaName, tableName)
	build := engineModelPlan{
		stmts: []string{fmt.Sprintf("CREATE TABLE %s AS %s", target, inner)},
		// The CREATE is the publish, so a check can look only once the table is at the
		// target name. A refused one is dropped: the CREATE refuses an occupied name, so
		// what it made is this run's alone and there was nothing there before it.
		auditAfter:   1,
		auditTable:   target,
		blockCleanup: fmt.Sprintf("DROP TABLE %s PURGE", target),
	}
	if !owned {
		// One CREATE, which refuses an occupied name. Oracle's CREATE TABLE … AS is a
		// single DDL statement, so a failed one leaves no table behind.
		return build, nil
	}

	targetExists, retiredExists, err := oracleModelCatalogState(ctx, runner, schemaName, tableName)
	if err != nil {
		return engineModelPlan{}, err
	}

	retiredName := strings.ToUpper(tableName + modelRetiredSuffix)
	var stmts []string
	if !targetExists {
		if !retiredExists {
			// Dropped outside rsync.ai: build it again, the same way a first run does.
			return build, nil
		}
		// The previous run died between its renames: the target name is empty and the
		// data sits under the retired name, and nothing on Oracle rolled that back. Put
		// it back first. The rebuild below must never begin by dropping the retired
		// name — at this point it is the only copy there is.
		stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s RENAME TO %s",
			quoteModelIdent(dialect, schemaName, tableName+modelRetiredSuffix), `"`+strings.ToUpper(tableName)+`"`))
	}

	staging := quoteModelIdent(dialect, schemaName, tableName+modelStagingSuffix)
	retired := quoteModelIdent(dialect, schemaName, tableName+modelRetiredSuffix)
	// After the recovery rename, if any, and the staging build.
	auditAfter := len(stmts) + 2
	stmts = append(stmts,
		oracleDropIfExists(staging),
		fmt.Sprintf("CREATE TABLE %s AS %s", staging, inner),
		// Cleared only after the replacement is built (see buildMaterializationPlan).
		oracleDropIfExists(retired),
		// ALTER TABLE … RENAME TO takes a bare new name: it cannot move a table
		// between schemas.
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s", target, `"`+retiredName+`"`),
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s", staging, `"`+strings.ToUpper(tableName)+`"`),
		fmt.Sprintf("DROP TABLE %s PURGE", retired),
	)
	return engineModelPlan{stmts: stmts, sweep: oracleDropIfExists(staging), auditAfter: auditAfter, auditTable: staging}, nil
}

// oracleModelCatalogState reports whether the target and the retired name each hold a
// table. ALL_TABLES lists tables only, so a view at the target name reads as "nothing
// there", and the CREATE onto it refuses — the right answer for an object rsync.ai did
// not create.
//
// The names are interpolated as literals. That is safe only because both have passed
// modelIdentPart, which admits no quote character.
func oracleModelCatalogState(ctx context.Context, runner modelEngineRunner, schemaName, tableName string) (targetExists, retiredExists bool, err error) {
	// An unqualified target resolves in the session's current schema, so the check
	// resolves it the same way rather than guessing at the login user.
	owner := "SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')"
	if schemaName != "" {
		owner = "'" + strings.ToUpper(schemaName) + "'"
	}
	targetName := strings.ToUpper(tableName)
	retiredName := strings.ToUpper(tableName + modelRetiredSuffix)
	query := fmt.Sprintf("SELECT TABLE_NAME FROM ALL_TABLES WHERE OWNER = %s AND TABLE_NAME IN ('%s', '%s')",
		owner, targetName, retiredName)

	rows, err := runner.queryRows(ctx, query)
	if err != nil {
		// Not knowing is not "it is not there": that guess would send a live table into
		// a plain CREATE and fail it at best. Fail the run instead; nothing has run yet.
		return false, false, fmt.Errorf("failed to check whether the target table exists: %w", err)
	}
	for _, row := range rows {
		for k, v := range row {
			if !strings.EqualFold(k, "TABLE_NAME") {
				continue
			}
			switch strings.ToUpper(fmt.Sprint(v)) {
			case targetName:
				targetExists = true
			case retiredName:
				retiredExists = true
			}
		}
	}
	return targetExists, retiredExists, nil
}

// engineModelSweepTimeout bounds the cleanup after a failed run. It runs on a context
// of its own, because the run's context may be the thing that expired.
const engineModelSweepTimeout = 60 * time.Second

// executeEnginePlan runs the plan in order, stopping at the first failure, and sweeps
// the scratch table when the plan has one. With blocking checks it stops after
// plan.auditAfter statements to run them on plan.auditTable; a block runs the plan's
// cleanup and nothing after that point, and returns errModelCheckBlocked. Warnings run
// on liveTable once the whole plan has.
func executeEnginePlan(ctx context.Context, runner modelEngineRunner, plan engineModelPlan, checks *modelCheckRun, liveTable string) (*int64, error) {
	audits := checks.hasBlock()
	if audits && (plan.auditAfter <= 0 || plan.auditAfter > len(plan.stmts)) {
		// newModelCheckRun turns a block this engine cannot hold into a warning, so
		// this is a plan with no audit point the run did not expect. Fail closed.
		checks.auditUnreachable(modelCheckNoAuditPoint)
		return nil, errModelCheckBlocked
	}
	rd := engineCheckReader{runner: runner}
	blocked := func() (*int64, error) {
		cleanup := plan.blockCleanup
		if cleanup == "" {
			cleanup = plan.sweep
		}
		engineModelCleanup(runner, cleanup, "could not remove the table a data check refused")
		return nil, errModelCheckBlocked
	}

	var rowsAffected *int64
	for i, stmt := range plan.stmts {
		if audits && i == plan.auditAfter && checks.audit(ctx, rd, plan.auditTable) {
			return blocked()
		}
		n, err := runner.exec(ctx, stmt)
		if err != nil {
			engineModelCleanup(runner, plan.sweep, "could not sweep the staging table after a failed rebuild")
			return nil, fmt.Errorf("statement failed: %w", err)
		}
		// A CREATE or a RENAME affects no rows by definition; a count there would read
		// as "0 rows" (see reportsRowsAffected).
		if n != nil && sqlexec.ReportsRowsAffected(validators.ClassifyStatementSQL(stmt)) {
			v := *n
			rowsAffected = &v
		}
	}
	// A plan whose last statement is its build: an Oracle first run, whose one CREATE
	// is also its publish. The table is at the target name already, so the cleanup
	// drops it.
	if audits && plan.auditAfter == len(plan.stmts) && checks.audit(ctx, rd, plan.auditTable) {
		return blocked()
	}
	checks.report(ctx, rd, liveTable)
	return rowsAffected, nil
}

// engineModelCleanup runs a cleanup statement on a fresh deadline: the run's context
// may be the thing that expired. Best effort; the failure worth reporting is the
// run's own.
func engineModelCleanup(runner modelEngineRunner, stmt, failure string) {
	if stmt == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), engineModelSweepTimeout)
	defer cancel()
	if _, err := runner.exec(cleanupCtx, stmt); err != nil {
		log.WithError(err).Warn("model run: " + failure)
	}
}

// ---------------------------------------------------------------------------
// Through the orchestrator: BigQuery, Snowflake, ClickHouse, Oracle
// ---------------------------------------------------------------------------

// orchestratorModelRunner sends each statement to the connector's MCP `execute` tool
// through the orchestrator, the same route an Explorer write on these engines takes —
// but as a model run, which the orchestrator sends once and gives the run's whole
// budget instead of an interactive query's 30 seconds.
type orchestratorModelRunner struct {
	connectorType string
	config        map[string]interface{}
}

// modelStatementMargin is how much sooner than the run's own deadline the warehouse is
// asked to stop a statement, so the warehouse ends it and says why before the gateway
// gives up on the call — a statement abandoned rather than stopped would keep running,
// and a first-run CREATE that lands after its run failed leaves a table rsync.ai never
// recorded owning.
const modelStatementMargin = 5 * time.Second

// modelStatementTimeoutSeconds is the warehouse-side limit for one statement.
func modelStatementTimeoutSeconds(ctx context.Context) int {
	remaining := modelRunTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	secs := int((remaining - modelStatementMargin) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// orchestratorModelClientTimeout outlasts the run, so the run's context — not the
// HTTP client — decides when a statement has taken too long.
func orchestratorModelClientTimeout(ctx context.Context) time.Duration {
	remaining := modelRunTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	return remaining + time.Minute
}

func (r orchestratorModelRunner) exec(ctx context.Context, stmt string) (*int64, error) {
	body, err := explorerOrchestrator().PostExplorerQuery(ctx, map[string]interface{}{
		"connector_type":  r.connectorType,
		"config":          r.config,
		"query":           stmt,
		"write":           true,
		"model_run":       true,
		"timeout_seconds": modelStatementTimeoutSeconds(ctx),
	}, orchestratorModelClientTimeout(ctx))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		RowsAffected *int64 `json:"rows_affected"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("explorer_query: decode response: %w", err)
	}
	return envelope.RowsAffected, nil
}

func (r orchestratorModelRunner) queryRows(ctx context.Context, query string) ([]map[string]interface{}, error) {
	// A catalog read: no redaction (table names, no customer data), a few rows at most.
	resp, err := explorerOrchestrator().ExplorerQuery(ctx, r.connectorType, r.config, query, 10, false, false)
	if err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// ---------------------------------------------------------------------------
// Databricks: the Statement Execution API, asynchronously
// ---------------------------------------------------------------------------

// databricksModelPollInterval is how often a running statement is checked. A package
// var so a test does not wait on it.
var databricksModelPollInterval = 2 * time.Second

// databricksModelRunner runs a statement through the Statement Execution API the
// Explorer uses, but submitted asynchronously and polled: the API blocks for at most
// 50 seconds, and a rebuild can take far longer. Submitting with wait_timeout=0s
// returns the statement id at once, so from the moment the statement exists the
// runner can cancel it — the run's deadline stops the statement on the warehouse
// instead of walking away from it.
type databricksModelRunner struct {
	baseURL      string
	token        string
	warehouseID  string
	catalog      string
	schemaName   string
	client       *http.Client
	pollInterval time.Duration
}

func newDatabricksModelRunner(config map[string]interface{}) *databricksModelRunner {
	r := &databricksModelRunner{
		token:       sqlexec.ConfigString(config, "access_token", "token", "pat"),
		warehouseID: sqlexec.ConfigString(config, "warehouse_id", "warehouseId", "sql_warehouse_id"),
		catalog:     sqlexec.ConfigString(config, "catalog", "database"),
		schemaName:  sqlexec.ConfigString(config, "schema", "schema_name"),
		// SSRF-guarded like the Explorer's own Databricks calls: the host is
		// user-supplied and every request carries a Bearer token. Per request, not per
		// run — each poll is short.
		client:       safehttp.NewClient(60 * time.Second),
		pollInterval: databricksModelPollInterval,
	}
	if host := sqlexec.NormalizeDatabricksHost(sqlexec.ConfigString(config, "host", "server_hostname", "workspace_host")); host != "" {
		r.baseURL = "https://" + host
	}
	if r.warehouseID == "" {
		r.warehouseID = sqlexec.WarehouseIDFromHTTPPath(sqlexec.ConfigString(config, "http_path"))
	}
	return r
}

func (r *databricksModelRunner) do(ctx context.Context, method, path string, payload interface{}) (*sqlexec.DatabricksStatementResponse, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
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
	var out sqlexec.DatabricksStatementResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("failed to parse databricks response: %w", err)
	}
	return &out, nil
}

// cancel stops a statement on the warehouse. It runs on a context of its own, because
// it is called when the run's context has ended.
func (r *databricksModelRunner) cancel(statementID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/api/2.0/sql/statements/"+statementID+"/cancel", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		log.WithError(err).Warn("model run: could not cancel the Databricks statement at the run's deadline")
		return
	}
	_ = resp.Body.Close()
}

func (r *databricksModelRunner) exec(ctx context.Context, stmt string) (*int64, error) {
	resp, err := r.run(ctx, stmt)
	if err != nil {
		return nil, err
	}
	return databricksAffectedRows(resp), nil
}

// queryRows reads a data check's counts, through the same submit-and-poll as a
// rebuild: a check on a large table can outlast the API's blocking window too. Every
// value comes back as the API sends it, a string or null.
func (r *databricksModelRunner) queryRows(ctx context.Context, query string) ([]map[string]interface{}, error) {
	resp, err := r.run(ctx, query)
	if err != nil {
		return nil, err
	}
	if resp.Result == nil {
		return nil, nil
	}
	cols := resp.Manifest.Schema.Columns
	rows := make([]map[string]interface{}, 0, len(resp.Result.DataArray))
	for _, values := range resp.Result.DataArray {
		row := make(map[string]interface{}, len(cols))
		for i, col := range cols {
			if i < len(values) {
				row[col.Name] = values[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// run submits one statement and polls it to an end state.
func (r *databricksModelRunner) run(ctx context.Context, stmt string) (*sqlexec.DatabricksStatementResponse, error) {
	if r.baseURL == "" || r.token == "" || r.warehouseID == "" {
		return nil, fmt.Errorf("missing databricks config (host/token/warehouse_id required)")
	}
	submit := map[string]interface{}{
		"statement":    stmt,
		"warehouse_id": r.warehouseID,
		"disposition":  "INLINE",
		"format":       "JSON_ARRAY",
		"wait_timeout": "0s",
	}
	// An unqualified target lands in the connection's catalog and schema, the same
	// place the Explorer reads it from.
	if r.catalog != "" {
		submit["catalog"] = r.catalog
	}
	if r.schemaName != "" {
		submit["schema"] = r.schemaName
	}
	resp, err := r.do(ctx, http.MethodPost, "/api/2.0/sql/statements", submit)
	if err != nil {
		return nil, err
	}
	id := resp.StatementID
	for {
		switch strings.ToUpper(resp.Status.State) {
		case "SUCCEEDED":
			return resp, nil
		case "FAILED", "CANCELED", "CLOSED":
			msg := strings.TrimSpace(resp.Status.Error.Message)
			if msg == "" {
				msg = "statement status=" + resp.Status.State
			}
			return nil, fmt.Errorf("%s", msg)
		}
		if id == "" {
			return nil, fmt.Errorf("databricks returned no statement id for a running statement")
		}
		select {
		case <-ctx.Done():
			r.cancel(id)
			return nil, fmt.Errorf("databricks statement stopped at the run's deadline: %w", ctx.Err())
		case <-time.After(r.pollInterval):
		}
		if resp, err = r.do(ctx, http.MethodGet, "/api/2.0/sql/statements/"+id, nil); err != nil {
			if ctx.Err() != nil {
				r.cancel(id)
			}
			return nil, err
		}
	}
}

// databricksAffectedRows reads num_affected_rows, which Databricks returns as the
// result of a DML statement. DDL returns no such column, and the count is then nil.
func databricksAffectedRows(resp *sqlexec.DatabricksStatementResponse) *int64 {
	if resp.Result == nil || len(resp.Result.DataArray) == 0 {
		return nil
	}
	for i, col := range resp.Manifest.Schema.Columns {
		if col.Name != "num_affected_rows" || i >= len(resp.Result.DataArray[0]) {
			continue
		}
		n, err := strconv.ParseInt(fmt.Sprint(resp.Result.DataArray[0][i]), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	return nil
}
