package agentgw

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/handlers"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

// Handlers are the REST handlers the tools read through, in-process and as the
// token's user, so a tool can never see more than that user sees in the UI.
// Production uses DefaultHandlers; tests pass fakes.
type Handlers struct {
	ListConnections   gin.HandlerFunc
	GetSchemaIndex    gin.HandlerFunc
	ListPipelines     gin.HandlerFunc
	GetPipeline       gin.HandlerFunc
	GetExecution      gin.HandlerFunc
	GetPipelineHealth gin.HandlerFunc
	ListRuns          gin.HandlerFunc
	ListConnectors    gin.HandlerFunc
	// GetConnectorGeneration reads one generation run, as get does.
	GetConnectorGeneration gin.HandlerFunc
	// StartConnectorGeneration starts a generation run. It is called
	// directly, not through get: it writes, and its refusals are typed
	// (*handlers.GenerationRefusal), so the agent reads the real reason.
	StartConnectorGeneration func(c *gin.Context, actor services.Actor, in handlers.GenerationInput) (*handlers.GenerationRun, error)

	// The model tools' handlers (models.go). The writes go through do, and
	// a refusal's own reason reaches the agent through explain.
	ListSavedQueries      gin.HandlerFunc
	ListWorkspaceLayers   gin.HandlerFunc
	GetSavedQuery         gin.HandlerFunc
	GetSavedQuerySchedule gin.HandlerFunc
	// SuggestSavedQueryUpstreams reads the tables a model's query names and the
	// pipelines and models that write them; get_model passes on what it found.
	SuggestSavedQueryUpstreams   gin.HandlerFunc
	ListSavedQueryRuns           gin.HandlerFunc
	CreateSavedQuery             gin.HandlerFunc
	DeleteSavedQuery             gin.HandlerFunc
	SetSavedQueryMaterialization gin.HandlerFunc
	GetSilverSQL                 gin.HandlerFunc
	CreateSavedQuerySchedule     gin.HandlerFunc
	StartSavedQueryModelRun      gin.HandlerFunc
	PauseSavedQuerySchedule      gin.HandlerFunc
	ResumeSavedQuerySchedule     gin.HandlerFunc

	// The model check tools' handlers (model_checks.go). PlanModelChecks judges
	// a draft and saves nothing; no tool saves, changes or runs a check.
	ListModelChecks gin.HandlerFunc
	PlanModelChecks gin.HandlerFunc

	// ProposePipelinePlan stores a proposed pipeline (plans.go). It is called
	// directly, as StartConnectorGeneration is: its refusals are typed
	// (*handlers.PlanRefusal). Nothing here applies a plan.
	ProposePipelinePlan func(c *gin.Context, actor services.Actor, in handlers.PipelinePlanInput) (*handlers.PipelinePlan, error)

	// ProposeWorkflowDraft stores a drafted workflow (workflows.go), called
	// directly for its typed refusals. Nothing here turns a draft on or runs it.
	ProposeWorkflowDraft func(c *gin.Context, actor services.Actor, in handlers.WorkflowDraftInput) (*handlers.Workflow, error)

	// ListWorkflows lists the workspace's workflows for list_workflows, which
	// shapes them to names and descriptions: never their SQL or messages.
	ListWorkflows func(c *gin.Context, actor services.Actor) ([]*handlers.Workflow, error)

	// ProposeWorkflowChange stores a change to a workflow's branches
	// (plan_workflow_change). Nothing here applies it: the person does.
	ProposeWorkflowChange func(c *gin.Context, actor services.Actor, workflowID string,
		in []handlers.WorkflowBranchInput, summary string) (*handlers.WorkflowChange, error)

	// The dashboard tools' handlers (dashboards.go), called directly for their typed
	// refusals. ListChartSources and DescribeChartSource read names and column kinds;
	// ListDashboards and GetDashboard read dashboards, their tiles and their charts'
	// settings; ProposeDashboard stores a draft the person saves, and
	// ProposeDashboardChange a change to one tile the person applies. No tool saves a
	// chart or a dashboard.
	ListChartSources       func(c *gin.Context, actor services.Actor) ([]handlers.ChartSourceItem, error)
	DescribeChartSource    func(c *gin.Context, actor services.Actor, id string) (*handlers.ChartSourceItem, []handlers.ChartSourceColumn, error)
	ProposeDashboard       func(c *gin.Context, actor services.Actor, in handlers.DashboardProposalInput) (*handlers.DashboardProposal, error)
	ListDashboards         func(c *gin.Context, actor services.Actor) ([]handlers.DashboardItem, error)
	GetDashboard           func(c *gin.Context, actor services.Actor, id string) (*handlers.DashboardOutline, error)
	ProposeDashboardChange func(c *gin.Context, actor services.Actor, in handlers.DashboardChangeInput) (*handlers.DashboardChange, error)

	// PlanUsage reads the workspace's plan and the counts against it
	// (usage.go): never a member's e-mail or the data the workspace moved.
	PlanUsage func(c *gin.Context, actor services.Actor) (*handlers.PlanUsage, error)
}

func DefaultHandlers() Handlers {
	return Handlers{
		ListConnections:   handlers.ListConnections,
		GetSchemaIndex:    handlers.GetSchemaIndex,
		ListPipelines:     handlers.ListPipelines,
		GetPipeline:       handlers.GetPipeline,
		GetExecution:      handlers.GetExecution,
		GetPipelineHealth: handlers.GetPipelineHealth,
		ListRuns:          handlers.ListRunHistory,
		ListConnectors:    handlers.ListMCPConnectors,

		GetConnectorGeneration:   handlers.GetConnectorGeneration,
		StartConnectorGeneration: handlers.StartConnectorGeneration,

		ListSavedQueries:             handlers.ListSavedQueries,
		ListWorkspaceLayers:          handlers.ListWorkspaceLayers,
		GetSavedQuery:                handlers.GetSavedQuery,
		GetSavedQuerySchedule:        handlers.GetSavedQuerySchedule,
		SuggestSavedQueryUpstreams:   handlers.SuggestSavedQueryUpstreams,
		ListSavedQueryRuns:           handlers.ListSavedQueryRuns,
		CreateSavedQuery:             handlers.CreateSavedQuery,
		DeleteSavedQuery:             handlers.DeleteSavedQuery,
		SetSavedQueryMaterialization: handlers.SetSavedQueryMaterialization,
		GetSilverSQL:                 handlers.GetSilverSQL,
		CreateSavedQuerySchedule:     handlers.CreateSavedQuerySchedule,
		StartSavedQueryModelRun:      handlers.StartSavedQueryModelRun,
		PauseSavedQuerySchedule:      handlers.PauseSavedQuerySchedule,
		ResumeSavedQuerySchedule:     handlers.ResumeSavedQuerySchedule,

		ListModelChecks: handlers.ListModelChecks,
		PlanModelChecks: handlers.PlanModelChecks,

		ProposePipelinePlan:   handlers.ProposePipelinePlan,
		ProposeWorkflowDraft:  handlers.ProposeWorkflowDraft,
		ListWorkflows:         handlers.ListWorkflowsAs,
		ProposeWorkflowChange: handlers.ProposeWorkflowChange,

		ListChartSources:       handlers.ListChartSourcesAs,
		DescribeChartSource:    handlers.DescribeChartSourceAs,
		ProposeDashboard:       handlers.ProposeDashboard,
		ListDashboards:         handlers.ListDashboardsAs,
		GetDashboard:           handlers.GetDashboardAs,
		ProposeDashboardChange: handlers.ProposeDashboardChange,

		PlanUsage: handlers.PlanUsageAs,
	}
}

