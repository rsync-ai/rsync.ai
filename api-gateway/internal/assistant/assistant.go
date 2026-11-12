// Package assistant is the in-app assistant ("Ask rsync.ai"): a question from the
// signed-in user, answered by the model the user picks, with the agent
// gateway's tools run as that user (services.AssistantScopes): the read tools,
// plan_pipeline for a member, and for an owner or admin the model writes,
// plan_workflow, plan_dashboard, and generate_connector when they ask with a URL
// (docs/api/assistant.md, design in docs/internal/rsync-assistant-design.md).
//
// The loop runs here. Each model turn is one call to llm-service's
// POST /v1/assistant/step, which sends it through the egress guard; each tool
// the model asks for runs in-process through agentgw.Server.Call with the
// assistant's actor, so it meets the same scope check, argument checks, typed
// output and audit row as a call from an agent token. Model keys an admin set
// in the panel are kept encrypted (keys.go) and never returned.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// Entries are the picker's models, in the order it lists them.
var Entries = []string{"claude", "chatgpt", "gemini"}

var labels = map[string]string{"claude": "Claude", "chatgpt": "ChatGPT", "gemini": "Gemini"}

func knownEntry(entry string) bool {
	_, ok := labels[entry]
	return ok
}

// Tools is the agent gateway as the loop uses it.
type Tools interface {
	ToolSpecs() []agentgw.ToolSpec
	// Run runs one tool as the signed-in user. Its ResourceID is what a
	// successful call named or made, such as the plan plan_pipeline stored.
	Run(c *gin.Context, name string, arguments json.RawMessage) agentgw.Result
}

// Limits bound one question. Zero fields take the defaults.
type Limits struct {
	Steps           int           // model turns per question
	ToolCalls       int           // tools run per question
	Deadline        time.Duration // the whole question, tools included
	ToolResultChars int           // one tool result the model may read
	ToolBudgetChars int           // all tool results in one question
	HistoryTurns    int           // earlier questions kept
	HistoryChars    int           // characters of earlier questions and answers sent
	StopCheck       time.Duration // how often a question looks for another replica's Stop
}

func (l Limits) withDefaults() Limits {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&l.Steps, 6)
	def(&l.ToolCalls, 12)
	def(&l.ToolResultChars, 12_000)
	def(&l.ToolBudgetChars, 48_000)
	def(&l.HistoryTurns, 5)
	def(&l.HistoryChars, 24_000)
	if l.Deadline <= 0 {
		l.Deadline = 120 * time.Second
	}
	if l.StopCheck <= 0 {
		l.StopCheck = time.Second
	}
	return l
}

// Options wires the assistant to the rest of api-gateway.
type Options struct {
	Tools Tools
	LLM   Stepper
	Keys  Keys
	Store Store // nil keeps no history
	// Actor is the signed-in user as the tools see them (handlers.AssistantActor).
	Actor   func(*gin.Context) services.Actor
	QuotaOK func(ctx context.Context, userID string) bool
	// Audit writes one audit row as the request's user (handlers.LogAudit).
	Audit func(c *gin.Context, action, resourceType, resourceID string, details map[string]interface{})
	// RateLimit guards POST /assistant/messages (handlers.ChatRateLimitMiddleware()).
	RateLimit gin.HandlerFunc
	// Samples is the sample_rows tool's switches and sampler
	// (handlers.AssistantSamples). nil, the tool is never offered.
	Samples Samples
	// SQL is the generate_sql tool's writer (handlers.AssistantSQL). nil, the
	// tool is never offered.
	SQL SQLWriter
	// MeterSQL counts one NL→SQL query on the Usage page, as an Explorer
	// generation does, when a tool the model wrote SQL for saved it
	// (handlers.RecordNLQueryUsage). nil counts nothing.
	MeterSQL func(c *gin.Context, connectionID string)
	Limits   Limits
}

// Service serves the assistant's routes.
type Service struct {
	opts    Options
	limits  Limits
	running running
}

// New checks the options and builds the service.
func New(opts Options) (*Service, error) {
	if opts.Tools == nil || opts.LLM == nil || opts.Keys == nil || opts.Actor == nil || opts.QuotaOK == nil || opts.Audit == nil {
		return nil, errors.New("assistant: Tools, LLM, Keys, Actor, QuotaOK and Audit are required")
	}
	if opts.Store == nil {
		opts.Store = NoStore{}
	}
	return &Service{opts: opts, limits: opts.Limits.withDefaults()}, nil
}

// Mount registers the routes. main.go calls it only while
// RSYNC_ASSISTANT_ENABLED is on, so while off the routes do not exist. api is
// the session-authenticated /api/v1 group, admin its platform-admin /admin
// group.
func Mount(api, admin gin.IRoutes, opts Options) (*Service, error) {
	s, err := New(opts)
	if err != nil {
		return nil, err
	}
	ask := []gin.HandlerFunc{s.Messages}
	if opts.RateLimit != nil {
		ask = append([]gin.HandlerFunc{opts.RateLimit}, ask...)
	}
	api.POST("/assistant/messages", ask...)
	api.POST("/assistant/messages/:request_id/cancel", s.Cancel)
	api.GET("/assistant/models", s.Models)
	admin.GET("/assistant/models", s.AdminList)
	admin.PUT("/assistant/models/:entry", s.AdminSet)
	admin.DELETE("/assistant/models/:entry", s.AdminRemove)
	admin.POST("/assistant/models/:entry/test", s.AdminTest)
	if opts.Samples != nil {
		admin.PUT("/assistant/models/:entry/sample-rows", s.AdminSetSampleRows)
	}
	return s, nil
}
