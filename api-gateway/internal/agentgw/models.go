package agentgw

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/handlers"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

// The model tools: read, create, schedule, run and pause the workspace's models,
// the saved queries a run writes to a table. A silver model cleans one table a
// pipeline writes; any other model is create_model's. A model's layer is one of
// the workspace's own (handlers/workspace_layers.go), by name, or none.
//
// Every tool goes through the same REST handlers the UI calls, so a model made
// here passes the checks a model made there does: a table model's query must only
// read, a run cannot take over a table rsync.ai did not create, and a schedule runs
// as an admin. What an agent cannot do here is the rest: delete a model, change an
// existing model's query, use statement mode, or touch a connection.
//
// No output carries a model's query text, a run's row count or a value: a list of
// models is names, targets, layers, states and times.

const (
	maxModels = 200
	// maxQueryRunes caps the query create_model takes. The handler's own cap is
	// in bytes and larger; this one keeps one tool call to what a model writes.
	maxQueryRunes = 64 * 1024
	// maxIDList fits handlers' 16 upstreams, written as comma-separated uuids.
	maxIDList = 16 * 40
)

// ---- outputs ----------------------------------------------------------------

type modelOut struct {
	ID              string `json:"model_id"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	ConnectionID    string `json:"connection_id"`
	ConnectorType   string `json:"connector_type,omitempty"`
	QueryLanguage   string `json:"query_language,omitempty"`
	Materialization string `json:"materialization"`
	TargetTable     string `json:"target_table,omitempty"`
	Layer           string `json:"layer,omitempty"`
	Visibility      string `json:"visibility"`
	ScheduleStatus  string `json:"schedule_status,omitempty"`
	// NextRunAt is when a clock schedule fires next; nil for a paused one and for
	// one that runs after its upstreams.
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`
	LastRunAt     string     `json:"last_run_at,omitempty"`
	LastRunStatus string     `json:"last_run_status,omitempty"`
	// LastRunError is masked as diagnose_run masks a run's error.
	LastRunError             string `json:"last_run_error,omitempty"`
	FreshnessDeadlineSeconds *int   `json:"freshness_deadline_seconds,omitempty"`
	UpdatedAt                string `json:"updated_at"`
}

type listModelsOut struct {
	Models []modelOut `json:"models"`
	// Total is how many models matched, before the maxModels cap.
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
	// Layers are the workspace's own layers, in order: the names a layer argument
	// takes. Empty when an owner or admin has made none.
	Layers []string `json:"layers"`
}

type getModelOut struct {
	modelOut
	// Schedule is nil when the model has none: it runs only when someone runs it.
	Schedule *modelScheduleOut `json:"schedule,omitempty"`
	// Reads are the tables the model's query names and what writes each; nil,
	// with ReadsNote saying so, when rsync.ai could not look them up.
	Reads     *modelReadsOut `json:"reads,omitempty"`
	ReadsNote string         `json:"reads_note,omitempty"`
}

// modelReadsOut is the schedule dialog's upstream suggestion
// (handlers.SuggestSavedQueryUpstreams), read from the query's text: a table a
// pipeline has written, or a model builds, on the model's own connection.
type modelReadsOut struct {
	// Tables are every table the query names, as it names them.
	Tables []string `json:"tables"`
	// WrittenBy are the pipelines and models that write one of those tables.
	WrittenBy []modelWriterOut `json:"written_by,omitempty"`
	// NoKnownWriter are the tables no pipeline or model here is known to write:
	// loaded some other way, or by a pipeline to object storage.
	NoKnownWriter []string `json:"no_known_writer,omitempty"`
	// Ambiguous is a table named without a schema that matches tables in more
	// than one.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

type modelWriterOut struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Table string `json:"table"`
}

// upstreamSuggestion is the part of handlers.SuggestSavedQueryUpstreams's
// answer get_model passes on.
type upstreamSuggestion struct {
	References []string `json:"references"`
	Unresolved []string `json:"unresolved"`
	Candidates []struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Table string `json:"table"`
	} `json:"candidates"`
	Ambiguous bool `json:"ambiguous"`
}