// Tool is one MCP tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	Args        []Arg
	// Action is what services.Authorize checks; it names the token scope the
	// tool needs.
	Action services.Action
	// Write marks a tool that starts work instead of reading. Its annotations
	// say so, and New requires its action to need a scope beyond
	// metadata:read, so a write can never ride on a read-only token.
	Write bool
	// Destructive marks a write that replaces data already there: run_model
	// rebuilds the model's table. Its annotations say so.
	Destructive bool
	// OpenWorld marks a tool that reaches outside rsync.ai: generate_connector
	// has the generator fetch a public API's documents.
	OpenWorld bool
	// ResourceType, and the value of the ResourceArg argument, are what the
	// audit row records for a call.
	ResourceType string
	ResourceArg  string
	// Output is the zero value of the tool's output type. The type IS the
	// allowlist: a key it does not declare can never be returned, whatever the
	// handler behind it starts returning. Returns() lists its key paths.
	Output any
	// Guide is the topic guide that holds the tool's rules (guides.go); its
	// description ends by naming it. Empty only for read_guide itself.
	Guide string
	run   func(call *toolCall) (any, error)
}

// Arg is one tool argument; the tool's JSON Schema is built from these.
type Arg struct {
	Name        string
	Type        string // string | integer | array
	Description string
	Required    bool
	Format      string // "uuid"
	Enum        []string
	// Synonyms maps other words for an Enum value onto it (keys as enumKey
	// writes them), so "successful" filters as "passed" instead of matching
	// nothing.
	Synonyms  map[string]string
	MaxLength int
	Min, Max  int // integer bounds, both inclusive
	Default   int // integer default when absent
	// Raw sends a string argument as written, with its line breaks, instead of
	// cleaned to one line: create_model's query. A Raw argument with a control
	// or invisible formatting character is refused rather than cleaned, so what
	// runs is what a person reading the call sees.
	Raw bool
	// Items are the fields of each object in an array argument, as string and
	// integer Args; MaxItems is how many objects it may hold.
	Items    []Arg
	MaxItems int
}

const (
	maxConnections  = 200
	maxConnectors   = 300
	maxColumns      = 300
	maxSelectedList = 500
	maxNamedTypes   = 30
)

// ---- outputs ----------------------------------------------------------------

type listConnectionsOut struct {
	Connections []connectionOut `json:"connections"`
	// Total is how many connections matched, before the maxConnections cap.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// Note says why a filtered list is empty, naming the connector types the
	// workspace does have, so "none" is never a guess.
	Note string `json:"note,omitempty"`
}

type connectionOut struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Slug             string     `json:"slug,omitempty"`
	Type             string     `json:"type"`
	ConnectorType    string     `json:"connector_type"`
	ConnectorVersion string     `json:"connector_version,omitempty"`
	Status           string     `json:"status"`
	IsConnected      bool       `json:"is_connected"`
	IsExpired        bool       `json:"is_expired"`
	LastTestStatus   string     `json:"last_test_status,omitempty"`
	LastTestedAt     *time.Time `json:"last_tested_at,omitempty"`
	// SQLDialect is the SQL the connection is queried in, the dialect
	// generate_sql writes for it; empty for a connection with no SQL.
	SQLDialect string `json:"sql_dialect,omitempty"`
}

type schemaOut struct {
	ConnectionID    string          `json:"connection_id"`
	LastRefreshedAt *time.Time      `json:"last_refreshed_at,omitempty"`
	TablesMatched   int             `json:"tables_matched"`
	Truncated       bool            `json:"truncated"`
	Tables          []tableOut      `json:"tables"`
	ForeignKeys     []foreignKeyOut `json:"foreign_keys"`
}

type tableOut struct {
	Schema           string      `json:"schema,omitempty"`
	Name             string      `json:"name"`
	PrimaryKey       []string    `json:"primary_key,omitempty"`
	Columns          []columnOut `json:"columns"`
	ColumnsTruncated bool        `json:"columns_truncated,omitempty"`
}

type columnOut struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	IsPrimaryKey bool   `json:"is_primary_key,omitempty"`
	IsNullable   bool   `json:"is_nullable,omitempty"`
}

type foreignKeyOut struct {
	FromSchema string `json:"from_schema,omitempty"`
	FromTable  string `json:"from_table"`
	FromColumn string `json:"from_column"`
	ToSchema   string `json:"to_schema,omitempty"`
	ToTable    string `json:"to_table"`
	ToColumn   string `json:"to_column"`
	// Inferred marks a relationship guessed from names, not a declared
	// constraint.
	Inferred bool `json:"inferred,omitempty"`
}

type listPipelinesOut struct {
	Pipelines  []pipelineItemOut `json:"pipelines"`
	Pagination paginationOut     `json:"pagination"`
}

type paginationOut struct {
	Page       int  `json:"page"`
	PerPage    int  `json:"per_page"`
	Total      int  `json:"total"`
	TotalPages int  `json:"total_pages"`
	HasNext    bool `json:"has_next"`
	HasPrev    bool `json:"has_prev"`
}

