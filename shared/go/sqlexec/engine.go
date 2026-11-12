// Package sqlexec runs a customer's statements on their own database: a read under a
// row cap, an authorized write, and the schema index the Explorer searches. It is the
// one place that does (query-service-design.md § 6), so a read is held read-only in one
// place and a new engine is added in one place.
//
// The gateway calls it in process today (Q0). query-service (Q1) will call it instead,
// and the gateway will stop linking it.
package sqlexec

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rsync-ai/shared/sqlexec/schemaindex"
	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// Engine runs statements on one connection.
type Engine interface {
	// Query runs a read and returns at most l.Rows rows. A direct engine runs it in a
	// transaction the database holds to reads (readonly.go). args are bound by the
	// driver, never pasted into the SQL.
	Query(ctx context.Context, sql string, args []any, l Limits) (*Result, error)
	// Exec runs a write the caller is allowed: a statement of a class above allowed is
	// refused before it reaches the database.
	Exec(ctx context.Context, sql string, allowed stmt.StatementClass) (*Result, error)
	// SchemaIndex lists the connection's tables, columns and foreign keys.
	// allDatabases widens a MySQL connection to every database it can see.
	SchemaIndex(ctx context.Context, allDatabases bool) (*schemaindex.Index, error)
}

// Limits bounds one read.
type Limits struct {
	// Rows is the most rows the read returns.
	Rows int
	// Mask hides the values of columns whose names mark them as personal data
	// (mask.ShouldRedactColumnName). A preview, a chart and a workflow step are masked;
	// an export is not: the data owner sees raw values (T2-7).
	Mask bool
}

// Config is what an engine needs to reach a connection.
type Config struct {
	// Settings is the connection's decrypted config.
	Settings map[string]interface{}
	// Orchestrator runs a delegated engine's statements through the connector.
	Orchestrator Orchestrator
	// HTTPClient makes the client a Databricks engine reaches its workspace with. It must
	// refuse internal and link-local addresses: the host is the user's and the request
	// carries their token (the gateway passes safehttp.NewClient).
	HTTPClient func(timeout time.Duration) *http.Client
}

// Orchestrator is where a delegated engine sends its statements: the orchestrator's
// explorer-query endpoint, which runs the connector's MCP tool.
type Orchestrator struct {
	// URL is the orchestrator's base URL, without a trailing slash.
	URL string
	// Authorize signs each request as an internal service.
	Authorize func(*http.Request)
}

// ErrUnsupported is For's error for a connection the Explorer cannot query.
var ErrUnsupported = errors.New("unsupported connection type")

// ErrNoSchemaIndex is a delegated engine's SchemaIndex error: its schema comes from the
// connector's discover_schema tool, not from an index the engine builds.
var ErrNoSchemaIndex = errors.New("this connection's schema comes from its connector")

// For returns the engine for a connection of connectorType. It is the one dispatcher:
// every read, write and schema index goes through it. The match is the case-insensitive
// substring match the capability table uses (handlers.ResolveExplorerCapability), in the
// same order, so the two never disagree (pinned by a test there).
func For(connectorType string, cfg Config) (Engine, error) {
	ct := strings.ToLower(connectorType)
	switch {
	case strings.Contains(ct, "mysql") || strings.Contains(ct, "mariadb"):
		return mysqlEngine{cfg}, nil
	case strings.Contains(ct, "postgres") || strings.Contains(ct, "redshift"):
		// Redshift speaks the PostgreSQL wire protocol and uses the Postgres driver.
		return postgresEngine{cfg}, nil
	case strings.Contains(ct, "sqlserver") || strings.Contains(ct, "mssql"):
		return sqlServerEngine{cfg}, nil
	case strings.Contains(ct, "databricks"):
		return databricksEngine{cfg}, nil
	case strings.Contains(ct, "bigquery"), strings.Contains(ct, "clickhouse"),
		strings.Contains(ct, "snowflake"), strings.Contains(ct, "oracle"),
		strings.Contains(ct, "mongo"):
		// No driver here: the connector runs the statement. MongoDB is a document
		// store; its callers browse it through the connector's find tool, not Query.
		return delegatedEngine{connectorType: connectorType, cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, connectorType)
	}
}

// IsDelegated reports whether e runs its statements through the connector.
func IsDelegated(e Engine) bool {
	_, ok := e.(delegatedEngine)
	return ok
}

// refuseAbove refuses a statement of a class above allowed. The gateway ran the same
// check on the caller's role; this one holds when a caller skips it.
func refuseAbove(sqlText string, allowed stmt.StatementClass) error {
	class := stmt.ClassifyStatementSQL(sqlText)
	if !stmt.Permits(allowed, class) {
		return fmt.Errorf("a %s statement is above what this caller may run (%s)", class, allowed)
	}
	return nil
}

type postgresEngine struct{ cfg Config }

func (e postgresEngine) Query(ctx context.Context, sqlText string, args []any, l Limits) (*Result, error) {
	return executePostgresQueryWithRedact(ctx, e.cfg.Settings, sqlText, l.Rows, l.Mask, args...)
}

func (e postgresEngine) Exec(ctx context.Context, sqlText string, allowed stmt.StatementClass) (*Result, error) {
	if err := refuseAbove(sqlText, allowed); err != nil {
		return nil, err
	}
	return executeDirectWrite(ctx, "postgres", postgresExplorerDSN(e.cfg.Settings), sqlText)
}