type modelScheduleOut struct {
	ScheduleID   string `json:"schedule_id"`
	ScheduleType string `json:"schedule_type"`
	Cron         string `json:"cron,omitempty"`
	EverySeconds int    `json:"every_seconds,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	// Upstreams are the pipelines and models an after_upstream schedule waits on.
	Upstreams      []modelUpstreamOut `json:"upstreams,omitempty"`
	UpstreamPolicy string             `json:"upstream_policy,omitempty"`
	Status         string             `json:"status"`
	PausedAt       *time.Time         `json:"paused_at,omitempty"`
	PausedReason   string             `json:"paused_reason,omitempty"`
	// AutoPausedReason says why rsync.ai paused the schedule itself, after runs that
	// kept failing.
	AutoPausedAt     *time.Time `json:"auto_paused_at,omitempty"`
	AutoPausedReason string     `json:"auto_paused_reason,omitempty"`
	// Blocked is an active schedule whose run would be refused, with why.
	Blocked       bool      `json:"blocked,omitempty"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type modelUpstreamOut struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type modelRunOut struct {
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	TriggerSource string `json:"trigger_source"`
	TargetTable   string `json:"target_table,omitempty"`
	// Error is masked as diagnose_run masks a run's error.
	Error           string `json:"error,omitempty"`
	AutoPauseReason string `json:"auto_pause_reason,omitempty"`
	// SkipReason is why a skipped run did not rebuild: upstream_failed,
	// upstream_skipped, upstream_check_failed (a data check held the upstream's table
	// back), waiting_on_upstreams, or an overlap with a run in flight.
	SkipReason     string    `json:"skip_reason,omitempty"`
	UpstreamKind   string    `json:"upstream_kind,omitempty"`
	UpstreamID     string    `json:"upstream_id,omitempty"`
	UpstreamName   string    `json:"upstream_name,omitempty"`
	TriggerDepth   int       `json:"trigger_depth,omitempty"`
	CoalescedCount int       `json:"coalesced_count,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	DurationMS     int64     `json:"duration_ms"`
}

type listModelRunsOut struct {
	ModelID string        `json:"model_id"`
	Runs    []modelRunOut `json:"runs"`
	// Truncated says older runs exist than those listed.
	Truncated bool `json:"truncated"`
}

type createModelOut struct {
	ModelID         string `json:"model_id"`
	Name            string `json:"name"`
	ConnectionID    string `json:"connection_id"`
	Materialization string `json:"materialization"`
	TargetTable     string `json:"target_table"`
	Layer           string `json:"layer,omitempty"`
	Next            string `json:"next"`
}

// madeResourceID is the model create_model saved, for its audit row.
func (m createModelOut) madeResourceID() string { return m.ModelID }

type silverModelOut struct {
	createModelOut
	// SourceTable, Shape, Keys and Columns are what the generator read off the
	// pipeline table: its shape (history, soft_delete or snapshot), the key a
	// history table is collapsed on, and the columns the model keeps.
	SourceTable      string   `json:"source_table"`
	Shape            string   `json:"shape"`
	Keys             []string `json:"keys,omitempty"`
	Columns          []string `json:"columns"`
	ColumnsTruncated bool     `json:"columns_truncated,omitempty"`
}

type scheduleResultOut struct {
	ModelID  string           `json:"model_id"`
	Schedule modelScheduleOut `json:"schedule"`
	Next     string           `json:"next,omitempty"`
}

type runModelOut struct {
	ModelID string `json:"model_id"`
	// Name and TargetTable say which table this run replaces.
	Name        string `json:"name"`
	TargetTable string `json:"target_table"`
	Status      string `json:"status"`
	Next        string `json:"next"`
}

// ---- wire shapes --------------------------------------------------------------

type wireCreatedQuery struct {
	ID string `json:"id"`
}

type wireMaterialization struct {
	Materialization string `json:"materialization"`
	TargetTable     string `json:"target_table"`
	Layer           string `json:"layer"`
}

type wireSilverPlan struct {
	SourceTable string   `json:"source_table"`
	Shape       string   `json:"shape"`
	Keys        []string `json:"keys"`
	Columns     []string `json:"columns"`
	SQLText     string   `json:"sql_text"`
	TargetTable string   `json:"target_table"`
}

// ---- registry ---------------------------------------------------------------

var (
	argModelID = Arg{Name: "model_id", Type: "string", Format: "uuid", Required: true,
		Description: "The model's id, from list_models or the tool that created it."}

	modelRunStatuses   = []string{"succeeded", "failed", "skipped"}
	modelRunStatusSyns = map[string]string{
		"success": "succeeded", "successful": "succeeded", "passed": "succeeded", "ok": "succeeded", "completed": "succeeded",
		"error": "failed", "errored": "failed", "failure": "failed",
		"skip": "skipped", "waiting": "skipped",
	}
	scheduleTypes        = []string{"cron", "interval", "after_upstream"}
	scheduleTypeSynonyms = map[string]string{
		"crontab": "cron", "every": "interval", "periodic": "interval", "repeat": "interval",
		"after": "after_upstream", "upstream": "after_upstream", "after_upstreams": "after_upstream",
		"after_pipeline": "after_upstream", "after_model": "after_upstream", "chain": "after_upstream",
		"chained": "after_upstream", "event": "after_upstream", "on_upstream": "after_upstream",
	}
)

const modelQueryHelp = `One SELECT for a SQL engine; for MongoDB, JSON such as {"collection": "orders", "pipeline": ` +
	`[...]}, with "database" for another one. It must only read.`

func modelTools(h Handlers) []*Tool {
	return []*Tool{
		{
			Name:  "list_models",
			Title: "List models",
			Description: "Lists the workspace's models: saved queries a run writes to a table. Each has its target table, " +
				"layer, schedule status and last run; layers lists the workspace's layers in order. Never returns " +
				"a model's query text or a row count. Rules: read_guide('models').",
			Guide: "models",
			Args: []Arg{
				{Name: "connection_id", Type: "string", Format: "uuid", Description: "Only models on this connection; its id, from list_connections."},
				{Name: "layer", Type: "string", MaxLength: 40,
					Description: "Only models in this layer: one of the workspace's own layers, by name, in any case."},
				{Name: "name_contains", Type: "string", MaxLength: 100, Description: "Only models whose name contains this text (case-insensitive)."},
			},
			Action:       services.ActionModelRead,
			ResourceType: "model",
			Output:       listModelsOut{},
			run: func(call *toolCall) (any, error) {
				return listModels(call, h)
			},
		},
		{
			Name:  "get_model",
			Title: "Get a model",
			Description: "Returns one model: its target table, layer, last run and schedule (cron, interval, or the " +
				"pipelines and models it runs after), whether the schedule is paused or blocked and why, and the " +
				"tables its query reads with the pipelines and models that write them. Rules: read_guide('models').",
			Guide:        "models",
			Args:         []Arg{argModelID},
			Action:       services.ActionModelRead,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       getModelOut{},
			run: func(call *toolCall) (any, error) {
				var q handlers.SavedQuery
				params := gin.Params{{Key: "id", Value: call.str("model_id")}}
				if err := call.get(h.GetSavedQuery, params, nil, &q); err != nil {
					return nil, err
				}
				out := getModelOut{modelOut: shapeModel(q)}
				sched, err := getModelSchedule(call, h.GetSavedQuerySchedule)
				if err != nil {
					return nil, err
				}
				out.Schedule = sched
				// The model is the answer; what it reads is added to it, so a lookup
				// that fails says so and leaves the rest.
				if out.Reads, err = getModelReads(call, h.SuggestSavedQueryUpstreams); err != nil {
					out.ReadsNote = "rsync.ai could not look up which tables this model reads."
				}
				return out, nil
			},
		},
		{
			Name:  "list_model_runs",
			Title: "List a model's runs",
			Description: "A model's run history, newest first: each run's status (succeeded, failed or skipped), what " +
				"triggered it (a person, its schedule, or the upstream that finished), why a skipped run did not " +
				"rebuild, and a failed run's error with values masked. No row counts. Rules: " +
				"read_guide('models').",
			Guide: "models",
			Args: []Arg{
				argModelID,
				{Name: "status", Type: "string", Enum: modelRunStatuses, Synonyms: modelRunStatusSyns, Description: "Only runs that ended this way."},
				{Name: "limit", Type: "integer", Min: 1, Max: 50, Default: 10, Description: "At most this many runs, newest first (1-50, default 10)."},
			},
			Action:       services.ActionModelRead,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       listModelRunsOut{},
			run: func(call *toolCall) (any, error) {
				return listModelRuns(call, h.ListSavedQueryRuns)
			},
		},
		{
			Name:  "create_model",
			Title: "Create a model",
			Description: "Saves a model: one query on a connection whose result each run writes to target_table, which " +
				"rsync.ai creates on the first run. For a cleaned copy of one table a pipeline writes, use " +
				"create_silver_model. Rules: read_guide('models').",
			Guide: "models",
			Args: []Arg{
				{Name: "connection_id", Type: "string", Format: "uuid", Required: true,
					Description: "The connection the query runs on, and where target_table is written; from list_connections."},
				{Name: "name", Type: "string", Required: true, MaxLength: 200, Description: "The model's name, unique in the workspace."},
				{Name: "query", Type: "string", Required: true, MaxLength: maxQueryRunes, Raw: true, Description: modelQueryHelp},
				{Name: "target_table", Type: "string", Required: true, MaxLength: 300,
					Description: "The table each run writes, as schema.table or table (a collection on MongoDB)."},
				{Name: "layer", Type: "string", MaxLength: 40,
					Description: "One of the workspace's own layers, by name (list_models lists them). Leave it out for none; a " +
						"name the workspace has not made is refused."},
				{Name: "description", Type: "string", MaxLength: 500, Description: "One line on what the model is for."},
			},
			Action:       services.ActionModelWrite,
			Write:        true,
			ResourceType: "model",
			Output:       createModelOut{},
			run: func(call *toolCall) (any, error) {
				return createModel(call, h, modelSpec{
					connectionID: call.str("connection_id"),
					name:         call.str("name"),
					description:  call.str("description"),
					query:        call.str("query"),
					targetTable:  call.str("target_table"),
					layer:        call.str("layer"),
				})
			},
		},
		{
			Name:  "create_silver_model",
			Title: "Create a silver model",
			Description: "Saves the silver model of one table a pipeline writes, with SQL rsync.ai generates from its " +
				"columns (no AI): a CDC history table's latest version of each key, deleted keys dropped; a " +
				"soft-delete table's live rows; else the table as it is. Rules: read_guide('models').",
			Guide: "models",
			Args: []Arg{
				{Name: "connection_id", Type: "string", Format: "uuid", Required: true,
					Description: "The connection holding the pipeline table: a pipeline's destination, from list_connections."},
				{Name: "table", Type: "string", Required: true, MaxLength: 300, Description: "The pipeline table, as schema.table or table."},
				{Name: "keys", Type: "string", MaxLength: 2000,
					Description: "Comma-separated key column names. Only for a history table whose key rsync.ai cannot read from its catalog."},
				{Name: "target_table", Type: "string", MaxLength: 300,
					Description: "The table each run writes. Defaults to the name rsync.ai suggests beside the pipeline table."},
				{Name: "name", Type: "string", MaxLength: 200, Description: "The model's name. Defaults to \"silver <table>\"."},
				{Name: "description", Type: "string", MaxLength: 500, Description: "One line on what the model is for."},
			},
			Action:       services.ActionModelWrite,
			Write:        true,
			ResourceType: "model",
			Output:       silverModelOut{},
			run: func(call *toolCall) (any, error) {
				return createSilverModel(call, h)
			},
		},
		{
			Name:  "schedule_model",
			Title: "Schedule a model",
			Description: "Gives a model its one schedule, active at once: cron, interval (every_seconds, at least 60) or " +
				"after_upstream (each time the listed pipelines or models finish a run). It runs as the token's " +
				"user. Rules: read_guide('models').",
			Guide: "models",
			Args: []Arg{
				argModelID,
				{Name: "schedule_type", Type: "string", Required: true, Enum: scheduleTypes, Synonyms: scheduleTypeSynonyms,
					Description: "cron, interval, or after_upstream."},
				{Name: "cron", Type: "string", MaxLength: 100, Description: "For cron: a five-field cron expression, such as 0 * * * * for hourly."},
				{Name: "every_seconds", Type: "integer", Min: 0, Max: 31 * 24 * 3600, Default: 0,
					Description: "For interval: seconds between runs, at least 60."},
				{Name: "timezone", Type: "string", MaxLength: 64, Description: "For cron: an IANA timezone such as Europe/Berlin. Default UTC."},
				{Name: "after_pipelines", Type: "string", MaxLength: maxIDList,
					Description: "For after_upstream: comma-separated pipeline ids, from list_pipelines."},
				{Name: "after_models", Type: "string", MaxLength: maxIDList,
					Description: "For after_upstream: comma-separated model ids, from list_models."},
				{Name: "upstream_policy", Type: "string", Enum: []string{"any", "all"},
					Description: "For after_upstream with several upstreams: run after any one finishes (default), or once all have."},
			},
			Action:       services.ActionModelWrite,
			Write:        true,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       scheduleResultOut{},
			run: func(call *toolCall) (any, error) {
				return scheduleModel(call, h.CreateSavedQuerySchedule)
			},
		},
		{
			Name:  "run_model",
			Title: "Run a model",
			Description: "Starts one run of a model in the background: its query runs and its result replaces " +
				"target_table. Returns at once; follow the run with list_model_runs. Rules: read_guide('models').",
			Guide:        "models",
			Args:         []Arg{argModelID},
			Action:       services.ActionModelWrite,
			Write:        true,
			Destructive:  true,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       runModelOut{},
			run: func(call *toolCall) (any, error) {
				id := call.str("model_id")
				params := gin.Params{{Key: "id", Value: id}}
				var m handlers.SavedQuery
				if err := call.get(h.GetSavedQuery, params, nil, &m); err != nil {
					return nil, err
				}
				if err := call.do(http.MethodPost, h.StartSavedQueryModelRun, params, nil, nil, nil); err != nil {
					return nil, explain(err)
				}
				return runModelOut{ModelID: id, Name: m.Name, TargetTable: m.TargetTable, Status: "started",
					Next: "The run is in the background. Check it with list_model_runs; a run of a large query can take minutes."}, nil
			},
		},
		{
			Name:  "pause_model_schedule",
			Title: "Pause a model's schedule",
			Description: "Pauses a model's schedule: it stops running on its own until resume_model_schedule. A run in " +
				"flight finishes. Rules: read_guide('models').",
			Guide: "models",
			Args: []Arg{
				argModelID,
				{Name: "reason", Type: "string", MaxLength: 200, Description: "Why, for the people who see the paused schedule."},
			},
			Action:       services.ActionModelWrite,
			Write:        true,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       scheduleResultOut{},
			run: func(call *toolCall) (any, error) {
				body := map[string]string{"reason": call.str("reason")}
				return changeSchedule(call, h.PauseSavedQuerySchedule, body, "")
			},
		},
		{
			Name:  "resume_model_schedule",
			Title: "Resume a model's schedule",
			Description: "Resumes a paused model schedule, including one rsync.ai paused after failed runs. Rules: " +
				"read_guide('models').",
			Guide:        "models",
			Args:         []Arg{argModelID},
			Action:       services.ActionModelWrite,
			Write:        true,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       scheduleResultOut{},
			run: func(call *toolCall) (any, error) {
				return changeSchedule(call, h.ResumeSavedQuerySchedule, map[string]string{},
					"If rsync.ai paused it after failed runs, check the last failure with list_model_runs: it fails again unless that was fixed.")
			},
		},
	}
}

// ---- reads ------------------------------------------------------------------

func listModels(call *toolCall, h Handlers) (any, error) {
	layers, err := workspaceLayerNames(call, h.ListWorkspaceLayers)
	if err != nil {
		return nil, err
	}
	layer := call.str("layer")
	if layer != "" {
		if layer = matchLayer(layers, layer); layer == "" {
			return nil, unknownLayer(layers, call.str("layer"))
		}
	}
	q := url.Values{}
	call.setQuery(q, "connection_id", "connection_id")
	var body struct {
		SavedQueries []handlers.SavedQuery `json:"saved_queries"`
	}
	if err := call.get(h.ListSavedQueries, nil, q, &body); err != nil {
		return nil, err
	}
	needle := strings.ToLower(call.str("name_contains"))
	out := listModelsOut{Models: []modelOut{}, Layers: layers}
	for _, sq := range body.SavedQueries {
		if ident(sq.Materialization) == "" || sq.Materialization == "none" {
			continue // a saved query, not a model
		}
		if layer != "" && !strings.EqualFold(sq.Layer, layer) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(sq.Name), needle) {
			continue
		}
		out.Total++
		if len(out.Models) == maxModels {
			out.Truncated = true
			continue
		}
		out.Models = append(out.Models, shapeModel(sq))
	}
	if out.Total == 0 {
		out.Note = "No model matches. A model is a saved query whose runs write a table; " +
			"create one with create_model or create_silver_model."
	}
	return out, nil
}

// workspaceLayerNames reads the workspace's own layers, in order, cleaned as any
// person-typed text is.
func workspaceLayerNames(call *toolCall, h gin.HandlerFunc) ([]string, error) {
	var body struct {
		Layers []handlers.WorkspaceLayer `json:"layers"`
	}
	if err := call.get(h, nil, nil, &body); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(body.Layers))
	for _, l := range body.Layers {
		if n := cleanText(l.Name); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// matchLayer is the workspace's own spelling of name, matched ignoring case as
// the handlers match it; "" when the workspace has no such layer.
func matchLayer(layers []string, name string) string {
	name = strings.Join(strings.Fields(name), " ")
	for _, l := range layers {
		if strings.EqualFold(l, name) {
			return l
		}
	}
	return ""
}

func unknownLayer(layers []string, name string) *toolError {
	if len(layers) == 0 {
		return argError("this workspace has no layers yet, so no model is in %q; an owner or admin adds them in Workspace settings", cleanText(name))
	}
	return argError("this workspace has no layer named %q; its layers are %s", cleanText(name), strings.Join(layers, ", "))
}

func shapeModel(q handlers.SavedQuery) modelOut {
	out := modelOut{
		ID:                       ident(q.ID),
		Name:                     q.Name,
		Description:              q.Description,
		ConnectionID:             ident(q.ConnectionID),
		ConnectorType:            ident(q.ConnectorType),
		QueryLanguage:            ident(q.QueryLanguage),
		Materialization:          ident(q.Materialization),
		TargetTable:              q.TargetTable,
		Layer:                    cleanText(q.Layer),
		Visibility:               ident(q.Visibility),
		ScheduleStatus:           ident(q.ScheduleStatus),
		NextRunAt:                q.NextRunAt,
		LastRunStatus:            ident(q.LastRunStatus),
		LastRunError:             maskNumbers(llmscrub.Scrub(q.LastRunError)),
		FreshnessDeadlineSeconds: q.FreshnessDeadlineSeconds,
		UpdatedAt:                ident(q.UpdatedAt),
	}
	if q.LastRunAt != nil {
		out.LastRunAt = ident(*q.LastRunAt)
	}
	return out
}

// getModelSchedule reads a model's schedule; nil when it has none, which the
// handler answers with a 404.
func getModelSchedule(call *toolCall, h gin.HandlerFunc) (*modelScheduleOut, error) {
	var s handlers.SavedQuerySchedule
	err := call.get(h, gin.Params{{Key: "id", Value: call.str("model_id")}}, nil, &s)
	if te, ok := err.(*toolError); ok && te.status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := shapeSchedule(s)
	return &out, nil
}

// getModelReads reads the tables a model's query names and what writes them.
func getModelReads(call *toolCall, h gin.HandlerFunc) (*modelReadsOut, error) {
	var s upstreamSuggestion
	if err := call.get(h, gin.Params{{Key: "id", Value: call.str("model_id")}}, nil, &s); err != nil {
		return nil, err
	}
	out := &modelReadsOut{Tables: append([]string{}, s.References...), NoKnownWriter: s.Unresolved, Ambiguous: s.Ambiguous}
	for _, c := range s.Candidates {
		out.WrittenBy = append(out.WrittenBy, modelWriterOut{Kind: ident(c.Kind), ID: ident(c.ID), Name: c.Name, Table: c.Table})
	}
	return out, nil
}

func shapeSchedule(s handlers.SavedQuerySchedule) modelScheduleOut {
	out := modelScheduleOut{
		ScheduleID:       ident(s.ScheduleID),
		ScheduleType:     ident(s.ScheduleType),
		Cron:             s.ScheduleSpec.Cron,
		EverySeconds:     s.ScheduleSpec.EverySeconds,
		Timezone:         s.ScheduleSpec.Timezone,
		UpstreamPolicy:   ident(s.UpstreamPolicy),
		Status:           ident(s.Status),
		PausedAt:         s.PausedAt,
		PausedReason:     maskNumbers(llmscrub.Scrub(s.PausedReason)),
		AutoPausedAt:     s.AutoPausedAt,
		AutoPausedReason: maskNumbers(llmscrub.Scrub(s.AutoPausedReason)),
		Blocked:          s.Blocked,
		BlockedReason:    maskNumbers(llmscrub.Scrub(s.BlockedReason)),
		CreatedAt:        s.CreatedAt,
	}
	for _, u := range s.Upstreams {
		out.Upstreams = append(out.Upstreams, modelUpstreamOut{Kind: ident(u.Kind), ID: ident(u.ID), Name: u.Name})
	}
	return out
}

func listModelRuns(call *toolCall, h gin.HandlerFunc) (any, error) {
	limit := call.num("limit")
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	call.setQuery(q, "status", "status")
	var body struct {
		Runs       []handlers.SavedQueryRun `json:"runs"`
		NextCursor string                   `json:"next_cursor"`
	}
	id := call.str("model_id")
	if err := call.get(h, gin.Params{{Key: "id", Value: id}}, q, &body); err != nil {
		return nil, err
	}
	out := listModelRunsOut{ModelID: id, Runs: []modelRunOut{}, Truncated: body.NextCursor != ""}
	for _, r := range body.Runs {
		if len(out.Runs) == limit {
			out.Truncated = true
			break
		}
		out.Runs = append(out.Runs, modelRunOut{
			RunID:           ident(r.RunID),
			Status:          ident(r.Status),
			TriggerSource:   ident(r.TriggerSource),
			TargetTable:     r.TargetTable,
			Error:           maskNumbers(llmscrub.Scrub(r.Error)),
			AutoPauseReason: maskNumbers(llmscrub.Scrub(r.AutoPauseReason)),
			SkipReason:      ident(r.SkipReason),
			UpstreamKind:    ident(r.UpstreamKind),
			UpstreamID:      ident(r.UpstreamID),
			UpstreamName:    r.UpstreamName,
			TriggerDepth:    r.TriggerDepth,
			CoalescedCount:  r.CoalescedCount,
			StartedAt:       r.StartedAt,
			FinishedAt:      r.FinishedAt,
			DurationMS:      r.DurationMS,
		})
	}
	return out, nil
}

// ---- writes -----------------------------------------------------------------

type modelSpec struct {
	connectionID, name, description, query, targetTable, layer string
}

// createModel saves the query and makes it a table model, as the model dialog
// does: two handler calls, so the second's checks (a read-only query, a target
// rsync.ai may write) are the UI's own. A query the second call refuses is deleted
// again, so a refused create leaves nothing behind.
func createModel(call *toolCall, h Handlers, m modelSpec) (createModelOut, error) {
	var created wireCreatedQuery
	err := call.do(http.MethodPost, h.CreateSavedQuery, nil, nil, map[string]string{
		"connection_id": m.connectionID,
		"name":          m.name,
		"description":   m.description,
		"sql_text":      m.query,
		"visibility":    "workspace",
	}, &created)
	if err != nil {
		return createModelOut{}, explain(err)
	}
	id := ident(created.ID)
	if _, perr := uuid.Parse(id); perr != nil {
		return createModelOut{}, &toolError{status: http.StatusBadGateway, outcome: "error",
			message: "rsync.ai saved the query but returned no id this tool could read; check list_models before trying again."}
	}

	params := gin.Params{{Key: "id", Value: id}}
	var mat wireMaterialization
	err = call.do(http.MethodPut, h.SetSavedQueryMaterialization, params, nil, map[string]any{
		"materialization": "table",
		"target_table":    m.targetTable,
		"layer":           m.layer,
	}, &mat)
	if err != nil {
		refused := explain(err)
		if derr := call.do(http.MethodDelete, h.DeleteSavedQuery, params, nil, nil, nil); derr != nil {
			te := asToolError(refused)
			return createModelOut{}, &toolError{status: te.status, outcome: te.outcome, message: te.message +
				" The query was saved as " + id + " but rsync.ai could not remove it; delete it in Explorer before trying again."}
		}
		return createModelOut{}, refused
	}
	return createModelOut{
		ModelID:         id,
		Name:            m.name,
		ConnectionID:    m.connectionID,
		Materialization: ident(mat.Materialization),
		TargetTable:     mat.TargetTable,
		Layer:           cleanText(mat.Layer),
		Next: "Saved; it has not run. Run it now with run_model, or give it a schedule with schedule_model " +
			"(after_upstream follows the pipeline or model it reads).",
	}, nil
}

func createSilverModel(call *toolCall, h Handlers) (any, error) {
	keys, err := splitList(call.str("keys"), "keys", false)
	if err != nil {
		return nil, err
	}
	connectionID := call.str("connection_id")
	var plan wireSilverPlan
	err = call.do(http.MethodPost, h.GetSilverSQL, gin.Params{{Key: "id", Value: connectionID}}, nil,
		map[string]any{"table": call.str("table"), "keys": keys}, &plan)
	if err != nil {
		return nil, explain(err)
	}
	target := call.str("target_table")
	if target == "" {
		target = plan.TargetTable
	}
	if target == "" {
		return nil, argError("target_table is required: rsync.ai has no name to suggest beside this table")
	}
	name := call.str("name")
	if name == "" {
		name = "silver " + plan.SourceTable
		if r := []rune(name); len(r) > 200 {
			name = string(r[:200])
		}
	}
	// A workspace's layers are its own: the model goes in one named Silver only
	// when the workspace has made one.
	layers, err := workspaceLayerNames(call, h.ListWorkspaceLayers)
	if err != nil {
		return nil, err
	}
	created, err := createModel(call, h, modelSpec{
		connectionID: connectionID,
		name:         name,
		description:  call.str("description"),
		query:        plan.SQLText,
		targetTable:  target,
		layer:        matchLayer(layers, "silver"),
	})
	if err != nil {
		return nil, err
	}
	out := silverModelOut{
		createModelOut: created,
		SourceTable:    plan.SourceTable,
		Shape:          ident(plan.Shape),
		Keys:           plan.Keys,
		Columns:        plan.Columns,
	}
	if len(out.Columns) > maxColumns {
		out.Columns, out.ColumnsTruncated = out.Columns[:maxColumns], true
	}
	return out, nil
}

func scheduleModel(call *toolCall, h gin.HandlerFunc) (any, error) {
	kind := call.str("schedule_type")
	tz := call.str("timezone")
	if tz == "" {
		tz = "UTC"
	}
	spec := map[string]any{"timezone": tz}
	var upstreams []map[string]string
	pipelines, err := splitList(call.str("after_pipelines"), "after_pipelines", true)
	if err != nil {
		return nil, err
	}
	models, err := splitList(call.str("after_models"), "after_models", true)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "cron":
		if call.str("cron") == "" {
			return nil, argError("cron is required for a cron schedule")
		}
		spec["cron"] = call.str("cron")
	case "interval":
		if call.num("every_seconds") < 60 {
			return nil, argError("every_seconds must be at least 60 for an interval schedule")
		}
		spec["every_seconds"] = call.num("every_seconds")
	case "after_upstream":
		for _, id := range pipelines {
			upstreams = append(upstreams, map[string]string{"kind": "pipeline", "id": id})
		}
		for _, id := range models {
			upstreams = append(upstreams, map[string]string{"kind": "model", "id": id})
		}
		if len(upstreams) == 0 {
			return nil, argError("after_upstream needs after_pipelines or after_models")
		}
	}
	if kind != "after_upstream" && len(pipelines)+len(models) > 0 {
		return nil, argError("after_pipelines and after_models are only for an after_upstream schedule")
	}
	body := map[string]any{"schedule_type": kind, "schedule_spec": spec}
	if len(upstreams) > 0 {
		body["upstreams"] = upstreams
		if p := call.str("upstream_policy"); p != "" {
			body["upstream_policy"] = p
		}
	}
	var s handlers.SavedQuerySchedule
	id := call.str("model_id")
	if err := call.do(http.MethodPost, h, gin.Params{{Key: "id", Value: id}}, nil, body, &s); err != nil {
		return nil, explain(err)
	}
	return scheduleResultOut{ModelID: id, Schedule: shapeSchedule(s),
		Next: "The schedule is active. list_model_runs shows each run it starts."}, nil
}

func changeSchedule(call *toolCall, h gin.HandlerFunc, body any, next string) (any, error) {
	var s handlers.SavedQuerySchedule
	id := call.str("model_id")
	if err := call.do(http.MethodPost, h, gin.Params{{Key: "id", Value: id}}, nil, body, &s); err != nil {
		return nil, explain(err)
	}
	return scheduleResultOut{ModelID: id, Schedule: shapeSchedule(s), Next: next}, nil
}

// splitList reads a comma-separated argument: column names, or (ids) uuids.
func splitList(s, arg string, ids bool) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if ids {
			u, err := uuid.Parse(part)
			if err != nil {
				return nil, argError("%s must be comma-separated ids (uuids)", arg)
			}
			part = u.String()
		}
		out = append(out, part)
	}
	if len(out) > 32 {
		return nil, argError("%s lists at most %d entries", arg, 32)
	}
	return out, nil
}