// pipelineItemOut is one row of a list, so it has no description: that is a
// row's longest field, and a page of rows has to fit what a model reads at
// once. get_pipeline returns it.
type pipelineItemOut struct {
	ID             string                           `json:"id"`
	Name           string                           `json:"name"`
	State          string                           `json:"state"`
	PipelineStatus string                           `json:"pipeline_status"`
	SyncMode       string                           `json:"sync_mode,omitempty"`
	CDCMode        string                           `json:"cdc_mode,omitempty"`
	Source         *connRefOut                      `json:"source,omitempty"`
	Destination    *connRefOut                      `json:"destination,omitempty"`
	Schedule       *handlers.PipelineHealthSchedule `json:"schedule,omitempty"`
	LastRun        *runRefOut                       `json:"last_run,omitempty"`
	CreatedAt      time.Time                        `json:"created_at"`
	UpdatedAt      time.Time                        `json:"updated_at"`
}

type connRefOut struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ConnectorType string `json:"connector_type"`
}

type runRefOut struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ErrorClass  string     `json:"error_class,omitempty"`
}

type pipelineOut struct {
	ID                      string       `json:"id"`
	Name                    string       `json:"name"`
	Description             string       `json:"description,omitempty"`
	Status                  string       `json:"status"`
	SyncMode                string       `json:"sync_mode,omitempty"`
	CDCMode                 string       `json:"cdc_mode,omitempty"`
	CDCInitialLoad          string       `json:"cdc_initial_load,omitempty"`
	DefaultRunMode          string       `json:"default_run_mode,omitempty"`
	Source                  *connRefOut  `json:"source,omitempty"`
	Destination             *connRefOut  `json:"destination,omitempty"`
	SelectedTables          []string     `json:"selected_tables,omitempty"`
	SelectedTablesTruncated bool         `json:"selected_tables_truncated,omitempty"`
	Strategy                *strategyOut `json:"data_loading_strategy,omitempty"`
	CreatedAt               time.Time    `json:"created_at"`
	UpdatedAt               time.Time    `json:"updated_at"`
}

type strategyOut struct {
	Mode              string `json:"mode,omitempty"`
	InitialLoad       string `json:"initial_load,omitempty"`
	OngoingSync       string `json:"ongoing_sync,omitempty"`
	RerunDefault      string `json:"rerun_default,omitempty"`
	ScheduleType      string `json:"schedule_type,omitempty"`
	ScheduleStatus    string `json:"schedule_status,omitempty"`
	EffectiveCDCMode  string `json:"effective_cdc_mode,omitempty"`
	EffectiveSyncMode string `json:"effective_sync_mode,omitempty"`
}

type runOut struct {
	RunID         string     `json:"run_id"`
	PipelineID    string     `json:"pipeline_id"`
	PipelineName  string     `json:"pipeline_name,omitempty"`
	Status        string     `json:"status"`
	Outcome       string     `json:"outcome,omitempty"`
	TriggerSource string     `json:"trigger_source,omitempty"`
	LiveStream    bool       `json:"live_stream,omitempty"`
	ScheduledTime *time.Time `json:"scheduled_time,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	ErrorClass    string     `json:"error_class,omitempty"`
}

type diagnosisOut struct {
	RunID      string `json:"run_id"`
	PipelineID string `json:"pipeline_id"`
	Status     string `json:"status"`
	ErrorClass string `json:"error_class,omitempty"`
	// Message is the run's error text after llmscrub has masked quoted
	// literals, hosts, emails and credentials, and maskNumbers every number:
	// llmscrub keeps a short number, and in a run's error that is a row count
	// ("read=48213 landed=48100") or a key value ("Key (id)=(1234)").
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
	// Retryable is false when rsync.ai marked the error as one that fails the
	// same way again; absent when it did not say.
	Retryable *bool `json:"retryable,omitempty"`
}

type listRunsOut struct {
	Runs []runOut `json:"runs"`
	// Total counts every run that matches the filters, not only those listed.
	Total     int          `json:"total"`
	ByOutcome runCountsOut `json:"by_outcome"`
	Truncated bool         `json:"truncated"`
}

// runCountsOut is how many runs ended each way. With a pipeline_id it counts
// that pipeline's runs; it ignores the status filter, so one call answers
// "how many failed, out of how many?".
type runCountsOut struct {
	Succeeded      int `json:"succeeded"`
	Failed         int `json:"failed"`
	Cancelled      int `json:"cancelled"`
	Running        int `json:"running"`
	Pending        int `json:"pending"`
	WaitingForUser int `json:"waiting_for_user"`
	Other          int `json:"other"`
}

type listConnectorsOut struct {
	Connectors []connectorOut `json:"connectors"`
	Total      int            `json:"total"`
	Truncated  bool           `json:"truncated"`
}

type connectorOut struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Version     string `json:"version,omitempty"`
	// Origin is built_in for a connector rsync.ai ships, generated for one the
	// connector generator made.
	Origin              string `json:"origin,omitempty"`
	Lifecycle           string `json:"lifecycle,omitempty"`
	SupportsSource      bool   `json:"supports_source"`
	SupportsDestination bool   `json:"supports_destination"`
	SupportsCDC         bool   `json:"supports_cdc"`
}

// ---- wire shapes (only the fields a tool reads) -----------------------------

type wireExecution struct {
	ID            string     `json:"id"`
	PipelineID    string     `json:"pipeline_id"`
	PipelineName  string     `json:"pipeline_name"`
	Status        string     `json:"status"`
	StartTime     *time.Time `json:"start_time"`
	EndTime       *time.Time `json:"end_time"`
	ScheduledTime *time.Time `json:"scheduled_time"`
	TriggerSource string     `json:"trigger_source"`
	LiveStream    bool       `json:"live_stream"`
	ErrorMessage  string     `json:"error_message"`
}

type wireRunHistory struct {
	ID            string     `json:"id"`
	PipelineID    string     `json:"pipeline_id"`
	PipelineName  string     `json:"pipeline_name"`
	Status        string     `json:"status"`
	Outcome       string     `json:"outcome"`
	TriggerSource string     `json:"trigger_source"`
	LiveStream    bool       `json:"live_stream"`
	ScheduledTime *time.Time `json:"scheduled_time"`
	StartTime     *time.Time `json:"start_time"`
	EndTime       *time.Time `json:"end_time"`
	ErrorClass    string     `json:"error_class"`
}

type wireConnector struct {
	Name                string `json:"name"`
	DisplayName         string `json:"display_name"`
	Description         string `json:"description"`
	Category            string `json:"category"`
	Version             string `json:"version"`
	Source              string `json:"source"`
	Lifecycle           string `json:"lifecycle"`
	SupportsSource      bool   `json:"supports_source"`
	SupportsDestination bool   `json:"supports_destination"`
	SupportsCDC         bool   `json:"supports_cdc"`
}

type wireSchedule struct {
	ScheduleType string `json:"schedule_type"`
	Status       string `json:"status"`
	Timezone     string `json:"timezone"`
	Spec         struct {
		Cron         string  `json:"cron"`
		EverySeconds float64 `json:"every_seconds"`
	} `json:"schedule_spec"`
}

type wireRun struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	ErrorMessage string     `json:"error_message"`
}

type generationOut struct {
	RunID            string     `json:"run_id"`
	ConnectorName    string     `json:"connector_name"`
	Status           string     `json:"status"`
	SourceURL        string     `json:"source_url,omitempty"`
	Version          string     `json:"version,omitempty"`
	ValidationPassed *bool      `json:"validation_passed,omitempty"`
	ToolCount        *int       `json:"tool_count,omitempty"`
	Error            string     `json:"error,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	// Next is a fixed sentence: what to do now the run has this status.
	Next string `json:"next,omitempty"`
}