func (e postgresEngine) SchemaIndex(ctx context.Context, _ bool) (*schemaindex.Index, error) {
	tables, err := fetchPostgresSchemaIndex(ctx, e.cfg.Settings)
	if err != nil {
		return nil, err
	}
	// Foreign keys are a nicety: an index without them still serves search.
	fks, _ := fetchPostgresForeignKeys(ctx, e.cfg.Settings)
	return &schemaindex.Index{Tables: tables, ForeignKeys: fks}, nil
}

type mysqlEngine struct{ cfg Config }

func (e mysqlEngine) Query(ctx context.Context, sqlText string, args []any, l Limits) (*Result, error) {
	return executeMySQLQueryWithRedact(ctx, e.cfg.Settings, sqlText, l.Rows, l.Mask, args...)
}

func (e mysqlEngine) Exec(ctx context.Context, sqlText string, allowed stmt.StatementClass) (*Result, error) {
	if err := refuseAbove(sqlText, allowed); err != nil {
		return nil, err
	}
	return executeDirectWrite(ctx, "mysql", mysqlExplorerDSN(e.cfg.Settings), sqlText)
}

func (e mysqlEngine) SchemaIndex(ctx context.Context, allDatabases bool) (*schemaindex.Index, error) {
	settings := e.cfg.Settings
	if allDatabases {
		// No default database: the index lists every database the user can see.
		settings = make(map[string]interface{}, len(e.cfg.Settings))
		for k, v := range e.cfg.Settings {
			settings[k] = v
		}
		settings["database"] = ""
	}
	tables, err := fetchMySQLSchemaIndex(ctx, settings)
	if err != nil {
		return nil, err
	}
	fks, _ := fetchMySQLForeignKeys(ctx, settings)
	return &schemaindex.Index{Tables: tables, ForeignKeys: fks}, nil
}

type sqlServerEngine struct{ cfg Config }

func (e sqlServerEngine) Query(ctx context.Context, sqlText string, args []any, l Limits) (*Result, error) {
	return executeSQLServerQueryWithRedact(ctx, e.cfg.Settings, sqlText, l.Rows, l.Mask, args...)
}

func (e sqlServerEngine) Exec(ctx context.Context, sqlText string, allowed stmt.StatementClass) (*Result, error) {
	if err := refuseAbove(sqlText, allowed); err != nil {
		return nil, err
	}
	return executeDirectWrite(ctx, "sqlserver", sqlServerExplorerDSN(e.cfg.Settings), sqlText)
}

func (e sqlServerEngine) SchemaIndex(ctx context.Context, _ bool) (*schemaindex.Index, error) {
	tables, err := fetchSQLServerSchemaIndex(ctx, e.cfg.Settings)
	if err != nil {
		return nil, err
	}
	return &schemaindex.Index{Tables: tables}, nil
}

type databricksEngine struct{ cfg Config }

// Query binds each arg as a named parameter; each must be a DatabricksParam.
func (e databricksEngine) Query(ctx context.Context, sqlText string, args []any, l Limits) (*Result, error) {
	params := make([]DatabricksParam, 0, len(args))
	for _, a := range args {
		p, ok := a.(DatabricksParam)
		if !ok {
			return nil, fmt.Errorf("a Databricks statement takes named parameters, not %T", a)
		}
		params = append(params, p)
	}
	if len(params) == 0 {
		params = nil
	}
	return executeDatabricksQueryParams(ctx, e.cfg.HTTPClient, e.cfg.Settings, sqlText, l.Rows, l.Mask, params)
}

func (e databricksEngine) Exec(ctx context.Context, sqlText string, allowed stmt.StatementClass) (*Result, error) {
	if err := refuseAbove(sqlText, allowed); err != nil {
		return nil, err
	}
	return executeDatabricksWrite(ctx, e.cfg.HTTPClient, e.cfg.Settings, sqlText)
}

func (e databricksEngine) SchemaIndex(ctx context.Context, _ bool) (*schemaindex.Index, error) {
	tables, err := fetchDatabricksSchemaIndex(ctx, e.cfg.HTTPClient, e.cfg.Settings)
	if err != nil {
		return nil, err
	}
	return &schemaindex.Index{Tables: tables}, nil
}

// delegatedEngine sends its statements to the connector through the orchestrator: a
// read to the connector's export tool, a write to its execute tool.
type delegatedEngine struct {
	connectorType string
	cfg           Config
}

func (e delegatedEngine) Query(ctx context.Context, sqlText string, args []any, l Limits) (*Result, error) {
	if len(args) > 0 {
		return nil, errors.New("this connection takes no bound values")
	}
	return e.cfg.Orchestrator.ExplorerQuery(ctx, e.connectorType, e.cfg.Settings, sqlText, l.Rows, l.Mask, false)
}

func (e delegatedEngine) Exec(ctx context.Context, sqlText string, allowed stmt.StatementClass) (*Result, error) {
	if err := refuseAbove(sqlText, allowed); err != nil {
		return nil, err
	}
	// The limit and mask apply to rows; a write returns none.
	return e.cfg.Orchestrator.ExplorerQuery(ctx, e.connectorType, e.cfg.Settings, sqlText, 0, false, true)
}

func (delegatedEngine) SchemaIndex(context.Context, bool) (*schemaindex.Index, error) {
	return nil, ErrNoSchemaIndex
}
