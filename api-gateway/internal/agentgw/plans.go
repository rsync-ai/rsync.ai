package agentgw

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"api-gateway/internal/handlers"
	"api-gateway/internal/services"
)

// The plan tool: propose a pipeline the person then creates in rsync.ai. A plan
// creates nothing and moves no data. The server checks it with the chat's own
// rules (handlers.ProposePipelinePlan) and stores it; the person who asked opens
// it, picks any table the source has in more than one schema, and clicks Create,
// which is a REST call with their session. No token can apply a plan: tokens
// reach only this gateway, and the gateway has no apply tool.
//
// The output is names, types, ids and the server's notes: never a row value.

// maxPlanTablesArg fits handlers' 100 tables, written comma-separated.
const maxPlanTablesArg = 16 * 1024

type planConnOut struct {
	ConnectionID  string `json:"connection_id"`
	Name          string `json:"name"`
	ConnectorType string `json:"connector_type"`
}

type planChoiceOut struct {
	Name    string   `json:"name"`
	Options []string `json:"options"`
}

type planScheduleOut struct {
	Type         string `json:"type"`
	Cron         string `json:"cron,omitempty"`
	EverySeconds int    `json:"every_seconds,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Description  string `json:"description"`
}

type planOut struct {
	PlanID      string      `json:"plan_id"`
	Status      string      `json:"status"`
	Source      planConnOut `json:"source"`
	Destination planConnOut `json:"destination"`
	SyncMode    string      `json:"sync_mode"`
	CDCMode     string      `json:"cdc_mode,omitempty"`
	Tables      []string    `json:"tables"`
	// TableChoices are names the source has in more than one schema; the person
	// picks one on the plan, never the agent.
	TableChoices  []planChoiceOut `json:"table_choices,omitempty"`
	MissingTables []string        `json:"missing_tables,omitempty"`
	// TablesChecked is false when rsync.ai could not read the source's tables; the
	// named ones are kept as given.
	TablesChecked        bool             `json:"tables_checked"`
	Schedule             *planScheduleOut `json:"schedule,omitempty"`
	DestinationNamespace string           `json:"destination_namespace,omitempty"`
	Notes                []string         `json:"notes,omitempty"`
	URL                  string           `json:"url"`
	ExpiresAt            time.Time        `json:"expires_at"`
	// Next is a fixed sentence: what to tell the person.
	Next string `json:"next"`
}

// madeResourceID is the plan plan_pipeline stored, for its audit row and for
// the assistant's plan card.
func (p planOut) madeResourceID() string { return p.PlanID }

var (
	planSyncModes        = []string{"batch", "cdc"}
	planSyncModeSynonyms = map[string]string{
		"full": "batch", "full_load": "batch", "snapshot": "batch", "scheduled": "batch", "one_time": "batch",
		"once": "batch", "onetime": "batch", "one_off": "batch", "single_run": "batch", "ad_hoc": "batch", "periodic": "batch",
		"streaming": "cdc", "stream": "cdc", "realtime": "cdc", "real_time": "cdc", "change_data_capture": "cdc",
		"replication": "cdc",
	}
	planScheduleTypes        = []string{"cron", "interval"}
	planScheduleTypeSynonyms = map[string]string{
		"crontab": "cron", "every": "interval", "periodic": "interval", "repeat": "interval",
	}
)

func planTools(h Handlers) []*Tool {
	return []*Tool{
		{
			Name:  "plan_pipeline",
			Title: "Propose a pipeline",
			Description: "Proposes a pipeline from one connection to another, for the person to create in rsync.ai. " +
				"rsync.ai checks the sync mode, schedule and tables against the source and returns a plan with a " +
				"url. It creates nothing and moves no data. Rules: read_guide('pipelines').",
			Guide: "pipelines",
			Args: []Arg{
				{Name: "source_connection_id", Type: "string", Format: "uuid", Required: true,
					Description: "The connection the data comes from; from list_connections."},
				{Name: "destination_connection_id", Type: "string", Format: "uuid", Required: true,
					Description: "The connection the data goes to; from list_connections."},
				{Name: "tables", Type: "string", MaxLength: maxPlanTablesArg,
					Description: "Comma-separated source tables, as table or schema.table, at most 100. Leave it out " +
						"to let the person pick when the pipeline first runs."},
				{Name: "sync_mode", Type: "string", Enum: planSyncModes, Synonyms: planSyncModeSynonyms,
					Description: "batch copies the tables on each run; cdc streams each change as it happens. Leave it out for the " +
						"source connection's default."},
				{Name: "schedule_type", Type: "string", Enum: planScheduleTypes, Synonyms: planScheduleTypeSynonyms,
					Description: "For a batch pipeline that runs on a schedule: cron or interval. Leave it out for a " +
						"pipeline that runs once when created."},
				{Name: "cron", Type: "string", MaxLength: 100,
					Description: "For cron: a five-field cron expression, such as 0 * * * * for hourly."},
				{Name: "every_seconds", Type: "integer", Min: 0, Max: 31 * 24 * 3600, Default: 0,
					Description: "For interval: seconds between runs, at least 60."},
				{Name: "timezone", Type: "string", MaxLength: 64,
					Description: "For cron: an IANA timezone such as Europe/Berlin. Default UTC."},
				{Name: "destination_namespace", Type: "string", MaxLength: 200,
					Description: "The schema or database the data lands in. Leave it out to let rsync.ai choose."},
			},
			Action:       services.ActionPipelinePlan,
			Write:        true,
			ResourceType: "pipeline_plan",
			Output:       planOut{},
			run: func(call *toolCall) (any, error) {
				return planPipeline(call, h)
			},
		},
	}
}

func planPipeline(call *toolCall, h Handlers) (any, error) {
	if h.ProposePipelinePlan == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Proposing pipelines is not available on this rsync.ai."}
	}
	var tables []string
	for _, t := range strings.Split(call.str("tables"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			tables = append(tables, t)
		}
	}
	actor := call.s.opts.Actor(call.c)
	plan, err := h.ProposePipelinePlan(call.c, actor, handlers.PipelinePlanInput{
		SourceConnectionID:      call.str("source_connection_id"),
		DestinationConnectionID: call.str("destination_connection_id"),
		Tables:                  tables,
		SyncMode:                call.str("sync_mode"),
		ScheduleType:            call.str("schedule_type"),
		Cron:                    call.str("cron"),
		EverySeconds:            call.num("every_seconds"),
		Timezone:                call.str("timezone"),
		DestinationNamespace:    call.str("destination_namespace"),
	})
	var refusal *handlers.PlanRefusal
	if errors.As(err, &refusal) {
		return nil, &toolError{status: refusal.Status, outcome: refusal.Code, message: cleanText(refusal.Message)}
	}
	if err != nil {
		return nil, err
	}
	return shapePlan(*plan, actor.Channel), nil
}

func shapePlan(p handlers.PipelinePlan, channel services.Channel) planOut {
	out := planOut{
		PlanID:               p.ID,
		Status:               ident(p.Status),
		Source:               planConnOut{ConnectionID: p.Source.ID, Name: p.Source.Name, ConnectorType: ident(p.Source.ConnectorType)},
		Destination:          planConnOut{ConnectionID: p.Destination.ID, Name: p.Destination.Name, ConnectorType: ident(p.Destination.ConnectorType)},
		SyncMode:             ident(p.SyncMode),
		CDCMode:              ident(p.CDCMode),
		Tables:               p.Tables,
		MissingTables:        p.MissingTables,
		TablesChecked:        p.TablesChecked,
		DestinationNamespace: p.DestinationNamespace,
		Notes:                p.Notes,
		URL:                  p.URL,
		ExpiresAt:            p.ExpiresAt,
	}
	if out.Tables == nil {
		out.Tables = []string{}
	}
	for _, ch := range p.TableChoices {
		out.TableChoices = append(out.TableChoices, planChoiceOut{Name: ch.Name, Options: ch.Options})
	}
	if s := p.Schedule; s != nil {
		out.Schedule = &planScheduleOut{Type: ident(s.Type), Cron: s.Cron, EverySeconds: s.EverySeconds,
			Timezone: s.Timezone, Description: s.Description}
	}
	switch {
	case channel == services.ChannelAssistant:
		out.Next = "Nothing is created yet. The person sees this plan as a card under your answer, where they pick " +
			"any table listed in table_choices and click Create. Tell them that; only they can create it."
	default:
		out.Next = "Nothing is created yet. Give the person the url: they open it in rsync.ai, pick any table listed in " +
			"table_choices, and click Create. Only they can create the pipeline; no tool can."
	}
	return out
}