// madeResourceID is the run generate_connector started, for its audit row.
func (g generationOut) madeResourceID() string { return g.RunID }

func shapeGeneration(r handlers.GenerationRun) generationOut {
	out := generationOut{
		RunID:            ident(r.ID),
		ConnectorName:    ident(r.ConnectorName),
		Status:           ident(r.Status),
		SourceURL:        r.SourceURL,
		Version:          ident(r.Version),
		ValidationPassed: r.ValidationPassed,
		ToolCount:        r.ToolCount,
		Error:            llmscrub.Scrub(r.Error),
		StartedAt:        r.StartedAt,
		FinishedAt:       r.FinishedAt,
	}
	switch out.Status {
	case "running":
		out.Next = "Generation runs in the background, usually for 1-3 minutes and at most 15. " +
			"Check it with get_connector_generation."
	case "succeeded":
		out.Next = "The connector is in the catalog and pipelines can use it. A person adds its credentials " +
			"on the connection form in rsync.ai; never ask for them here."
	case "failed":
		out.Next = "Nothing was added to the catalog. Check the URL points at the API's OpenAPI document or docs, then try again."
	}
	return out
}

// ---- registry ---------------------------------------------------------------

var (
	argPipelineID = Arg{Name: "pipeline_id", Type: "string", Format: "uuid", Required: true,
		Description: "The pipeline's id, from list_pipelines."}
	argRunID = Arg{Name: "run_id", Type: "string", Format: "uuid", Required: true,
		Description: "The run's id: last_run.id from list_pipelines or get_pipeline_health, or runs[].run_id from list_runs."}

	// The values handlers.ListPipelines' derived_status takes.
	pipelineStates   = []string{"running", "passed", "failed", "stopped", "idle", "paused", "waiting_for_user"}
	pipelineSynonyms = map[string]string{
		"success": "passed", "succeeded": "passed", "successful": "passed", "completed": "passed",
		"complete": "passed", "ok": "passed", "passing": "passed",
		"error": "failed", "errored": "failed", "failing": "failed", "failure": "failed", "broken": "failed",
		"streaming": "running", "in_progress": "running", "processing": "running", "live": "running",
		"cancelled": "stopped", "canceled": "stopped",
		"waiting": "waiting_for_user", "needs_input": "waiting_for_user",
	}
	// The outcomes handlers.ListRunHistory counts.
	runOutcomes     = []string{"succeeded", "failed", "cancelled", "running", "pending", "waiting_for_user"}
	outcomeSynonyms = map[string]string{
		"success": "succeeded", "successful": "succeeded", "completed": "succeeded", "complete": "succeeded",
		"passed": "succeeded", "ok": "succeeded",
		"error": "failed", "errored": "failed", "failure": "failed",
		"canceled": "cancelled", "stopped": "cancelled",
		"processing": "running", "in_progress": "running",
		"queued":  "pending",
		"waiting": "waiting_for_user",
	}
	connectionTypeSynonyms = map[string]string{
		"sources": "source", "src": "source",
		"destinations": "destination", "dest": "destination", "target": "destination", "targets": "destination",
		"sink": "destination", "sinks": "destination",
	}
)

// registry builds the tools. The reads need the metadata:read scope. The
// writes each need their own scope: generate_connector connectors:generate,
// the model writes (models.go) models:write and plan_workflow (workflows.go)
// workflows:write, each with an admin; plan_pipeline (plans.go)
// pipelines:plan, with a member. None returns a row value, a sample, a row
// count, a query's text or a credential.
func registry(h Handlers) []*Tool {
	tools := []*Tool{
		{
			Name:  "list_connections",
			Title: "List connections",
			Description: "Lists the connections (sources and destinations) in the workspace: name, connector type, status, " +
				"whether the last connection test passed and, for one queried with SQL, its SQL dialect, with the total. " +
				"Never returns connection settings or credentials. Rules: read_guide('connectors').",
			Guide: "connectors",
			Args: []Arg{
				{Name: "connector_type", Type: "string", MaxLength: 64, Description: "Only connections of this connector, by its id or " +
					"the name the UI shows: postgresql, postgres, gcs, Google Cloud Storage, Amazon S3, BigQuery."},
				{Name: "type", Type: "string", Enum: []string{"source", "destination"}, Synonyms: connectionTypeSynonyms,
					Description: "Only sources or only destinations."},
			},
			Action:       services.ActionConnectionRead,
			ResourceType: "connection",
			Output:       listConnectionsOut{},
			run: func(call *toolCall) (any, error) {
				// connector_type is matched here, not by the handler, whose
				// filter is an exact match on the stored id: "Google Cloud
				// Storage" or "aws s3" would find nothing there.
				q := url.Values{}
				call.setQuery(q, "type", "type")
				var body struct {
					Connections []connectionOut `json:"connections"`
				}
				if err := call.get(h.ListConnections, nil, q, &body); err != nil {
					return nil, err
				}
				want := connectorKeys(call.str("connector_type"))
				out := listConnectionsOut{Connections: []connectionOut{}}
				present := map[string]bool{}
				for _, cn := range body.Connections {
					cn.ConnectorType = ident(cn.ConnectorType)
					if cn.ConnectorType != "" {
						present[cn.ConnectorType] = true
					}
					if len(want) > 0 && !want[handlers.ConnectorKey(cn.ConnectorType)] {
						continue
					}
					out.Total++
					if len(out.Connections) == maxConnections {
						out.Truncated = true
						continue
					}
					cn.Type = ident(cn.Type)
					cn.ConnectorVersion = ident(cn.ConnectorVersion)
					cn.Status = ident(cn.Status)
					cn.LastTestStatus = ident(cn.LastTestStatus)
					cn.SQLDialect = handlers.SQLDialect(cn.ConnectorType)
					out.Connections = append(out.Connections, cn)
				}
				if len(want) > 0 && out.Total == 0 {
					out.Note = noMatchNote(present)
				}
				return out, nil
			},
		},
		{
			Name:  "describe_schema",
			Title: "Describe a connection's schema",
			Description: "Lists the tables of a connection with their columns, column types, primary keys and foreign " +
				"keys, from the schema index the Explorer uses. Names only: no row values, samples or row counts. " +
				"Rules: read_guide('connectors').",
			Guide: "connectors",
			Args: []Arg{
				{Name: "connection_id", Type: "string", Format: "uuid", Required: true, Description: "The connection's id, from list_connections."},
				{Name: "name_contains", Type: "string", MaxLength: 100, Description: "Only tables whose schema.table name contains this text (case-insensitive)."},
				{Name: "max_tables", Type: "integer", Min: 1, Max: 200, Default: 50, Description: "At most this many tables (1-200, default 50)."},
			},
			Action:       services.ActionConnectionRead,
			ResourceType: "connection",
			ResourceArg:  "connection_id",
			Output:       schemaOut{},
			run: func(call *toolCall) (any, error) {
				return describeSchema(call, h.GetSchemaIndex)
			},
		},
		{
			Name:  "list_pipelines",
			Title: "List pipelines",
			Description: "Lists the workspace's pipelines with their state, source and destination, schedule and last run. " +
				"Count from pagination.total (every page), not the rows. A failed run carries an error class, not " +
				"its text: diagnose_run has that. Rules: read_guide('pipelines').",
			Guide: "pipelines",
			Args: []Arg{
				{Name: "query", Type: "string", MaxLength: 100, Description: "Only pipelines whose name contains this text."},
				{Name: "status", Type: "string", Enum: pipelineStates, Synonyms: pipelineSynonyms,
					Description: "Only pipelines in this state. passed means the last run succeeded; a pipeline's state is its last run, not its history (use list_runs)."},
				{Name: "type", Type: "string", Enum: []string{"batch", "cdc", "etl"}, Description: "Only pipelines of this type."},
				{Name: "page", Type: "integer", Min: 1, Max: 1000, Default: 1, Description: "Page number, from 1."},
				{Name: "per_page", Type: "integer", Min: 1, Max: 100, Default: 25, Description: "Pipelines per page (1-100, default 25)."},
			},
			Action:       services.ActionPipelineRead,
			ResourceType: "pipeline",
			Output:       listPipelinesOut{},
			run: func(call *toolCall) (any, error) {
				return listPipelines(call, h.ListPipelines)
			},
		},
		{
			Name:  "get_pipeline",
			Title: "Get a pipeline",
			Description: "Returns one pipeline's definition: source and destination, sync and CDC mode, selected tables " +
				"and loading strategy. Rules: read_guide('pipelines').",
			Guide:        "pipelines",
			Args:         []Arg{argPipelineID},
			Action:       services.ActionPipelineRead,
			ResourceType: "pipeline",
			ResourceArg:  "pipeline_id",
			Output:       pipelineOut{},
			run: func(call *toolCall) (any, error) {
				return getPipeline(call, h.GetPipeline)
			},
		},
		{
			Name:  "get_pipeline_health",
			Title: "Get a pipeline's health",
			Description: `Answers "why is this pipeline stale?": its state, the one thing blocking it (waiting for you, ` +
				"paused, failed with an error class, never run, no schedule…) with a fixed hint, the last run, " +
				"the last success and the schedule. Rules: read_guide('pipelines').",
			Guide:        "pipelines",
			Args:         []Arg{argPipelineID},
			Action:       services.ActionPipelineRead,
			ResourceType: "pipeline",
			ResourceArg:  "pipeline_id",
			Output:       handlers.PipelineHealth{},
			run: func(call *toolCall) (any, error) {
				var out handlers.PipelineHealth
				if err := call.get(h.GetPipelineHealth, gin.Params{{Key: "id", Value: call.str("pipeline_id")}}, nil, &out); err != nil {
					return nil, err
				}
				return shapeHealth(out), nil
			},
		},
		{
			Name:  "get_run_status",
			Title: "Get a run's status",
			Description: "Returns one run's status and outcome, trigger, start and end times, and its error class if it " +
				"failed. Rules: read_guide('pipelines').",
			Guide:        "pipelines",
			Args:         []Arg{argRunID},
			Action:       services.ActionRunRead,
			ResourceType: "execution",
			ResourceArg:  "run_id",
			Output:       runOut{},
			run: func(call *toolCall) (any, error) {
				e, err := getExecution(call, h.GetExecution)
				if err != nil {
					return nil, err
				}
				status := ident(e.Status)
				return runOut{
					RunID:         e.ID,
					PipelineID:    e.PipelineID,
					PipelineName:  e.PipelineName,
					Status:        status,
					Outcome:       handlers.RunOutcome(status),
					TriggerSource: ident(e.TriggerSource),
					LiveStream:    e.LiveStream,
					ScheduledTime: e.ScheduledTime,
					StartedAt:     e.StartTime,
					EndedAt:       e.EndTime,
					ErrorClass:    handlers.RunErrorClass(status, e.ErrorMessage),
				}, nil
			},
		},
		{
			Name:  "diagnose_run",
			Title: "Diagnose a failed run",
			Description: "Explains why a run failed: its error class, a fixed hint for that class, and the error message " +
				"with quoted values, numbers, hosts, emails and credentials masked. Calls no model. Rules: " +
				"read_guide('pipelines').",
			Guide:        "pipelines",
			Args:         []Arg{argRunID},
			Action:       services.ActionRunRead,
			ResourceType: "execution",
			ResourceArg:  "run_id",
			Output:       diagnosisOut{},
			run: func(call *toolCall) (any, error) {
				e, err := getExecution(call, h.GetExecution)
				if err != nil {
					return nil, err
				}
				status := ident(e.Status)
				out := diagnosisOut{RunID: e.ID, PipelineID: e.PipelineID, Status: status}
				if class := handlers.RunErrorClass(status, e.ErrorMessage); class != "" {
					out.ErrorClass = class
					out.Hint = handlers.RunErrorHint(class)
					out.Message = maskNumbers(llmscrub.Scrub(e.ErrorMessage))
					if notRetryableRe.MatchString(e.ErrorMessage) {
						no := false
						out.Retryable = &no
						out.Hint += " rsync.ai marked this error as not retryable: running it again unchanged fails the same way."
					}
				}
				return out, nil
			},
		},
		{
			Name:  "list_runs",
			Title: "List runs",
			Description: "Run history: how many runs succeeded, failed, were cancelled or are running (by_outcome, over " +
				"every run, not just those listed) and the newest runs with their outcome and error class. Use it " +
				`for "has it ever failed?"; a pipeline list shows only the last run. Rules: ` +
				"read_guide('pipelines').",
			Guide: "pipelines",
			Args: []Arg{
				{Name: "pipeline_id", Type: "string", Format: "uuid", Description: "Only this pipeline's runs; its id, from list_pipelines."},
				{Name: "status", Type: "string", Enum: runOutcomes, Synonyms: outcomeSynonyms,
					Description: "Only runs that ended this way. by_outcome still counts every run."},
				{Name: "limit", Type: "integer", Min: 1, Max: 100, Default: 25, Description: "At most this many runs, newest first (1-100, default 25)."},
			},
			Action:       services.ActionRunRead,
			ResourceType: "execution",
			Output:       listRunsOut{},
			run: func(call *toolCall) (any, error) {
				return listRuns(call, h.ListRuns)
			},
		},
		{
			Name:  "list_connectors",
			Title: "List connectors",
			Description: "The connector catalog: every connector rsync.ai can connect to (built in, or made with " +
				"Connectors → Generate New), and whether it works as a source, a destination or for CDC. Use it " +
				`for "does rsync.ai support X?"; list_connections has only this workspace's. Rules: ` +
				"read_guide('connectors').",
			Guide: "connectors",
			Args: []Arg{
				{Name: "name_contains", Type: "string", MaxLength: 64, Description: "Only connectors whose id or name contains this text (case-insensitive)."},
				{Name: "supports", Type: "string", Enum: []string{"source", "destination", "cdc"},
					Synonyms: map[string]string{"sources": "source", "destinations": "destination", "target": "destination",
						"change_data_capture": "cdc", "streaming": "cdc"},
					Description: "Only connectors that work as a source, a destination, or for CDC."},
			},
			Action:       services.ActionConnectionRead,
			ResourceType: "connector",
			Output:       listConnectorsOut{},
			run: func(call *toolCall) (any, error) {
				return listConnectors(call, h.ListConnectors)
			},
		},
		{
			Name:  "generate_connector",
			Title: "Generate a connector",
			Description: "Generates a connector from a public API's OpenAPI or Swagger document (spec_url) or docs page " +
				"(docs_url) and adds it to the catalog. Starts a background run and returns its run_id; follow it " +
				"with get_connector_generation. Rules: read_guide('connectors').",
			Guide: "connectors",
			Args: []Arg{
				{Name: "name", Type: "string", Required: true, MaxLength: 100,
					Description: "The new connector's id, lowercase kebab-case, such as acme-crm."},
				{Name: "spec_url", Type: "string", MaxLength: 2048,
					Description: "https URL of the API's OpenAPI or Swagger document (JSON or YAML). Give this or docs_url."},
				{Name: "docs_url", Type: "string", MaxLength: 2048,
					Description: "https URL of the API's documentation page, when it has no OpenAPI document. Give this or spec_url."},
				{Name: "description", Type: "string", MaxLength: 500, Description: "One line on what the API is for."},
			},
			Action:       services.ActionConnectorGenerate,
			Write:        true,
			OpenWorld:    true,
			ResourceType: "connector_generation",
			Output:       generationOut{},
			run: func(call *toolCall) (any, error) {
				if h.StartConnectorGeneration == nil {
					return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
						message: "Connector generation is not available on this rsync.ai."}
				}
				run, err := h.StartConnectorGeneration(call.c, call.s.opts.Actor(call.c), handlers.GenerationInput{
					Name:        call.str("name"),
					SpecURL:     call.str("spec_url"),
					DocsURL:     call.str("docs_url"),
					Description: call.str("description"),
				})
				var refusal *handlers.GenerationRefusal
				if errors.As(err, &refusal) {
					return nil, &toolError{status: refusal.Status, outcome: refusal.Code, message: refusal.Message}
				}
				if err != nil {
					return nil, err
				}
				return shapeGeneration(*run), nil
			},
		},
		{
			Name:  "get_connector_generation",
			Title: "Get a connector generation",
			Description: "Returns one connector generation run of this workspace: running, succeeded (with the version and " +
				"how many tools the connector has) or failed (with the reason). Rules: read_guide('connectors').",
			Guide: "connectors",
			Args: []Arg{
				{Name: "run_id", Type: "string", Format: "uuid", Required: true, Description: "The run_id generate_connector returned."},
			},
			Action:       services.ActionConnectionRead,
			ResourceType: "connector_generation",
			ResourceArg:  "run_id",
			Output:       generationOut{},
			run: func(call *toolCall) (any, error) {
				var run handlers.GenerationRun
				if err := call.get(h.GetConnectorGeneration, gin.Params{{Key: "id", Value: call.str("run_id")}}, nil, &run); err != nil {
					return nil, err
				}
				return shapeGeneration(run), nil
			},
		},
	}
	tools = append(tools, modelTools(h)...)
	tools = append(tools, modelCheckTools(h)...)
	tools = append(tools, planTools(h)...)
	tools = append(tools, workflowTools(h)...)
	tools = append(tools, dashboardTools(h)...)
	tools = append(tools, usageTools(h)...)
	return append(tools, guideTools()...)
}

// notRetryableRe is how a run's error says rsync.ai will not retry it: the
// executor's [DETERMINISTIC:…] prefix, or Temporal's "retryable: false".
var notRetryableRe = regexp.MustCompile(`(?i)\[deterministic\b|retryable: false`)

// connectorKeys are the keys a connector_type argument can mean: the whole
// text, and for "Google Cloud Storage (GCS)" the name and the part in
// parentheses. Empty for no argument.
func connectorKeys(s string) map[string]bool {
	keys := map[string]bool{}
	add := func(v string) {
		if k := handlers.ConnectorKey(v); k != "" {
			keys[k] = true
		}
	}
	if s = strings.TrimSpace(s); s == "" {
		return keys
	}
	add(s)
	if before, inner, ok := strings.Cut(s, "("); ok {
		add(before)
		inner, _, _ = strings.Cut(inner, ")")
		add(inner)
	}
	return keys
}

func noMatchNote(present map[string]bool) string {
	if len(present) == 0 {
		return "No connection matches: this workspace has no connections."
	}
	types := make([]string, 0, len(present))
	for t := range present {
		types = append(types, t)
	}
	sort.Strings(types)
	if len(types) > maxNamedTypes {
		types = types[:maxNamedTypes]
	}
	return "No connection matches that connector type. This workspace's connections are of these connector types: " +
		strings.Join(types, ", ") + "."
}

func listRuns(call *toolCall, h gin.HandlerFunc) (any, error) {
	q := url.Values{}
	call.setQuery(q, "pipeline_id", "pipeline_id")
	call.setQuery(q, "status", "outcome")
	limit := call.num("limit")
	q.Set("limit", strconv.Itoa(limit))
	var body struct {
		Runs      []wireRunHistory `json:"runs"`
		Total     int              `json:"total"`
		ByOutcome runCountsOut     `json:"by_outcome"`
	}
	if err := call.get(h, nil, q, &body); err != nil {
		return nil, err
	}
	out := listRunsOut{Runs: []runOut{}, Total: body.Total, ByOutcome: body.ByOutcome}
	for _, r := range body.Runs {
		if len(out.Runs) == limit {
			break
		}
		out.Runs = append(out.Runs, runOut{
			RunID:         ident(r.ID),
			PipelineID:    ident(r.PipelineID),
			PipelineName:  r.PipelineName,
			Status:        ident(r.Status),
			Outcome:       ident(r.Outcome),
			TriggerSource: ident(r.TriggerSource),
			LiveStream:    r.LiveStream,
			ScheduledTime: r.ScheduledTime,
			StartedAt:     r.StartTime,
			EndedAt:       r.EndTime,
			ErrorClass:    ident(r.ErrorClass),
		})
	}
	out.Truncated = out.Total > len(out.Runs)
	return out, nil
}

func listConnectors(call *toolCall, h gin.HandlerFunc) (any, error) {
	var body struct {
		Connectors []wireConnector `json:"connectors"`
	}
	if err := call.get(h, nil, nil, &body); err != nil {
		return nil, err
	}
	needle := strings.ToLower(call.str("name_contains"))
	supports := call.str("supports")
	out := listConnectorsOut{Connectors: []connectorOut{}}
	for _, cn := range body.Connectors {
		if needle != "" && !strings.Contains(strings.ToLower(cn.Name), needle) &&
			!strings.Contains(strings.ToLower(cn.DisplayName), needle) {
			continue
		}
		if (supports == "source" && !cn.SupportsSource) || (supports == "destination" && !cn.SupportsDestination) ||
			(supports == "cdc" && !cn.SupportsCDC) {
			continue
		}
		out.Total++
		if len(out.Connectors) == maxConnectors {
			out.Truncated = true
			continue
		}
		out.Connectors = append(out.Connectors, connectorOut{
			Name:                ident(cn.Name),
			DisplayName:         cn.DisplayName,
			Description:         cn.Description,
			Category:            ident(cn.Category),
			Version:             ident(cn.Version),
			Origin:              ident(cn.Source),
			Lifecycle:           ident(cn.Lifecycle),
			SupportsSource:      cn.SupportsSource,
			SupportsDestination: cn.SupportsDestination,
			SupportsCDC:         cn.SupportsCDC,
		})
	}
	return out, nil
}

var digitsRe = regexp.MustCompile(`[0-9]+`)

// maskNumbers replaces every run of digits. Applied after llmscrub, whose
// markers carry none.
func maskNumbers(s string) string { return digitsRe.ReplaceAllString(s, "[num-redacted]") }

// shapeHealth keeps each enum-like field of a health read only when it has
// its field's shape, the same check every other tool applies to a status.
// The fields the handler builds from fixed tables (hints) pass as they are.
func shapeHealth(h handlers.PipelineHealth) handlers.PipelineHealth {
	h.Mode, h.State = ident(h.Mode), ident(h.State)
	if b := h.Blocking; b != nil {
		b.Kind, b.ReasonType, b.ErrorClass = ident(b.Kind), handlers.SafeCode(b.ReasonType), ident(b.ErrorClass)
	}
	if r := h.LastRun; r != nil {
		r.ID, r.Status, r.ErrorClass = ident(r.ID), ident(r.Status), ident(r.ErrorClass)
	}
	if sc := h.Schedule; sc != nil {
		sc.Type, sc.Status = ident(sc.Type), ident(sc.Status)
		sc.Cron, sc.Timezone = handlers.SafeCron(sc.Cron), handlers.SafeTimezone(sc.Timezone)
	}
	return h
}

func describeSchema(call *toolCall, h gin.HandlerFunc) (any, error) {
	var body struct {
		ConnectionID    string    `json:"connection_id"`
		LastRefreshedAt time.Time `json:"last_refreshed_at"`
		Tables          []struct {
			Name       string      `json:"name"`
			Schema     string      `json:"schema"`
			Columns    []columnOut `json:"columns"`
			PrimaryKey []string    `json:"primary_key"`
		} `json:"tables"`
		ForeignKeys []struct {
			foreignKeyOut
			Confidence float64 `json:"confidence"`
		} `json:"foreign_keys"`
	}
	id := call.str("connection_id")
	if err := call.get(h, gin.Params{{Key: "id", Value: id}}, nil, &body); err != nil {
		return nil, err
	}
	out := schemaOut{ConnectionID: id, Tables: []tableOut{}, ForeignKeys: []foreignKeyOut{}}
	if !body.LastRefreshedAt.IsZero() {
		t := body.LastRefreshedAt.UTC()
		out.LastRefreshedAt = &t
	}
	needle := strings.ToLower(call.str("name_contains"))
	limit := call.num("max_tables")
	type key struct{ schema, table string }
	kept := map[key]bool{}
	names := map[string]bool{}
	for _, t := range body.Tables {
		if needle != "" && !strings.Contains(strings.ToLower(t.Schema+"."+t.Name), needle) {
			continue
		}
		out.TablesMatched++
		if len(out.Tables) == limit {
			out.Truncated = true
			continue
		}
		tbl := tableOut{Schema: t.Schema, Name: t.Name, PrimaryKey: t.PrimaryKey, Columns: t.Columns}
		if tbl.Columns == nil {
			tbl.Columns = []columnOut{}
		}
		if len(tbl.Columns) > maxColumns {
			tbl.Columns, tbl.ColumnsTruncated = tbl.Columns[:maxColumns], true
		}
		out.Tables = append(out.Tables, tbl)
		kept[key{t.Schema, t.Name}] = true
		names[t.Name] = true
	}
	has := func(schema, table string) bool {
		if schema == "" {
			return names[table]
		}
		return kept[key{schema, table}]
	}
	for _, fk := range body.ForeignKeys {
		if !has(fk.FromSchema, fk.FromTable) || !has(fk.ToSchema, fk.ToTable) {
			continue
		}
		f := fk.foreignKeyOut
		f.Inferred = fk.Confidence > 0 && fk.Confidence < 1
		out.ForeignKeys = append(out.ForeignKeys, f)
	}
	return out, nil
}

func listPipelines(call *toolCall, h gin.HandlerFunc) (any, error) {
	q := url.Values{}
	call.setQuery(q, "query", "q")
	call.setQuery(q, "status", "status")
	call.setQuery(q, "type", "type")
	q.Set("page", strconv.Itoa(call.num("page")))
	q.Set("per_page", strconv.Itoa(call.num("per_page")))
	var body struct {
		Pipelines []struct {
			ID                    string        `json:"id"`
			Name                  string        `json:"name"`
			Description           string        `json:"description"`
			PipelineStatus        string        `json:"pipeline_status"`
			DerivedStatus         string        `json:"derived_status"`
			SyncMode              *string       `json:"sync_mode"`
			CDCMode               *string       `json:"cdc_mode"`
			SourceConnection      *connRefOut   `json:"source_connection"`
			DestinationConnection *connRefOut   `json:"destination_connection"`
			Schedule              *wireSchedule `json:"schedule"`
			LastExecution         *wireRun      `json:"last_execution"`
			CreatedAt             time.Time     `json:"created_at"`
			UpdatedAt             time.Time     `json:"updated_at"`
		} `json:"pipelines"`
		Pagination paginationOut `json:"pagination"`
	}
	if err := call.get(h, nil, q, &body); err != nil {
		return nil, err
	}
	out := listPipelinesOut{Pipelines: []pipelineItemOut{}, Pagination: body.Pagination}
	for _, p := range body.Pipelines {
		item := pipelineItemOut{
			ID:             p.ID,
			Name:           p.Name,
			State:          ident(p.DerivedStatus),
			PipelineStatus: ident(p.PipelineStatus),
			SyncMode:       identPtr(p.SyncMode),
			CDCMode:        identPtr(p.CDCMode),
			Source:         connRef(p.SourceConnection),
			Destination:    connRef(p.DestinationConnection),
			CreatedAt:      p.CreatedAt,
			UpdatedAt:      p.UpdatedAt,
		}
		if s := p.Schedule; s != nil && s.ScheduleType != "" {
			item.Schedule = &handlers.PipelineHealthSchedule{
				Type:     ident(s.ScheduleType),
				Status:   ident(s.Status),
				Cron:     handlers.SafeCron(s.Spec.Cron),
				Timezone: handlers.SafeTimezone(s.Timezone),
			}
			if s.Spec.EverySeconds > 0 {
				item.Schedule.EverySeconds = int64(s.Spec.EverySeconds)
			}
		}
		if r := p.LastExecution; r != nil && r.ID != "" {
			status := ident(r.Status)
			item.LastRun = &runRefOut{
				ID:          r.ID,
				Status:      status,
				StartedAt:   r.StartedAt,
				CompletedAt: r.CompletedAt,
				ErrorClass:  handlers.RunErrorClass(status, r.ErrorMessage),
			}
		}
		out.Pipelines = append(out.Pipelines, item)
	}
	return out, nil
}

func getPipeline(call *toolCall, h gin.HandlerFunc) (any, error) {
	var p struct {
		ID                    string       `json:"id"`
		Name                  string       `json:"name"`
		Description           string       `json:"description"`
		Status                string       `json:"status"`
		SyncMode              *string      `json:"sync_mode"`
		CDCMode               *string      `json:"cdc_mode"`
		CDCInitialLoad        *string      `json:"cdc_initial_load"`
		DefaultRunMode        *string      `json:"default_run_mode"`
		SourceConnection      *connRefOut  `json:"source_connection"`
		DestinationConnection *connRefOut  `json:"destination_connection"`
		SelectedTables        []string     `json:"selected_tables"`
		Strategy              *strategyOut `json:"data_loading_strategy"`
		CreatedAt             time.Time    `json:"created_at"`
		UpdatedAt             time.Time    `json:"updated_at"`
	}
	if err := call.get(h, gin.Params{{Key: "id", Value: call.str("pipeline_id")}}, nil, &p); err != nil {
		return nil, err
	}
	out := pipelineOut{
		ID:             p.ID,
		Name:           p.Name,
		Description:    p.Description,
		Status:         ident(p.Status),
		SyncMode:       identPtr(p.SyncMode),
		CDCMode:        identPtr(p.CDCMode),
		CDCInitialLoad: identPtr(p.CDCInitialLoad),
		DefaultRunMode: identPtr(p.DefaultRunMode),
		Source:         connRef(p.SourceConnection),
		Destination:    connRef(p.DestinationConnection),
		SelectedTables: p.SelectedTables,
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
	}
	if len(out.SelectedTables) > maxSelectedList {
		out.SelectedTables, out.SelectedTablesTruncated = out.SelectedTables[:maxSelectedList], true
	}
	if s := p.Strategy; s != nil {
		out.Strategy = &strategyOut{
			Mode:              ident(s.Mode),
			InitialLoad:       ident(s.InitialLoad),
			OngoingSync:       ident(s.OngoingSync),
			RerunDefault:      ident(s.RerunDefault),
			ScheduleType:      ident(s.ScheduleType),
			ScheduleStatus:    ident(s.ScheduleStatus),
			EffectiveCDCMode:  ident(s.EffectiveCDCMode),
			EffectiveSyncMode: ident(s.EffectiveSyncMode),
		}
	}
	return out, nil
}

func getExecution(call *toolCall, h gin.HandlerFunc) (wireExecution, error) {
	var e wireExecution
	err := call.get(h, gin.Params{{Key: "id", Value: call.str("run_id")}}, nil, &e)
	return e, err
}

func connRef(c *connRefOut) *connRefOut {
	if c == nil || c.ID == "" {
		return nil
	}
	return &connRefOut{ID: c.ID, Name: c.Name, ConnectorType: ident(c.ConnectorType)}
}

func identPtr(s *string) string {
	if s == nil {
		return ""
	}
	return ident(*s)
}
